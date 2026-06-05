# RunPod Adapter

日期：2026-06-05

The Phase 2 RunPod adapter implements the core `ProviderAdapter` interface against the RunPod REST Pods API. It is covered by local `httptest` contract tests and does not require a real RunPod API key.

Official API references:

- Create Pod: `POST https://rest.runpod.io/v1/pods`
- Find Pod by ID: `GET https://rest.runpod.io/v1/pods/{podId}`
- Delete Pod: `DELETE https://rest.runpod.io/v1/pods/{podId}`

## Request Mapping

| `core.WorkloadRequest` | RunPod create field | Notes |
| --- | --- | --- |
| idempotency key | `name` | Sanitized as `gpu-burst-node-<namespace>-<uid>`. This is not yet remote create de-duplication. |
| `Image` | `imageName` | Required by transformer. |
| `Command` | `dockerEntrypoint` | Preserved as array. |
| `Args` | `dockerStartCmd` | Preserved as array. |
| `Env` | `env` | Literal env map only. |
| `GPUCount` | `gpuCount` | Required to be greater than zero by transformer. |
| `GPUType` | `gpuTypeIds` | Single-item list when set. |
| `Ports` | `ports` | Converted to RunPod strings such as `9090/tcp`. |
| constant | `computeType: GPU` | Phase 2 only targets GPU pods. |

## Status Mapping

| RunPod `desiredStatus` | `core.RemotePhase` |
| --- | --- |
| `RUNNING` | `RemoteRunning` |
| `EXITED` | `RemoteSucceeded` |
| `TERMINATED` | `RemoteDeleted` |
| unknown / empty | `RemotePending` |

## Delete Semantics

`DELETE /pods/{podId}` treats both `204 No Content` and `404 Not Found` as successful cleanup. This matches the lifecycle invariant that remote not found during deletion means there is no paid resource left to clean up.

## Current Limitations

- No real RunPod integration test yet.
- No remote create de-duplication after ambiguous HTTP failures. The current idempotency key is only encoded into the RunPod name.
- No RunPod catalog lookup for GPU type normalization, pricing or capacity.
- No container registry auth ID mapping from Kubernetes `imagePullSecrets`.
- No logs API integration.
- No support for network volumes or templates.

