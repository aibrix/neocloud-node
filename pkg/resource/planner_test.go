package resource

import (
	"math"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestPlannerChoosesRiskAdjustedSpotOffer(t *testing.T) {
	t.Parallel()

	planner := Planner{}
	decision, err := planner.Plan(Intent{
		GPUCount:       1,
		GPUType:        "H100",
		GPUMemory:      "80GB",
		Market:         MarketEither,
		Recoverable:    true,
		MaxHourlyPrice: 5.0,
	}, []Offer{
		{
			ID:                  "cheap-risky",
			Provider:            "runpod",
			Location:            "US-TX-3",
			GPUType:             "H100",
			GPUMemory:           "80GB",
			GPUCount:            1,
			Market:              MarketSpot,
			HourlyPrice:         1.5,
			Available:           true,
			InterruptionPenalty: 2.0,
		},
		{
			ID:                  "balanced",
			Provider:            "lambda",
			Location:            "us-west-1",
			GPUType:             "H100",
			GPUMemory:           "80GB",
			GPUCount:            1,
			Market:              MarketSpot,
			HourlyPrice:         2.2,
			Available:           true,
			InterruptionPenalty: 0.3,
		},
	}, Policy{})
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	if decision.Selected.ID != "balanced" {
		t.Fatalf("selected = %s, want balanced", decision.Selected.ID)
	}
	if math.Abs(decision.RiskAdjustedHourlyCost-2.5) > 0.000001 {
		t.Fatalf("risk-adjusted cost = %.2f, want 2.50", decision.RiskAdjustedHourlyCost)
	}
	if len(decision.Fallback) != 1 || decision.Fallback[0].ID != "cheap-risky" {
		t.Fatalf("fallback = %#v, want cheap-risky", decision.Fallback)
	}
}

func TestPlannerRejectsUnsupportedAndBlockedOffers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	planner := Planner{Now: func() time.Time { return now }}
	_, err := planner.Plan(Intent{
		GPUCount:    1,
		GPUType:     "A100",
		Market:      MarketSpot,
		Recoverable: true,
		Capabilities: []Capability{
			CapabilityPorts,
			CapabilityRegistryAuth,
		},
	}, []Offer{
		{
			ID:           "missing-registry",
			Provider:     "runpod",
			GPUType:      "A100",
			GPUCount:     1,
			Market:       MarketSpot,
			HourlyPrice:  1,
			Available:    true,
			Capabilities: map[Capability]bool{CapabilityPorts: true},
		},
		{
			ID:           "blocked",
			Provider:     "lambda",
			GPUType:      "A100",
			GPUCount:     1,
			Market:       MarketSpot,
			HourlyPrice:  2,
			Available:    true,
			Capabilities: map[Capability]bool{CapabilityPorts: true, CapabilityRegistryAuth: true},
		},
	}, Policy{
		BlockedOffers: []BlockedOffer{
			{OfferID: "blocked", Class: core.ErrorClassCapacity, Until: now.Add(time.Minute)},
		},
	})
	if err == nil {
		t.Fatal("Plan returned nil error, want unavailable")
	}
	unavailable, ok := err.(*UnavailableError)
	if !ok {
		t.Fatalf("Plan error = %T %[1]v, want UnavailableError", err)
	}
	if len(unavailable.Rejected) != 2 {
		t.Fatalf("rejected = %#v, want two rejected offers", unavailable.Rejected)
	}
	assertRejected(t, unavailable.Rejected, "missing-registry", ReasonUnsupportedCapability)
	assertRejected(t, unavailable.Rejected, "blocked", ReasonRecentlyBlocked)
}

func TestPlannerRequiresRecoverableWorkloadForSpot(t *testing.T) {
	t.Parallel()

	planner := Planner{}
	_, err := planner.Plan(Intent{
		GPUCount: 1,
		GPUType:  "A100",
		Market:   MarketSpot,
	}, []Offer{
		{
			ID:          "spot",
			Provider:    "runpod",
			GPUType:     "A100",
			GPUCount:    1,
			Market:      MarketSpot,
			HourlyPrice: 1,
			Available:   true,
		},
	}, Policy{})
	if err == nil {
		t.Fatal("Plan returned nil error, want spot recovery rejection")
	}
	unavailable := err.(*UnavailableError)
	assertRejected(t, unavailable.Rejected, "spot", ReasonSpotRequiresRecovery)
}

func TestPlannerHonorsProviderOrderBeforeCost(t *testing.T) {
	t.Parallel()

	planner := Planner{}
	decision, err := planner.Plan(Intent{
		GPUCount: 1,
		GPUType:  "L40S",
		Market:   MarketOnDemand,
	}, []Offer{
		{ID: "cheap", Provider: "runpod", GPUType: "L40S", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 1.0, Available: true},
		{ID: "preferred", Provider: "lambda", GPUType: "L40S", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 2.0, Available: true},
	}, Policy{ProviderOrder: []string{"lambda", "runpod"}})
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	if decision.Selected.ID != "preferred" {
		t.Fatalf("selected = %s, want preferred", decision.Selected.ID)
	}
}

func assertRejected(t *testing.T, rejected []RejectedOffer, offerID string, reason RejectionReason) {
	t.Helper()

	for _, item := range rejected {
		if item.Offer.ID == offerID {
			if item.Reason != reason {
				t.Fatalf("offer %s reason = %s, want %s", offerID, item.Reason, reason)
			}
			return
		}
	}
	t.Fatalf("offer %s not rejected; rejected=%#v", offerID, rejected)
}
