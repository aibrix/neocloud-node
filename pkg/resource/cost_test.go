package resource

import (
	"testing"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestEstimateBurnRate(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	now := start.Add(90 * time.Minute)
	snapshot := EstimateBurnRate(core.RemoteResource{
		ID:         "remote-1",
		Provider:   "runpod",
		HourlyCost: 2.0,
		CostSource: "provider",
	}, start, now, 30*time.Minute)

	if snapshot.Elapsed != 90*time.Minute {
		t.Fatalf("elapsed = %s, want 90m", snapshot.Elapsed)
	}
	if snapshot.CurrentBurnRate != 2.0 {
		t.Fatalf("burn rate = %.2f, want 2.00", snapshot.CurrentBurnRate)
	}
	if snapshot.Spent != 3.0 {
		t.Fatalf("spent = %.2f, want 3.00", snapshot.Spent)
	}
	if snapshot.ProjectedTotal != 4.0 {
		t.Fatalf("projected total = %.2f, want 4.00", snapshot.ProjectedTotal)
	}
}

func TestEstimateBurnRateHandlesMissingCost(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)
	snapshot := EstimateBurnRate(core.RemoteResource{
		ID:       "remote-1",
		Provider: "lambda",
	}, start, start.Add(time.Hour), 0)

	if snapshot.CurrentBurnRate != 0 || snapshot.Spent != 0 || snapshot.ProjectedTotal != 0 {
		t.Fatalf("snapshot = %#v, want zero cost fields", snapshot)
	}
	if snapshot.CostSource != "" {
		t.Fatalf("cost source = %q, want empty", snapshot.CostSource)
	}
}
