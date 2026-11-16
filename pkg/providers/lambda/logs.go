package lambda

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/logstream"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

var _ core.ProviderLogReader = (*Client)(nil)

// Include metadata lookup and SSH handshakes in the connection limit.
var sshLogConnections = make(chan struct{}, 16)

func (c *Client) OpenLogs(ctx context.Context, t core.LogTarget, o core.LogOptions) (io.ReadCloser, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.RemoteID == "" {
		return nil, fmt.Errorf("missing Lambda instance ID")
	}
	if o.Tail != nil && *o.Tail == 0 && !o.Follow {
		return io.NopCloser(strings.NewReader("")), nil
	}
	config, err := c.sshClientConfig()
	if err != nil {
		return nil, err
	}
	select {
	case sshLogConnections <- struct{}{}:
	default:
		return nil, fmt.Errorf("concurrent Lambda SSH log limit reached")
	}
	owned := true
	var client *ssh.Client
	defer func() {
		if owned {
			if client != nil {
				client.Close()
			}
			<-sshLogConnections
		}
	}()
	instance, err := c.Get(ctx, t.RemoteID)
	if err != nil {
		return nil, err
	}
	if instance.ID != t.RemoteID || instance.Phase != core.RemoteRunning || net.ParseIP(instance.DirectEndpointHost) == nil {
		return nil, fmt.Errorf("Lambda instance has no active SSH address")
	}
	port := c.sshConfig.SSHPort
	if port == 0 {
		port = 22
	}
	address := net.JoinHostPort(instance.DirectEndpointHost, strconv.Itoa(port))
	client, err = dialLogSSH(ctx, address, config)
	if err != nil {
		return nil, fmt.Errorf("connect Lambda SSH logs: %w", err)
	}
	command := "exec sudo -n docker logs --timestamps"
	if o.Follow {
		command += " --follow"
	}
	if o.Tail != nil {
		command += " --tail " + strconv.Itoa(*o.Tail)
	}
	if !o.SinceTime.IsZero() {
		command += " --since " + o.SinceTime.UTC().Format(time.RFC3339Nano)
	}
	// All arguments are fixed identifiers or validated numbers/timestamps.
	// Merge stderr remotely so application stderr is returned as log output too.
	command += " neocloud-node-workload 2>&1"
	common := o
	common.Tail = nil
	common.SinceTime = time.Time{}
	r, err := logstream.Open(ctx, common, func(streamCtx context.Context, emit func(logstream.Event) error) error {
		defer func() { client.Close(); <-sshLogConnections }()
		stop := context.AfterFunc(streamCtx, func() { client.Close() })
		defer stop()
		session, err := client.NewSession()
		if err != nil {
			return fmt.Errorf("open Lambda SSH session: %w", err)
		}
		defer session.Close()
		stdout, err := session.StdoutPipe()
		if err != nil {
			return err
		}
		if err := session.Start(command); err != nil {
			return fmt.Errorf("start Lambda Docker logs: %w", err)
		}
		if err := decodeDockerLogs(stdout, emit); err != nil {
			return err
		}
		if err := session.Wait(); err != nil {
			return fmt.Errorf("Lambda Docker logs command failed: %w", err)
		}
		return nil
	}, logstream.Timing{Finite: true})
	if err != nil {
		return nil, err
	}
	owned = false
	return r, nil
}

func (c *Client) sshClientConfig() (*ssh.ClientConfig, error) {
	if c.sshConfig.SSHPrivateKeyFile == "" || c.sshConfig.SSHKnownHostsFile == "" {
		return nil, fmt.Errorf("Lambda logs require SSH private-key and known-hosts files")
	}
	key, err := os.ReadFile(c.sshConfig.SSHPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read Lambda SSH private key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("parse Lambda SSH private key: %w", err)
	}
	verifyHost, err := knownhosts.New(c.sshConfig.SSHKnownHostsFile)
	if err != nil {
		return nil, fmt.Errorf("load Lambda SSH known hosts: %w", err)
	}
	user := c.sshConfig.SSHUser
	if user == "" {
		user = "ubuntu"
	}
	return &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: verifyHost}, nil
}

func dialLogSSH(ctx context.Context, address string, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, err
	}
	sshConn, channels, requests, err := ssh.NewClientConn(conn, address, config)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		sshConn.Close()
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		sshConn.Close()
		return nil, err
	}
	return ssh.NewClient(sshConn, channels, requests), nil
}

func decodeDockerLogs(src io.Reader, emit func(logstream.Event) error) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), logstream.MaxLineBytes+65)
	for scanner.Scan() {
		timestamp, line, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			return fmt.Errorf("Lambda Docker log record has no timestamp")
		}
		ts, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return fmt.Errorf("Lambda Docker log record has invalid timestamp")
		}
		if err := emit(logstream.Event{Timestamp: ts, Line: line}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Lambda Docker logs: %w", err)
	}
	return nil
}
