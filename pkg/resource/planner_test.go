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

func TestPlannerAmortizesMigrationCost(t *testing.T) {
	t.Parallel()

	planner := Planner{}
	decision, err := planner.Plan(Intent{
		GPUCount:     1,
		GPUType:      "H100",
		Market:       MarketSpot,
		Recoverable:  true,
		RemainingRun: 10 * time.Hour,
	}, []Offer{
		{
			ID:            "cheap-far",
			Provider:      "runpod",
			Location:      "asia-south2",
			GPUType:       "H100",
			GPUCount:      1,
			Market:        MarketSpot,
			HourlyPrice:   1.0,
			Available:     true,
			MigrationCost: 30,
		},
		{
			ID:          "local",
			Provider:    "runpod",
			Location:    "us-west1",
			GPUType:     "H100",
			GPUCount:    1,
			Market:      MarketSpot,
			HourlyPrice: 2.0,
			Available:   true,
		},
	}, Policy{})
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	if decision.Selected.ID != "local" {
		t.Fatalf("selected = %s, want local", decision.Selected.ID)
	}
	if math.Abs(decision.RiskAdjustedHourlyCost-2.0) > 0.000001 {
		t.Fatalf("risk-adjusted cost = %.2f, want 2.00", decision.RiskAdjustedHourlyCost)
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

func TestPlannerSwitchesToOnDemandWhenDeadlineIsTight(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	planner := Planner{Now: func() time.Time { return now }}
	decision, err := planner.Plan(Intent{
		GPUCount:     1,
		GPUType:      "H100",
		Market:       MarketEither,
		Recoverable:  true,
		Deadline:     now.Add(2 * time.Hour),
		RemainingRun: 90 * time.Minute,
	}, []Offer{
		{
			ID:          "cheap-spot",
			Provider:    "lambda",
			GPUType:     "H100",
			GPUCount:    1,
			Market:      MarketSpot,
			HourlyPrice: 1,
			Available:   true,
			ColdStart:   10 * time.Minute,
		},
		{
			ID:          "od-safety-net",
			Provider:    "lambda",
			GPUType:     "H100",
			GPUCount:    1,
			Market:      MarketOnDemand,
			HourlyPrice: 4,
			Available:   true,
			ColdStart:   10 * time.Minute,
		},
	}, Policy{DeadlineFallbackBuffer: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Plan returned error: %v", err)
	}
	if decision.Selected.ID != "od-safety-net" {
		t.Fatalf("selected = %s, want od-safety-net", decision.Selected.ID)
	}
	assertRejected(t, decision.Rejected, "cheap-spot", ReasonDeadlineRequiresOD)
}

func TestPlannerRejectsOfferThatCannotMeetDeadline(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	planner := Planner{Now: func() time.Time { return now }}
	_, err := planner.Plan(Intent{
		GPUCount:     1,
		GPUType:      "H100",
		Market:       MarketOnDemand,
		Deadline:     now.Add(1 * time.Hour),
		RemainingRun: 70 * time.Minute,
	}, []Offer{
		{
			ID:          "too-late",
			Provider:    "lambda",
			GPUType:     "H100",
			GPUCount:    1,
			Market:      MarketOnDemand,
			HourlyPrice: 4,
			Available:   true,
			ColdStart:   5 * time.Minute,
		},
	}, Policy{})
	if err == nil {
		t.Fatal("Plan returned nil error, want unavailable")
	}
	unavailable := err.(*UnavailableError)
	assertRejected(t, unavailable.Rejected, "too-late", ReasonDeadlineCannotMeet)
}

func TestPlannerRejectsShortExpectedSpotLifetime(t *testing.T) {
	t.Parallel()

	planner := Planner{}
	_, err := planner.Plan(Intent{
		GPUCount:    1,
		GPUType:     "H100",
		Market:      MarketSpot,
		Recoverable: true,
	}, []Offer{
		{
			ID:               "volatile",
			Provider:         "runpod",
			GPUType:          "H100",
			GPUCount:         1,
			Market:           MarketSpot,
			HourlyPrice:      1,
			Available:        true,
			ColdStart:        10 * time.Minute,
			ExpectedLifetime: 10 * time.Minute,
		},
	}, Policy{})
	if err == nil {
		t.Fatal("Plan returned nil error, want unavailable")
	}
	unavailable := err.(*UnavailableError)
	assertRejected(t, unavailable.Rejected, "volatile", ReasonSpotLifetimeTooShort)
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
