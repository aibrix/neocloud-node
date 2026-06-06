# AIBrix GPU Burst Node

AIBrix GPU Burst Node exposes elastic cloud GPU capacity through Kubernetes virtual nodes.

The project targets Kubernetes-native AI workloads that need burst GPU capacity from specialized cloud providers such as RunPod and Lambda Cloud. The first implementation layer focuses on durable workload binding, provider-neutral transformation, fake-provider conformance, idempotent lifecycle reconciliation, and a real upstream Virtual Kubelet provider boundary.

Current Phase 0 status:

- Provider-neutral core types.
- In-memory binding store for lifecycle tests.
- Kubernetes `CloudPodBinding` store backed by the host-cluster API server.
- Minimal transformer for one GPU container.
- Fake provider adapter with idempotent create/delete semantics.
- RunPod REST adapter thin slice.
- Lifecycle service that keeps local binding state until remote cleanup succeeds.
- Kubernetes `corev1.Pod` adapter in `pkg/kube`.
- Upstream Virtual Kubelet `nodeutil.Provider` implementation in `pkg/vk`.
- Runnable `gpu-burst-node` command in `cmd/gpu-burst-node`.
- Base CRD/RBAC/Deployment manifest and fake-provider smoke Pod in `deploy/`.

## Local Verification

```bash
go test -count=1 ./...
go build ./cmd/gpu-burst-node
```

## Kubernetes Smoke Test

The default deployment uses the fake provider so the control-plane path can be tested without cloud credentials:

```bash
kubectl apply -f deploy/gpu-burst-node.yaml
kubectl -n gpu-burst-node-system rollout status deploy/gpu-burst-node
kubectl apply -f deploy/examples/fake-pod.yaml
kubectl get pod gpu-burst-fake-smoke -w
kubectl get cloudpodbindings
```

To switch the deployment to RunPod, set `GPU_BURST_PROVIDER=runpod` and provide `RUNPOD_API_KEY` through the `gpu-burst-node-runpod` Secret referenced by the deployment.
