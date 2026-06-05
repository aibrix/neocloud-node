package core

import (
	"context"
	"testing"
)

func TestFakeProviderCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := NewFakeProvider("runpod")
	request := WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
		Provider: "runpod",
	}

	first, err := provider.Create(ctx, "default/pod-123", request)
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	second, err := provider.Create(ctx, "default/pod-123", request)
	if err != nil {
		t.Fatalf("second Create returned error: %v", err)
	}

	if second.ID != first.ID {
		t.Fatalf("second remote ID = %q, want %q", second.ID, first.ID)
	}
	if provider.CreateCount() != 1 {
		t.Fatalf("CreateCount = %d, want 1", provider.CreateCount())
	}
}

func TestFakeProviderGetAndDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := NewFakeProvider("runpod")
	remote, err := provider.Create(ctx, "default/pod-123", WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
		Provider: "runpod",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	got, err := provider.Get(ctx, remote.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.Phase != RemoteRunning {
		t.Fatalf("remote phase = %q, want %q", got.Phase, RemoteRunning)
	}

	if err := provider.Delete(ctx, remote.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := provider.Delete(ctx, "missing-remote"); err != nil {
		t.Fatalf("Delete missing remote returned error: %v", err)
	}
	if _, err := provider.Get(ctx, remote.ID); err == nil {
		t.Fatalf("Get after delete returned nil error, want not found")
	}
}
