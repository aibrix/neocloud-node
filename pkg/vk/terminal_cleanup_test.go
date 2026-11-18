package vk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"
)

func TestTerminalCleanupStartupWithoutCachedPods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := core.NewMemoryBindingStore()
	adapter := core.NewFakeProvider("runpod")
	pod := gpuPod("finished", "uid-finished")
	pod.Spec.NodeName = "test-node"
	pod.Status.Phase = corev1.PodSucceeded
	service := core.NewLifecycleService(store, adapter)
	binding, err := service.EnsureCreated(ctx, core.Binding{Namespace: pod.Namespace, Name: pod.Name, WorkloadUID: string(pod.UID), Request: core.WorkloadRequest{Provider: "runpod"}})
	if err != nil {
		t.Fatal(err)
	}
	binding.Phase = core.BindingSucceeded
	core.SetCondition(&binding, core.BindingCondition{Type: core.ConditionTerminal, Status: core.ConditionTrue, LastTransitionTime: time.Now().Add(-time.Hour)})
	if _, err := store.Update(ctx, binding); err != nil {
		t.Fatal(err)
	}
	recorder := record.NewFakeRecorder(10)
	client := kubefake.NewSimpleClientset(pod)
	provider, err := NewProvider(Config{Store: store, Adapter: adapter, PodClient: client.CoreV1(), NodeName: "test-node", StatusSyncContext: ctx,
		StatusSyncInterval: 0, TerminalCleanupDelay: time.Minute, EventRecorder: recorder})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, "Normal RemoteCleanedUp") {
			t.Fatalf("event = %s", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("startup did not clean persisted terminal binding with status sync disabled")
	}
	if len(provider.pods) != 0 {
		t.Fatal("test must exercise an empty Pod cache")
	}
	got, err := store.GetByWorkloadUID(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil || !core.RemoteCleanedUp(got) || got.Phase != core.BindingSucceeded {
		t.Fatalf("binding = %+v, error = %v", got, err)
	}
	if _, err := adapter.Get(ctx, binding.RemoteID); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Fatalf("remote still exists: %v", err)
	}
	kept, err := client.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil || kept.Status.Phase != corev1.PodSucceeded {
		t.Fatal("cleanup did not retain terminal Pod")
	}
	provider.cleanupTerminalBindings(ctx)
	select {
	case event := <-recorder.Events:
		t.Fatalf("duplicate event: %s", event)
	default:
	}
}

func TestTerminalCleanupRequiresPodClient(t *testing.T) {
	_, err := NewProvider(Config{
		Store:                core.NewMemoryBindingStore(),
		Adapter:              core.NewFakeProvider("runpod"),
		TerminalCleanupDelay: time.Minute,
	})
	if err == nil {
		t.Fatal("expected cleanup without a Pod client to be rejected")
	}
}

func TestTerminalCleanupSkipsPodReadBeforeDeadline(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	pod := gpuPod("future", "uid-future")
	pod.Spec.NodeName = "test-node"
	client := kubefake.NewSimpleClientset(pod)
	binding := core.Binding{
		Namespace: pod.Namespace, Name: pod.Name, WorkloadUID: string(pod.UID),
		RemoteID: "remote", Phase: core.BindingSucceeded,
		Conditions: []core.BindingCondition{{Type: core.ConditionTerminal, Status: core.ConditionTrue,
			LastTransitionTime: time.Now().Add(time.Hour)}},
	}
	if _, _, err := store.CreateOrGet(ctx, binding); err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(Config{Store: store, Adapter: core.NewFakeProvider("runpod"),
		PodClient: client.CoreV1(), NodeName: "test-node"})
	if err != nil {
		t.Fatal(err)
	}
	provider.lifecycle = core.NewLifecycleService(store, core.NewFakeProvider("runpod"), core.WithTerminalCleanupDelay(time.Minute))
	provider.cleanupTerminalBindings(ctx)
	if actions := client.Actions(); len(actions) != 0 {
		t.Fatalf("Pod API actions before cleanup deadline = %v, want none", actions)
	}
}

