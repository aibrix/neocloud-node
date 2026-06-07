package resource

import (
	"context"
	"time"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

type CatalogRefresher struct {
	Catalog *ObservedCatalog
	Sources []ProviderOfferSource
	TTL     time.Duration
	Now     func() time.Time
}

type CatalogRefreshReport struct {
	Recorded int
	Errors   []CatalogError
}

func (r CatalogRefresher) Refresh(ctx context.Context) CatalogRefreshReport {
	if err := ctx.Err(); err != nil {
		return CatalogRefreshReport{
			Errors: []CatalogError{{Class: core.ClassifyError(err), Err: err}},
		}
	}
	if r.Catalog == nil {
		return CatalogRefreshReport{}
	}

	now := r.now()
	report := CatalogRefreshReport{}
	for _, source := range r.Sources {
		if source == nil {
			continue
		}
		offers, err := source.ListOffers(ctx, Intent{})
		if err != nil {
			report.Errors = append(report.Errors, CatalogError{
				Provider: source.Provider(),
				Class:    core.ClassifyError(err),
				Err:      err,
			})
			continue
		}
		for _, offer := range offers {
			r.Catalog.Record(OfferObservation{
				Offer:      offer,
				ObservedAt: now,
				TTL:        r.TTL,
				Source:     source.Provider(),
			})
			report.Recorded++
		}
	}
	return report
}

func (r CatalogRefresher) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	r.Refresh(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Refresh(ctx)
		}
	}
}

func (r CatalogRefresher) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}
