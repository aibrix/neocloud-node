package kube

import (
	"fmt"
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
		InitContainers:   containersFromCoreV1(pod.Spec.InitContainers),
		Volumes:          volumesFromCoreV1(pod.Spec.Volumes),
		HostNetwork:      pod.Spec.HostNetwork,
		Annotations:      cloneStringMap(pod.Annotations),
		Containers:       containersFromCoreV1(pod.Spec.Containers),
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

func containersFromCoreV1(containers []corev1.Container) []core.ContainerSpec {
	if len(containers) == 0 {
		return nil
	}
	out := make([]core.ContainerSpec, 0, len(containers))
	for _, container := range containers {
		env, valueFromIndices := envFromCoreV1(container.Env)
		out = append(out, core.ContainerSpec{
			Name:                container.Name,
			Image:               container.Image,
			Command:             append([]string(nil), container.Command...),
			Args:                append([]string(nil), container.Args...),
			Env:                 env,
			EnvValueFromIndices: valueFromIndices,
			HasEnvFrom:          len(container.EnvFrom) > 0,
			Ports:               portsFromCoreV1(container.Ports),
			Resources:           resourcesFromCoreV1(container.Resources),
			SecurityContext:     securityContextFromCoreV1(container.SecurityContext),
			ReadinessProbe:      probeFromCoreV1(container, container.ReadinessProbe),
		})
	}
	return out
}

func envFromCoreV1(env []corev1.EnvVar) (map[string]string, []int) {
	if len(env) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(env))
	var valueFromIndices []int
	for index, item := range env {
		if item.ValueFrom == nil {
			out[item.Name] = item.Value
		} else {
			valueFromIndices = append(valueFromIndices, index)
		}
	}
	return out, valueFromIndices
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
