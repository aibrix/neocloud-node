# AIBrix NeoCloud Node

[![CI](https://github.com/aibrix/neocloud-node/actions/workflows/ci.yml/badge.svg)](https://github.com/aibrix/neocloud-node/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Kubernetes bridge to elastic cloud GPUs.

AIBrix NeoCloud Node exposes burst GPU capacity from specialized cloud
providers through a Kubernetes virtual node. The command source lives at
`cmd/neocloud-node`, and release builds publish the binary as `neocloud-node`.

## Project Coordinates

- Project: `AIBrix NeoCloud Node`
- Repo: `aibrix/neocloud-node`
- Module: `github.com/aibrix/neocloud-node`
- Binary: `neocloud-node`
- Image: `ghcr.io/aibrix/neocloud-node`

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

NeoCloud Node intentionally does not claim full Kubernetes Pod semantics. The
current provider path does not support cluster-internal Service networking,
CNI/CSI, PVCs, hostPath, privileged workloads, sidecars, `kubectl exec`, attach,
or port-forward. Container logs are not wired yet; provider-native console links
or log collection are future work.

Lambda Cloud support is still experimental. The adapter and smoke script exist,
but VM runner heartbeat timeout, logs, and stronger failure recovery need more
work before it should be treated as a production provider.

## Local Verification

Development requires Go 1.25 and Kustomize 5.4 or newer. Docker is optional and
is only needed for container builds.

```bash
make verify
make test-race
make docker-build
```

## Install On Kubernetes

The default deployment uses the fake provider so the control-plane path can be
tested without cloud credentials. The Kustomize bases under `config/` are the
canonical deployment source. `deploy/neocloud-node.yaml` is the generated,
single-file equivalent for users who do not use Kustomize.

```bash
kubectl apply -k config/default
kubectl -n aibrix-system rollout status deploy/aibrix-neocloud-node
kubectl apply -f config/samples/fake-pod.yaml
kubectl get pod neocloud-node-fake-smoke -w
kubectl get cloudpodbindings
```

For a pinned image or environment-specific settings, create a Kustomize overlay
that references `config/default` and patches the Deployment. Run `make manifests`
after changing a base so the generated installation manifest stays current.

To switch the deployment to RunPod, set `GPU_BURST_PROVIDER=runpod` and provide
`RUNPOD_API_KEY` through the `aibrix-neocloud-node-runpod` Secret referenced by
the deployment. Provider credentials are never included in the repository.

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
- virtual node: `neocloud-node-runpod-e2e`
- binary: `/tmp/neocloud-node-e2e`

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

NeoCloud Node is the Kubernetes-facing runtime. The lower-level resource broker
API is planned as a reusable layer for AIBrix batch, serving, and virtual-node
integrations. That broker will own catalog, planning, lease, fallback, and cost
interfaces; NeoCloud Node will consume it rather than embedding long-term
resource strategy directly in the Virtual Kubelet adapter.
