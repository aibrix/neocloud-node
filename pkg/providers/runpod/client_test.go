package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
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
			"costPerHr": 0.44,
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
		MarketType: core.MarketTypeSpot,
		ProviderHints: map[string]string{
			core.AnnotationRunPodTemplateID:         "template-1",
			core.AnnotationRunPodDataCenterIDs:      "US-TX-3,EU-RO-1",
			core.AnnotationRunPodDataCenterPriority: "custom",
			core.AnnotationRunPodNetworkVolumeID:    "volume-1",
			core.AnnotationRunPodRegistryAuthID:     "registry-1",
			core.AnnotationRunPodVolumeMountPath:    "/workspace",
			core.AnnotationRunPodCloudType:          "COMMUNITY",
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
	if math.Abs(remote.HourlyCost-0.44) > 0.000001 {
		t.Fatalf("remote hourly cost = %.2f, want 0.44", remote.HourlyCost)
	}
	if remote.CostSource != "runpod.costPerHr" {
		t.Fatalf("remote cost source = %q, want runpod.costPerHr", remote.CostSource)
	}

	assertPayloadString(t, gotPayload, "name", "gpu-burst-node-default-pod-123")
	assertPayloadString(t, gotPayload, "imageName", "example.com/train:latest")
	assertPayloadString(t, gotPayload, "computeType", "GPU")
	assertPayloadStringSlice(t, gotPayload, "dockerEntrypoint", []string{"python"})
	assertPayloadStringSlice(t, gotPayload, "dockerStartCmd", []string{"train.py"})
	assertPayloadStringSlice(t, gotPayload, "gpuTypeIds", []string{"NVIDIA H100"})
	assertPayloadStringSlice(t, gotPayload, "ports", []string{"9090/tcp"})
	assertPayloadStringSlice(t, gotPayload, "dataCenterIds", []string{"US-TX-3", "EU-RO-1"})
	assertPayloadString(t, gotPayload, "dataCenterPriority", "custom")
	assertPayloadString(t, gotPayload, "templateId", "template-1")
	assertPayloadString(t, gotPayload, "networkVolumeId", "volume-1")
	assertPayloadString(t, gotPayload, "containerRegistryAuthId", "registry-1")
	assertPayloadString(t, gotPayload, "volumeMountPath", "/workspace")
	assertPayloadString(t, gotPayload, "cloudType", "COMMUNITY")

	if gotPayload["gpuCount"] != float64(1) {
		t.Fatalf("gpuCount = %#v, want 1", gotPayload["gpuCount"])
	}
	if gotPayload["interruptible"] != true {
		t.Fatalf("interruptible = %#v, want true", gotPayload["interruptible"])
	}
	env, ok := gotPayload["env"].(map[string]any)
	if !ok {
		t.Fatalf("env = %#v, want object", gotPayload["env"])
	}
	if env["MODEL"] != "llama" {
		t.Fatalf("env MODEL = %#v, want llama", env["MODEL"])
	}
}

func TestClientCreateRecoversLostCreateResponseByExternalName(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests == 1 && r.Method == http.MethodPost && r.URL.Path == "/pods":
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case requests == 2 && r.Method == http.MethodGet && r.URL.Path == "/pods":
			if got := r.URL.Query().Get("name"); got != "gpu-burst-node-default-pod-123" {
				t.Fatalf("name query = %q, want external name", got)
			}
			_, _ = w.Write([]byte(`[{
				"id": "runpod-existing",
				"name": "gpu-burst-node-default-pod-123",
				"desiredStatus": "RUNNING",
				"image": "example.com/train:latest"
			}]`))
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.ID != "runpod-existing" {
		t.Fatalf("remote ID = %q, want runpod-existing", remote.ID)
	}
}

func TestClientCreateRecoversDecodeFailureByExternalName(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests == 1 && r.Method == http.MethodPost && r.URL.Path == "/pods":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":`))
		case requests == 2 && r.Method == http.MethodGet && r.URL.Path == "/pods":
			if got := r.URL.Query().Get("name"); got != "gpu-burst-node-default-pod-123" {
				t.Fatalf("name query = %q, want external name", got)
			}
			_, _ = w.Write([]byte(`[{
				"id": "runpod-created",
				"name": "gpu-burst-node-default-pod-123",
				"desiredStatus": "RUNNING",
				"image": "example.com/train:latest",
				"costPerHr": 0.44
			}]`))
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.ID != "runpod-created" {
		t.Fatalf("remote ID = %q, want runpod-created", remote.ID)
	}
	if remote.HourlyCost != 0.44 {
		t.Fatalf("hourly cost = %.2f, want 0.44", remote.HourlyCost)
	}
}

func TestClientClassifiesProviderErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		statusCode     int
		body           string
		wantClass      core.ErrorClass
		wantIsNotFound bool
	}{
		{name: "auth", statusCode: http.StatusUnauthorized, body: `{"error":"unauthorized"}`, wantClass: core.ErrorClassAuth},
		{name: "capacity", statusCode: http.StatusBadRequest, body: `{"error":"insufficient capacity"}`, wantClass: core.ErrorClassCapacity},
		{name: "capacity in server error", statusCode: http.StatusInternalServerError, body: `{"error":"create pod: There are no longer any instances available with the request specifications."}`, wantClass: core.ErrorClassCapacity},
		{name: "not found", statusCode: http.StatusNotFound, body: `{"error":"not found"}`, wantClass: core.ErrorClassRemoteNotFound, wantIsNotFound: true},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, body: `{"error":"rate limited"}`, wantClass: core.ErrorClassRateLimited},
		{name: "server error", statusCode: http.StatusBadGateway, body: `{"error":"upstream failed"}`, wantClass: core.ErrorClassTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
			_, err := client.Get(context.Background(), "runpod-1")
			var providerErr *core.ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("Get error = %T %[1]v, want ProviderError", err)
			}
			if providerErr.Class != tt.wantClass {
				t.Fatalf("error class = %s, want %s", providerErr.Class, tt.wantClass)
			}
			if errors.Is(err, core.ErrRemoteNotFound) != tt.wantIsNotFound {
				t.Fatalf("errors.Is(ErrRemoteNotFound) = %t, want %t", errors.Is(err, core.ErrRemoteNotFound), tt.wantIsNotFound)
			}
		})
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
					"image": "example.com/train:latest",
					"costPerHr": "0.74",
					"adjustedCostPerHr": 0.69
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
			if remote.HourlyCost != 0.69 || remote.CostSource != "runpod.adjustedCostPerHr" {
				t.Fatalf("cost = %.2f/%q, want 0.69 adjusted source", remote.HourlyCost, remote.CostSource)
			}
		})
	}
}

func TestClientListBackfillsRemoteRequestShape(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/pods" {
			t.Fatalf("path = %s, want /pods", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{
			"id": "runpod-1",
			"name": "gpu-burst-node-default-pod-123",
			"desiredStatus": "RUNNING",
			"interruptible": true,
			"costPerHr": 0.74,
			"gpu": {"count": 1, "displayName": "A40"},
			"machine": {"dataCenterId": "US-TX-3"}
		}]`))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	remotes, err := client.ListRemoteResources(context.Background())
	if err != nil {
		t.Fatalf("ListRemoteResources() error = %v", err)
	}
	if len(remotes) != 1 {
		t.Fatalf("remotes = %#v, want one remote", remotes)
	}
	request := remotes[0].Request
	if request.Provider != providerName || request.GPUType != "A40" || request.GPUCount != 1 || request.RegionHint != "US-TX-3" {
		t.Fatalf("request shape = %#v, want runpod A40 in US-TX-3", request)
	}
	if request.MarketType != core.MarketTypeSpot {
		t.Fatalf("market type = %q, want spot", request.MarketType)
	}
	if request.ProviderHints[core.AnnotationRunPodDataCenterIDs] != "US-TX-3" {
		t.Fatalf("provider hints = %#v, want datacenter hint", request.ProviderHints)
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

func TestClientDeleteRecoversLostDeleteResponseWhenRemoteIsGone(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests == 1 && r.Method == http.MethodDelete && r.URL.Path == "/pods/runpod-1":
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case requests == 2 && r.Method == http.MethodGet && r.URL.Path == "/pods/runpod-1":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	if err := client.Delete(context.Background(), "runpod-1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
}

func TestClientDeleteClassifiesTransportErrors(t *testing.T) {
	t.Parallel()

	client := NewClient(Config{
		BaseURL: "https://runpod.example",
		APIKey:  "test-token",
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, context.DeadlineExceeded
		})},
	})

	err := client.Delete(context.Background(), "runpod-1")
	if err == nil {
		t.Fatal("Delete() error = nil, want transport error")
	}
	var providerErr *core.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("Delete() error = %T %[1]v, want ProviderError", err)
	}
	if providerErr.Operation != "delete" {
		t.Fatalf("operation = %q, want delete", providerErr.Operation)
	}
	if providerErr.Class != core.ErrorClassTimeout {
		t.Fatalf("class = %s, want %s", providerErr.Class, core.ErrorClassTimeout)
	}
}

func assertPayloadString(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()

	got, ok := payload[key].(string)
	if !ok || got != want {
		t.Fatalf("%s = %#v, want %q", key, payload[key], want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
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
