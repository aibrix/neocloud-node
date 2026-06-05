package core

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

type Offer struct {
	Provider     string
	Region       string
	GPUType      string
	GPUCount     int
	PricePerHour float64
	Available    bool
}

type PlacementPolicy struct {
	ProviderOrder []string
	RegionOrder   []string
}

type PlacementDecision struct {
	Selected Offer
	Rejected []RejectedOffer
}

type RejectedOffer struct {
	Offer   Offer
	Reason  string
	Message string
}

type PlacementUnavailableError struct {
	Rejected []RejectedOffer
}

func (e *PlacementUnavailableError) Error() string {
	return fmt.Sprintf("no placement offer available; rejected=%d", len(e.Rejected))
}

func SelectOffer(request WorkloadRequest, offers []Offer, policy PlacementPolicy) (PlacementDecision, error) {
	maxPrice := maxPrice(request.MaxPrice)
	matches := make([]Offer, 0, len(offers))
	rejected := make([]RejectedOffer, 0)

	for _, offer := range offers {
		if rejection, ok := rejectOffer(request, offer, maxPrice); ok {
			rejected = append(rejected, rejection)
			continue
		}
		matches = append(matches, offer)
	}

	if len(matches) == 0 {
		return PlacementDecision{}, &PlacementUnavailableError{Rejected: rejected}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		left := matches[i]
		right := matches[j]

		if providerRank(left.Provider, policy.ProviderOrder) != providerRank(right.Provider, policy.ProviderOrder) {
			return providerRank(left.Provider, policy.ProviderOrder) < providerRank(right.Provider, policy.ProviderOrder)
		}
		if regionRank(left.Region, policy.RegionOrder) != regionRank(right.Region, policy.RegionOrder) {
			return regionRank(left.Region, policy.RegionOrder) < regionRank(right.Region, policy.RegionOrder)
		}
		return left.PricePerHour < right.PricePerHour
	})

	return PlacementDecision{
		Selected: matches[0],
		Rejected: rejected,
	}, nil
}

func rejectOffer(request WorkloadRequest, offer Offer, maxPriceValue float64) (RejectedOffer, bool) {
	if request.GPUType != "" && offer.GPUType != request.GPUType {
		return RejectedOffer{Offer: offer, Reason: "GPUTypeMismatch", Message: "offer GPU type does not match request"}, true
	}
	if request.GPUCount > 0 && offer.GPUCount < request.GPUCount {
		return RejectedOffer{Offer: offer, Reason: "InsufficientGPUCount", Message: "offer does not have enough GPUs"}, true
	}
	if !offer.Available {
		return RejectedOffer{Offer: offer, Reason: "Unavailable", Message: "offer is not currently available"}, true
	}
	if maxPriceValue < math.Inf(1) && offer.PricePerHour > maxPriceValue {
		return RejectedOffer{Offer: offer, Reason: "PriceExceeded", Message: "offer price exceeds request max price"}, true
	}
	return RejectedOffer{}, false
}

func maxPrice(raw string) float64 {
	if raw == "" {
		return math.Inf(1)
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return math.Inf(1)
	}
	return value
}

func providerRank(provider string, order []string) int {
	for idx, item := range order {
		if item == provider {
			return idx
		}
	}
	return len(order)
}

func regionRank(region string, order []string) int {
	for idx, item := range order {
		if item == region {
			return idx
		}
	}
	return len(order)
}
