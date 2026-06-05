package core

import (
	"context"
	"errors"
	"testing"
)

func TestLifecycleServiceEnsureCreatedIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	service := NewLifecycleService(store, provider)

	binding := Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:    "example.com/train:latest",
			GPUCount: 1,
			Provider: "runpod",
		},
	}

	first, err := service.EnsureCreated(ctx, binding)
	if err != nil {
		t.Fatalf("EnsureCreated returned error: %v", err)
	}
	second, err := service.EnsureCreated(ctx, binding)
	if err != nil {
		t.Fatalf("second EnsureCreated returned error: %v", err)
	}

	if first.RemoteID == "" {
		t.Fatalf("first binding remote ID is empty")
	}
	if second.RemoteID != first.RemoteID {
		t.Fatalf("second remote ID = %q, want %q", second.RemoteID, first.RemoteID)
	}
	if provider.CreateCount() != 1 {
		t.Fatalf("provider CreateCount = %d, want 1", provider.CreateCount())
	}

	stored, err := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if err != nil {
		t.Fatalf("GetByWorkloadUID returned error: %v", err)
	}
	if stored.Phase != BindingRunning {
		t.Fatalf("stored phase = %q, want %q", stored.Phase, BindingRunning)
	}
}

func TestLifecycleServiceEnsureDeletedKeepsBindingWhenRemoteDeleteFails(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := &deleteOnceProvider{ProviderAdapter: NewFakeProvider("runpod")}
	service := NewLifecycleService(store, provider)

	binding, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:    "example.com/train:latest",
			GPUCount: 1,
			Provider: "runpod",
		},
	})
	if err != nil {
		t.Fatalf("EnsureCreated returned error: %v", err)
	}

	provider.deleteErr = errors.New("temporary delete failure")
	if err := service.EnsureDeleted(ctx, "default", "pod-123"); err == nil {
		t.Fatalf("EnsureDeleted returned nil error, want delete failure")
	}

	stored, err := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if err != nil {
		t.Fatalf("GetByWorkloadUID after failed delete returned error: %v", err)
	}
	if stored.Phase != BindingDeleting {
		t.Fatalf("stored phase = %q, want %q", stored.Phase, BindingDeleting)
	}
	if stored.RemoteID != binding.RemoteID {
		t.Fatalf("stored remote ID = %q, want %q", stored.RemoteID, binding.RemoteID)
	}

	provider.deleteErr = nil
	if err := service.EnsureDeleted(ctx, "default", "pod-123"); err != nil {
		t.Fatalf("second EnsureDeleted returned error: %v", err)
	}
	if _, err := store.GetByWorkloadUID(ctx, "default", "pod-123"); err == nil {
		t.Fatalf("GetByWorkloadUID after successful delete returned nil error, want not found")
	}
}

type deleteOnceProvider struct {
	ProviderAdapter
	deleteErr error
}

func (p *deleteOnceProvider) Delete(ctx context.Context, remoteID string) error {
	if p.deleteErr != nil {
		err := p.deleteErr
		p.deleteErr = nil
		return err
	}
	return p.ProviderAdapter.Delete(ctx, remoteID)
}
