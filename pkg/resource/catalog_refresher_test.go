package resource

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestCatalogRefresherRecordsSourceOffers(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	catalog := NewObservedCatalog(ObservedCatalogOptions{
		Now:        func() time.Time { return now },
		DefaultTTL: 10 * time.Minute,
	})
	refresher := CatalogRefresher{
		Catalog: catalog,
		Sources: []ProviderOfferSource{
			staticOfferSource{
				provider: "runpod",
				offers: []Offer{
					{ID: "runpod-a40", Provider: "runpod", Location: "US-TX-3", GPUType: "A40", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.74, Available: true},
				},
			},
		},
		Now: func() time.Time { return now },
		TTL: 10 * time.Minute,
	}

	report := refresher.Refresh(context.Background())
	if report.Recorded != 1 || len(report.Errors) != 0 {
		t.Fatalf("report = %#v, want one recorded no errors", report)
	}
	snapshot, err := catalog.Snapshot(context.Background(), Intent{})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snapshot.Offers) != 1 || snapshot.Offers[0].ID != "runpod-a40" {
		t.Fatalf("offers = %#v, want recorded runpod offer", snapshot.Offers)
	}
}

func TestCatalogRefresherReportsSourceErrors(t *testing.T) {
	t.Parallel()

	refresher := CatalogRefresher{
		Catalog: NewObservedCatalog(ObservedCatalogOptions{}),
		Sources: []ProviderOfferSource{
			failingOfferSource{
				provider: "lambda",
				err:      &core.ProviderError{Provider: "lambda", Operation: "list", Class: core.ErrorClassRateLimited, Message: "try later"},
			},
		},
	}

	report := refresher.Refresh(context.Background())
	if report.Recorded != 0 || len(report.Errors) != 1 {
		t.Fatalf("report = %#v, want one source error", report)
	}
	if report.Errors[0].Provider != "lambda" || report.Errors[0].Class != core.ErrorClassRateLimited {
		t.Fatalf("errors = %#v, want lambda rate-limited catalog error", report.Errors)
	}
}

func TestCatalogRefresherRunStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	catalog := NewObservedCatalog(ObservedCatalogOptions{})
	source := &countingOfferSource{
		provider: "runpod",
		offers: []Offer{
			{ID: "runpod-a40", Provider: "runpod", GPUType: "A40", GPUCount: 1, Market: MarketOnDemand, Available: true},
		},
	}
	refresher := CatalogRefresher{
		Catalog: catalog,
		Sources: []ProviderOfferSource{source},
		TTL:     time.Minute,
	}

	done := make(chan struct{})
	go func() {
		refresher.Run(ctx, time.Millisecond)
		close(done)
	}()

	deadline := time.After(time.Second)
	for {
		if source.Count() > 0 {
			cancel()
			break
		}
		select {
		case <-deadline:
			t.Fatal("refresher did not call source before deadline")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop after context cancellation")
	}
}

type countingOfferSource struct {
	mu       sync.Mutex
	provider string
	offers   []Offer
	count    int
}

func (s *countingOfferSource) Provider() string {
	return s.provider
}

func (s *countingOfferSource) ListOffers(ctx context.Context, intent Intent) ([]Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.count++
	return cloneOffers(s.offers), nil
}

func (s *countingOfferSource) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}
