package app

import (
	"context"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCredentialNodeUpdatesCapacityWithoutFailingNode(t *testing.T) {
	source := newCredentialProvider(ProviderRunPod, func(string) core.ProviderAdapter { return core.NewFakeProvider(ProviderRunPod) })
	base := &corev1.Node{}
	base.Status.Capacity = corev1.ResourceList{corev1.ResourceName(NvidiaGPUResource): resource.MustParse("8")}
	base.Status.Allocatable = base.Status.Capacity.DeepCopy()
	node := newCredentialNode(base, source.available)
	var last *corev1.Node
	node.NotifyNodeStatus(context.Background(), func(n *corev1.Node) { last = n.DeepCopy() })
	assert := func(want string) {
		t.Helper()
		if last.Status.Phase != corev1.NodeRunning {
			t.Fatalf("node phase = %s, want Running", last.Status.Phase)
		}
		q := last.Status.Allocatable[corev1.ResourceName(NvidiaGPUResource)]
		if q.String() != want {
			t.Fatalf("allocatable=%s want %s", q.String(), want)
		}
		for _, c := range last.Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
				t.Fatal("credential loss marked controller unhealthy")
			}
		}
	}
	assert("0")
	source.update("test-key", "")
	node.publish()
	assert("8")
	source.update("", "")
	node.publish()
	assert("0")
	q := base.Status.Capacity[corev1.ResourceName(NvidiaGPUResource)]
	if q.String() != "8" {
		t.Fatal("mutated initial node template")
	}
}
