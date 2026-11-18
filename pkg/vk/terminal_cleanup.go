package vk

import (
	"context"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (p *Provider) runTerminalCleanup(ctx context.Context, interval time.Duration) {
	p.cleanupTerminalBindings(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.cleanupTerminalBindings(ctx)
		}
	}
}

func (p *Provider) cleanupTerminalBindings(ctx context.Context) {
	bindings, err := p.store.List(ctx)
	if err != nil {
		log.Printf("neocloud-node terminal cleanup: list bindings: %v", err)
		return
	}
	for _, binding := range bindings {
		if ctx.Err() != nil {
			return
		}
		if !p.lifecycle.TerminalCleanupDue(binding) {
			continue
		}
		// Terminal Pods are not restored into VK's cache after a restart.
		pod := p.getCachedPod(podKey(binding.Namespace, binding.Name))
		if p.podClient != nil {
			pod, err = p.podClient.Pods(binding.Namespace).Get(ctx, binding.Name, metav1.GetOptions{})
			if err != nil {
				log.Printf("neocloud-node terminal cleanup: read pod %s/%s: %v", binding.Namespace, binding.Name, err)
				continue
			}
		}
		if pod == nil || string(pod.UID) != binding.WorkloadUID || pod.Spec.NodeName != p.nodeName {
			continue
		}
		if _, err := p.lifecycle.SyncStatus(ctx, binding.Namespace, binding.WorkloadUID); err != nil {
			log.Printf("neocloud-node terminal cleanup: binding %s/%s: %v", binding.Namespace, binding.Name, err)
		}
	}
}
