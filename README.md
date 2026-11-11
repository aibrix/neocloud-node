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
adapters for RunPod, Lambda Cloud, and Vast.ai.

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
- Vast.ai marketplace adapter with offer search, price/reliability constraints,
  create/get/delete, deterministic recovery, and paginated resource listing.
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

Development requires Go 1.26.10 or newer and Kustomize 5.4 or newer. Docker is optional and
is only needed for container builds.

```bash
make verify
make test-race
make docker-build
```

## Install On Kubernetes

Helm is the recommended installation method. The default provider is `fake`,
so no cloud credentials are required for control-plane validation.

```bash
helm upgrade --install neocloud ./charts/neocloud-node \
  --namespace aibrix-system --create-namespace --wait --timeout 3m
kubectl get node neocloud-node
kubectl apply -f config/samples/fake-pod.yaml
kubectl get cloudpodbindings
```

See the [chart guide](charts/neocloud-node/README.md) for provider Secrets,
commit-pinned images, DOKS workload tolerations, CRD upgrades, and migration
from the legacy YAML installation. The chart is currently installed from a
repository checkout. Legacy Kustomize and generated YAML remain available.

Every push to `main` (including a merged PR) runs CI and, after verification,
race tests, and the container build pass, publishes a Linux amd64/arm64 image:
`ghcr.io/aibrix/neocloud-node:<full-40-character-commit-SHA>`.
Use the resulting commit on `main`, which may differ from the PR branch's SHA
after squash or merge. The image reference and digest appear in the CI run's
job summary. Each main commit has its own run, so newer merges do not cancel
earlier publications. PR and manual CI runs do not publish images.
The `latest` tag continues to track formal releases.

For RunPod, set Helm values `provider=runpod` and
`credentials.runpod.secretName` to an existing Secret containing `apiKey`.
Provider credentials are never included in the repository.

For Vast.ai, set `provider=vast` and `credentials.vast.secretName`.
The adapter defaults to 20 GiB
of disk, verified hosts, reliability of at least 0.95, and on-demand pricing.
Pods can narrow placement with `cloud.aibrix.ai/vast-verified`,
`cloud.aibrix.ai/vast-machine-id`, `cloud.aibrix.ai/vast-disk-gb`, and
`cloud.aibrix.ai/vast-offer-id`. The generic `cloud.aibrix.ai/max-price`
annotation is enforced during offer search and again before rental.

## Inference Examples

Use the [vLLM and SGLang examples](examples/inference/README.md) to run a small
Qwen model on one RunPod A40 through an existing Helm installation. The guide
includes version-pinned Pod YAML, HTTP requests, and remote-resource cleanup.

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

## Vast.ai E2E Smoke Tests

Both Vast tests are explicitly gated because they create a paid instance. The
provider smoke exercises the Go adapter directly, including deterministic
create recovery, readiness, paginated listing, deletion, and final lookup:

```bash
export VAST_API_KEY=...
VAST_E2E=1 hack/e2e/vast-provider-smoke.sh
```

The Kubernetes smoke additionally builds and runs the local virtual-kubelet
controller, creates a Pod and `CloudPodBinding`, waits for the remote instance,
deletes the Pod, and verifies that no Vast resource remains by ID or label:

```bash
export VAST_API_KEY=...
VAST_E2E=1 hack/e2e/vast-smoke.sh
```

Defaults use one verified `GTX 1070`, 20 GiB disk, reliability >= 0.95, and a
maximum rental price of $0.10/hour. Override them with `VAST_E2E_GPU_TYPE`,
`VAST_E2E_DISK_GB`, `VAST_E2E_MIN_RELIABILITY`, and `VAST_E2E_MAX_PRICE`.

## Resource Broker Direction

NeoCloud Node is the Kubernetes-facing runtime. The lower-level resource broker
API is planned as a reusable layer for AIBrix batch, serving, and virtual-node
integrations. That broker will own catalog, planning, lease, fallback, and cost
interfaces; NeoCloud Node will consume it rather than embedding long-term
resource strategy directly in the Virtual Kubelet adapter.
