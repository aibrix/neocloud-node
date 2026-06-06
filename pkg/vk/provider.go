package vk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/kube"
)

const defaultStatsNodeName = "gpu-burst-node"

type PodTransformer interface {
	TransformPod(pod *corev1.Pod) (core.Binding, error)
}

type EventRecorder interface {
	Eventf(object runtime.Object, eventtype, reason, messageFmt string, args ...interface{})
}

type Config struct {
	Store         core.BindingStore
	Adapter       core.ProviderAdapter
	Transformer   PodTransformer
	LaunchGuard   *core.LaunchGuard
	NodeName      string
	EventRecorder EventRecorder

	StatusSyncContext  context.Context
	StatusSyncInterval time.Duration
}

type Provider struct {
	transformer PodTransformer
	lifecycle   *core.LifecycleService
	store       core.BindingStore
	nodeName    string
	recorder    EventRecorder

	mu       sync.RWMutex
	pods     map[string]*corev1.Pod
	notifier func(*corev1.Pod)
}

var _ nodeutil.Provider = (*Provider)(nil)
var _ node.PodNotifier = (*Provider)(nil)

func NewProvider(config Config) (*Provider, error) {
	if config.Store == nil {
		return nil, errdefs.InvalidInput("missing binding store")
	}
	if config.Adapter == nil {
		return nil, errdefs.InvalidInput("missing provider adapter")
	}
	if config.Transformer == nil {
		config.Transformer = kube.Transformer{}
	}
	if config.NodeName == "" {
		config.NodeName = defaultStatsNodeName
	}

	options := []core.LifecycleOption{}
	if config.LaunchGuard != nil {
		options = append(options, core.WithLaunchGuard(*config.LaunchGuard))
	}

	provider := &Provider{
		transformer: config.Transformer,
		lifecycle:   core.NewLifecycleService(config.Store, config.Adapter, options...),
		store:       config.Store,
		nodeName:    config.NodeName,
		recorder:    config.EventRecorder,
		pods:        make(map[string]*corev1.Pod),
	}
	if config.StatusSyncInterval > 0 {
		syncContext := config.StatusSyncContext
		if syncContext == nil {
			syncContext = context.Background()
		}
		go provider.runStatusSync(syncContext, config.StatusSyncInterval)
	}
	return provider, nil
}

func NewProviderFunc(config Config) nodeutil.NewProviderFunc {
	return func(providerConfig nodeutil.ProviderConfig) (nodeutil.Provider, node.NodeProvider, error) {
		resolved := config
		if resolved.NodeName == "" && providerConfig.Node != nil {
			resolved.NodeName = providerConfig.Node.Name
		}

		provider, err := NewProvider(resolved)
		if err != nil {
			return nil, nil, err
		}
		return provider, nil, nil
	}
}

func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	return p.upsertPod(ctx, pod)
}

func (p *Provider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	return p.upsertPod(ctx, pod)
}

func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if pod == nil {
		return errdefs.InvalidInput("pod is nil")
	}

	key := podKey(pod.Namespace, pod.Name)
	cached := p.getCachedPod(key)
	if err := p.lifecycle.EnsureDeleted(ctx, pod.Namespace, string(pod.UID)); err != nil {
		if cached != nil {
			p.recordPodEvent(cached, corev1.EventTypeWarning, "DeleteFailure", err.Error())
		} else {
			p.recordPodEvent(pod, corev1.EventTypeWarning, "DeleteFailure", err.Error())
		}
		return normalizeProviderError(err)
	}

	p.mu.Lock()
	delete(p.pods, key)
	p.mu.Unlock()

	if cached != nil {
		terminal := podWithTerminalStatus(cached)
		p.notify(terminal)
	}
	return nil
}

func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	pod := p.getCachedPod(podKey(namespace, name))
	if pod == nil {
		return nil, errdefs.NotFoundf("pod %q is not known to the provider", namespace+"/"+name)
	}
	return p.syncPodStatus(ctx, pod)
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return pod.Status.DeepCopy(), nil
}

func (p *Provider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	p.mu.RLock()
	keys := make([]string, 0, len(p.pods))
	for key := range p.pods {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	pods := make([]*corev1.Pod, 0, len(keys))
	for _, key := range keys {
		pods = append(pods, p.pods[key].DeepCopy())
	}
	p.mu.RUnlock()

	synced := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		out, err := p.syncPodStatus(ctx, pod)
		if err != nil {
			return nil, err
		}
		synced = append(synced, out)
	}
	return synced, nil
}

