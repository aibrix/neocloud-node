package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/aibrix/neocloud-node/pkg/core"
	burstresource "github.com/aibrix/neocloud-node/pkg/resource"
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

func TestParseOptionsReadsFallbackProviders(t *testing.T) {
	env := mapEnv(map[string]string{
		"RUNPOD_API_KEY":                 "runpod-key",
		"LAMBDA_API_KEY":                 "lambda-key",
		"GPU_BURST_PROVIDER":             ProviderRunPod,
		"GPU_BURST_FALLBACK_PROVIDERS":   " Lambda , fake ",
		"GPU_BURST_LAMBDA_REGION":        "us-west-1",
		"GPU_BURST_LAMBDA_INSTANCE_TYPE": "gpu_1x_a10",
		"GPU_BURST_LAMBDA_SSH_KEY_NAMES": "lambda-key-name",
	})

	options, err := ParseOptions([]string{"--binding-store=memory"}, env)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	if options.FallbackProviders != "lambda,fake" {
		t.Fatalf("FallbackProviders = %q, want normalized lambda,fake", options.FallbackProviders)
	}
}

func TestParseOptionsRequiresFallbackProviderCredentials(t *testing.T) {
	_, err := ParseOptions([]string{
		"--provider=runpod",
		"--runpod-api-key=runpod-key",
		"--fallback-providers=lambda",
	}, emptyEnv)
	if err == nil {
		t.Fatal("ParseOptions() error = nil, want missing lambda fallback credential error")
	}
}

func TestNewFallbackProviderAdaptersBuildsPrimaryAndFallback(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=runpod",
		"--runpod-api-key=runpod-key",
		"--lambda-api-key=lambda-key",
		"--fallback-providers=lambda",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}

	adapters, order, err := newFallbackProviderAdapters(options)
	if err != nil {
		t.Fatalf("newFallbackProviderAdapters() error = %v", err)
	}
	if len(order) != 1 || order[0] != ProviderLambda {
		t.Fatalf("fallback order = %#v, want lambda", order)
	}
	if adapters[ProviderRunPod] == nil || adapters[ProviderLambda] == nil {
		t.Fatalf("adapters = %#v, want runpod and lambda adapters", adapters)
	}
}

func TestParseOptionsReadsOrphanSweeperOptions(t *testing.T) {
	env := mapEnv(map[string]string{
		"GPU_BURST_ORPHAN_SWEEPER_INTERVAL": "5m",
		"GPU_BURST_ORPHAN_SWEEPER_DRY_RUN":  "false",
		"GPU_BURST_STATUS_SYNC_INTERVAL":    "20s",
	})

	options, err := ParseOptions([]string{
		"--orphan-sweeper-interval=30s",
		"--orphan-sweeper-dry-run=true",
		"--status-sync-interval=3s",
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
	if options.StatusSyncInterval != 3*time.Second {
		t.Fatalf("StatusSyncInterval = %s, want 3s", options.StatusSyncInterval)
	}
}

func TestParseOptionsReadsPlacementOptions(t *testing.T) {
	env := mapEnv(map[string]string{
		"GPU_BURST_PLACEMENT_OFFERS":           "provider=runpod,location=US-TX-3,gpu=A40,count=1,market=on-demand,price=0.74",
		"GPU_BURST_PLACEMENT_OFFER_TTL":        "30m",
		"GPU_BURST_PLACEMENT_REFRESH_INTERVAL": "45s",
	})

	options, err := ParseOptions([]string{
		"--placement-signal-cooldown=2m",
	}, env)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	if options.PlacementOffers == "" {
		t.Fatal("PlacementOffers is empty, want env value")
	}
	if options.PlacementOfferTTL != 30*time.Minute {
		t.Fatalf("PlacementOfferTTL = %s, want 30m", options.PlacementOfferTTL)
	}
	if options.PlacementSignalCooldown != 2*time.Minute {
		t.Fatalf("PlacementSignalCooldown = %s, want flag override", options.PlacementSignalCooldown)
	}
	if options.PlacementRefreshInterval != 45*time.Second {
		t.Fatalf("PlacementRefreshInterval = %s, want 45s", options.PlacementRefreshInterval)
	}
}

func TestParsePlacementOffers(t *testing.T) {
	offers, err := parsePlacementOffers("provider=runpod,location=US-TX-3,gpu=A40,count=1,market=on-demand,price=0.74; provider=lambda,location=us-west-1,gpu=A10,count=1,market=spot,price=1.10,available=false,capabilities=cloud-init|public-ip")
	if err != nil {
		t.Fatalf("parsePlacementOffers() error = %v", err)
	}
	if len(offers) != 2 {
		t.Fatalf("offers = %#v, want two", offers)
	}
	if offers[0].Provider != "runpod" || offers[0].Location != "US-TX-3" || offers[0].GPUType != "A40" || offers[0].GPUCount != 1 || offers[0].HourlyPrice != 0.74 || !offers[0].Available {
		t.Fatalf("first offer = %#v, want runpod A40", offers[0])
	}
	if offers[1].Market != burstresource.MarketSpot || offers[1].Available {
		t.Fatalf("second offer market/availability = %#v, want unavailable spot", offers[1])
	}
	if !offers[1].Capabilities[burstresource.CapabilityCloudInit] || !offers[1].Capabilities[burstresource.CapabilityPublicIP] {
		t.Fatalf("second offer capabilities = %#v, want cloud-init and public-ip", offers[1].Capabilities)
	}
}

func TestNewCandidatePlannerFromBootstrapOffers(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=runpod",
		"--runpod-api-key=runpod-key",
		"--fallback-providers=lambda",
		"--lambda-api-key=lambda-key",
		"--placement-offers=provider=runpod,location=US-TX-3,gpu=A40,count=1,market=on-demand,price=0.74;provider=lambda,location=us-west-1,gpu=A40,count=1,market=on-demand,price=0.90",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}

	planner, err := newCandidatePlanner(options, []string{ProviderLambda})
	if err != nil {
		t.Fatalf("newCandidatePlanner() error = %v", err)
	}
	if planner == nil {
		t.Fatal("newCandidatePlanner() = nil, want planner")
	}
	candidates, err := planner.CreateCandidates(context.Background(), core.Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-123",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "A40",
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   ProviderRunPod,
		},
	})
	if err != nil {
		t.Fatalf("CreateCandidates() error = %v", err)
	}
	if len(candidates) != 2 || candidates[0].Provider != ProviderRunPod || candidates[1].Provider != ProviderLambda {
		t.Fatalf("candidates = %#v, want runpod then lambda", candidates)
	}
}

