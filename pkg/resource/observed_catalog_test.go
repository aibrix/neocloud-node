package resource

import (
	"context"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestObservedCatalogReturnsFreshOffers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	catalog := NewObservedCatalog(ObservedCatalogOptions{
		Now:        func() time.Time { return now },
		DefaultTTL: 10 * time.Minute,
	})
	catalog.Record(OfferObservation{
		Offer: Offer{
			ID:          "runpod-a40-us-tx-3",
			Provider:    "runpod",
			Location:    "US-TX-3",
			GPUType:     "A40",
			GPUCount:    1,
			Market:      MarketOnDemand,
			HourlyPrice: 0.74,
			Available:   true,
		},
		ObservedAt: now.Add(-time.Minute),
		Source:     "runpod-list",
	})

	snapshot, err := catalog.Snapshot(context.Background(), Intent{GPUType: "A40", GPUCount: 1})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snapshot.Offers) != 1 {
		t.Fatalf("offers = %#v, want one fresh offer", snapshot.Offers)
	}
	if snapshot.Offers[0].Provider != "runpod" || snapshot.Offers[0].HourlyPrice != 0.74 {
		t.Fatalf("offer = %#v, want runpod price observation", snapshot.Offers[0])
	}
}

func TestObservedCatalogFiltersExpiredOffers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	catalog := NewObservedCatalog(ObservedCatalogOptions{
		Now:        func() time.Time { return now },
		DefaultTTL: 10 * time.Minute,
	})
	catalog.Record(OfferObservation{
		Offer: Offer{
			ID:        "expired",
			Provider:  "runpod",
			Location:  "US-TX-3",
			GPUType:   "A40",
			GPUCount:  1,
			Market:    MarketOnDemand,
			Available: true,
		},
		ObservedAt: now.Add(-11 * time.Minute),
		Source:     "probe",
	})

	snapshot, err := catalog.Snapshot(context.Background(), Intent{})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snapshot.Offers) != 0 {
		t.Fatalf("offers = %#v, want expired offer filtered", snapshot.Offers)
	}
}

func TestObservedCatalogUpdatesOfferByStableScope(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	catalog := NewObservedCatalog(ObservedCatalogOptions{
		Now:        func() time.Time { return now },
		DefaultTTL: 10 * time.Minute,
	})
	catalog.Record(OfferObservation{
		Offer: Offer{
			Provider:    "lambda",
			Location:    "us-west-1",
			GPUType:     "A10",
			GPUCount:    1,
			Market:      MarketOnDemand,
			HourlyPrice: 1.10,
			Available:   true,
		},
		ObservedAt: now.Add(-2 * time.Minute),
		Source:     "bootstrap",
	})
	catalog.Record(OfferObservation{
		Offer: Offer{
			Provider:    "lambda",
			Location:    "us-west-1",
			GPUType:     "A10",
			GPUCount:    1,
			Market:      MarketOnDemand,
			HourlyPrice: 0.95,
			Available:   false,
		},
		ObservedAt: now.Add(-time.Minute),
		Source:     "capacity-probe",
	})

	snapshot, err := catalog.Snapshot(context.Background(), Intent{})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snapshot.Offers) != 1 {
		t.Fatalf("offers = %#v, want one updated offer", snapshot.Offers)
	}
	if snapshot.Offers[0].HourlyPrice != 0.95 || snapshot.Offers[0].Available {
		t.Fatalf("offer = %#v, want latest unavailable price observation", snapshot.Offers[0])
	}
}

func TestObservedCatalogCanFeedLifecycleCandidatePlanner(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	catalog := NewObservedCatalog(ObservedCatalogOptions{
		Now:        func() time.Time { return now },
		DefaultTTL: 10 * time.Minute,
	})
	catalog.Record(OfferObservation{
		Offer: Offer{
			ID:          "runpod-a40-us-tx-3",
			Provider:    "runpod",
			Location:    "US-TX-3",
			GPUType:     "A40",
			GPUCount:    1,
			Market:      MarketOnDemand,
			HourlyPrice: 0.74,
			Available:   true,
		},
		ObservedAt: now,
		Source:     "bootstrap",
	})

	planner := LifecycleCandidatePlanner{
		Catalog: catalog,
		Planner: Planner{Now: func() time.Time {
			return now
		}},
	}
	candidates, err := planner.CreateCandidates(context.Background(), testBindingWithRequest("runpod", "A40"))
	if err != nil {
		t.Fatalf("CreateCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Provider != "runpod" {
		t.Fatalf("candidates = %#v, want runpod candidate from observed catalog", candidates)
	}
}

func testBindingWithRequest(provider string, gpuType string) core.Binding {
	return core.Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    gpuType,
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   provider,
		},
	}
}
