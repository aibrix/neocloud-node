package runner

import (
	"fmt"
	"strings"
)

type StatusReport struct {
	WorkloadID string `json:"workloadID"`
	ExitCode   int    `json:"exitCode"`
	Message    string `json:"message,omitempty"`
}

func (r StatusReport) NamespaceAndUID() (string, string, error) {
	parts := strings.Split(r.WorkloadID, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("invalid workloadID %q; want <namespace>/<workloadUID>", r.WorkloadID)
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), nil
}
