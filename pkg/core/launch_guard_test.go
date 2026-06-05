package core

import (
	"errors"
	"testing"
)

func TestLaunchGuardRejectsUnsafeRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		policy  LaunchPolicy
		request WorkloadRequest
		code    string
	}{
		{
			name:   "missing max price",
			policy: LaunchPolicy{RequireMaxPrice: true},
			request: WorkloadRequest{
				Image:    "example.com/train:latest",
				GPUCount: 1,
			},
			code: "MissingMaxPrice",
		},
		{
			name:   "too many gpus",
			policy: LaunchPolicy{MaxGPUCount: 4},
			request: WorkloadRequest{
				Image:    "example.com/train:latest",
				GPUCount: 8,
				MaxPrice: "4.50",
			},
			code: "GPUCountExceeded",
		},
		{
			name:   "too many ports",
			policy: LaunchPolicy{MaxPorts: 1},
			request: WorkloadRequest{
				Image:    "example.com/train:latest",
				GPUCount: 1,
				MaxPrice: "4.50",
				Ports: []Port{
					{Name: "metrics", ContainerPort: 9090, Protocol: "TCP"},
					{Name: "http", ContainerPort: 8080, Protocol: "TCP"},
				},
			},
			code: "PortCountExceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			guard := NewLaunchGuard(tt.policy)
			err := guard.Validate(tt.request)
			var violation *PolicyViolationError
			if !errors.As(err, &violation) {
				t.Fatalf("Validate error = %v, want PolicyViolationError", err)
			}
			if violation.Code != tt.code {
				t.Fatalf("violation code = %q, want %q", violation.Code, tt.code)
			}
		})
	}
}

func TestLaunchGuardAllowsSafeRequest(t *testing.T) {
	t.Parallel()

	guard := NewLaunchGuard(LaunchPolicy{
		RequireMaxPrice: true,
		MaxGPUCount:     4,
		MaxPorts:        2,
	})

	err := guard.Validate(WorkloadRequest{
		Image:    "example.com/train:latest",
		GPUCount: 2,
		MaxPrice: "4.50",
		Ports: []Port{
			{Name: "metrics", ContainerPort: 9090, Protocol: "TCP"},
		},
	})
	if err != nil {
		t.Fatalf("Validate returned error: %v", err)
	}
}
