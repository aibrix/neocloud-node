package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
	lambdacloud "github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/providers/lambda"
	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/providers/runpod"
	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/runner"
	kstore "github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/store/kubernetes"
	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/vk"
)

const (
	DefaultNodeName = "aibrix-gpu-burst"

	ProviderFake   = "fake"
	ProviderRunPod = "runpod"
	ProviderLambda = "lambda"

	BindingStoreMemory     = "memory"
	BindingStoreKubernetes = "kubernetes"

	NodeLabelProvider = "gpu-burst-node.aibrix.ai/provider"
	NodeTaintKey      = "gpu-burst-node.aibrix.ai/provider"
	NvidiaGPUResource = "nvidia.com/gpu"
)

type Options struct {
	NodeName                 string
	Provider                 string
	FallbackProviders        string
	BindingStore             string
	Kubeconfig               string
	ListenAddr               string
	KubeletAPIEnabled        bool
	TLSCertFile              string
	TLSKeyFile               string
	RunnerCallbackListenAddr string
	RunnerCallbackSecret     string

	RunPodAPIKey  string
	RunPodBaseURL string

	LambdaAPIKey        string
	LambdaBaseURL       string
	LambdaCallbackURL   string
	LambdaDefaultRegion string
	LambdaInstanceType  string
	LambdaSSHKeyNames   string

	RequireMaxPrice bool
	MaxGPUCount     int
	MaxPorts        int

	AdvertiseGPUCount    int
	AdvertisePodCapacity int
	AdvertiseCPU         string
	AdvertiseMemory      string

	OrphanSweeperInterval time.Duration
	OrphanSweeperDryRun   bool

	StatusSyncInterval time.Duration
}

