package core

import (
	"context"
	"errors"
	"fmt"
)

type RemoteResourceLister interface {
	ListRemoteResources(ctx context.Context) ([]RemoteResource, error)
}

type RemoteResourceCleaner interface {
	RemoteResourceLister
	Delete(ctx context.Context, remoteID string) error
}

type OrphanSweeper struct {
	Store    BindingStore
	Provider RemoteResourceCleaner
	DryRun   bool
}

type OrphanSweepReport struct {
	ObservedRemoteResources int
	ActiveBindings          int
	Orphans                 []RemoteResource
	Deleted                 []RemoteResource
	DeleteErrors            []OrphanDeleteError
}

type OrphanDeleteError struct {
	RemoteID string
	Err      error
}

func (e OrphanDeleteError) Error() string {
	if e.Err == nil {
		return e.RemoteID
	}
	return fmt.Sprintf("%s: %v", e.RemoteID, e.Err)
}

func (s OrphanSweeper) Sweep(ctx context.Context) (OrphanSweepReport, error) {
	if err := ctx.Err(); err != nil {
		return OrphanSweepReport{}, err
	}
	if s.Store == nil {
		return OrphanSweepReport{}, errors.New("missing binding store")
	}
	if s.Provider == nil {
		return OrphanSweepReport{}, errors.New("missing provider")
	}

	bindings, err := s.Store.List(ctx)
	if err != nil {
		return OrphanSweepReport{}, err
	}
	remotes, err := s.Provider.ListRemoteResources(ctx)
	if err != nil {
		return OrphanSweepReport{}, err
	}

	liveRemoteIDs := make(map[string]struct{}, len(bindings))
	for _, binding := range bindings {
		if binding.RemoteID == "" {
			continue
		}
		if binding.Phase == BindingDeleted {
			continue
		}
		liveRemoteIDs[binding.RemoteID] = struct{}{}
	}

	report := OrphanSweepReport{
		ObservedRemoteResources: len(remotes),
		ActiveBindings:          len(liveRemoteIDs),
	}
	var joined error
	for _, remote := range remotes {
		if !isManagedRemote(remote) {
			continue
		}
		if _, ok := liveRemoteIDs[remote.ID]; ok {
			continue
		}
		report.Orphans = append(report.Orphans, remote)
		if s.DryRun {
			continue
		}
		if err := s.Provider.Delete(ctx, remote.ID); err != nil {
			deleteErr := OrphanDeleteError{RemoteID: remote.ID, Err: err}
			report.DeleteErrors = append(report.DeleteErrors, deleteErr)
			joined = errors.Join(joined, deleteErr)
			continue
		}
		report.Deleted = append(report.Deleted, remote)
	}
	return report, joined
}

func isManagedRemote(remote RemoteResource) bool {
	return remote.IdempotencyKey != "" || IsManagedExternalName(remote.ExternalName)
}
