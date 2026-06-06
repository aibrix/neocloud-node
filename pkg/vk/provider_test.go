package vk

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestProviderImplementsVirtualKubeletProvider(t *testing.T) {
	var _ nodeutil.Provider = (*Provider)(nil)
}

func TestCreatePodLaunchesRemoteWorkloadAndCachesRunningStatus(t *testing.T) {
	ctx := context.Background()
	adapter := core.NewFakeProvider("runpod")
	provider := newTestProvider(t, adapter)
	pod := gpuPod("training", "uid-training")

	var notified []*corev1.Pod
	provider.NotifyPods(ctx, func(pod *corev1.Pod) {
		notified = append(notified, pod.DeepCopy())
	})

	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	if got := adapter.CreateCount(); got != 1 {
		t.Fatalf("remote Create count = %d, want 1", got)
	}

	cached, err := provider.GetPod(ctx, "default", "training")
	if err != nil {
		t.Fatalf("GetPod() error = %v", err)
	}
	if cached == pod {
		t.Fatal("GetPod() returned the caller-owned pod pointer")
	}
	assertRunningPod(t, cached)

	status, err := provider.GetPodStatus(ctx, "default", "training")
	if err != nil {
		t.Fatalf("GetPodStatus() error = %v", err)
	}
	if status.Phase != corev1.PodRunning {
		t.Fatalf("GetPodStatus().Phase = %s, want %s", status.Phase, corev1.PodRunning)
	}
	if len(notified) != 1 {
		t.Fatalf("notifier calls = %d, want 1", len(notified))
	}
	assertRunningPod(t, notified[0])
}

func TestCreatePodRejectsUnsupportedSpecBeforeLaunchingRemoteWorkload(t *testing.T) {
	ctx := context.Background()
	adapter := core.NewFakeProvider("runpod")
	provider := newTestProvider(t, adapter)
	pod := gpuPod("service", "uid-service")
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	var notified []*corev1.Pod
	provider.NotifyPods(ctx, func(pod *corev1.Pod) {
		notified = append(notified, pod.DeepCopy())
	})

	err := provider.CreatePod(ctx, pod)
	if err == nil {
		t.Fatal("CreatePod() error = nil, want invalid input")
	}
	if !errdefs.IsInvalidInput(err) {
		t.Fatalf("CreatePod() error = %T %[1]v, want errdefs.InvalidInput", err)
	}
	if got := adapter.CreateCount(); got != 0 {
		t.Fatalf("remote Create count = %d, want 0", got)
	}
	cached, err := provider.GetPod(ctx, "default", "service")
	if err != nil {
		t.Fatalf("GetPod() error = %v, want cached failed pod", err)
	}
	if cached.Status.Phase != corev1.PodFailed {
		t.Fatalf("cached phase = %s, want Failed", cached.Status.Phase)
	}
	if got := podReadyCondition(cached.Status.Conditions).Reason; got != string(core.ErrorClassUnsupportedSpec) {
		t.Fatalf("PodReady reason = %q, want %q", got, core.ErrorClassUnsupportedSpec)
	}
	if got := cached.Status.ContainerStatuses[0].State.Terminated.Reason; got != string(core.ErrorClassUnsupportedSpec) {
		t.Fatalf("terminated reason = %q, want %q", got, core.ErrorClassUnsupportedSpec)
	}
	if len(notified) != 1 || notified[0].Status.Phase != corev1.PodFailed {
		t.Fatalf("notified = %#v, want one failed pod notification", notified)
	}
}

