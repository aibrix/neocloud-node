package resource

import (
	"context"
	"strings"
	"sync"
	"time"
)

// DefaultObservedOfferTTL controls how long observations remain usable by default.
const DefaultObservedOfferTTL = 5 * time.Minute

// ObservedCatalogOptions configures an in-memory observed offer catalog.
type ObservedCatalogOptions struct {
	Now        func() time.Time
	DefaultTTL time.Duration
}

// OfferObservation is one normalized provider/capacity signal.
type OfferObservation struct {
	Offer      Offer
	ObservedAt time.Time
	TTL        time.Duration
	Source     string
}

// ObservedCatalog stores fresh provider offer observations for placement.
type ObservedCatalog struct {
	mu         sync.RWMutex
	now        func() time.Time
	defaultTTL time.Duration
	offers     map[string]OfferObservation
}

// NewObservedCatalog creates a thread-safe observed offer catalog.
func NewObservedCatalog(options ObservedCatalogOptions) *ObservedCatalog {
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	defaultTTL := options.DefaultTTL
	if defaultTTL <= 0 {
		defaultTTL = DefaultObservedOfferTTL
	}
	return &ObservedCatalog{
		now:        now,
		defaultTTL: defaultTTL,
		offers:     make(map[string]OfferObservation),
	}
}

// Snapshot returns fresh observed offers and filters stale entries.
func (c *ObservedCatalog) Snapshot(ctx context.Context, _ Intent) (CatalogSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return CatalogSnapshot{}, err
	}
	if c == nil {
		return CatalogSnapshot{}, nil
	}

	now := c.now()
	c.mu.RLock()
	defer c.mu.RUnlock()

	offers := make([]Offer, 0, len(c.offers))
	for _, observation := range c.offers {
		if observationExpired(observation, now, c.defaultTTL) {
			continue
		}
		offers = append(offers, observation.Offer)
	}
	return CatalogSnapshot{Offers: cloneOffers(offers)}, nil
}

// Record upserts an offer observation by offer ID or provider/location/GPU/market scope.
func (c *ObservedCatalog) Record(observation OfferObservation) {
	if c == nil {
		return
	}
	observation.Offer.Provider = strings.TrimSpace(observation.Offer.Provider)
	if observation.Offer.Provider == "" {
		return
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = c.now()
	}
	key := offerObservationKey(observation.Offer)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.offers[key] = observation
}

func observationExpired(observation OfferObservation, now time.Time, defaultTTL time.Duration) bool {
	ttl := observation.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	return ttl > 0 && !now.Before(observation.ObservedAt.Add(ttl))
}

func offerObservationKey(offer Offer) string {
	if strings.TrimSpace(offer.ID) != "" {
		return "id:" + strings.TrimSpace(offer.ID)
	}
	return strings.Join([]string{
		"scope",
		strings.TrimSpace(offer.Provider),
		strings.TrimSpace(offer.Location),
		strings.TrimSpace(offer.GPUType),
		strings.TrimSpace(offer.GPUMemory),
		strings.TrimSpace(string(offer.Market)),
	}, "\x00")
}
