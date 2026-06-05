package core

import (
	"fmt"
	"strings"
)

const (
	AnnotationProvider = "gpu-burst-node.aibrix.ai/provider"
	AnnotationGPUType  = "gpu-burst-node.aibrix.ai/gpu-type"
	AnnotationMaxPrice = "gpu-burst-node.aibrix.ai/max-price"
	AnnotationRegion   = "gpu-burst-node.aibrix.ai/region"
)

type PodSpec struct {
	Namespace        string
	Name             string
	WorkloadUID      string
	RestartPolicy    RestartPolicy
	ImagePullSecrets []string
	InitContainers   []ContainerSpec
	Volumes          []VolumeSpec
	HostNetwork      bool
	Annotations      map[string]string
	Containers       []ContainerSpec
}

type ContainerSpec struct {
	Name            string
	Image           string
	Command         []string
	Args            []string
	Env             map[string]string
	Ports           []PortSpec
	Resources       ResourceSpec
	SecurityContext SecurityContext
}

type RestartPolicy string

const (
	RestartPolicyNever     RestartPolicy = "Never"
	RestartPolicyOnFailure RestartPolicy = "OnFailure"
	RestartPolicyAlways    RestartPolicy = "Always"
)

type PortSpec struct {
	Name          string
	ContainerPort int
	Protocol      string
}

type VolumeSpec struct {
	Name string
	Kind string
}

type SecurityContext struct {
	Privileged bool
}

type ResourceSpec struct {
	GPUCount int
}

type UnsupportedSpecError struct {
	Code    string
	Field   string
	Message string
}

func (e *UnsupportedSpecError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Message)
}

type DefaultTransformer struct{}

func (DefaultTransformer) Transform(pod PodSpec) (WorkloadRequest, error) {
	if pod.RestartPolicy == RestartPolicyAlways {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedRestartPolicy",
			Field:   "spec.restartPolicy",
			Message: "restartPolicy Always is not supported for remote GPU workloads",
		}
	}
	if len(pod.InitContainers) > 0 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedInitContainers",
			Field:   "spec.initContainers",
			Message: "init containers are not supported in Phase 1",
		}
	}
	if len(pod.Volumes) > 0 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedVolumes",
			Field:   "spec.volumes",
			Message: "volumes are not supported in Phase 1",
		}
	}
	if pod.HostNetwork {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedHostNetwork",
			Field:   "spec.hostNetwork",
			Message: "host networking is not available on virtual cloud GPU nodes",
		}
	}
	if len(pod.Containers) != 1 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedContainerCount",
			Field:   "spec.containers",
			Message: "exactly one main container is supported",
		}
	}

	container := pod.Containers[0]
	if container.SecurityContext.Privileged {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedPrivilegedContainer",
			Field:   "spec.containers[0].securityContext.privileged",
			Message: "privileged containers are not supported on remote GPU providers",
		}
	}
	if strings.TrimSpace(container.Image) == "" {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "MissingImage",
			Field:   "spec.containers[0].image",
			Message: "container image is required",
		}
	}
	if container.Resources.GPUCount <= 0 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "MissingGPURequest",
			Field:   "spec.containers[0].resources.limits[nvidia.com/gpu]",
			Message: "nvidia.com/gpu request must be greater than zero",
		}
	}

	return WorkloadRequest{
		Image:            container.Image,
		Command:          append([]string(nil), container.Command...),
		Args:             append([]string(nil), container.Args...),
		Env:              cloneStringMap(container.Env),
		GPUCount:         container.Resources.GPUCount,
		GPUType:          pod.Annotations[AnnotationGPUType],
		MaxPrice:         pod.Annotations[AnnotationMaxPrice],
		Provider:         pod.Annotations[AnnotationProvider],
		RegionHint:       pod.Annotations[AnnotationRegion],
		RestartPolicy:    restartPolicyOrDefault(pod.RestartPolicy),
		ImagePullSecrets: append([]string(nil), pod.ImagePullSecrets...),
		Ports:            portsFromSpecs(container.Ports),
	}, nil
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

func restartPolicyOrDefault(policy RestartPolicy) string {
	if policy == "" {
		return string(RestartPolicyNever)
	}
	return string(policy)
}

func portsFromSpecs(specs []PortSpec) []Port {
	if len(specs) == 0 {
		return nil
	}
	ports := make([]Port, 0, len(specs))
	for _, spec := range specs {
		protocol := spec.Protocol
		if protocol == "" {
			protocol = "TCP"
		}
		ports = append(ports, Port{
			Name:          spec.Name,
			ContainerPort: spec.ContainerPort,
			Protocol:      protocol,
		})
	}
	return ports
}
