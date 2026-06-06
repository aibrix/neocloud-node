# AIBrix GPU Burst Node

AIBrix GPU Burst Node exposes elastic cloud GPU capacity through Kubernetes virtual nodes.

The project targets Kubernetes-native AI workloads that need burst GPU capacity from specialized cloud providers such as RunPod and Lambda Cloud. The first implementation layer focuses on durable workload binding, provider-neutral transformation, fake-provider conformance, and idempotent lifecycle reconciliation.

Current Phase 0 status:

- Provider-neutral core types.
- In-memory binding store for lifecycle tests.
- Minimal transformer for one GPU container.
- Fake provider adapter with idempotent create/delete semantics.
- Lifecycle service that keeps local binding state until remote cleanup succeeds.
- Kubernetes `corev1.Pod` adapter in `pkg/kube`.

The production integration will wrap this core with upstream Virtual Kubelet and Kubernetes clients.
