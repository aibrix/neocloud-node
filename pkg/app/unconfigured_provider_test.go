package app

import (
	"context"
	"errors"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
)

func TestOptionalCredentialsKeepControllerConfigValid(t *testing.T) {
	for _, provider := range []string{ProviderRunPod, ProviderLambda, ProviderVast} {
		t.Run(provider, func(t *testing.T) {
			options, err := ParseOptions([]string{"--provider=" + provider, "--fallback-providers=runpod,lambda,vast"}, mapEnv(map[string]string{"GPU_BURST_ALLOW_MISSING_CREDENTIALS": "true"}))
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := newProviderAdapter(options)
			if err != nil {
				t.Fatal(err)
			}
			_, createErr := adapter.Create(context.Background(), "test", core.WorkloadRequest{})
			_, getErr := adapter.Get(context.Background(), "existing-remote")
			deleteErr := adapter.Delete(context.Background(), "existing-remote")
			_, listErr := adapter.(core.RemoteResourceLister).ListRemoteResources(context.Background())
			for _, err := range []error{createErr, getErr, deleteErr, listErr} {
				var providerErr *core.ProviderError
				if !errors.As(err, &providerErr) || providerErr.Code != "MissingCredentials" || providerErr.Class != core.ErrorClassAuth {
					t.Fatalf("expected explicit missing credential error, got %v", err)
				}
				if errors.Is(err, core.ErrRemoteNotFound) {
					t.Fatal("missing credentials must not imply remote deletion")
				}
			}
			config := &nodeutil.NodeConfig{NodeSpec: corev1.Node{}}
			if err := configureNodeSpec(config, options); err != nil {
				t.Fatal(err)
			}
			gpu := config.NodeSpec.Status.Allocatable[corev1.ResourceName(NvidiaGPUResource)]
			if gpu.Value() != 0 {
				t.Fatalf("unconfigured node advertises %s GPUs", gpu.String())
			}
			options.RunPodAPIKey = "test-key"
			options.LambdaAPIKey = "test-key"
			options.VastAPIKey = "test-key"
			configured, err := newProviderAdapter(options)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := configured.(unconfiguredProvider); ok {
				t.Fatal("configured provider remains disabled")
			}
			if err := configureNodeSpec(config, options); err != nil {
				t.Fatal(err)
			}
			gpu = config.NodeSpec.Status.Allocatable[corev1.ResourceName(NvidiaGPUResource)]
			if gpu.Value() != int64(options.AdvertiseGPUCount) {
				t.Fatal("configured provider must restore GPU capacity")
			}
		})
	}
}
