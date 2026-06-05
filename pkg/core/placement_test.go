package core

import (
	"errors"
	"testing"
)

func TestPlacementSelectsByProviderThenRegionThenPrice(t *testing.T) {
	t.Parallel()

	decision, err := SelectOffer(
		WorkloadRequest{GPUCount: 1, GPUType: "H100", MaxPrice: "5.00"},
		[]Offer{
			{Provider: "lambda", Region: "us-west-1", GPUType: "H100", GPUCount: 1, PricePerHour: 3.50, Available: true},
			{Provider: "runpod", Region: "eu", GPUType: "H100", GPUCount: 1, PricePerHour: 4.00, Available: true},
			{Provider: "runpod", Region: "us", GPUType: "H100", GPUCount: 1, PricePerHour: 4.50, Available: true},
			{Provider: "runpod", Region: "us", GPUType: "A100", GPUCount: 1, PricePerHour: 2.00, Available: true},
		},
		PlacementPolicy{
			ProviderOrder: []string{"runpod", "lambda"},
			RegionOrder:   []string{"us", "eu"},
		},
	)
	if err != nil {
		t.Fatalf("SelectOffer returned error: %v", err)
	}
	if decision.Selected.Provider != "runpod" || decision.Selected.Region != "us" {
		t.Fatalf("selected offer = %#v, want runpod/us", decision.Selected)
	}
	if len(decision.Rejected) != 1 {
		t.Fatalf("rejected length = %d, want 1", len(decision.Rejected))
	}
	if decision.Rejected[0].Reason != "GPUTypeMismatch" {
		t.Fatalf("rejected reason = %q, want GPUTypeMismatch", decision.Rejected[0].Reason)
	}
}

func TestPlacementReturnsRejectedReasonsWhenNoOfferMatches(t *testing.T) {
	t.Parallel()

	_, err := SelectOffer(
		WorkloadRequest{GPUCount: 2, GPUType: "H100", MaxPrice: "3.00"},
		[]Offer{
			{Provider: "runpod", Region: "us", GPUType: "H100", GPUCount: 1, PricePerHour: 2.50, Available: true},
			{Provider: "lambda", Region: "us", GPUType: "H100", GPUCount: 2, PricePerHour: 4.00, Available: true},
			{Provider: "runpod", Region: "eu", GPUType: "H100", GPUCount: 2, PricePerHour: 2.90, Available: false},
		},
		PlacementPolicy{},
	)

	var unavailable *PlacementUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("SelectOffer error = %v, want PlacementUnavailableError", err)
	}
	if len(unavailable.Rejected) != 3 {
		t.Fatalf("rejected length = %d, want 3", len(unavailable.Rejected))
	}

	reasons := map[string]bool{}
	for _, rejected := range unavailable.Rejected {
		reasons[rejected.Reason] = true
	}
	for _, want := range []string{"InsufficientGPUCount", "PriceExceeded", "Unavailable"} {
		if !reasons[want] {
			t.Fatalf("missing rejected reason %q in %#v", want, unavailable.Rejected)
		}
	}
}
