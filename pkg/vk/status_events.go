package vk

import (
	"fmt"

	"github.com/aibrix/neocloud-node/pkg/core"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

type statusEvent struct {
	uid     types.UID
	reason  string
	class   core.ErrorClass
	missing bool
}

func describeCreateCandidate(candidate core.CreateCandidate) string {
	provider := candidate.Provider
	if provider == "" {
		provider = "default"
	}
	if candidate.Request.RegionHint != "" {
		return fmt.Sprintf("provider %s in %s", provider, candidate.Request.RegionHint)
	}
	return "provider " + provider
}

func (p *Provider) recordStatusSyncFailure(pod *corev1.Pod, binding core.Binding, err error) {
	class := core.ClassifyError(err)
	provider := binding.Placement.Provider
	if provider == "" {
		provider = binding.Request.Provider
	}
	message := fmt.Sprintf("Could not refresh remote workload status (%s)", class)
	if provider != "" {
		message = fmt.Sprintf("Could not refresh status from provider %s (%s)", provider, class)
	}
	p.recordStatusEvent(pod, statusEvent{reason: "ProviderStatusSyncFailed", class: class}, message)
}

func (p *Provider) recordStatusSyncResult(pod *corev1.Pod, binding core.Binding) {
	if condition := core.FindCondition(binding.Conditions, core.ConditionRemoteDeleted); condition != nil && condition.Status == core.ConditionTrue {
		p.recordStatusEvent(pod, statusEvent{reason: condition.Reason, missing: true}, condition.Message)
		return
	}
	p.recordStatusEvent(pod, statusEvent{}, "")
}

func (p *Provider) recordStatusEvent(pod *corev1.Pod, event statusEvent, message string) {
	if p.recorder == nil || pod.DeletionTimestamp != nil {
		return
	}
	event.uid = pod.UID
	key := podKey(pod.Namespace, pod.Name)
	p.mu.Lock()
	current := p.pods[key]
	previous := p.statusEvents[key]
	if current == nil || current.UID != pod.UID || current.DeletionTimestamp != nil ||
		(previous.uid == pod.UID && (previous.missing || previous == event)) {
		p.mu.Unlock()
		return
	}
	if event.reason == "" {
		delete(p.statusEvents, key)
	} else {
		p.statusEvents[key] = event
	}
	p.mu.Unlock()
	if event.reason != "" {
		p.recordPodEvent(pod, corev1.EventTypeWarning, event.reason, message)
	}
}
