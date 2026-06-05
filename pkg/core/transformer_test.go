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
