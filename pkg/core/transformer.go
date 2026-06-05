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
	Namespace   string
	Name        string
	WorkloadUID string
	Annotations map[string]string
	Containers  []ContainerSpec
}

type ContainerSpec struct {
	Name      string
	Image     string
	Command   []string
	Args      []string
	Env       map[string]string
	Resources ResourceSpec
}

type ResourceSpec struct {
	GPUCount int
}

type UnsupportedSpecError struct {
	Code    string
	Message string
}

func (e *UnsupportedSpecError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

type DefaultTransformer struct{}

func (DefaultTransformer) Transform(pod PodSpec) (WorkloadRequest, error) {
	if len(pod.Containers) != 1 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedContainerCount",
			Message: "exactly one main container is supported",
		}
	}

	container := pod.Containers[0]
	if strings.TrimSpace(container.Image) == "" {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "MissingImage",
			Message: "container image is required",
		}
	}
	if container.Resources.GPUCount <= 0 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "MissingGPURequest",
			Message: "nvidia.com/gpu request must be greater than zero",
		}
	}

	return WorkloadRequest{
		Image:      container.Image,
		Command:    append([]string(nil), container.Command...),
		Args:       append([]string(nil), container.Args...),
		Env:        cloneStringMap(container.Env),
		GPUCount:   container.Resources.GPUCount,
		GPUType:    pod.Annotations[AnnotationGPUType],
		MaxPrice:   pod.Annotations[AnnotationMaxPrice],
		Provider:   pod.Annotations[AnnotationProvider],
		RegionHint: pod.Annotations[AnnotationRegion],
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
