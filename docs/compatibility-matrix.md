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
| `spec.containers[0].env[*].value` | `WorkloadRequest.Env` | Literal values. |
| `spec.containers[0].env[*].valueFrom.secretKeyRef`, `configMapKeyRef` | `WorkloadRequest.EnvRefs` | See [Environment sources](#environment-sources). |
| `spec.containers[0].env[*].valueFrom.fieldRef` | `WorkloadRequest.Env` | `metadata.name`, `metadata.namespace`, `metadata.uid`, `metadata.labels['<key>']`, `metadata.annotations['<key>']`, `spec.nodeName`, `spec.serviceAccountName`. Resolved from the Pod. |
| `spec.containers[0].envFrom` (`secretRef`, `configMapRef`) | `WorkloadRequest.EnvFrom` | See [Environment sources](#environment-sources). |
| `spec.containers[0].ports` | `WorkloadRequest.Ports` | Published by the provider. See [Remote endpoints](#remote-endpoints). |
| `spec.containers[0].resources.requests/limits[nvidia.com/gpu]` | `WorkloadRequest.GPUCount` | Must be greater than zero. |
| `spec.containers[0].readinessProbe` (`httpGet`, `tcpSocket`) | Pod `Ready` condition | See [Readiness](#readiness). |

## Rejected In Phase 1

| Kubernetes Pod field | Error code | Field path |
| --- | --- | --- |
| more than one main container | `UnsupportedContainerCount` | `spec.containers` |
| `spec.containers[0].image` missing | `MissingImage` | `spec.containers[0].image` |
| GPU request missing or zero | `MissingGPURequest` | `spec.containers[0].resources.limits[nvidia.com/gpu]` |
| `env[*].valueFrom.resourceFieldRef`, `fileKeyRef`, or a `fieldRef` on `status.*` | `UnsupportedEnvValueFrom` | `spec.containers[0].env[i].valueFrom` (first source index) |
| `spec.initContainers` | `UnsupportedInitContainers` | `spec.initContainers` |
| `spec.volumes` | `UnsupportedVolumes` | `spec.volumes` |
| `spec.hostNetwork: true` | `UnsupportedHostNetwork` | `spec.hostNetwork` |
| `spec.containers[0].livenessProbe` | `UnsupportedLivenessProbe` | `spec.containers[0].livenessProbe` |
| `spec.containers[0].startupProbe` | `UnsupportedStartupProbe` | `spec.containers[0].startupProbe` |
| `exec` or `grpc` readiness probe | `UnsupportedReadinessProbe` | `spec.containers[0].readinessProbe` |
| readiness probe port that is not a declared container port | `UnsupportedReadinessProbe` | `spec.containers[0].readinessProbe.<handler>.port` |
| `spec.containers[0].securityContext.privileged: true` | `UnsupportedPrivilegedContainer` | `spec.containers[0].securityContext.privileged` |

## Restart Semantics

The controller never recreates a remote workload in place. What happens when
the container exits depends on the provider and on `restartPolicy`.

RunPod restarts an exited container in place and keeps reporting the Pod as
running; it does not report the exit code. The controller notices the restart
from the container uptime, and:

- with `Always`, keeps the Pod `Running` and increments the container's
  `restartCount`, as kubelet would, and records a `ContainerRestarted` event;
- with `Never` or `OnFailure`, stops the remote workload so that the command
  does not run again, and reports the Pod as `Failed` with reason
  `ContainerExited`. Because the exit code is unknown, a Pod whose command
  succeeded is reported as `Failed` too.

RunPod refreshes the uptime about every 30 seconds, so the restart is noticed
up to that long plus one status sync interval after it happens. The command may
run again during that time before it is stopped, so batch commands should
tolerate a partial second run. When a provider cannot report restarts,
an exited container is reported only if the provider reports the workload as
stopped or failed.

When the remote workload itself stops or fails, for example RunPod `EXITED`,
the Pod is reported as `Failed`:

- A Deployment's ReplicaSet treats the `Failed` Pod as inactive and creates a
  replacement Pod. The replacement gets a new remote workload, so it pulls the
  image and loads the model again.
- A Job counts the `Failed` Pod against `backoffLimit` and creates a new Pod.
- A bare Pod stays `Failed`. Nothing recreates it.
- A rollout creates a new remote workload for every new Pod. The old remote
  workload is deleted when its Pod is deleted.

ReplicaSets do not delete `Failed` Pods, and the remote workload of a `Failed`
Pod is deleted only together with the Pod. A stopped RunPod Pod no longer bills
GPU time but keeps its disk. Delete `Failed` Pods to release that storage until
terminal cleanup is implemented
([#52](https://github.com/aibrix/neocloud-node/issues/52)).

## Remote Endpoints

When the provider assigns a public address, the controller records every
published container port in `CloudPodBinding.status.endpoints` and in the Pod
annotation `cloud.aibrix.ai/endpoints`, for example:

```json
[{"containerPort":8000,"protocol":"TCP","host":"203.0.113.10","port":40123}]
```

The first endpoint with an IP address becomes `status.podIP`. It is set once
and not changed afterwards. Providers usually publish a container port on a
different public port, so `podIP:containerPort` is reachable only when the
published port equals the container port. Callers that need the remapped port
read the annotation. Selector-based Services and kube-proxy forward to
`podIP:targetPort` and therefore do not work with remapped ports.

Endpoints are removed from the binding and the annotation once the workload
stops (`Failed`, `Succeeded`, or deleted remotely), even if the provider still
reports the ports. `status.podIP` stays, matching kubelet.

Published ports are reachable from the internet. Protect them in the workload,
for example with an API key, or with provider firewalls where they exist.

| Provider | Endpoints |
| --- | --- |
| RunPod | `publicIp` and `portMappings` of TCP ports. |
| Vast.ai | Not reported yet. |
| Lambda Cloud | Not reported. The VM runner does not publish container ports yet. |

## Readiness

Without a readiness probe, a Pod is `Ready` as soon as the provider reports the
remote workload as running. For serving Pods that is too early: the image is
still being pulled and the model is still loading.

With an `httpGet` or `tcpSocket` readiness probe, the controller runs the probe
itself against the published endpoint of the probe port (see
[Remote endpoints](#remote-endpoints)). The Pod stays `Running` but not `Ready`
until the provider publishes that port and the probe passes. The controller
honors `initialDelaySeconds`, `periodSeconds`, `timeoutSeconds`,
`successThreshold` and `failureThreshold`, like kubelet. The probe travels from
the controller to the provider's public address, so allow for that latency in
`timeoutSeconds`. HTTPS probes do not verify the certificate, as in kubelet.

`exec` and `grpc` readiness probes cannot run from outside the remote host and
are rejected. The probe port must be declared in `ports`, otherwise the provider
does not publish it.

Liveness and startup probes are rejected for all handlers rather than silently
ignored. The controller marks the Pod `Failed` and emits a Warning event naming
the unsupported field before creating any remote resource. This is controller
validation, not admission validation: `kubectl apply` can succeed. Inspect
`kubectl describe pod` and remove the field from the workload template. For a
Deployment, update the template to stop repeated replacement of failed Pods.

## Environment Sources

The controller reads Secrets and ConfigMaps referenced by `env[*].valueFrom`
and `envFrom` from the Pod namespace when it creates the remote workload, and
merges them like kubelet: `envFrom` sources in order, then `env` entries, with
later entries winning. `envFrom` keys that are not valid variable names are
skipped. `optional` references to missing Secrets, ConfigMaps or keys are
skipped.

- **Values are sent to the provider.** The provider receives them as plain
  container environment variables, and anyone with access to the provider
  account can read them, for example in the provider console.
- **Values are not stored in Kubernetes objects.** `CloudPodBinding` keeps only
  the Secret and ConfigMap names and keys.
- **Values are read once.** Changing a Secret later does not update a running
  remote workload, as with kubelet. A replacement Pod reads the new value.
- **A missing required reference fails the Pod** with reason
  `CreateContainerConfigError` and no remote workload is created. Create the
  Secret or ConfigMap before the Pod. A Deployment replaces the failed Pod and
  tries again.

The controller's ClusterRole allows it to read Secrets in all namespaces.

`$(VAR)` references in `env`, `command` and `args` are not expanded.

## Non-Goals For Phase 1

- Kubernetes Service / ClusterIP connectivity to remote pods.
- CNI, CSI, PVC, hostPath or projected service account token semantics.
- Multi-container sidecar semantics.
- Privileged or host namespace workloads.
- Provider-specific placement/fallback. That starts after this transformer contract is stable.
