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
| idempotency key | `name` | Sanitized as `neocloud-node-<namespace>-<uid>`. Transient create failures are recovered by listing this external name before retrying. |
| `Image` | `imageName` | Required by transformer. |
| `Command`, `Args` | `dockerEntrypoint`, `dockerStartCmd` | See [Command and args](#command-and-args). |
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

## Command and Args

RunPod rejects `null` for `dockerEntrypoint` and `dockerStartCmd`, and treats an
omitted or empty field as "use the image default". Empty fields are therefore
omitted.

| Pod `command` | Pod `args` | `dockerEntrypoint` | `dockerStartCmd` |
| --- | --- | --- | --- |
| empty | empty | omitted (image ENTRYPOINT) | omitted (image CMD) |
| empty | set | omitted (image ENTRYPOINT) | `args` |
| set | set | `command` | `args` |
| set | empty | `command[0]` | `command[1:]` |

Kubernetes ignores the image CMD when only `command` is set. RunPod v1 cannot
clear CMD, so the command is split across ENTRYPOINT and CMD to replace both.
A single-element `command` without `args` still gets the image CMD appended.

## Status Mapping

| RunPod `desiredStatus` | `core.RemotePhase` | Pod result |
| --- | --- | --- |
| `RUNNING` | `RemoteRunning` | `Running` |
| `EXITED` | `RemoteFailed` | `Failed`, reason `RemoteExited` |
| `TERMINATED` | `RemoteDeleted` | `Succeeded`, reason `RemoteWorkloadDeleted` |
| unknown / empty | `RemotePending` | `Pending` |

RunPod does not report the container exit code. `EXITED` therefore cannot be
told apart from a crash, an out-of-memory kill, or a reclaimed host, so it is
reported as `Failed` with reason `RemoteExited` instead of `Succeeded`. A Job
retries such a Pod according to its `backoffLimit`, including a run that
actually exited with code 0. A Pod reaches `Succeeded` only through a runner
callback that reports exit code 0, which RunPod Pods do not have yet.

The exited RunPod Pod is not deleted automatically. It no longer bills GPU time,
but it keeps its disk until the Kubernetes Pod is deleted.

## Endpoints

`publicIp` and `portMappings` from `GET /pods/{podId}` become
`RemoteResource.Endpoints`. A mapping such as `{"8000": 40123}` means container
port 8000 is reachable at `<publicIp>:40123`. RunPod reports the address only
after the Pod has started, so endpoints usually appear after the binding is
already `Running`. Only TCP ports receive a public mapping; `Ports` are always
requested as TCP.

## Delete Semantics

`DELETE /pods/{podId}` treats both `204 No Content` and `404 Not Found` as successful cleanup. If delete returns a transient/timeout error, the adapter reads `GET /pods/{podId}` once; if the pod is already gone, cleanup is treated as successful. This matches the lifecycle invariant that remote not found during deletion means there is no paid resource left to clean up.

## Error Semantics

RunPod HTTP errors are converted to `core.ProviderError`:

| HTTP/result | Error class | Runtime behavior |
| --- | --- | --- |
| 401/403 | `Auth` | Terminal by default; no cross-provider fallback unless policy explicitly allows. |
| 400/422 without a capacity or quota message | `UnsupportedSpec` | Terminal; RunPod rejected the request itself. |
| 404 | `RemoteNotFound` | Used for remote manually deleted and idempotent cleanup. |
| 409 | `Conflict` | Retryable. |
| 429 | `RateLimited` | Retryable and fallback-eligible. |
| 5xx | `Transient` | Retryable and fallback-eligible. |
| body mentioning capacity/availability | `Capacity` | Fallback-eligible. |
| body mentioning quota/limit | `Quota` | Policy-dependent fallback. |

If `Create` returns a transient error after the request may have reached RunPod, the adapter runs `List(name=<externalName>)` before returning the error. If a matching pod exists, it returns that remote allocation instead of creating a duplicate on the next reconcile.

## Current Limitations

- A gated real RunPod smoke test exists in `hack/e2e/runpod-smoke.sh`; it is not run by default because it creates paid resources.
- RunPod catalog lookup is still limited; GPU type normalization, pricing and capacity signals are mostly provided by configured or observed placement offers.
- Kubernetes `imagePullSecrets` are not automatically converted into RunPod registry auth objects; users must currently reference an existing `containerRegistryAuthId`.
- No logs API integration.
- No billing history polling yet; launch-time and provider response cost fields are preserved for binding status and burn-rate metrics.
