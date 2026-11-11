package core

import (
	"fmt"
	"strings"
)

const (
	AnnotationProvider   = "cloud.aibrix.ai/provider"
	AnnotationGPUType    = "cloud.aibrix.ai/gpu-type"
	AnnotationGPUMemory  = "cloud.aibrix.ai/gpu-memory"
	AnnotationMaxPrice   = "cloud.aibrix.ai/max-price"
	AnnotationMarketType = "cloud.aibrix.ai/market-type"
	AnnotationRegion     = "cloud.aibrix.ai/region"

	// AnnotationEndpoints is written by the controller on remote Pods. It holds
	// a JSON list of published container ports with their public host and port.
	AnnotationEndpoints = "cloud.aibrix.ai/endpoints"

	AnnotationRunPodCloudType          = "cloud.aibrix.ai/runpod-cloud-type"
	AnnotationRunPodTemplateID         = "cloud.aibrix.ai/runpod-template-id"
	AnnotationRunPodDataCenterIDs      = "cloud.aibrix.ai/runpod-datacenter-ids"
	AnnotationRunPodDataCenterPriority = "cloud.aibrix.ai/runpod-datacenter-priority"
	AnnotationRunPodNetworkVolumeID    = "cloud.aibrix.ai/runpod-network-volume-id"
	AnnotationRunPodRegistryAuthID     = "cloud.aibrix.ai/runpod-registry-auth-id"
	AnnotationRunPodVolumeMountPath    = "cloud.aibrix.ai/runpod-volume-mount-path"

	AnnotationLambdaInstanceType   = "cloud.aibrix.ai/lambda-instance-type"
	AnnotationLambdaSSHKeyNames    = "cloud.aibrix.ai/lambda-ssh-key-names"
	AnnotationLambdaFileSystemIDs  = "cloud.aibrix.ai/lambda-file-system-ids"
	AnnotationLambdaImageID        = "cloud.aibrix.ai/lambda-image-id"
	AnnotationLambdaFirewallIDs    = "cloud.aibrix.ai/lambda-firewall-ids"
	AnnotationLambdaCallbackSecret = "cloud.aibrix.ai/lambda-callback-secret"

	AnnotationVastVerified  = "cloud.aibrix.ai/vast-verified"
	AnnotationVastMachineID = "cloud.aibrix.ai/vast-machine-id"
	AnnotationVastDiskGB    = "cloud.aibrix.ai/vast-disk-gb"
	AnnotationVastOfferID   = "cloud.aibrix.ai/vast-offer-id"
)

const (
	MarketTypeOnDemand = "on-demand"
	MarketTypeSpot     = "spot"
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
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	EnvRefs []EnvRef
	EnvFrom []EnvFromRef
	// EnvValueFromIndices lists env entries whose valueFrom source cannot be
	// resolved, preserving source positions for unsupported-field errors.
	EnvValueFromIndices []int
	Ports               []PortSpec
	Resources           ResourceSpec
	SecurityContext     SecurityContext
	ReadinessProbe      *ProbeSpec
}

// Probe handlers that the controller can run against a published port.
const (
	ProbeHandlerHTTPGet   = "httpGet"
	ProbeHandlerTCPSocket = "tcpSocket"
	ProbeHandlerExec      = "exec"
	ProbeHandlerGRPC      = "grpc"
)

