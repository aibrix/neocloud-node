package kube

import (
	"context"
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestEnvSourceReaderReadsSecretsAndConfigMaps(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	reader := NewEnvSourceReader(kubefake.NewSimpleClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "hf"},
			Data:       map[string][]byte{"token": []byte("hf-secret")},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: "settings"},
			Data:       map[string]string{"mode": "fast"},
		},
	).CoreV1())

	secret, err := reader.SecretData(ctx, "ml", "hf")
	if err != nil || !reflect.DeepEqual(secret, map[string]string{"token": "hf-secret"}) {
		t.Fatalf("SecretData() = %#v, %v; want token", secret, err)
	}
	configMap, err := reader.ConfigMapData(ctx, "ml", "settings")
	if err != nil || !reflect.DeepEqual(configMap, map[string]string{"mode": "fast"}) {
		t.Fatalf("ConfigMapData() = %#v, %v; want mode", configMap, err)
	}
	if _, err := reader.SecretData(ctx, "other", "hf"); !errors.Is(err, core.ErrEnvSourceNotFound) {
		t.Fatalf("SecretData(other namespace) error = %v, want ErrEnvSourceNotFound", err)
	}
	if _, err := reader.ConfigMapData(ctx, "ml", "absent"); !errors.Is(err, core.ErrEnvSourceNotFound) {
		t.Fatalf("ConfigMapData(absent) error = %v, want ErrEnvSourceNotFound", err)
	}
}
