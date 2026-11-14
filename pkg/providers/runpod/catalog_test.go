package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	burstresource "github.com/aibrix/neocloud-node/pkg/resource"
)

const catalogFixture = `{"gpus":[{
	"id":"NVIDIA GeForce RTX 4090", "name":"RTX 4090", "memory":24,
	"secure":true, "community":true,
	"price":{"secure":0.44,"community":0.31},
	"maxCount":{"secure":8,"community":4}, "availability":"HIGH",
	"dataCenters":[{"id":"US-KS-2","availability":"LOW"},{"id":"US-TX-3","availability":"NONE"}]
}]}`

func TestClientListOffersMapsCatalog(t *testing.T) {
	t.Parallel()
	var clouds []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v2/catalog/gpus" {
			t.Errorf("request = %s %s, want GET /v2/catalog/gpus", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer catalog-test-key" {
			t.Error("missing catalog bearer authentication")
		}
		query := r.URL.Query()
		cloud := query.Get("cloud")
		if query.Get("include") != "AVAILABILITY" || query.Get("product") != "POD" || query.Get("count") != "2" {
			t.Errorf("unexpected query: %v", query)
		}
		clouds = append(clouds, cloud)
		body := catalogFixture
		if cloud == "COMMUNITY" {
			body = strings.ReplaceAll(body, "US-KS-2", "EU-RO-1")
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	client := NewClient(Config{BaseURL: server.URL + "/v2", APIKey: "catalog-test-key"})
	intent := burstresource.Intent{GPUCount: 2, GPUType: "NVIDIA GeForce RTX 4090"}
	offers, err := client.ListOffers(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	if client.Provider() != "runpod" || !reflect.DeepEqual(clouds, []string{"SECURE", "COMMUNITY"}) || len(offers) != 4 {
		t.Fatalf("provider/clouds/offers = %s/%v/%#v", client.Provider(), clouds, offers)
	}
	ids := make(map[string]bool)
	for i, offer := range offers {
		cloud, location, price := "SECURE", "US-KS-2", 0.88
		if i >= 2 {
			cloud, location, price = "COMMUNITY", "EU-RO-1", 0.62
		}
		if i%2 == 1 {
			location = "US-TX-3"
		}
		wantID := fmt.Sprintf("runpod:%s:%s:%s:2", intent.GPUType, cloud, location)
		if ids[offer.ID] || offer.ID != wantID || offer.Provider != "runpod" || offer.Location != location || offer.GPUType != intent.GPUType || offer.GPUCount != 2 || offer.GPUMemory != "24Gi" {
			t.Fatalf("unexpected offer shape: %#v", offer)
		}
		ids[offer.ID] = true
		if offer.Market != burstresource.MarketOnDemand || offer.Available != (i%2 == 0) || math.Abs(offer.HourlyPrice-price) > 1e-9 {
			t.Fatalf("unexpected price/market/stock: %#v", offer)
		}
		if !offer.Capabilities[burstresource.CapabilityPorts] || !offer.Capabilities[burstresource.CapabilityRegistryAuth] || offer.Capabilities[burstresource.CapabilityNetworkVolume] != (cloud == "SECURE") || offer.Capabilities[burstresource.CapabilityCloudInit] {
			t.Fatalf("unexpected capabilities: %v", offer.Capabilities)
		}
		payload, err := newCreatePodRequest("catalog-test", core.WorkloadRequest{
			GPUType: offer.GPUType, GPUCount: offer.GPUCount, ProviderHints: offer.ProviderHints,
		})
		if err != nil || payload.Cloud != cloud || !reflect.DeepEqual(payload.DataCenterIDs, []string{location}) || payload.GPU.ID != intent.GPUType || payload.GPU.Count != 2 {
			t.Fatalf("offer cannot reproduce create placement: %#v, %v", payload, err)
		}
	}
}

func TestClientListOffersFiltersCatalog(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		intent  burstresource.Intent
		old     string
		new     string
		want    int
		inStock bool
	}{
		{name: "default count", want: 4, inStock: true},
		{name: "GPU ID", intent: burstresource.Intent{GPUType: "NVIDIA GeForce RTX 4090"}, want: 4, inStock: true},
		{name: "whitespace GPU type", intent: burstresource.Intent{GPUType: " "}, want: 4, inStock: true},
		{name: "other GPU", intent: burstresource.Intent{GPUType: "NVIDIA A40"}},
		{name: "display name is not an ID", intent: burstresource.Intent{GPUType: "RTX 4090"}},
		{name: "secure count ceiling", intent: burstresource.Intent{GPUCount: 8}, want: 2, inStock: true},
		{name: "beyond count ceiling", intent: burstresource.Intent{GPUCount: 9}},
		{name: "unsupported tier", old: `"community":true`, new: `"community":false`, want: 2, inStock: true},
		{name: "missing tier price", old: `"community":0.31`, new: `"unused":0.31`, want: 2, inStock: true},
		{name: "null tier price", old: `"community":0.31`, new: `"community":null`, want: 2, inStock: true},
		{name: "zero tier price", old: `"community":0.31`, new: `"community":0`, want: 2, inStock: true},
		{name: "negative tier price", old: `"community":0.31`, new: `"community":-1`, want: 2, inStock: true},
		{name: "missing GPU ID", old: `"id":"NVIDIA GeForce RTX 4090"`, new: `"id":""`},
		{name: "empty datacenters", old: `"id":"US-KS-2"`, new: `"id":""`, want: 2},
		{name: "overall NONE", old: `"availability":"HIGH"`, new: `"availability":"NONE"`, want: 4},
		{name: "unknown overall stock", old: `"availability":"HIGH"`, new: `"availability":"UNKNOWN"`, want: 4},
		{name: "missing overall stock", old: `"availability":"HIGH",`, new: ``, want: 4},
		{name: "unknown datacenter stock", old: `"availability":"LOW"`, new: `"availability":"UNKNOWN"`, want: 4},
		{name: "missing datacenter stock", old: `,"availability":"LOW"`, new: ``, want: 4},
		{name: "medium datacenter stock", old: `"availability":"LOW"`, new: `"availability":"MEDIUM"`, want: 4, inStock: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := catalogFixture
			if tt.old != "" {
				body = strings.Replace(body, tt.old, tt.new, 1)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				count := tt.intent.GPUCount
				if count <= 0 {
					count = 1
				}
				if r.URL.Query().Get("count") != fmt.Sprint(count) {
					t.Errorf("count query = %s, want %d", r.URL.Query().Get("count"), count)
				}
				fmt.Fprint(w, body)
			}))
			defer server.Close()
			offers, err := NewClient(Config{BaseURL: server.URL}).ListOffers(context.Background(), tt.intent)
			if err != nil || len(offers) != tt.want {
				t.Fatalf("ListOffers = %#v, %v; want %d offers", offers, err, tt.want)
			}
			available := false
			for _, offer := range offers {
				available = available || offer.Available
			}
			if available != tt.inStock {
				t.Fatalf("any available = %v, want %v", available, tt.inStock)
			}
		})
	}
}

