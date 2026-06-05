package core

import (
	"context"
	"testing"
)

func TestMemoryBindingStoreCreateOrGetIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()

	first, created, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "H100",
			MaxPrice:   "4.50",
			Provider:   "runpod",
			RegionHint: "us",
		},
	})
	if err != nil {
		t.Fatalf("CreateOrGet returned error: %v", err)
	}
	if !created {
		t.Fatalf("first CreateOrGet created = false, want true")
	}

	second, created, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:    "different:image",
			GPUCount: 8,
			Provider: "lambda",
		},
	})
	if err != nil {
		t.Fatalf("second CreateOrGet returned error: %v", err)
	}
	if created {
		t.Fatalf("second CreateOrGet created = true, want false")
	}
	if second.Request.Image != first.Request.Image {
		t.Fatalf("second binding image = %q, want original %q", second.Request.Image, first.Request.Image)
	}
	if second.WorkloadUID != "pod-123" {
		t.Fatalf("second binding workload UID = %q, want pod-123", second.WorkloadUID)
	}
}

func TestMemoryBindingStoreDeleteStatePreservesRemoteID(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()

	binding, _, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		RemoteID:    "remote-1",
		Phase:       BindingRunning,
		Request: WorkloadRequest{
			Image:    "example.com/train:latest",
			GPUCount: 1,
			Provider: "runpod",
		},
	})
	if err != nil {
		t.Fatalf("CreateOrGet returned error: %v", err)
	}
	binding.RemoteID = "remote-1"
	binding.Phase = BindingRunning
	if err := store.Update(ctx, binding); err != nil {
		t.Fatalf("Update returned error: %v", err)
	}

	deleting, err := store.MarkDeleting(ctx, "default", "pod-123")
	if err != nil {
		t.Fatalf("MarkDeleting returned error: %v", err)
	}
	if deleting.RemoteID != "remote-1" {
		t.Fatalf("deleting binding remote ID = %q, want remote-1", deleting.RemoteID)
	}
	if deleting.Phase != BindingDeleting {
		t.Fatalf("deleting binding phase = %q, want %q", deleting.Phase, BindingDeleting)
	}
	if !deleting.Deleting {
		t.Fatalf("deleting flag = false, want true")
	}

	if err := store.CompleteDelete(ctx, "default", "pod-123"); err != nil {
		t.Fatalf("CompleteDelete returned error: %v", err)
	}
	if _, err := store.GetByWorkloadUID(ctx, "default", "pod-123"); err == nil {
		t.Fatalf("GetByWorkloadUID after CompleteDelete returned nil error, want not found")
	}
}
