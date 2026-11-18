package core

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"
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
	service := NewLifecycleService(store, provider, WithCreateCandidatePlanner(staticCreateCandidatePlanner{
		candidates: []CreateCandidate{
			{
				Provider:            "runpod",
				EstimatedHourlyCost: 0.88,
				CostSource:          CostSourcePlacementEstimate,
				Request: WorkloadRequest{
					Image:    "example.com/train:latest",
					GPUCount: 1,
					Provider: "runpod",
				},
			},
		},
	}))

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

func TestLifecycleServiceRecordsCandidateCostEstimateOnCreate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	service := NewLifecycleService(store, provider, WithCreateCandidatePlanner(staticCreateCandidatePlanner{
		candidates: []CreateCandidate{
			{
				Provider:            "runpod",
				EstimatedHourlyCost: 0.72,
				CostSource:          CostSourcePlacementEstimate,
				Request: WorkloadRequest{
					Image:      "example.com/train:latest",
					GPUCount:   1,
					GPUType:    "A40",
					Provider:   "runpod",
					RegionHint: "US-TX-3",
				},
			},
		},
	}))

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
	if binding.Cost.HourlyUSD != 0.72 || binding.Cost.Source != CostSourcePlacementEstimate || binding.Cost.ObservedAt.IsZero() {
		t.Fatalf("binding cost = %#v, want placement estimate", binding.Cost)
	}

	stored, err := store.GetByWorkloadUID(ctx, "default", "pod-123")
	if err != nil {
		t.Fatalf("GetByWorkloadUID returned error: %v", err)
	}
	if stored.Cost.HourlyUSD != 0.72 || stored.Cost.Source != CostSourcePlacementEstimate || stored.Cost.ObservedAt.IsZero() {
		t.Fatalf("stored cost = %#v, want persisted placement estimate", stored.Cost)
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

	binding, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-success", RunnerStatus{
		Phase:   RunnerPhaseSucceeded,
		Message: "completed",
	})
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

func TestRunnerTerminalCallbackClearsPublishedEndpoints(t *testing.T) {
	for _, phase := range []RunnerPhase{RunnerPhaseRunning, RunnerPhaseSucceeded, RunnerPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryBindingStore()
			published := []Endpoint{{ContainerPort: 8000, Protocol: "TCP", Host: "203.0.113.10", Port: 8000}}
			_, _, err := store.CreateOrGet(ctx, Binding{Namespace: "default", WorkloadUID: "runner-endpoints", RemoteID: "vm", Phase: BindingRunning, Endpoints: published})
			if err != nil {
				t.Fatal(err)
			}
			got, err := RecordRunnerStatus(ctx, store, "default", "runner-endpoints", RunnerStatus{Phase: phase})
			if err != nil {
				t.Fatal(err)
			}
			stored, err := store.GetByWorkloadUID(ctx, "default", "runner-endpoints")
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if phase == RunnerPhaseRunning {
				want = 1
			}
			if len(got.Endpoints) != want || len(stored.Endpoints) != want {
				t.Fatalf("callback endpoints: returned=%v stored=%v want count=%d", got.Endpoints, stored.Endpoints, want)
			}
		})
	}
}

