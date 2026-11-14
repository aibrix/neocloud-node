package runpod

import (
	"context"
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
