package app

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/providers/runpod"
	kstore "github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/store/kubernetes"
	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/vk"
)

const (
	DefaultNodeName = "aibrix-gpu-burst"

	ProviderFake   = "fake"
	ProviderRunPod = "runpod"

	BindingStoreMemory     = "memory"
	BindingStoreKubernetes = "kubernetes"

	NodeLabelProvider = "gpu-burst-node.aibrix.ai/provider"
	NodeTaintKey      = "gpu-burst-node.aibrix.ai/provider"
	NvidiaGPUResource = "nvidia.com/gpu"
)

type Options struct {
	NodeName          string
	Provider          string
	BindingStore      string
	Kubeconfig        string
	ListenAddr        string
	KubeletAPIEnabled bool
	TLSCertFile       string
	TLSKeyFile        string

	RunPodAPIKey  string
	RunPodBaseURL string

	RequireMaxPrice bool
	MaxGPUCount     int
	MaxPorts        int

	AdvertiseGPUCount    int
	AdvertisePodCapacity int
	AdvertiseCPU         string
	AdvertiseMemory      string
}

func ParseOptions(args []string, getenv func(string) string) (Options, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}

	options := Options{
		NodeName:     envOr(getenv, "GPU_BURST_NODE_NAME", DefaultNodeName),
		Provider:     envOr(getenv, "GPU_BURST_PROVIDER", ProviderFake),
		BindingStore: envOr(getenv, "GPU_BURST_BINDING_STORE", BindingStoreKubernetes),
		Kubeconfig:   envOr(getenv, "KUBECONFIG", ""),
		ListenAddr:   envOr(getenv, "GPU_BURST_LISTEN_ADDR", ":10250"),
		TLSCertFile:  envOr(getenv, "GPU_BURST_TLS_CERT_FILE", ""),
		TLSKeyFile:   envOr(getenv, "GPU_BURST_TLS_KEY_FILE", ""),

		RunPodAPIKey:  envOr(getenv, "RUNPOD_API_KEY", ""),
		RunPodBaseURL: envOr(getenv, "GPU_BURST_RUNPOD_BASE_URL", ""),

		KubeletAPIEnabled: boolEnv(getenv, "GPU_BURST_ENABLE_KUBELET_API", false),
		RequireMaxPrice:   boolEnv(getenv, "GPU_BURST_REQUIRE_MAX_PRICE", false),
		AdvertiseGPUCount: intEnv(getenv, "GPU_BURST_ADVERTISE_GPU_COUNT", 100),
		AdvertisePodCapacity: intEnv(getenv,
			"GPU_BURST_ADVERTISE_POD_CAPACITY", 1000),
		AdvertiseCPU:    envOr(getenv, "GPU_BURST_ADVERTISE_CPU", "1000"),
		AdvertiseMemory: envOr(getenv, "GPU_BURST_ADVERTISE_MEMORY", "1Ti"),
	}

	fs := flag.NewFlagSet("gpu-burst-node", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&options.NodeName, "node-name", options.NodeName, "virtual node name")
	fs.StringVar(&options.Provider, "provider", options.Provider, "provider adapter: fake or runpod")
	fs.StringVar(&options.BindingStore, "binding-store", options.BindingStore, "binding store backend: kubernetes or memory")
	fs.StringVar(&options.Kubeconfig, "kubeconfig", options.Kubeconfig, "path to kubeconfig")
	fs.StringVar(&options.ListenAddr, "listen-addr", options.ListenAddr, "kubelet API listen address")
	fs.BoolVar(&options.KubeletAPIEnabled, "enable-kubelet-api", options.KubeletAPIEnabled, "serve kubelet-compatible pod routes")
	fs.StringVar(&options.TLSCertFile, "tls-cert-file", options.TLSCertFile, "TLS certificate file for kubelet API")
	fs.StringVar(&options.TLSKeyFile, "tls-key-file", options.TLSKeyFile, "TLS key file for kubelet API")
	fs.StringVar(&options.RunPodAPIKey, "runpod-api-key", options.RunPodAPIKey, "RunPod API key")
	fs.StringVar(&options.RunPodBaseURL, "runpod-base-url", options.RunPodBaseURL, "RunPod REST API base URL")
	fs.BoolVar(&options.RequireMaxPrice, "require-max-price", options.RequireMaxPrice, "reject pods without max price annotation")
	fs.IntVar(&options.MaxGPUCount, "max-gpu-count", intEnv(getenv, "GPU_BURST_MAX_GPU_COUNT", 0), "maximum GPUs per pod; 0 disables the guard")
	fs.IntVar(&options.MaxPorts, "max-ports", intEnv(getenv, "GPU_BURST_MAX_PORTS", 0), "maximum exposed container ports per pod; 0 disables the guard")
	fs.IntVar(&options.AdvertiseGPUCount, "advertise-gpu-count", options.AdvertiseGPUCount, "virtual node GPU capacity advertised to the scheduler")
	fs.IntVar(&options.AdvertisePodCapacity, "advertise-pod-capacity", options.AdvertisePodCapacity, "virtual node pod capacity advertised to the scheduler")
	fs.StringVar(&options.AdvertiseCPU, "advertise-cpu", options.AdvertiseCPU, "virtual node CPU capacity advertised to the scheduler")
	fs.StringVar(&options.AdvertiseMemory, "advertise-memory", options.AdvertiseMemory, "virtual node memory capacity advertised to the scheduler")

	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	options.Provider = strings.ToLower(strings.TrimSpace(options.Provider))
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

	providerConfig := vk.Config{
		Store:       store,
		Adapter:     adapter,
		LaunchGuard: launchGuard(options),
		NodeName:    options.NodeName,
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
	switch options.Provider {
	case ProviderFake:
		return core.NewFakeProvider(ProviderFake), nil
	case ProviderRunPod:
		return runpod.NewClient(runpod.Config{
			APIKey:  options.RunPodAPIKey,
			BaseURL: options.RunPodBaseURL,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", options.Provider)
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
	default:
		return fmt.Errorf("unsupported provider %q", options.Provider)
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
	return nil
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