func TestRecordRunnerStatusMarksBindingRunning(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, Binding{
		Namespace:   "default",
		Name:        "runner-running",
		WorkloadUID: "uid-runner-running",
		RemoteID:    "lambda-1",
		Phase:       BindingPending,
		Request:     WorkloadRequest{Provider: "lambda"},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	binding, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-running", RunnerStatus{
		Phase:   RunnerPhaseRunning,
		Message: "runner started",
	})
	if err != nil {
		t.Fatalf("RecordRunnerStatus() error = %v", err)
	}
	if binding.Phase != BindingRunning {
		t.Fatalf("phase = %s, want %s", binding.Phase, BindingRunning)
	}
	condition := FindCondition(binding.Conditions, ConditionReady)
	if condition == nil || condition.Status != ConditionTrue || condition.Reason != "RunnerRunning" {
		t.Fatalf("ready condition = %#v, want RunnerRunning true", condition)
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
	if _, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-terminal", RunnerStatus{
		Phase:   RunnerPhaseSucceeded,
		Message: "completed",
	}); err != nil {
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

	binding, err := RecordRunnerStatus(ctx, store, "default", "uid-runner-failed", RunnerStatus{
		Phase:    RunnerPhaseFailed,
		ExitCode: 137,
		Message:  "killed",
	})
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

func TestLifecycleServiceRecordsRemoteFailureReasonOnSync(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		reason      string
		message     string
		wantReason  string
		wantMessage string
	}{
		{
			name:        "provider reason",
			reason:      "RemoteExited",
			message:     "remote exited without an exit code",
			wantReason:  "RemoteExited",
			wantMessage: "remote exited without an exit code",
		},
		{
			name:        "generic failure",
			wantReason:  "RemoteFailed",
			wantMessage: "remote workload failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store := NewMemoryBindingStore()
			provider := NewFakeProvider("runpod")
			service := NewLifecycleService(store, provider)

			binding, err := service.EnsureCreated(ctx, Binding{
				Namespace:   "default",
				Name:        "serve",
				WorkloadUID: "pod-failed",
				Request: WorkloadRequest{
					Image:    "example.com/serve:latest",
					GPUCount: 1,
					Provider: "runpod",
				},
			})
			if err != nil {
				t.Fatalf("EnsureCreated() error = %v", err)
			}
			if err := provider.SetStatus(binding.RemoteID, RemoteFailed, tt.reason, tt.message); err != nil {
				t.Fatalf("SetStatus() setup error = %v", err)
			}

			synced, err := service.SyncStatus(ctx, "default", "pod-failed")
			if err != nil {
				t.Fatalf("SyncStatus() error = %v", err)
			}
			if synced.Phase != BindingFailed {
				t.Fatalf("phase = %s, want Failed", synced.Phase)
			}
			if synced.LastError != tt.wantMessage {
				t.Fatalf("last error = %q, want %q", synced.LastError, tt.wantMessage)
			}
			providerError := FindCondition(synced.Conditions, ConditionProviderError)
			if providerError == nil || providerError.Status != ConditionTrue || providerError.Reason != tt.wantReason {
				t.Fatalf("provider error condition = %#v, want true %s", providerError, tt.wantReason)
			}
			ready := FindCondition(synced.Conditions, ConditionReady)
			if ready == nil || ready.Status != ConditionFalse || ready.Reason != tt.wantReason {
				t.Fatalf("ready condition = %#v, want false %s", ready, tt.wantReason)
			}

			stored, err := store.GetByWorkloadUID(ctx, "default", "pod-failed")
			if err != nil {
				t.Fatalf("GetByWorkloadUID() error = %v", err)
			}
			if stored.Phase != BindingFailed || stored.LastError != tt.wantMessage {
				t.Fatalf("stored binding = %s/%q, want Failed/%q", stored.Phase, stored.LastError, tt.wantMessage)
			}
		})
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

func TestLifecycleServiceKeepsPublishedEndpoints(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	service := NewLifecycleService(store, provider)

	binding, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "default",
		Name:        "serve",
		WorkloadUID: "pod-endpoints",
		Request: WorkloadRequest{
			Image:    "example.com/serve:latest",
			GPUCount: 1,
			Provider: "runpod",
		},
	})
	if err != nil {
		t.Fatalf("EnsureCreated() error = %v", err)
	}
	if len(binding.Endpoints) != 0 {
		t.Fatalf("endpoints after create = %#v, want none", binding.Endpoints)
	}

	published := []Endpoint{
		{ContainerPort: 9090, Protocol: "TCP", Host: "203.0.113.10", Port: 40500},
		{ContainerPort: 8000, Protocol: "TCP", Host: "203.0.113.10", Port: 40123},
	}
	if err := provider.SetEndpoints(binding.RemoteID, published); err != nil {
		t.Fatalf("SetEndpoints() setup error = %v", err)
	}
	synced, err := service.SyncStatus(ctx, "default", "pod-endpoints")
	if err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	want := []Endpoint{published[1], published[0]}
	if !reflect.DeepEqual(synced.Endpoints, want) {
		t.Fatalf("endpoints = %#v, want sorted %#v", synced.Endpoints, want)
	}

	if err := provider.SetEndpoints(binding.RemoteID, nil); err != nil {
		t.Fatalf("SetEndpoints() setup error = %v", err)
	}
	synced, err = service.SyncStatus(ctx, "default", "pod-endpoints")
	if err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	if !reflect.DeepEqual(synced.Endpoints, want) {
		t.Fatalf("endpoints after empty observation = %#v, want kept %#v", synced.Endpoints, want)
	}
}

