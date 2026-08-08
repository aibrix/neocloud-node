package resource

import (
	"context"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestRemoteResourceOfferSourceListsObservedOffers(t *testing.T) {
	t.Parallel()

	source := RemoteResourceOfferSource{
		ProviderName: "runpod",
		Lister: remoteResourceLister{
			remotes: []core.RemoteResource{
				{
					ID:       "remote-1",
					Provider: "runpod",
					Phase:    core.RemoteRunning,
					Request: core.WorkloadRequest{
						Provider:   "runpod",
						RegionHint: "US-TX-3",
						GPUType:    "A40",
						GPUMemory:  "48Gi",
						GPUCount:   1,
						MarketType: core.MarketTypeSpot,
						ProviderHints: map[string]string{
							core.AnnotationRunPodDataCenterIDs: "US-TX-3",
						},
					},
					HourlyCost: 0.74,
					CostSource: "runpod.costPerHr",
				},
			},
		},
	}

	offers, err := source.ListOffers(context.Background(), Intent{GPUType: "A40", GPUCount: 1})
	if err != nil {
		t.Fatalf("ListOffers() error = %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("offers = %#v, want one observed offer", offers)
	}
	offer := offers[0]
	if offer.Provider != "runpod" || offer.Location != "US-TX-3" || offer.GPUType != "A40" || offer.GPUCount != 1 || offer.Market != MarketSpot {
		t.Fatalf("offer = %#v, want runpod A40 spot", offer)
	}
	if offer.HourlyPrice != 0.74 || !offer.Available {
		t.Fatalf("offer price/availability = %#v, want 0.74 available", offer)
	}
	if offer.ProviderHints[core.AnnotationRunPodDataCenterIDs] != "US-TX-3" {
		t.Fatalf("provider hints = %#v, want datacenter hint", offer.ProviderHints)
	}
}

func TestRemoteResourceOfferSourceSkipsIncompleteShapes(t *testing.T) {
	t.Parallel()

	source := RemoteResourceOfferSource{
		ProviderName: "lambda",
		Lister: remoteResourceLister{
			remotes: []core.RemoteResource{
				{ID: "missing-gpu", Provider: "lambda", Phase: core.RemoteRunning, Request: core.WorkloadRequest{GPUCount: 1}},
				{ID: "missing-count", Provider: "lambda", Phase: core.RemoteRunning, Request: core.WorkloadRequest{GPUType: "A10"}},
			},
		},
	}

	offers, err := source.ListOffers(context.Background(), Intent{})
	if err != nil {
		t.Fatalf("ListOffers() error = %v", err)
	}
	if len(offers) != 0 {
		t.Fatalf("offers = %#v, want incomplete shapes skipped", offers)
	}
}

func TestRemoteResourceOfferSourceMarksFailedRemotesUnavailable(t *testing.T) {
	t.Parallel()

	source := RemoteResourceOfferSource{
		ProviderName: "lambda",
		Lister: remoteResourceLister{
			remotes: []core.RemoteResource{
				{
					ID:       "failed",
					Provider: "lambda",
					Phase:    core.RemoteFailed,
					Request: core.WorkloadRequest{
						Provider:   "lambda",
						RegionHint: "us-west-1",
						GPUType:    "A10",
						GPUCount:   1,
						MarketType: core.MarketTypeOnDemand,
					},
				},
			},
		},
	}

	offers, err := source.ListOffers(context.Background(), Intent{})
	if err != nil {
		t.Fatalf("ListOffers() error = %v", err)
	}
	if len(offers) != 1 || offers[0].Available {
		t.Fatalf("offers = %#v, want one unavailable offer", offers)
	}
}

type remoteResourceLister struct {
	remotes []core.RemoteResource
}

func (l remoteResourceLister) ListRemoteResources(ctx context.Context) ([]core.RemoteResource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]core.RemoteResource(nil), l.remotes...), nil
}
