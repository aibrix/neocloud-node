# Lambda Cloud Adapter

Date: 2026-06-06

The Lambda Cloud adapter implements the core `ProviderAdapter` against Lambda Cloud's On-Demand Cloud API. It is covered by local `httptest` contract tests and does not require a real Lambda API key.

Official API references:

- Launch instances: `POST https://cloud.lambda.ai/api/v1/instance-operations/launch`
- List running instances: `GET https://cloud.lambda.ai/api/v1/instances`
- Retrieve instance details: `GET https://cloud.lambda.ai/api/v1/instances/{id}`
- Terminate instances: `POST https://cloud.lambda.ai/api/v1/instance-operations/terminate`

## Request Mapping

| `core.WorkloadRequest` / config | Lambda launch field | Notes |
| --- | --- | --- |
| idempotency key | `name`, `hostname`, tags | Sanitized as `gpu-burst-node-<namespace>-<uid>` and truncated for Lambda's name limit. |
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

Delete treats `RemoteNotFound` as successful cleanup.

## Current Limitations

- No real Lambda integration test yet.
- The controller exposes `POST /gpu-burst-node/v1/runner/status` when the kubelet API server is enabled. The shell runner can report terminal exit code through this endpoint, but heartbeat and logs are not implemented yet.
- No automatic SSH key creation.
- No logs collection.