// The fake remote keeps reporting its endpoints after it stops, as RunPod does
// for an EXITED pod. A stopped workload must not keep publishing them.
func TestLifecycleServiceClearsEndpointsWhenWorkloadStops(t *testing.T) {
	t.Parallel()

	published := []Endpoint{{ContainerPort: 8000, Protocol: "TCP", Host: "203.0.113.10", Port: 40123}}
	for _, tt := range []struct {
		name string
		stop func(*FakeProvider, string) error
	}{
		{"remote failed", func(provider *FakeProvider, remoteID string) error {
			return provider.SetStatus(remoteID, RemoteFailed, "RemoteExited", "exited")
		}},
		{"restart stopped by restartPolicy", func(provider *FakeProvider, remoteID string) error {
			return provider.SetContainerUptime(remoteID, 4*time.Second)
		}},
		{"remote deleted outside the controller", func(provider *FakeProvider, remoteID string) error {
			return provider.Delete(context.Background(), remoteID)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			provider := NewFakeProvider("runpod")
			service, store, binding := newRestartTestService(t, provider, RestartPolicyNever)
			if err := provider.SetEndpoints(binding.RemoteID, published); err != nil {
				t.Fatalf("SetEndpoints() setup error = %v", err)
			}
			running, err := syncWithUptime(t, service, provider, binding.RemoteID, 60)
			if err != nil || !reflect.DeepEqual(running.Endpoints, published) {
				t.Fatalf("running endpoints = %#v, %v; want %#v", running.Endpoints, err, published)
			}

			if err := tt.stop(provider, binding.RemoteID); err != nil {
				t.Fatalf("stop setup error = %v", err)
			}
			synced, err := service.SyncStatus(context.Background(), "default", "pod-restart")
			if err != nil && !errors.Is(err, ErrRemoteNotFound) {
				t.Fatalf("SyncStatus() error = %v", err)
			}
			if synced.Phase != BindingFailed || len(synced.Endpoints) != 0 {
				t.Fatalf("binding = %s endpoints=%#v, want Failed without endpoints", synced.Phase, synced.Endpoints)
			}
			stored, err := store.GetByWorkloadUID(context.Background(), "default", "pod-restart")
			if err != nil || len(stored.Endpoints) != 0 {
				t.Fatalf("stored endpoints = %#v, %v; want none", stored.Endpoints, err)
			}
		})
	}
}

func directEndpointObservation() RemoteResource {
	return RemoteResource{ID: "vm-1", Provider: "test-vm", Phase: RemoteRunning, DirectEndpointHost: "203.0.113.10"}
}

type directEndpointProvider struct {
	*FakeProvider
	remote RemoteResource
	gets   int
}

func (p *directEndpointProvider) Create(context.Context, string, WorkloadRequest) (RemoteResource, error) {
	return p.remote, nil
}
func (p *directEndpointProvider) Get(context.Context, string) (RemoteResource, error) {
	p.gets++
	return p.remote, nil
}
func (p *directEndpointProvider) ListRemoteStatuses(context.Context) ([]RemoteResource, error) {
	return []RemoteResource{p.remote}, nil
}

