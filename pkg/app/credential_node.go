package app

import (
	"context"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// credentialNode owns status publication so the polling loop does not race VK
// with a second writer patching nodes/status directly.
type credentialNode struct {
	mu               sync.Mutex
	base             *corev1.Node
	available        func() bool
	notify           func(*corev1.Node)
	lastAvailability corev1.ConditionStatus
	transition       metav1.Time
}

func newCredentialNode(base *corev1.Node, available func() bool) *credentialNode {
	return &credentialNode{base: base.DeepCopy(), available: available}
}

func (*credentialNode) Ping(ctx context.Context) error { return ctx.Err() }

func (n *credentialNode) NotifyNodeStatus(_ context.Context, notify func(*corev1.Node)) {
	n.mu.Lock()
	n.notify = notify
	n.mu.Unlock()
	n.publish()
}

func (n *credentialNode) publish() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.notify == nil {
		return
	}
	out := n.base.DeepCopy()
	out.Status.Phase = corev1.NodeRunning
	now := metav1.Now()
	status := corev1.ConditionTrue
	reason, message := "CredentialsConfigured", "provider credentials are configured"
	if !n.available() {
		status = corev1.ConditionFalse
		reason, message = "CredentialsUnavailable", "provider credentials are unavailable; check the configured Secret and provider authentication"
		if out.Status.Capacity == nil {
			out.Status.Capacity = corev1.ResourceList{}
		}
		if out.Status.Allocatable == nil {
			out.Status.Allocatable = corev1.ResourceList{}
		}
		out.Status.Capacity[corev1.ResourceName(NvidiaGPUResource)] = resource.MustParse("0")
		out.Status.Allocatable[corev1.ResourceName(NvidiaGPUResource)] = resource.MustParse("0")
	}
	if n.lastAvailability != status {
		n.transition = now
		n.lastAvailability = status
	}
	conditions := out.Status.Conditions[:0]
	for _, c := range out.Status.Conditions {
		if c.Type != corev1.NodeReady && c.Type != "ProviderCredentialsReady" {
			conditions = append(conditions, c)
		}
	}
	out.Status.Conditions = append(conditions,
		corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady", Message: "virtual kubelet is running", LastHeartbeatTime: now, LastTransitionTime: now},
		corev1.NodeCondition{Type: "ProviderCredentialsReady", Status: status, Reason: reason, Message: message, LastHeartbeatTime: now, LastTransitionTime: n.transition},
	)
	n.notify(out)
}
