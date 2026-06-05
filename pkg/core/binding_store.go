package core

import (
	"context"
	"sync"
)

type BindingStore interface {
	CreateOrGet(ctx context.Context, binding Binding) (Binding, bool, error)
	GetByWorkloadUID(ctx context.Context, namespace string, workloadUID string) (Binding, error)
	Update(ctx context.Context, binding Binding) error
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
		return existing, false, nil
	}
	if binding.Phase == "" {
		binding.Phase = BindingPending
	}
	s.bindings[key] = binding
	return binding, true, nil
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
	return binding, nil
}

func (s *MemoryBindingStore) Update(ctx context.Context, binding Binding) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := bindingKey(binding.Namespace, binding.WorkloadUID)
	if _, ok := s.bindings[key]; !ok {
		return ErrBindingNotFound
	}
	s.bindings[key] = binding
	return nil
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
	binding.Deleting = true
	binding.Phase = BindingDeleting
	s.bindings[key] = binding
	return binding, nil
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
