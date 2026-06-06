package kubernetes

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestCreateOrGetCreatesCloudPodBinding(t *testing.T) {
	ctx := context.Background()
	store := newTestBindingStore()
	binding := testBinding()

	got, created, err := store.CreateOrGet(ctx, binding)
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}
	if !created {
		t.Fatal("CreateOrGet() created = false, want true")
	}
	if got.Phase != core.BindingPending {
		t.Fatalf("phase = %s, want %s", got.Phase, core.BindingPending)
	}

	object, err := store.client.Resource(CloudPodBindingsGVR).Namespace(binding.Namespace).Get(ctx, BindingName(binding.WorkloadUID), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("created object Get() error = %v", err)
	}
	if object.GetAPIVersion() != CloudPodBindingAPIVersion {
		t.Fatalf("apiVersion = %s, want %s", object.GetAPIVersion(), CloudPodBindingAPIVersion)
	}
	if object.GetKind() != CloudPodBindingKind {
		t.Fatalf("kind = %s, want %s", object.GetKind(), CloudPodBindingKind)
	}
	if object.GetLabels()[LabelPodUID] != binding.WorkloadUID {
		t.Fatalf("pod uid label = %q, want %q", object.GetLabels()[LabelPodUID], binding.WorkloadUID)
	}
	if !hasString(object.GetFinalizers(), CloudPodBindingFinalizer) {
		t.Fatalf("finalizers = %#v, want %s", object.GetFinalizers(), CloudPodBindingFinalizer)
	}

	roundTrip, err := store.GetByWorkloadUID(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if roundTrip.Request.Image != binding.Request.Image || roundTrip.Request.GPUCount != binding.Request.GPUCount {
		t.Fatalf("round trip request = %#v, want %#v", roundTrip.Request, binding.Request)
	}
}

func TestCreateOrGetReturnsExistingBindingWithoutOverwritingRemoteState(t *testing.T) {
	ctx := context.Background()
	store := newTestBindingStore()
	binding := testBinding()

	if _, _, err := store.CreateOrGet(ctx, binding); err != nil {
		t.Fatalf("CreateOrGet() setup error = %v", err)
	}
	binding.RemoteID = "remote-1"
	binding.Phase = core.BindingRunning
	binding.Placement.Provider = "runpod"
	if err := store.Update(ctx, binding); err != nil {
		t.Fatalf("Update() setup error = %v", err)
	}

	desired := testBinding()
	got, created, err := store.CreateOrGet(ctx, desired)
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}
	if created {
		t.Fatal("CreateOrGet() created = true, want false")
	}
	if got.RemoteID != "remote-1" || got.Phase != core.BindingRunning || got.Placement.Provider != "runpod" {
		t.Fatalf("existing binding = %#v, want persisted remote state", got)
	}
}

