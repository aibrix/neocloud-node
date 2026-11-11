# AIBrix NeoCloud Node Compatibility Matrix

Date:2026-06-05

This matrix documents the Phase 1 Pod transformation contract. NeoCloud Node accepts standard Kubernetes workload objects at the integration layer, but the core transformer supports only the subset that can be mapped honestly to remote cloud GPU providers.

Unsupported fields must fail with a stable `UnsupportedSpecError` code and field path. They must not be silently ignored.

## Supported In Phase 1

| Kubernetes Pod field | Core field | Notes |
| --- | --- | --- |
| `metadata.namespace` | `Binding.Namespace` | Used for binding identity and future scoping. |
| `metadata.name` | `Binding.Name` | Used for observability only. |
| `metadata.uid` | `Binding.WorkloadUID` | Used as the idempotency key source. |
| `metadata.annotations[cloud.aibrix.ai/provider]` | `WorkloadRequest.Provider` | Provider hint, for example `runpod`. |
| `metadata.annotations[cloud.aibrix.ai/gpu-type]` | `WorkloadRequest.GPUType` | Provider-neutral GPU model hint. |
| `metadata.annotations[cloud.aibrix.ai/max-price]` | `WorkloadRequest.MaxPrice` | String for now; later phases can parse and normalize. |
| `metadata.annotations[cloud.aibrix.ai/region]` | `WorkloadRequest.RegionHint` | Region/datacenter hint. |
| `spec.restartPolicy: Never` | `WorkloadRequest.RestartPolicy` | Default when omitted. |
| `spec.restartPolicy: OnFailure` | `WorkloadRequest.RestartPolicy` | Accepted. See [Restart semantics](#restart-semantics). |
| `spec.restartPolicy: Always` | `WorkloadRequest.RestartPolicy` | Accepted for serving Pods, including Deployment-managed Pods. See [Restart semantics](#restart-semantics). |
| `spec.imagePullSecrets[*].name` | `WorkloadRequest.ImagePullSecrets` | Provider adapter decides whether it can map registry auth. |
| `spec.containers[0].image` | `WorkloadRequest.Image` | Required. |
| `spec.containers[0].command` | `WorkloadRequest.Command` | Preserved as-is. |
| `spec.containers[0].args` | `WorkloadRequest.Args` | Preserved as-is. |
| `spec.containers[0].env` | `WorkloadRequest.Env` | Phase 1 supports literal key/value env only in core. |
| `spec.containers[0].ports` | `WorkloadRequest.Ports` | Port metadata is preserved; provider-specific exposure is later. |
| `spec.containers[0].resources.requests/limits[nvidia.com/gpu]` | `WorkloadRequest.GPUCount` | Must be greater than zero. |

## Rejected In Phase 1

| Kubernetes Pod field | Error code | Field path |
| --- | --- | --- |
| more than one main container | `UnsupportedContainerCount` | `spec.containers` |
| `spec.containers[0].image` missing | `MissingImage` | `spec.containers[0].image` |
| GPU request missing or zero | `MissingGPURequest` | `spec.containers[0].resources.limits[nvidia.com/gpu]` |
| `spec.containers[0].env[*].valueFrom` | `UnsupportedEnvValueFrom` | `spec.containers[0].env[i].valueFrom` (first source index) |
| `spec.containers[0].envFrom` | `UnsupportedEnvFrom` | `spec.containers[0].envFrom` |
| `spec.initContainers` | `UnsupportedInitContainers` | `spec.initContainers` |
| `spec.volumes` | `UnsupportedVolumes` | `spec.volumes` |
| `spec.hostNetwork: true` | `UnsupportedHostNetwork` | `spec.hostNetwork` |
| `spec.containers[0].securityContext.privileged: true` | `UnsupportedPrivilegedContainer` | `spec.containers[0].securityContext.privileged` |

## Restart Semantics

NeoCloud Node never restarts a remote container in place, whatever the
`restartPolicy`. When the remote workload exits or fails, the Pod is reported
as `Failed` instead of entering `CrashLoopBackOff`, and its restart count stays
at zero.

- A Deployment's ReplicaSet treats the `Failed` Pod as inactive and creates a
  replacement Pod. The replacement gets a new remote workload, so it pulls the
  image and loads the model again.
- A Job counts the `Failed` Pod against `backoffLimit` and creates a new Pod.
- A bare Pod stays `Failed`. Nothing recreates it.
- A rollout creates a new remote workload for every new Pod. The old remote
  workload is deleted when its Pod is deleted.

ReplicaSets do not delete `Failed` Pods, and the remote workload of a `Failed`
Pod is deleted only together with the Pod. An exited RunPod Pod no longer bills
GPU time but keeps its disk. Delete `Failed` Pods to release that storage until
terminal cleanup is implemented
([#52](https://github.com/aibrix/neocloud-node/issues/52)).

## Non-Goals For Phase 1

- Kubernetes Service / ClusterIP connectivity to remote pods.
- CNI, CSI, PVC, hostPath or projected service account token semantics.
- Multi-container sidecar semantics.
- Privileged or host namespace workloads.
- Provider-specific placement/fallback. That starts after this transformer contract is stable.