func ParseOptions(args []string, getenv func(string) string) (Options, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	options := Options{
		NodeName: envOr(getenv, "GPU_BURST_NODE_NAME", DefaultNodeName),
		Provider: envOr(getenv, "GPU_BURST_PROVIDER", ProviderFake),
		FallbackProviders: envOr(getenv,
			"GPU_BURST_FALLBACK_PROVIDERS", ""),
		BindingStore: envOr(getenv, "GPU_BURST_BINDING_STORE", BindingStoreKubernetes),
		Kubeconfig:   envOr(getenv, "KUBECONFIG", ""),
		ListenAddr:   envOr(getenv, "GPU_BURST_LISTEN_ADDR", ":10250"),
		TLSCertFile:  envOr(getenv, "GPU_BURST_TLS_CERT_FILE", ""),
		TLSKeyFile:   envOr(getenv, "GPU_BURST_TLS_KEY_FILE", ""),
		RunnerCallbackListenAddr: envOr(getenv,
			"GPU_BURST_RUNNER_CALLBACK_ADDR", ""),
		RunnerCallbackSecret: envOr(getenv,
			"GPU_BURST_RUNNER_CALLBACK_SECRET", ""),

		RunPodAPIKey:  envOr(getenv, "RUNPOD_API_KEY", ""),
		RunPodBaseURL: envOr(getenv, "GPU_BURST_RUNPOD_BASE_URL", ""),

		LambdaAPIKey:        envOr(getenv, "LAMBDA_API_KEY", ""),
		LambdaBaseURL:       envOr(getenv, "GPU_BURST_LAMBDA_BASE_URL", ""),
		LambdaCallbackURL:   envOr(getenv, "GPU_BURST_LAMBDA_CALLBACK_URL", ""),
		LambdaDefaultRegion: envOr(getenv, "GPU_BURST_LAMBDA_REGION", ""),
		LambdaInstanceType:  envOr(getenv, "GPU_BURST_LAMBDA_INSTANCE_TYPE", ""),
		LambdaSSHKeyNames:   envOr(getenv, "GPU_BURST_LAMBDA_SSH_KEY_NAMES", ""),

		KubeletAPIEnabled: boolEnv(getenv, "GPU_BURST_ENABLE_KUBELET_API", false),
		RequireMaxPrice:   boolEnv(getenv, "GPU_BURST_REQUIRE_MAX_PRICE", false),
		AdvertiseGPUCount: intEnv(getenv, "GPU_BURST_ADVERTISE_GPU_COUNT", 100),
		AdvertisePodCapacity: intEnv(getenv,
			"GPU_BURST_ADVERTISE_POD_CAPACITY", 1000),
		AdvertiseCPU:    envOr(getenv, "GPU_BURST_ADVERTISE_CPU", "1000"),
		AdvertiseMemory: envOr(getenv, "GPU_BURST_ADVERTISE_MEMORY", "1Ti"),

		OrphanSweeperInterval: durationEnv(getenv, "GPU_BURST_ORPHAN_SWEEPER_INTERVAL", 0),
		OrphanSweeperDryRun:   boolEnv(getenv, "GPU_BURST_ORPHAN_SWEEPER_DRY_RUN", true),
		StatusSyncInterval:    durationEnv(getenv, "GPU_BURST_STATUS_SYNC_INTERVAL", 15*time.Second),
	}

	fs := flag.NewFlagSet("gpu-burst-node", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&options.NodeName, "node-name", options.NodeName, "virtual node name")
	fs.StringVar(&options.Provider, "provider", options.Provider, "provider adapter: fake, runpod or lambda")
	fs.StringVar(&options.FallbackProviders, "fallback-providers", options.FallbackProviders, "comma-separated provider adapters to try after the primary provider fails with a fallback-safe error")
	fs.StringVar(&options.BindingStore, "binding-store", options.BindingStore, "binding store backend: kubernetes or memory")
	fs.StringVar(&options.Kubeconfig, "kubeconfig", options.Kubeconfig, "path to kubeconfig")
	fs.StringVar(&options.ListenAddr, "listen-addr", options.ListenAddr, "kubelet API listen address")
	fs.BoolVar(&options.KubeletAPIEnabled, "enable-kubelet-api", options.KubeletAPIEnabled, "serve kubelet-compatible pod routes")
	fs.StringVar(&options.TLSCertFile, "tls-cert-file", options.TLSCertFile, "TLS certificate file for kubelet API")
	fs.StringVar(&options.TLSKeyFile, "tls-key-file", options.TLSKeyFile, "TLS key file for kubelet API")
	fs.StringVar(&options.RunnerCallbackListenAddr, "runner-callback-addr", options.RunnerCallbackListenAddr, "plain HTTP listen address for VM runner status callbacks; empty disables it")
	fs.StringVar(&options.RunnerCallbackSecret, "runner-callback-secret", options.RunnerCallbackSecret, "shared HMAC secret required for VM runner status callbacks; empty permits unsigned callbacks")
	fs.StringVar(&options.RunPodAPIKey, "runpod-api-key", options.RunPodAPIKey, "RunPod API key")
	fs.StringVar(&options.RunPodBaseURL, "runpod-base-url", options.RunPodBaseURL, "RunPod REST API base URL")
	fs.StringVar(&options.LambdaAPIKey, "lambda-api-key", options.LambdaAPIKey, "Lambda Cloud API key")
	fs.StringVar(&options.LambdaBaseURL, "lambda-base-url", options.LambdaBaseURL, "Lambda Cloud API base URL")
	fs.StringVar(&options.LambdaCallbackURL, "lambda-callback-url", options.LambdaCallbackURL, "runner callback URL for Lambda Cloud cloud-init")
	fs.StringVar(&options.LambdaDefaultRegion, "lambda-region", options.LambdaDefaultRegion, "default Lambda Cloud region")
	fs.StringVar(&options.LambdaInstanceType, "lambda-instance-type", options.LambdaInstanceType, "default Lambda Cloud instance type")
	fs.StringVar(&options.LambdaSSHKeyNames, "lambda-ssh-key-names", options.LambdaSSHKeyNames, "comma-separated default Lambda Cloud SSH key names")
	fs.BoolVar(&options.RequireMaxPrice, "require-max-price", options.RequireMaxPrice, "reject pods without max price annotation")
	fs.IntVar(&options.MaxGPUCount, "max-gpu-count", intEnv(getenv, "GPU_BURST_MAX_GPU_COUNT", 0), "maximum GPUs per pod; 0 disables the guard")
	fs.IntVar(&options.MaxPorts, "max-ports", intEnv(getenv, "GPU_BURST_MAX_PORTS", 0), "maximum exposed container ports per pod; 0 disables the guard")
	fs.IntVar(&options.AdvertiseGPUCount, "advertise-gpu-count", options.AdvertiseGPUCount, "virtual node GPU capacity advertised to the scheduler")
	fs.IntVar(&options.AdvertisePodCapacity, "advertise-pod-capacity", options.AdvertisePodCapacity, "virtual node pod capacity advertised to the scheduler")
	fs.StringVar(&options.AdvertiseCPU, "advertise-cpu", options.AdvertiseCPU, "virtual node CPU capacity advertised to the scheduler")
	fs.StringVar(&options.AdvertiseMemory, "advertise-memory", options.AdvertiseMemory, "virtual node memory capacity advertised to the scheduler")
	fs.DurationVar(&options.OrphanSweeperInterval, "orphan-sweeper-interval", options.OrphanSweeperInterval, "periodic orphan sweeper interval; 0 disables it")
	fs.BoolVar(&options.OrphanSweeperDryRun, "orphan-sweeper-dry-run", options.OrphanSweeperDryRun, "report orphaned provider resources without deleting them")
	fs.DurationVar(&options.StatusSyncInterval, "status-sync-interval", options.StatusSyncInterval, "periodic remote status sync interval; 0 disables it")

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	options.Provider = strings.ToLower(strings.TrimSpace(options.Provider))
	options.FallbackProviders = normalizeCSV(options.FallbackProviders)
	options.BindingStore = strings.ToLower(strings.TrimSpace(options.BindingStore))

	if err := validateOptions(options); err != nil {
		return Options{}, err
	}
	return options, nil
}

