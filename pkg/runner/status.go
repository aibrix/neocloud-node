package runner

import (
	"fmt"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

type ReportPhase string

const (
	ReportPhaseRunning   ReportPhase = "Running"
	ReportPhaseSucceeded ReportPhase = "Succeeded"
	ReportPhaseFailed    ReportPhase = "Failed"
)

type StatusReport struct {
	WorkloadID string      `json:"workloadID"`
	Phase      ReportPhase `json:"phase,omitempty"`
	ExitCode   int         `json:"exitCode,omitempty"`
	Reason     string      `json:"reason,omitempty"`
	Message    string      `json:"message,omitempty"`
}

func (r StatusReport) NamespaceAndUID() (string, string, error) {
	parts := strings.Split(r.WorkloadID, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid workloadID %q; want <namespace>/<workloadUID>", r.WorkloadID)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}

func (r StatusReport) CoreStatus() (core.RunnerStatus, error) {
	phase, err := r.corePhase()
	if err != nil {
		return core.RunnerStatus{}, err
	}
	return core.RunnerStatus{
		Phase:    phase,
		ExitCode: r.ExitCode,
		Reason:   strings.TrimSpace(r.Reason),
		Message:  strings.TrimSpace(r.Message),
	}, nil
}

func (r StatusReport) corePhase() (core.RunnerPhase, error) {
	switch strings.ToLower(strings.TrimSpace(string(r.Phase))) {
	case "":
		if r.ExitCode == 0 {
			return core.RunnerPhaseSucceeded, nil
		}
		return core.RunnerPhaseFailed, nil
	case "running", "heartbeat":
		return core.RunnerPhaseRunning, nil
	case "succeeded", "success", "completed":
		return core.RunnerPhaseSucceeded, nil
	case "failed", "failure":
		return core.RunnerPhaseFailed, nil
	default:
		return "", fmt.Errorf("unsupported runner phase %q", r.Phase)
	}
}
