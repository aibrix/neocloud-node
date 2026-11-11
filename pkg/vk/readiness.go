package vk

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/kube"
)

const (
	reasonReadinessPending = "ReadinessProbePending"
	reasonReadinessFailed  = "ReadinessProbeFailed"
)

// readinessManager runs readiness probes for remote Pods. The provider runs
// the container on a host the cluster cannot reach directly, so the controller
// probes the provider's published endpoint for the probe's container port.
type readinessManager struct {
	ctx      context.Context
	timeUnit time.Duration
	onChange func(key string)

	mu      sync.Mutex
	workers map[string]*readinessWorker
}

type readinessTarget struct {
	host string
	port int
}

type readinessWorker struct {
	uid    types.UID
	target readinessTarget
	cancel context.CancelFunc

	mu      sync.Mutex
	ready   bool
	message string
}

func newReadinessManager(ctx context.Context, onChange func(key string)) *readinessManager {
	if ctx == nil {
		ctx = context.Background()
	}
	return &readinessManager{
		ctx:      ctx,
		timeUnit: time.Second,
		onChange: onChange,
		workers:  make(map[string]*readinessWorker),
	}
}

// observe returns the readiness of a running Pod with a readiness probe and
// starts or restarts its probe worker when needed. It returns ok=false when
// the probe does not apply and the phase-based readiness should be kept.
// seedReady carries the readiness already reported for the Pod, so a
// controller restart does not drop Ready before the next probe runs.
func (m *readinessManager) observe(key string, pod *corev1.Pod, endpoints []core.Endpoint, seedReady bool) (bool, bool, string) {
	container, probe := readinessProbeFor(pod)
	if probe == nil || pod.Status.Phase != corev1.PodRunning {
		m.stop(key)
		return false, false, ""
	}

	port := probePort(container, probe)
	target, found := readinessTargetFor(endpoints, port)
	if !found {
		m.stop(key)
		return true, false, fmt.Sprintf("waiting for the provider to publish container port %d", port)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if worker, ok := m.workers[key]; ok {
		if worker.uid == pod.UID && worker.target == target {
			ready, message := worker.result()
			return true, ready, message
		}
		worker.cancel()
		delete(m.workers, key)
	}

	ctx, cancel := context.WithCancel(m.ctx)
	worker := &readinessWorker{
		uid:     pod.UID,
		target:  target,
		cancel:  cancel,
		ready:   seedReady,
		message: "readiness probe has not succeeded yet",
	}
	m.workers[key] = worker
	go worker.run(ctx, probe.DeepCopy(), m.timeUnit, func() {
		if m.onChange != nil {
			m.onChange(key)
		}
	})
	ready, message := worker.result()
	return true, ready, message
}

// result returns the current result of the worker for a Pod.
func (m *readinessManager) result(key string, uid types.UID) (bool, string, bool) {
	m.mu.Lock()
	worker, ok := m.workers[key]
	m.mu.Unlock()
	if !ok || worker.uid != uid {
		return false, "", false
	}
	ready, message := worker.result()
	return ready, message, true
}

func (m *readinessManager) stop(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if worker, ok := m.workers[key]; ok {
		worker.cancel()
		delete(m.workers, key)
	}
}

func (w *readinessWorker) result() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ready, w.message
}

// run probes the target like kubelet: after the initial delay, every period,
// and flips readiness after successThreshold consecutive successes or
// failureThreshold consecutive failures.
func (w *readinessWorker) run(ctx context.Context, probe *corev1.Probe, unit time.Duration, onChange func()) {
	if !sleepContext(ctx, time.Duration(probe.InitialDelaySeconds)*unit) {
		return
	}
	period := time.Duration(atLeastOne(probe.PeriodSeconds, 10)) * unit
	timeout := time.Duration(atLeastOne(probe.TimeoutSeconds, 1)) * unit
	successThreshold := int(atLeastOne(probe.SuccessThreshold, 1))
	failureThreshold := int(atLeastOne(probe.FailureThreshold, 3))

	successes, failures := 0, 0
	for {
		err := runReadinessProbe(ctx, probe, w.target, timeout)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			successes++
			failures = 0
		} else {
			failures++
			successes = 0
		}
		if w.record(err, successes >= successThreshold, failures >= failureThreshold) {
			onChange()
		}
		if !sleepContext(ctx, period) {
			return
		}
	}
}

