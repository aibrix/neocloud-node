package resource

import (
	"context"
	"errors"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestCompositeCatalogAllowsPartialProviderErrors(t *testing.T) {
	t.Parallel()

	catalog := CompositeCatalog{
		AllowPartial: true,
		Sources: []ProviderOfferSource{
			staticOfferSource{
				provider: "runpod",
				offers: []Offer{
					{ID: "runpod-a40", Provider: "runpod", GPUType: "A40", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.74, Available: true},
				},
			},
			failingOfferSource{
				provider: "lambda",
				err:      &core.ProviderError{Provider: "lambda", Operation: "catalog", Class: core.ErrorClassCapacity, Message: "no capacity"},
			},
		},
	}

	snapshot, err := catalog.Snapshot(context.Background(), Intent{GPUType: "A40", GPUCount: 1})
	if err != nil {
		t.Fatalf("Snapshot() error = %v, want nil with partial offers", err)
	}
	if len(snapshot.Offers) != 1 || snapshot.Offers[0].ID != "runpod-a40" {
		t.Fatalf("offers = %#v, want runpod-a40", snapshot.Offers)
	}
	if len(snapshot.Errors) != 1 || snapshot.Errors[0].Provider != "lambda" {
		t.Fatalf("errors = %#v, want lambda catalog error", snapshot.Errors)
	}
	if snapshot.Errors[0].Class != core.ErrorClassCapacity {
		t.Fatalf("class = %s, want %s", snapshot.Errors[0].Class, core.ErrorClassCapacity)
	}
}

func TestCompositeCatalogFailsWhenNoOffersAreAvailable(t *testing.T) {
	t.Parallel()

	catalog := CompositeCatalog{
		AllowPartial: true,
		Sources: []ProviderOfferSource{
			failingOfferSource{provider: "runpod", err: context.DeadlineExceeded},
			failingOfferSource{provider: "lambda", err: errors.New("catalog unavailable")},
		},
	}

	snapshot, err := catalog.Snapshot(context.Background(), Intent{GPUType: "A40", GPUCount: 1})
	if err == nil {
		t.Fatal("Snapshot() error = nil, want catalog errors")
	}
	if len(snapshot.Errors) != 2 {
		t.Fatalf("errors = %#v, want two provider errors", snapshot.Errors)
	}
	var catalogErrors CatalogErrors
	if !errors.As(err, &catalogErrors) {
		t.Fatalf("Snapshot() error = %T %[1]v, want CatalogErrors", err)
	}
	if snapshot.Errors[0].Class != core.ErrorClassTimeout {
		t.Fatalf("first class = %s, want %s", snapshot.Errors[0].Class, core.ErrorClassTimeout)
	}
}

func TestEngineSelectsFromCatalogAndCarriesPartialErrors(t *testing.T) {
	t.Parallel()

	engine := Engine{
		Catalog: CompositeCatalog{
			AllowPartial: true,
			Sources: []ProviderOfferSource{
				staticOfferSource{
					provider: "runpod",
					offers: []Offer{
						{ID: "runpod-a40", Provider: "runpod", GPUType: "A40", GPUCount: 1, Market: MarketOnDemand, HourlyPrice: 0.74, Available: true},
					},
				},
				failingOfferSource{
					provider: "lambda",
					err:      &core.ProviderError{Provider: "lambda", Operation: "catalog", Class: core.ErrorClassRateLimited, Message: "try later"},
				},
			},
		},
		Policy: Policy{ProviderOrder: []string{"lambda", "runpod"}},
	}

	selection, err := engine.Select(context.Background(), Intent{GPUType: "A40", GPUCount: 1, Market: MarketOnDemand})
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if selection.Decision.Selected.ID != "runpod-a40" {
		t.Fatalf("selected = %s, want runpod-a40", selection.Decision.Selected.ID)
	}
	if len(selection.CatalogErrors) != 1 || selection.CatalogErrors[0].Class != core.ErrorClassRateLimited {
		t.Fatalf("catalog errors = %#v, want lambda rate limit", selection.CatalogErrors)
	}
}

type staticOfferSource struct {
	provider string
	offers   []Offer
}

func (s staticOfferSource) Provider() string {
	return s.provider
}

func (s staticOfferSource) ListOffers(context.Context, Intent) ([]Offer, error) {
	return cloneOffers(s.offers), nil
}

type failingOfferSource struct {
	provider string
	err      error
}

func (s failingOfferSource) Provider() string {
	return s.provider
}

func (s failingOfferSource) ListOffers(context.Context, Intent) ([]Offer, error) {
	return nil, s.err
}
