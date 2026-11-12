package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func newRestartTestService(t *testing.T, provider ProviderAdapter, policy RestartPolicy) (*LifecycleService, BindingStore, Binding) {
	t.Helper()

	store := NewMemoryBindingStore()
	service := NewLifecycleService(store, provider)
	binding, err := service.EnsureCreated(context.Background(), Binding{
		Namespace:   "default",
		Name:        "job",
		WorkloadUID: "pod-restart",
		Request: WorkloadRequest{
			Image:         "example.com/job:latest",
			GPUCount:      1,
			Provider:      "runpod",
			RestartPolicy: string(policy),
		},
	})
	if err != nil {
		t.Fatalf("EnsureCreated() error = %v", err)
	}
	return service, store, binding
}

func syncWithUptime(t *testing.T, service *LifecycleService, provider *FakeProvider, remoteID string, seconds int) (Binding, error) {
	t.Helper()

	if err := provider.SetContainerUptime(remoteID, time.Duration(seconds)*time.Second); err != nil {
		t.Fatalf("SetContainerUptime() error = %v", err)
	}
	return service.SyncStatus(context.Background(), "default", "pod-restart")
}

func TestContainerRestartIsCountedForRestartPolicyAlways(t *testing.T) {
	t.Parallel()

	provider := NewFakeProvider("runpod")
	service, store, binding := newRestartTestService(t, provider, RestartPolicyAlways)

	// RunPod refreshes the uptime about every 30 seconds and repeats the last
	// value in between. Repeated and growing values are the same container.
	for _, seconds := range []int{26, 26, 26, 55, 55} {
		synced, err := syncWithUptime(t, service, provider, binding.RemoteID, seconds)
		if err != nil {
			t.Fatalf("SyncStatus() error = %v", err)
		}
		if synced.RestartCount != 0 || synced.ContainerUptime != time.Duration(seconds)*time.Second {
			t.Fatalf("after uptime %ds: restarts=%d uptime=%s, want 0 restarts", seconds, synced.RestartCount, synced.ContainerUptime)
		}
	}

	synced, err := syncWithUptime(t, service, provider, binding.RemoteID, 23)
	if err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	if synced.Phase != BindingRunning || synced.RestartCount != 1 || synced.ContainerUptime != 23*time.Second {
		t.Fatalf("binding = %s restarts=%d uptime=%s, want Running/1/23s", synced.Phase, synced.RestartCount, synced.ContainerUptime)
	}
	if got := provider.StopCount(); got != 0 {
		t.Fatalf("stop count = %d, want 0 for restartPolicy Always", got)
	}
	stored, err := store.GetByWorkloadUID(context.Background(), "default", "pod-restart")
	if err != nil || stored.RestartCount != 1 {
		t.Fatalf("stored restart count = %d, %v; want 1", stored.RestartCount, err)
	}
}

func TestContainerExitStopsRemoteForBatchPolicies(t *testing.T) {
	t.Parallel()

	for _, policy := range []RestartPolicy{RestartPolicyNever, RestartPolicyOnFailure} {
		t.Run(string(policy), func(t *testing.T) {
			t.Parallel()

			provider := NewFakeProvider("runpod")
			service, _, binding := newRestartTestService(t, provider, policy)
			for _, seconds := range []int{10, 10, 39} {
				if _, err := syncWithUptime(t, service, provider, binding.RemoteID, seconds); err != nil {
					t.Fatalf("SyncStatus() error = %v", err)
				}
			}
			if got := provider.StopCount(); got != 0 {
				t.Fatalf("stop count before a restart = %d, want 0", got)
			}

			synced, err := syncWithUptime(t, service, provider, binding.RemoteID, 4)
			if err != nil {
				t.Fatalf("SyncStatus() error = %v", err)
			}
			if got := provider.StopCount(); got != 1 {
				t.Fatalf("stop count = %d, want 1", got)
			}
			if synced.Phase != BindingFailed || synced.RestartCount != 1 {
				t.Fatalf("binding = %s restarts=%d, want Failed/1", synced.Phase, synced.RestartCount)
			}
			condition := FindCondition(synced.Conditions, ConditionProviderError)
			if condition == nil || condition.Reason != ReasonContainerExited {
				t.Fatalf("provider error condition = %#v, want %s", condition, ReasonContainerExited)
			}
			if ready := FindCondition(synced.Conditions, ConditionReady); ready == nil || ready.Status != ConditionFalse {
				t.Fatalf("ready condition = %#v, want False", ready)
			}

			// The binding is terminal; later syncs do not stop the remote again.
			if _, err := service.SyncStatus(context.Background(), "default", "pod-restart"); err != nil {
				t.Fatalf("SyncStatus() after failure error = %v", err)
			}
			if got := provider.StopCount(); got != 1 {
				t.Fatalf("stop count after terminal sync = %d, want 1", got)
			}
		})
	}
}

