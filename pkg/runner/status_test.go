package runner

import (
	"testing"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestStatusReportNamespaceAndUID(t *testing.T) {
	t.Parallel()

	report := StatusReport{WorkloadID: "default/pod-123", ExitCode: 0}
	namespace, workloadUID, err := report.NamespaceAndUID()
	if err != nil {
		t.Fatalf("NamespaceAndUID() error = %v", err)
	}
	if namespace != "default" || workloadUID != "pod-123" {
		t.Fatalf("namespace/uid = %s/%s, want default/pod-123", namespace, workloadUID)
	}
}

func TestStatusReportRejectsInvalidWorkloadID(t *testing.T) {
	t.Parallel()

	for _, workloadID := range []string{"", "default", "/pod-123", "default/"} {
		report := StatusReport{WorkloadID: workloadID}
		if _, _, err := report.NamespaceAndUID(); err == nil {
			t.Fatalf("NamespaceAndUID(%q) error = nil, want validation error", workloadID)
		}
	}
}

func TestStatusReportCoreStatusSupportsPhasesAndLegacyExitCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		report StatusReport
		phase  core.RunnerPhase
	}{
		{
			name:   "running phase",
			report: StatusReport{WorkloadID: "default/pod-123", Phase: ReportPhaseRunning, Message: "started"},
			phase:  core.RunnerPhaseRunning,
		},
		{
			name:   "legacy success exit code",
			report: StatusReport{WorkloadID: "default/pod-123", ExitCode: 0},
			phase:  core.RunnerPhaseSucceeded,
		},
		{
			name:   "legacy failed exit code",
			report: StatusReport{WorkloadID: "default/pod-123", ExitCode: 2},
			phase:  core.RunnerPhaseFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			status, err := tt.report.CoreStatus()
			if err != nil {
				t.Fatalf("CoreStatus() error = %v", err)
			}
			if status.Phase != tt.phase {
				t.Fatalf("phase = %s, want %s", status.Phase, tt.phase)
			}
		})
	}
}

func TestStatusReportCoreStatusRejectsUnknownPhase(t *testing.T) {
	t.Parallel()

	report := StatusReport{WorkloadID: "default/pod-123", Phase: "Paused"}
	if _, err := report.CoreStatus(); err == nil {
		t.Fatal("CoreStatus() error = nil, want unknown phase error")
	}
}
