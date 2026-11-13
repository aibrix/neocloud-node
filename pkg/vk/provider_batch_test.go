package vk

import (
	"context"
	"errors"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type countingStatusLister struct {
	*core.FakeProvider
	listCalls    int
	getCalls     int
	omitID       string
	missingGetID string
	listErr      error
}

type failingListStore struct{ core.BindingStore }

func (s failingListStore) List(context.Context) ([]core.Binding, error) {
	return nil, errors.New("binding list unavailable")
}

func (p *countingStatusLister) Get(ctx context.Context, remoteID string) (core.RemoteResource, error) {
	p.getCalls++
	if remoteID == p.missingGetID {
		return core.RemoteResource{}, core.ErrRemoteNotFound
	}
	return p.FakeProvider.Get(ctx, remoteID)
}

func (p *countingStatusLister) ListRemoteStatuses(ctx context.Context) ([]core.RemoteResource, error) {
	p.listCalls++
	if p.listErr != nil {
		return nil, p.listErr
	}
	items, err := p.FakeProvider.ListRemoteResources(ctx)
	if err != nil {
		return nil, err
	}
	filtered := items[:0]
	for _, item := range items {
		if item.ID != p.omitID {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func TestPeriodicStatusSyncUsesOneCompleteList(t *testing.T) {
	ctx := context.Background()
	adapter := &countingStatusLister{FakeProvider: core.NewFakeProvider("runpod")}
	provider := newTestProvider(t, adapter)
	for _, name := range []string{"batch-a", "batch-b", "batch-c"} {
		if err := provider.CreatePod(ctx, gpuPod(name, types.UID(name))); err != nil {
			t.Fatalf("CreatePod(%s): %v", name, err)
		}
	}

	provider.syncCachedPods(ctx)
	if adapter.listCalls != 1 || adapter.getCalls != 0 {
		t.Fatalf("periodic sync calls: List=%d Get=%d, want 1/0", adapter.listCalls, adapter.getCalls)
	}
	if _, err := provider.GetPod(ctx, "default", "batch-a"); err != nil {
		t.Fatalf("GetPod(): %v", err)
	}
	if adapter.listCalls != 1 || adapter.getCalls != 1 {
		t.Fatalf("direct GetPod calls: List=%d Get=%d, want 1/1", adapter.listCalls, adapter.getCalls)
	}
	if _, err := provider.GetPods(ctx); err != nil {
		t.Fatalf("GetPods(): %v", err)
	}
	if adapter.listCalls != 1 || adapter.getCalls != 4 {
		t.Fatalf("direct GetPods calls: List=%d Get=%d, want 1/4", adapter.listCalls, adapter.getCalls)
	}
}

func TestPeriodicStatusSyncGetsOnlyMissingListID(t *testing.T) {
	ctx := context.Background()
	adapter := &countingStatusLister{FakeProvider: core.NewFakeProvider("runpod")}
	provider := newTestProvider(t, adapter)
	for _, name := range []string{"batch-a", "batch-b"} {
		if err := provider.CreatePod(ctx, gpuPod(name, types.UID(name))); err != nil {
			t.Fatalf("CreatePod(%s): %v", name, err)
		}
	}
	bindings, err := provider.store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adapter.omitID = bindings[0].RemoteID
	provider.syncCachedPods(ctx)
	if adapter.listCalls != 1 || adapter.getCalls != 1 {
		t.Fatalf("calls after missing list ID: List=%d Get=%d, want 1/1", adapter.listCalls, adapter.getCalls)
	}
}

func TestPeriodicStatusSyncDoesNotFanOutAfterListFailure(t *testing.T) {
	ctx := context.Background()
	adapter := &countingStatusLister{FakeProvider: core.NewFakeProvider("runpod"), listErr: errors.New("rate limited")}
	provider := newTestProvider(t, adapter)
	for _, name := range []string{"batch-a", "batch-b"} {
		if err := provider.CreatePod(ctx, gpuPod(name, types.UID(name))); err != nil {
			t.Fatalf("CreatePod(%s): %v", name, err)
		}
	}
	provider.syncCachedPods(ctx)
	if adapter.listCalls != 1 || adapter.getCalls != 0 {
		t.Fatalf("calls after list failure: List=%d Get=%d, want 1/0", adapter.listCalls, adapter.getCalls)
	}
	bindings, err := provider.store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if binding.Phase != core.BindingRunning {
			t.Fatalf("list failure changed binding %s phase to %s", binding.Name, binding.Phase)
		}
	}
}

func TestPeriodicStatusSyncConfirmsMissingIDBeforeMarkingRemoteDeleted(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	adapter := &countingStatusLister{FakeProvider: core.NewFakeProvider("runpod")}
	provider := newTestProviderWithConfig(t, Config{Store: store, Adapter: adapter})
	if err := provider.CreatePod(ctx, gpuPod("missing", "uid-missing")); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetByWorkloadUID(ctx, "default", "uid-missing")
	if err != nil {
		t.Fatal(err)
	}
	adapter.omitID, adapter.missingGetID = binding.RemoteID, binding.RemoteID
	provider.syncCachedPods(ctx)
	if adapter.listCalls != 1 || adapter.getCalls != 1 {
		t.Fatalf("calls = List %d / Get %d, want 1 / 1", adapter.listCalls, adapter.getCalls)
	}
	binding, err = store.GetByWorkloadUID(ctx, "default", "uid-missing")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Phase != core.BindingFailed || core.FindCondition(binding.Conditions, core.ConditionRemoteDeleted) == nil {
		t.Fatalf("binding = %#v, want failed with RemoteDeleted condition", binding)
	}
}

func TestPeriodicStatusSyncListsOnlyCachedPodsBySelectedProvider(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	runpod := &countingStatusLister{FakeProvider: core.NewFakeProvider("runpod")}
	lambda := &countingStatusLister{FakeProvider: core.NewFakeProvider("lambda")}
	vast := &countingStatusLister{FakeProvider: core.NewFakeProvider("vast")}
	provider := newTestProviderWithConfig(t, Config{
		Store: store, Adapter: runpod,
		FallbackAdapters: map[string]core.ProviderAdapter{"runpod": runpod, "lambda": lambda, "vast": vast},
	})
	primary := gpuPod("primary", "uid-primary")
	fallback := gpuPod("fallback", "uid-fallback")
	fallback.Annotations[core.AnnotationProvider] = "lambda"
	for _, pod := range []*corev1.Pod{primary, fallback} {
		if err := provider.CreatePod(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := vast.Create(ctx, "default/uid-foreign", core.WorkloadRequest{Image: "ubuntu", GPUCount: 1, Provider: "vast"})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = store.CreateOrGet(ctx, core.Binding{
		Namespace: "default", Name: "foreign", WorkloadUID: "uid-foreign",
		RemoteID: remote.ID, Phase: core.BindingRunning,
		Request:   core.WorkloadRequest{Provider: "vast"},
		Placement: core.Placement{Provider: "vast"},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.syncCachedPods(ctx)
	if runpod.listCalls != 1 || lambda.listCalls != 1 || vast.listCalls != 0 {
		t.Fatalf("list calls: runpod=%d lambda=%d vast=%d, want 1/1/0", runpod.listCalls, lambda.listCalls, vast.listCalls)
	}
}

func TestPeriodicStatusSyncSkipsTerminalBinding(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	adapter := &countingStatusLister{FakeProvider: core.NewFakeProvider("runpod")}
	provider := newTestProviderWithConfig(t, Config{Store: store, Adapter: adapter})
	if err := provider.CreatePod(ctx, gpuPod("finished", "uid-finished")); err != nil {
		t.Fatal(err)
	}
	binding, err := store.GetByWorkloadUID(ctx, "default", "uid-finished")
	if err != nil {
		t.Fatal(err)
	}
	binding.Phase = core.BindingFailed
	if err := store.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	provider.syncCachedPods(ctx)
	if adapter.listCalls != 0 || adapter.getCalls != 0 {
		t.Fatalf("terminal binding triggered List=%d Get=%d, want 0/0", adapter.listCalls, adapter.getCalls)
	}
}

func TestPeriodicStatusSyncUsesSelectedFallbackProvider(t *testing.T) {
	ctx := context.Background()
	runpod := failingProvider{err: &core.ProviderError{
		Provider: "runpod", Operation: "create", Class: core.ErrorClassCapacity,
	}}
	lambda := &countingStatusLister{FakeProvider: core.NewFakeProvider("lambda")}
	provider := newTestProviderWithConfig(t, Config{
		Adapter:          runpod,
		FallbackAdapters: map[string]core.ProviderAdapter{"runpod": runpod, "lambda": lambda},
		FallbackOrder:    []string{"lambda"},
	})
	if err := provider.CreatePod(ctx, gpuPod("fallback-batch", "uid-fallback-batch")); err != nil {
		t.Fatal(err)
	}
	provider.syncCachedPods(ctx)
	if lambda.listCalls != 1 || lambda.getCalls != 0 {
		t.Fatalf("fallback Lambda calls: List=%d Get=%d, want 1/0", lambda.listCalls, lambda.getCalls)
	}
}

func TestPeriodicStatusSyncFallsBackWhenBindingListFails(t *testing.T) {
	ctx := context.Background()
	adapter := &countingStatusLister{FakeProvider: core.NewFakeProvider("lambda")}
	provider := newTestProviderWithConfig(t, Config{
		Store: failingListStore{core.NewMemoryBindingStore()}, Adapter: adapter,
	})
	if err := provider.CreatePod(ctx, gpuPod("list-failed", "uid-list-failed")); err != nil {
		t.Fatal(err)
	}
	provider.syncCachedPods(ctx)
	if adapter.listCalls != 0 || adapter.getCalls != 1 {
		t.Fatalf("binding-list failure calls: List=%d Get=%d, want 0/1", adapter.listCalls, adapter.getCalls)
	}
}
