package runpod

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestClientCreateSendsRunPodPayload(t *testing.T) {
	t.Parallel()

	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/pods" {
			t.Fatalf("path = %s, want /pods", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Fatalf("authorization header = %q, want bearer token", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"id": "runpod-1",
			"desiredStatus": "RUNNING",
			"image": "example.com/train:latest",
			"gpu": {"count": 1, "displayName": "NVIDIA H100"}
		}`))
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL: server.URL,
		APIKey:  "test-token",
	})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:    "example.com/train:latest",
		Command:  []string{"python"},
		Args:     []string{"train.py"},
		Env:      map[string]string{"MODEL": "llama"},
		GPUCount: 1,
		GPUType:  "NVIDIA H100",
		Ports: []core.Port{
			{Name: "metrics", ContainerPort: 9090, Protocol: "TCP"},
		},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.ID != "runpod-1" {
		t.Fatalf("remote ID = %q, want runpod-1", remote.ID)
	}
	if remote.Phase != core.RemoteRunning {
		t.Fatalf("remote phase = %q, want %q", remote.Phase, core.RemoteRunning)
	}
	if remote.Provider != "runpod" {
		t.Fatalf("remote provider = %q, want runpod", remote.Provider)
	}

	assertPayloadString(t, gotPayload, "name", "gpu-burst-node-default-pod-123")
	assertPayloadString(t, gotPayload, "imageName", "example.com/train:latest")
	assertPayloadString(t, gotPayload, "computeType", "GPU")
	assertPayloadStringSlice(t, gotPayload, "dockerEntrypoint", []string{"python"})
	assertPayloadStringSlice(t, gotPayload, "dockerStartCmd", []string{"train.py"})
	assertPayloadStringSlice(t, gotPayload, "gpuTypeIds", []string{"NVIDIA H100"})
	assertPayloadStringSlice(t, gotPayload, "ports", []string{"9090/tcp"})

	if gotPayload["gpuCount"] != float64(1) {
		t.Fatalf("gpuCount = %#v, want 1", gotPayload["gpuCount"])
	}
	env, ok := gotPayload["env"].(map[string]any)
	if !ok {
		t.Fatalf("env = %#v, want object", gotPayload["env"])
	}
	if env["MODEL"] != "llama" {
		t.Fatalf("env MODEL = %#v, want llama", env["MODEL"])
	}
}

func TestClientGetMapsDesiredStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		desiredStatus string
		wantPhase     core.RemotePhase
	}{
		{name: "running", desiredStatus: "RUNNING", wantPhase: core.RemoteRunning},
		{name: "exited", desiredStatus: "EXITED", wantPhase: core.RemoteSucceeded},
		{name: "terminated", desiredStatus: "TERMINATED", wantPhase: core.RemoteDeleted},
		{name: "unknown", desiredStatus: "STARTING", wantPhase: core.RemotePending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Fatalf("method = %s, want GET", r.Method)
				}
				if r.URL.Path != "/pods/runpod-1" {
					t.Fatalf("path = %s, want /pods/runpod-1", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{
					"id": "runpod-1",
					"desiredStatus": "` + tt.desiredStatus + `",
					"image": "example.com/train:latest"
				}`))
			}))
			defer server.Close()

			client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
			remote, err := client.Get(context.Background(), "runpod-1")
			if err != nil {
				t.Fatalf("Get returned error: %v", err)
			}
			if remote.ID != "runpod-1" {
				t.Fatalf("remote ID = %q, want runpod-1", remote.ID)
			}
			if remote.Phase != tt.wantPhase {
				t.Fatalf("remote phase = %q, want %q", remote.Phase, tt.wantPhase)
			}
		})
	}
}

func TestClientDeleteTreatsNotFoundAsSuccess(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("method = %s, want DELETE", r.Method)
		}
		requests++
		switch requests {
		case 1:
			if r.URL.Path != "/pods/runpod-1" {
				t.Fatalf("path = %s, want /pods/runpod-1", r.URL.Path)
			}
			w.WriteHeader(http.StatusNoContent)
		case 2:
			if r.URL.Path != "/pods/missing" {
				t.Fatalf("path = %s, want /pods/missing", r.URL.Path)
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	if err := client.Delete(context.Background(), "runpod-1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := client.Delete(context.Background(), "missing"); err != nil {
		t.Fatalf("Delete missing returned error: %v", err)
	}
}

func assertPayloadString(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()

	got, ok := payload[key].(string)
	if !ok || got != want {
		t.Fatalf("%s = %#v, want %q", key, payload[key], want)
	}
}

func assertPayloadStringSlice(t *testing.T, payload map[string]any, key string, want []string) {
	t.Helper()

	raw, ok := payload[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want string array", key, payload[key])
	}
	if len(raw) != len(want) {
		t.Fatalf("%s length = %d, want %d", key, len(raw), len(want))
	}
	for idx := range raw {
		if raw[idx] != want[idx] {
			t.Fatalf("%s[%d] = %#v, want %q", key, idx, raw[idx], want[idx])
		}
	}
}