func (p *Provider) NotifyPods(ctx context.Context, notifier func(*corev1.Pod)) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.notifier = notifier
}

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, unsupportedMethod("container logs")
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return unsupportedMethod("exec")
}

func (p *Provider) AttachToContainer(ctx context.Context, namespace, podName, containerName string, attach api.AttachIO) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return unsupportedMethod("attach")
}

func (p *Provider) PortForward(ctx context.Context, namespace, pod string, port int32, stream io.ReadWriteCloser) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return unsupportedMethod("port-forward")
}

func (p *Provider) GetStatsSummary(ctx context.Context) (*stats.Summary, error) {
	pods, err := p.GetPods(ctx)
	if err != nil {
		return nil, err
	}

	summary := &stats.Summary{
		Node: stats.NodeStats{
			NodeName: p.nodeName,
		},
		Pods: make([]stats.PodStats, 0, len(pods)),
	}
	for _, pod := range pods {
		summary.Pods = append(summary.Pods, podStats(pod))
	}
	return summary, nil
}

func (p *Provider) GetMetricsResource(ctx context.Context) ([]*dto.MetricFamily, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bindings, err := p.store.List(ctx)
	if err != nil {
		return nil, err
	}
	return bindingMetricFamilies(bindings), nil
}

func (p *Provider) runStatusSync(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.syncCachedPods(ctx)
		}
	}
}

func (p *Provider) syncCachedPods(ctx context.Context) {
	p.mu.RLock()
	pods := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		pods = append(pods, pod.DeepCopy())
	}
	p.mu.RUnlock()

	for _, pod := range pods {
		if ctx.Err() != nil {
			return
		}
		_, _ = p.syncPodStatus(ctx, pod)
	}
}

func (p *Provider) upsertPod(ctx context.Context, pod *corev1.Pod) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	binding, err := p.transformer.TransformPod(pod)
	if err != nil {
		cached := podWithErrorStatus(pod, core.ClassifyError(err), err.Error())
		p.mu.Lock()
		p.pods[podKey(cached.Namespace, cached.Name)] = cached.DeepCopy()
		p.mu.Unlock()
		p.notify(cached)
		p.recordPodEvent(cached, corev1.EventTypeWarning, string(core.ClassifyError(err)), err.Error())
		return normalizeProviderError(err)
	}

	binding, err = p.lifecycle.EnsureCreated(ctx, binding)
	if err != nil {
		cached := podWithBindingStatus(pod, binding)
		p.mu.Lock()
		p.pods[podKey(cached.Namespace, cached.Name)] = cached.DeepCopy()
		p.mu.Unlock()
		p.notify(cached)
		reason, message := podReasonMessage(binding)
		if reason == "" {
			reason = string(core.ClassifyError(err))
		}
		if message == "" {
			message = err.Error()
		}
		p.recordPodEvent(cached, corev1.EventTypeWarning, reason, message)
		return normalizeProviderError(err)
	}

	cached := podWithBindingStatus(pod, binding)
	p.mu.Lock()
	p.pods[podKey(cached.Namespace, cached.Name)] = cached.DeepCopy()
	p.mu.Unlock()

	p.notify(cached)
	return nil
}

func (p *Provider) getCachedPod(key string) *corev1.Pod {
	p.mu.RLock()
	defer p.mu.RUnlock()

	pod, ok := p.pods[key]
	if !ok {
		return nil
	}
	return pod.DeepCopy()
}

func (p *Provider) syncPodStatus(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	if pod == nil || pod.UID == "" {
		return pod, nil
	}

	binding, err := p.lifecycle.SyncStatus(ctx, pod.Namespace, string(pod.UID))
	if errors.Is(err, core.ErrBindingNotFound) && pod.Status.Phase == corev1.PodFailed {
		return pod.DeepCopy(), nil
	}
	if err != nil && !errors.Is(err, core.ErrRemoteNotFound) {
		return nil, normalizeProviderError(err)
	}

	synced := podWithBindingStatus(pod, binding)
	p.mu.Lock()
	key := podKey(synced.Namespace, synced.Name)
	if current, ok := p.pods[key]; ok && current.UID == synced.UID {
		p.pods[key] = synced.DeepCopy()
	}
	p.mu.Unlock()

	if pod.Status.Phase != synced.Status.Phase {
		p.notify(synced)
	}
	return synced, nil
}

