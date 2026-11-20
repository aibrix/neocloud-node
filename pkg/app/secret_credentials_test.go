package app

import (
	"context"
	"errors"
	"fmt"
	resource "github.com/aibrix/neocloud-node/pkg/resource"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestSecretPollingDeduplicatesAndHandlesLifecycle(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	options := Options{ProviderSecretNamespace: "system", SecretPollInterval: time.Second, SecretRequestTimeout: time.Second, ProviderSecrets: map[string]providerSecretRef{
		ProviderRunPod: {Name: "shared", Key: "runpod"}, ProviderLambda: {Name: "shared", Key: "lambda"},
	}}
	poller := newSecretCredentials(options, client)
	poller.refresh(ctx)
	if poller.providers[ProviderRunPod].available() {
		t.Fatal("missing Secret enabled provider")
	}
	if len(client.Actions()) != 1 {
		t.Fatalf("GETs = %d, want 1", len(client.Actions()))
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "system"}, Data: map[string][]byte{"runpod": []byte("test-first"), "lambda": []byte("test-lambda")}}
	if _, err := client.CoreV1().Secrets("system").Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	poller.refresh(ctx)
	v, err := poller.providers[ProviderRunPod].acquire("get")
	if err != nil || v.key != "test-first" {
		t.Fatal("Secret creation did not activate provider")
	}
	secret.Data["runpod"] = []byte("test-second")
	_, _ = client.CoreV1().Secrets("system").Update(ctx, secret, metav1.UpdateOptions{})
	poller.refresh(ctx)
	next, _ := poller.providers[ProviderRunPod].acquire("get")
	if next == v || next.key != "test-second" || v.key != "test-first" {
		t.Fatal("rotation mutated an in-flight credential")
	}
	delete(secret.Data, "runpod")
	_, _ = client.CoreV1().Secrets("system").Update(ctx, secret, metav1.UpdateOptions{})
	poller.refresh(ctx)
	if poller.providers[ProviderRunPod].available() || !poller.providers[ProviderLambda].available() {
		t.Fatal("missing key disabled wrong providers")
	}
	_ = client.CoreV1().Secrets("system").Delete(ctx, "shared", metav1.DeleteOptions{})
	poller.refresh(ctx)
	if poller.providers[ProviderLambda].available() {
		t.Fatal("deleted Secret still usable")
	}
	secret.Data["runpod"] = []byte("test-third")
	_, _ = client.CoreV1().Secrets("system").Create(ctx, secret, metav1.CreateOptions{})
	poller.refresh(ctx)
	if !poller.providers[ProviderRunPod].available() {
		t.Fatal("recreated Secret did not recover")
	}
}

func TestCredentialCatalogFiltersCachedOffersAfterSecretDeletion(t *testing.T) {
	poller := newSecretCredentials(Options{ProviderSecrets: map[string]providerSecretRef{ProviderRunPod: {Name: "provider", Key: "apiKey"}}}, fake.NewClientset())
	catalog := resource.NewObservedCatalog(resource.ObservedCatalogOptions{})
	catalog.Record(resource.OfferObservation{Offer: resource.Offer{ID: "runpod", Provider: ProviderRunPod}})
	catalog.Record(resource.OfferObservation{Offer: resource.Offer{ID: "vast", Provider: ProviderVast}})
	filtered := credentialCatalog{catalog: catalog, credentials: poller}
	source := poller.providers[ProviderRunPod]
	source.update("test-first", "")
	snapshot, err := filtered.Snapshot(context.Background(), resource.Intent{})
	if err != nil || len(snapshot.Offers) != 2 {
		t.Fatal("available provider excluded")
	}
	source.update("", "")
	snapshot, err = filtered.Snapshot(context.Background(), resource.Intent{})
	if err != nil || len(snapshot.Offers) != 1 || snapshot.Offers[0].Provider != ProviderVast {
		t.Fatal("cached offers still allow unavailable provider")
	}
	source.update("test-next", "")
	snapshot, err = filtered.Snapshot(context.Background(), resource.Intent{})
	if err != nil || len(snapshot.Offers) != 2 {
		t.Fatal("restored provider remains filtered")
	}
}

func TestSecretPollingRunsWithoutRestartAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := fake.NewClientset()
	options := Options{ProviderSecretNamespace: "system", SecretPollInterval: 10 * time.Millisecond, SecretRequestTimeout: time.Second, ProviderSecrets: map[string]providerSecretRef{ProviderRunPod: {Name: "provider", Key: "apiKey"}}}
	poller := newSecretCredentials(options, client)
	observed := make(chan bool, 10)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.run(ctx, func() { observed <- poller.providers[ProviderRunPod].available() })
	}()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "system"}, Data: map[string][]byte{"apiKey": []byte("test-key")}}
	_, _ = client.CoreV1().Secrets("system").Create(ctx, secret, metav1.CreateOptions{})
	wait := func(want bool) {
		t.Helper()
		select {
		case got := <-observed:
			if got != want {
				t.Fatalf("availability=%v want %v", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("polling did not observe transition")
		}
	}
	wait(true)
	_ = client.CoreV1().Secrets("system").Delete(ctx, "provider", metav1.DeleteOptions{})
	wait(false)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("polling loop did not stop")
	}
}

