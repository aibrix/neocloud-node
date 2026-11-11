package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
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
	ExternalName   string
	Provider       string
	Phase          RemotePhase
	Request        WorkloadRequest
	// Endpoints lists the published container ports, when the provider has
	// assigned an address.
	Endpoints  []Endpoint
	HourlyCost float64
	CostSource string
	// StatusReason and StatusMessage explain the phase when the provider
	// reports more than a phase, for example why a remote workload failed.
	StatusReason  string
	StatusMessage string
	// ContainerUptime is how long the container has run, or zero when
	// unknown. Some providers restart an exited container in place, so an
	// uptime lower than previously observed means the container exited.
	ContainerUptime time.Duration
}

type ProviderAdapter interface {
	Create(ctx context.Context, idempotencyKey string, request WorkloadRequest) (RemoteResource, error)
	Get(ctx context.Context, remoteID string) (RemoteResource, error)
	Delete(ctx context.Context, remoteID string) error
}

// RemoteStopper is implemented by providers that can stop a remote workload
// without deleting it. Stopping releases compute but keeps the disk.
type RemoteStopper interface {
	Stop(ctx context.Context, remoteID string) error
}

type FakeProvider struct {
	mu          sync.RWMutex
	provider    string
	sequence    int
	createCount int
	stopCount   int
	stopErr     error
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
		ExternalName:   ManagedExternalName(idempotencyKey),
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

// SetStatus changes the observed status of an existing fake remote resource.
func (p *FakeProvider) SetStatus(remoteID string, phase RemotePhase, reason string, message string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	remote, ok := p.byID[remoteID]
	if !ok {
		return ErrRemoteNotFound
	}
	remote.Phase = phase
	remote.StatusReason = reason
	remote.StatusMessage = message
	p.byID[remoteID] = remote
	p.byKey[remote.IdempotencyKey] = remote
	return nil
}

func (p *FakeProvider) ListRemoteResources(ctx context.Context) ([]RemoteResource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	remotes := make([]RemoteResource, 0, len(p.byID))
	for _, remote := range p.byID {
		remotes = append(remotes, remote)
	}
	return remotes, nil
}

// Stop stops a fake remote resource, like a provider that keeps the disk of a
// stopped workload.
func (p *FakeProvider) Stop(ctx context.Context, remoteID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.stopErr != nil {
		return p.stopErr
	}
	remote, ok := p.byID[remoteID]
	if !ok {
		return ErrRemoteNotFound
	}
	p.stopCount++
	remote.Phase = RemoteFailed
	remote.StatusReason = "RemoteStopped"
	remote.StatusMessage = "remote workload was stopped"
	p.byID[remoteID] = remote
	p.byKey[remote.IdempotencyKey] = remote
	return nil
}

// SetStopError makes Stop fail with err until it is set back to nil.
func (p *FakeProvider) SetStopError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.stopErr = err
}

// StopCount returns how many times Stop succeeded.
func (p *FakeProvider) StopCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.stopCount
}

// SetContainerUptime changes how long the fake container has run.
func (p *FakeProvider) SetContainerUptime(remoteID string, uptime time.Duration) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	remote, ok := p.byID[remoteID]
	if !ok {
		return ErrRemoteNotFound
	}
	remote.ContainerUptime = uptime
	p.byID[remoteID] = remote
	p.byKey[remote.IdempotencyKey] = remote
	return nil
}

func (p *FakeProvider) CreateCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()

	return p.createCount
}

// SetEndpoints changes the published endpoints of an existing fake remote resource.
func (p *FakeProvider) SetEndpoints(remoteID string, endpoints []Endpoint) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	remote, ok := p.byID[remoteID]
	if !ok {
		return ErrRemoteNotFound
	}
	remote.Endpoints = append([]Endpoint(nil), endpoints...)
	p.byID[remoteID] = remote
	p.byKey[remote.IdempotencyKey] = remote
	return nil
}
