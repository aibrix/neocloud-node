package resource

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

type Market string

const (
	MarketOnDemand Market = "on-demand"
	MarketSpot     Market = "spot"
	MarketEither   Market = "either"
)

type Capability string

const (
	CapabilityPorts         Capability = "ports"
	CapabilityRegistryAuth  Capability = "registry-auth"
	CapabilityNetworkVolume Capability = "network-volume"
	CapabilityCloudInit     Capability = "cloud-init"
	CapabilityPublicIP      Capability = "public-ip"
)

type Intent struct {
	GPUCount       int
	GPUType        string
	GPUMemory      string
	Market         Market
	Recoverable    bool
	MaxHourlyPrice float64
	Capabilities   []Capability
}

type Offer struct {
	ID                  string
	Provider            string
	Location            string
	GPUType             string
	GPUMemory           string
	GPUCount            int
	Market              Market
	HourlyPrice         float64
	Available           bool
	Capabilities        map[Capability]bool
	StartupPenalty      float64
	FailurePenalty      float64
	InterruptionPenalty float64
}

type Policy struct {
	ProviderOrder            []string
	LocationOrder            []string
	AllowSpotWithoutRecovery bool
	BlockedOffers            []BlockedOffer
}

type BlockedOffer struct {
	OfferID  string
	Provider string
	Location string
	GPUType  string
	Class    core.ErrorClass
	Until    time.Time
}

type Decision struct {
	Selected               Offer
	Fallback               []Offer
	Rejected               []RejectedOffer
	EstimatedHourlyCost    float64
	RiskAdjustedHourlyCost float64
}

type RejectedOffer struct {
	Offer   Offer
	Reason  RejectionReason
	Message string
	Class   core.ErrorClass
}

type RejectionReason string

const (
	ReasonGPUTypeMismatch       RejectionReason = "GPUTypeMismatch"
	ReasonGPUMemoryMismatch     RejectionReason = "GPUMemoryMismatch"
	ReasonInsufficientGPUCount  RejectionReason = "InsufficientGPUCount"
	ReasonMarketMismatch        RejectionReason = "MarketMismatch"
	ReasonUnavailable           RejectionReason = "Unavailable"
	ReasonPriceExceeded         RejectionReason = "PriceExceeded"
	ReasonUnsupportedCapability RejectionReason = "UnsupportedCapability"
	ReasonRecentlyBlocked       RejectionReason = "RecentlyBlocked"
	ReasonSpotRequiresRecovery  RejectionReason = "SpotRequiresRecovery"
)

type UnavailableError struct {
	Rejected []RejectedOffer
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("no resource offer available; rejected=%d", len(e.Rejected))
}

type Planner struct {
	Now func() time.Time
}

func (p Planner) Plan(intent Intent, offers []Offer, policy Policy) (Decision, error) {
	now := p.now()
	matches := make([]Offer, 0, len(offers))
	rejected := make([]RejectedOffer, 0)

	for _, offer := range offers {
		if rejection, ok := rejectOffer(intent, offer, policy, now); ok {
			rejected = append(rejected, rejection)
			continue
		}
		matches = append(matches, offer)
	}

	if len(matches) == 0 {
		return Decision{}, &UnavailableError{Rejected: rejected}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		left := matches[i]
		right := matches[j]
		if providerRank(left.Provider, policy.ProviderOrder) != providerRank(right.Provider, policy.ProviderOrder) {
			return providerRank(left.Provider, policy.ProviderOrder) < providerRank(right.Provider, policy.ProviderOrder)
		}
		if locationRank(left.Location, policy.LocationOrder) != locationRank(right.Location, policy.LocationOrder) {
			return locationRank(left.Location, policy.LocationOrder) < locationRank(right.Location, policy.LocationOrder)
		}
		return riskAdjustedCost(left) < riskAdjustedCost(right)
	})

	selected := matches[0]
	return Decision{
		Selected:               selected,
		Fallback:               append([]Offer(nil), matches[1:]...),
		Rejected:               rejected,
		EstimatedHourlyCost:    selected.HourlyPrice,
		RiskAdjustedHourlyCost: riskAdjustedCost(selected),
	}, nil
}

