package app

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/aibrix/neocloud-node/pkg/core"
	resource "github.com/aibrix/neocloud-node/pkg/resource"
)

// credentialVersion is immutable after publication. A whole provider operation,
// including its recovery requests, uses this one client and credential.
type credentialVersion struct {
	key    string
	client core.ProviderAdapter
}

type credentialProvider struct {
	name     string
	build    func(string) core.ProviderAdapter
	mu       sync.RWMutex
	version  *credentialVersion
	reason   string
	verified map[string]*credentialVersion
	onChange func()
	createMu sync.Mutex
}

func newCredentialProvider(name string, build func(string) core.ProviderAdapter) *credentialProvider {
	return &credentialProvider{name: name, build: build, reason: "MissingCredentials", verified: make(map[string]*credentialVersion)}
}

func (p *credentialProvider) update(key, reason string) {
	key = strings.TrimSpace(key)
	if key == "" && reason == "" {
		reason = "MissingCredentials"
	}
	p.mu.Lock()
	if p.version != nil && p.version.key == key && reason == "" && (p.reason == "" || p.reason == "AuthenticationFailed") {
		p.mu.Unlock()
		return
	}
	if p.version == nil && key == "" && p.reason == reason {
		p.mu.Unlock()
		return
	}
	p.version = nil
	if key != "" {
		p.version = &credentialVersion{key: key, client: p.build(key)}
	}
	p.reason = reason
	p.verified = make(map[string]*credentialVersion)
	p.mu.Unlock()
	p.notify()
}

func (p *credentialProvider) available() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.version != nil && p.reason == ""
}

func (p *credentialProvider) notify() {
	if p.onChange != nil {
		p.onChange()
	}
}

func (p *credentialProvider) acquire(operation string) (*credentialVersion, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.version == nil || (p.reason != "" && (p.reason != "AuthenticationFailed" || operation == "create" || operation == "catalog")) {
		return nil, &core.ProviderError{Provider: p.name, Operation: operation, Class: core.ErrorClassAuth, Code: p.reason, Message: "provider credentials are unavailable; check the configured Secret and read permissions"}
	}
	return p.version, nil
}

func (p *credentialProvider) finish(v *credentialVersion, operation string, err error) error {
	if core.ClassifyError(err) == core.ErrorClassAuth {
		p.mu.Lock()
		changed := p.version == v && p.reason != "AuthenticationFailed"
		if changed {
			p.reason = "AuthenticationFailed"
		}
		p.mu.Unlock()
		if changed {
			p.notify()
		}
	}
	if err == nil {
		return nil
	}
	// Provider response bodies can echo submitted credentials. Never retain the
	// original error as a cause: Error(), logs and durable status must be safe.
	var providerErr *core.ProviderError
	safe := &core.ProviderError{Provider: p.name, Operation: operation, Class: core.ClassifyError(err), Message: "provider operation failed; check provider authentication, configuration and resource status"}
	if errors.As(err, &providerErr) {
		safe.StatusCode = providerErr.StatusCode
	}
	if errors.Is(err, core.ErrRemoteNotFound) {
		safe.Err = core.ErrRemoteNotFound
	}
	return safe
}

type reloadableProvider struct{ credentials *credentialProvider }
type reloadableRunPod struct{ *reloadableProvider }
type reloadableBatch struct{ *reloadableProvider }
type reloadableLambda struct{ *reloadableBatch }

func wrapCredentialProvider(p *credentialProvider) core.ProviderAdapter {
	base := &reloadableProvider{credentials: p}
	switch p.name {
	case ProviderRunPod:
		return &reloadableRunPod{base}
	case ProviderLambda:
		return &reloadableLambda{&reloadableBatch{base}}
	default:
		return &reloadableBatch{base}
	}
}

func (p *reloadableProvider) Provider() string { return p.credentials.name }

// FindByWorkloadKey is implemented by real adapters using their own external-name
// rules. Lookup before POST also recovers an allocation after a controller restart.
type workloadFinder interface {
	FindByWorkloadKey(context.Context, string) (core.RemoteResource, error)
}

func (p *reloadableProvider) Create(ctx context.Context, key string, request core.WorkloadRequest) (core.RemoteResource, error) {
	p.credentials.createMu.Lock()
	defer p.credentials.createMu.Unlock()
	v, err := p.credentials.acquire("create")
	if err != nil {
		return core.RemoteResource{}, err
	}
	if finder, ok := v.client.(workloadFinder); ok {
		remote, findErr := finder.FindByWorkloadKey(ctx, key)
		if findErr == nil {
			remote.IdempotencyKey = key
			remote.Request = request
			return remote, nil
		}
		if !errors.Is(findErr, core.ErrRemoteNotFound) {
			return core.RemoteResource{}, p.credentials.finish(v, "recover", findErr)
		}
	}
	remote, err := v.client.Create(ctx, key, request)
	return remote, p.credentials.finish(v, "create", err)
}

