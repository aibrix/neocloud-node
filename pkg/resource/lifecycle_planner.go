package resource

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

// LifecycleCandidatePlanner adapts the resource planner to core lifecycle create candidates.
type LifecycleCandidatePlanner struct {
	Catalog      OfferCatalog
	Planner      Planner
	Policy       Policy
	SignalPolicy FailureSignalPolicy
}

// CreateCandidates turns a durable binding into ordered provider create attempts.
func (p LifecycleCandidatePlanner) CreateCandidates(ctx context.Context, binding core.Binding) ([]core.CreateCandidate, error) {
	intent := IntentFromWorkloadRequest(binding.Request)
	policy := p.Policy.WithFailureSignals(binding.Attempts, p.SignalPolicy)
	engine := Engine{
		Catalog: p.Catalog,
		Planner: p.Planner,
		Policy:  policy,
	}
	selection, err := engine.Select(ctx, intent)
	if err != nil {
		return nil, placementProviderError(binding.Request, err)
	}

	offers := make([]Offer, 0, 1+len(selection.Decision.Fallback))
	offers = append(offers, selection.Decision.Selected)
	offers = append(offers, selection.Decision.Fallback...)

	candidates := make([]core.CreateCandidate, 0, len(offers))
	for _, offer := range offers {
		if strings.TrimSpace(offer.Provider) == "" {
			continue
		}
		request := requestForOffer(binding.Request, offer)
		candidates = append(candidates, core.CreateCandidate{
			Provider: request.Provider,
			Request:  request,
		})
	}
	if len(candidates) == 0 {
		return nil, &core.ProviderError{
			Provider:  binding.Request.Provider,
			Operation: "placement",
			Class:     core.ErrorClassCapacity,
			Message:   "placement selected no provider candidates",
		}
	}
	return candidates, nil
}

// IntentFromWorkloadRequest maps the provider-neutral workload request into placement inputs.
func IntentFromWorkloadRequest(request core.WorkloadRequest) Intent {
	return Intent{
		GPUCount:       request.GPUCount,
		GPUType:        strings.TrimSpace(request.GPUType),
		GPUMemory:      strings.TrimSpace(request.GPUMemory),
		Market:         marketFromRequest(request.MarketType),
		Recoverable:    restartPolicyRecoverable(request.RestartPolicy),
		MaxHourlyPrice: maxHourlyPrice(request.MaxPrice),
		Capabilities:   capabilitiesFromRequest(request),
	}
}

func requestForOffer(request core.WorkloadRequest, offer Offer) core.WorkloadRequest {
	out := request
	out.Provider = offer.Provider
	out.RegionHint = offer.Location
	if strings.TrimSpace(offer.GPUType) != "" {
		out.GPUType = offer.GPUType
	}
	if strings.TrimSpace(offer.GPUMemory) != "" {
		out.GPUMemory = offer.GPUMemory
	}
	if offer.Market != "" && offer.Market != MarketEither {
		out.MarketType = string(offer.Market)
	}
	return out
}

func placementProviderError(request core.WorkloadRequest, err error) error {
	if err == nil {
		return nil
	}
	var providerErr *core.ProviderError
	if errors.As(err, &providerErr) {
		return err
	}
	return &core.ProviderError{
		Provider:  request.Provider,
		Operation: "placement",
		Class:     placementErrorClass(err),
		Message:   err.Error(),
		Err:       err,
	}
}

func placementErrorClass(err error) core.ErrorClass {
	var unavailable *UnavailableError
	if errors.As(err, &unavailable) {
		return core.ErrorClassCapacity
	}
	var catalogErrors CatalogErrors
	if errors.As(err, &catalogErrors) {
		for _, item := range catalogErrors {
			if item.Class != "" && item.Class != core.ErrorClassUnknown {
				return item.Class
			}
		}
		return core.ErrorClassTransient
	}
	return core.ClassifyError(err)
}

func marketFromRequest(raw string) Market {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", core.MarketTypeOnDemand, "ondemand", "on_demand":
		return MarketOnDemand
	case core.MarketTypeSpot, "interruptible", "preemptible":
		return MarketSpot
	case string(MarketEither), "any":
		return MarketEither
	default:
		return Market(raw)
	}
}

func restartPolicyRecoverable(policy string) bool {
	switch strings.TrimSpace(policy) {
	case string(core.RestartPolicyOnFailure), string(core.RestartPolicyAlways):
		return true
	default:
		return false
	}
}

func maxHourlyPrice(raw string) float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || value < 0 {
		return 0
	}
	return value
}

func capabilitiesFromRequest(request core.WorkloadRequest) []Capability {
	capabilities := make([]Capability, 0, 4)
	if len(request.Ports) > 0 {
		capabilities = append(capabilities, CapabilityPorts)
	}
	if len(request.ImagePullSecrets) > 0 || providerHint(request, core.AnnotationRunPodRegistryAuthID) != "" {
		capabilities = append(capabilities, CapabilityRegistryAuth)
	}
	if providerHint(request, core.AnnotationRunPodNetworkVolumeID) != "" {
		capabilities = append(capabilities, CapabilityNetworkVolume)
	}
	if providerHint(request, core.AnnotationLambdaCallbackSecret) != "" {
		capabilities = append(capabilities, CapabilityCloudInit)
	}
	return capabilities
}

func providerHint(request core.WorkloadRequest, key string) string {
	if len(request.ProviderHints) == 0 {
		return ""
	}
	return strings.TrimSpace(request.ProviderHints[key])
}
