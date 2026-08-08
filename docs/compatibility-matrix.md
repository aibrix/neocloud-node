# AIBrix GPU Burst Node Compatibility Matrix

Date:2026-06-05

This matrix documents the Phase 1 Pod transformation contract. GPU Burst Node accepts standard Kubernetes workload objects at the integration layer, but the core transformer supports only the subset that can be mapped honestly to remote cloud GPU providers.

Unsupported fields must fail with a stable `UnsupportedSpecError` code and field path. They must not be silently ignored.

## Supported In Phase 1

| Kubernetes Pod field | Core field | Notes |
| --- | --- | --- |
| `metadata.namespace` | `Binding.Namespace` | Used for binding identity and future scoping. |
| `metadata.name` | `Binding.Name` | Used for observability only. |
| `metadata.uid` | `Binding.WorkloadUID` | Used as the idempotency key source. |
| `metadata.annotations[gpu-burst-node.aibrix.ai/provider]` | `WorkloadRequest.Provider` | Provider hint, for example `runpod`. |
| `metadata.annotations[gpu-burst-node.aibrix.ai/gpu-type]` | `WorkloadRequest.GPUType` | Provider-neutral GPU model hint. |
| `metadata.annotations[gpu-burst-node.aibrix.ai/max-price]` | `WorkloadRequest.MaxPrice` | String for now; later phases can parse and normalize. |
| `metadata.annotations[gpu-burst-node.aibrix.ai/region]` | `WorkloadRequest.RegionHint` | Region/datacenter hint. |
| `spec.restartPolicy: Never` | `WorkloadRequest.RestartPolicy` | Default when omitted. |
| `spec.restartPolicy: OnFailure` | `WorkloadRequest.RestartPolicy` | Supported for batch-style retry semantics. |
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
| `spec.restartPolicy: Always` | `UnsupportedRestartPolicy` | `spec.restartPolicy` |
| `spec.initContainers` | `UnsupportedInitContainers` | `spec.initContainers` |
| `spec.volumes` | `UnsupportedVolumes` | `spec.volumes` |
| `spec.hostNetwork: true` | `UnsupportedHostNetwork` | `spec.hostNetwork` |
| `spec.containers[0].securityContext.privileged: true` | `UnsupportedPrivilegedContainer` | `spec.containers[0].securityContext.privileged` |

## Non-Goals For Phase 1

- Kubernetes Service / ClusterIP connectivity to remote pods.
- CNI, CSI, PVC, hostPath or projected service account token semantics.
- Multi-container sidecar semantics.
- Privileged or host namespace workloads.
- Provider-specific placement/fallback. That starts after this transformer contract is stable.

