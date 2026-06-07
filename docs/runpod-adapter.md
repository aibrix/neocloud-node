# RunPod Adapter

Date: 2026-06-06

The Phase 2 RunPod adapter implements the core `ProviderAdapter` interface against the RunPod REST Pods API. It is covered by local `httptest` contract tests and does not require a real RunPod API key.

Official API references:

- Create Pod: `POST https://rest.runpod.io/v1/pods`
- List Pods: `GET https://rest.runpod.io/v1/pods`
- Find Pod by ID: `GET https://rest.runpod.io/v1/pods/{podId}`
- Delete Pod: `DELETE https://rest.runpod.io/v1/pods/{podId}`
- Templates, network volumes, registry auth and billing are separate RunPod API surfaces and should stay adapter-owned.

## Request Mapping

| `core.WorkloadRequest` | RunPod create field | Notes |
| --- | --- | --- |
| idempotency key | `name` | Sanitized as `gpu-burst-node-<namespace>-<uid>`. Transient create failures are recovered by listing this external name before retrying. |
| `Image` | `imageName` | Required by transformer. |
| `Command` | `dockerEntrypoint` | Preserved as array. |
| `Args` | `dockerStartCmd` | Preserved as array. |
| `Env` | `env` | Literal env map only. |
| `GPUCount` | `gpuCount` | Required to be greater than zero by transformer. |
| `GPUType` | `gpuTypeIds` | Single-item list when set. |
| `MarketType=spot` | `interruptible` | Creates an interruptible/spot Pod. |
| `Ports` | `ports` | Converted to RunPod strings such as `9090/tcp`. |
| provider hint `runpod-cloud-type` | `cloudType` | `SECURE` or `COMMUNITY`. |
| provider hint `runpod-template-id` | `templateId` | Uses a RunPod template. |
| provider hint `runpod-datacenter-ids` | `dataCenterIds` | Comma-separated annotation becomes ordered array. |
| provider hint `runpod-datacenter-priority` | `dataCenterPriority` | Use `custom` when user order must be preserved. |
| provider hint `runpod-network-volume-id` | `networkVolumeId` | Attaches a network volume. |
| provider hint `runpod-registry-auth-id` | `containerRegistryAuthId` | Uses an existing RunPod registry auth object. |
| provider hint `runpod-volume-mount-path` | `volumeMountPath` | Mount path for pod/network volume. |
| constant | `computeType: GPU` | Phase 2 only targets GPU pods. |

## Status Mapping

| RunPod `desiredStatus` | `core.RemotePhase` |
| --- | --- |
| `RUNNING` | `RemoteRunning` |
| `EXITED` | `RemoteSucceeded` |
| `TERMINATED` | `RemoteDeleted` |
| unknown / empty | `RemotePending` |

## Delete Semantics

`DELETE /pods/{podId}` treats both `204 No Content` and `404 Not Found` as successful cleanup. If delete returns a transient/timeout error, the adapter reads `GET /pods/{podId}` once; if the pod is already gone, cleanup is treated as successful. This matches the lifecycle invariant that remote not found during deletion means there is no paid resource left to clean up.

## Error Semantics

RunPod HTTP errors are converted to `core.ProviderError`:

| HTTP/result | Error class | Runtime behavior |
| --- | --- | --- |
| 401/403 | `Auth` | Terminal by default; no cross-provider fallback unless policy explicitly allows. |
| 404 | `RemoteNotFound` | Used for remote manually deleted and idempotent cleanup. |
| 409 | `Conflict` | Retryable. |
| 429 | `RateLimited` | Retryable and fallback-eligible. |
| 5xx | `Transient` | Retryable and fallback-eligible. |
| body mentioning capacity/availability | `Capacity` | Fallback-eligible. |
| body mentioning quota/limit | `Quota` | Policy-dependent fallback. |

If `Create` returns a transient error after the request may have reached RunPod, the adapter runs `List(name=<externalName>)` before returning the error. If a matching pod exists, it returns that remote allocation instead of creating a duplicate on the next reconcile.

## Current Limitations

- No real RunPod integration test yet.
- No RunPod catalog lookup for GPU type normalization, pricing or capacity.
- No automatic creation of RunPod registry auth from Kubernetes `imagePullSecrets`; users must currently reference an existing `containerRegistryAuthId`.
- No logs API integration.
- No billing history polling yet; current code only preserves fields needed to extract per-pod cost later.
