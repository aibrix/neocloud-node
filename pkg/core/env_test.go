package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeEnvSources struct {
	secrets    map[string]map[string]string
	configMaps map[string]map[string]string
	reads      int
}

func (f *fakeEnvSources) SecretData(_ context.Context, namespace string, name string) (map[string]string, error) {
	f.reads++
	data, ok := f.secrets[namespace+"/"+name]
	if !ok {
		return nil, ErrEnvSourceNotFound
	}
	return data, nil
}

func (f *fakeEnvSources) ConfigMapData(_ context.Context, namespace string, name string) (map[string]string, error) {
	f.reads++
	data, ok := f.configMaps[namespace+"/"+name]
	if !ok {
		return nil, ErrEnvSourceNotFound
	}
	return data, nil
}

func TestResolveEnvMergesInKubeletOrder(t *testing.T) {
	t.Parallel()

	sources := &fakeEnvSources{
		secrets: map[string]map[string]string{
			"ml/hf": {"token": "hf-secret", "user": "bot"},
		},
		configMaps: map[string]map[string]string{
			"ml/settings": {"MODEL": "from-config", "LOG_LEVEL": "info", "1INVALID": "skipped", "bad key": "skipped"},
			"ml/extra":    {"MODEL": "from-extra"},
		},
	}
	request := WorkloadRequest{
		Env: map[string]string{"LOG_LEVEL": "debug"},
		EnvFrom: []EnvFromRef{
			{ConfigMapName: "settings"},
			{ConfigMapName: "extra"},
			{Prefix: "HF_", SecretName: "hf"},
			{SecretName: "missing", Optional: true},
		},
		EnvRefs: []EnvRef{
			{Name: "HF_TOKEN_ALIAS", SecretKeyRef: &EnvKeyRef{Name: "hf", Key: "token"}},
			{Name: "OPTIONAL_KEY", ConfigMapKeyRef: &EnvKeyRef{Name: "settings", Key: "absent", Optional: true}},
			{Name: "OPTIONAL_SOURCE", SecretKeyRef: &EnvKeyRef{Name: "missing", Key: "x", Optional: true}},
		},
	}

	env, err := ResolveEnv(context.Background(), sources, "ml", request)
	if err != nil {
		t.Fatalf("ResolveEnv() error = %v", err)
	}
	want := map[string]string{
		"MODEL":          "from-extra",
		"LOG_LEVEL":      "debug",
		"HF_token":       "hf-secret",
		"HF_user":        "bot",
		"HF_TOKEN_ALIAS": "hf-secret",
	}
	if !reflect.DeepEqual(env, want) {
		t.Fatalf("env = %#v, want %#v", env, want)
	}
	if sources.reads != 4 {
		t.Fatalf("source reads = %d, want 4 (each source read once)", sources.reads)
	}
}

func TestResolveEnvReportsMissingSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		request WorkloadRequest
	}{
		{
			name: "missing secret",
			request: WorkloadRequest{EnvRefs: []EnvRef{
				{Name: "TOKEN", SecretKeyRef: &EnvKeyRef{Name: "absent", Key: "token"}},
			}},
		},
		{
			name: "missing key",
			request: WorkloadRequest{EnvRefs: []EnvRef{
				{Name: "TOKEN", SecretKeyRef: &EnvKeyRef{Name: "hf", Key: "absent"}},
			}},
		},
		{
			name:    "missing envFrom config map",
			request: WorkloadRequest{EnvFrom: []EnvFromRef{{ConfigMapName: "absent"}}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sources := &fakeEnvSources{
				secrets: map[string]map[string]string{"ml/hf": {"token": "hf-secret"}},
			}
			_, err := ResolveEnv(context.Background(), sources, "ml", tt.request)
			if ClassifyError(err) != ErrorClassConfig {
				t.Fatalf("ResolveEnv() error = %v, want %s", err, ErrorClassConfig)
			}
		})
	}
}

func TestResolveEnvWithoutReader(t *testing.T) {
	t.Parallel()

	env, err := ResolveEnv(context.Background(), nil, "ml", WorkloadRequest{Env: map[string]string{"A": "1"}})
	if err != nil || !reflect.DeepEqual(env, map[string]string{"A": "1"}) {
		t.Fatalf("ResolveEnv(literal) = %#v, %v; want literal env", env, err)
	}

	_, err = ResolveEnv(context.Background(), nil, "ml", WorkloadRequest{
		EnvFrom: []EnvFromRef{{SecretName: "hf"}},
	})
	if ClassifyError(err) != ErrorClassConfig {
		t.Fatalf("ResolveEnv(references) error = %v, want %s", err, ErrorClassConfig)
	}
}