func TestNewPlacementPlannerIncludesRemoteResourceSources(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--binding-store=memory",
		"--placement-refresh-interval=1m",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	adapter := core.NewFakeProvider(ProviderFake)
	remote, err := adapter.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Provider:   ProviderFake,
		RegionHint: "observed",
		GPUType:    "A10",
		GPUCount:   1,
		MarketType: core.MarketTypeOnDemand,
	})
	if err != nil {
		t.Fatalf("fake Create() error = %v", err)
	}
	if remote.ID == "" {
		t.Fatal("remote ID is empty")
	}

	setup, err := newPlacementPlannerSetup(options, []string{}, map[string]core.ProviderAdapter{ProviderFake: adapter})
	if err != nil {
		t.Fatalf("newPlacementPlannerSetup() error = %v", err)
	}
	if setup.Planner == nil || setup.Refresher == nil {
		t.Fatalf("setup = %#v, want planner and refresher", setup)
	}
	report := setup.Refresher.Refresh(context.Background())
	if report.Recorded == 0 {
		t.Fatalf("refresh report = %#v, want remote resource observations", report)
	}

	candidates, err := setup.Planner.CreateCandidates(context.Background(), core.Binding{
		Namespace:   "default",
		Name:        "train-job",
		WorkloadUID: "pod-456",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "A10",
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   ProviderFake,
		},
	})
	if err != nil {
		t.Fatalf("CreateCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Request.RegionHint != "observed" {
		t.Fatalf("candidates = %#v, want observed remote-source candidate", candidates)
	}
}

func TestNewPlacementPlannerDisabledWithoutBootstrapOrRefresh(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--binding-store=memory",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	adapter := core.NewFakeProvider(ProviderFake)
	if _, err := adapter.Create(context.Background(), "default/pod-123", core.WorkloadRequest{
		Provider:   ProviderFake,
		RegionHint: "observed",
		GPUType:    "A10",
		GPUCount:   1,
		MarketType: core.MarketTypeOnDemand,
	}); err != nil {
		t.Fatalf("fake Create() error = %v", err)
	}

	setup, err := newPlacementPlannerSetup(options, []string{}, map[string]core.ProviderAdapter{ProviderFake: adapter})
	if err != nil {
		t.Fatalf("newPlacementPlannerSetup() error = %v", err)
	}
	if setup.Planner != nil || setup.Refresher != nil {
		t.Fatalf("setup = %#v, want placement disabled without bootstrap offers or refresh", setup)
	}
}

