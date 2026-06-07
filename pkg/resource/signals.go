package resource

import (
	"strings"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

// DefaultFailureSignalCooldown is used when callers do not supply an explicit cooldown.
const DefaultFailureSignalCooldown = 5 * time.Minute

// FailureSignalPolicy controls how recent provider attempts affect placement.
type FailureSignalPolicy struct {
	Cooldown time.Duration
	Now      func() time.Time
}

// WithFailureSignals returns a copy of the policy with recent provider attempt signals appended.
func (p Policy) WithFailureSignals(attempts []core.Attempt, signalPolicy FailureSignalPolicy) Policy {
	out := p
	out.BlockedOffers = append([]BlockedOffer(nil), p.BlockedOffers...)
	out.BlockedOffers = append(out.BlockedOffers, BlockedOffersFromAttempts(attempts, signalPolicy)...)
	return out
}

// BlockedOffersFromAttempts converts recent fallback-eligible failures into placement cooldowns.
func BlockedOffersFromAttempts(attempts []core.Attempt, policy FailureSignalPolicy) []BlockedOffer {
	if len(attempts) == 0 {
		return nil
	}

	now := failureSignalNow(policy)
	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = DefaultFailureSignalCooldown
	}

	blocked := make([]BlockedOffer, 0, len(attempts))
	byScope := make(map[string]int, len(attempts))
	for _, attempt := range attempts {
		provider := strings.TrimSpace(attempt.Provider)
		if provider == "" || attempt.Timestamp.IsZero() || !core.AllowsFallbackClass(attempt.Class) {
			continue
		}
		until := attempt.Timestamp.Add(cooldown)
		if !now.Before(until) {
			continue
		}

		location := strings.TrimSpace(attempt.Region)
		item := BlockedOffer{
			Provider: provider,
			Location: location,
			Class:    attempt.Class,
			Until:    until,
		}
		key := blockedOfferScopeKey(item)
		if index, ok := byScope[key]; ok {
			if item.Until.After(blocked[index].Until) {
				blocked[index] = item
			}
			continue
		}
		byScope[key] = len(blocked)
		blocked = append(blocked, item)
	}
	return blocked
}

func failureSignalNow(policy FailureSignalPolicy) time.Time {
	if policy.Now != nil {
		return policy.Now()
	}
	return time.Now().UTC()
}

func blockedOfferScopeKey(item BlockedOffer) string {
	return item.Provider + "\x00" + item.Location + "\x00" + item.GPUType
}
