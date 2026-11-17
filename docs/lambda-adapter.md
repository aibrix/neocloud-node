# Lambda Cloud Adapter

Date: 2026-06-06

The Lambda Cloud adapter implements the core `ProviderAdapter` against Lambda Cloud's On-Demand Cloud API. It is covered by local `httptest` contract tests and does not require a real Lambda API key.

Official API references:

- Launch instances: `POST https://cloud.lambda.ai/api/v1/instance-operations/launch`
- List instance types and regional capacity: `GET https://cloud.lambda.ai/api/v1/instance-types`
- List running instances: `GET https://cloud.lambda.ai/api/v1/instances`
- Retrieve instance details: `GET https://cloud.lambda.ai/api/v1/instances/{id}`
- Terminate instances: `POST https://cloud.lambda.ai/api/v1/instance-operations/terminate`

## Request Mapping

| `core.WorkloadRequest` / config | Lambda launch field | Notes |
| --- | --- | --- |
| idempotency key | `name`, `hostname`, tags | Sanitized as `neocloud-node-<namespace>-<uid>` and truncated for Lambda's name limit. |
| `RegionHint` or `--lambda-region` | `region_name` | Required. |
| provider hint `lambda-instance-type` or `--lambda-instance-type` | `instance_type_name` | Required because Lambda instance type is not the same as a generic GPU type. |
| provider hint `lambda-ssh-key-names` or `--lambda-ssh-key-names` | `ssh_key_names` | Required by Lambda launch API. |
| provider hint `lambda-image-id` | `image.id` | Optional base image override. |
| provider hint `lambda-file-system-ids` | `file_system_mounts` | Comma-separated filesystem IDs mapped to deterministic mount paths. |
| provider hint `lambda-firewall-ids` | `firewall_rulesets` | Optional firewall rulesets. |
| `--lambda-callback-url` plus container request | `user_data` | Uses the existing cloud-init runner bootstrap to pull and run the container. |

## Status Mapping

| Lambda instance status | `core.RemotePhase` |
| --- | --- |
| `booting` | `RemotePending` |
| `active` | `RemoteRunning` |
| `unhealthy` | `RemoteFailed` |
| `preempted` | `RemoteFailed` |
| `terminating` | `RemotePending` |
| `terminated` | `RemoteDeleted` |
| unknown / empty | `RemotePending` |

## Error Semantics

Lambda HTTP errors are converted to `core.ProviderError`:

| HTTP/result | Error class |
| --- | --- |
| 401/403 or `invalid-api-key` | `Auth` |
| 404 | `RemoteNotFound` |
| 409 | `Conflict` |
| 429 | `RateLimited` |
| 5xx | `Transient` |
| body mentioning `insufficient-capacity` | `Capacity` |
| body mentioning quota/limit | `Quota` |

Delete treats `RemoteNotFound` as successful cleanup. If terminate returns a transient/timeout error, the adapter reads `GET /instances/{id}` once; if the instance is already gone, cleanup is treated as successful.

## Catalog Semantics

The adapter implements `resource.ProviderOfferSource` by reading `GET /instance-types`. Each region in `regions_with_capacity_available` becomes a normalized on-demand offer with hourly price, GPU count, GPU description and a `lambda-instance-type` provider hint. Empty catalog refreshes are conservative and expose only single-GPU instance types; explicit multi-GPU placement should come through a workload intent or a bootstrap offer so a one-card workload is not accidentally routed to an eight-card VM.

## Container ports

The VM runner publishes declared TCP ports 1:1 on the VM, for example
`8000:8000/tcp`. The binding request retains these ports; the lifecycle derives
endpoints from that request and the public VM address, including after controller
restart or batch status sync. Image entrypoints are replaced only when the Pod
provides `command`; `args` preserve the image entrypoint when command is absent.

Attach a provider firewall ruleset with
`cloud.aibrix.ai/lambda-firewall-ids` that permits the controller and intended
clients to reach the declared ports. Endpoint publication does not open a
provider firewall. Existing HTTP/TCP readiness probes use these endpoints; an
active VM alone does not prove the model server is healthy.

## Container logs

Configure the authenticated runner log upload and kubelet endpoint described in
[container logs](container-logs.md). Both are opt-in. The runner retains bounded
records and replays them after a controller restart; Python 3.10 nanosecond
Docker timestamps are supported. Exec, attach and port-forward remain unsupported.

## GPU image compatibility

Choose an engine image compatible with the VM's driver. For a driver570 data
center GPU, the pinned vLLM0.32.0 and SGLang0.5.23 images include CUDA13 forward
compatibility libraries. A workload can explicitly activate them before starting
the original example command:

```bash
driver=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | head -n1)
case "$driver" in
  570.*) export LD_LIBRARY_PATH=/usr/local/cuda-13.0/compat${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH} ;;
esac
exec python3 -m vllm.entrypoints.openai.api_server ...
```

Use `bash -c` to preserve the image's PATH. Do not force compatibility libraries
on other driver branches without checking NVIDIA's
[forward compatibility matrix](https://docs.nvidia.com/deploy/cuda-compatibility/latest/forward-compatibility.html).
The same setup applies to `python3 -m sglang.launch_server`. Keep the original
example model and engine flags, select the Lambda virtual node/provider, and
attach a suitable firewall ruleset.

## Current Limitations

- Lambda support remains experimental and requires a VM runner for container execution.
- No automatic SSH key creation.
- Credentials and callback URLs are operator-managed; see the log guide for
  network, authentication and bounded-retention requirements.
- PVCs and cluster-internal Pod networking remain unsupported.

## Gated E2E Smoke

`hack/e2e/lambda-smoke.sh` runs a paid lifecycle smoke against Lambda Cloud:

```bash
export LAMBDA_E2E=1
export LAMBDA_API_KEY=...
export LAMBDA_E2E_SSH_KEY_NAMES=public-key-dev
hack/e2e/lambda-smoke.sh
```

The script also accepts `LAMBDA_CLOUD_API_KEY` and `LAMBDA_CLOUD_SSH_KEYS` for compatibility with existing Lambda Cloud environment variable names.

The script queries `GET /instance-types`, chooses `gpu_1x_a10` first and `gpu_1x_a100` only if A10 capacity is unavailable, refuses any other instance type, and sets `--max-gpu-count=1`. It creates one Kubernetes Pod, waits for a `CloudPodBinding` with a Lambda instance ID, deletes the Pod, then verifies no matching Lambda instance remains.
