package core

import (
	"context"
	"errors"
	"time"
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
			SetCondition(&binding, BindingCondition{
				Type:    ConditionUnsupportedSpec,
				Status:  ConditionTrue,
				Reason:  ClassifyError(err).String(),
				Message: err.Error(),
			})
			_ = s.store.Update(ctx, binding)
			return binding, err
		}
	}

	remote, err := s.provider.Create(ctx, idempotencyKey(binding), binding.Request)
	if err != nil {
		recordProviderFailure(&binding, binding.Request.Provider, binding.Request.RegionHint, "CreateFailed", err)
		_ = s.store.Update(ctx, binding)
		return binding, err
	}

	binding.RemoteID = remote.ID
	binding.Phase = bindingPhaseFromRemote(remote.Phase)
	binding.Placement.Provider = remote.Provider
	if binding.Placement.Provider == "" {
		binding.Placement.Provider = binding.Request.Provider
	}
	recordRemoteCost(&binding, remote)
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
			SetCondition(&binding, BindingCondition{
				Type:    ConditionRemoteDeleted,
				Status:  ConditionTrue,
				Reason:  string(ErrorClassRemoteNotFound),
				Message: "remote resource is missing; it may have been deleted outside the controller",
			})
			_ = s.store.Update(ctx, binding)
		}
		return binding, err
	}

	binding.Phase = bindingPhaseFromRemote(remote.Phase)
	recordRemoteCost(&binding, remote)
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
			recordDeleteFailure(&binding, err)
			_ = s.store.Update(ctx, binding)
			return err
		}
	}
	return s.store.CompleteDelete(ctx, namespace, workloadUID)
}

func recordProviderFailure(binding *Binding, provider string, region string, reason string, err error) {
	if binding == nil {
		return
	}
	class := ClassifyError(err)
	binding.Phase = BindingFailed
	binding.LastError = err.Error()
	binding.Attempts = append(binding.Attempts, Attempt{
		Provider:        provider,
		Region:          region,
		Class:           class,
		Reason:          reason,
		Message:         err.Error(),
		Retryable:       IsRetryableClass(class),
		FallbackAllowed: AllowsFallbackClass(class),
		Timestamp:       time.Now().UTC(),
	})
	SetCondition(binding, BindingCondition{
		Type:    conditionTypeForErrorClass(class),
		Status:  ConditionTrue,
		Reason:  string(class),
		Message: err.Error(),
	})
}

func recordDeleteFailure(binding *Binding, err error) {
	if binding == nil {
		return
	}
	class := ClassifyError(err)
	binding.LastError = err.Error()
	SetCondition(binding, BindingCondition{
		Type:    ConditionDeleteFailure,
		Status:  ConditionTrue,
		Reason:  string(class),
		Message: err.Error(),
	})
}

func conditionTypeForErrorClass(class ErrorClass) ConditionType {
	switch class {
	case ErrorClassCapacity:
		return ConditionCapacity
	case ErrorClassAuth:
		return ConditionAuth
	default:
		return ConditionProviderError
	}
}

func recordRemoteCost(binding *Binding, remote RemoteResource) {
	if binding == nil {
		return
	}
	if remote.HourlyCost <= 0 && remote.CostSource == "" {
		return
	}
	binding.Cost = CostStatus{
		HourlyUSD:  remote.HourlyCost,
		Source:     remote.CostSource,
		ObservedAt: time.Now().UTC(),
	}
}

func (class ErrorClass) String() string {
	if class == "" {
		return string(ErrorClassUnknown)
	}
	return string(class)
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