func (p *reloadableProvider) get(ctx context.Context, v *credentialVersion, id string) (core.RemoteResource, error) {
	remote, err := v.client.Get(ctx, id)
	p.credentials.mu.Lock()
	if err == nil && p.credentials.version == v {
		p.credentials.verified[id] = v
	}
	verified := p.credentials.verified[id] == v
	p.credentials.mu.Unlock()
	if errors.Is(err, core.ErrRemoteNotFound) && !verified {
		// A newly loaded key may refer to another account. Preserve the old binding
		// rather than accepting its first 404 as proof that a paid resource is gone.
		err = &core.ProviderError{Class: core.ErrorClassAuth, Code: "CredentialScopeUnverified", Message: "existing remote resource is not visible with the current credentials; verify the provider account before cleanup"}
		// Scope uncertainty does not prove the key itself is invalid.
		return core.RemoteResource{}, err
	}
	return remote, p.credentials.finish(v, "get", err)
}

func (p *reloadableProvider) Get(ctx context.Context, id string) (core.RemoteResource, error) {
	v, err := p.credentials.acquire("get")
	if err != nil {
		return core.RemoteResource{}, err
	}
	return p.get(ctx, v, id)
}

func (p *reloadableProvider) Delete(ctx context.Context, id string) error {
	v, err := p.credentials.acquire("delete")
	if err != nil {
		return err
	}
	// DELETE APIs often treat 404 as success internally, so verify scope first.
	if _, err := p.get(ctx, v, id); err != nil {
		if errors.Is(err, core.ErrRemoteNotFound) {
			return nil
		}
		return err
	}
	return p.credentials.finish(v, "delete", v.client.Delete(ctx, id))
}

func (p *reloadableProvider) ListRemoteResources(ctx context.Context) ([]core.RemoteResource, error) {
	v, err := p.credentials.acquire("list")
	if err != nil {
		return nil, err
	}
	remotes, err := v.client.(core.RemoteResourceLister).ListRemoteResources(ctx)
	return remotes, p.credentials.finish(v, "list", err)
}

func (p *reloadableBatch) ListRemoteStatuses(ctx context.Context) ([]core.RemoteResource, error) {
	// Do not use list results as authoritative observations across credential
	// generations. Per-resource Get verifies visibility before accepting a 404.
	v, err := p.credentials.acquire("list")
	if err != nil {
		return nil, err
	}
	remotes, err := v.client.(core.RemoteStatusLister).ListRemoteStatuses(ctx)
	if err != nil {
		return nil, p.credentials.finish(v, "list", err)
	}
	p.credentials.mu.RLock()
	current := p.credentials.version == v
	p.credentials.mu.RUnlock()
	if !current {
		return nil, &core.ProviderError{Class: core.ErrorClassTransient, Message: "provider credentials changed during status listing; retry"}
	}
	return remotes, nil
}

func (p *reloadableProvider) ListOffers(ctx context.Context, intent resource.Intent) ([]resource.Offer, error) {
	v, err := p.credentials.acquire("catalog")
	if err != nil {
		return nil, err
	}
	offers, err := v.client.(resource.ProviderOfferSource).ListOffers(ctx, intent)
	if err != nil {
		return nil, p.credentials.finish(v, "catalog", err)
	}
	p.credentials.mu.RLock()
	current := p.credentials.version == v && p.credentials.reason == ""
	p.credentials.mu.RUnlock()
	if !current {
		return nil, &core.ProviderError{Class: core.ErrorClassTransient, Message: "provider credentials changed during catalog query; retry"}
	}
	return offers, nil
}

func (p *reloadableRunPod) Stop(ctx context.Context, id string) error {
	v, err := p.credentials.acquire("stop")
	if err != nil {
		return err
	}
	if _, err := p.get(ctx, v, id); err != nil {
		return err
	}
	return p.credentials.finish(v, "stop", v.client.(core.RemoteStopper).Stop(ctx, id))
}

func (p *reloadableProvider) openLogs(ctx context.Context, target core.LogTarget, opts core.LogOptions) (io.ReadCloser, error) {
	v, err := p.credentials.acquire("logs")
	if err != nil {
		return nil, err
	}
	out, err := v.client.(core.ProviderLogReader).OpenLogs(ctx, target, opts)
	if err != nil {
		return nil, p.credentials.finish(v, "logs", err)
	}
	return &credentialLogReader{ReadCloser: out, credentials: p.credentials, version: v}, nil
}

func (p *reloadableRunPod) OpenLogs(ctx context.Context, target core.LogTarget, opts core.LogOptions) (io.ReadCloser, error) {
	return p.openLogs(ctx, target, opts)
}
func (p *reloadableLambda) OpenLogs(ctx context.Context, target core.LogTarget, opts core.LogOptions) (io.ReadCloser, error) {
	return p.openLogs(ctx, target, opts)
}

type credentialLogReader struct {
	io.ReadCloser
	credentials *credentialProvider
	version     *credentialVersion
}

func (r *credentialLogReader) Read(b []byte) (int, error) {
	n, err := r.ReadCloser.Read(b)
	if err != nil && err != io.EOF {
		err = r.credentials.finish(r.version, "logs", err)
	}
	return n, err
}
