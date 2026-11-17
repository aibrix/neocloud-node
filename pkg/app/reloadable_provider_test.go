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

func TestReloadableCreatePinsCredentialsDuringRecovery(t *testing.T) {
	var p *credentialProvider
	var creates, lists atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			creates.Add(1)
			if r.Header.Get("Authorization") != "Bearer test-old" {
				t.Error("create changed credentials")
			}
			p.update("test-new", "")
			http.Error(w, "lost response", http.StatusGatewayTimeout)
		case r.URL.Path == "/pods":
			lists.Add(1)
			if r.Header.Get("Authorization") != "Bearer test-old" {
				t.Error("create recovery changed credentials")
			}
			if creates.Load() == 0 {
				fmt.Fprint(w, `{"pods":[],"pagination":{"hasNextPage":false}}`)
				return
			}
			fmt.Fprint(w, `{"pods":[{"id":"remote","name":"neocloud-node-test-uid","status":"RUNNING"}],"pagination":{"hasNextPage":false}}`)
		default:
			if r.Header.Get("Authorization") != "Bearer test-new" {
				t.Error("next operation did not use new credentials")
			}
			fmt.Fprint(w, `{"id":"remote","status":"RUNNING"}`)
		}
	}))
	defer server.Close()
	p = newCredentialProvider(ProviderRunPod, func(key string) core.ProviderAdapter {
		return runpod.NewClient(runpod.Config{BaseURL: server.URL, APIKey: key})
	})
	p.update("test-old", "")
	adapter := wrapCredentialProvider(p)
	remote, err := adapter.Create(context.Background(), "test/uid", core.WorkloadRequest{Image: "example.test/image", GPUType: "NVIDIA A40", GPUCount: 1})
	if err != nil || remote.ID != "remote" {
		t.Fatalf("create=%s %v", remote.ID, err)
	}
	if _, err := adapter.Get(context.Background(), "remote"); err != nil {
		t.Fatal(err)
	}
	if creates.Load() != 1 || lists.Load() != 1 {
		t.Fatalf("POST=%d LIST=%d; credential reload must preserve adapter recovery requests", creates.Load(), lists.Load())
	}
}

func TestReloadableProviderPreservesSameAccountNotFoundSemantics(t *testing.T) {
	var gets, deletes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
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
	if _, err := adapter.Get(context.Background(), "existing"); !errors.Is(err, core.ErrRemoteNotFound) {
		t.Fatalf("same-account missing resource=%v", err)
	}
	if err := adapter.Delete(context.Background(), "existing"); err != nil {
		t.Fatalf("idempotent delete=%v", err)
	}
	if gets.Load() != 1 || deletes.Load() != 1 {
		t.Fatal("credential reload added remote existence probes")
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
