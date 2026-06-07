package resource

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

type OfferCatalog interface {
	Snapshot(ctx context.Context, intent Intent) (CatalogSnapshot, error)
}

type ProviderOfferSource interface {
	Provider() string
	ListOffers(ctx context.Context, intent Intent) ([]Offer, error)
}

type CatalogSnapshot struct {
	Offers []Offer
	Errors []CatalogError
}

type CatalogError struct {
	Provider string
	Class    core.ErrorClass
	Err      error
}

func (e CatalogError) Error() string {
	if e.Err == nil {
		return e.Provider
	}
	if e.Provider == "" {
		return e.Err.Error()
	}
	return e.Provider + ": " + e.Err.Error()
}

func (e CatalogError) Unwrap() error {
	return e.Err
}

type CatalogErrors []CatalogError

func (e CatalogErrors) Error() string {
	if len(e) == 0 {
		return "catalog errors"
	}
	providers := make([]string, 0, len(e))
	for _, item := range e {
		if item.Provider != "" {
			providers = append(providers, item.Provider)
		}
	}
	if len(providers) == 0 {
		return fmt.Sprintf("catalog errors: %d provider sources failed", len(e))
	}
	return "catalog errors: " + strings.Join(providers, ", ")
}

type CompositeCatalog struct {
	Sources      []ProviderOfferSource
	AllowPartial bool
}

func (c CompositeCatalog) Snapshot(ctx context.Context, intent Intent) (CatalogSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return CatalogSnapshot{}, err
	}

	var snapshot CatalogSnapshot
	for _, source := range c.Sources {
		if source == nil {
			continue
		}
		offers, err := source.ListOffers(ctx, intent)
		if err != nil {
			snapshot.Errors = append(snapshot.Errors, CatalogError{
				Provider: source.Provider(),
				Class:    core.ClassifyError(err),
				Err:      err,
			})
			continue
		}
		snapshot.Offers = append(snapshot.Offers, cloneOffers(offers)...)
	}

	if len(snapshot.Errors) > 0 && (!c.AllowPartial || len(snapshot.Offers) == 0) {
		return snapshot, CatalogErrors(snapshot.Errors)
	}
	return snapshot, nil
}

type StaticCatalog struct {
	ProviderName string
	Offers       []Offer
}

func (c StaticCatalog) Provider() string {
	return c.ProviderName
}

func (c StaticCatalog) ListOffers(ctx context.Context, _ Intent) ([]Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return cloneOffers(c.Offers), nil
}

type Selection struct {
	Decision      Decision
	CatalogErrors []CatalogError
}

type Engine struct {
	Catalog OfferCatalog
	Planner Planner
	Policy  Policy
}

func (e Engine) Select(ctx context.Context, intent Intent) (Selection, error) {
	if e.Catalog == nil {
		return Selection{}, errors.New("missing offer catalog")
	}
	snapshot, err := e.Catalog.Snapshot(ctx, intent)
	selection := Selection{
		CatalogErrors: append([]CatalogError(nil), snapshot.Errors...),
	}
	if err != nil {
		return selection, err
	}

	decision, err := e.Planner.Plan(intent, snapshot.Offers, e.Policy)
	if err != nil {
		return selection, err
	}
	selection.Decision = decision
	return selection, nil
}

func cloneOffers(in []Offer) []Offer {
	if len(in) == 0 {
		return nil
	}
	out := make([]Offer, 0, len(in))
	for _, offer := range in {
		copied := offer
		if len(offer.Capabilities) > 0 {
			copied.Capabilities = make(map[Capability]bool, len(offer.Capabilities))
			for capability, supported := range offer.Capabilities {
				copied.Capabilities[capability] = supported
			}
		}
		if len(offer.ProviderHints) > 0 {
			copied.ProviderHints = make(map[string]string, len(offer.ProviderHints))
			for key, value := range offer.ProviderHints {
				copied.ProviderHints[key] = value
			}
		}
		out = append(out, copied)
	}
	return out
}
