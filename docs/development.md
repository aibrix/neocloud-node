# Development

This guide covers local checks, container images, and the paid end-to-end
smoke tests. For contribution rules, see [CONTRIBUTING.md](../CONTRIBUTING.md).

## Prerequisites

- Go 1.26.11 or newer
- Kustomize 5.4 or newer
- Docker, only for container builds
- `kubectl` and a cluster, only for the smoke tests

## Local Checks

```bash
make verify      # gofmt, go mod tidy/verify, go vet, manifest check, unit tests, build
make test-race   # unit tests with the race detector
```

CI runs both. Other useful targets:

| Target | Purpose |
| --- | --- |
| `make vulncheck` | Strict local `govulncheck` scan. In CI the scan is advisory; see [CONTRIBUTING.md](../CONTRIBUTING.md). |
| `make docker-build` | Build the controller image. |
| `make manifests` | Regenerate `deploy/neocloud-node.yaml` from `config/`. Commit the result whenever a Kustomize base changes. |
| `make help` | List all targets. |

## Container Images

| Tag | Published when |
| --- | --- |
| `ghcr.io/aibrix/neocloud-node:<version>` and `latest` | A version tag is released. |
| `ghcr.io/aibrix/neocloud-node:<commit-sha>` | A commit lands on `main` and CI passes verification, race tests, and the container build. |

Commit images are Linux amd64/arm64 and use the full 40-character SHA of the
commit on `main`. After a squash or merge, that SHA differs from the PR
branch's SHA. The image reference and digest appear in the CI run's job
summary. Each `main` commit has its own run, so newer merges do not cancel
earlier publications. PR and manual CI runs do not publish images.

To run a commit image, set the chart value `image.tag` to the SHA.

## Paid E2E Smoke Tests

These tests create real, billed GPU resources. Each one is gated behind an
explicit environment variable so it never runs by accident, and each one
checks the provider for leftovers when it finishes. Run them only with an
account you control and a plan to clean up by hand if a test fails halfway.

### RunPod

The script builds the local binary, runs the controller against the current
Kubernetes context, creates one A40 RunPod Pod, verifies the Kubernetes `Pod`
and `CloudPodBinding` state, deletes the Pod, and checks RunPod for leftovers
by external name.

```bash
export RUNPOD_API_KEY=...
RUNPOD_E2E=1 hack/e2e/runpod-smoke.sh
```

Defaults:

- GPU type: `NVIDIA A40`
- GPU count: `1`
- Virtual node: `neocloud-node-runpod-e2e`
- Binary: `/tmp/neocloud-node-e2e`

Optional capacity-classification check:

```bash
RUNPOD_E2E=1 RUNPOD_E2E_CAPACITY_CHECK=1 hack/e2e/runpod-smoke.sh
```

The capacity check requests `NVIDIA A40` with `COMMUNITY` and spot. If capacity
is unavailable, the binding condition must be `Capacity`. If capacity is
available, the script deletes the created Pod.

### Lambda Cloud

The script prefers a single A10 instance, falls back to a single A100 only
when A10 capacity is unavailable, and refuses other instance types.

```bash
export LAMBDA_E2E=1
export LAMBDA_API_KEY=...
export LAMBDA_E2E_SSH_KEY_NAMES=public-key-dev
hack/e2e/lambda-smoke.sh
```

See the [Lambda adapter guide](lambda-adapter.md#gated-e2e-smoke) for details.

### Vast.ai

The provider smoke exercises the Go adapter directly: deterministic create
recovery, readiness, paginated listing, deletion, and final lookup.

```bash
export VAST_API_KEY=...
VAST_E2E=1 hack/e2e/vast-provider-smoke.sh
```

The Kubernetes smoke also builds and runs the local controller, creates a Pod
and `CloudPodBinding`, waits for the remote instance, deletes the Pod, and
verifies that no Vast resource remains by ID or label.

```bash
export VAST_API_KEY=...
VAST_E2E=1 hack/e2e/vast-smoke.sh
```

Both default to one verified `GTX 1070`, 20 GiB disk, reliability of at least
0.95, and a maximum price of $0.10/hour. Override them with
`VAST_E2E_GPU_TYPE`, `VAST_E2E_DISK_GB`, `VAST_E2E_MIN_RELIABILITY`, and
`VAST_E2E_MAX_PRICE`.
