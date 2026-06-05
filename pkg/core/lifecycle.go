package core

import (
	"context"
	"errors"
)

type LifecycleService struct {
	store    BindingStore
	provider ProviderAdapter
	guard    *LaunchGuard
}

type LifecycleOption func(*LifecycleService)

func NewLifecycleService(store BindingStore, provider ProviderAdapter, options ...LifecycleOption) *LifecycleService {
	service := &LifecycleService{
		store:    store,
		provider: provider,
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func WithLaunchGuard(guard LaunchGuard) LifecycleOption {
	return func(service *LifecycleService) {
		service.guard = &guard
	}
}

func (s *LifecycleService) EnsureCreated(ctx context.Context, desired Binding) (Binding, error) {
	binding, _, err := s.store.CreateOrGet(ctx, desired)
	if err != nil {
		return Binding{}, err
	}
	if binding.RemoteID != "" {
		return binding, nil
	}
	if s.guard != nil {
		if err := s.guard.Validate(binding.Request); err != nil {
			binding.Phase = BindingFailed
			binding.LastError = err.Error()
			_ = s.store.Update(ctx, binding)
			return binding, err
		}
	}

	remote, err := s.provider.Create(ctx, idempotencyKey(binding), binding.Request)
	if err != nil {
		binding.Phase = BindingFailed
		binding.LastError = err.Error()
		_ = s.store.Update(ctx, binding)
		return binding, err
	}

	binding.RemoteID = remote.ID
	binding.Phase = bindingPhaseFromRemote(remote.Phase)
	binding.Placement.Provider = remote.Provider
	if binding.Placement.Provider == "" {
		binding.Placement.Provider = binding.Request.Provider
	}
	if err := s.store.Update(ctx, binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func (s *LifecycleService) SyncStatus(ctx context.Context, namespace string, workloadUID string) (Binding, error) {
	binding, err := s.store.GetByWorkloadUID(ctx, namespace, workloadUID)
	if err != nil {
		return Binding{}, err
	}
	if binding.RemoteID == "" {
		return binding, nil
	}

	remote, err := s.provider.Get(ctx, binding.RemoteID)
	if err != nil {
		if errors.Is(err, ErrRemoteNotFound) {
			binding.Phase = BindingFailed
			binding.LastError = ErrRemoteNotFound.Error()
			_ = s.store.Update(ctx, binding)
		}
		return binding, err
	}

	binding.Phase = bindingPhaseFromRemote(remote.Phase)
	if err := s.store.Update(ctx, binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func (s *LifecycleService) EnsureDeleted(ctx context.Context, namespace string, workloadUID string) error {
	binding, err := s.store.MarkDeleting(ctx, namespace, workloadUID)
	if err != nil {
		if errors.Is(err, ErrBindingNotFound) {
			return nil
		}
		return err
	}

	if binding.RemoteID != "" {
		if err := s.provider.Delete(ctx, binding.RemoteID); err != nil && !errors.Is(err, ErrRemoteNotFound) {
			return err
		}
	}
	return s.store.CompleteDelete(ctx, namespace, workloadUID)
}

func idempotencyKey(binding Binding) string {
	return binding.Namespace + "/" + binding.WorkloadUID
}

func bindingPhaseFromRemote(phase RemotePhase) BindingPhase {
	switch phase {
	case RemoteRunning:
		return BindingRunning
	case RemoteSucceeded:
		return BindingDeleted
	case RemoteFailed:
		return BindingFailed
	case RemoteDeleted:
		return BindingDeleted
	default:
		return BindingPending
	}
}
