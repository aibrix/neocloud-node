package core

import (
	"errors"
	"testing"
)

func TestTransformerAcceptsSingleGPUContainer(t *testing.T) {
	t.Parallel()

	transformer := DefaultTransformer{}
	request, err := transformer.Transform(PodSpec{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Annotations: map[string]string{
			"gpu-burst-node.aibrix.ai/provider":  "runpod",
			"gpu-burst-node.aibrix.ai/gpu-type":  "H100",
			"gpu-burst-node.aibrix.ai/max-price": "4.50",
			"gpu-burst-node.aibrix.ai/region":    "us",
		},
		Containers: []ContainerSpec{
			{
				Name:    "main",
				Image:   "example.com/train:latest",
				Command: []string{"python"},
				Args:    []string{"train.py"},
				Env: map[string]string{
					"MODEL": "llama",
				},
				Resources: ResourceSpec{
					GPUCount: 1,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Transform returned error: %v", err)
	}

	if request.Image != "example.com/train:latest" {
		t.Fatalf("request image = %q, want example.com/train:latest", request.Image)
	}
	if request.GPUCount != 1 {
		t.Fatalf("request GPU count = %d, want 1", request.GPUCount)
	}
	if request.Provider != "runpod" {
		t.Fatalf("request provider = %q, want runpod", request.Provider)
	}
	if request.GPUType != "H100" {
		t.Fatalf("request GPU type = %q, want H100", request.GPUType)
	}
	if request.Env["MODEL"] != "llama" {
		t.Fatalf("request env MODEL = %q, want llama", request.Env["MODEL"])
	}
}

func TestTransformerRejectsUnsupportedSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pod  PodSpec
		code string
	}{
		{
			name: "multi container",
			pod: PodSpec{
				Containers: []ContainerSpec{
					{Name: "main", Image: "example.com/train:latest", Resources: ResourceSpec{GPUCount: 1}},
					{Name: "sidecar", Image: "example.com/sidecar:latest", Resources: ResourceSpec{GPUCount: 1}},
				},
			},
			code: "UnsupportedContainerCount",
		},
		{
			name: "missing image",
			pod: PodSpec{
				Containers: []ContainerSpec{
					{Name: "main", Resources: ResourceSpec{GPUCount: 1}},
				},
			},
			code: "MissingImage",
		},
		{
			name: "zero gpu",
			pod: PodSpec{
				Containers: []ContainerSpec{
					{Name: "main", Image: "example.com/train:latest"},
				},
			},
			code: "MissingGPURequest",
		},
	}

	transformer := DefaultTransformer{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := transformer.Transform(tt.pod)
			var unsupported *UnsupportedSpecError
			if !errors.As(err, &unsupported) {
				t.Fatalf("Transform error = %v, want UnsupportedSpecError", err)
			}
			if unsupported.Code != tt.code {
				t.Fatalf("unsupported code = %q, want %q", unsupported.Code, tt.code)
			}
		})
	}
}

func TestTransformerMapsSupportedCompatibilityFields(t *testing.T) {
	t.Parallel()

	transformer := DefaultTransformer{}
	request, err := transformer.Transform(PodSpec{
		Namespace:        "default",
		Name:             "train-job",
		WorkloadUID:      "pod-123",
		RestartPolicy:    RestartPolicyOnFailure,
		ImagePullSecrets: []string{"registry-creds"},
		Annotations: map[string]string{
			AnnotationProvider: "runpod",
			AnnotationGPUType:  "H100",
		},
		Containers: []ContainerSpec{
			{
				Name:    "main",
				Image:   "example.com/train:latest",
				Command: []string{"python"},
				Args:    []string{"train.py"},
				Env: map[string]string{
					"MODEL": "llama",
				},
				Ports: []PortSpec{
					{Name: "metrics", ContainerPort: 9090, Protocol: "TCP"},
				},
				Resources: ResourceSpec{
					GPUCount: 1,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Transform returned error: %v", err)
	}

	if request.RestartPolicy != string(RestartPolicyOnFailure) {
		t.Fatalf("restart policy = %q, want %q", request.RestartPolicy, RestartPolicyOnFailure)
	}
	if len(request.ImagePullSecrets) != 1 || request.ImagePullSecrets[0] != "registry-creds" {
		t.Fatalf("image pull secrets = %#v, want registry-creds", request.ImagePullSecrets)
	}
	if len(request.Ports) != 1 {
		t.Fatalf("ports length = %d, want 1", len(request.Ports))
	}
	if request.Ports[0].Name != "metrics" || request.Ports[0].ContainerPort != 9090 || request.Ports[0].Protocol != "TCP" {
		t.Fatalf("port = %#v, want metrics TCP 9090", request.Ports[0])
	}
}

func TestTransformerRejectsPhase1UnsupportedSpecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		pod   PodSpec
		code  string
		field string
	}{
		{
			name: "restart policy always",
			pod: PodSpec{
				RestartPolicy: RestartPolicyAlways,
				Containers: []ContainerSpec{
					{Name: "main", Image: "example.com/train:latest", Resources: ResourceSpec{GPUCount: 1}},
				},
			},
			code:  "UnsupportedRestartPolicy",
			field: "spec.restartPolicy",
		},
		{
			name: "init containers",
			pod: PodSpec{
				InitContainers: []ContainerSpec{
					{Name: "init", Image: "example.com/init:latest"},
				},
				Containers: []ContainerSpec{
					{Name: "main", Image: "example.com/train:latest", Resources: ResourceSpec{GPUCount: 1}},
				},
			},
			code:  "UnsupportedInitContainers",
			field: "spec.initContainers",
		},
		{
			name: "volumes",
			pod: PodSpec{
				Volumes: []VolumeSpec{
					{Name: "data", Kind: "persistentVolumeClaim"},
				},
				Containers: []ContainerSpec{
					{Name: "main", Image: "example.com/train:latest", Resources: ResourceSpec{GPUCount: 1}},
				},
			},
			code:  "UnsupportedVolumes",
			field: "spec.volumes",
		},
		{
			name: "host network",
			pod: PodSpec{
				HostNetwork: true,
				Containers: []ContainerSpec{
					{Name: "main", Image: "example.com/train:latest", Resources: ResourceSpec{GPUCount: 1}},
				},
			},
			code:  "UnsupportedHostNetwork",
			field: "spec.hostNetwork",
		},
		{
			name: "privileged container",
			pod: PodSpec{
				Containers: []ContainerSpec{
					{
						Name:            "main",
						Image:           "example.com/train:latest",
						Resources:       ResourceSpec{GPUCount: 1},
						SecurityContext: SecurityContext{Privileged: true},
					},
				},
			},
			code:  "UnsupportedPrivilegedContainer",
			field: "spec.containers[0].securityContext.privileged",
		},
	}

	transformer := DefaultTransformer{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := transformer.Transform(tt.pod)
			var unsupported *UnsupportedSpecError
			if !errors.As(err, &unsupported) {
				t.Fatalf("Transform error = %v, want UnsupportedSpecError", err)
			}
			if unsupported.Code != tt.code {
				t.Fatalf("unsupported code = %q, want %q", unsupported.Code, tt.code)
			}
			if unsupported.Field != tt.field {
				t.Fatalf("unsupported field = %q, want %q", unsupported.Field, tt.field)
			}
		})
	}
}
