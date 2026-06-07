package resource

import (
	"context"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

// RemoteResourceOfferSource converts provider-visible remote resources into offer signals.
type RemoteResourceOfferSource struct {
	ProviderName string
	Lister       core.RemoteResourceLister
}

func (s RemoteResourceOfferSource) Provider() string {
	return s.ProviderName
}

func (s RemoteResourceOfferSource) ListOffers(ctx context.Context, _ Intent) ([]Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.Lister == nil {
		return nil, nil
	}
	remotes, err := s.Lister.ListRemoteResources(ctx)
	if err != nil {
		return nil, err
	}
	offers := make([]Offer, 0, len(remotes))
	for _, remote := range remotes {
		offer, ok := offerFromRemoteResource(remote, s.ProviderName)
		if !ok {
			continue
		}
		offers = append(offers, offer)
	}
	return offers, nil
}

func offerFromRemoteResource(remote core.RemoteResource, providerName string) (Offer, bool) {
	request := remote.Request
	provider := firstNonEmptyString(remote.Provider, request.Provider, providerName)
	gpuType := strings.TrimSpace(request.GPUType)
	if provider == "" || gpuType == "" || request.GPUCount <= 0 {
		return Offer{}, false
	}

	return Offer{
		ID:            remote.ID,
		Provider:      provider,
		Location:      strings.TrimSpace(request.RegionHint),
		GPUType:       gpuType,
		GPUMemory:     strings.TrimSpace(request.GPUMemory),
		GPUCount:      request.GPUCount,
		Market:        marketFromRequest(request.MarketType),
		HourlyPrice:   remote.HourlyCost,
		Available:     remoteOfferAvailable(remote.Phase),
		ProviderHints: cloneProviderHints(request.ProviderHints),
	}, true
}

func remoteOfferAvailable(phase core.RemotePhase) bool {
	switch phase {
	case core.RemoteFailed, core.RemoteDeleted:
		return false
	default:
		return true
	}
}

func cloneProviderHints(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
