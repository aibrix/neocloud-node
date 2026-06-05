package core

import (
	"fmt"
	"strings"
)

type LaunchPolicy struct {
	RequireMaxPrice bool
	MaxGPUCount     int
	MaxPorts        int
}

type LaunchGuard struct {
	policy LaunchPolicy
}

func NewLaunchGuard(policy LaunchPolicy) LaunchGuard {
	return LaunchGuard{policy: policy}
}

func (g LaunchGuard) Validate(request WorkloadRequest) error {
	if g.policy.RequireMaxPrice && strings.TrimSpace(request.MaxPrice) == "" {
		return &PolicyViolationError{
			Code:    "MissingMaxPrice",
			Field:   "maxPrice",
			Message: "max price is required before launching remote GPU resources",
		}
	}
	if g.policy.MaxGPUCount > 0 && request.GPUCount > g.policy.MaxGPUCount {
		return &PolicyViolationError{
			Code:    "GPUCountExceeded",
			Field:   "gpuCount",
			Message: fmt.Sprintf("gpu count %d exceeds max %d", request.GPUCount, g.policy.MaxGPUCount),
		}
	}
	if g.policy.MaxPorts > 0 && len(request.Ports) > g.policy.MaxPorts {
		return &PolicyViolationError{
			Code:    "PortCountExceeded",
			Field:   "ports",
			Message: fmt.Sprintf("port count %d exceeds max %d", len(request.Ports), g.policy.MaxPorts),
		}
	}
	return nil
}

type PolicyViolationError struct {
	Code    string
	Field   string
	Message string
}

func (e *PolicyViolationError) Error() string {
	if e.Field == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Code, e.Field, e.Message)
}