func (p Planner) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func rejectOffer(intent Intent, offer Offer, policy Policy, now time.Time) (RejectedOffer, bool) {
	if blocked, ok := blockedOffer(offer, policy.BlockedOffers, now); ok {
		return RejectedOffer{
			Offer:   offer,
			Reason:  ReasonRecentlyBlocked,
			Message: "offer is temporarily blocked by a recent provider failure",
			Class:   blocked.Class,
		}, true
	}
	if strings.TrimSpace(intent.GPUType) != "" && offer.GPUType != intent.GPUType {
		return RejectedOffer{Offer: offer, Reason: ReasonGPUTypeMismatch, Message: "offer GPU type does not match request"}, true
	}
	if strings.TrimSpace(intent.GPUMemory) != "" && strings.TrimSpace(offer.GPUMemory) != "" && normalizeMemory(offer.GPUMemory) < normalizeMemory(intent.GPUMemory) {
		return RejectedOffer{Offer: offer, Reason: ReasonGPUMemoryMismatch, Message: "offer GPU memory is lower than request"}, true
	}
	if intent.GPUCount > 0 && offer.GPUCount < intent.GPUCount {
		return RejectedOffer{Offer: offer, Reason: ReasonInsufficientGPUCount, Message: "offer does not have enough GPUs"}, true
	}
	if !marketMatches(intent.Market, offer.Market) {
		return RejectedOffer{Offer: offer, Reason: ReasonMarketMismatch, Message: "offer market does not match request"}, true
	}
	if offer.Market == MarketSpot && !intent.Recoverable && !policy.AllowSpotWithoutRecovery {
		return RejectedOffer{Offer: offer, Reason: ReasonSpotRequiresRecovery, Message: "spot requires recoverable or checkpointed workload"}, true
	}
	if !offer.Available {
		return RejectedOffer{Offer: offer, Reason: ReasonUnavailable, Message: "offer is not currently available", Class: core.ErrorClassCapacity}, true
	}
	if intent.MaxHourlyPrice > 0 && offer.HourlyPrice > intent.MaxHourlyPrice {
		return RejectedOffer{Offer: offer, Reason: ReasonPriceExceeded, Message: "offer price exceeds request max price"}, true
	}
	for _, capability := range intent.Capabilities {
		if !offer.Capabilities[capability] {
			return RejectedOffer{
				Offer:   offer,
				Reason:  ReasonUnsupportedCapability,
				Message: fmt.Sprintf("offer does not support required capability %s", capability),
			}, true
		}
	}
	return RejectedOffer{}, false
}

func blockedOffer(offer Offer, blocked []BlockedOffer, now time.Time) (BlockedOffer, bool) {
	for _, item := range blocked {
		if !item.Until.IsZero() && !now.Before(item.Until) {
			continue
		}
		if item.OfferID != "" && item.OfferID == offer.ID {
			return item, true
		}
		if item.Provider != "" && item.Provider != offer.Provider {
			continue
		}
		if item.Location != "" && item.Location != offer.Location {
			continue
		}
		if item.GPUType != "" && item.GPUType != offer.GPUType {
			continue
		}
		if item.OfferID == "" && (item.Provider != "" || item.Location != "" || item.GPUType != "") {
			return item, true
		}
	}
	return BlockedOffer{}, false
}

func marketMatches(intent Market, offer Market) bool {
	if intent == "" || intent == MarketEither {
		return true
	}
	return intent == offer
}

func riskAdjustedCost(offer Offer) float64 {
	return offer.HourlyPrice + offer.StartupPenalty + offer.FailurePenalty + offer.InterruptionPenalty
}

func providerRank(provider string, order []string) int {
	for index, item := range order {
		if item == provider {
			return index
		}
	}
	return len(order)
}

func locationRank(location string, order []string) int {
	for index, item := range order {
		if item == location {
			return index
		}
	}
	return len(order)
}

func normalizeMemory(value string) float64 {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimSuffix(value, "ib")
	value = strings.TrimSuffix(value, "b")
	multiplier := 1.0
	switch {
	case strings.HasSuffix(value, "g"):
		value = strings.TrimSuffix(value, "g")
	case strings.HasSuffix(value, "m"):
		value = strings.TrimSuffix(value, "m")
		multiplier = 1.0 / 1024.0
	default:
		multiplier = 1.0
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return math.Inf(1)
	}
	return parsed * multiplier
}