func (p *Provider) notify(pod *corev1.Pod) {
	p.mu.RLock()
	notifier := p.notifier
	p.mu.RUnlock()

	if notifier != nil {
		notifier(pod.DeepCopy())
	}
}

func (p *Provider) recordPodEvent(pod *corev1.Pod, eventType string, reason string, message string) {
	if p.recorder == nil || pod == nil {
		return
	}
	if reason == "" {
		reason = string(core.ErrorClassUnknown)
	}
	p.recorder.Eventf(pod, eventType, reason, "%s", message)
}

func podWithBindingStatus(pod *corev1.Pod, binding core.Binding) *corev1.Pod {
	out := pod.DeepCopy()
	now := metav1.Now()
	phase := podPhase(binding.Phase)
	reason, message := podReasonMessage(binding)
	out.Status.Phase = phase
	out.Status.StartTime = startTime(out, now)
	out.Status.Conditions = podConditions(phase, reason, message)
	out.Status.ContainerStatuses = containerStatuses(out.Spec.Containers, phase, now, reason, message)
	return out
}

func podWithTerminalStatus(pod *corev1.Pod) *corev1.Pod {
	out := pod.DeepCopy()
	now := metav1.Now()
	out.Status.Phase = corev1.PodSucceeded
	out.Status.Conditions = podConditions(corev1.PodSucceeded, "RemoteWorkloadDeleted", "remote workload was deleted")
	out.Status.ContainerStatuses = containerStatuses(out.Spec.Containers, corev1.PodSucceeded, now, "RemoteWorkloadDeleted", "remote workload was deleted")
	return out
}

func podWithErrorStatus(pod *corev1.Pod, class core.ErrorClass, message string) *corev1.Pod {
	out := pod.DeepCopy()
	now := metav1.Now()
	reason := string(class)
	if reason == "" {
		reason = string(core.ErrorClassUnknown)
	}
	out.Status.Phase = corev1.PodFailed
	out.Status.StartTime = startTime(out, now)
	out.Status.Conditions = podConditions(corev1.PodFailed, reason, message)
	out.Status.ContainerStatuses = containerStatuses(out.Spec.Containers, corev1.PodFailed, now, reason, message)
	return out
}

func podStats(pod *corev1.Pod) stats.PodStats {
	start := pod.CreationTimestamp
	if pod.Status.StartTime != nil {
		start = *pod.Status.StartTime
	}

	podStats := stats.PodStats{
		PodRef: stats.PodReference{
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       string(pod.UID),
		},
		StartTime: start,
	}
	for _, container := range pod.Spec.Containers {
		podStats.Containers = append(podStats.Containers, stats.ContainerStats{
			Name:      container.Name,
			StartTime: start,
		})
	}
	return podStats
}

func bindingMetricFamilies(bindings []core.Binding) []*dto.MetricFamily {
	sort.SliceStable(bindings, func(i, j int) bool {
		if bindings[i].Namespace != bindings[j].Namespace {
			return bindings[i].Namespace < bindings[j].Namespace
		}
		return bindings[i].Name < bindings[j].Name
	})

	phaseFamily := gaugeFamily(
		"gpu_burst_node_binding_phase",
		"Current CloudPodBinding phase by pod.",
	)
	burnRateFamily := gaugeFamily(
		"gpu_burst_node_current_burn_rate_usd_per_hour",
		"Current estimated provider burn rate in USD per hour by pod.",
	)

	for _, binding := range bindings {
		provider := firstNonEmpty(binding.Placement.Provider, binding.Request.Provider)
		phaseFamily.Metric = append(phaseFamily.Metric, gaugeMetric(1,
			label("namespace", binding.Namespace),
			label("pod", binding.Name),
			label("provider", provider),
			label("phase", string(binding.Phase)),
		))
		if binding.Cost.HourlyUSD > 0 {
			burnRateFamily.Metric = append(burnRateFamily.Metric, gaugeMetric(binding.Cost.HourlyUSD,
				label("namespace", binding.Namespace),
				label("pod", binding.Name),
				label("provider", provider),
				label("remote_id", binding.RemoteID),
				label("cost_source", binding.Cost.Source),
			))
		}
	}

	return []*dto.MetricFamily{phaseFamily, burnRateFamily}
}

func gaugeFamily(name string, help string) *dto.MetricFamily {
	metricType := dto.MetricType_GAUGE
	return &dto.MetricFamily{
		Name: &name,
		Help: &help,
		Type: &metricType,
	}
}