func TestResolveEnvPropagatesReadErrors(t *testing.T) {
	t.Parallel()

	readErr := errors.New("apiserver unavailable")
	_, err := ResolveEnv(context.Background(), failingEnvSources{err: readErr}, "ml", WorkloadRequest{
		EnvRefs: []EnvRef{{Name: "TOKEN", SecretKeyRef: &EnvKeyRef{Name: "hf", Key: "token"}}},
	})
	if !errors.Is(err, readErr) {
		t.Fatalf("ResolveEnv() error = %v, want %v", err, readErr)
	}
}

type failingEnvSources struct{ err error }

func (f failingEnvSources) SecretData(context.Context, string, string) (map[string]string, error) {
	return nil, f.err
}

func (f failingEnvSources) ConfigMapData(context.Context, string, string) (map[string]string, error) {
	return nil, f.err
}

func TestLifecycleSendsSecretValuesOnlyToProvider(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	sources := &fakeEnvSources{secrets: map[string]map[string]string{"ml/hf": {"token": "hf-secret"}}}
	service := NewLifecycleService(store, provider, WithEnvSourceReader(sources))

	binding, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "ml",
		Name:        "serve",
		WorkloadUID: "pod-env",
		Request: WorkloadRequest{
			Image:    "example.com/serve:latest",
			GPUCount: 1,
			Provider: "runpod",
			Env:      map[string]string{"MODEL": "qwen"},
			EnvRefs:  []EnvRef{{Name: "HF_TOKEN", SecretKeyRef: &EnvKeyRef{Name: "hf", Key: "token"}}},
		},
	})
	if err != nil {
		t.Fatalf("EnsureCreated() error = %v", err)
	}

	remote, err := provider.Get(ctx, binding.RemoteID)
	if err != nil {
		t.Fatalf("provider Get() error = %v", err)
	}
	wantEnv := map[string]string{"MODEL": "qwen", "HF_TOKEN": "hf-secret"}
	if !reflect.DeepEqual(remote.Request.Env, wantEnv) || len(remote.Request.EnvRefs) != 0 {
		t.Fatalf("provider request env = %#v refs=%#v, want resolved %#v", remote.Request.Env, remote.Request.EnvRefs, wantEnv)
	}

	stored, err := store.GetByWorkloadUID(ctx, "ml", "pod-env")
	if err != nil {
		t.Fatalf("GetByWorkloadUID() error = %v", err)
	}
	if _, leaked := stored.Request.Env["HF_TOKEN"]; leaked {
		t.Fatalf("stored env = %#v, must not contain the Secret value", stored.Request.Env)
	}
	if len(stored.Request.EnvRefs) != 1 || stored.Request.EnvRefs[0].SecretKeyRef.Name != "hf" {
		t.Fatalf("stored env refs = %#v, want the Secret reference", stored.Request.EnvRefs)
	}
}

func TestLifecycleFailsCreateWhenSecretIsMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := NewMemoryBindingStore()
	provider := NewFakeProvider("runpod")
	service := NewLifecycleService(store, provider, WithEnvSourceReader(&fakeEnvSources{}))

	binding, err := service.EnsureCreated(ctx, Binding{
		Namespace:   "ml",
		Name:        "serve",
		WorkloadUID: "pod-env-missing",
		Request: WorkloadRequest{
			Image:    "example.com/serve:latest",
			GPUCount: 1,
			Provider: "runpod",
			EnvRefs:  []EnvRef{{Name: "HF_TOKEN", SecretKeyRef: &EnvKeyRef{Name: "hf", Key: "token"}}},
		},
	})
	if ClassifyError(err) != ErrorClassConfig {
		t.Fatalf("EnsureCreated() error = %v, want %s", err, ErrorClassConfig)
	}
	if got := provider.CreateCount(); got != 0 {
		t.Fatalf("remote Create count = %d, want 0", got)
	}
	if binding.Phase != BindingFailed {
		t.Fatalf("phase = %s, want Failed", binding.Phase)
	}
	condition := FindCondition(binding.Conditions, ConditionProviderError)
	if condition == nil || condition.Reason != string(ErrorClassConfig) {
		t.Fatalf("provider error condition = %#v, want %s", condition, ErrorClassConfig)
	}
}