func TestCreatePodCachesProviderFailureForDeletion(t *testing.T) {
	ctx := context.Background()
	provider := newTestProvider(t, failingProvider{
		err: &core.ProviderError{
			Provider:  "runpod",
			Operation: "post",
			Class:     core.ErrorClassCapacity,
			Message:   "no A40 capacity",
		},
	})
	pod := gpuPod("capacity-failed", "uid-capacity-failed")

	err := provider.CreatePod(ctx, pod)
	if err == nil {
		t.Fatal("CreatePod() error = nil, want provider failure")
	}

	cached, err := provider.GetPod(ctx, "default", "capacity-failed")
	if err != nil {
		t.Fatalf("GetPod() error = %v, want cached failed pod", err)
	}
	if cached.Status.Phase != corev1.PodFailed {
		t.Fatalf("cached phase = %s, want Failed", cached.Status.Phase)
	}
	if got := podReadyCondition(cached.Status.Conditions).Reason; got != string(core.ErrorClassCapacity) {
		t.Fatalf("PodReady reason = %q, want %q", got, core.ErrorClassCapacity)
	}

	if err := provider.DeletePod(ctx, pod); err != nil {
		t.Fatalf("DeletePod() error = %v", err)
	}
	if _, err := provider.GetPod(ctx, "default", "capacity-failed"); !errdefs.IsNotFound(err) {
		t.Fatalf("GetPod() after delete error = %T %[1]v, want not found", err)
	}
}

func TestDeletePodIsIdempotentAndRemovesCachedPod(t *testing.T) {
	ctx := context.Background()
	provider := newTestProvider(t, core.NewFakeProvider("runpod"))
	pod := gpuPod("batch", "uid-batch")

	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	if err := provider.DeletePod(ctx, pod); err != nil {
		t.Fatalf("DeletePod() first call error = %v", err)
	}
	if err := provider.DeletePod(ctx, pod); err != nil {
		t.Fatalf("DeletePod() second call error = %v", err)
	}

	_, err := provider.GetPod(ctx, "default", "batch")
	if err == nil {
		t.Fatal("GetPod() error = nil after delete, want not found")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("GetPod() error = %T %[1]v, want errdefs.NotFound", err)
	}
}

func TestGetPodsReturnsDeepCopies(t *testing.T) {
	ctx := context.Background()
	provider := newTestProvider(t, core.NewFakeProvider("runpod"))
	pod := gpuPod("copy-check", "uid-copy-check")

	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	pods, err := provider.GetPods(ctx)
	if err != nil {
		t.Fatalf("GetPods() error = %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("len(GetPods()) = %d, want 1", len(pods))
	}

	pods[0].Status.Phase = corev1.PodFailed
	pods[0].Annotations["changed"] = "true"

	cached, err := provider.GetPod(ctx, "default", "copy-check")
	if err != nil {
		t.Fatalf("GetPod() error = %v", err)
	}
	if cached.Status.Phase != corev1.PodRunning {
		t.Fatalf("cached phase = %s, want %s", cached.Status.Phase, corev1.PodRunning)
	}
	if _, ok := cached.Annotations["changed"]; ok {
		t.Fatal("mutating GetPods() result changed provider cache")
	}
}

func TestUnsupportedStreamingMethodsReturnInvalidInput(t *testing.T) {
	ctx := context.Background()
	provider := newTestProvider(t, core.NewFakeProvider("runpod"))

	logs, err := provider.GetContainerLogs(ctx, "default", "pod", "main", api.ContainerLogOpts{})
	if logs != nil {
		_ = logs.Close()
		t.Fatal("GetContainerLogs() returned a stream, want nil")
	}
	assertInvalidInput(t, "GetContainerLogs", err)
	assertInvalidInput(t, "RunInContainer", provider.RunInContainer(ctx, "default", "pod", "main", []string{"nvidia-smi"}, nil))
	assertInvalidInput(t, "AttachToContainer", provider.AttachToContainer(ctx, "default", "pod", "main", nil))
	assertInvalidInput(t, "PortForward", provider.PortForward(ctx, "default", "pod", 8000, nopReadWriteCloser{}))
}

func TestStatsSummaryIncludesKnownPods(t *testing.T) {
	ctx := context.Background()
	provider := newTestProvider(t, core.NewFakeProvider("runpod"))
	pod := gpuPod("stats", "uid-stats")

	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	summary, err := provider.GetStatsSummary(ctx)
	if err != nil {
		t.Fatalf("GetStatsSummary() error = %v", err)
	}
	if summary == nil {
		t.Fatal("GetStatsSummary() = nil, want summary")
	}
	if len(summary.Pods) != 1 {
		t.Fatalf("len(summary.Pods) = %d, want 1", len(summary.Pods))
	}
	if summary.Pods[0].PodRef.Namespace != "default" || summary.Pods[0].PodRef.Name != "stats" || summary.Pods[0].PodRef.UID != "uid-stats" {
		t.Fatalf("summary pod ref = %#v, want default/stats uid-stats", summary.Pods[0].PodRef)
	}

	metrics, err := provider.GetMetricsResource(ctx)
	if err != nil {
		t.Fatalf("GetMetricsResource() error = %v", err)
	}
	if metrics == nil {
		t.Fatal("GetMetricsResource() = nil, want empty slice")
	}
}

func TestGetPodStatusSyncsRemoteProviderPhase(t *testing.T) {
	ctx := context.Background()
	adapter := &phaseProvider{phase: core.RemoteRunning}
	provider := newTestProvider(t, adapter)
	pod := gpuPod("sync", "uid-sync")

	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	adapter.phase = core.RemoteSucceeded

	status, err := provider.GetPodStatus(ctx, "default", "sync")
	if err != nil {
		t.Fatalf("GetPodStatus() error = %v", err)
	}
	if status.Phase != corev1.PodSucceeded {
		t.Fatalf("GetPodStatus().Phase = %s, want %s", status.Phase, corev1.PodSucceeded)
	}

	cached, err := provider.GetPod(ctx, "default", "sync")
	if err != nil {
		t.Fatalf("GetPod() error = %v", err)
	}
	if cached.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("cached phase = %s, want %s", cached.Status.Phase, corev1.PodSucceeded)
	}
}

