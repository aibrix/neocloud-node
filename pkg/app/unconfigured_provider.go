package app

import (
	"context"
	"strings"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func missingCredential(options Options, provider string) bool {
	switch provider {
	case ProviderRunPod:
		return strings.TrimSpace(options.RunPodAPIKey) == ""
	case ProviderLambda:
		return strings.TrimSpace(options.LambdaAPIKey) == ""
	case ProviderVast:
		return strings.TrimSpace(options.VastAPIKey) == ""
	default:
		return false
	}
}

// unconfiguredProvider never sends unauthenticated requests or reports remote
// resources as deleted merely because their credentials are missing.
type unconfiguredProvider struct{ provider string }

func (p unconfiguredProvider) unavailable(operation string) error {
	return &core.ProviderError{
		Provider: p.provider, Operation: operation, Class: core.ErrorClassAuth,
		Code:    "MissingCredentials",
		Message: "provider credentials are not configured; create the referenced Secret and restart the controller",
	}
}

func (p unconfiguredProvider) Create(context.Context, string, core.WorkloadRequest) (core.RemoteResource, error) {
	return core.RemoteResource{}, p.unavailable("create")
}
func (p unconfiguredProvider) Get(context.Context, string) (core.RemoteResource, error) {
	return core.RemoteResource{}, p.unavailable("get")
}
func (p unconfiguredProvider) Delete(context.Context, string) error {
	return p.unavailable("delete")
}
func (p unconfiguredProvider) ListRemoteResources(context.Context) ([]core.RemoteResource, error) {
	return nil, p.unavailable("list")
}
