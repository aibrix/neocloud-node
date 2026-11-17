package app

import (
	"context"
	"testing"
	"time"

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
	updates := make(chan *corev1.Node, 1)
	node.NotifyNodeStatus(context.Background(), func(n *corev1.Node) { updates <- n.DeepCopy() })
	assert := func(want string) {
		t.Helper()
		var last *corev1.Node
		select {
		case last = <-updates:
		case <-time.After(time.Second):
			t.Fatal("node status was not published")
		}
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

func TestCredentialNodeRegistrationDoesNotWaitForStatusConsumer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	node := newCredentialNode(&corev1.Node{}, func() bool { return true })
	updates := make(chan *corev1.Node)
	registered := make(chan struct{})
	go func() {
		node.NotifyNodeStatus(ctx, func(n *corev1.Node) {
			select {
			case updates <- n:
			case <-ctx.Done():
			}
		})
		close(registered)
	}()
	// VK starts its consumer only after NotifyNodeStatus returns.
	select {
	case <-registered:
	case <-time.After(time.Second):
		t.Fatal("registration blocked waiting for the status consumer")
	}
	select {
	case <-updates:
	case <-time.After(time.Second):
		t.Fatal("initial node status missing")
	}
}
