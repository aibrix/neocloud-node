package kube

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/aibrix/neocloud-node/pkg/core"
)

const nvidiaGPUResource = "nvidia.com/gpu"

type Transformer struct {
	Core core.DefaultTransformer
}

func (t Transformer) TransformPod(pod *corev1.Pod) (core.Binding, error) {
	if pod == nil {
		return core.Binding{}, fmt.Errorf("pod is nil")
	}

	corePod := core.PodSpec{
		Namespace:        pod.Namespace,
		Name:             pod.Name,
		WorkloadUID:      string(pod.UID),
		RestartPolicy:    restartPolicyFromCoreV1(pod.Spec.RestartPolicy),
		ImagePullSecrets: imagePullSecretNames(pod.Spec.ImagePullSecrets),
		InitContainers:   containersFromCoreV1(pod, pod.Spec.InitContainers),
		Volumes:          volumesFromCoreV1(pod.Spec.Volumes),
		HostNetwork:      pod.Spec.HostNetwork,
		Annotations:      cloneStringMap(pod.Annotations),
		Containers:       containersFromCoreV1(pod, pod.Spec.Containers),
	}

	transformer := t.Core
	request, err := transformer.Transform(corePod)
	if err != nil {
		return core.Binding{}, err
	}

	return core.Binding{
		Namespace:   pod.Namespace,
		Name:        pod.Name,
		WorkloadUID: string(pod.UID),
		Request:     request,
	}, nil
}

func restartPolicyFromCoreV1(policy corev1.RestartPolicy) core.RestartPolicy {
	switch policy {
	case corev1.RestartPolicyOnFailure:
		return core.RestartPolicyOnFailure
	case corev1.RestartPolicyAlways:
		return core.RestartPolicyAlways
	default:
		return core.RestartPolicyNever
	}
}

func imagePullSecretNames(refs []corev1.LocalObjectReference) []string {
	if len(refs) == 0 {
		return nil
	}
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.Name != "" {
			names = append(names, ref.Name)
		}
	}
	return names
}

func containersFromCoreV1(pod *corev1.Pod, containers []corev1.Container) []core.ContainerSpec {
	if len(containers) == 0 {
		return nil
	}
	out := make([]core.ContainerSpec, 0, len(containers))
	for _, container := range containers {
		env, refs, unsupportedIndices := envFromCoreV1(pod, container.Env)
		out = append(out, core.ContainerSpec{
			Name:                container.Name,
			Image:               container.Image,
			Command:             append([]string(nil), container.Command...),
			Args:                append([]string(nil), container.Args...),
			Env:                 env,
			EnvRefs:             refs,
			EnvFrom:             envFromSourcesFromCoreV1(container.EnvFrom),
			EnvValueFromIndices: unsupportedIndices,
			Ports:               portsFromCoreV1(container.Ports),
			Resources:           resourcesFromCoreV1(container.Resources),
			SecurityContext:     securityContextFromCoreV1(container.SecurityContext),
			ReadinessProbe:      probeFromCoreV1(container, container.ReadinessProbe),
		})
	}
	return out
}

// envFromCoreV1 splits env entries into literal values and Secret/ConfigMap
// references. fieldRef values are resolved from the Pod here. Entries are
// processed in order, so a later entry with the same name replaces an earlier
// one, as in kubelet. It also returns the indices of unsupported sources.
func envFromCoreV1(pod *corev1.Pod, env []corev1.EnvVar) (map[string]string, []core.EnvRef, []int) {
	if len(env) == 0 {
		return nil, nil, nil
	}
	literal := make(map[string]string, len(env))
	var refs []core.EnvRef
	var unsupported []int
	for index, item := range env {
		source := item.ValueFrom
		switch {
		case source == nil:
			literal[item.Name] = item.Value
			refs = removeEnvRef(refs, item.Name)
		case source.SecretKeyRef != nil:
			delete(literal, item.Name)
			refs = append(removeEnvRef(refs, item.Name), core.EnvRef{
				Name:         item.Name,
				SecretKeyRef: keyRef(source.SecretKeyRef.Name, source.SecretKeyRef.Key, source.SecretKeyRef.Optional),
			})
		case source.ConfigMapKeyRef != nil:
			delete(literal, item.Name)
			refs = append(removeEnvRef(refs, item.Name), core.EnvRef{
				Name: item.Name,
				ConfigMapKeyRef: keyRef(source.ConfigMapKeyRef.Name, source.ConfigMapKeyRef.Key,
					source.ConfigMapKeyRef.Optional),
			})
		case source.FieldRef != nil:
			value, ok := fieldRefValue(pod, source.FieldRef.FieldPath)
			if !ok {
				unsupported = append(unsupported, index)
				continue
			}
			literal[item.Name] = value
			refs = removeEnvRef(refs, item.Name)
		default:
			unsupported = append(unsupported, index)
		}
	}
	if len(literal) == 0 {
		literal = nil
	}
	return literal, refs, unsupported
}

func keyRef(name string, key string, optional *bool) *core.EnvKeyRef {
	return &core.EnvKeyRef{Name: name, Key: key, Optional: optional != nil && *optional}
}

func removeEnvRef(refs []core.EnvRef, name string) []core.EnvRef {
	out := refs[:0]
	for _, ref := range refs {
		if ref.Name != name {
			out = append(out, ref)
		}
	}
	return out
}

var metadataFieldPath = regexp.MustCompile(`^metadata\.(labels|annotations)\['(.+)'\]$`)

