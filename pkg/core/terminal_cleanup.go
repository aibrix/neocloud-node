package core

import (
	"context"
	"errors"
	"time"
)

func WithTerminalCleanupDelay(delay time.Duration) LifecycleOption {
	return func(s *LifecycleService) { s.terminalCleanupDelay = delay }
}

// WithRemoteCleanupObserver runs after cleanup completion has been persisted.
func WithRemoteCleanupObserver(observer func(Binding)) LifecycleOption {
	return func(s *LifecycleService) { s.onRemoteCleanup = observer }
}

func recordTerminal(binding *Binding, now time.Time) {
	if binding.RemoteID == "" || !terminalCleanupPhase(binding.Phase) {
		return
	}
	condition := FindCondition(binding.Conditions, ConditionTerminal)
	if condition != nil && condition.Status == ConditionTrue && !condition.LastTransitionTime.IsZero() {
		return
	}
	SetCondition(binding, BindingCondition{
		Type: ConditionTerminal, Status: ConditionTrue,
		Reason: string(binding.Phase), LastTransitionTime: now.UTC(),
	})
}

func RemoteCleanedUp(binding Binding) bool {
	condition := FindCondition(binding.Conditions, ConditionRemoteCleanedUp)
	return condition != nil && condition.Status == ConditionTrue
}

// TerminalCleanupDue reports whether a terminal binding needs reconciliation.
// Bindings awaiting deletion confirmation stay due so the provider can be polled.
func (s *LifecycleService) TerminalCleanupDue(binding Binding) bool {
	if s.terminalCleanupDelay <= 0 || binding.Deleting || binding.RemoteID == "" ||
		RemoteCleanedUp(binding) || !terminalCleanupPhase(binding.Phase) {
		return false
	}
	cleanup := FindCondition(binding.Conditions, ConditionRemoteCleanedUp)
	if cleanup != nil && cleanup.Reason == "DeletionRequested" {
		return true
	}
	terminal := FindCondition(binding.Conditions, ConditionTerminal)
	return terminal == nil || terminal.Status != ConditionTrue || terminal.LastTransitionTime.IsZero() ||
		!s.now().Before(terminal.LastTransitionTime.Add(s.terminalCleanupDelay))
}

func terminalCleanupPhase(phase BindingPhase) bool {
	return phase == BindingSucceeded || phase == BindingFailed
}

func (s *LifecycleService) persistBinding(ctx context.Context, binding *Binding) error {
	recordTerminal(binding, s.now())
	updated, err := s.store.Update(ctx, *binding)
	if err != nil {
		return err
	}
	*binding = updated
	return nil
}

func (s *LifecycleService) cleanupTerminal(ctx context.Context, binding Binding) (Binding, error) {
	if !s.TerminalCleanupDue(binding) {
		return binding, nil
	}
	terminal := FindCondition(binding.Conditions, ConditionTerminal)
	if terminal == nil || terminal.Status != ConditionTrue || terminal.LastTransitionTime.IsZero() {
		// Legacy bindings have no trustworthy finish time; give them the full delay.
		err := s.persistBinding(ctx, &binding)
		return binding, err
	}
	provider := s.providerForBinding(binding)
	if provider == nil {
		return binding, &ProviderError{Provider: selectedProvider(binding), Operation: "delete",
			Class: ErrorClassUnsupportedSpec, Message: "provider adapter is not configured"}
	}
	cleanup := FindCondition(binding.Conditions, ConditionRemoteCleanedUp)
	if cleanup == nil || cleanup.Reason != "DeletionRequested" {
		if err := provider.Delete(ctx, binding.RemoteID); err != nil && !errors.Is(err, ErrRemoteNotFound) {
			return binding, err
		}
		SetCondition(&binding, BindingCondition{Type: ConditionRemoteCleanedUp, Status: ConditionFalse,
			Reason: "DeletionRequested", Message: "remote deletion requested; awaiting confirmation", LastTransitionTime: s.now()})
		if err := s.persistBinding(ctx, &binding); err != nil {
			return binding, err
		}
	}
	remote, err := provider.Get(ctx, binding.RemoteID)
	if err != nil && !errors.Is(err, ErrRemoteNotFound) {
		return binding, err
	}
	if err == nil && remote.Phase != RemoteDeleted {
		return binding, nil
	}
	SetCondition(&binding, BindingCondition{Type: ConditionRemoteCleanedUp, Status: ConditionTrue,
		Reason: "RemoteCleanedUp", Message: "remote resource cleaned up", LastTransitionTime: s.now()})
	err = s.persistBinding(ctx, &binding)
	if err == nil && s.onRemoteCleanup != nil {
		s.onRemoteCleanup(binding)
	}
	return binding, err
}
