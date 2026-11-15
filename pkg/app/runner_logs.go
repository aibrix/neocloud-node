package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/runner"
)

// runnerLogsHandler authenticates uploads independently of kubelet API settings.
func runnerLogsHandler(bindings core.BindingStore, logs *runner.LogStore, secret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.TrimSpace(secret) == "" {
			http.Error(w, "runner log authentication is not configured", http.StatusServiceUnavailable)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, runner.MaxLogBatchBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "log batch exceeds limit", http.StatusRequestEntityTooLarge)
			return
		}
		var batch runner.LogBatch
		if err := json.Unmarshal(body, &batch); err != nil {
			http.Error(w, "invalid log batch", http.StatusBadRequest)
			return
		}
		if err := runner.VerifyLogBatch(secret, batch.WorkloadID, r.Header.Get("X-NeoCloud-Runner-Timestamp"), r.Header.Get("X-NeoCloud-Runner-Signature"), body, time.Now()); err != nil {
			http.Error(w, "invalid log batch signature", http.StatusUnauthorized)
			return
		}
		parts := strings.Split(batch.WorkloadID, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "invalid workload identity", http.StatusBadRequest)
			return
		}
		b, err := bindings.GetByWorkloadUID(r.Context(), parts[0], parts[1])
		if errors.Is(err, core.ErrBindingNotFound) {
			http.Error(w, "binding not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "binding lookup failed", http.StatusServiceUnavailable)
			return
		}
		selected := b.Placement.Provider
		if selected == "" {
			selected = b.Request.Provider
		}
		if selected != ProviderLambda || b.Namespace != parts[0] || b.WorkloadUID != parts[1] || b.RemoteID == "" || b.Deleting || b.Phase == core.BindingDeleted {
			http.Error(w, "workload is not an active Lambda binding", http.StatusConflict)
			return
		}
		ack, err := logs.Accept(core.LogTarget{Namespace: b.Namespace, WorkloadUID: b.WorkloadUID, RemoteID: b.RemoteID}, batch)
		if err != nil {
			http.Error(w, "log batch rejected: "+err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ack)
	})
}