func TestLifecycleDirectEndpointsSurviveRestartAndSnapshots(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := &directEndpointProvider{FakeProvider: NewFakeProvider("test-vm"), remote: directEndpointObservation()}
	service := NewLifecycleService(store, provider)
	desired := Binding{Namespace: "default", Name: "serve", WorkloadUID: "pod-direct", Request: WorkloadRequest{Provider: "test-vm", Image: "serve", Ports: []Port{{Name: "metrics", ContainerPort: 9000, Protocol: "TCP"}, {Name: "http", ContainerPort: 8000}}}}
	want := []Endpoint{{ContainerPort: 8000, Protocol: "TCP", Host: "203.0.113.10", Port: 8000}, {ContainerPort: 9000, Protocol: "TCP", Host: "203.0.113.10", Port: 9000}}
	created, err := service.EnsureCreated(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(created.Endpoints, want) {
		t.Errorf("created endpoints=%#v, want %#v", created.Endpoints, want)
	}
	// Simulate persisted binding deserialization into a fresh store and service.
	data, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	var restored Binding
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	restored.Endpoints = nil // prove derivation from durable request, not old endpoints
	fresh := NewMemoryBindingStore()
	if _, _, err = fresh.CreateOrGet(ctx, restored); err != nil {
		t.Fatal(err)
	}
	restarted := NewLifecycleService(fresh, provider)
	got, err := restarted.SyncStatus(ctx, restored.Namespace, restored.WorkloadUID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Endpoints, want) {
		t.Errorf("restart endpoints=%#v, want %#v", got.Endpoints, want)
	}
	if provider.gets != 1 {
		t.Fatalf("provider GET count=%d, want 1", provider.gets)
	}
	snapshot := restarted.SnapshotRemoteStatuses(ctx, []Binding{got})
	got, err = restarted.SyncStatusWithSnapshot(ctx, got.Namespace, got.WorkloadUID, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Endpoints, want) {
		t.Errorf("snapshot endpoints=%#v, want %#v", got.Endpoints, want)
	}
	if provider.gets != 1 {
		t.Fatalf("snapshot made extra GET, count=%d", provider.gets)
	}
}

func TestRecordDirectEndpointsOnlyMapsDeclaredValidTCPPorts(t *testing.T) {
	explicit := []Endpoint{{ContainerPort: 8000, Protocol: "TCP", Host: "proxy.example", Port: 12345}}
	for _, tt := range []struct {
		name     string
		ports    []Port
		explicit []Endpoint
		want     []Endpoint
	}{
		{name: "no ports"},
		{name: "invalid ports", ports: []Port{{ContainerPort: 0}, {ContainerPort: -1}, {ContainerPort: 65536}, {ContainerPort: 8000, Protocol: "UDP"}, {ContainerPort: 9000, Protocol: "SCTP"}}},
		{name: "valid bounds", ports: []Port{{ContainerPort: 1, Protocol: "TCP"}, {ContainerPort: 65535}}, want: []Endpoint{{ContainerPort: 1, Protocol: "TCP", Host: "203.0.113.10", Port: 1}, {ContainerPort: 65535, Protocol: "TCP", Host: "203.0.113.10", Port: 65535}}},
		{name: "explicit wins", ports: []Port{{ContainerPort: 8000}, {ContainerPort: 9000}}, explicit: explicit, want: explicit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remote := directEndpointObservation()
			remote.Endpoints = tt.explicit
			binding := Binding{Request: WorkloadRequest{Ports: tt.ports}}
			recordRemoteEndpoints(&binding, remote)
			if !reflect.DeepEqual(binding.Endpoints, tt.want) {
				t.Fatalf("endpoints=%#v, want %#v", binding.Endpoints, tt.want)
			}
		})
	}
}