func Run(ctx context.Context, args []string, getenv func(string) string) error {
	options, err := ParseOptions(args, getenv)
	if err != nil {
		return err
	}

	kubeClient, dynamicClient, err := clientsFromOptions(options)
	if err != nil {
		return err
	}
	node, err := NewNode(ctx, options, kubeClient, dynamicClient)
	if err != nil {
		return err
	}
	return node.Run(ctx)
}

func NewNode(ctx context.Context, options Options, kubeClient kubernetes.Interface, dynamicClient dynamic.Interface) (*nodeutil.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kubeClient == nil {
		return nil, errors.New("missing kubernetes client")
	}

	store, err := newBindingStore(options, dynamicClient)
	if err != nil {
		return nil, err
	}
	adapter, err := newProviderAdapter(options)
	if err != nil {
		return nil, err
	}
	fallbackAdapters, fallbackOrder, err := newFallbackProviderAdapters(options)
	if err != nil {
		return nil, err
	}
	if err := startOrphanSweeper(ctx, store, adapter, options); err != nil {
		return nil, err
	}
	if _, _, err := startRunnerCallbackServer(ctx, options.RunnerCallbackListenAddr, options.RunnerCallbackSecret, store); err != nil {
		return nil, err
	}

	providerConfig := vk.Config{
		Store:            store,
		Adapter:          adapter,
		FallbackAdapters: fallbackAdapters,
		FallbackOrder:    fallbackOrder,
		LaunchGuard:      launchGuard(options),
		NodeName:         options.NodeName,
		EventRecorder: newEventRecorder(kubeClient,
			"gpu-burst-node/"+options.Provider),
		StatusSyncContext:  ctx,
		StatusSyncInterval: options.StatusSyncInterval,
	}

	nodeOptions := []nodeutil.NodeOpt{
		nodeutil.WithClient(kubeClient),
		func(config *nodeutil.NodeConfig) error {
			config.HTTPListenAddr = options.ListenAddr
			config.SkipDownwardAPIResolution = true
			return configureNodeSpec(config, options)
		},
	}

	if options.KubeletAPIEnabled {
		tlsConfig, err := tlsConfig(options)
		if err != nil {
			return nil, err
		}
		mux := http.NewServeMux()
		mux.Handle("/gpu-burst-node/v1/runner/status", runnerStatusHandler(store, options.RunnerCallbackSecret))
		nodeOptions = append(nodeOptions,
			nodeutil.AttachProviderRoutes(mux),
			func(config *nodeutil.NodeConfig) error {
				config.Handler = mux
				config.TLSConfig = tlsConfig
				return nil
			},
		)
	}

	return nodeutil.NewNode(options.NodeName, vk.NewProviderFunc(providerConfig), nodeOptions...)
}

