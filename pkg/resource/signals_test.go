package resource

import (
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestBlockedOffersFromAttemptsConvertsRecentFallbackFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	attempts := []core.Attempt{
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassCapacity,
			FallbackAllowed: true,
			Timestamp:       now.Add(-2 * time.Minute),
		},
		{
			Provider:        "lambda",
			Region:          "us-west-1",
			Class:           core.ErrorClassRateLimited,
			FallbackAllowed: true,
			Timestamp:       now.Add(-1 * time.Minute),
		},
	}

	blocked := BlockedOffersFromAttempts(attempts, FailureSignalPolicy{
		Cooldown: 10 * time.Minute,
		Now:      func() time.Time { return now },
	})

	if len(blocked) != 2 {
		t.Fatalf("blocked = %#v, want two entries", blocked)
	}
	assertBlockedOffer(t, blocked[0], "runpod", "US-TX-3", core.ErrorClassCapacity, now.Add(8*time.Minute))
	assertBlockedOffer(t, blocked[1], "lambda", "us-west-1", core.ErrorClassRateLimited, now.Add(9*time.Minute))
}

func TestBlockedOffersFromAttemptsSkipsNonFallbackSignals(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	attempts := []core.Attempt{
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassAuth,
			FallbackAllowed: false,
			Timestamp:       now.Add(-time.Minute),
		},
		{
			Provider:        "lambda",
			Region:          "us-west-1",
			Class:           core.ErrorClassUnsupportedSpec,
			FallbackAllowed: false,
			Timestamp:       now.Add(-time.Minute),
		},
	}

	blocked := BlockedOffersFromAttempts(attempts, FailureSignalPolicy{
		Cooldown: 10 * time.Minute,
		Now:      func() time.Time { return now },
	})

	if len(blocked) != 0 {
		t.Fatalf("blocked = %#v, want no entries", blocked)
	}
}

func TestBlockedOffersFromAttemptsSkipsExpiredAndIncompleteAttempts(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	attempts := []core.Attempt{
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassCapacity,
			FallbackAllowed: true,
			Timestamp:       now.Add(-11 * time.Minute),
		},
		{
			Class:           core.ErrorClassCapacity,
			FallbackAllowed: true,
			Timestamp:       now.Add(-time.Minute),
		},
		{
			Provider:        "lambda",
			Class:           core.ErrorClassCapacity,
			FallbackAllowed: true,
		},
	}

	blocked := BlockedOffersFromAttempts(attempts, FailureSignalPolicy{
		Cooldown: 10 * time.Minute,
		Now:      func() time.Time { return now },
	})

	if len(blocked) != 0 {
		t.Fatalf("blocked = %#v, want no entries", blocked)
	}
}

func TestBlockedOffersFromAttemptsDeduplicatesByMostRecentProviderLocation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	attempts := []core.Attempt{
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassCapacity,
			FallbackAllowed: true,
			Timestamp:       now.Add(-6 * time.Minute),
		},
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassTransient,
			FallbackAllowed: true,
			Timestamp:       now.Add(-2 * time.Minute),
		},
	}

	blocked := BlockedOffersFromAttempts(attempts, FailureSignalPolicy{
		Cooldown: 10 * time.Minute,
		Now:      func() time.Time { return now },
	})

	if len(blocked) != 1 {
		t.Fatalf("blocked = %#v, want one deduplicated entry", blocked)
	}
	assertBlockedOffer(t, blocked[0], "runpod", "US-TX-3", core.ErrorClassTransient, now.Add(8*time.Minute))
}

func TestPolicyWithFailureSignalsAppendsWithoutMutatingOriginal(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	original := Policy{
		BlockedOffers: []BlockedOffer{
			{Provider: "lambda", Location: "us-east-1", Class: core.ErrorClassCapacity, Until: now.Add(time.Minute)},
		},
	}

	merged := original.WithFailureSignals([]core.Attempt{
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassCapacity,
			FallbackAllowed: true,
			Timestamp:       now.Add(-2 * time.Minute),
		},
	}, FailureSignalPolicy{
		Cooldown: 10 * time.Minute,
		Now:      func() time.Time { return now },
	})

	if len(original.BlockedOffers) != 1 {
		t.Fatalf("original policy mutated: %#v", original.BlockedOffers)
	}
	if len(merged.BlockedOffers) != 2 {
		t.Fatalf("merged blocked offers = %#v, want existing plus signal", merged.BlockedOffers)
	}
	assertBlockedOffer(t, merged.BlockedOffers[1], "runpod", "US-TX-3", core.ErrorClassCapacity, now.Add(8*time.Minute))
}

func assertBlockedOffer(t *testing.T, got BlockedOffer, provider string, location string, class core.ErrorClass, until time.Time) {
	t.Helper()

	if got.Provider != provider || got.Location != location || got.Class != class || !got.Until.Equal(until) {
		t.Fatalf("blocked offer = %#v, want provider=%s location=%s class=%s until=%s", got, provider, location, class, until)
	}
}