func gaugeMetric(value float64, labels ...*dto.LabelPair) *dto.Metric {
	return &dto.Metric{
		Label: labels,
		Gauge: &dto.Gauge{Value: &value},
	}
}

func label(name string, value string) *dto.LabelPair {
	return &dto.LabelPair{Name: &name, Value: &value}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func podPhase(phase core.BindingPhase) corev1.PodPhase {
	switch phase {
	case core.BindingRunning:
		return corev1.PodRunning
	case core.BindingSucceeded, core.BindingDeleted:
		return corev1.PodSucceeded
	case core.BindingFailed:
		return corev1.PodFailed
	default:
		return corev1.PodPending
	}
}

func podConditions(phase corev1.PodPhase, reason string, message string) []corev1.PodCondition {
	readyStatus := corev1.ConditionFalse
	if phase == corev1.PodRunning {
		readyStatus = corev1.ConditionTrue
	}
	return []corev1.PodCondition{
		{Type: corev1.PodInitialized, Status: corev1.ConditionTrue},
		{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
		{Type: corev1.PodReady, Status: readyStatus, Reason: reason, Message: message},
		{Type: corev1.ContainersReady, Status: readyStatus, Reason: reason, Message: message},
	}
}

func containerStatuses(containers []corev1.Container, phase corev1.PodPhase, now metav1.Time, reason string, message string) []corev1.ContainerStatus {
	statuses := make([]corev1.ContainerStatus, 0, len(containers))
	for _, container := range containers {
		status := corev1.ContainerStatus{
			Name:         container.Name,
			Image:        container.Image,
			Ready:        phase == corev1.PodRunning,
			RestartCount: 0,
		}
		switch phase {
		case corev1.PodRunning:
			status.State.Running = &corev1.ContainerStateRunning{StartedAt: now}
		case corev1.PodSucceeded:
			status.State.Terminated = &corev1.ContainerStateTerminated{
				ExitCode:   0,
				Reason:     reasonOrDefault(reason, "RemoteWorkloadSucceeded"),
				Message:    message,
				FinishedAt: now,
			}
		case corev1.PodFailed:
			status.State.Terminated = &corev1.ContainerStateTerminated{
				ExitCode:   1,
				Reason:     reasonOrDefault(reason, "RemoteWorkloadFailed"),
				Message:    message,
				FinishedAt: now,
			}
		default:
			status.State.Waiting = &corev1.ContainerStateWaiting{
				Reason:  reasonOrDefault(reason, "RemoteWorkloadPending"),
				Message: message,
			}
		}
		statuses = append(statuses, status)
	}
	return statuses
}

func podReasonMessage(binding core.Binding) (string, string) {
	for _, conditionType := range []core.ConditionType{
		core.ConditionUnsupportedSpec,
		core.ConditionAuth,
		core.ConditionCapacity,
		core.ConditionDeleteFailure,
		core.ConditionRemoteDeleted,
		core.ConditionProviderError,
	} {
		condition := core.FindCondition(binding.Conditions, conditionType)
		if condition != nil && condition.Status == core.ConditionTrue {
			return condition.Reason, condition.Message
		}
	}
	if binding.LastError != "" {
		return "RemoteWorkloadError", binding.LastError
	}
	return "", ""
}

func startTime(pod *corev1.Pod, fallback metav1.Time) *metav1.Time {
	if pod.Status.StartTime != nil {
		return pod.Status.StartTime.DeepCopy()
	}
	return &fallback
}

func reasonOrDefault(reason string, fallback string) string {
	if reason != "" {
		return reason
	}
	return fallback
}

func podKey(namespace string, name string) string {
	return namespace + "/" + name
}

func unsupportedMethod(method string) error {
	return errdefs.InvalidInput(fmt.Sprintf("%s is not supported by GPU Burst Node provider yet", method))
}

func normalizeProviderError(err error) error {
	if err == nil {
		return nil
	}

	var unsupported *core.UnsupportedSpecError
	if errors.As(err, &unsupported) {
		return errdefs.AsInvalidInput(err)
	}
	var policyViolation *core.PolicyViolationError
	if errors.As(err, &policyViolation) {
		return errdefs.AsInvalidInput(err)
	}
	if errors.Is(err, core.ErrBindingNotFound) {
		return errdefs.AsNotFound(err)
	}
	return err
}