func clientsFromOptions(options Options) (kubernetes.Interface, dynamic.Interface, error) {
	config, err := restConfig(options)
	if err != nil {
		return nil, nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("create dynamic client: %w", err)
	}
	return kubeClient, dynamicClient, nil
}

func restConfig(options Options) (*rest.Config, error) {
	if options.Kubeconfig != "" {
		config, err := clientcmd.BuildConfigFromFlags("", options.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig %q: %w", options.Kubeconfig, err)
		}
		return config, nil
	}

	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	config, kubeconfigErr := clientcmd.BuildConfigFromFlags("", "")
	if kubeconfigErr == nil {
		return config, nil
	}
	return nil, fmt.Errorf("load kubernetes config: in-cluster=%v kubeconfig=%v", err, kubeconfigErr)
}

func newBindingStore(options Options, dynamicClient dynamic.Interface) (core.BindingStore, error) {
	switch options.BindingStore {
	case BindingStoreMemory:
		return core.NewMemoryBindingStore(), nil
	case BindingStoreKubernetes:
		if dynamicClient == nil {
			return nil, errors.New("kubernetes binding store requires a dynamic client")
		}
		return kstore.NewBindingStore(dynamicClient), nil
	default:
		return nil, fmt.Errorf("unsupported binding store %q", options.BindingStore)
	}
}

func newProviderAdapter(options Options) (core.ProviderAdapter, error) {
	return providerAdapterFor(options, options.Provider)
}

func newFallbackProviderAdapters(options Options) (map[string]core.ProviderAdapter, []string, error) {
	fallbackOrder := commaSeparated(options.FallbackProviders)
	if len(fallbackOrder) == 0 {
		return nil, nil, nil
	}
	adapters := make(map[string]core.ProviderAdapter, len(fallbackOrder)+1)
	primary, err := providerAdapterFor(options, options.Provider)
	if err != nil {
		return nil, nil, err
	}
	adapters[options.Provider] = primary
	for _, provider := range fallbackOrder {
		if provider == options.Provider {
			continue
		}
		adapter, err := providerAdapterFor(options, provider)
		if err != nil {
			return nil, nil, err
		}
		adapters[provider] = adapter
	}
	return adapters, fallbackOrder, nil
}

func providerAdapterFor(options Options, provider string) (core.ProviderAdapter, error) {
	switch provider {
	case ProviderFake:
		return core.NewFakeProvider(ProviderFake), nil
	case ProviderRunPod:
		return runpod.NewClient(runpod.Config{
			APIKey:  options.RunPodAPIKey,
			BaseURL: options.RunPodBaseURL,
		}), nil
	case ProviderLambda:
		return lambdacloud.NewClient(lambdacloud.Config{
			APIKey:              options.LambdaAPIKey,
			BaseURL:             options.LambdaBaseURL,
			CallbackURL:         options.LambdaCallbackURL,
			CallbackSecret:      options.RunnerCallbackSecret,
			DefaultRegion:       options.LambdaDefaultRegion,
			DefaultInstanceType: options.LambdaInstanceType,
			DefaultSSHKeyNames:  commaSeparated(options.LambdaSSHKeyNames),
		}), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func launchGuard(options Options) *core.LaunchGuard {
	if !options.RequireMaxPrice && options.MaxGPUCount == 0 && options.MaxPorts == 0 {
		return nil
	}
	guard := core.NewLaunchGuard(core.LaunchPolicy{
		RequireMaxPrice: options.RequireMaxPrice,
		MaxGPUCount:     options.MaxGPUCount,
		MaxPorts:        options.MaxPorts,
	})
	return &guard
}

func newEventRecorder(kubeClient kubernetes.Interface, component string) record.EventRecorder {
	if kubeClient == nil {
		return nil
	}
	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	return broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: component})
}

