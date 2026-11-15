package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/logstream"
	"github.com/aibrix/neocloud-node/pkg/vk"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
)

func TestKubeletSelfSignedDevelopmentModeAndAddress(t *testing.T) {
	o, err := ParseOptions([]string{"--enable-kubelet-api", "--kubelet-auth-mode=none", "--pod-ip=10.0.0.8"}, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsConfig(o)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := cert.VerifyHostname("10.0.0.8"); err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Fatal("weak TLS")
	}
	nodeCfg := &nodeutil.NodeConfig{}
	if err := configureNodeSpec(nodeCfg, o); err != nil {
		t.Fatal(err)
	}
	if len(nodeCfg.NodeSpec.Status.Addresses) != 1 || nodeCfg.NodeSpec.Status.Addresses[0].Type != corev1.NodeInternalIP || nodeCfg.NodeSpec.Status.Addresses[0].Address != "10.0.0.8" || nodeCfg.NodeSpec.Status.DaemonEndpoints.KubeletEndpoint.Port != 10250 {
		t.Fatalf("node status=%+v", nodeCfg.NodeSpec.Status)
	}
}

func TestKubeletRequiresExplicitAuthAndValidCertificatePair(t *testing.T) {
	for _, args := range [][]string{
		{"--enable-kubelet-api", "--pod-ip=10.0.0.8"},
		{"--enable-kubelet-api", "--kubelet-auth-mode=none"},
		{"--enable-kubelet-api", "--kubelet-auth-mode=none", "--pod-ip=invalid"},
		{"--enable-kubelet-api", "--kubelet-auth-mode=none", "--pod-ip=10.0.0.8", "--tls-cert-file=missing"},
	} {
		if _, err := ParseOptions(args, emptyEnv); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
	o, err := ParseOptions([]string{"--enable-kubelet-api", "--kubelet-auth-mode=none", "--pod-ip=10.0.0.8", "--tls-cert-file=missing", "--tls-key-file=missing"}, emptyEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tlsConfig(o); err == nil {
		t.Fatal("explicit missing certificates silently fell back")
	}
}

func TestKubeletLogRoutePreservesExplicitZeroTail(t *testing.T) {
	// VK's ContainerLogOpts.Tail collapses omitted tailLines and explicit zero.
	// Verify our actual HTTP boundary restores that distinction for the provider.
	for _, query := range []string{"", "?tailLines=0"} {
		p := &tailProbeProvider{}
		mux := http.NewServeMux()
		api.AttachPodRoutes(api.PodHandlerConfig{GetContainerLogs: p.GetContainerLogs}, mux, false)
		h := preserveLogTail(mux)
		req := httptest.NewRequest(http.MethodGet, "/containerLogs/default/pod/container"+query, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK || p.explicit != (query != "") {
			t.Fatalf("query=%q code=%d body=%s explicit=%t", query, w.Code, w.Body.String(), p.explicit)
		}
	}
}

func TestKubeletDevelopmentEndpointExposesOnlyContainerLogs(t *testing.T) {
	h := preserveLogTail(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "private pod state") }))
	for _, path := range []string{"/pods", "/stats/summary", "/exec/default/pod/container", "/neocloud-node/v1/runner/status"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("path=%s code=%d body=%s", path, w.Code, w.Body.String())
		}
	}
}

// This probe delegates to the real VK context conversion without cloud allocation.
type tailProbeProvider struct{ explicit bool }

func (p *tailProbeProvider) GetContainerLogs(ctx context.Context, namespace, pod, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	p.explicit = vk.LogTailSpecified(ctx)
	return io.NopCloser(strings.NewReader("output\n")), nil
}

func TestKubeletLogStreamFailureAbortsResponse(t *testing.T) {
	mux := http.NewServeMux()
	api.AttachPodRoutes(api.PodHandlerConfig{GetContainerLogs: func(ctx context.Context, namespace, pod, container string, o api.ContainerLogOpts) (io.ReadCloser, error) {
		return logstream.Open(ctx, core.LogOptions{Follow: true}, func(ctx context.Context, emit func(logstream.Event) error) error {
			if err := emit(logstream.Event{Line: "real log"}); err != nil {
				return err
			}
			return errors.New("upstream transport failed")
		}, logstream.Timing{})
	}}, mux, false)
	server := httptest.NewServer(preserveLogTail(mux))
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/containerLogs/default/pod/container?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("transport failure became successful logs: status=%d body=%q", resp.StatusCode, b)
	}
	if string(b) != "real log\n" {
		t.Fatalf("controller error text leaked into container output: %q", b)
	}
}

func TestKubeletLogFailureBeforeOutputReturnsServerError(t *testing.T) {
	mux := http.NewServeMux()
	api.AttachPodRoutes(api.PodHandlerConfig{GetContainerLogs: func(ctx context.Context, namespace, pod, container string, o api.ContainerLogOpts) (io.ReadCloser, error) {
		return logstream.Open(ctx, core.LogOptions{}, func(context.Context, func(logstream.Event) error) error { return errors.New("upstream unavailable") }, logstream.Timing{})
	}}, mux, false)
	server := httptest.NewServer(preserveLogTail(mux))
	defer server.Close()
	resp, err := server.Client().Get(server.URL + "/containerLogs/default/pod/container")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("early failure status=%d body=%q", resp.StatusCode, b)
	}
}
