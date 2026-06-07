package resource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestLifecycleCandidatePlannerBuildsSelectedAndFallbackCandidates(t *testing.T) {
	t.Parallel()

	planner := LifecycleCandidatePlanner{
		Catalog: CompositeCatalog{
			AllowPartial: true,
			Sources: []ProviderOfferSource{
				StaticCatalog{
					ProviderName: "test",
					Offers: []Offer{
						{ID: "runpod-a10", Provider: "runpod", Location: "US-TX-3", GPUType: "A10", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.60, Available: true},
						{ID: "lambda-a10", Provider: "lambda", Location: "us-west-1", GPUType: "A10", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.75, Available: true},
					},
				},
			},
		},
		Policy: Policy{ProviderOrder: []string{"runpod", "lambda"}},
	}

	candidates, err := planner.CreateCandidates(context.Background(), core.Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			Command:    []string{"python"},
			Args:       []string{"train.py"},
			Env:        map[string]string{"MODEL": "llama"},
			GPUCount:   1,
			GPUType:    "A10",
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   "lambda",
			RegionHint: "us-east-1",
		},
	})
	if err != nil {
		t.Fatalf("CreateCandidates() error = %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v, want selected plus fallback", candidates)
	}
	assertCandidate(t, candidates[0], "runpod", "US-TX-3", "A10", core.MarketTypeOnDemand)
	assertCandidate(t, candidates[1], "lambda", "us-west-1", "A10", core.MarketTypeOnDemand)
	if candidates[0].Request.Image != "example.com/train:latest" || candidates[0].Request.Command[0] != "python" || candidates[0].Request.Env["MODEL"] != "llama" {
		t.Fatalf("candidate request lost workload fields: %#v", candidates[0].Request)
	}
}

func TestLifecycleCandidatePlannerUsesRecentAttemptsAsCooldown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	planner := LifecycleCandidatePlanner{
		Catalog: CompositeCatalog{
			AllowPartial: true,
			Sources: []ProviderOfferSource{
				StaticCatalog{
					ProviderName: "test",
					Offers: []Offer{
						{ID: "runpod-a10", Provider: "runpod", Location: "US-TX-3", GPUType: "A10", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.60, Available: true},
						{ID: "lambda-a10", Provider: "lambda", Location: "us-west-1", GPUType: "A10", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.75, Available: true},
					},
				},
			},
		},
		Planner: Planner{Now: func() time.Time { return now }},
		Policy:  Policy{ProviderOrder: []string{"runpod", "lambda"}},
		SignalPolicy: FailureSignalPolicy{
			Cooldown: 10 * time.Minute,
			Now:      func() time.Time { return now },
		},
	}

	candidates, err := planner.CreateCandidates(context.Background(), core.Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "A10",
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   "runpod",
			RegionHint: "US-TX-3",
		},
		Attempts: []core.Attempt{
			{
				Provider:        "runpod",
				Region:          "US-TX-3",
				Class:           core.ErrorClassCapacity,
				FallbackAllowed: true,
				Timestamp:       now.Add(-2 * time.Minute),
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateCandidates() error = %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v, want only non-blocked lambda", candidates)
	}
	assertCandidate(t, candidates[0], "lambda", "us-west-1", "A10", core.MarketTypeOnDemand)
}

func TestLifecycleCandidatePlannerReturnsCapacityErrorWhenUnavailable(t *testing.T) {
	t.Parallel()

	planner := LifecycleCandidatePlanner{
		Catalog: CompositeCatalog{
			AllowPartial: true,
			Sources: []ProviderOfferSource{
				StaticCatalog{
					ProviderName: "test",
					Offers: []Offer{
						{ID: "expensive", Provider: "runpod", Location: "US-TX-3", GPUType: "A10", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 2.00, Available: true},
					},
				},
			},
		},
	}

	_, err := planner.CreateCandidates(context.Background(), core.Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "A10",
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   "runpod",
			RegionHint: "US-TX-3",
		},
	})
	var providerErr *core.ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("CreateCandidates() error = %T %[1]v, want ProviderError", err)
	}
	if providerErr.Operation != "placement" || providerErr.Class != core.ErrorClassCapacity {
		t.Fatalf("provider error = %#v, want placement capacity", providerErr)
	}
}

func TestIntentFromWorkloadRequestMapsCapabilitiesAndPrice(t *testing.T) {
	t.Parallel()

	intent := IntentFromWorkloadRequest(core.WorkloadRequest{
		GPUCount:         1,
		GPUType:          "A40",
		GPUMemory:        "48Gi",
		MaxPrice:         "0.90",
		MarketType:       core.MarketTypeSpot,
		RestartPolicy:    string(core.RestartPolicyOnFailure),
		ImagePullSecrets: []string{"registry"},
		Ports:            []core.Port{{ContainerPort: 8080}},
		ProviderHints: map[string]string{
			core.AnnotationRunPodNetworkVolumeID: "volume-1",
		},
	})

	if intent.GPUCount != 1 || intent.GPUType != "A40" || intent.GPUMemory != "48Gi" {
		t.Fatalf("intent resource shape = %#v", intent)
	}
	if intent.MaxHourlyPrice != 0.90 || intent.Market != MarketSpot || !intent.Recoverable {
		t.Fatalf("intent policy fields = %#v", intent)
	}
	if !hasCapability(intent.Capabilities, CapabilityPorts) || !hasCapability(intent.Capabilities, CapabilityRegistryAuth) || !hasCapability(intent.Capabilities, CapabilityNetworkVolume) {
		t.Fatalf("intent capabilities = %#v, want ports, registry auth and network volume", intent.Capabilities)
	}
}

func assertCandidate(t *testing.T, candidate core.CreateCandidate, provider string, region string, gpuType string, marketType string) {
	t.Helper()

	if candidate.Provider != provider {
		t.Fatalf("candidate provider = %q, want %q", candidate.Provider, provider)
	}
	if candidate.Request.Provider != provider || candidate.Request.RegionHint != region || candidate.Request.GPUType != gpuType || candidate.Request.MarketType != marketType {
		t.Fatalf("candidate request = %#v, want provider=%s region=%s gpuType=%s market=%s", candidate.Request, provider, region, gpuType, marketType)
	}
}

func hasCapability(capabilities []Capability, want Capability) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}