func TestGetPodStatusSurfacesRemoteDeletedCondition(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	adapter := core.NewFakeProvider("runpod")
	provider := newTestProvider(t, adapter)
	pod := gpuPod("remote-deleted", "uid-remote-deleted")

	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	binding, err := provider.lifecycle.SyncStatus(ctx, "default", "uid-remote-deleted")
	if err != nil {
		t.Fatalf("SyncStatus() setup error = %v", err)
	}
	if err := adapter.Delete(ctx, binding.RemoteID); err != nil {
		t.Fatalf("fake Delete() setup error = %v", err)
	}

	status, err := provider.GetPodStatus(ctx, "default", "remote-deleted")
	if err != nil {
		t.Fatalf("GetPodStatus() error = %v", err)
	}
	if status.Phase != corev1.PodFailed {
		t.Fatalf("phase = %s, want Failed", status.Phase)
	}
	if len(status.ContainerStatuses) != 1 || status.ContainerStatuses[0].State.Terminated == nil {
		t.Fatalf("container statuses = %#v, want terminated", status.ContainerStatuses)
	}
	if got := status.ContainerStatuses[0].State.Terminated.Reason; got != string(core.ErrorClassRemoteNotFound) {
		t.Fatalf("terminated reason = %q, want %q", got, core.ErrorClassRemoteNotFound)
	}
	if got := podReadyCondition(status.Conditions).Message; got == "" {
		t.Fatal("PodReady message is empty, want provider error message")
	}
}

func TestNewProviderFuncBuildsNodeutilProvider(t *testing.T) {
	ctx := context.Background()
	factory := NewProviderFunc(Config{
		Store:   core.NewMemoryBindingStore(),
		Adapter: core.NewFakeProvider("runpod"),
	})
	provider, nodeProvider, err := factory(nodeutil.ProviderConfig{
		Node: &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "aibrix-gpu-burst"},
		},
	})
	if err != nil {
		t.Fatalf("factory() error = %v", err)
	}
	if provider == nil {
		t.Fatal("factory() provider = nil, want provider")
	}
	if nodeProvider != nil {
		t.Fatalf("factory() node provider = %#v, want nil so nodeutil can use its default", nodeProvider)
	}

	if err := provider.CreatePod(ctx, gpuPod("factory", "uid-factory")); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	summary, err := provider.GetStatsSummary(ctx)
	if err != nil {
		t.Fatalf("GetStatsSummary() error = %v", err)
	}
	if summary.Node.NodeName != "aibrix-gpu-burst" {
		t.Fatalf("summary node name = %s, want aibrix-gpu-burst", summary.Node.NodeName)
	}
}