func TestTerminalCleanupDelay(t *testing.T) {
	for _, phase := range []BindingPhase{BindingSucceeded, BindingFailed} {
		for _, tc := range []struct {
			name           string
			delay, elapsed time.Duration
			deleted        bool
		}{
			{"disabled", 0, time.Hour, false},
			{"before", time.Minute, time.Minute - time.Nanosecond, false},
			{"at", time.Minute, time.Minute, true},
			{"after", time.Minute, 2 * time.Minute, true},
		} {
			t.Run(string(phase)+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				finished := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				store := NewMemoryBindingStore()
				binding := terminalTestBinding(phase, finished)
				if _, _, err := store.CreateOrGet(ctx, binding); err != nil {
					t.Fatal(err)
				}
				provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod")}
				events := 0
				service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(tc.delay), WithRemoteCleanupObserver(func(b Binding) {
					persisted, err := store.GetByWorkloadUID(ctx, b.Namespace, b.WorkloadUID)
					if err != nil || !RemoteCleanedUp(persisted) {
						t.Errorf("event before persistence: %v", err)
					}
					events++
				}))
				service.now = func() time.Time { return finished.Add(tc.elapsed) }
				for i := 0; i < 3; i++ {
					got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
					if err != nil {
						t.Fatal(err)
					}
					if got.Phase != phase || got.RemoteID != binding.RemoteID || got.LastError != binding.LastError {
						t.Fatalf("result changed: %+v", got)
					}
					if RemoteCleanedUp(got) != tc.deleted {
						t.Fatalf("cleanup = %v", RemoteCleanedUp(got))
					}
				}
				want := 0
				if tc.deleted {
					want = 1
				}
				if provider.deletes != want || events != want || provider.gets != want {
					t.Fatalf("deletes/events = %d/%d, want %d", provider.deletes, events, want)
				}
				if tc.deleted {
					if err := service.EnsureDeleted(ctx, binding.Namespace, binding.WorkloadUID); err != nil {
						t.Fatal(err)
					}
					if provider.deletes != 1 {
						t.Fatal("Pod deletion repeated completed cleanup")
					}
				}
			})
		}
	}
}

