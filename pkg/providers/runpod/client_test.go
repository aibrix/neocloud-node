package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

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
			"status": "RUNNING",
			"image": "example.com/train:latest",
			"cost": 0.44,
			"gpu": {"count": 1, "id": "NVIDIA H100"}
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
		MarketType: core.MarketTypeOnDemand,
		ProviderHints: map[string]string{
			core.AnnotationRunPodTemplateID:      "template-1",
			core.AnnotationRunPodDataCenterIDs:   "US-TX-3,EU-RO-1",
			core.AnnotationRunPodNetworkVolumeID: "volume-1",
			core.AnnotationRunPodRegistryAuthID:  "registry-1",
			core.AnnotationRunPodVolumeMountPath: "/workspace",
			core.AnnotationRunPodCloudType:       "COMMUNITY",
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
	if remote.CostSource != "runpod.cost" {
		t.Fatalf("remote cost source = %q, want runpod.cost", remote.CostSource)
	}

	assertPayloadString(t, gotPayload, "name", "neocloud-node-default-pod-123")
	assertPayloadString(t, gotPayload, "image", "example.com/train:latest")
	assertPayloadStringSlice(t, gotPayload, "entrypoint", []string{"python"})
	assertPayloadStringSlice(t, gotPayload, "cmd", []string{"train.py"})
	assertPayloadStringSlice(t, gotPayload, "ports", []string{"9090/tcp"})
	assertPayloadStringSlice(t, gotPayload, "dataCenterIds", []string{"US-TX-3", "EU-RO-1"})
	assertPayloadString(t, gotPayload, "templateId", "template-1")
	assertPayloadString(t, gotPayload, "registry", "registry-1")
	assertPayloadString(t, gotPayload, "cloud", "COMMUNITY")

	if got := gotPayload["gpu"]; !reflect.DeepEqual(got, map[string]any{"id": "NVIDIA H100", "count": float64(1)}) {
		t.Fatalf("gpu = %#v", got)
	}
	wantMounts := map[string]any{"network": []any{map[string]any{"volumeId": "volume-1", "path": "/workspace"}}}
	if !reflect.DeepEqual(gotPayload["mounts"], wantMounts) {
		t.Fatalf("mounts = %#v", gotPayload["mounts"])
	}
	for _, field := range []string{"interruptible", "computeType", "gpuCount", "gpuTypeIds", "dataCenterPriority", "networkVolumeId", "volumeMountPath", "args"} {
		if _, ok := gotPayload[field]; ok {
			t.Errorf("unexpected field %s", field)
		}
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
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"pods":[{
				"id": "runpod-existing",
				"name": "neocloud-node-default-pod-123",
				"status": "RUNNING",
				"image": "example.com/train:latest"
			}],"pagination":{"nextCursor":null,"hasNextPage":false}}`))
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
		GPUType:  "NVIDIA A40",
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
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"pods":[{
				"id": "runpod-created",
				"name": "neocloud-node-default-pod-123",
				"status": "RUNNING",
				"image": "example.com/train:latest",
				"cost": 0.44
			}],"pagination":{"nextCursor":null,"hasNextPage":false}}`))
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
		GPUType:  "NVIDIA A40",
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

func TestClientCreateMapsCommandAndArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		command        []string
		args           []string
		wantEntrypoint []string
		wantStartCmd   []string
	}{
		{name: "image defaults"},
		{name: "args only", args: []string{"--port", "8000"}, wantStartCmd: []string{"--port", "8000"}},
		{
			name:           "command and args",
			command:        []string{"python3", "-m", "server"},
			args:           []string{"--port", "8000"},
			wantEntrypoint: []string{"python3", "-m", "server"},
			wantStartCmd:   []string{"--port", "8000"},
		},
		{
			name:           "command only replaces the image CMD",
			command:        []string{"sh", "-c", "echo ready; sleep 60"},
			wantEntrypoint: []string{"sh", "-c", "echo ready; sleep 60"},
			wantStartCmd:   []string{},
		},
		{name: "single-element command", command: []string{"/start.sh"}, wantEntrypoint: []string{"/start.sh"}, wantStartCmd: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var payload map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Errorf("decode request body: %v", err)
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"runpod-1","status":"RUNNING"}`))
			}))
			defer server.Close()

			client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
			if _, err := client.Create(context.Background(), "default/pod-1", core.WorkloadRequest{
				Image:    "example.com/serve:latest",
				Command:  tt.command,
				Args:     tt.args,
				GPUCount: 1,
				GPUType:  "NVIDIA A40",
			}); err != nil {
				t.Fatalf("Create returned error: %v", err)
			}
			assertOptionalStringSlice(t, payload, "entrypoint", tt.wantEntrypoint)
			assertOptionalStringSlice(t, payload, "cmd", tt.wantStartCmd)
		})
	}
}

// assertOptionalStringSlice expects the key to be absent when want is empty,
// because RunPod rejects null.
func assertOptionalStringSlice(t *testing.T, payload map[string]any, key string, want []string) {
	t.Helper()

	if want == nil {
		if value, ok := payload[key]; ok {
			t.Fatalf("%s = %#v, want field omitted", key, value)
		}
		return
	}
	assertPayloadStringSlice(t, payload, key, want)
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
		{name: "insufficient balance", statusCode: http.StatusPaymentRequired, body: `{"title":"Payment Required","status":402,"detail":"Insufficient balance"}`, wantClass: core.ErrorClassQuota},
		{name: "validation mentions capacity", statusCode: http.StatusUnprocessableEntity, body: `{"title":"Unprocessable Entity","status":422,"detail":"Invalid capacity field"}`, wantClass: core.ErrorClassUnsupportedSpec},
		{name: "auth", statusCode: http.StatusUnauthorized, body: `{"title":"Unauthorized","status":401,"detail":"Invalid credentials"}`, wantClass: core.ErrorClassAuth},
		{name: "capacity", statusCode: http.StatusBadRequest, body: `{"title":"Bad Request","status":400,"detail":"Insufficient capacity"}`, wantClass: core.ErrorClassCapacity},
		{name: "server error remains transient", statusCode: http.StatusInternalServerError, body: `{"title":"Internal Server Error","status":500,"detail":"There are no longer any instances available with the request specifications."}`, wantClass: core.ErrorClassTransient},
		{name: "not found", statusCode: http.StatusNotFound, body: `{"title":"Not Found","status":404,"detail":"Pod not found"}`, wantClass: core.ErrorClassRemoteNotFound, wantIsNotFound: true},
		{name: "rate limited", statusCode: http.StatusTooManyRequests, body: `{"title":"Too Many Requests","status":429,"detail":"Rate limited"}`, wantClass: core.ErrorClassRateLimited},
		{name: "server error", statusCode: http.StatusBadGateway, body: `{"title":"Bad Gateway","status":502,"detail":"Upstream failed"}`, wantClass: core.ErrorClassTransient},
		{
			name:       "invalid request",
			statusCode: http.StatusUnprocessableEntity,
			body:       `{"title":"Unprocessable Entity","status":422,"detail":"Invalid request","errors":["cmd must be an array"]}`,
			wantClass:  core.ErrorClassUnsupportedSpec,
		},
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

func TestClientGetMapsStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     string
		wantPhase  core.RemotePhase
		wantReason string
	}{
		{name: "running", status: "RUNNING", wantPhase: core.RemoteRunning},
		{name: "exited without exit code", status: "EXITED", wantPhase: core.RemoteFailed, wantReason: ReasonRemoteExited},
		{name: "terminated", status: "TERMINATED", wantPhase: core.RemoteDeleted},
		{name: "starting", status: "STARTING", wantPhase: core.RemotePending},
		{name: "provisioning", status: "PROVISIONING", wantPhase: core.RemotePending},
		{name: "error", status: "ERROR", wantPhase: core.RemoteFailed, wantReason: "RemoteError"},
		{name: "unknown", status: "FUTURE_STATUS", wantPhase: core.RemotePending},
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
					"status": "` + tt.status + `",
					"image": "example.com/train:latest",
					"cost": 0.69
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
			if remote.StatusReason != tt.wantReason {
				t.Fatalf("remote status reason = %q, want %q", remote.StatusReason, tt.wantReason)
			}
			if tt.wantReason != "" && remote.StatusMessage == "" {
				t.Fatal("remote status message is empty, want an explanation")
			}
			if remote.HourlyCost != 0.69 || remote.CostSource != "runpod.cost" {
				t.Fatalf("cost = %.2f/%q, want 0.69 runpod.cost", remote.HourlyCost, remote.CostSource)
			}
		})
	}
}

func TestClientGetMapsPublicEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want []core.Endpoint
	}{
		{
			name: "remapped ports",
			body: `{"id":"runpod-1","status":"RUNNING","runtime":{"ports":[
    {"private":8000,"public":40123,"type":"tcp","ip":"203.0.113.10"},
    {"private":22,"public":40001,"type":"tcp","ip":"203.0.113.10"},
    {"private":9090,"public":40500,"type":"tcp","ip":"203.0.113.11"}]}}`,
			want: []core.Endpoint{
				{ContainerPort: 22, Protocol: "TCP", Host: "203.0.113.10", Port: 40001},
				{ContainerPort: 8000, Protocol: "TCP", Host: "203.0.113.10", Port: 40123},
				{ContainerPort: 9090, Protocol: "TCP", Host: "203.0.113.11", Port: 40500},
			},
		},
		{name: "address not assigned yet", body: `{"id":"runpod-1","status":"STARTING","runtime":null}`},
		{name: "nonpublic and non-TCP ports ignored", body: `{"id":"runpod-1","status":"RUNNING","runtime":{"ports":[
   {"private":8888,"public":null,"type":"http","ip":null},
   {"private":22,"public":40001,"type":"tcp","ip":null},
   {"private":8000,"public":0,"type":"tcp","ip":"203.0.113.10"},
   {"private":9000,"public":40123,"type":"udp","ip":"203.0.113.10"}]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"})
			remote, err := client.Get(context.Background(), "runpod-1")
			if err != nil {
				t.Fatalf("Get returned error: %v", err)
			}
			if !reflect.DeepEqual(remote.Endpoints, tt.want) {
				t.Fatalf("endpoints = %#v, want %#v", remote.Endpoints, tt.want)
			}
		})
	}
}

