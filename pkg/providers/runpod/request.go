package runpod

import (
	"strings"

	"github.com/aibrix/neocloud-node/pkg/core"
)

const (
	defaultContainerDiskGB  = 50
	defaultPersistentDiskGB = 20
	defaultMountPath        = "/workspace"
)

// Wire fields follow CreatePodRequest in https://api.runpod.io/v2/openapi.json.
type createPodRequest struct {
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	Disk          int               `json:"disk,omitempty"`
	Cloud         string            `json:"cloud,omitempty"`
	Entrypoint    []string          `json:"entrypoint,omitempty"`
	Cmd           *[]string         `json:"cmd,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	GPU           podGPU            `json:"gpu"`
	Ports         []string          `json:"ports,omitempty"`
	TemplateID    string            `json:"templateId,omitempty"`
	DataCenterIDs []string          `json:"dataCenterIds,omitempty"`
	Registry      string            `json:"registry,omitempty"`
	Mounts        *podMounts        `json:"mounts,omitempty"`
}

type podGPU struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

type podMounts struct {
	Persistent *persistentMount `json:"persistent,omitempty"`
	Network    []networkMount   `json:"network,omitempty"`
}

type persistentMount struct {
	Size int    `json:"size"`
	Path string `json:"path"`
}

type networkMount struct {
	VolumeID string `json:"volumeId"`
	Path     string `json:"path"`
}

func newCreatePodRequest(name string, request core.WorkloadRequest) (createPodRequest, error) {
	if request.MarketType == core.MarketTypeSpot {
		return createPodRequest{}, unsupported(core.AnnotationMarketType, "RunPod REST v2 does not support spot Pods")
	}
	if providerHint(request, core.AnnotationRunPodDataCenterPriority) != "" {
		return createPodRequest{}, unsupported(core.AnnotationRunPodDataCenterPriority, "RunPod REST v2 has no data center priority equivalent")
	}
	if strings.TrimSpace(request.GPUType) == "" || strings.Contains(request.GPUType, ",") {
		return createPodRequest{}, unsupported(core.AnnotationGPUType, "RunPod REST v2 requires one explicit GPU type ID")
	}
	if request.GPUCount < 1 || request.GPUCount > 1024 {
		return createPodRequest{}, unsupported("gpuCount", "RunPod REST v2 requires a GPU count between 1 and 1024")
	}

	payload := createPodRequest{
		Name:          name,
		Image:         request.Image,
		Cloud:         providerHint(request, core.AnnotationRunPodCloudType),
		Entrypoint:    request.Command,
		Env:           cloneStringMap(request.Env),
		GPU:           podGPU{ID: strings.TrimSpace(request.GPUType), Count: request.GPUCount},
		Ports:         runpodPorts(request.Ports),
		TemplateID:    providerHint(request, core.AnnotationRunPodTemplateID),
		DataCenterIDs: splitCSV(providerHint(request, core.AnnotationRunPodDataCenterIDs)),
		Registry:      providerHint(request, core.AnnotationRunPodRegistryAuthID),
	}
	if payload.TemplateID == "" {
		// v1 defaulted the container disk to 50 GB. v2 deployments need an
		// explicit disk when no template supplies the container configuration.
		payload.Disk = defaultContainerDiskGB
	}
	if len(request.Command) > 0 || len(request.Args) > 0 {
		// An explicit empty CMD clears the image default when only command is set.
		cmd := append([]string{}, request.Args...)
		payload.Cmd = &cmd
	}

	volumeID := providerHint(request, core.AnnotationRunPodNetworkVolumeID)
	path := providerHint(request, core.AnnotationRunPodVolumeMountPath)
	if path == "" {
		path = defaultMountPath
	}
	switch {
	case volumeID != "":
		payload.Mounts = &podMounts{Network: []networkMount{{VolumeID: volumeID, Path: path}}}
	case payload.TemplateID == "":
		// Preserve v1's default volume; v2 otherwise provisions no persistent storage.
		payload.Mounts = &podMounts{Persistent: &persistentMount{Size: defaultPersistentDiskGB, Path: path}}
	case providerHint(request, core.AnnotationRunPodVolumeMountPath) != "":
		// A path alone cannot override an unknown template volume size in v2.
		return createPodRequest{}, unsupported(core.AnnotationRunPodVolumeMountPath, "with a template, specify a network volume or leave its mount path to the template")
	}
	return payload, nil
}

func unsupported(field, message string) error {
	return &core.UnsupportedSpecError{Code: "UnsupportedRunPodSpec", Field: field, Message: message}
}
