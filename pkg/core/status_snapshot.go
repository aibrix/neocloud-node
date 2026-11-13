package core

import (
	"context"
	"fmt"
)

// RemoteStatusLister opts a provider into periodic batch status sync. Unlike the
// orphan-sweeper list, this list must contain every field its Get returns that
// affects binding and Pod status.
type RemoteStatusLister interface {
	ListRemoteStatuses(ctx context.Context) ([]RemoteResource, error)
}

// RemoteStatusSnapshot is one periodic tick's provider observations. A failed
// List is kept distinct from a missing ID so it cannot mark a Pod as deleted.
type RemoteStatusSnapshot struct {
	listed map[string]map[string]RemoteResource
	errors map[string]error
}

func (s *LifecycleService) SnapshotRemoteStatuses(ctx context.Context, bindings []Binding) RemoteStatusSnapshot {
	snapshot := RemoteStatusSnapshot{
		listed: make(map[string]map[string]RemoteResource),
		errors: make(map[string]error),
	}
	for _, binding := range bindings {
		if binding.RemoteID == "" || isTerminalBindingPhase(binding.Phase) {
			continue
		}
		name := selectedProvider(binding)
		if _, done := snapshot.listed[name]; done {
			continue
		}
		if _, failed := snapshot.errors[name]; failed {
			continue
		}
		provider := s.providerFor(name)
		lister, ok := provider.(RemoteStatusLister)
		if !ok {
			continue
		}
		remotes, err := lister.ListRemoteStatuses(ctx)
		if err != nil {
			snapshot.errors[name] = err
			continue
		}
		byID := make(map[string]RemoteResource, len(remotes))
		for _, remote := range remotes {
			if remote.ID != "" {
				byID[remote.ID] = remote
			}
		}
		snapshot.listed[name] = byID
	}
	return snapshot
}

func (s RemoteStatusSnapshot) Errors() map[string]error {
	return s.errors
}

func (s RemoteStatusSnapshot) lookup(ctx context.Context, providerName string, provider ProviderAdapter, remoteID string) (RemoteResource, error) {
	if err, failed := s.errors[providerName]; failed {
		return RemoteResource{}, fmt.Errorf("list remote statuses for %q failed: %v", providerName, err)
	}
	if remotes, listed := s.listed[providerName]; listed {
		if remote, found := remotes[remoteID]; found {
			return remote, nil
		}
	}
	return provider.Get(ctx, remoteID)
}
