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

func TestLifecycleServiceFallsBackOnCapacityCreateFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	runpod := &createFailureProvider{
		provider: "runpod",
		err: &ProviderError{
			Provider:  "runpod",
			Operation: "create",
			Class:     ErrorClassCapacity,
			Message:   "no A40 capacity",
		},
	}
	lambda := NewFakeProvider("lambda")
	service := NewLifecycleService(
		store,
		runpod,
		WithFallbackProviders(map[string]ProviderAdapter{
			"runpod": runpod,
			"lambda": lambda,
		}, []string{"lambda"}),
	)

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
	if binding.Phase != BindingRunning {
		t.Fatalf("phase = %s, want Running", binding.Phase)
	}
	if binding.Placement.Provider != "lambda" {
		t.Fatalf("placement provider = %q, want lambda", binding.Placement.Provider)
	}
	if binding.RemoteID != "lambda-1" {
		t.Fatalf("remote ID = %q, want lambda-1", binding.RemoteID)
	}
	if len(binding.Attempts) != 1 {
		t.Fatalf("attempts = %#v, want one failed runpod attempt", binding.Attempts)
	}
	attempt := binding.Attempts[0]
	if attempt.Provider != "runpod" || attempt.Class != ErrorClassCapacity || !attempt.FallbackAllowed {
		t.Fatalf("attempt = %#v, want capacity fallback attempt for runpod", attempt)
	}
	if condition := FindCondition(binding.Conditions, ConditionCapacity); condition != nil && condition.Status == ConditionTrue {
		t.Fatalf("capacity condition = %#v, want inactive after fallback success", condition)
	}
	ready := FindCondition(binding.Conditions, ConditionReady)
	if ready == nil || ready.Status != ConditionTrue || ready.Reason != "RemoteCreated" {
		t.Fatalf("ready condition = %#v, want RemoteCreated true after fallback success", ready)
	}
	if lambda.CreateCount() != 1 {
		t.Fatalf("lambda CreateCount = %d, want 1", lambda.CreateCount())
	}

	stored, err := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if stored.Placement.Provider != "lambda" || stored.RemoteID != "lambda-1" {
		t.Fatalf("stored binding = %#v, want lambda placement", stored)
	}
	if condition := FindCondition(stored.Conditions, ConditionCapacity); condition != nil && condition.Status == ConditionTrue {
		t.Fatalf("stored capacity condition = %#v, want inactive after fallback success", condition)
	}
}

func TestLifecycleServiceStopsFallbackOnAuthFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	runpod := &createFailureProvider{
		provider: "runpod",
		err: &ProviderError{
			Provider:  "runpod",
			Operation: "create",
			Class:     ErrorClassAuth,
			Message:   "bad api key",
		},
	}
	lambda := NewFakeProvider("lambda")
	service := NewLifecycleService(
		store,
		runpod,
		WithFallbackProviders(map[string]ProviderAdapter{
			"runpod": runpod,
			"lambda": lambda,
		}, []string{"lambda"}),
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
	if err == nil {
		t.Fatal("EnsureCreated() error = nil, want auth error")
	}
	if lambda.CreateCount() != 0 {
		t.Fatalf("lambda CreateCount = %d, want no fallback on auth", lambda.CreateCount())
	}
	stored, getErr := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if getErr != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", getErr)
	}
	if stored.Phase != BindingFailed {
		t.Fatalf("phase = %s, want Failed", stored.Phase)
	}
	if len(stored.Attempts) != 1 || stored.Attempts[0].FallbackAllowed {
		t.Fatalf("attempts = %#v, want one non-fallback auth attempt", stored.Attempts)
	}
}