func runnerStatusHandler(store core.BindingStore, callbackSecret string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read runner status: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := verifyRunnerCallbackSignature(r, body, callbackSecret, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		var report runner.StatusReport
		if err := json.NewDecoder(strings.NewReader(string(body))).Decode(&report); err != nil {
			http.Error(w, "decode runner status: "+err.Error(), http.StatusBadRequest)
			return
		}
		namespace, workloadUID, err := report.NamespaceAndUID()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := core.RecordRunnerStatus(r.Context(), store, namespace, workloadUID, report.ExitCode, report.Message); err != nil {
			if errors.Is(err, core.ErrBindingNotFound) {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func verifyRunnerCallbackSignature(r *http.Request, body []byte, secret string, now time.Time) error {
	if strings.TrimSpace(secret) == "" {
		return nil
	}
	timestamp := strings.TrimSpace(r.Header.Get("X-GPU-Burst-Runner-Timestamp"))
	signature := strings.TrimSpace(r.Header.Get("X-GPU-Burst-Runner-Signature"))
	if timestamp == "" || signature == "" {
		return errors.New("missing runner callback signature")
	}
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errors.New("invalid runner callback timestamp")
	}
	signedAt := time.Unix(seconds, 0)
	if now.Sub(signedAt) > 10*time.Minute || signedAt.Sub(now) > 10*time.Minute {
		return errors.New("stale runner callback timestamp")
	}
	expected := runnerCallbackSignature(secret, timestamp, body)
	if !hmac.Equal([]byte(signature), []byte(expected)) {
		return errors.New("invalid runner callback signature")
	}
	return nil
}

func runnerCallbackSignature(secret string, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}

func startRunnerCallbackServer(ctx context.Context, listenAddr string, callbackSecret string, store core.BindingStore) (string, func(context.Context) error, error) {
	if strings.TrimSpace(listenAddr) == "" {
		return "", func(context.Context) error { return nil }, nil
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return "", nil, fmt.Errorf("listen runner callback server: %w", err)
	}
	server := &http.Server{Handler: runnerStatusHandler(store, callbackSecret)}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("gpu-burst-node runner callback server failed: addr=%s error=%v", listener.Addr().String(), err)
		}
	}()
	return listener.Addr().String(), server.Shutdown, nil
}

func startOrphanSweeper(ctx context.Context, store core.BindingStore, adapter core.ProviderAdapter, options Options) error {
	if options.OrphanSweeperInterval <= 0 {
		return nil
	}
	cleaner, ok := adapter.(core.RemoteResourceCleaner)
	if !ok {
		return fmt.Errorf("orphan sweeper requires provider %q to support remote resource listing", options.Provider)
	}

	sweeper := core.OrphanSweeper{
		Store:    store,
		Provider: cleaner,
		DryRun:   options.OrphanSweeperDryRun,
	}
	go runOrphanSweeper(ctx, options.OrphanSweeperInterval, sweeper)
	return nil
}

func runOrphanSweeper(ctx context.Context, interval time.Duration, sweeper core.OrphanSweeper) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			report, err := sweeper.Sweep(ctx)
			logOrphanSweepReport(report, sweeper.DryRun, err)
		}
	}
}

func logOrphanSweepReport(report core.OrphanSweepReport, dryRun bool, err error) {
	if err != nil {
		log.Printf("gpu-burst-node orphan sweeper failed: dryRun=%t remote=%d activeBindings=%d orphans=%d deleted=%d deleteErrors=%d error=%v",
			dryRun,
			report.ObservedRemoteResources,
			report.ActiveBindings,
			len(report.Orphans),
			len(report.Deleted),
			len(report.DeleteErrors),
			err)
		return
	}
	log.Printf("gpu-burst-node orphan sweeper completed: dryRun=%t remote=%d activeBindings=%d orphans=%d deleted=%d",
		dryRun,
		report.ObservedRemoteResources,
		report.ActiveBindings,
		len(report.Orphans),
		len(report.Deleted))
}

func tlsConfig(options Options) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(options.TLSCertFile, options.TLSKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load kubelet API TLS certificate: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}, nil
}