func TestUpdatePersistsRemoteStatus(t *testing.T) {
	ctx := context.Background()
	store := newTestBindingStore()
	binding := testBinding()
	if _, _, err := store.CreateOrGet(ctx, binding); err != nil {
		t.Fatalf("CreateOrGet() setup error = %v", err)
	}

	binding.RemoteID = "rp-123"
	binding.Phase = core.BindingRunning
	binding.LastError = ""
	binding.Attempts = []core.Attempt{
		{
			Provider:        "runpod",
			Region:          "US-TX-3",
			Class:           core.ErrorClassCapacity,
			Reason:          "Selected",
			Message:         "first offer",
			Retryable:       false,
			FallbackAllowed: true,
		},
	}
	binding.Conditions = []core.BindingCondition{
		{
			Type:    core.ConditionCapacity,
			Status:  core.ConditionTrue,
			Reason:  string(core.ErrorClassCapacity),
			Message: "runpod had no capacity in US-TX-3",
		},
	}
	if err := store.Update(ctx, binding); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	got, err := store.GetByWorkloadUID(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if got.RemoteID != "rp-123" || got.Phase != core.BindingRunning {
		t.Fatalf("binding = %#v, want remote running state", got)
	}
	if len(got.Attempts) != 1 || got.Attempts[0].Region != "US-TX-3" {
		t.Fatalf("attempts = %#v, want one RunPod attempt", got.Attempts)
	}
	if got.Attempts[0].Class != core.ErrorClassCapacity || !got.Attempts[0].FallbackAllowed {
		t.Fatalf("attempt = %#v, want capacity fallback attempt", got.Attempts[0])
	}
	condition := core.FindCondition(got.Conditions, core.ConditionCapacity)
	if condition == nil || condition.Status != core.ConditionTrue {
		t.Fatalf("conditions = %#v, want capacity condition", got.Conditions)
	}
}

func TestListReturnsBindingsAcrossNamespaces(t *testing.T) {
	ctx := context.Background()
	store := newTestBindingStore()

	first := testBinding()
	second := testBinding()
	second.Namespace = "other"
	second.Name = "train-2"
	second.WorkloadUID = "22222222-2222-3333-4444-555555555555"
	if _, _, err := store.CreateOrGet(ctx, first); err != nil {
		t.Fatalf("CreateOrGet() first error = %v", err)
	}
	if _, _, err := store.CreateOrGet(ctx, second); err != nil {
		t.Fatalf("CreateOrGet() second error = %v", err)
	}

	bindings, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("len(List()) = %d, want 2", len(bindings))
	}
}

func TestMarkDeletingAndCompleteDelete(t *testing.T) {
	ctx := context.Background()
	store := newTestBindingStore()
	binding := testBinding()
	if _, _, err := store.CreateOrGet(ctx, binding); err != nil {
		t.Fatalf("CreateOrGet() setup error = %v", err)
	}

	deleting, err := store.MarkDeleting(ctx, binding.Namespace, binding.WorkloadUID)
	if err != nil {
		t.Fatalf("MarkDeleting() error = %v", err)
	}
	if !deleting.Deleting || deleting.Phase != core.BindingDeleting {
		t.Fatalf("deleting binding = %#v, want deleting phase", deleting)
	}

	if err := store.CompleteDelete(ctx, binding.Namespace, binding.WorkloadUID); err != nil {
		t.Fatalf("CompleteDelete() error = %v", err)
	}
	_, err = store.GetByWorkloadUID(ctx, binding.Namespace, binding.WorkloadUID)
	if !errors.Is(err, core.ErrBindingNotFound) {
		t.Fatalf("GetByWorkloadUID() error = %v, want ErrBindingNotFound", err)
	}
}

func TestGetByWorkloadUIDReturnsBindingNotFound(t *testing.T) {
	ctx := context.Background()
	store := newTestBindingStore()

	_, err := store.GetByWorkloadUID(ctx, "default", "missing")
	if !errors.Is(err, core.ErrBindingNotFound) {
		t.Fatalf("GetByWorkloadUID() error = %v, want ErrBindingNotFound", err)
	}
}

func testBinding() core.Binding {
	return core.Binding{
		Namespace:   "default",
		Name:        "train",
		WorkloadUID: "11111111-2222-3333-4444-555555555555",
		Phase:       core.BindingPending,
		Request: core.WorkloadRequest{
			Image:         "ghcr.io/example/train:latest",
			Command:       []string{"python"},
			Args:          []string{"train.py"},
			Env:           map[string]string{"MODEL": "llama"},
			GPUCount:      1,
			GPUType:       "NVIDIA A100",
			GPUMemory:     "80Gi",
			MarketType:    core.MarketTypeSpot,
			Provider:      "runpod",
			RegionHint:    "US-TX-3",
			RestartPolicy: string(corev1.RestartPolicyNever),
			Ports: []core.Port{
				{Name: "http", ContainerPort: 8080, Protocol: "TCP"},
			},
			ProviderHints: map[string]string{
				core.AnnotationRunPodTemplateID: "template-1",
			},
		},
	}
}

func newTestBindingStore() *BindingStore {
	return NewBindingStore(fake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			CloudPodBindingsGVR: CloudPodBindingKind + "List",
		},
	))
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
