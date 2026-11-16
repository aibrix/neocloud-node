package lambda

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type sshLogFixture struct {
	config  Config
	command chan string
	closed  chan struct{}
	execs   atomic.Int32
}

func newSSHLogFixture(t *testing.T, output string, exit uint32, follow bool, delay time.Duration) *sshLogFixture {
	t.Helper()
	newKey := func() (ed25519.PrivateKey, ssh.Signer) {
		t.Helper()
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return key, signer
	}
	private, clientKey := newKey()
	_, hostKey := newKey()
	block, err := ssh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privatePath, hostsPath := filepath.Join(dir, "id"), filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte(knownhosts.Line([]string{listener.Addr().String()}, hostKey.PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	n, _ := strconv.Atoi(port)
	f := &sshLogFixture{config: Config{SSHUser: "ubuntu", SSHPort: n, SSHPrivateKeyFile: privatePath, SSHKnownHostsFile: hostsPath}, command: make(chan string, 1), closed: make(chan struct{})}
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/instances/vm" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "vm", "status": "active", "ip": "127.0.0.1"}})
	}))
	f.config.BaseURL = metadata.URL
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "ubuntu" || string(key.Marshal()) != string(clientKey.PublicKey().Marshal()) {
			return nil, os.ErrPermission
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostKey)
	var connections []net.Conn
	var mu sync.Mutex
	var wg sync.WaitGroup
	var once sync.Once
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				defer once.Do(func() { close(f.closed) })
				server, channels, requests, err := ssh.NewServerConn(conn, serverConfig)
				if err != nil {
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for next := range channels {
					if next.ChannelType() != "session" {
						next.Reject(ssh.UnknownChannelType, "session required")
						continue
					}
					channel, reqs, err := next.Accept()
					if err != nil {
						return
					}
					for req := range reqs {
						if req.Type != "exec" {
							req.Reply(false, nil)
							continue
						}
						var payload struct{ Command string }
						if ssh.Unmarshal(req.Payload, &payload) != nil {
							return
						}
						f.execs.Add(1)
						f.command <- payload.Command
						req.Reply(true, nil)
						if delay > 0 {
							io.WriteString(channel, "2026-11-16T00:00:00Z first\n")
							time.Sleep(delay)
						}
						io.WriteString(channel, output)
						if follow {
							io.Copy(io.Discard, channel)
						}
						channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exit}))
						channel.Close()
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		mu.Lock()
		for _, c := range connections {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
		metadata.Close()
	})
	return f
}

func TestSSHLogsDockerOptionsAndPresentation(t *testing.T) {
	tail, zero := 3, 0
	since := time.Date(2026, 11, 16, 0, 0, 0, 123, time.UTC)
	for _, tc := range []struct {
		name          string
		options       core.LogOptions
		command, want string
	}{
		{"default", core.LogOptions{}, "exec sudo -n docker logs --timestamps neocloud-node-workload 2>&1", "hello\n"},
		{"tail since timestamps", core.LogOptions{Tail: &tail, SinceTime: since, Timestamps: true}, "exec sudo -n docker logs --timestamps --tail 3 --since 2026-11-16T00:00:00.000000123Z neocloud-node-workload 2>&1", "2026-11-16T00:00:01.123456789Z hello\n"},
		{"follow zero", core.LogOptions{Tail: &zero, Follow: true}, "exec sudo -n docker logs --timestamps --follow --tail 0 neocloud-node-workload 2>&1", "hello\n"},
		{"byte limit", core.LogOptions{LimitBytes: 3}, "exec sudo -n docker logs --timestamps neocloud-node-workload 2>&1", "hel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSHLogFixture(t, "2026-11-16T00:00:01.123456789Z hello\n", 0, false, 0)
			r, err := NewClient(f.config).OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, tc.options)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			out, err := io.ReadAll(r)
			if err != nil || string(out) != tc.want {
				t.Fatalf("logs=%q err=%v", out, err)
			}
			if cmd := <-f.command; cmd != tc.command {
				t.Fatalf("command=%q want %q", cmd, tc.command)
			}
		})
	}
}