func validateOptions(options Options) error {
	if strings.TrimSpace(options.NodeName) == "" {
		return errors.New("node name is required")
	}
	if _, _, err := net.SplitHostPort(options.ListenAddr); err != nil {
		return fmt.Errorf("invalid listen address %q: %w", options.ListenAddr, err)
	}
	switch options.Provider {
	case ProviderFake:
	case ProviderRunPod:
		if strings.TrimSpace(options.RunPodAPIKey) == "" {
			return errors.New("runpod provider requires --runpod-api-key or RUNPOD_API_KEY")
		}
	case ProviderLambda:
		if strings.TrimSpace(options.LambdaAPIKey) == "" {
			return errors.New("lambda provider requires --lambda-api-key or LAMBDA_API_KEY")
		}
	default:
		return fmt.Errorf("unsupported provider %q", options.Provider)
	}
	for _, provider := range commaSeparated(options.FallbackProviders) {
		if provider == options.Provider {
			continue
		}
		if err := validateProviderConfigured(options, provider); err != nil {
			return err
		}
	}
	switch options.BindingStore {
	case BindingStoreMemory, BindingStoreKubernetes:
	default:
		return fmt.Errorf("unsupported binding store %q", options.BindingStore)
	}
	if options.KubeletAPIEnabled && (options.TLSCertFile == "" || options.TLSKeyFile == "") {
		return errors.New("kubelet API requires --tls-cert-file and --tls-key-file")
	}
	if options.AdvertiseGPUCount < 0 {
		return errors.New("advertise gpu count cannot be negative")
	}
	if options.AdvertisePodCapacity <= 0 {
		return errors.New("advertise pod capacity must be greater than zero")
	}
	if options.OrphanSweeperInterval < 0 {
		return errors.New("orphan sweeper interval cannot be negative")
	}
	return nil
}

func validateProviderConfigured(options Options, provider string) error {
	switch provider {
	case ProviderFake:
		return nil
	case ProviderRunPod:
		if strings.TrimSpace(options.RunPodAPIKey) == "" {
			return errors.New("runpod fallback provider requires --runpod-api-key or RUNPOD_API_KEY")
		}
		return nil
	case ProviderLambda:
		if strings.TrimSpace(options.LambdaAPIKey) == "" {
			return errors.New("lambda fallback provider requires --lambda-api-key or LAMBDA_API_KEY")
		}
		return nil
	default:
		return fmt.Errorf("unsupported fallback provider %q", provider)
	}
}

func commaSeparated(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func normalizeCSV(raw string) string {
	return strings.Join(commaSeparated(strings.ToLower(raw)), ",")
}

func configureNodeSpec(config *nodeutil.NodeConfig, options Options) error {
	if config.NodeSpec.Labels == nil {
		config.NodeSpec.Labels = map[string]string{}
	}
	config.NodeSpec.Labels[NodeLabelProvider] = options.Provider

	config.NodeSpec.Spec.Taints = upsertProviderTaint(config.NodeSpec.Spec.Taints, options.Provider)

	cpu, err := resource.ParseQuantity(options.AdvertiseCPU)
	if err != nil {
		return fmt.Errorf("invalid advertised CPU %q: %w", options.AdvertiseCPU, err)
	}
	memory, err := resource.ParseQuantity(options.AdvertiseMemory)
	if err != nil {
		return fmt.Errorf("invalid advertised memory %q: %w", options.AdvertiseMemory, err)
	}
	gpu := resource.MustParse(strconv.Itoa(options.AdvertiseGPUCount))
	pods := resource.MustParse(strconv.Itoa(options.AdvertisePodCapacity))

	capacity := corev1.ResourceList{
		corev1.ResourceCPU:                     cpu,
		corev1.ResourceMemory:                  memory,
		corev1.ResourcePods:                    pods,
		corev1.ResourceName(NvidiaGPUResource): gpu,
	}
	config.NodeSpec.Status.Capacity = capacity.DeepCopy()
	config.NodeSpec.Status.Allocatable = capacity.DeepCopy()
	return nil
}

func upsertProviderTaint(existing []corev1.Taint, provider string) []corev1.Taint {
	taint := corev1.Taint{
		Key:    NodeTaintKey,
		Value:  provider,
		Effect: corev1.TaintEffectNoSchedule,
	}
	for index := range existing {
		if existing[index].Key == NodeTaintKey {
			existing[index] = taint
			return existing
		}
	}
	return append(existing, taint)
}

func envOr(getenv func(string) string, key string, fallback string) string {
	if value := strings.TrimSpace(getenv(key)); value != "" {
		return value
	}
	return fallback
}

func boolEnv(getenv func(string) string, key string, fallback bool) bool {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func intEnv(getenv func(string) string, key string, fallback int) int {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func durationEnv(getenv func(string) string, key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
