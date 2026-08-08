package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	burstresource "github.com/aibrix/neocloud-node/pkg/resource"
)

func TestClientCreateLaunchesLambdaInstance(t *testing.T) {
	t.Parallel()

	var gotPayload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/instance-operations/launch" {
			t.Fatalf("path = %s, want /instance-operations/launch", r.URL.Path)
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "lambda-key" || password != "" {
			t.Fatalf("basic auth = %q/%q/%t, want lambda-key empty password", user, password, ok)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotPayload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}

		_, _ = w.Write([]byte(`{"data":{"instance_ids":["lambda-1"]}}`))
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:             server.URL,
		APIKey:              "lambda-key",
		CallbackURL:         "https://controller.example.com/v1/runner/status",
		DefaultRegion:       "us-west-1",
		DefaultInstanceType: "gpu_1x_a10",
		DefaultSSHKeyNames:  []string{"default-key"},
	})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:      "example.com/train:latest",
		Command:    []string{"python"},
		Args:       []string{"train.py"},
		Env:        map[string]string{"MODEL": "llama"},
		GPUCount:   1,
		RegionHint: "us-east-1",
		ProviderHints: map[string]string{
			core.AnnotationLambdaInstanceType:  "gpu_1x_h100_sxm5",
			core.AnnotationLambdaSSHKeyNames:   "job-key",
			core.AnnotationLambdaFileSystemIDs: "fs-1,fs-2",
			core.AnnotationLambdaImageID:       "image-1",
			core.AnnotationLambdaFirewallIDs:   "fw-1,fw-2",
		},
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.ID != "lambda-1" {
		t.Fatalf("remote ID = %q, want lambda-1", remote.ID)
	}
	if remote.Provider != providerName {
		t.Fatalf("remote provider = %q, want %q", remote.Provider, providerName)
	}
	assertPayloadString(t, gotPayload, "region_name", "us-east-1")
	assertPayloadString(t, gotPayload, "instance_type_name", "gpu_1x_h100_sxm5")
	assertPayloadString(t, gotPayload, "name", "gpu-burst-node-default-pod-123")
	assertPayloadStringSlice(t, gotPayload, "ssh_key_names", []string{"job-key"})
	assertPayloadString(t, gotPayload, "user_data", "#cloud-config")
	assertPayloadTags(t, gotPayload, map[string]string{
		"gpu-burst-node:aibrix:workload":   "default/pod-123",
		"gpu-burst-node:aibrix:managed-by": "gpu-burst-node",
	})

	image, ok := gotPayload["image"].(map[string]any)
	if !ok || image["id"] != "image-1" {
		t.Fatalf("image = %#v, want id image-1", gotPayload["image"])
	}
	if len(gotPayload["file_system_mounts"].([]any)) != 2 {
		t.Fatalf("file_system_mounts = %#v, want two mounts", gotPayload["file_system_mounts"])
	}
	if len(gotPayload["firewall_rulesets"].([]any)) != 2 {
		t.Fatalf("firewall_rulesets = %#v, want two rulesets", gotPayload["firewall_rulesets"])
	}
}

func TestClientCreateRecoversDecodeFailureByExternalName(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case requests == 1 && r.Method == http.MethodPost && r.URL.Path == "/instance-operations/launch":
			_, _ = w.Write([]byte(`{"data":`))
		case requests == 2 && r.Method == http.MethodGet && r.URL.Path == "/instances":
			_, _ = w.Write([]byte(`{"data":[{
				"id":"lambda-created",
				"name":"gpu-burst-node-default-pod-123",
				"status":"active",
				"instance_type":{"price_cents_per_hour":74}
			}]}`))
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		BaseURL:             server.URL,
		APIKey:              "lambda-key",
		DefaultRegion:       "us-west-1",
		DefaultInstanceType: "gpu_1x_a10",
		DefaultSSHKeyNames:  []string{"default-key"},
	})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.ID != "lambda-created" {
		t.Fatalf("remote ID = %q, want lambda-created", remote.ID)
	}
	if remote.HourlyCost != 0.74 {
		t.Fatalf("hourly cost = %.2f, want 0.74", remote.HourlyCost)
	}
}

func TestClientGetMapsLambdaStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    string
		wantPhase core.RemotePhase
	}{
		{name: "booting", status: "booting", wantPhase: core.RemotePending},
		{name: "active", status: "active", wantPhase: core.RemoteRunning},
		{name: "unhealthy", status: "unhealthy", wantPhase: core.RemoteFailed},
		{name: "preempted", status: "preempted", wantPhase: core.RemoteFailed},
		{name: "terminated", status: "terminated", wantPhase: core.RemoteDeleted},
		{name: "terminating", status: "terminating", wantPhase: core.RemotePending},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Fatalf("method = %s, want GET", r.Method)
				}
				if r.URL.Path != "/instances/lambda-1" {
					t.Fatalf("path = %s, want /instances/lambda-1", r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"data":{"id":"lambda-1","name":"train","status":"` + tt.status + `","instance_type":{"price_cents_per_hour":1592}}}`))
			}))
			defer server.Close()

			client := NewClient(Config{BaseURL: server.URL, APIKey: "lambda-key"})
			remote, err := client.Get(context.Background(), "lambda-1")
			if err != nil {
				t.Fatalf("Get returned error: %v", err)
			}
			if remote.Phase != tt.wantPhase {
				t.Fatalf("phase = %s, want %s", remote.Phase, tt.wantPhase)
			}
			if math.Abs(remote.HourlyCost-15.92) > 0.000001 || remote.CostSource != "lambda.instance_type.price_cents_per_hour" {
				t.Fatalf("cost = %.2f/%q, want 15.92 lambda source", remote.HourlyCost, remote.CostSource)
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
		if r.URL.Path != "/instances" {
			t.Fatalf("path = %s, want /instances", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{
			"id":"lambda-1",
			"name":"gpu-burst-node-default-pod-123",
			"status":"active",
			"region":{"name":"us-west-1"},
			"instance_type":{
				"name":"gpu_1x_a10",
				"gpu_description":"A10",
				"price_cents_per_hour":74,
				"specs":{"gpus":1}
			}
		}]}`))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "lambda-key"})
	remotes, err := client.ListRemoteResources(context.Background())
	if err != nil {
		t.Fatalf("ListRemoteResources() error = %v", err)
	}
	if len(remotes) != 1 {
		t.Fatalf("remotes = %#v, want one remote", remotes)
	}
	request := remotes[0].Request
	if request.Provider != providerName || request.GPUType != "A10" || request.GPUCount != 1 || request.RegionHint != "us-west-1" {
		t.Fatalf("request shape = %#v, want lambda A10 in us-west-1", request)
	}
	if request.ProviderHints[core.AnnotationLambdaInstanceType] != "gpu_1x_a10" {
		t.Fatalf("provider hints = %#v, want lambda instance type", request.ProviderHints)
	}
}