func TestSecretRotationChangesRealProviderRequests(t *testing.T) {
	for _, provider := range []string{ProviderRunPod, ProviderLambda, ProviderVast} {
		t.Run(provider, func(t *testing.T) {
			expected := "test-first"
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if provider == ProviderLambda {
					user, _, ok := r.BasicAuth()
					if !ok || user != expected {
						t.Error("Lambda did not use current credentials")
					}
				} else if r.Header.Get("Authorization") != "Bearer "+expected {
					t.Error("provider did not use current credentials")
				}
				switch provider {
				case ProviderRunPod:
					fmt.Fprint(w, `{"id":"remote","status":"RUNNING"}`)
				case ProviderLambda:
					fmt.Fprint(w, `{"data":{"id":"remote","status":"active","ip":"203.0.113.1"}}`)
				case ProviderVast:
					fmt.Fprint(w, `{"instances":{"id":1,"actual_status":"running"}}`)
				}
			}))
			defer server.Close()
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "system"}, Data: map[string][]byte{"apiKey": []byte(expected)}}
			client := fake.NewClientset(secret)
			options := Options{ProviderSecretNamespace: "system", ProviderSecrets: map[string]providerSecretRef{provider: {Name: "provider", Key: "apiKey"}}, RunPodBaseURL: server.URL, LambdaBaseURL: server.URL, VastBaseURL: server.URL}
			poller := newSecretCredentials(options, client)
			adapter, err := poller.adapter(provider)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Get(context.Background(), "remote"); err == nil {
				t.Fatal("request allowed before first Secret read")
			}
			if calls != 0 {
				t.Fatal("unauthenticated request sent")
			}
			poller.refresh(context.Background())
			if _, err := adapter.Get(context.Background(), "remote"); err != nil {
				t.Fatal(err)
			}
			expected = "test-next"
			secret.Data["apiKey"] = []byte(expected)
			_, _ = client.CoreV1().Secrets("system").Update(context.Background(), secret, metav1.UpdateOptions{})
			poller.refresh(context.Background())
			if _, err := adapter.Get(context.Background(), "remote"); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("provider requests=%d", calls)
			}
		})
	}
}

func TestSecretPollingReadFailureAndConcurrentRecovery(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "system"}, Data: map[string][]byte{"apiKey": []byte("test-key")}})
	options := Options{ProviderSecretNamespace: "system", SecretRequestTimeout: time.Second, ProviderSecrets: map[string]providerSecretRef{ProviderVast: {Name: "provider", Key: "apiKey"}}}
	poller := newSecretCredentials(options, client)
	poller.refresh(ctx)
	fail := false
	client.PrependReactor("get", "secrets", func(ktesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, errors.New("do not expose response body")
		}
		return false, nil, nil
	})
	fail = true
	poller.refresh(ctx)
	if poller.providers[ProviderVast].available() {
		t.Fatal("failed read must block new allocations")
	}
	if _, err := poller.providers[ProviderVast].acquire("delete"); err == nil || err.Error() == "do not expose response body" {
		t.Fatalf("unsafe read error %v", err)
	}
	fail = false
	poller.refresh(ctx)
	if !poller.providers[ProviderVast].available() {
		t.Fatal("successful read did not recover")
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				_, _ = poller.providers[ProviderVast].acquire("get")
			}
		})
	}
	for range 20 {
		poller.refresh(ctx)
	}
	wg.Wait()
}

func TestParseSecretReferences(t *testing.T) {
	options, err := ParseOptions([]string{"--provider=runpod"}, mapEnv(map[string]string{
		"GPU_BURST_PROVIDER_SECRET_NAMESPACE": "system", "RUNPOD_API_KEY_SECRET_NAME": "provider", "RUNPOD_API_KEY_SECRET_KEY": "token",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if options.ProviderSecrets[ProviderRunPod].Name != "provider" || options.ProviderSecrets[ProviderRunPod].Key != "token" || options.SecretPollInterval != 15*time.Second {
		t.Fatal("reference configuration lost")
	}
	if _, err := ParseOptions([]string{"--provider-secret-poll-interval=0"}, nil); err == nil {
		t.Fatal("zero poll interval accepted")
	}
}