// ProbeSpec is the part of a probe needed to validate it. Port is the resolved
// container port, or 0 when a named port does not match a container port.
type ProbeSpec struct {
	Handler string
	Port    int
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
	if len(container.EnvValueFromIndices) > 0 {
		return WorkloadRequest{}, &UnsupportedSpecError{
			Code:    "UnsupportedEnvValueFrom",
			Field:   fmt.Sprintf("spec.containers[0].env[%d].valueFrom", container.EnvValueFromIndices[0]),
			Message: "only secretKeyRef, configMapKeyRef and metadata or spec fieldRef environment sources are supported",
		}
	}
	if err := validateReadinessProbe(container); err != nil {
		return WorkloadRequest{}, err
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
		EnvRefs:          cloneEnvRefs(container.EnvRefs),
		EnvFrom:          append([]EnvFromRef(nil), container.EnvFrom...),
		GPUCount:         container.Resources.GPUCount,
		GPUType:          pod.Annotations[AnnotationGPUType],
		GPUMemory:        pod.Annotations[AnnotationGPUMemory],
		MaxPrice:         pod.Annotations[AnnotationMaxPrice],
		MarketType:       marketTypeOrDefault(pod.Annotations[AnnotationMarketType]),
		Provider:         pod.Annotations[AnnotationProvider],
		RegionHint:       pod.Annotations[AnnotationRegion],
		RestartPolicy:    restartPolicyOrDefault(pod.RestartPolicy),
		ImagePullSecrets: append([]string(nil), pod.ImagePullSecrets...),
		Ports:            portsFromSpecs(container.Ports),
		ProviderHints:    providerHints(pod.Annotations),
	}, nil
}

// validateReadinessProbe accepts probes that the controller can run from
// outside the remote host: httpGet and tcpSocket against a declared container
// port, which the provider publishes.
func validateReadinessProbe(container ContainerSpec) error {
	probe := container.ReadinessProbe
	if probe == nil {
		return nil
	}
	switch probe.Handler {
	case ProbeHandlerHTTPGet, ProbeHandlerTCPSocket:
	default:
		handler := probe.Handler
		if handler == "" {
			handler = "unknown"
		}
		return &UnsupportedSpecError{
			Code:    "UnsupportedReadinessProbe",
			Field:   "spec.containers[0].readinessProbe",
			Message: fmt.Sprintf("%s readiness probes are not supported; use httpGet or tcpSocket", handler),
		}
	}
	for _, port := range container.Ports {
		if probe.Port > 0 && port.ContainerPort == probe.Port {
			return nil
		}
	}
	return &UnsupportedSpecError{
		Code:    "UnsupportedReadinessProbe",
		Field:   fmt.Sprintf("spec.containers[0].readinessProbe.%s.port", probe.Handler),
		Message: "readiness probe port must be a declared container port so that the provider publishes it",
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

func cloneEnvRefs(refs []EnvRef) []EnvRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]EnvRef, 0, len(refs))
	for _, ref := range refs {
		clone := EnvRef{Name: ref.Name}
		if ref.SecretKeyRef != nil {
			keyRef := *ref.SecretKeyRef
			clone.SecretKeyRef = &keyRef
		}
		if ref.ConfigMapKeyRef != nil {
			keyRef := *ref.ConfigMapKeyRef
			clone.ConfigMapKeyRef = &keyRef
		}
		out = append(out, clone)
	}
	return out
}

func restartPolicyOrDefault(policy RestartPolicy) string {
	if policy == "" {
		return string(RestartPolicyNever)
	}
	return string(policy)
}

func marketTypeOrDefault(marketType string) string {
	if strings.TrimSpace(marketType) == "" {
		return MarketTypeOnDemand
	}
	return marketType
}

func providerHints(annotations map[string]string) map[string]string {
	if len(annotations) == 0 {
		return nil
	}
	hints := make(map[string]string)
	for _, key := range []string{
		AnnotationRunPodCloudType,
		AnnotationRunPodTemplateID,
		AnnotationRunPodDataCenterIDs,
		AnnotationRunPodDataCenterPriority,
		AnnotationRunPodNetworkVolumeID,
		AnnotationRunPodRegistryAuthID,
		AnnotationRunPodVolumeMountPath,
		AnnotationLambdaInstanceType,
		AnnotationLambdaSSHKeyNames,
		AnnotationLambdaFileSystemIDs,
		AnnotationLambdaImageID,
		AnnotationLambdaFirewallIDs,
		AnnotationLambdaCallbackSecret,
		AnnotationVastVerified,
		AnnotationVastMachineID,
		AnnotationVastDiskGB,
		AnnotationVastOfferID,
	} {
		if value := strings.TrimSpace(annotations[key]); value != "" {
			hints[key] = value
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
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
