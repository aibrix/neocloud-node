# AIBrix NeoCloudNode

Kubernetes bridge to elastic cloud GPUs.

AIBrix NeoCloudNode exposes burst GPU capacity from specialized cloud providers
through a Kubernetes virtual node. The code and binary names currently remain
`gpu-burst-node`; the public project name is changing first so the code can be
renamed deliberately in a later cleanup.

## Status

This project is experimental and not production-ready.

The current implementation focuses on a reliable Kubernetes control-plane path:
durable workload bindings, provider-neutral Pod transformation, idempotent
remote lifecycle reconciliation, fake-provider conformance tests, and provider
adapters for RunPod and Lambda Cloud.

Real cloud smoke tests are gated by explicit environment variables because they
create paid GPU resources.

## What Works Today

- A Kubernetes virtual node backed by Virtual Kubelet.
- `CloudPodBinding` state for durable Pod-to-provider lifecycle tracking.
- Provider-neutral transformation for a constrained single-container GPU Pod
  subset.
- Fake provider tests for local control-plane validation.
- RunPod Pod lifecycle adapter with create/get/delete/list-by-external-name.
- Lambda Cloud VM adapter with cloud-init runner callback support.
- Provider error classification, fallback hooks, orphan sweeper mechanics, and
  basic burn-rate metrics.

## Current Boundaries

NeoCloudNode intentionally does not claim full Kubernetes Pod semantics. The current
provider path does not support cluster-internal Service networking, CNI/CSI,
PVCs, hostPath, privileged workloads, sidecars, `kubectl exec`, attach, or
port-forward. Container logs are not wired yet; provider-native console links or
log collection are future work.

Lambda Cloud support is still experimental. The adapter and smoke script exist,
but VM runner heartbeat timeout, logs, and stronger failure recovery need more
work before it should be treated as a production provider.

## Local Verification

```bash
go test -count=1 ./...
go build ./cmd/gpu-burst-node
```

## Kubernetes Smoke Test

The default deployment uses the fake provider so the control-plane path can be
tested without cloud credentials:

```bash
kubectl apply -f deploy/gpu-burst-node.yaml
kubectl -n gpu-burst-node-system rollout status deploy/gpu-burst-node
kubectl apply -f deploy/examples/fake-pod.yaml
kubectl get pod gpu-burst-fake-smoke -w
kubectl get cloudpodbindings
```

To switch the deployment to RunPod, set `GPU_BURST_PROVIDER=runpod` and provide
`RUNPOD_API_KEY` through the `gpu-burst-node-runpod` Secret referenced by the
deployment.

## RunPod E2E Smoke Test

The real RunPod smoke test is gated so it never creates cloud resources by
accident. It builds the local binary, runs the controller against the current
Kubernetes context, creates one A40 RunPod pod, verifies Kubernetes `Pod` /
`CloudPodBinding` state, deletes the pod, and checks RunPod for leftovers by
external name.

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

The capacity check requests `NVIDIA A40` with `COMMUNITY` + spot. If capacity is
unavailable, the binding condition must be `Capacity`; if capacity is available,
the script deletes the created pod.

## Lambda Cloud E2E Smoke Test

The Lambda smoke test is also gated. It prefers a single A10 instance, falls
back to a single A100 only when A10 capacity is unavailable, and refuses other
instance types by default.

```bash
export LAMBDA_E2E=1
export LAMBDA_API_KEY=...
export LAMBDA_E2E_SSH_KEY_NAMES=public-key-dev
hack/e2e/lambda-smoke.sh
```

## Resource Broker Direction

NeoCloudNode is the Kubernetes-facing runtime. The lower-level resource broker API is
planned as a reusable layer for AIBrix batch, serving, and virtual-node
integrations. That broker will own catalog, planning, lease, fallback, and cost
interfaces; NeoCloudNode will consume it rather than embedding long-term resource
strategy directly in the Virtual Kubelet adapter.