func TestClientListOffersResponses(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		status int
		body   string
		class  core.ErrorClass
	}{
		{"empty", 200, `{"gpus":[]}`, ""},
		{"no datacenters", 200, `{"gpus":[{"id":"NVIDIA A40","secure":true,"price":{"secure":0.5},"maxCount":{"secure":8},"availability":"NONE"}]}`, ""},
		{"community stock without datacenters", 200, `{"gpus":[{"id":"NVIDIA GeForce RTX 4090","community":true,"price":{"community":0.34},"maxCount":{"community":8},"availability":"LOW"}]}`, ""},
		{"missing gpus", 200, `{}`, core.ErrorClassTransient},
		{"null gpus", 200, `{"gpus":null}`, core.ErrorClassTransient},
		{"malformed JSON", 200, `{"gpus":`, core.ErrorClassTransient},
		{"wrong shape", 200, `{"gpus":{}}`, core.ErrorClassTransient},
		{"unauthorized", 401, `{"title":"Unauthorized"}`, core.ErrorClassAuth},
		{"forbidden", 403, `{"title":"Forbidden"}`, core.ErrorClassAuth},
		{"rate limited", 429, `{"title":"Too Many Requests"}`, core.ErrorClassRateLimited},
		{"server failure", 500, `{"title":"Internal Server Error"}`, core.ErrorClassTransient},
		{"service unavailable", 503, `{"title":"Service Unavailable"}`, core.ErrorClassTransient},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				fmt.Fprint(w, tt.body)
			}))
			defer server.Close()
			offers, err := NewClient(Config{BaseURL: server.URL}).ListOffers(context.Background(), burstresource.Intent{})
			if tt.class == "" {
				if err != nil || len(offers) != 0 {
					t.Fatalf("expected empty catalog; got %v, %v", offers, err)
				}
				return
			}
			var providerErr *core.ProviderError
			if !errors.As(err, &providerErr) || providerErr.Provider != "runpod" || core.ClassifyError(err) != tt.class || len(offers) != 0 {
				t.Fatalf("expected %s error and no offers; got %v, %v", tt.class, offers, err)
			}
		})
	}
}

func TestClientListOffersDiscardsPartialCatalog(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cloud") == "COMMUNITY" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, catalogFixture)
	}))
	defer server.Close()
	offers, err := NewClient(Config{BaseURL: server.URL}).ListOffers(context.Background(), burstresource.Intent{})
	if len(offers) != 0 || core.ClassifyError(err) != core.ErrorClassRateLimited {
		t.Fatalf("expected failed whole snapshot; got %v, %v", offers, err)
	}
}

func TestClientListOffersSkipsSpotAndHonorsCancellation(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unexpected catalog request")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client := NewClient(Config{BaseURL: server.URL})
	if offers, err := client.ListOffers(context.Background(), burstresource.Intent{Market: burstresource.MarketSpot}); err != nil || len(offers) != 0 {
		t.Fatalf("spot offers = %v, %v", offers, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.ListOffers(ctx, burstresource.Intent{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error = %v", err)
	}
}

func TestCatalogOfferIDsRemainStable(t *testing.T) {
	t.Parallel()
	var response struct {
		GPUs []catalogGPU `json:"gpus"`
	}
	if err := json.Unmarshal([]byte(catalogFixture), &response); err != nil {
		t.Fatal(err)
	}
	before := offersFromCatalogGPUs(response.GPUs, "SECURE", 1, burstresource.Intent{})
	*response.GPUs[0].Price["secure"] = 0.5
	response.GPUs[0].Availability = "NONE"
	after := offersFromCatalogGPUs(response.GPUs, "SECURE", 1, burstresource.Intent{})
	multi := offersFromCatalogGPUs(response.GPUs, "SECURE", 2, burstresource.Intent{})
	if len(before) != 2 || len(after) != 2 || len(multi) != 2 || before[0].ID != after[0].ID || before[0].ID == multi[0].ID {
		t.Fatalf("unstable/colliding IDs: %v, %v, %v", before, after, multi)
	}
}