func newTestProvider(t *testing.T, adapter core.ProviderAdapter) *Provider {
	t.Helper()

	provider, err := NewProvider(Config{
		Store:   core.NewMemoryBindingStore(),
		Adapter: adapter,
	})
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	return provider
}

func gpuPod(name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "default",
			Name:              name,
			UID:               uid,
			CreationTimestamp: metav1.Unix(1700000000, 0),
			Annotations: map[string]string{
				core.AnnotationProvider: "runpod",
				core.AnnotationGPUType:  "NVIDIA A100",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "ghcr.io/example/train:latest",
					Command: []string{"python"},
					Args:    []string{"train.py"},
					Env: []corev1.EnvVar{
						{Name: "MODEL", Value: "llama"},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
						},
					},
				},
			},
		},
	}
}

func assertRunningPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()

	if pod.Status.Phase != corev1.PodRunning {
		t.Fatalf("pod phase = %s, want %s", pod.Status.Phase, corev1.PodRunning)
	}
	if len(pod.Status.ContainerStatuses) != 1 {
		t.Fatalf("container status count = %d, want 1", len(pod.Status.ContainerStatuses))
	}
	status := pod.Status.ContainerStatuses[0]
	if status.Name != "main" {
		t.Fatalf("container status name = %s, want main", status.Name)
	}
	if !status.Ready {
		t.Fatal("container status Ready = false, want true")
	}
	if status.State.Running == nil {
		t.Fatalf("container state = %#v, want running", status.State)
	}
}

func podReadyCondition(conditions []corev1.PodCondition) corev1.PodCondition {
	for _, condition := range conditions {
		if condition.Type == corev1.PodReady {
			return condition
		}
	}
	return corev1.PodCondition{}
}

func assertInvalidInput(t *testing.T, method string, err error) {
	t.Helper()

	if err == nil {
		t.Fatalf("%s() error = nil, want invalid input", method)
	}
	if !errdefs.IsInvalidInput(err) {
		t.Fatalf("%s() error = %T %[2]v, want errdefs.InvalidInput", method, err)
	}
}

type nopReadWriteCloser struct{}

func (nopReadWriteCloser) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (nopReadWriteCloser) Write(p []byte) (int, error) {
	return len(p), nil
}

func (nopReadWriteCloser) Close() error {
	return nil
}

type phaseProvider struct {
	phase core.RemotePhase
}

type failingProvider struct {
	err error
}

func (p failingProvider) Create(context.Context, string, core.WorkloadRequest) (core.RemoteResource, error) {
	return core.RemoteResource{}, p.err
}

func (p failingProvider) Get(context.Context, string) (core.RemoteResource, error) {
	return core.RemoteResource{}, errors.New("unexpected get")
}

func (p failingProvider) Delete(context.Context, string) error {
	return nil
}

func (p *phaseProvider) Create(ctx context.Context, idempotencyKey string, request core.WorkloadRequest) (core.RemoteResource, error) {
	return core.RemoteResource{
		ID:             "remote-sync",
		IdempotencyKey: idempotencyKey,
		Provider:       "runpod",
		Phase:          p.phase,
		Request:        request,
	}, nil
}

func (p *phaseProvider) Get(ctx context.Context, remoteID string) (core.RemoteResource, error) {
	return core.RemoteResource{
		ID:       remoteID,
		Provider: "runpod",
		Phase:    p.phase,
	}, nil
}

func (p *phaseProvider) Delete(ctx context.Context, remoteID string) error {
	return nil
}
