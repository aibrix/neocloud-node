# Kubernetes Pod Adapter

Date：2026-06-05

NeoCloud Node now has a Kubernetes adapter layer in `pkg/kube`.

The core package stays provider-neutral and testable. The `pkg/kube.Transformer` accepts real Kubernetes `corev1.Pod` objects and converts them into `core.Binding`, then reuses `core.DefaultTransformer` for compatibility validation.

## Input

`pkg/kube.Transformer.TransformPod` accepts:

```go
func (Transformer) TransformPod(pod *corev1.Pod) (core.Binding, error)
```

## Mapped Fields

| Kubernetes field | Core field |
| --- | --- |
| `metadata.namespace` | `Binding.Namespace` |
| `metadata.name` | `Binding.Name` |
| `metadata.uid` | `Binding.WorkloadUID` |
| `metadata.annotations` | provider hints |
| `spec.restartPolicy` | `WorkloadRequest.RestartPolicy` |
| `spec.imagePullSecrets[*].name` | `WorkloadRequest.ImagePullSecrets` |
| `spec.containers[0].image` | `WorkloadRequest.Image` |
| `spec.containers[0].command` | `WorkloadRequest.Command` |
| `spec.containers[0].args` | `WorkloadRequest.Args` |
| literal `spec.containers[0].env` | `WorkloadRequest.Env` |
| `spec.containers[0].ports` | `WorkloadRequest.Ports` |
| `nvidia.com/gpu` limit or request | `WorkloadRequest.GPUCount` |
| `spec.hostNetwork` | compatibility validation |
| `spec.initContainers` | compatibility validation |
| `spec.volumes` | compatibility validation |
| `spec.containers[0].securityContext.privileged` | compatibility validation |

## Current Limits

- Only literal env vars are mapped. `ValueFrom` refs need an explicit Secret/ConfigMap projection model later.
- The adapter does not watch Kubernetes. It is a transformer layer only.
- The Virtual Kubelet provider wrapper still needs to call this adapter from `CreatePod`.
