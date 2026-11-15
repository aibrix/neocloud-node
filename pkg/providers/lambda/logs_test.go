package lambda

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/runner"
)

func TestLogsConfiguredStoreAndDisabled(t *testing.T) {
	target := core.LogTarget{Namespace: "ns", WorkloadUID: "uid", RemoteID: "vm"}
	if _, e := NewClient(Config{}).OpenLogs(context.Background(), target, core.LogOptions{}); e == nil {
		t.Fatal("disabled logs succeeded")
	}
	store := runner.NewLogStore(runner.LogStoreConfig{})
	b := runner.LogBatch{WorkloadID: "ns/uid", Generation: "launch", FirstSequence: 1, Records: []runner.LogRecord{{Sequence: 1, Timestamp: time.Now(), Line: "retained"}}, ReplayComplete: true, Complete: true}
	a, e := store.Accept(target, b)
	if e != nil {
		t.Fatal(e)
	}
	b.Epoch = a.Epoch
	if _, e = store.Accept(target, b); e != nil {
		t.Fatal(e)
	}
	client := NewClient(Config{RunnerLogsURL: "http://controller/logs", RunnerLogsSecret: "secret", LogStore: store})
	r, e := client.OpenLogs(context.Background(), target, core.LogOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	out, e := io.ReadAll(r)
	if e != nil || string(out) != "retained\n" {
		t.Fatalf("logs %q %v", out, e)
	}
	payload, e := NewClient(Config{CallbackURL: "http://controller/status", RunnerLogsURL: "http://controller/logs", RunnerLogsSecret: "root", DefaultRegion: "region", DefaultInstanceType: "type", DefaultSSHKeyNames: []string{"key"}}).launchPayload("ns/uid", "name", core.WorkloadRequest{Image: "image"})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(payload.UserData, "logship.py") {
		t.Fatal("Lambda did not pass logs config to runner")
	}
}

func TestLogsRequireRunnerBootstrap(t *testing.T) {
	_, e := NewClient(Config{RunnerLogsURL: "http://controller/logs", RunnerLogsSecret: "secret", DefaultRegion: "region", DefaultInstanceType: "type", DefaultSSHKeyNames: []string{"key"}}).launchPayload("ns/uid", "name", core.WorkloadRequest{Image: "image"})
	if e == nil {
		t.Fatal("logs configuration silently omitted runner")
	}
}