func TestTerminalCleanupDue(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service := NewLifecycleService(NewMemoryBindingStore(), NewFakeProvider("runpod"), WithTerminalCleanupDelay(time.Minute))
	service.now = func() time.Time { return now }

	future := terminalTestBinding(BindingSucceeded, now.Add(time.Minute))
	expired := terminalTestBinding(BindingFailed, now.Add(-time.Minute))
	legacy := terminalTestBinding(BindingSucceeded, time.Time{})
	legacy.Conditions = nil
	pending := terminalTestBinding(BindingSucceeded, now.Add(time.Hour))
	SetCondition(&pending, BindingCondition{Type: ConditionRemoteCleanedUp, Status: ConditionFalse, Reason: "DeletionRequested"})
	cleaned := expired
	SetCondition(&cleaned, BindingCondition{Type: ConditionRemoteCleanedUp, Status: ConditionTrue})
	deleting := expired
	deleting.Deleting = true

	for _, tc := range []struct {
		name    string
		binding Binding
		want    bool
	}{
		{"before deadline", future, false},
		{"at deadline", terminalTestBinding(BindingSucceeded, now.Add(-time.Minute)), true},
		{"legacy timestamp", legacy, true},
		{"pending confirmation", pending, true},
		{"already cleaned", cleaned, false},
		{"deleting", deleting, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := service.TerminalCleanupDue(tc.binding); got != tc.want {
				t.Fatalf("TerminalCleanupDue() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTerminalCleanupRetryAndRestart(t *testing.T) {
	ctx := context.Background()
	finished := time.Now().Add(-time.Hour)
	store := NewMemoryBindingStore()
	binding := terminalTestBinding(BindingFailed, finished)
	store.CreateOrGet(ctx, binding)
	provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod"), failures: 1}
	service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
	if _, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); err == nil {
		t.Fatal("expected delete failure")
	}
	// A new service has no timer or other in-memory history.
	service = NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
	got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil {
		t.Fatal(err)
	}
	if !RemoteCleanedUp(got) || provider.deletes != 2 {
		t.Fatalf("cleanup = %v; deletes = %d", RemoteCleanedUp(got), provider.deletes)
	}
	if !FindCondition(got.Conditions, ConditionTerminal).LastTransitionTime.Equal(finished) {
		t.Fatal("retry moved terminal timestamp")
	}
}

func TestTerminalCleanupWaitsForConfirmationAcrossRestart(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBindingStore()
	binding := terminalTestBinding(BindingSucceeded, time.Now().Add(-time.Hour))
	store.CreateOrGet(ctx, binding)
	provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod"), asynchronous: true, phase: RemotePending}
	service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
	got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil || RemoteCleanedUp(got) {
		t.Fatalf("premature cleanup: %+v, %v", got, err)
	}
	provider.getError = errors.New("temporary read error")
	// A longer delay after restart must not postpone confirmation of an accepted delete.
	service = NewLifecycleService(store, provider, WithTerminalCleanupDelay(2*time.Hour))
	if _, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); err == nil {
		t.Fatal("expected confirmation error")
	}
	provider.getError = ErrRemoteNotFound
	got, err = service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil || !RemoteCleanedUp(got) || provider.deletes != 1 {
		t.Fatalf("confirmation: cleaned=%v deletes=%d err=%v", RemoteCleanedUp(got), provider.deletes, err)
	}
}

func TestTerminalCleanupLegacyTimestamp(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	store := NewMemoryBindingStore()
	binding := terminalTestBinding(BindingSucceeded, now.Add(-time.Hour))
	binding.Conditions[0].Type = ConditionReady
	store.CreateOrGet(ctx, binding)
	provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod")}
	service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
	service.now = func() time.Time { return now }
	got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil {
		t.Fatal(err)
	}
	if provider.deletes != 0 || !FindCondition(got.Conditions, ConditionTerminal).LastTransitionTime.Equal(now) {
		t.Fatal("used Ready time instead of first terminal observation")
	}
	now = now.Add(time.Minute)
	if _, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); err != nil {
		t.Fatal(err)
	}
	if provider.deletes != 1 {
		t.Fatal("legacy binding did not expire")
	}
}

func TestRunnerTerminalTimestampAndLateCallbacks(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryBindingStore()
	binding := terminalTestBinding(BindingRunning, time.Now().Add(-time.Hour))
	binding.Conditions[0].Type = ConditionReady
	store.CreateOrGet(ctx, binding)
	got, err := RecordRunnerStatus(ctx, store, binding.Namespace, binding.WorkloadUID, RunnerStatus{Phase: RunnerPhaseSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	terminal := FindCondition(got.Conditions, ConditionTerminal)
	if terminal == nil || !terminal.LastTransitionTime.After(binding.Conditions[0].LastTransitionTime) {
		t.Fatal("terminal timestamp reused Running timestamp")
	}
	for _, phase := range []RunnerPhase{RunnerPhaseRunning, RunnerPhaseSucceeded, RunnerPhaseFailed} {
		next, err := RecordRunnerStatus(ctx, store, binding.Namespace, binding.WorkloadUID, RunnerStatus{Phase: phase})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, next) {
			t.Fatalf("late %s changed terminal result", phase)
		}
	}
}

func TestTerminalCleanupPersistenceFailure(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(strconv.Itoa(failAt), func(t *testing.T) {
			ctx := context.Background()
			store := &cleanupFailingStore{BindingStore: NewMemoryBindingStore(), failAt: failAt}
			binding := terminalTestBinding(BindingSucceeded, time.Now().Add(-time.Hour))
			store.CreateOrGet(ctx, binding)
			provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod")}
			events := 0
			service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute), WithRemoteCleanupObserver(func(Binding) { events++ }))
			if _, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); err == nil {
				t.Fatal("expected persistence error")
			}
			stored, _ := store.GetByWorkloadUID(ctx, binding.Namespace, binding.WorkloadUID)
			if RemoteCleanedUp(stored) || events != 0 {
				t.Fatal("completion escaped failed persistence")
			}
			got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
			if err != nil || !RemoteCleanedUp(got) || events != 1 {
				t.Fatalf("retry failed: %v, events %d", err, events)
			}
			want := 1
			if failAt == 1 {
				want = 2
			}
			if provider.deletes != want {
				t.Fatalf("deletes = %d, want %d", provider.deletes, want)
			}
		})
	}
}

func terminalTestBinding(phase BindingPhase, finished time.Time) Binding {
	return Binding{Namespace: "default", Name: "job", WorkloadUID: "terminal-job", RemoteID: "remote", Phase: phase,
		Request: WorkloadRequest{Provider: "runpod"}, LastError: "preserve result",
		Conditions: []BindingCondition{{Type: ConditionTerminal, Status: ConditionTrue, Reason: string(phase), LastTransitionTime: finished}}}
}

