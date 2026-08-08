package resource

import (
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
)

type BurnRateSnapshot struct {
	RemoteID        string
	Provider        string
	CostSource      string
	Elapsed         time.Duration
	Remaining       time.Duration
	CurrentBurnRate float64
	Spent           float64
	ProjectedTotal  float64
}

func EstimateBurnRate(remote core.RemoteResource, startedAt time.Time, now time.Time, remaining time.Duration) BurnRateSnapshot {
	if now.Before(startedAt) {
		now = startedAt
	}
	elapsed := now.Sub(startedAt)
	snapshot := BurnRateSnapshot{
		RemoteID:        remote.ID,
		Provider:        remote.Provider,
		CostSource:      remote.CostSource,
		Elapsed:         elapsed,
		Remaining:       remaining,
		CurrentBurnRate: remote.HourlyCost,
	}
	if remote.HourlyCost <= 0 {
		return snapshot
	}
	snapshot.Spent = remote.HourlyCost * elapsed.Hours()
	snapshot.ProjectedTotal = snapshot.Spent + remote.HourlyCost*remaining.Hours()
	return snapshot
}