func TestSSHLogsFiniteSnapshotWaitsForCommandEOF(t *testing.T) {
	f := newSSHLogFixture(t, "2026-11-16T00:00:02Z second\n", 0, false, 1200*time.Millisecond)
	r, err := NewClient(f.config).OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil || string(out) != "first\nsecond\n" {
		t.Fatalf("truncated logs=%q err=%v", out, err)
	}
}

func TestSSHLogsRejectUnknownAndChangedHosts(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			f := newSSHLogFixture(t, "", 0, false, 0)
			contents := ""
			if changed {
				_, key, _ := ed25519.GenerateKey(rand.Reader)
				signer, _ := ssh.NewSignerFromKey(key)
				contents = knownhosts.Line([]string{net.JoinHostPort("127.0.0.1", strconv.Itoa(f.config.SSHPort))}, signer.PublicKey()) + "\n"
			}
			if err := os.WriteFile(f.config.SSHKnownHostsFile, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			r, err := NewClient(f.config).OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{})
			if err == nil {
				r.Close()
				t.Fatal("untrusted SSH host accepted")
			}
			if f.execs.Load() != 0 {
				t.Fatal("command executed on untrusted host")
			}
		})
	}
}

func TestSSHLogsRejectInvalidCredentialsAndMetadata(t *testing.T) {
	for _, which := range []string{"missing key", "invalid key", "missing hosts", "metadata"} {
		t.Run(which, func(t *testing.T) {
			f := newSSHLogFixture(t, "", 0, false, 0)
			switch which {
			case "missing key":
				f.config.SSHPrivateKeyFile = ""
			case "invalid key":
				os.WriteFile(f.config.SSHPrivateKeyFile, []byte("not-a-private-key"), 0600)
			case "missing hosts":
				f.config.SSHKnownHostsFile = ""
			case "metadata":
				f.config.BaseURL += "/missing"
			}
			r, err := NewClient(f.config).OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{})
			if err == nil {
				r.Close()
				t.Fatal("invalid SSH log configuration accepted")
			}
		})
	}
}

func TestSSHLogsSnapshotReportsCommandAndDecodeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		exit         uint32
	}{
		{"command failed", "2026-11-16T00:00:00Z partial\n", 17},
		{"invalid timestamp", "bad-timestamp output\n", 0},
		{"oversized record", "2026-11-16T00:00:00Z " + strings.Repeat("x", (1<<20)+1) + "\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSSHLogFixture(t, tc.output, tc.exit, false, 0)
			r, err := NewClient(f.config).OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			out, err := io.ReadAll(r)
			if err == nil || len(out) != 0 {
				t.Fatalf("failed snapshot returned %q err=%v", out, err)
			}
		})
	}
}

func TestSSHLogsCloseCancelsSilentFollow(t *testing.T) {
	f := newSSHLogFixture(t, "2026-11-16T00:00:00Z live\n", 0, true, 0)
	r, err := NewClient(f.config).OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil || line != "live\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	r.Close()
	select {
	case <-f.closed:
	case <-time.After(time.Second):
		t.Fatal("closing logs did not close SSH connection")
	}
}

func TestSSHLogsCancellationDuringHandshake(t *testing.T) {
	f := newSSHLogFixture(t, "", 0, false, 0)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	f.config.SSHPort, _ = strconv.Atoi(port)
	closed := make(chan struct{})
	go func() {
		conn, e := listener.Accept()
		if e != nil {
			close(closed)
			return
		}
		defer conn.Close()
		io.Copy(io.Discard, conn)
		close(closed)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r, err := NewClient(f.config).OpenLogs(ctx, core.LogTarget{RemoteID: "vm"}, core.LogOptions{})
	if err == nil {
		r.Close()
		t.Fatal("stalled handshake succeeded")
	}
	if time.Since(start) > time.Second {
		t.Fatal("handshake ignored cancellation")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled handshake socket leaked")
	}
}

func TestSSHLogsZeroTailAndUnsupportedPrevious(t *testing.T) {
	zero := 0
	c := NewClient(Config{})
	r, err := c.OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{Tail: &zero})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil || len(out) != 0 {
		t.Fatalf("tail=0 logs=%q err=%v", out, err)
	}
	if _, err := c.OpenLogs(context.Background(), core.LogTarget{RemoteID: "vm"}, core.LogOptions{Previous: true}); err == nil {
		t.Fatal("previous logs accepted")
	}
}