type terminalTestProvider struct {
	*FakeProvider
	deletes, failures, gets int
	asynchronous            bool
	phase                   RemotePhase
	getError                error
}

func (p *terminalTestProvider) Delete(context.Context, string) error {
	p.deletes++
	if p.failures > 0 {
		p.failures--
		return errors.New("temporary delete error")
	}
	if !p.asynchronous {
		p.phase = RemoteDeleted
	}
	return nil
}
func (p *terminalTestProvider) Get(context.Context, string) (RemoteResource, error) {
	p.gets++
	return RemoteResource{ID: "remote", Phase: p.phase}, p.getError
}

type cleanupFailingStore struct {
	BindingStore
	updates, failAt int
}

func (s *cleanupFailingStore) Update(ctx context.Context, binding Binding) (Binding, error) {
	s.updates++
	if s.updates == s.failAt {
		return Binding{}, errors.New("persistence unavailable")
	}
	return s.BindingStore.Update(ctx, binding)
}

func TestTerminalTimestampOnRemoteCompletion(t *testing.T) {
	for _, phase := range []RemotePhase{RemoteSucceeded, RemoteFailed} {
		t.Run(string(phase), func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			store := NewMemoryBindingStore()
			binding := terminalTestBinding(BindingRunning, now.Add(-time.Hour))
			binding.Conditions[0].Type = ConditionReady
			store.CreateOrGet(ctx, binding)
			provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod"), phase: phase}
			service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
			service.now = func() time.Time { return now }
			got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
			if err != nil {
				t.Fatal(err)
			}
			condition := FindCondition(got.Conditions, ConditionTerminal)
			if condition == nil || !condition.LastTransitionTime.Equal(now) || provider.deletes != 0 {
				t.Fatalf("terminal = %+v, deletes = %d", condition, provider.deletes)
			}
		})
	}
}

func TestTerminalCleanupDoesNotOverwriteConcurrentDeletion(t *testing.T) {
	ctx := context.Background()
	memory := NewMemoryBindingStore()
	store := &cleanupConcurrentStore{BindingStore: memory}
	binding := terminalTestBinding(BindingSucceeded, time.Now().Add(-time.Hour))
	store.CreateOrGet(ctx, binding)
	provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod")}
	service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
	if _, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("stale cleanup: %v", err)
	}
	got, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Deleting || got.Phase != BindingDeleting || RemoteCleanedUp(got) {
		t.Fatalf("cleanup overwrote concurrent state: %+v", got)
	}
	if provider.deletes != 1 {
		t.Fatal("status sync retried a binding being deleted")
	}
}

type cleanupConcurrentStore struct {
	BindingStore
	raced bool
}

func (s *cleanupConcurrentStore) Update(ctx context.Context, binding Binding) (Binding, error) {
	if !s.raced {
		s.raced = true
		if _, err := s.MarkDeleting(ctx, binding.Namespace, binding.WorkloadUID); err != nil {
			return Binding{}, err
		}
	}
	return s.BindingStore.Update(ctx, binding)
}

func TestTerminalCleanupIgnoresIneligibleBindings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		phase    BindingPhase
		remote   string
		deleting bool
	}{
		{"running", BindingRunning, "remote", false},
		{"pending", BindingPending, "remote", false},
		{"deleted", BindingDeleted, "remote", false},
		{"deleting", BindingFailed, "remote", true},
		{"no remote", BindingFailed, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := NewMemoryBindingStore()
			binding := terminalTestBinding(tc.phase, time.Now().Add(-time.Hour))
			binding.RemoteID, binding.Deleting = tc.remote, tc.deleting
			store.CreateOrGet(ctx, binding)
			provider := &terminalTestProvider{FakeProvider: NewFakeProvider("runpod"), phase: RemoteRunning}
			service := NewLifecycleService(store, provider, WithTerminalCleanupDelay(time.Minute))
			if _, err := service.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); err != nil {
				t.Fatal(err)
			}
			if provider.deletes != 0 {
				t.Fatal("deleted an ineligible binding")
			}
		})
	}
}
