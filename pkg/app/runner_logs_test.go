package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/runner"
)

func TestRunnerLogsAuthenticationAndReplay(t *testing.T) {
	ctx := context.Background()
	bindings := core.NewMemoryBindingStore()
	bindings.CreateOrGet(ctx, core.Binding{Namespace: "default", WorkloadUID: "uid", RemoteID: "vm", Placement: core.Placement{Provider: ProviderLambda}})
	logs := runner.NewLogStore(runner.LogStoreConfig{})
	h := runnerLogsHandler(bindings, logs, "root-secret")
	b := runner.LogBatch{WorkloadID: "default/uid", Generation: "launch", FirstSequence: 1}
	post := func(batch runner.LogBatch, key string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(batch)
		r := httptest.NewRequest(http.MethodPost, "/neocloud-node/v1/runner/logs", bytes.NewReader(raw))
		ts := strconv.FormatInt(time.Now().Unix(), 10)
		r.Header.Set("X-NeoCloud-Runner-Timestamp", ts)
		r.Header.Set("X-NeoCloud-Runner-Signature", runner.SignLogBatch(key, batch.WorkloadID, ts, raw))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := post(b, "wrong"); w.Code != 401 {
		t.Fatalf("invalid signature code=%d", w.Code)
	}
	w := post(b, "root-secret")
	if w.Code != 200 {
		t.Fatalf("handshake code=%d body=%s", w.Code, w.Body.String())
	}
	var ack runner.LogAck
	json.Unmarshal(w.Body.Bytes(), &ack)
	if !ack.ReplayRequired {
		t.Fatal("new controller did not request replay")
	}
	b.Epoch = ack.Epoch
	b.ReplayComplete = true
	b.Complete = true
	b.Records = []runner.LogRecord{{Sequence: 1, Timestamp: time.Now(), Line: "lambda output"}}
	if w := post(b, "root-secret"); w.Code != 200 {
		t.Fatalf("replay code=%d body=%s", w.Code, w.Body.String())
	}
	r, err := logs.OpenLogs(ctx, core.LogTarget{Namespace: "default", WorkloadUID: "uid", RemoteID: "vm"}, core.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil || string(data) != "lambda output\n" {
		t.Fatalf("logs=%q err=%v", data, err)
	}
	b.WorkloadID = "default/other"
	if w := post(b, "root-secret"); w.Code != 404 {
		t.Fatalf("unknown workload code=%d", w.Code)
	}
}

func TestRunnerLogsRejectsMissingSecretForeignProviderAndOversizedBody(t *testing.T) {
	bindings := core.NewMemoryBindingStore()
	bindings.CreateOrGet(context.Background(), core.Binding{Namespace: "default", WorkloadUID: "uid", RemoteID: "pod", Placement: core.Placement{Provider: ProviderRunPod}})
	for _, tc := range []struct {
		name, secret string
		want         int
		oversize     bool
	}{{"no secret", "", 503, false}, {"foreign provider", "secret", 409, false}, {"oversize", "secret", 413, true}} {
		t.Run(tc.name, func(t *testing.T) {
			batch := runner.LogBatch{WorkloadID: "default/uid", Generation: "g", FirstSequence: 1}
			body, _ := json.Marshal(batch)
			if tc.oversize {
				body = []byte(strings.Repeat("x", runner.MaxLogBatchBytes+1))
			}
			r := httptest.NewRequest(http.MethodPost, "/neocloud-node/v1/runner/logs", bytes.NewReader(body))
			ts := strconv.FormatInt(time.Now().Unix(), 10)
			r.Header.Set("X-NeoCloud-Runner-Timestamp", ts)
			r.Header.Set("X-NeoCloud-Runner-Signature", runner.SignLogBatch(tc.secret, batch.WorkloadID, ts, body))
			w := httptest.NewRecorder()
			runnerLogsHandler(bindings, runner.NewLogStore(runner.LogStoreConfig{}), tc.secret).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