func TestClientGetReadsContainerUptime(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, body string
		want       time.Duration
	}{
		{"running", `{"status":"RUNNING","runtime":{"uptime":90}}`, 90 * time.Second},
		{"starting", `{"status":"STARTING","runtime":{"uptime":90}}`, 0},
		{"no runtime", `{"status":"RUNNING","runtime":null}`, 0},
		{"zero uptime", `{"status":"RUNNING","runtime":{"uptime":0}}`, 0},
		{"negative uptime", `{"status":"RUNNING","runtime":{"uptime":-1}}`, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v2/pods/runpod-1" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			remote, err := NewClient(Config{BaseURL: server.URL + "/v2"}).Get(context.Background(), "runpod-1")
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || remote.ContainerUptime != tt.want {
				t.Fatalf("calls=%d uptime=%s, want 1/%s", calls, remote.ContainerUptime, tt.want)
			}
		})
	}
}

func TestClientBaseURLDefaults(t *testing.T) {
	t.Parallel()
	if got := NewClient(Config{}).baseURL; got != "https://api.runpod.io/v2" {
		t.Fatalf("base URL = %q", got)
	}
	if got := NewClient(Config{BaseURL: "http://127.0.0.1:1/v2/"}).baseURL; got != "http://127.0.0.1:1/v2" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestClientStop(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "stopped", status: http.StatusOK},
		{name: "already gone", status: http.StatusNotFound},
		{name: "server error", status: http.StatusBadGateway, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/pods/runpod-1/action" {
					t.Errorf("request = %s %s, want POST /pods/runpod-1/action", r.Method, r.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !reflect.DeepEqual(body, map[string]string{"action": "stop"}) {
					t.Errorf("stop body = %#v, err %v", body, err)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"id":"runpod-1","status":"EXITED"}`))
			}))
			defer server.Close()

			err := NewClient(Config{BaseURL: server.URL, APIKey: "test-token"}).Stop(context.Background(), "runpod-1")
			if (err != nil) != tt.wantErr {
				t.Fatalf("Stop() error = %v, wantErr %t", err, tt.wantErr)
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
		_, _ = w.Write([]byte(`{"pods":[{
			"id": "runpod-1",
			"name": "neocloud-node-default-pod-123",
			"status": "RUNNING",
			"cost": 0.74,
			"gpu": {"count": 1, "id": "A40"},
			"dataCenterId": "US-TX-3"
		}],"pagination":{"nextCursor":null,"hasNextPage":false}}`))
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
	if request.MarketType != core.MarketTypeOnDemand {
		t.Fatalf("market type = %q, want on-demand", request.MarketType)
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
