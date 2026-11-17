package app

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
	"github.com/aibrix/neocloud-node/pkg/providers/runpod"
)

func TestReloadableProviderMissingCredentialsPreservesBinding(t *testing.T) {
	p := newCredentialProvider(ProviderRunPod, func(string) core.ProviderAdapter { return core.NewFakeProvider(ProviderRunPod) })
	adapter := wrapCredentialProvider(p)
	store := core.NewMemoryBindingStore()
	_, _, _ = store.CreateOrGet(context.Background(), core.Binding{Namespace: "test", WorkloadUID: "uid", RemoteID: "remote", Phase: core.BindingRunning, Request: core.WorkloadRequest{Provider: ProviderRunPod}})
	service := core.NewLifecycleService(store, adapter)
	if err := service.EnsureDeleted(context.Background(), "test", "uid"); err == nil || errors.Is(err, core.ErrRemoteNotFound) {
		t.Fatalf("delete must fail with credentials unavailable: %v", err)
	}
	binding, err := store.GetByWorkloadUID(context.Background(), "test", "uid")
	if err != nil || binding.RemoteID != "remote" {
		t.Fatalf("lost binding: %#v %v", binding, err)
	}
}

func TestReloadableErrorDoesNotExposeEncodedCredential(t *testing.T) {
	p := newCredentialProvider(ProviderLambda, func(string) core.ProviderAdapter { return core.NewFakeProvider(ProviderLambda) })
	p.update("test-sensitive", "")
	v, _ := p.acquire("get")
	encoded := base64.StdEncoding.EncodeToString([]byte("test-sensitive:"))
	err := p.finish(v, "get", &core.ProviderError{Class: core.ErrorClassAuth, Message: "server echoed Basic " + encoded})
	if strings.Contains(err.Error(), encoded) {
		t.Fatal("credential-derived value reached error output")
	}
}

func TestReloadableCreatePinsCredentialsAndRecoversBeforeRetry(t *testing.T) {
	var p *credentialProvider
	var creates atomic.Int32
	var allocated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer test-old" {
				t.Error("create used wrong credential")
			}
			creates.Add(1)
			allocated.Store(true)
			p.update("test-new", "")
			http.Error(w, "lost response", http.StatusGatewayTimeout)
		case http.MethodGet:
			if allocated.Load() {
				fmt.Fprint(w, `{"pods":[{"id":"remote","name":"neocloud-node-test-uid","status":"RUNNING"}],"pagination":{"hasNextPage":false}}`)
			} else {
				fmt.Fprint(w, `{"pods":[],"pagination":{"hasNextPage":false}}`)
			}
		}
	}))
	defer server.Close()
	p = newCredentialProvider(ProviderRunPod, func(key string) core.ProviderAdapter {
		return runpod.NewClient(runpod.Config{BaseURL: server.URL, APIKey: key})
	})
	p.update("test-old", "")
	adapter := wrapCredentialProvider(p)
	request := core.WorkloadRequest{Image: "example.test/image", GPUType: "NVIDIA A40", GPUCount: 1}
	for range 2 {
		remote, err := adapter.Create(context.Background(), "test/uid", request)
		if err != nil || remote.ID != "remote" {
			t.Fatalf("create = %s, %v", remote.ID, err)
		}
	}
	if creates.Load() != 1 {
		t.Fatalf("allocated %d times for one UID", creates.Load())
	}
}

func TestReloadableFirstNotFoundWithNewCredentialsDoesNotCleanUp(t *testing.T) {
	var deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	p := newCredentialProvider(ProviderRunPod, func(key string) core.ProviderAdapter {
		return runpod.NewClient(runpod.Config{BaseURL: server.URL, APIKey: key})
	})
	p.update("test-key", "")
	adapter := wrapCredentialProvider(p)
	if _, err := adapter.Get(context.Background(), "existing"); err == nil || errors.Is(err, core.ErrRemoteNotFound) {
		t.Fatalf("unverified 404 = %v", err)
	}
	if err := adapter.Delete(context.Background(), "existing"); err == nil {
		t.Fatal("unverified delete succeeded")
	}
	if deletes.Load() != 0 {
		t.Fatal("sent delete before verifying account scope")
	}
}

func TestReloadableRetiredGetDoesNotOverwriteCurrentVerification(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var absent atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer test-old" {
			close(started)
			<-release
		}
		if absent.Load() {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"id":"remote","status":"RUNNING"}`)
	}))
	defer server.Close()
	p := newCredentialProvider(ProviderRunPod, func(key string) core.ProviderAdapter {
		return runpod.NewClient(runpod.Config{BaseURL: server.URL, APIKey: key})
	})
	p.update("test-old", "")
	adapter := wrapCredentialProvider(p)
	done := make(chan error, 1)
	go func() { _, err := adapter.Get(context.Background(), "remote"); done <- err }()
	<-started
	p.update("test-new", "")
	_, newErr := adapter.Get(context.Background(), "remote")
	close(release)
	if oldErr := <-done; oldErr != nil || newErr != nil {
		t.Fatal("fixture resource could not be verified")
	}
	absent.Store(true)
	if _, err := adapter.Get(context.Background(), "remote"); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Fatalf("retired response invalidated current proof: %v", err)
	}
}

func TestReloadableProviderRetiredAuthFailureCannotDisableNewCredentials(t *testing.T) {
	p := newCredentialProvider(ProviderRunPod, func(string) core.ProviderAdapter { return core.NewFakeProvider(ProviderRunPod) })
	p.update("test-old", "")
	old, err := p.acquire("get")
	if err != nil {
		t.Fatal(err)
	}
	p.update("test-new", "")
	err = p.finish(old, "get", &core.ProviderError{Class: core.ErrorClassAuth, Message: "rejected test-old"})
	if strings.Contains(err.Error(), "test-old") {
		t.Fatal("credential exposed")
	}
	if !p.available() {
		t.Fatal("retired response disabled new credentials")
	}
	current, _ := p.acquire("get")
	_ = p.finish(current, "get", &core.ProviderError{Class: core.ErrorClassAuth, Message: "bad key"})
	p.update("test-new", "")
	if p.available() {
		t.Fatal("unchanged poll reset authentication failure")
	}
	p.update("test-next", "")
	if !p.available() {
		t.Fatal("rotation did not recover")
	}
}

func TestReloadableProviderPreservesOptionalCapabilities(t *testing.T) {
	for _, name := range []string{ProviderRunPod, ProviderLambda, ProviderVast} {
		p := newCredentialProvider(name, func(string) core.ProviderAdapter { return core.NewFakeProvider(name) })
		adapter := wrapCredentialProvider(p)
		_, batch := adapter.(core.RemoteStatusLister)
		if batch != (name != ProviderRunPod) {
			t.Fatalf("%s batch status = %v", name, batch)
		}
		_, logs := adapter.(core.ProviderLogReader)
		if logs != (name != ProviderVast) {
			t.Fatalf("%s logs = %v", name, logs)
		}
		_, stop := adapter.(core.RemoteStopper)
		if stop != (name == ProviderRunPod) {
			t.Fatalf("%s stop = %v", name, stop)
		}
	}
}