func TestLifecycleServiceUsesSelectedProviderForSyncAndDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	runpod := &createFailureProvider{
		provider: "runpod",
		err: &ProviderError{
			Provider:  "runpod",
			Operation: "create",
			Class:     ErrorClassCapacity,
			Message:   "no capacity",
		},
	}
	lambda := &trackingProvider{ProviderAdapter: NewFakeProvider("lambda")}
	service := NewLifecycleService(
		store,
		runpod,
		WithFallbackProviders(map[string]ProviderAdapter{
			"runpod": runpod,
			"lambda": lambda,
		}, []string{"lambda"}),
	)

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
	if binding.Placement.Provider != "lambda" {
		t.Fatalf("placement provider = %s, want lambda", binding.Placement.Provider)
	}

	if _, err := service.SyncStatus(ctx, "default", "pod-123"); err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	if lambda.getCount != 1 {
		t.Fatalf("lambda getCount = %d, want selected provider sync", lambda.getCount)
	}

	if err := service.EnsureDeleted(ctx, "default", "pod-123"); err != nil {
		t.Fatalf("EnsureDeleted() error = %v", err)
	}
	if lambda.deleteCount != 1 {
		t.Fatalf("lambda deleteCount = %d, want selected provider delete", lambda.deleteCount)
	}
}

func TestLifecycleServiceUsesCreateCandidatePlanner(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	runpod := &createFailureProvider{
		provider: "runpod",
		err: &ProviderError{
			Provider:  "runpod",
			Operation: "create",
			Class:     ErrorClassCapacity,
			Message:   "no runpod capacity",
		},
	}
	lambda := NewFakeProvider("lambda")
	service := NewLifecycleService(
		store,
		runpod,
		WithFallbackProviders(map[string]ProviderAdapter{
			"runpod": runpod,
			"lambda": lambda,
		}, []string{"runpod"}),
		WithCreateCandidatePlanner(staticCreateCandidatePlanner{
			candidates: []CreateCandidate{
				{
					Provider: "lambda",
					Request: WorkloadRequest{
						Image:      "example.com/train:latest",
						GPUCount:   1,
						GPUType:    "A10",
						Provider:   "lambda",
						RegionHint: "us-west-1",
					},
				},
				{
					Provider: "runpod",
					Request: WorkloadRequest{
						Image:      "example.com/train:latest",
						GPUCount:   1,
						GPUType:    "A10",
						Provider:   "runpod",
						RegionHint: "US-TX-3",
					},
				},
			},
		}),
	)

	binding, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "A10",
			Provider:   "runpod",
			RegionHint: "US-TX-3",
		},
	})
	if err != nil {
		t.Fatalf("EnsureCreated() error = %v", err)
	}
	if binding.Placement.Provider != "lambda" {
		t.Fatalf("placement provider = %q, want lambda", binding.Placement.Provider)
	}
	if binding.Placement.Region != "us-west-1" {
		t.Fatalf("placement region = %q, want us-west-1", binding.Placement.Region)
	}
	if lambda.CreateCount() != 1 {
		t.Fatalf("lambda CreateCount = %d, want planner-selected lambda create", lambda.CreateCount())
	}
}

func TestLifecycleServiceRecordsCreateCandidatePlannerFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	service := NewLifecycleService(
		store,
		provider,
		WithCreateCandidatePlanner(staticCreateCandidatePlanner{
			err: &ProviderError{
				Provider:  "runpod",
				Operation: "placement",
				Class:     ErrorClassCapacity,
				Message:   "no feasible offers",
			},
		}),
	)

	_, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			Provider:   "runpod",
			RegionHint: "US-TX-3",
		},
	})
	if err == nil {
		t.Fatal("EnsureCreated() error = nil, want planner failure")
	}
	if provider.CreateCount() != 0 {
		t.Fatalf("provider CreateCount = %d, want no remote create after planner failure", provider.CreateCount())
	}

	stored, getErr := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if getErr != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", getErr)
	}
	if stored.Phase != BindingFailed {
		t.Fatalf("phase = %s, want Failed", stored.Phase)
	}
	if len(stored.Attempts) != 1 {
		t.Fatalf("attempts = %#v, want planner failure attempt", stored.Attempts)
	}
	if stored.Attempts[0].Reason != "PlacementFailed" || stored.Attempts[0].Class != ErrorClassCapacity {
		t.Fatalf("attempt = %#v, want capacity placement failure", stored.Attempts[0])
	}
	condition := FindCondition(stored.Conditions, ConditionCapacity)
	if condition == nil || condition.Status != ConditionTrue {
		t.Fatalf("capacity condition = %#v, want true condition", condition)
	}
}