func TestTerminalCleanupChecksPodOwnership(t *testing.T) {
	for _, tc := range []struct{ name, node, uid string }{
		{"different node", "another-node", "uid-job"},
		{"replaced Pod", "test-node", "another-uid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := core.NewMemoryBindingStore()
			adapter := core.NewFakeProvider("runpod")
			service := core.NewLifecycleService(store, adapter, core.WithTerminalCleanupDelay(time.Minute))
			binding, err := service.EnsureCreated(ctx, core.Binding{Namespace: "default", Name: "job", WorkloadUID: "uid-job", Request: core.WorkloadRequest{Provider: "runpod"}})
			if err != nil {
				t.Fatal(err)
			}
			binding.Phase = core.BindingFailed
			core.SetCondition(&binding, core.BindingCondition{Type: core.ConditionTerminal, Status: core.ConditionTrue, LastTransitionTime: time.Now().Add(-time.Hour)})
			if _, err := store.Update(ctx, binding); err != nil {
				t.Fatal(err)
			}
			pod := gpuPod("job", types.UID(tc.uid))
			pod.Spec.NodeName = tc.node
			provider, err := NewProvider(Config{Store: store, Adapter: adapter, NodeName: "test-node", PodClient: kubefake.NewSimpleClientset(pod).CoreV1()})
			if err != nil {
				t.Fatal(err)
			}
			provider.lifecycle = service
			provider.cleanupTerminalBindings(ctx)
			if _, err := adapter.Get(ctx, binding.RemoteID); err != nil {
				t.Fatalf("deleted unowned remote: %v", err)
			}
		})
	}
}

func TestLogsUnavailableOnceCleanupStarts(t *testing.T) {
	for _, status := range []core.ConditionStatus{core.ConditionFalse, core.ConditionTrue} {
		t.Run(string(status), func(t *testing.T) {
			ctx := context.Background()
			pod := gpuPod("logs", "uid-logs")
			pod.Spec.NodeName = "test-node"
			store := core.NewMemoryBindingStore()
			_, _, err := store.CreateOrGet(ctx, core.Binding{Namespace: pod.Namespace, Name: pod.Name, WorkloadUID: string(pod.UID), RemoteID: "remote", Phase: core.BindingSucceeded,
				Placement: core.Placement{Provider: "runpod"}, Conditions: []core.BindingCondition{{Type: core.ConditionRemoteCleanedUp, Status: status, Reason: "DeletionRequested"}}})
			if err != nil {
				t.Fatal(err)
			}
			reader := &recordingLogReader{}
			provider, err := NewProvider(Config{Store: store, Adapter: core.NewFakeProvider("runpod"), NodeName: "test-node", PodClient: kubefake.NewSimpleClientset(pod).CoreV1(), LogReaders: map[string]core.ProviderLogReader{"runpod": reader}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = provider.GetContainerLogs(ctx, pod.Namespace, pod.Name, "", api.ContainerLogOpts{})
			if err == nil || !strings.Contains(err.Error(), "cleanup has started") || reader.calls != 0 {
				t.Fatalf("logs: err=%v calls=%d", err, reader.calls)
			}
		})
	}
}

func TestConfirmedCleanupStopsReportedBurnRate(t *testing.T) {
	binding := core.Binding{Cost: core.CostStatus{HourlyUSD: 2.5}, Conditions: []core.BindingCondition{{Type: core.ConditionRemoteCleanedUp, Status: core.ConditionFalse}}}
	for _, tc := range []struct {
		status core.ConditionStatus
		want   float64
	}{
		{core.ConditionFalse, 2.5}, {core.ConditionTrue, 0},
	} {
		binding.Conditions[0].Status = tc.status
		families := bindingMetricFamilies([]core.Binding{binding})
		for _, family := range families {
			if family.GetName() == "gpu_burst_node_current_burn_rate_usd_per_hour" {
				if len(family.Metric) != 1 || family.Metric[0].GetGauge().GetValue() != tc.want {
					t.Fatalf("burn rate = %+v, want %v", family.Metric, tc.want)
				}
			}
		}
		if binding.Cost.HourlyUSD != 2.5 {
			t.Fatal("cleanup erased cost history")
		}
	}
}