// fieldRefValue resolves the downward API fields that are known before the
// remote workload starts. status.podIP and status.hostIP are not known then.
func fieldRefValue(pod *corev1.Pod, fieldPath string) (string, bool) {
	if pod == nil {
		return "", false
	}
	switch fieldPath {
	case "metadata.name":
		return pod.Name, true
	case "metadata.namespace":
		return pod.Namespace, true
	case "metadata.uid":
		return string(pod.UID), true
	case "spec.nodeName":
		return pod.Spec.NodeName, true
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, true
	}
	match := metadataFieldPath.FindStringSubmatch(fieldPath)
	if match == nil {
		return "", false
	}
	if match[1] == "labels" {
		return pod.Labels[match[2]], true
	}
	return pod.Annotations[match[2]], true
}

func envFromSourcesFromCoreV1(sources []corev1.EnvFromSource) []core.EnvFromRef {
	if len(sources) == 0 {
		return nil
	}
	out := make([]core.EnvFromRef, 0, len(sources))
	for _, source := range sources {
		switch {
		case source.SecretRef != nil:
			out = append(out, core.EnvFromRef{
				Prefix:     source.Prefix,
				SecretName: source.SecretRef.Name,
				Optional:   source.SecretRef.Optional != nil && *source.SecretRef.Optional,
			})
		case source.ConfigMapRef != nil:
			out = append(out, core.EnvFromRef{
				Prefix:        source.Prefix,
				ConfigMapName: source.ConfigMapRef.Name,
				Optional:      source.ConfigMapRef.Optional != nil && *source.ConfigMapRef.Optional,
			})
		}
	}
	return out
}

func portsFromCoreV1(ports []corev1.ContainerPort) []core.PortSpec {
	if len(ports) == 0 {
		return nil
	}
	out := make([]core.PortSpec, 0, len(ports))
	for _, port := range ports {
		out = append(out, core.PortSpec{
			Name:          port.Name,
			ContainerPort: int(port.ContainerPort),
			Protocol:      string(port.Protocol),
		})
	}
	return out
}

func probeFromCoreV1(container corev1.Container, probe *corev1.Probe) *core.ProbeSpec {
	if probe == nil {
		return nil
	}
	switch {
	case probe.HTTPGet != nil:
		return &core.ProbeSpec{
			Handler: core.ProbeHandlerHTTPGet,
			Port:    ResolveContainerPort(container, probe.HTTPGet.Port),
		}
	case probe.TCPSocket != nil:
		return &core.ProbeSpec{
			Handler: core.ProbeHandlerTCPSocket,
			Port:    ResolveContainerPort(container, probe.TCPSocket.Port),
		}
	case probe.Exec != nil:
		return &core.ProbeSpec{Handler: core.ProbeHandlerExec}
	case probe.GRPC != nil:
		return &core.ProbeSpec{Handler: core.ProbeHandlerGRPC, Port: int(probe.GRPC.Port)}
	default:
		return &core.ProbeSpec{}
	}
}

// ResolveContainerPort returns the numeric container port for a probe port,
// resolving a named port against the container's ports. It returns 0 when a
// named port does not exist.
func ResolveContainerPort(container corev1.Container, port intstr.IntOrString) int {
	if port.Type == intstr.Int {
		return port.IntValue()
	}
	for _, containerPort := range container.Ports {
		if containerPort.Name != "" && containerPort.Name == port.StrVal {
			return int(containerPort.ContainerPort)
		}
	}
	return 0
}

func resourcesFromCoreV1(resources corev1.ResourceRequirements) core.ResourceSpec {
	quantity, ok := resources.Limits[corev1.ResourceName(nvidiaGPUResource)]
	if !ok {
		quantity, ok = resources.Requests[corev1.ResourceName(nvidiaGPUResource)]
	}
	if !ok {
		return core.ResourceSpec{}
	}
	return core.ResourceSpec{GPUCount: int(quantity.Value())}
}

func securityContextFromCoreV1(context *corev1.SecurityContext) core.SecurityContext {
	if context == nil || context.Privileged == nil {
		return core.SecurityContext{}
	}
	return core.SecurityContext{Privileged: *context.Privileged}
}

func volumesFromCoreV1(volumes []corev1.Volume) []core.VolumeSpec {
	if len(volumes) == 0 {
		return nil
	}
	out := make([]core.VolumeSpec, 0, len(volumes))
	for _, volume := range volumes {
		if isDefaultServiceAccountVolume(volume) {
			continue
		}
		out = append(out, core.VolumeSpec{
			Name: volume.Name,
			Kind: volumeKind(volume),
		})
	}
	return out
}

func isDefaultServiceAccountVolume(volume corev1.Volume) bool {
	if !strings.HasPrefix(volume.Name, "kube-api-access-") || volume.Projected == nil {
		return false
	}
	var hasToken, hasRootCA, hasDownwardAPI bool
	for _, source := range volume.Projected.Sources {
		if source.ServiceAccountToken != nil {
			hasToken = true
		}
		if source.ConfigMap != nil && source.ConfigMap.Name == "kube-root-ca.crt" {
			hasRootCA = true
		}
		if source.DownwardAPI != nil {
			hasDownwardAPI = true
		}
	}
	return hasToken && hasRootCA && hasDownwardAPI
}

func volumeKind(volume corev1.Volume) string {
	switch {
	case volume.PersistentVolumeClaim != nil:
		return "persistentVolumeClaim"
	case volume.HostPath != nil:
		return "hostPath"
	case volume.Secret != nil:
		return "secret"
	case volume.ConfigMap != nil:
		return "configMap"
	case volume.EmptyDir != nil:
		return "emptyDir"
	default:
		return "unknown"
	}
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