func TestRecordRunnerStatusMarksBindingSucceeded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "runner-success",
		WorkloadUID: "uid-runner-success",
		RemoteID:    "lambda-1",
		Phase:       BindingRunning,
		Request:     WorkloadRequest{Provider: "lambda"},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	binding, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-success", 0, "completed")
	if err != nil {
		t.Fatalf("RecordRunnerStatus() error = %v", err)
	}
	if binding.Phase != BindingSucceeded {
		t.Fatalf("phase = %s, want %s", binding.Phase, BindingSucceeded)
	}
	if binding.LastError != "" {
		t.Fatalf("last error = %q, want empty", binding.LastError)
	}
}

func TestLifecycleServiceDoesNotOverwriteTerminalRunnerStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("lambda")
	service := NewLifecycleService(store, provider)

	binding, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "runner-terminal",
		WorkloadUID: "uid-runner-terminal",
		Request: WorkloadRequest{
			Image:    "example.com/train:latest",
			GPUCount: 1,
			Provider: "lambda",
		},
	})
	if err != nil {
		t.Fatalf("EnsureCreated() error = %v", err)
	}
	if binding.Phase != BindingRunning {
		t.Fatalf("created phase = %s, want Running", binding.Phase)
	}
	if _, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-terminal", 0, "completed"); err != nil {
		t.Fatalf("RecordRunnerStatus() error = %v", err)
	}

	synced, err := service.SyncStatus(ctx, "default", "uid-runner-terminal")
	if err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	if synced.Phase != BindingSucceeded {
		t.Fatalf("synced phase = %s, want Succeeded", synced.Phase)
	}
}

func TestRecordRunnerStatusMarksBindingFailed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "runner-failed",
		WorkloadUID: "uid-runner-failed",
		RemoteID:    "lambda-1",
		Phase:       BindingRunning,
		Request:     WorkloadRequest{Provider: "lambda"},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	binding, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-failed", 137, "killed")
	if err != nil {
		t.Fatalf("RecordRunnerStatus() error = %v", err)
	}
	if binding.Phase != BindingFailed {
		t.Fatalf("phase = %s, want %s", binding.Phase, BindingFailed)
	}
	if binding.LastError == "" {
		t.Fatal("last error is empty, want runner failure")
	}
	condition := FindCondition(binding.Conditions, ConditionProviderError)
	if condition == nil || condition.Reason != "RunnerExitCode" || condition.Status != ConditionTrue {
		t.Fatalf("condition = %#v, want RunnerExitCode provider error", condition)
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
	provider string
	err      error
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

type trackingProvider struct {
	ProviderAdapter
	getCount    int
	deleteCount int
}

func (p *trackingProvider) Get(ctx context.Context, remoteID string) (RemoteResource, error) {
	p.getCount++
	return p.ProviderAdapter.Get(ctx, remoteID)
}

func (p *trackingProvider) Delete(ctx context.Context, remoteID string) error {
	p.deleteCount++
	return p.ProviderAdapter.Delete(ctx, remoteID)
}

type staticCreateCandidatePlanner struct {
	candidates []CreateCandidate
	err        error
}

func (p staticCreateCandidatePlanner) CreateCandidates(ctx context.Context, binding Binding) ([]CreateCandidate, error) {
	if p.err != nil {
		return nil, p.err
	}
	return append([]CreateCandidate(nil), p.candidates...), nil
}
