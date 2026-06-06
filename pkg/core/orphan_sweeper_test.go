package core

import (
	"context"
	"testing"
)

func TestOrphanSweeperDeletesManagedRemoteWithoutBinding(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := &sweeperProvider{
		remotes: []RemoteResource{
			{ID: "live", ExternalName: ManagedExternalName("default/live"), Provider: "runpod"},
			{ID: "orphan", ExternalName: ManagedExternalName("default/orphan"), Provider: "runpod"},
			{ID: "manual", ExternalName: "manual-resource", Provider: "runpod"},
		},
	}
	_, _, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "live",
		WorkloadUID: "live",
		RemoteID:    "live",
		Phase:       BindingRunning,
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	report, err := (OrphanSweeper{Store: store, Provider: provider}).Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if len(report.Orphans) != 1 || report.Orphans[0].ID != "orphan" {
		t.Fatalf("orphans = %#v, want orphan only", report.Orphans)
	}
	if len(provider.deleted) != 1 || provider.deleted[0] != "orphan" {
		t.Fatalf("deleted = %#v, want orphan", provider.deleted)
	}
}

func TestOrphanSweeperDryRunDoesNotDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := &sweeperProvider{
		remotes: []RemoteResource{
			{ID: "orphan", ExternalName: ManagedExternalName("default/orphan"), Provider: "runpod"},
		},
	}

	report, err := (OrphanSweeper{
		Store:    NewMemoryBindingStore(),
		Provider: provider,
		DryRun:   true,
	}).Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if len(report.Orphans) != 1 || len(provider.deleted) != 0 {
		t.Fatalf("orphans/deleted = %#v/%#v, want one orphan and no deletes", report.Orphans, provider.deleted)
	}
}

type sweeperProvider struct {
	remotes []RemoteResource
	deleted []string
}

func (p *sweeperProvider) ListRemoteResources(ctx context.Context) ([]RemoteResource, error) {
	return append([]RemoteResource(nil), p.remotes...), nil
}

func (p *sweeperProvider) Delete(ctx context.Context, remoteID string) error {
	p.deleted = append(p.deleted, remoteID)
	return nil
}