func TestContainerExitRetriesFailedStop(t *testing.T) {
	t.Parallel()

	provider := NewFakeProvider("runpod")
	service, store, binding := newRestartTestService(t, provider, RestartPolicyNever)
	if _, err := syncWithUptime(t, service, provider, binding.RemoteID, 60); err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}

	stopErr := &ProviderError{Provider: "runpod", Operation: "post", Class: ErrorClassTransient, Message: "unavailable"}
	provider.SetStopError(stopErr)
	synced, err := syncWithUptime(t, service, provider, binding.RemoteID, 5)
	if !errors.Is(err, stopErr) {
		t.Fatalf("SyncStatus() error = %v, want stop error", err)
	}
	if synced.Phase != BindingRunning || synced.RestartCount != 0 {
		t.Fatalf("binding after failed stop = %s restarts=%d, want Running/0", synced.Phase, synced.RestartCount)
	}
	stored, err := store.GetByWorkloadUID(context.Background(), "default", "pod-restart")
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if condition := FindCondition(stored.Conditions, ConditionProviderError); condition == nil || condition.Reason != reasonStopFailed {
		t.Fatalf("stored provider error condition = %#v, want %s", condition, reasonStopFailed)
	}

	// By the retry the new container has run longer than the old one did, so
	// only the failed stop recorded on the binding shows that it exited.
	provider.SetStopError(nil)
	synced, err = syncWithUptime(t, service, provider, binding.RemoteID, 90)
	if err != nil {
		t.Fatalf("SyncStatus() retry error = %v", err)
	}
	if synced.Phase != BindingFailed || synced.RestartCount != 1 || provider.StopCount() != 1 {
		t.Fatalf("binding after retry = %s restarts=%d stops=%d, want Failed/1/1",
			synced.Phase, synced.RestartCount, provider.StopCount())
	}
}

// deleteOnlyProvider hides FakeProvider.Stop.
type deleteOnlyProvider struct {
	ProviderAdapter
}

func TestContainerExitDeletesRemoteWhenProviderCannotStop(t *testing.T) {
	t.Parallel()

	fake := NewFakeProvider("runpod")
	service, _, binding := newRestartTestService(t, deleteOnlyProvider{fake}, RestartPolicyNever)
	if _, err := syncWithUptime(t, service, fake, binding.RemoteID, 60); err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	synced, err := syncWithUptime(t, service, fake, binding.RemoteID, 5)
	if err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	if synced.Phase != BindingFailed {
		t.Fatalf("phase = %s, want Failed", synced.Phase)
	}
	if _, err := fake.Get(context.Background(), binding.RemoteID); !errors.Is(err, ErrRemoteNotFound) {
		t.Fatalf("remote Get() error = %v, want deleted remote", err)
	}
}

func TestContainerUptimeIsIgnoredUntilRunning(t *testing.T) {
	t.Parallel()

	provider := NewFakeProvider("runpod")
	service, _, binding := newRestartTestService(t, provider, RestartPolicyNever)
	if err := provider.SetStatus(binding.RemoteID, RemotePending, "", ""); err != nil {
		t.Fatalf("SetStatus() error = %v", err)
	}
	synced, err := syncWithUptime(t, service, provider, binding.RemoteID, 30)
	if err != nil {
		t.Fatalf("SyncStatus() error = %v", err)
	}
	if synced.ContainerUptime != 0 {
		t.Fatalf("container uptime = %s, want unset while the remote is not running", synced.ContainerUptime)
	}
}
