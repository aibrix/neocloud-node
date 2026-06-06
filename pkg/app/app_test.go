package app

import (
	"context"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestParseOptionsDefaultsToFakeProviderAndKubernetesStore(t *testing.T) {
	options, err := ParseOptions(nil, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	if options.NodeName != DefaultNodeName {
		t.Fatalf("NodeName = %q, want %q", options.NodeName, DefaultNodeName)
	}
	if options.Provider != ProviderFake {
		t.Fatalf("Provider = %q, want %q", options.Provider, ProviderFake)
	}
	if options.BindingStore != BindingStoreKubernetes {
		t.Fatalf("BindingStore = %q, want %q", options.BindingStore, BindingStoreKubernetes)
	}
	if options.KubeletAPIEnabled {
		t.Fatal("KubeletAPIEnabled = true, want false by default")
	}
}

func TestParseOptionsReadsFlagsAndEnvironment(t *testing.T) {
	env := mapEnv(map[string]string{
		"RUNPOD_API_KEY":            "secret-key",
		"GPU_BURST_NODE_NAME":       "env-node",
		"GPU_BURST_PROVIDER":        ProviderRunPod,
		"GPU_BURST_BINDING_STORE":   BindingStoreMemory,
		"GPU_BURST_RUNPOD_BASE_URL": "https://runpod.example/v1",
	})

	options, err := ParseOptions([]string{
		"--node-name=flag-node",
		"--provider=runpod",
		"--binding-store=memory",
		"--listen-addr=127.0.0.1:10443",
		"--max-gpu-count=8",
		"--max-ports=4",
	}, env)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	if options.NodeName != "flag-node" {
		t.Fatalf("NodeName = %q, want flag-node", options.NodeName)
	}
	if options.Provider != ProviderRunPod || options.RunPodAPIKey != "secret-key" {
		t.Fatalf("provider/key = %q/%q, want runpod/secret-key", options.Provider, options.RunPodAPIKey)
	}
	if options.BindingStore != BindingStoreMemory {
		t.Fatalf("BindingStore = %q, want memory", options.BindingStore)
	}
	if options.RunPodBaseURL != "https://runpod.example/v1" {
		t.Fatalf("RunPodBaseURL = %q", options.RunPodBaseURL)
	}
	if options.ListenAddr != "127.0.0.1:10443" {
		t.Fatalf("ListenAddr = %q, want 127.0.0.1:10443", options.ListenAddr)
	}
	if options.MaxGPUCount != 8 || options.MaxPorts != 4 {
		t.Fatalf("guard limits = gpu:%d ports:%d, want gpu:8 ports:4", options.MaxGPUCount, options.MaxPorts)
	}
}

func TestParseOptionsReadsOrphanSweeperOptions(t *testing.T) {
	env := mapEnv(map[string]string{
		"GPU_BURST_ORPHAN_SWEEPER_INTERVAL": "5m",
		"GPU_BURST_ORPHAN_SWEEPER_DRY_RUN":  "false",
	})

	options, err := ParseOptions([]string{
		"--orphan-sweeper-interval=30s",
		"--orphan-sweeper-dry-run=true",
	}, env)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	if options.OrphanSweeperInterval != 30*time.Second {
		t.Fatalf("OrphanSweeperInterval = %s, want 30s", options.OrphanSweeperInterval)
	}
	if !options.OrphanSweeperDryRun {
		t.Fatal("OrphanSweeperDryRun = false, want true flag override")
	}
}

func TestConfigureNodeSpecAdvertisesProviderTaintAndCapacity(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--advertise-gpu-count=64",
		"--advertise-pod-capacity=512",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	config := &nodeutil.NodeConfig{
		NodeSpec: corev1.Node{},
	}

	if err := configureNodeSpec(config, options); err != nil {
		t.Fatalf("configureNodeSpec() error = %v", err)
	}
	if config.NodeSpec.Labels[NodeLabelProvider] != ProviderFake {
		t.Fatalf("provider label = %q, want fake", config.NodeSpec.Labels[NodeLabelProvider])
	}
	if len(config.NodeSpec.Spec.Taints) != 1 || config.NodeSpec.Spec.Taints[0].Key != NodeTaintKey {
		t.Fatalf("taints = %#v, want provider taint", config.NodeSpec.Spec.Taints)
	}
	if got := config.NodeSpec.Status.Capacity[corev1.ResourceName("nvidia.com/gpu")]; got.Value() != 64 {
		t.Fatalf("gpu capacity = %s, want 64", got.String())
	}
	if got := config.NodeSpec.Status.Allocatable[corev1.ResourcePods]; got.Value() != 512 {
		t.Fatalf("pod allocatable = %s, want 512", got.String())
	}
}

func TestParseOptionsRejectsInvalidProviderAndStore(t *testing.T) {
	if _, err := ParseOptions([]string{"--provider=unknown"}, emptyEnv); err == nil {
		t.Fatal("ParseOptions() error = nil for unknown provider")
	}
	if _, err := ParseOptions([]string{"--binding-store=unknown"}, emptyEnv); err == nil {
		t.Fatal("ParseOptions() error = nil for unknown binding store")
	}
}

func TestParseOptionsRejectsNegativeOrphanSweeperInterval(t *testing.T) {
	if _, err := ParseOptions([]string{"--orphan-sweeper-interval=-1s"}, emptyEnv); err == nil {
		t.Fatal("ParseOptions() error = nil for negative orphan sweeper interval")
	}
}

func TestParseOptionsRequiresRunPodAPIKey(t *testing.T) {
	_, err := ParseOptions([]string{"--provider=runpod"}, emptyEnv)
	if err == nil {
		t.Fatal("ParseOptions() error = nil, want missing RunPod API key error")
	}
}

func TestParseOptionsReadsLambdaOptions(t *testing.T) {
	env := mapEnv(map[string]string{
		"LAMBDA_API_KEY":                 "lambda-key",
		"GPU_BURST_LAMBDA_BASE_URL":      "https://lambda.example/api/v1",
		"GPU_BURST_LAMBDA_CALLBACK_URL":  "https://controller.example/status",
		"GPU_BURST_LAMBDA_REGION":        "us-west-1",
		"GPU_BURST_LAMBDA_INSTANCE_TYPE": "gpu_1x_a10",
		"GPU_BURST_LAMBDA_SSH_KEY_NAMES": "key-a,key-b",
	})

	options, err := ParseOptions([]string{"--provider=lambda", "--binding-store=memory"}, env)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	if options.Provider != ProviderLambda || options.LambdaAPIKey != "lambda-key" {
		t.Fatalf("provider/key = %q/%q, want lambda/lambda-key", options.Provider, options.LambdaAPIKey)
	}
	if options.LambdaBaseURL != "https://lambda.example/api/v1" {
		t.Fatalf("LambdaBaseURL = %q", options.LambdaBaseURL)
	}
	if got := commaSeparated(options.LambdaSSHKeyNames); len(got) != 2 || got[0] != "key-a" || got[1] != "key-b" {
		t.Fatalf("lambda ssh key names = %#v, want key-a key-b", got)
	}
}

func TestParseOptionsRequiresLambdaAPIKey(t *testing.T) {
	_, err := ParseOptions([]string{"--provider=lambda"}, emptyEnv)
	if err == nil {
		t.Fatal("ParseOptions() error = nil, want missing Lambda API key error")
	}
}

func TestParseOptionsRequiresTLSFilesWhenKubeletAPIEnabled(t *testing.T) {
	_, err := ParseOptions([]string{"--enable-kubelet-api"}, emptyEnv)
	if err == nil {
		t.Fatal("ParseOptions() error = nil, want TLS cert/key error")
	}
}

func TestNewNodeBuildsVirtualKubeletNodeWithFakeProvider(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--binding-store=memory",
		"--node-name=test-gpu-burst",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}

	node, err := NewNode(context.Background(), options, kubefake.NewSimpleClientset(), dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
	if err != nil {
		t.Fatalf("NewNode() error = %v", err)
	}
	if node == nil {
		t.Fatal("NewNode() = nil, want node")
	}
}

func TestNewNodeRejectsOrphanSweeperWithoutProviderCleaner(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--binding-store=memory",
		"--orphan-sweeper-interval=1h",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}

	_, err = NewNode(context.Background(), options, kubefake.NewSimpleClientset(), dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
	if err == nil {
		t.Fatal("NewNode() error = nil, want unsupported orphan sweeper provider error")
	}
}

func TestNewNodeStartsOrphanSweeperForRunPodCleaner(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=runpod",
		"--binding-store=memory",
		"--orphan-sweeper-interval=1h",
	}, mapEnv(map[string]string{"RUNPOD_API_KEY": "test-key"}))
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	node, err := NewNode(ctx, options, kubefake.NewSimpleClientset(), dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
	if err != nil {
		t.Fatalf("NewNode() error = %v", err)
	}
	if node == nil {
		t.Fatal("NewNode() = nil, want node")
	}
}

func emptyEnv(string) string {
	return ""
}

func mapEnv(values map[string]string) func(string) string {
	return func(key string) string {
		return values[key]
	}
}
