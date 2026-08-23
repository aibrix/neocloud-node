package vast

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	burstresource "github.com/aibrix/neocloud-node/pkg/resource"
)

func TestClientCreateSearchesAndRentsCheapestMatchingOffer(t *testing.T) {
	t.Parallel()

	requests := 0
	var searchBody map[string]any
	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("authorization = %q, want bearer token", got)
		}
		switch {
		case requests == 1 && r.Method == http.MethodGet && r.URL.Path == "/instances/":
			assertLabelFilter(t, r.URL.Query(), "neocloud-node-default-pod-123")
			_, _ = w.Write([]byte(`{"instances":[],"next_token":null}`))
		case requests == 2 && r.Method == http.MethodPost && r.URL.Path == "/bundles/":
			if err := json.NewDecoder(r.Body).Decode(&searchBody); err != nil {
				t.Fatalf("decode search: %v", err)
			}
			_, _ = w.Write([]byte(`{
				"offers":[
					{"id":124,"gpu_name":"RTX 4090","num_gpus":1,"dph_total":0.42,"reliability":0.99},
					{"id":123,"gpu_name":"RTX 4090","num_gpus":1,"dph_total":0.31,"reliability":0.98,"machine_id":77}
				]
			}`))
		case requests == 3 && r.Method == http.MethodPut && r.URL.Path == "/asks/123/":
			if err := json.NewDecoder(r.Body).Decode(&createBody); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			_, _ = w.Write([]byte(`{"success":true,"new_contract":9001}`))
		default:
			t.Fatalf("unexpected request %d: %s %s", requests, r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "test-token", DiskGB: 20, MinimumReliability: 0.96})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Image:      "python:3.12-slim",
		Command:    []string{"sh", "-c"},
		Args:       []string{"echo ready; sleep 300"},
		Env:        map[string]string{"MODEL": "test"},
		GPUCount:   1,
		GPUType:    "RTX_4090",
		GPUMemory:  "20Gi",
		MaxPrice:   "0.50",
		MarketType: core.MarketTypeOnDemand,
		RegionHint: "US",
		Ports: []core.Port{
			{ContainerPort: 8080, Protocol: "TCP"},
			{ContainerPort: 5353, Protocol: "UDP"},
		},
		ProviderHints: map[string]string{
			core.AnnotationVastVerified: "false",
			core.AnnotationVastDiskGB:   "25",
		},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if remote.ID != "9001" || remote.ExternalName != "neocloud-node-default-pod-123" || remote.Provider != providerName {
		t.Fatalf("remote identity = %#v", remote)
	}
	if remote.Phase != core.RemotePending {
		t.Fatalf("phase = %s, want Pending", remote.Phase)
	}
	if math.Abs(remote.HourlyCost-0.31) > 0.000001 || remote.CostSource != "vast.dph_total" {
		t.Fatalf("cost = %f/%q, want 0.31 vast.dph_total", remote.HourlyCost, remote.CostSource)
	}

	assertSearchOperator(t, searchBody, "num_gpus", "eq", float64(1))
	assertSearchOperator(t, searchBody, "reliability", "gte", 0.96)
	assertSearchOperator(t, searchBody, "disk_space", "gte", float64(25))
	assertSearchOperator(t, searchBody, "gpu_ram", "gte", float64(20))
	assertSearchOperator(t, searchBody, "dph_total", "lte", 0.5)
	assertSearchOperator(t, searchBody, "verified", "eq", false)
	assertSearchOperator(t, searchBody, "geolocation", "eq", "US")
	assertSearchOperator(t, searchBody, "direct_port_count", "gte", float64(2))
	if got := searchBody["type"]; got != "on-demand" {
		t.Fatalf("search type = %#v, want on-demand", got)
	}
	assertStringSliceValue(t, searchBody["gpu_name"].(map[string]any)["in"], []string{"RTX 4090", "RTX_4090"})

	assertMapString(t, createBody, "client_id", "me")
	assertMapString(t, createBody, "image", "python:3.12-slim")
	assertMapString(t, createBody, "label", "neocloud-node-default-pod-123")
	assertMapString(t, createBody, "runtype", "args")
	assertMapString(t, createBody, "onstart", "sh")
	assertStringSliceValue(t, createBody["args"], []string{"-c", "echo ready; sleep 300"})
	if createBody["disk"] != float64(25) || createBody["cancel_unavail"] != true {
		t.Fatalf("create disk/cancel = %#v/%#v", createBody["disk"], createBody["cancel_unavail"])
	}
	environment, ok := createBody["env"].(map[string]any)
	if !ok {
		t.Fatalf("env = %#v, want object", createBody["env"])
	}
	if environment["MODEL"] != "test" || environment["-p 8080:8080"] != "1" || environment["-p 5353:5353/udp"] != "1" {
		t.Fatalf("env = %#v", environment)
	}
}

