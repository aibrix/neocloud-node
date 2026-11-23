package core

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"sync"
)

type BindingStore interface {
	CreateOrGet(ctx context.Context, binding Binding) (Binding, bool, error)
	GetByWorkloadUID(ctx context.Context, namespace string, workloadUID string) (Binding, error)
	List(ctx context.Context) ([]Binding, error)
	// Update returns the persisted snapshot and rejects stale versions with
	// ErrBindingConflict. Callers must reload and re-evaluate before retrying.
	Update(ctx context.Context, binding Binding) (Binding, error)
	MarkDeleting(ctx context.Context, namespace string, workloadUID string) (Binding, error)
	CompleteDelete(ctx context.Context, namespace string, workloadUID string) error
}

type MemoryBindingStore struct {
	mu       sync.RWMutex
	bindings map[string]Binding
}

func NewMemoryBindingStore() *MemoryBindingStore {
	return &MemoryBindingStore{
		bindings: make(map[string]Binding),
	}
}

func (s *MemoryBindingStore) CreateOrGet(ctx context.Context, binding Binding) (Binding, bool, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, false, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := bindingKey(binding.Namespace, binding.WorkloadUID)
	if existing, ok := s.bindings[key]; ok {
		return cloneBinding(existing), false, nil
	}
	if binding.Phase == "" {
		binding.Phase = BindingPending
	}
	s.bindings[key] = cloneBinding(binding)
	return cloneBinding(binding), true, nil
}

func (s *MemoryBindingStore) GetByWorkloadUID(ctx context.Context, namespace string, workloadUID string) (Binding, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	binding, ok := s.bindings[bindingKey(namespace, workloadUID)]
	if !ok {
		return Binding{}, ErrBindingNotFound
	}
	return cloneBinding(binding), nil
}

func (s *MemoryBindingStore) List(ctx context.Context) ([]Binding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Binding, 0, len(s.bindings))
	for _, binding := range s.bindings {
		out = append(out, cloneBinding(binding))
	}
	return out, nil
}

func (s *MemoryBindingStore) Update(ctx context.Context, binding Binding) (Binding, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := bindingKey(binding.Namespace, binding.WorkloadUID)
	current, ok := s.bindings[key]
	if !ok {
		return Binding{}, ErrBindingNotFound
	}
	if binding.ResourceVersion != current.ResourceVersion {
		return Binding{}, ErrBindingConflict
	}
	binding.ResourceVersion = nextVersion(current.ResourceVersion)
	stored := cloneBinding(binding)
	s.bindings[key] = stored
	return cloneBinding(stored), nil
}

func (s *MemoryBindingStore) MarkDeleting(ctx context.Context, namespace string, workloadUID string) (Binding, error) {
	if err := ctx.Err(); err != nil {
		return Binding{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := bindingKey(namespace, workloadUID)
	binding, ok := s.bindings[key]
	if !ok {
		return Binding{}, ErrBindingNotFound
	}
	binding.ResourceVersion = nextVersion(binding.ResourceVersion)
	binding.Deleting = true
	binding.Phase = BindingDeleting
	s.bindings[key] = cloneBinding(binding)
	return cloneBinding(binding), nil
}

func (s *MemoryBindingStore) CompleteDelete(ctx context.Context, namespace string, workloadUID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.bindings, bindingKey(namespace, workloadUID))
	return nil
}

func bindingKey(namespace string, workloadUID string) string {
	return namespace + "/" + workloadUID
}

func nextVersion(version string) string {
	n, _ := strconv.ParseUint(version, 10, 64)
	return strconv.FormatUint(n+1, 10)
}

func cloneBinding(binding Binding) Binding {
	binding.Conditions = slices.Clone(binding.Conditions)
	binding.Attempts = slices.Clone(binding.Attempts)
	binding.Endpoints = slices.Clone(binding.Endpoints)
	binding.Request.Command = slices.Clone(binding.Request.Command)
	binding.Request.Args = slices.Clone(binding.Request.Args)
	binding.Request.Env = maps.Clone(binding.Request.Env)
	binding.Request.ProviderHints = maps.Clone(binding.Request.ProviderHints)
	binding.Request.EnvRefs = slices.Clone(binding.Request.EnvRefs)
	for i := range binding.Request.EnvRefs {
		ref := &binding.Request.EnvRefs[i]
		if ref.SecretKeyRef != nil {
			copied := *ref.SecretKeyRef
			ref.SecretKeyRef = &copied
		}
		if ref.ConfigMapKeyRef != nil {
			copied := *ref.ConfigMapKeyRef
			ref.ConfigMapKeyRef = &copied
		}
	}
	binding.Request.EnvFrom = slices.Clone(binding.Request.EnvFrom)
	binding.Request.Ports = slices.Clone(binding.Request.Ports)
	binding.Request.ImagePullSecrets = slices.Clone(binding.Request.ImagePullSecrets)
	return binding
}
