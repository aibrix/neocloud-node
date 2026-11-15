package runner

import (
	"os/exec"
	"testing"
)

func TestLogShipperExecutable(t *testing.T) {
	p, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("python3 unavailable")
	}
	out, e := exec.Command(p, "-m", "unittest", "-v", "logship_test.py").CombinedOutput()
	if e != nil {
		t.Fatalf("Python helper: %v\n%s", e, out)
	}
}
