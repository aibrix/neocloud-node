package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrRemoteNotFound = errors.New("remote resource not found")

type RemotePhase string

const (
	RemotePending   RemotePhase = "Pending"
	RemoteRunning   RemotePhase = "Running"
	RemoteSucceeded RemotePhase = "Succeeded"
	RemoteFailed    RemotePhase = "Failed"
	RemoteDeleted   RemotePhase = "Deleted"
)

type RemoteResource struct {
	ID             string
	IdempotencyKey string
	Provider       string
	Phase          RemotePhase
	Request        WorkloadRequest
}

type ProviderAdapter interface {
	Create(ctx context.Context, idempotencyKey string, request WorkloadRequest) (RemoteResource, error)
	Get(ctx context.Context, remoteID string) (RemoteResource, error)
	Delete(ctx context.Context, remoteID string) error
}

type FakeProvider struct {
	mu          sync.RWMutex
	provider    string
	sequence    int
	createCount int
	byKey       map[string]RemoteResource
	byID        map[string]RemoteResource
}

func NewFakeProvider(provider string) *FakeProvider {
	return &FakeProvider{
		provider: provider,
		byKey:    make(map[string]RemoteResource),
		byID:     make(map[string]RemoteResource),
	}
}

func (p *FakeProvider) Create(ctx context.Context, idempotencyKey string, request WorkloadRequest) (RemoteResource, error) {
	if err := ctx.Err(); err != nil {
		return RemoteResource{}, err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if existing, ok := p.byKey[idempotencyKey]; ok {
		return existing, nil
	}

	p.sequence++
	p.createCount++
	remote := RemoteResource{
		ID:             fmt.Sprintf("%s-%d", p.provider, p.sequence),
		IdempotencyKey: idempotencyKey,
		Provider:       p.provider,
		Phase:          RemoteRunning,
		Request:        request,
	}
	p.byKey[idempotencyKey] = remote
	p.byID[remote.ID] = remote
	return remote, nil
}

func (p *FakeProvider) Get(ctx context.Context, remoteID string) (RemoteResource, error) {
	if err := ctx.Err(); err != nil {
		return RemoteResource{}, err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	remote, ok := p.byID[remoteID]
	if !ok {
		return RemoteResource{}, ErrRemoteNotFound
	}
	return remote, nil
}

func (p *FakeProvider) Delete(ctx context.Context, remoteID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	remote, ok := p.byID[remoteID]
	if !ok {
		return nil
	}
	delete(p.byID, remoteID)
	delete(p.byKey, remote.IdempotencyKey)
	return nil
}

func (p *FakeProvider) CreateCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.createCount
}
