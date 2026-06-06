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

## RunPod E2E Smoke Test

The real RunPod smoke test is gated so it never creates cloud resources by accident. It builds the local binary, runs the controller against the current Kubernetes context, creates one A40 RunPod pod, verifies Kubernetes `Pod` / `CloudPodBinding` state, deletes the pod, and checks RunPod for leftovers by external name.

```bash
export RUNPOD_API_KEY=...
RUNPOD_E2E=1 hack/e2e/runpod-smoke.sh
```

Defaults:

- GPU type: `NVIDIA A40`
- GPU count: `1`
- virtual node: `aibrix-gpu-burst-runpod-e2e`
- binary: `/tmp/gpu-burst-node-e2e`

Optional capacity-classification check:

```bash
RUNPOD_E2E=1 RUNPOD_E2E_CAPACITY_CHECK=1 hack/e2e/runpod-smoke.sh
```

The capacity check requests `NVIDIA A40` with `COMMUNITY` + spot. If capacity is unavailable, the binding condition must be `Capacity`; if capacity is available, the script deletes the created pod.
