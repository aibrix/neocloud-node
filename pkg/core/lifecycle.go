package core

import (
	"context"
	"errors"
	"strconv"
	"time"
)

type LifecycleService struct {
	store            BindingStore
	provider         ProviderAdapter
	providers        map[string]ProviderAdapter
	fallbackOrder    []string
	guard            *LaunchGuard
	candidatePlanner CreateCandidatePlanner
}

type LifecycleOption func(*LifecycleService)

type CreateCandidate struct {
	Provider string
	Request  WorkloadRequest
}

type CreateCandidatePlanner interface {
	CreateCandidates(ctx context.Context, binding Binding) ([]CreateCandidate, error)
}

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

func WithFallbackProviders(providers map[string]ProviderAdapter, fallbackOrder []string) LifecycleOption {
	return func(service *LifecycleService) {
		if len(providers) > 0 {
			service.providers = make(map[string]ProviderAdapter, len(providers))
			for name, provider := range providers {
				if name == "" || provider == nil {
					continue
				}
				service.providers[name] = provider
			}
		}
		service.fallbackOrder = append([]string(nil), fallbackOrder...)
	}
}

func WithCreateCandidatePlanner(planner CreateCandidatePlanner) LifecycleOption {
	return func(service *LifecycleService) {
		service.candidatePlanner = planner
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

	remote, request, err := s.createWithFallback(ctx, &binding)
	if err != nil {
		_ = s.store.Update(ctx, binding)
		return binding, err
	}

	binding.RemoteID = remote.ID
	binding.Phase = bindingPhaseFromRemote(remote.Phase)
	binding.Placement.Provider = remote.Provider
	if binding.Placement.Provider == "" {
		binding.Placement.Provider = request.Provider
	}
	binding.Placement.Region = request.RegionHint
	binding.LastError = ""
	recordCreateSuccess(&binding)
	recordRemoteCost(&binding, remote)
	if err := s.store.Update(ctx, binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func (s *LifecycleService) createWithFallback(ctx context.Context, binding *Binding) (RemoteResource, WorkloadRequest, error) {
	var lastErr error
	candidates, err := s.createCandidates(ctx, *binding)
	if err != nil {
		recordProviderFailure(binding, binding.Request.Provider, binding.Request.RegionHint, "PlacementFailed", err)
		return RemoteResource{}, binding.Request, err
	}
	for index, candidate := range candidates {
		adapter := s.providerFor(candidate.Provider)
		if adapter == nil {
			lastErr = &ProviderError{
				Provider:  candidate.Provider,
				Operation: "create",
				Class:     ErrorClassUnsupportedSpec,
				Message:   "provider adapter is not configured",
			}
			recordProviderFailure(binding, candidate.Provider, candidate.Request.RegionHint, "CreateFailed", lastErr)
			return RemoteResource{}, candidate.Request, lastErr
		}
		remote, err := adapter.Create(ctx, idempotencyKey(*binding), candidate.Request)
		if err == nil {
			return remote, candidate.Request, nil
		}
		lastErr = err
		recordProviderFailure(binding, candidate.Provider, candidate.Request.RegionHint, "CreateFailed", err)
		if index == len(candidates)-1 || !AllowsFallback(err) {
			return RemoteResource{}, candidate.Request, err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no provider candidate configured")
	}
	return RemoteResource{}, binding.Request, lastErr
}

func (s *LifecycleService) SyncStatus(ctx context.Context, namespace string, workloadUID string) (Binding, error) {
	binding, err := s.store.GetByWorkloadUID(ctx, namespace, workloadUID)
	if err != nil {
		return Binding{}, err
	}
	if binding.RemoteID == "" {
		return binding, nil
	}
	if isTerminalBindingPhase(binding.Phase) {
		return binding, nil
	}

	provider := s.providerForBinding(binding)
	if provider == nil {
		err := &ProviderError{
			Provider:  selectedProvider(binding),
			Operation: "get",
			Class:     ErrorClassUnsupportedSpec,
			Message:   "provider adapter is not configured",
		}
		return binding, err
	}
	remote, err := provider.Get(ctx, binding.RemoteID)
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
		provider := s.providerForBinding(binding)
		if provider == nil {
			err := &ProviderError{
				Provider:  selectedProvider(binding),
				Operation: "delete",
				Class:     ErrorClassUnsupportedSpec,
				Message:   "provider adapter is not configured",
			}
			recordDeleteFailure(&binding, err)
			_ = s.store.Update(ctx, binding)
			return err
		}
		if err := provider.Delete(ctx, binding.RemoteID); err != nil && !errors.Is(err, ErrRemoteNotFound) {
			recordDeleteFailure(&binding, err)
			_ = s.store.Update(ctx, binding)
			return err
		}
	}
	return s.store.CompleteDelete(ctx, namespace, workloadUID)
}

func RecordRunnerStatus(ctx context.Context, store BindingStore, namespace string, workloadUID string, status RunnerStatus) (Binding, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, err
	}
	if store == nil {
		return Binding{}, errors.New("missing binding store")
	}
	binding, err := store.GetByWorkloadUID(ctx, namespace, workloadUID)
	if err != nil {
		return Binding{}, err
	}
	phase := status.Phase
	if phase == "" {
		phase = runnerPhaseFromExitCode(status.ExitCode)
	}
	switch phase {
	case RunnerPhaseRunning:
		binding.Phase = BindingRunning
		binding.LastError = ""
		SetCondition(&binding, BindingCondition{
			Type:    ConditionReady,
			Status:  ConditionTrue,
			Reason:  defaultString(status.Reason, "RunnerRunning"),
			Message: defaultString(status.Message, "runner is running"),
		})
	case RunnerPhaseSucceeded:
		binding.Phase = BindingSucceeded
		binding.LastError = ""
		SetCondition(&binding, BindingCondition{
			Type:    ConditionReady,
			Status:  ConditionTrue,
			Reason:  defaultString(status.Reason, "RunnerSucceeded"),
			Message: defaultString(status.Message, "runner completed successfully"),
		})
		clearCreateFailureCondition(&binding, ConditionProviderError)
	case RunnerPhaseFailed:
		binding.Phase = BindingFailed
		binding.LastError = runnerFailureMessage(status.ExitCode, status.Message)
		SetCondition(&binding, BindingCondition{
			Type:    ConditionProviderError,
			Status:  ConditionTrue,
			Reason:  defaultString(status.Reason, "RunnerExitCode"),
			Message: binding.LastError,
		})
	default:
		return Binding{}, &ProviderError{
			Provider:  binding.Request.Provider,
			Operation: "runner-status",
			Class:     ErrorClassUnsupportedSpec,
			Message:   "unsupported runner phase " + string(phase),
		}
	}
	if err := store.Update(ctx, binding); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func runnerPhaseFromExitCode(exitCode int) RunnerPhase {
	if exitCode == 0 {
		return RunnerPhaseSucceeded
	}
	return RunnerPhaseFailed
}

func runnerFailureMessage(exitCode int, message string) string {
	if message != "" {
		return message
	}
	return "runner exited with code " + strconv.Itoa(exitCode)
}

func defaultString(value string, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
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

func recordCreateSuccess(binding *Binding) {
	if binding == nil {
		return
	}
	SetCondition(binding, BindingCondition{
		Type:    ConditionReady,
		Status:  ConditionTrue,
		Reason:  "RemoteCreated",
		Message: "remote resource created",
	})
	for _, conditionType := range []ConditionType{
		ConditionUnsupportedSpec,
		ConditionAuth,
		ConditionCapacity,
		ConditionProviderError,
	} {
		clearCreateFailureCondition(binding, conditionType)
	}
}

func clearCreateFailureCondition(binding *Binding, conditionType ConditionType) {
	condition := FindCondition(binding.Conditions, conditionType)
	if condition == nil || condition.Status != ConditionTrue {
		return
	}
	SetCondition(binding, BindingCondition{
		Type:    conditionType,
		Status:  ConditionFalse,
		Reason:  "RemoteCreated",
		Message: "condition cleared after successful remote create",
	})
}

func (s *LifecycleService) createCandidates(ctx context.Context, binding Binding) ([]CreateCandidate, error) {
	if s.candidatePlanner != nil {
		return s.candidatePlanner.CreateCandidates(ctx, binding)
	}

	providers := make([]string, 0, 1+len(s.fallbackOrder))
	seen := map[string]bool{}
	add := func(provider string) {
		if seen[provider] {
			return
		}
		seen[provider] = true
		providers = append(providers, provider)
	}
	add(binding.Request.Provider)
	for _, provider := range s.fallbackOrder {
		add(provider)
	}
	if len(providers) == 0 {
		providers = append(providers, "")
	}

	candidates := make([]CreateCandidate, 0, len(providers))
	for _, provider := range providers {
		request := binding.Request
		if provider != "" {
			request.Provider = provider
		}
		candidates = append(candidates, CreateCandidate{
			Provider: provider,
			Request:  request,
		})
	}
	return candidates, nil
}

func (s *LifecycleService) providerForBinding(binding Binding) ProviderAdapter {
	return s.providerFor(selectedProvider(binding))
}

func (s *LifecycleService) providerFor(provider string) ProviderAdapter {
	if provider != "" && len(s.providers) > 0 {
		if adapter := s.providers[provider]; adapter != nil {
			return adapter
		}
		return nil
	}
	return s.provider
}

func selectedProvider(binding Binding) string {
	if binding.Placement.Provider != "" {
		return binding.Placement.Provider
	}
	return binding.Request.Provider
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
		return BindingSucceeded
	case RemoteFailed:
		return BindingFailed
	case RemoteDeleted:
		return BindingDeleted
	default:
		return BindingPending
	}
}

func isTerminalBindingPhase(phase BindingPhase) bool {
	switch phase {
	case BindingSucceeded, BindingFailed, BindingDeleted:
		return true
	default:
		return false
	}
}
