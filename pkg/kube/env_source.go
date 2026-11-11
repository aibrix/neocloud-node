package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/aibrix/neocloud-node/pkg/core"
)

// EnvSourceReader reads Secrets and ConfigMaps referenced by Pod environment
// variables. It reads them once, when the remote workload is created.
type EnvSourceReader struct {
	client corev1client.CoreV1Interface
}

var _ core.EnvSourceReader = (*EnvSourceReader)(nil)

func NewEnvSourceReader(client corev1client.CoreV1Interface) *EnvSourceReader {
	return &EnvSourceReader{client: client}
}

func (r *EnvSourceReader) SecretData(ctx context.Context, namespace string, name string) (map[string]string, error) {
	secret, err := r.client.Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, core.ErrEnvSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read Secret %s/%s: %w", namespace, name, err)
	}
	data := make(map[string]string, len(secret.Data))
	for key, value := range secret.Data {
		data[key] = string(value)
	}
	return data, nil
}

func (r *EnvSourceReader) ConfigMapData(ctx context.Context, namespace string, name string) (map[string]string, error) {
	configMap, err := r.client.ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, core.ErrEnvSourceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read ConfigMap %s/%s: %w", namespace, name, err)
	}
	data := make(map[string]string, len(configMap.Data))
	for key, value := range configMap.Data {
		data[key] = value
	}
	return data, nil
}