func TestClientCreateUsesExistingDeterministicInstance(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/instances/" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"instances":[{"id":91,"label":"neocloud-node-default-pod-123","actual_status":"running","dph_total":0.22}]}`))
	}))
	defer server.Close()

	request := core.WorkloadRequest{Image: "python:3.12-slim", GPUCount: 1, Provider: providerName}
	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	remote, err := client.Create(context.Background(), "default/pod-123", request)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if requests != 1 || remote.ID != "91" || remote.IdempotencyKey != "default/pod-123" {
		t.Fatalf("requests/remote = %d/%#v", requests, remote)
	}
	if !reflect.DeepEqual(remote.Request, request) {
		t.Fatalf("request = %#v, want %#v", remote.Request, request)
	}
}

func TestClientCreateRecoversLostResponseByExternalName(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			_, _ = w.Write([]byte(`{"instances":[]}`))
		case 2:
			_, _ = w.Write([]byte(`{"offers":[{"id":123,"dph_total":0.20,"num_gpus":1}]}`))
		case 3:
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case 4:
			_, _ = w.Write([]byte(`{"instances":[{"id":92,"label":"neocloud-node-default-pod-123","actual_status":"loading","dph_total":0.20}]}`))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	remote, err := client.Create(context.Background(), "default/pod-123", core.WorkloadRequest{Image: "python:3.12-slim", GPUCount: 1})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if remote.ID != "92" || requests != 4 {
		t.Fatalf("remote/requests = %#v/%d", remote, requests)
	}
}

func TestClientCreateInterruptibleUsesBidFloor(t *testing.T) {
	t.Parallel()

	requests := 0
	var searchBody map[string]any
	var createBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			_, _ = w.Write([]byte(`{"instances":[]}`))
		case 2:
			_ = json.NewDecoder(r.Body).Decode(&searchBody)
			_, _ = w.Write([]byte(`{"offers":[{"id":321,"min_bid":0.08,"dph_total":0.12,"num_gpus":1}]}`))
		case 3:
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_, _ = w.Write([]byte(`{"success":true,"new_contract":93}`))
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	remote, err := client.Create(context.Background(), "default/spot", core.WorkloadRequest{
		Image: "python:3.12-slim", GPUCount: 1, MarketType: core.MarketTypeSpot, MaxPrice: "0.10",
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if searchBody["type"] != "bid" || createBody["price"] != 0.08 {
		t.Fatalf("search type/create price = %#v/%#v", searchBody["type"], createBody["price"])
	}
	assertSearchOperator(t, searchBody, "min_bid", "lte", 0.1)
	if remote.HourlyCost != 0.08 || remote.CostSource != "vast.min_bid" {
		t.Fatalf("remote cost = %#v", remote)
	}
}

func TestClientGetMapsStatusShapeAndNullNotFound(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			_, _ = w.Write([]byte(`{"instances":{
				"id":501,"label":"neocloud-node-default-pod","actual_status":"running",
				"gpu_name":"GTX 1070","num_gpus":"1","gpu_ram":8,"dph_total":"0.05222",
				"geolocation":"KR","machine_id":27850,"verification":"verified","ask_contract_id":801
			}}`))
			return
		}
		_, _ = w.Write([]byte(`{"instances":null}`))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	remote, err := client.Get(context.Background(), "501")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if remote.Phase != core.RemoteRunning || remote.HourlyCost != 0.05222 || remote.CostSource != "vast.dph_total" {
		t.Fatalf("remote status/cost = %#v", remote)
	}
	if remote.Request.Provider != providerName || remote.Request.GPUType != "GTX 1070" || remote.Request.GPUCount != 1 || remote.Request.RegionHint != "KR" {
		t.Fatalf("request shape = %#v", remote.Request)
	}
	if remote.Request.ProviderHints[core.AnnotationVastMachineID] != "27850" || remote.Request.ProviderHints[core.AnnotationVastOfferID] != "801" {
		t.Fatalf("provider hints = %#v", remote.Request.ProviderHints)
	}

	_, err = client.Get(context.Background(), "missing")
	if !errors.Is(err, core.ErrRemoteNotFound) || core.ClassifyError(err) != core.ErrorClassRemoteNotFound {
		t.Fatalf("missing error = %T %[1]v", err)
	}
}

func TestClientGetMapsTerminalStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status string
		phase  core.RemotePhase
	}{
		{status: "loading", phase: core.RemotePending},
		{status: "frozen", phase: core.RemoteRunning},
		{status: "exited", phase: core.RemoteFailed},
		{status: "offline", phase: core.RemoteFailed},
		{status: "destroyed", phase: core.RemoteDeleted},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			if got := phaseFromInstance(instance{ActualStatus: test.status}); got != test.phase {
				t.Fatalf("phase = %s, want %s", got, test.phase)
			}
		})
	}
}

