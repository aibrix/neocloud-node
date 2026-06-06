package runner

import "testing"

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