func TestClientListOffersFromInstanceTypesCatalog(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/instance-types" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{
			"gpu_1x_a10":{
				"instance_type":{
					"name":"gpu_1x_a10",
					"price_cents_per_hour":74,
					"specs":{"gpus":1,"gpu_description":"A10"}
				},
				"regions_with_capacity_available":[{"name":"us-west-1"}]
			},
			"gpu_8x_h100":{
				"instance_type":{
					"name":"gpu_8x_h100",
					"price_cents_per_hour":1272,
					"specs":{"gpus":8,"gpu_description":"H100"}
				},
				"regions_with_capacity_available":[{"name":"us-east-1"}]
			}
		}}`))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "lambda-key"})
	offers, err := client.ListOffers(context.Background(), burstresource.Intent{})
	if err != nil {
		t.Fatalf("ListOffers() error = %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("offers = %#v, want one single-GPU offer", offers)
	}
	offer := offers[0]
	if offer.Provider != providerName || offer.Location != "us-west-1" || offer.GPUType != "A10" || offer.GPUCount != 1 {
		t.Fatalf("offer = %#v, want lambda A10 us-west-1", offer)
	}
	if offer.Market != burstresource.MarketOnDemand || !offer.Available || math.Abs(offer.HourlyPrice-0.74) > 0.000001 {
		t.Fatalf("offer price/market = %#v, want available on-demand 0.74", offer)
	}
	if offer.ProviderHints[core.AnnotationLambdaInstanceType] != "gpu_1x_a10" {
		t.Fatalf("provider hints = %#v, want lambda instance type hint", offer.ProviderHints)
	}
}

func TestClientDeleteTreatsNotFoundAsSuccess(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/instance-operations/terminate" {
			t.Fatalf("path = %s, want terminate", r.URL.Path)
		}
		requests++
		switch requests {
		case 1:
			_, _ = w.Write([]byte(`{"data":{"terminated_instances":[{"id":"lambda-1","status":"terminating"}]}}`))
		case 2:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"global/object-does-not-exist"}}`))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "lambda-key"})
	if err := client.Delete(context.Background(), "lambda-1"); err != nil {
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
		case requests == 1 && r.Method == http.MethodPost && r.URL.Path == "/instance-operations/terminate":
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case requests == 2 && r.Method == http.MethodGet && r.URL.Path == "/instances/lambda-1":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"global/object-does-not-exist"}}`))
		default:
			t.Fatalf("unexpected request %d %s %s", requests, r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, APIKey: "lambda-key"})
	if err := client.Delete(context.Background(), "lambda-1"); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
}

func TestClientClassifiesLambdaErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		wantClass core.ErrorClass
	}{
		{name: "capacity", status: http.StatusBadRequest, body: `{"error":{"code":"instance-operations/launch/insufficient-capacity"}}`, wantClass: core.ErrorClassCapacity},
		{name: "quota", status: http.StatusBadRequest, body: `{"error":{"code":"global/quota-exceeded"}}`, wantClass: core.ErrorClassQuota},
		{name: "auth", status: http.StatusUnauthorized, body: `{"error":{"code":"global/invalid-api-key"}}`, wantClass: core.ErrorClassAuth},
		{name: "rate", status: http.StatusTooManyRequests, body: `{}`, wantClass: core.ErrorClassRateLimited},
		{name: "server", status: http.StatusBadGateway, body: `{}`, wantClass: core.ErrorClassTransient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(Config{BaseURL: server.URL, APIKey: "lambda-key"})
			_, err := client.Get(context.Background(), "lambda-1")
			var providerErr *core.ProviderError
			if !errors.As(err, &providerErr) {
				t.Fatalf("Get error = %T %[1]v, want ProviderError", err)
			}
			if providerErr.Class != tt.wantClass {
				t.Fatalf("class = %s, want %s", providerErr.Class, tt.wantClass)
			}
		})
	}
}

func assertPayloadString(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()

	got, ok := payload[key].(string)
	if !ok {
		t.Fatalf("%s = %#v, want string", key, payload[key])
	}
	if key == "user_data" {
		if len(got) < len(want) || got[:len(want)] != want {
			t.Fatalf("%s = %q, want prefix %q", key, got, want)
		}
		return
	}
	if got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
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

func assertPayloadTags(t *testing.T, payload map[string]any, want map[string]string) {
	t.Helper()

	raw, ok := payload["tags"].([]any)
	if !ok {
		t.Fatalf("tags = %#v, want object array", payload["tags"])
	}
	got := make(map[string]string, len(raw))
	for _, item := range raw {
		tag, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tag item = %#v, want object", item)
		}
		key, _ := tag["key"].(string)
		value, _ := tag["value"].(string)
		got[key] = value
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("tag %q = %q, want %q; tags=%#v", key, got[key], value, got)
		}
	}
}
