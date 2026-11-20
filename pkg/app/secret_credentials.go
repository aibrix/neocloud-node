package app

import (
	"context"
	"sort"
	"time"

	"github.com/aibrix/neocloud-node/pkg/core"
	resource "github.com/aibrix/neocloud-node/pkg/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

type providerSecretRef struct{ Name, Key string }

type secretCredentials struct {
	options   Options
	client    kubernetes.Interface
	providers map[string]*credentialProvider
	changed   chan struct{}
}

func newSecretCredentials(options Options, client kubernetes.Interface) *secretCredentials {
	p := &secretCredentials{options: options, client: client, providers: make(map[string]*credentialProvider), changed: make(chan struct{}, 1)}
	for name, ref := range options.ProviderSecrets {
		if ref.Name == "" {
			continue
		}
		name := name
		credential := newCredentialProvider(name, func(key string) core.ProviderAdapter {
			configured := options
			configured.ProviderSecrets = nil
			configured.AllowMissingCredentials = false
			switch name {
			case ProviderRunPod:
				configured.RunPodAPIKey = key
			case ProviderLambda:
				configured.LambdaAPIKey = key
			case ProviderVast:
				configured.VastAPIKey = key
			}
			adapter, _ := providerAdapterFor(configured, name)
			return adapter
		})
		credential.onChange = func() {
			select {
			case p.changed <- struct{}{}:
			default:
			}
		}
		p.providers[name] = credential
	}
	return p
}

func (p *secretCredentials) adapter(name string) (core.ProviderAdapter, error) {
	if source := p.providers[name]; source != nil {
		return wrapCredentialProvider(source), nil
	}
	return providerAdapterFor(p.options, name)
}

// refresh reads only named references, once per distinct Secret. Errors from the
// Kubernetes API are deliberately not exposed: they may include sensitive data.
func (p *secretCredentials) refresh(ctx context.Context) {
	byName := make(map[string][]string)
	for provider, ref := range p.options.ProviderSecrets {
		if p.providers[provider] != nil {
			byName[ref.Name] = append(byName[ref.Name], provider)
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		timeout := p.options.SecretRequestTimeout
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		secret, err := p.client.CoreV1().Secrets(p.options.ProviderSecretNamespace).Get(requestCtx, name, metav1.GetOptions{})
		cancel()
		for _, provider := range byName[name] {
			key, reason := "", ""
			if err != nil {
				reason = "SecretUnavailable"
				if apierrors.IsNotFound(err) {
					reason = "MissingCredentials"
				}
			} else {
				key = string(secret.Data[p.options.ProviderSecrets[provider].Key])
			}
			p.providers[provider].update(key, reason)
		}
	}
}

func (p *secretCredentials) run(ctx context.Context, onChange func()) {
	ticker := time.NewTicker(p.options.SecretPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refresh(ctx)
		case <-p.changed:
			if onChange != nil {
				onChange()
			}
		}
	}
}

// credentialCatalog excludes cached/bootstrap offers while the provider is
// unavailable, including observations that finished after a Secret was removed.
type credentialCatalog struct {
	catalog     resource.OfferCatalog
	credentials *secretCredentials
}

func (c credentialCatalog) Snapshot(ctx context.Context, intent resource.Intent) (resource.CatalogSnapshot, error) {
	snapshot, err := c.catalog.Snapshot(ctx, intent)
	filtered := snapshot.Offers[:0]
	for _, offer := range snapshot.Offers {
		p := c.credentials.providers[offer.Provider]
		if p == nil || p.available() {
			filtered = append(filtered, offer)
		}
	}
	snapshot.Offers = filtered
	return snapshot, err
}