func (w *readinessWorker) record(err error, becomeReady bool, becomeNotReady bool) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err == nil {
		w.message = ""
	} else {
		w.message = "readiness probe failed: " + err.Error()
	}
	switch {
	case becomeReady && !w.ready:
		w.ready = true
		return true
	case becomeNotReady && w.ready:
		w.ready = false
		return true
	default:
		return false
	}
}

func runReadinessProbe(ctx context.Context, probe *corev1.Probe, target readinessTarget, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if probe.TCPSocket != nil {
		host := target.host
		if probe.TCPSocket.Host != "" {
			host = probe.TCPSocket.Host
		}
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(target.port)))
		if err != nil {
			return err
		}
		return conn.Close()
	}
	if probe.HTTPGet != nil {
		return runHTTPReadinessProbe(ctx, probe.HTTPGet, target)
	}
	return fmt.Errorf("unsupported readiness probe handler")
}

func runHTTPReadinessProbe(ctx context.Context, action *corev1.HTTPGetAction, target readinessTarget) error {
	scheme := "http"
	if action.Scheme == corev1.URISchemeHTTPS {
		scheme = "https"
	}
	host := target.host
	if action.Host != "" {
		host = action.Host
	}
	path := action.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	probeURL := (&url.URL{Scheme: scheme, Host: net.JoinHostPort(host, strconv.Itoa(target.port))}).String() + path

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "neocloud-node-probe")
	for _, header := range action.HTTPHeaders {
		if strings.EqualFold(header.Name, "Host") {
			request.Host = header.Value
			continue
		}
		request.Header.Add(header.Name, header.Value)
	}

	client := &http.Client{
		Transport: &http.Transport{
			// Like kubelet, HTTPS probes do not verify the certificate.
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 10*1024))
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("HTTP probe returned status %d", response.StatusCode)
	}
	return nil
}

func readinessProbeFor(pod *corev1.Pod) (corev1.Container, *corev1.Probe) {
	if pod == nil {
		return corev1.Container{}, nil
	}
	for _, container := range pod.Spec.Containers {
		if container.ReadinessProbe != nil {
			return container, container.ReadinessProbe
		}
	}
	return corev1.Container{}, nil
}

func probePort(container corev1.Container, probe *corev1.Probe) int {
	switch {
	case probe.HTTPGet != nil:
		return kube.ResolveContainerPort(container, probe.HTTPGet.Port)
	case probe.TCPSocket != nil:
		return kube.ResolveContainerPort(container, probe.TCPSocket.Port)
	default:
		return 0
	}
}

func readinessTargetFor(endpoints []core.Endpoint, port int) (readinessTarget, bool) {
	for _, endpoint := range endpoints {
		if port > 0 && endpoint.ContainerPort == port && endpoint.Host != "" && endpoint.Port > 0 {
			return readinessTarget{host: endpoint.Host, port: endpoint.Port}, true
		}
	}
	return readinessTarget{}, false
}

// setPodReadiness overrides the phase-based Ready and ContainersReady
// conditions with the readiness probe result.
func setPodReadiness(pod *corev1.Pod, ready bool, message string) {
	status := corev1.ConditionFalse
	reason := reasonReadinessPending
	if ready {
		status = corev1.ConditionTrue
		reason = ""
		message = ""
	} else if strings.HasPrefix(message, "readiness probe failed") {
		reason = reasonReadinessFailed
	}
	for index := range pod.Status.Conditions {
		condition := &pod.Status.Conditions[index]
		if condition.Type != corev1.PodReady && condition.Type != corev1.ContainersReady {
			continue
		}
		condition.Status = status
		condition.Reason = reason
		condition.Message = message
	}
	for index := range pod.Status.ContainerStatuses {
		pod.Status.ContainerStatuses[index].Ready = ready
	}
}

func atLeastOne(value int32, fallback int32) int32 {
	if value <= 0 {
		return fallback
	}
	return value
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	if duration <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
