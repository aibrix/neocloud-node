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

func TestLifecycleServiceRecordsRemoteCostOnCreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := &costProvider{}
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
	if binding.Cost.HourlyUSD != 0.44 || binding.Cost.Source != "runpod.costPerHr" || binding.Cost.ObservedAt.IsZero() {
		t.Fatalf("binding cost = %#v, want 0.44 runpod cost with observation time", binding.Cost)
	}

	stored, err := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if err != nil {
		t.Fatalf("GetByWorkloadUID returned error: %v", err)
	}
	if stored.Cost.HourlyUSD != 0.44 || stored.Cost.Source != "runpod.costPerHr" || stored.Cost.ObservedAt.IsZero() {
		t.Fatalf("stored cost = %#v, want persisted provider cost", stored.Cost)
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

type costProvider struct{}

func (p *costProvider) Create(ctx context.Context, idempotencyKey string, request WorkloadRequest) (RemoteResource, error) {
	return RemoteResource{
		ID:             "remote-cost",
		IdempotencyKey: idempotencyKey,
		Provider:       "runpod",
		Phase:          RemoteRunning,
		Request:        request,
		HourlyCost:     0.44,
		CostSource:     "runpod.costPerHr",
	}, nil
}

func (p *costProvider) Get(context.Context, string) (RemoteResource, error) {
	return RemoteResource{
		ID:         "remote-cost",
		Provider:   "runpod",
		Phase:      RemoteRunning,
		HourlyCost: 0.50,
		CostSource: "runpod.adjustedCostPerHr",
	}, nil
}

func (p *costProvider) Delete(context.Context, string) error {
	return nil
}

func (p *deleteOnceProvider) Delete(ctx context.Context, remoteID string) error {
	if p.deleteErr != nil {
		err := p.deleteErr
		p.deleteErr = nil
		return err
	}
	return p.ProviderAdapter.Delete(ctx, remoteID)
}

func TestLifecycleServiceRejectsPolicyViolationBeforeProviderCreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	service := NewLifecycleService(
		store,
		provider,
		WithLaunchGuard(NewLaunchGuard(LaunchPolicy{RequireMaxPrice: true})),
	)

	_, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:    "example.com/train:latest",
			GPUCount: 1,
			Provider: "runpod",
		},
	})
	var violation *PolicyViolationError
	if !errors.As(err, &violation) {
		t.Fatalf("EnsureCreated error = %v, want PolicyViolationError", err)
	}
	if violation.Code != "MissingMaxPrice" {
		t.Fatalf("violation code = %q, want MissingMaxPrice", violation.Code)
	}
	if provider.CreateCount() != 0 {
		t.Fatalf("provider CreateCount = %d, want 0", provider.CreateCount())
	}
}

func TestLifecycleServiceRecordsProviderCreateFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := &createFailureProvider{
		err: &ProviderError{
			Provider:  "runpod",
			Operation: "create",
			Class:     ErrorClassRateLimited,
			Message:   "rate limited",
		},
	}
	service := NewLifecycleService(store, provider)

	_, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:    "example.com/train:latest",
			GPUCount: 1,
			Provider: "runpod",
		},
	})
	if err == nil {
		t.Fatal("EnsureCreated() error = nil, want provider error")
	}

	stored, getErr := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if getErr != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", getErr)
	}
	if stored.Phase != BindingFailed {
		t.Fatalf("phase = %s, want Failed", stored.Phase)
	}
	if len(stored.Attempts) != 1 {
		t.Fatalf("attempts = %#v, want one attempt", stored.Attempts)
	}
	if stored.Attempts[0].Class != ErrorClassRateLimited || !stored.Attempts[0].Retryable || !stored.Attempts[0].FallbackAllowed {
		t.Fatalf("attempt = %#v, want rate-limited retryable fallback attempt", stored.Attempts[0])
	}
	condition := FindCondition(stored.Conditions, ConditionProviderError)
	if condition == nil || condition.Status != ConditionTrue || condition.Reason != string(ErrorClassRateLimited) {
		t.Fatalf("provider condition = %#v, want rate-limited true condition", condition)
	}
}

func TestLifecycleServiceRecordsRemoteDeletedOnSync(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
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
		t.Fatalf("EnsureCreated() error = %v", err)
	}
	if err := provider.Delete(ctx, binding.RemoteID); err != nil {
		t.Fatalf("fake Delete() setup error = %v", err)
	}

	_, err = service.SyncStatus(ctx, "default", "pod-123")
	if !errors.Is(err, ErrRemoteNotFound) {
		t.Fatalf("SyncStatus() error = %v, want ErrRemoteNotFound", err)
	}

	stored, getErr := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if getErr != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", getErr)
	}
	if stored.Phase != BindingFailed {
		t.Fatalf("phase = %s, want Failed", stored.Phase)
	}
	condition := FindCondition(stored.Conditions, ConditionRemoteDeleted)
	if condition == nil || condition.Status != ConditionTrue {
		t.Fatalf("remote deleted condition = %#v, want true condition", condition)
	}
}

func TestLifecycleServiceRecordsDeleteFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := &deleteOnceProvider{ProviderAdapter: NewFakeProvider("runpod")}
	service := NewLifecycleService(store, provider)

	_, err := service.EnsureCreated(ctx, Binding{
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
		t.Fatalf("EnsureCreated() error = %v", err)
	}

	provider.deleteErr = context.DeadlineExceeded
	if err := service.EnsureDeleted(ctx, "default", "pod-123"); err == nil {
		t.Fatal("EnsureDeleted() error = nil, want delete error")
	}

	stored, getErr := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if getErr != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", getErr)
	}
	condition := FindCondition(stored.Conditions, ConditionDeleteFailure)
	if condition == nil || condition.Status != ConditionTrue || condition.Reason != string(ErrorClassTimeout) {
		t.Fatalf("delete failure condition = %#v, want timeout true condition", condition)
	}
}

type createFailureProvider struct {
	err error
}

func (p *createFailureProvider) Create(ctx context.Context, idempotencyKey string, request WorkloadRequest) (RemoteResource, error) {
	return RemoteResource{}, p.err
}

func (p *createFailureProvider) Get(ctx context.Context, remoteID string) (RemoteResource, error) {
	return RemoteResource{}, ErrRemoteNotFound
}

func (p *createFailureProvider) Delete(ctx context.Context, remoteID string) error {
	return nil
}
