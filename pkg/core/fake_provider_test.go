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

func TestFakeProviderReportsConfiguredHourlyCost(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := NewFakeProvider("fake").WithHourlyCost(1.25)
	remote, err := provider.Create(ctx, "default/pod-123", WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
		Provider: "fake",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.HourlyCost != 1.25 {
		t.Fatalf("remote hourly cost = %v, want 1.25", remote.HourlyCost)
	}
	if remote.CostSource != "fake.hourlyCost" {
		t.Fatalf("remote cost source = %q, want fake.hourlyCost", remote.CostSource)
	}
	got, err := provider.Get(ctx, remote.ID)
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if got.HourlyCost != 1.25 {
		t.Fatalf("Get hourly cost = %v, want 1.25", got.HourlyCost)
	}
}

func TestFakeProviderOmitsCostByDefault(t *testing.T) {
	t.Parallel()

	remote, err := NewFakeProvider("fake").Create(context.Background(), "default/pod-123", WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 1,
		Provider: "fake",
	})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if remote.HourlyCost != 0 || remote.CostSource != "" {
		t.Fatalf("remote cost = %v/%q, want zero cost", remote.HourlyCost, remote.CostSource)
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

func TestFakeProviderSupportsCompleteStatusListing(t *testing.T) {
	provider := NewFakeProvider("fake")
	if _, ok := any(provider).(RemoteStatusLister); !ok {
		t.Fatal("fake provider must opt into complete batch status listing")
	}
}
