package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrEnvSourceNotFound is returned by an EnvSourceReader when the referenced
// Secret or ConfigMap does not exist.
var ErrEnvSourceNotFound = errors.New("environment source not found")

// EnvKeyRef references one key of a Secret or ConfigMap in the Pod namespace.
type EnvKeyRef struct {
	Name     string
	Key      string
	Optional bool
}

// EnvRef is an environment variable whose value comes from a Secret or
// ConfigMap. Only the reference is stored in the binding; the value is read
// when the remote workload is created.
type EnvRef struct {
	Name            string
	SecretKeyRef    *EnvKeyRef
	ConfigMapKeyRef *EnvKeyRef
}

// EnvFromRef imports every key of a Secret or ConfigMap as environment
// variables, optionally with a prefix.
type EnvFromRef struct {
	Prefix        string
	SecretName    string
	ConfigMapName string
	Optional      bool
}

// EnvSourceReader reads Secret and ConfigMap data from the Pod namespace.
type EnvSourceReader interface {
	SecretData(ctx context.Context, namespace string, name string) (map[string]string, error)
	ConfigMapData(ctx context.Context, namespace string, name string) (map[string]string, error)
}

const (
	envSourceSecret    = "Secret"
	envSourceConfigMap = "ConfigMap"
)

// ResolveEnv returns the environment for the provider, merged in kubelet
// order: envFrom sources first, then env entries. Later entries win.
// Keys imported through envFrom that are not valid variable names are skipped.
func ResolveEnv(ctx context.Context, reader EnvSourceReader, namespace string, request WorkloadRequest) (map[string]string, error) {
	if len(request.EnvRefs) == 0 && len(request.EnvFrom) == 0 {
		return cloneStringMap(request.Env), nil
	}
	if reader == nil {
		return nil, envConfigError("environment sources require a Kubernetes client")
	}

	cache := map[string]map[string]string{}
	load := func(kind string, name string) (map[string]string, bool, error) {
		cacheKey := kind + "/" + name
		if data, ok := cache[cacheKey]; ok {
			return data, data != nil, nil
		}
		var data map[string]string
		var err error
		if kind == envSourceSecret {
			data, err = reader.SecretData(ctx, namespace, name)
		} else {
			data, err = reader.ConfigMapData(ctx, namespace, name)
		}
		if errors.Is(err, ErrEnvSourceNotFound) {
			cache[cacheKey] = nil
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		if data == nil {
			data = map[string]string{}
		}
		cache[cacheKey] = data
		return data, true, nil
	}

	env := map[string]string{}
	for _, source := range request.EnvFrom {
		kind, name := envSourceSecret, source.SecretName
		if name == "" {
			kind, name = envSourceConfigMap, source.ConfigMapName
		}
		data, found, err := load(kind, name)
		if err != nil {
			return nil, err
		}
		if !found {
			if source.Optional {
				continue
			}
			return nil, envConfigError(fmt.Sprintf("%s %q not found", kind, name))
		}
		for key, value := range data {
			if variable := source.Prefix + key; isEnvVarName(variable) {
				env[variable] = value
			}
		}
	}
	for key, value := range request.Env {
		env[key] = value
	}
	for _, ref := range request.EnvRefs {
		kind, keyRef := envSourceSecret, ref.SecretKeyRef
		if keyRef == nil {
			kind, keyRef = envSourceConfigMap, ref.ConfigMapKeyRef
		}
		if keyRef == nil {
			return nil, envConfigError(fmt.Sprintf("environment variable %q has no source", ref.Name))
		}
		data, found, err := load(kind, keyRef.Name)
		if err != nil {
			return nil, err
		}
		if !found {
			if keyRef.Optional {
				continue
			}
			return nil, envConfigError(fmt.Sprintf("%s %q not found", kind, keyRef.Name))
		}
		value, ok := data[keyRef.Key]
		if !ok {
			if keyRef.Optional {
				continue
			}
			return nil, envConfigError(fmt.Sprintf("key %q not found in %s %q", keyRef.Key, kind, keyRef.Name))
		}
		env[ref.Name] = value
	}
	return env, nil
}

func envConfigError(message string) error {
	return &ProviderError{
		Provider:  "kubernetes",
		Operation: "resolve-env",
		Class:     ErrorClassConfig,
		Message:   message,
	}
}

// isEnvVarName matches Kubernetes environment variable name validation.
func isEnvVarName(name string) bool {
	if name == "" || name == "." || strings.HasPrefix(name, "..") {
		return false
	}
	for index, char := range name {
		switch {
		case char == '-' || char == '.' || char == '_':
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}