func TestClientListRemoteResourcesPaginates(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if got := r.URL.Query().Get("after_token"); got != "" {
				t.Fatalf("first after_token = %q", got)
			}
			_, _ = w.Write([]byte(`{"instances":[{"id":1,"label":"one","actual_status":"running"}],"next_token":"page-2"}`))
			return
		}
		if got := r.URL.Query().Get("after_token"); got != "page-2" {
			t.Fatalf("second after_token = %q", got)
		}
		_, _ = w.Write([]byte(`{"instances":[{"id":2,"label":"two","actual_status":"loading"}],"next_token":null}`))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	remotes, err := client.ListRemoteResources(context.Background())
	if err != nil {
		t.Fatalf("ListRemoteResources() error = %v", err)
	}
	if len(remotes) != 2 || remotes[0].ID != "1" || remotes[1].ID != "2" {
		t.Fatalf("remotes = %#v", remotes)
	}
}

func TestClientDeleteIsIdempotentAndRecoversUncertainDelete(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			w.WriteHeader(http.StatusNotFound)
		case 2:
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case 3:
			_, _ = w.Write([]byte(`{"instances":null}`))
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	if err := client.Delete(context.Background(), "missing"); err != nil {
		t.Fatalf("Delete(missing) error = %v", err)
	}
	if err := client.Delete(context.Background(), "gone-after-timeout"); err != nil {
		t.Fatalf("Delete(uncertain) error = %v", err)
	}
}

func TestClientClassifiesVastErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
		body   string
		class  core.ErrorClass
	}{
		{name: "auth", status: 401, body: `{"error":"bad key"}`, class: core.ErrorClassAuth},
		{name: "stale ask", status: 410, body: `{"error":"no_such_ask"}`, class: core.ErrorClassCapacity},
		{name: "already rented", status: 400, body: `{"error":"offer already rented"}`, class: core.ErrorClassCapacity},
		{name: "credit", status: 400, body: `{"error":"insufficient credit"}`, class: core.ErrorClassQuota},
		{name: "bad arguments", status: 400, body: `{"error":"invalid_args"}`, class: core.ErrorClassUnsupportedSpec},
		{name: "rate", status: 429, body: `{"error":"slow down"}`, class: core.ErrorClassRateLimited},
		{name: "server", status: 502, body: `{"error":"upstream"}`, class: core.ErrorClassTransient},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyHTTPStatus(test.status, test.body); got != test.class {
				t.Fatalf("class = %s, want %s", got, test.class)
			}
		})
	}
}

func TestClientListOffersMapsCatalogAndOfferHints(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requests == 1 && body["type"] != "on-demand" {
			t.Fatalf("first search type = %#v", body["type"])
		}
		if requests == 2 && body["type"] != "bid" {
			t.Fatalf("second search type = %#v", body["type"])
		}
		priceField := `"dph_total":0.30`
		if requests == 2 {
			priceField = `"min_bid":0.12`
		}
		_, _ = w.Write([]byte(`{"offers":[{"id":71,"gpu_name":"RTX 4090","num_gpus":1,"gpu_ram":24,` + priceField + `,"machine_id":8,"verified":true,"geolocation":"US"}]}`))
	}))
	defer server.Close()

	client := NewClient(Config{BaseURL: server.URL, V1BaseURL: server.URL, APIKey: "token"})
	offers, err := client.ListOffers(context.Background(), burstresource.Intent{GPUCount: 1, Market: burstresource.MarketEither})
	if err != nil {
		t.Fatalf("ListOffers() error = %v", err)
	}
	if len(offers) != 2 || offers[0].Market != burstresource.MarketOnDemand || offers[1].Market != burstresource.MarketSpot {
		t.Fatalf("offers = %#v", offers)
	}
	if offers[0].ProviderHints[core.AnnotationVastOfferID] != "71" || offers[0].ProviderHints[core.AnnotationVastMachineID] != "8" {
		t.Fatalf("provider hints = %#v", offers[0].ProviderHints)
	}
	if offers[0].GPUMemory != "24Gi" || !offers[0].Capabilities[burstresource.CapabilityPorts] {
		t.Fatalf("catalog shape = %#v", offers[0])
	}
}

func assertLabelFilter(t *testing.T, query url.Values, want string) {
	t.Helper()
	var filters map[string]map[string][]string
	if err := json.Unmarshal([]byte(query.Get("select_filters")), &filters); err != nil {
		t.Fatalf("decode select_filters: %v", err)
	}
	if got := filters["label"]["in"]; !reflect.DeepEqual(got, []string{want}) {
		t.Fatalf("label filter = %#v, want %q", got, want)
	}
}

func assertSearchOperator(t *testing.T, payload map[string]any, field string, operator string, want any) {
	t.Helper()
	filter, ok := payload[field].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want filter object", field, payload[field])
	}
	if got := filter[operator]; got != want {
		t.Fatalf("%s.%s = %#v, want %#v", field, operator, got, want)
	}
}

func assertMapString(t *testing.T, payload map[string]any, key string, want string) {
	t.Helper()
	if got := payload[key]; got != want {
		t.Fatalf("%s = %#v, want %q", key, got, want)
	}
}

func assertStringSliceValue(t *testing.T, raw any, want []string) {
	t.Helper()
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("value = %#v, want string array", raw)
	}
	got := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if !ok {
			t.Fatalf("array item = %#v, want string", value)
		}
		got = append(got, text)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("array = %#v, want %#v", got, want)
	}
}
