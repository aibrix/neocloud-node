package vk

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestReadinessWorkerHonorsThresholds(t *testing.T) {
	t.Parallel()

	// Probe results in order: fail, ok, ok, fail, fail, ok, ...
	script := []int{http.StatusServiceUnavailable, http.StatusOK, http.StatusOK,
		http.StatusServiceUnavailable, http.StatusServiceUnavailable}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("probe path = %q, want /health", r.URL.Path)
		}
		index := int(requests.Add(1)) - 1
		status := http.StatusOK
		if index < len(script) {
			status = script[index]
		}
		w.WriteHeader(status)
	}))
	defer server.Close()

	worker := &readinessWorker{target: serverTarget(t, server.URL)}
	probe := &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health"}},
		PeriodSeconds:    1,
		TimeoutSeconds:   100,
		SuccessThreshold: 2,
		FailureThreshold: 2,
	}

	type transition struct {
		ready   bool
		request int32
	}
	var mu sync.Mutex
	var transitions []transition
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.run(ctx, probe, time.Millisecond, func() {
			ready, _ := worker.result()
			mu.Lock()
			transitions = append(transitions, transition{ready: ready, request: requests.Load()})
			if len(transitions) == 2 {
				cancel()
			}
			mu.Unlock()
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("worker did not report two transitions")
	}
	mu.Lock()
	defer mu.Unlock()
	want := []transition{{ready: true, request: 3}, {ready: false, request: 5}}
	if len(transitions) != len(want) {
		t.Fatalf("transitions = %#v, want %#v", transitions, want)
	}
	for index := range want {
		if transitions[index] != want[index] {
			t.Fatalf("transitions = %#v, want %#v", transitions, want)
		}
	}
}

func TestRunReadinessProbeHandlers(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if r.Header.Get("X-Probe") != "yes" || r.Host != "model.example" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		case "/redirect":
			http.Redirect(w, r, "http://elsewhere.example/", http.StatusFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(server.Close)
	target := serverTarget(t, server.URL)

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()

	tests := []struct {
		name    string
		probe   *corev1.Probe
		target  readinessTarget
		wantErr bool
	}{
		{
			name: "http success with headers",
			probe: httpProbe("/ok", corev1.HTTPHeader{Name: "X-Probe", Value: "yes"},
				corev1.HTTPHeader{Name: "Host", Value: "model.example"}),
			target: target,
		},
		{name: "http redirect counts as success", probe: httpProbe("/redirect"), target: target},
		{name: "http server error", probe: httpProbe("/fail"), target: target, wantErr: true},
		{
			name:   "tcp open port",
			probe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{}}},
			target: target,
		},
		{
			name:    "tcp closed port",
			probe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{}}},
			target:  readinessTarget{host: "127.0.0.1", port: closedPort},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := runReadinessProbe(context.Background(), tt.probe, tt.target, 2*time.Second)
			if (err != nil) != tt.wantErr {
				t.Fatalf("runReadinessProbe() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
	}
}

func TestReadinessProbeControlsPodReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var healthy atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	target := serverTarget(t, server.URL)

	adapter := core.NewFakeProvider("runpod")
	provider := newTestProviderWithConfig(t, Config{Adapter: adapter, StatusSyncContext: ctx})
	provider.readiness.timeUnit = 5 * time.Millisecond

	var mu sync.Mutex
	var readyUpdates []corev1.ConditionStatus
	provider.NotifyPods(ctx, func(pod *corev1.Pod) {
		mu.Lock()
		defer mu.Unlock()
		readyUpdates = append(readyUpdates, podReady(pod.Status.Conditions))
	})

	pod := gpuPod("serve", "uid-serve-probe")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolTCP}}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: "/health",
			Port: intstr.FromString("http"),
		}},
		PeriodSeconds:    1,
		TimeoutSeconds:   100,
		SuccessThreshold: 1,
		FailureThreshold: 2,
	}
	if err := provider.CreatePod(ctx, pod); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}

	// Running but the provider has not published the probe port yet.
	status, err := provider.GetPodStatus(ctx, "default", "serve")
	if err != nil {
		t.Fatalf("GetPodStatus() error = %v", err)
	}
	if status.Phase != corev1.PodRunning {
		t.Fatalf("phase = %s, want Running", status.Phase)
	}
	if ready := podReadyCondition(status.Conditions); ready.Status != corev1.ConditionFalse || ready.Reason != reasonReadinessPending {
		t.Fatalf("PodReady = %s/%s, want False/%s", ready.Status, ready.Reason, reasonReadinessPending)
	}

	binding, err := provider.lifecycle.SyncStatus(ctx, "default", "uid-serve-probe")
	if err != nil {
		t.Fatalf("SyncStatus() setup error = %v", err)
	}
	if err := adapter.SetEndpoints(binding.RemoteID, []core.Endpoint{
		{ContainerPort: 8000, Protocol: "TCP", Host: target.host, Port: target.port},
	}); err != nil {
		t.Fatalf("SetEndpoints() setup error = %v", err)
	}
	status, err = provider.GetPodStatus(ctx, "default", "serve")
	if err != nil {
		t.Fatalf("GetPodStatus() error = %v", err)
	}
	if ready := podReadyCondition(status.Conditions); ready.Status != corev1.ConditionFalse {
		t.Fatalf("PodReady while model loads = %s, want False", ready.Status)
	}

	healthy.Store(true)
	waitForCachedReady(t, provider, "serve", corev1.ConditionTrue)
	cached := provider.getCachedPod(podKey("default", "serve"))
	if !cached.Status.ContainerStatuses[0].Ready {
		t.Fatal("container Ready = false, want true")
	}

	healthy.Store(false)
	waitForCachedReady(t, provider, "serve", corev1.ConditionFalse)
	cached = provider.getCachedPod(podKey("default", "serve"))
	if ready := podReadyCondition(cached.Status.Conditions); ready.Reason != reasonReadinessFailed {
		t.Fatalf("PodReady reason = %q, want %q", ready.Reason, reasonReadinessFailed)
	}

	mu.Lock()
	defer mu.Unlock()
	if !containsTransition(readyUpdates, corev1.ConditionFalse, corev1.ConditionTrue) ||
		!containsTransition(readyUpdates, corev1.ConditionTrue, corev1.ConditionFalse) {
		t.Fatalf("notified Ready updates = %v, want False->True->False", readyUpdates)
	}
}

func TestPodWithoutReadinessProbeKeepsPhaseReadiness(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	provider := newTestProvider(t, core.NewFakeProvider("runpod"))
	if err := provider.CreatePod(ctx, gpuPod("train", "uid-train")); err != nil {
		t.Fatalf("CreatePod() error = %v", err)
	}
	status, err := provider.GetPodStatus(ctx, "default", "train")
	if err != nil {
		t.Fatalf("GetPodStatus() error = %v", err)
	}
	if ready := podReadyCondition(status.Conditions); ready.Status != corev1.ConditionTrue {
		t.Fatalf("PodReady = %s, want True for a running Pod without probe", ready.Status)
	}
}

func httpProbe(path string, headers ...corev1.HTTPHeader) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
		Path:        path,
		HTTPHeaders: headers,
	}}}
}

func serverTarget(t *testing.T, rawURL string) readinessTarget {
	t.Helper()

	host, port, err := net.SplitHostPort(rawURL[len("http://"):])
	if err != nil {
		t.Fatalf("split server address: %v", err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	return readinessTarget{host: host, port: portNumber}
}

func waitForCachedReady(t *testing.T, provider *Provider, name string, want corev1.ConditionStatus) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cached := provider.getCachedPod(podKey("default", name))
		if cached != nil && podReady(cached.Status.Conditions) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("PodReady did not become %s", want)
}

func containsTransition(values []corev1.ConditionStatus, from corev1.ConditionStatus, to corev1.ConditionStatus) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] == from && values[index] == to {
			return true
		}
	}
	return false
}
