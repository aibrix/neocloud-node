package vk

import (
	"context"
	"strings"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
)

// Check the full transformation/lifecycle boundary: unsupported probes must
// produce actionable status and events without ever allocating a paid resource.
func TestUnsupportedProbesFailBeforeAllocation(t *testing.T) {
	handlers := map[string]corev1.ProbeHandler{
		"http": {HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromInt32(8000)}},
		"tcp":  {TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8000)}},
		"exec": {Exec: &corev1.ExecAction{Command: []string{"true"}}},
		"grpc": {GRPC: &corev1.GRPCAction{Port: 8000}},
	}
	for _, kind := range []string{"liveness", "startup"} {
		for name, handler := range handlers {
			t.Run(kind+"/"+name, func(t *testing.T) {
				ctx := context.Background()
				adapter := core.NewFakeProvider("runpod")
				recorder := record.NewFakeRecorder(10)
				p := newTestProviderWithConfig(t, Config{Adapter: adapter, EventRecorder: recorder})
				pod := gpuPod("unsupported-probe", "probe-test-uid")
				probe := &corev1.Probe{ProbeHandler: handler}
				code := "UnsupportedLivenessProbe"
				if kind == "liveness" {
					pod.Spec.Containers[0].LivenessProbe = probe
				} else {
					pod.Spec.Containers[0].StartupProbe = probe
					code = "UnsupportedStartupProbe"
				}
				field := "spec.containers[0]." + kind + "Probe"
				var notified *corev1.Pod
				p.NotifyPods(ctx, func(pod *corev1.Pod) { notified = pod })
				err := p.CreatePod(ctx, pod)
				if !errdefs.IsInvalidInput(err) || !strings.Contains(err.Error(), code) || !strings.Contains(err.Error(), field) {
					t.Fatalf("CreatePod error = %v, want %s at %s", err, code, field)
				}
				if adapter.CreateCount() != 0 {
					t.Fatal("allocated a remote workload")
				}
				bindings, err := p.store.List(ctx)
				if err != nil || len(bindings) != 0 {
					t.Fatalf("bindings = %v, err = %v", bindings, err)
				}
				cached, err := p.GetPod(ctx, pod.Namespace, pod.Name)
				if err != nil {
					t.Fatal(err)
				}
				if cached.Status.Phase != corev1.PodFailed || notified == nil || notified.Status.Phase != corev1.PodFailed {
					t.Fatal("missing Failed status or notification")
				}
				condition := podReadyCondition(cached.Status.Conditions)
				if !strings.Contains(condition.Message, field) || !strings.Contains(condition.Message, "remove this field") {
					t.Fatalf("unhelpful condition: %+v", condition)
				}
				select {
				case event := <-recorder.Events:
					for _, want := range []string{"Warning", "UnsupportedSpec", code, field, "remove this field"} {
						if !strings.Contains(event, want) {
							t.Fatalf("event %q missing %q", event, want)
						}
					}
				default:
					t.Fatal("missing rejection event")
				}
			})
		}
	}
}