func TestNewPlacementPlannerIncludesProviderCatalogSources(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--binding-store=memory",
		"--placement-refresh-interval=1m",
	}, emptyEnv)
	if err != nil {
		t.Fatalf("ParseOptions() error = %v", err)
	}
	adapter := catalogProviderAdapter{FakeProvider: core.NewFakeProvider(ProviderFake)}

	setup, err := newPlacementPlannerSetup(options, []string{}, map[string]core.ProviderAdapter{ProviderFake: adapter})
	if err != nil {
		t.Fatalf("newPlacementPlannerSetup() error = %v", err)
	}
	if setup.Planner == nil || setup.Refresher == nil {
		t.Fatalf("setup = %#v, want planner and refresher", setup)
	}
	report := setup.Refresher.Refresh(context.Background())
	if report.Recorded != 1 {
		t.Fatalf("refresh report = %#v, want one catalog offer", report)
	}
	candidates, err := setup.Planner.CreateCandidates(context.Background(), core.Binding{
		Namespace:   "default",
		Name:        "catalog-job",
		WorkloadUID: "pod-catalog",
		Request: core.WorkloadRequest{
			Image:      "example.com/train:latest",
			GPUCount:   1,
			GPUType:    "A10",
			MaxPrice:   "1.00",
			MarketType: core.MarketTypeOnDemand,
			Provider:   ProviderFake,
		},
	})
	if err != nil {
		t.Fatalf("CreateCandidates() error = %v", err)
	}
	if len(candidates) != 1 || candidates[0].Request.RegionHint != "catalog-region" {
		t.Fatalf("candidates = %#v, want catalog-region candidate", candidates)
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
	if _, err := ParseOptions([]string{"--placement-refresh-interval=-1s"}, emptyEnv); err == nil {
		t.Fatal("ParseOptions() error = nil for negative placement refresh interval")
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
		"LAMBDA_API_KEY":                   "lambda-key",
		"GPU_BURST_LAMBDA_BASE_URL":        "https://lambda.example/api/v1",
		"GPU_BURST_LAMBDA_CALLBACK_URL":    "https://controller.example/status",
		"GPU_BURST_RUNNER_CALLBACK_ADDR":   "127.0.0.1:19090",
		"GPU_BURST_RUNNER_CALLBACK_SECRET": "shared-secret",
		"GPU_BURST_LAMBDA_REGION":          "us-west-1",
		"GPU_BURST_LAMBDA_INSTANCE_TYPE":   "gpu_1x_a10",
		"GPU_BURST_LAMBDA_SSH_KEY_NAMES":   "key-a,key-b",
	})

	options, err := ParseOptions([]string{
		"--provider=lambda",
		"--binding-store=memory",
		"--runner-callback-addr=127.0.0.1:19091",
	}, env)
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
	if options.RunnerCallbackListenAddr != "127.0.0.1:19091" {
		t.Fatalf("RunnerCallbackListenAddr = %q, want flag override", options.RunnerCallbackListenAddr)
	}
	if options.RunnerCallbackSecret != "shared-secret" {
		t.Fatalf("RunnerCallbackSecret = %q, want env secret", options.RunnerCallbackSecret)
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

func TestNewNodeStartsOrphanSweeperForFakeCleaner(t *testing.T) {
	options, err := ParseOptions([]string{
		"--provider=fake",
		"--binding-store=memory",
		"--orphan-sweeper-interval=1h",
	}, emptyEnv)
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

func TestRunnerStatusHandlerUpdatesBinding(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, core.Binding{
		Namespace:   "default",
		Name:        "lambda-runner",
		WorkloadUID: "uid-lambda-runner",
		RemoteID:    "lambda-1",
		Phase:       core.BindingRunning,
		Request:     core.WorkloadRequest{Provider: ProviderLambda},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/gpu-burst-node/v1/runner/status",
		bytes.NewBufferString(`{"workloadID":"default/uid-lambda-runner","exitCode":0,"message":"ok"}`))
	recorder := httptest.NewRecorder()

	runnerStatusHandler(store, "").ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, want %d; body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	binding, err := store.GetByWorkloadUID(ctx, "default", "uid-lambda-runner")
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if binding.Phase != core.BindingSucceeded {
		t.Fatalf("phase = %s, want %s", binding.Phase, core.BindingSucceeded)
	}
}

func TestRunnerStatusHandlerRecordsRunningPhase(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, core.Binding{
		Namespace:   "default",
		Name:        "lambda-runner",
		WorkloadUID: "uid-lambda-running",
		RemoteID:    "lambda-1",
		Phase:       core.BindingPending,
		Request:     core.WorkloadRequest{Provider: ProviderLambda},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/gpu-burst-node/v1/runner/status",
		bytes.NewBufferString(`{"workloadID":"default/uid-lambda-running","phase":"Running","message":"runner started"}`))
	recorder := httptest.NewRecorder()

	runnerStatusHandler(store, "").ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, want %d; body=%s", recorder.Code, http.StatusNoContent, recorder.Body.String())
	}
	binding, err := store.GetByWorkloadUID(ctx, "default", "uid-lambda-running")
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if binding.Phase != core.BindingRunning {
		t.Fatalf("phase = %s, want %s", binding.Phase, core.BindingRunning)
	}
	condition := core.FindCondition(binding.Conditions, core.ConditionReady)
	if condition == nil || condition.Status != core.ConditionTrue || condition.Reason != "RunnerRunning" {
		t.Fatalf("ready condition = %#v, want RunnerRunning true", condition)
	}
}

func TestStartRunnerCallbackServerUpdatesBinding(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := core.NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, core.Binding{
		Namespace:   "default",
		Name:        "lambda-runner",
		WorkloadUID: "uid-callback-server",
		RemoteID:    "lambda-1",
		Phase:       core.BindingRunning,
		Request:     core.WorkloadRequest{Provider: ProviderLambda},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	addr, shutdown, err := startRunnerCallbackServer(ctx, "127.0.0.1:0", "", store)
	if err != nil {
		t.Fatalf("startRunnerCallbackServer() error = %v", err)
	}
	defer shutdown(context.Background())

	resp, err := http.Post("http://"+addr+"/gpu-burst-node/v1/runner/status", "application/json",
		bytes.NewBufferString(`{"workloadID":"default/uid-callback-server","exitCode":0,"message":"ok"}`))
	if err != nil {
		t.Fatalf("runner callback POST error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	binding, err := store.GetByWorkloadUID(ctx, "default", "uid-callback-server")
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if binding.Phase != core.BindingSucceeded {
		t.Fatalf("phase = %s, want %s", binding.Phase, core.BindingSucceeded)
	}
}

func TestRunnerStatusHandlerAcceptsSignedCallback(t *testing.T) {
	ctx := context.Background()
	store := core.NewMemoryBindingStore()
	_, _, err := store.CreateOrGet(ctx, core.Binding{
		Namespace:   "default",
		Name:        "lambda-runner",
		WorkloadUID: "uid-signed",
		RemoteID:    "lambda-1",
		Phase:       core.BindingRunning,
		Request:     core.WorkloadRequest{Provider: ProviderLambda},
	})
	if err != nil {
		t.Fatalf("CreateOrGet() error = %v", err)
	}

	body := []byte(`{"workloadID":"default/uid-signed","exitCode":0}`)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/gpu-burst-node/v1/runner/status", bytes.NewReader(body))
	req.Header.Set("X-GPU-Burst-Runner-Timestamp", timestamp)
	req.Header.Set("X-GPU-Burst-Runner-Signature", runnerCallbackSignature("shared-secret", timestamp, body))
	recorder := httptest.NewRecorder()

	runnerStatusHandler(store, "shared-secret").ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status code = %d, body = %q, want %d", recorder.Code, recorder.Body.String(), http.StatusNoContent)
	}
}

func TestRunnerStatusHandlerRejectsBadSignature(t *testing.T) {
	store := core.NewMemoryBindingStore()
	body := []byte(`{"workloadID":"default/uid-signed","exitCode":0}`)
	req := httptest.NewRequest(http.MethodPost, "/gpu-burst-node/v1/runner/status", bytes.NewReader(body))
	req.Header.Set("X-GPU-Burst-Runner-Timestamp", strconv.FormatInt(time.Now().Unix(), 10))
	req.Header.Set("X-GPU-Burst-Runner-Signature", "v1=bad")
	recorder := httptest.NewRecorder()

	runnerStatusHandler(store, "shared-secret").ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status code = %d, want %d", recorder.Code, http.StatusUnauthorized)
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

type catalogProviderAdapter struct {
	*core.FakeProvider
}

func (a catalogProviderAdapter) Provider() string {
	return ProviderFake
}

func (a catalogProviderAdapter) ListOffers(ctx context.Context, intent burstresource.Intent) ([]burstresource.Offer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []burstresource.Offer{
		{
			ID:          "fake-a10-catalog",
			Provider:    ProviderFake,
			Location:    "catalog-region",
			GPUType:     "A10",
			GPUCount:    1,
			Market:      burstresource.MarketOnDemand,
			HourlyPrice: 0.42,
			Available:   true,
		},
	}, nil
}
