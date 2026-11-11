package kube

import (
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/aibrix/neocloud-node/pkg/core"
)

func TestTransformerTransformsCoreV1Pod(t *testing.T) {
	t.Parallel()

	gpuName := corev1.ResourceName("nvidia.com/gpu")
	privileged := false
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "train-job",
			UID:       types.UID("pod-123"),
			Annotations: map[string]string{
				core.AnnotationProvider: "runpod",
				core.AnnotationGPUType:  "NVIDIA H100",
				core.AnnotationMaxPrice: "4.50",
				core.AnnotationRegion:   "us",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			ImagePullSecrets: []corev1.LocalObjectReference{
				{Name: "registry-creds"},
			},
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "example.com/train:latest",
					Command: []string{"python"},
					Args:    []string{"train.py"},
					Env: []corev1.EnvVar{
						{Name: "MODEL", Value: "llama"},
					},
					Ports: []corev1.ContainerPort{
						{Name: "metrics", ContainerPort: 9090, Protocol: corev1.ProtocolTCP},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							gpuName: resource.MustParse("2"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						Privileged: &privileged,
					},
				},
			},
		},
	}

	transformer := Transformer{}
	binding, err := transformer.TransformPod(pod)
	if err != nil {
		t.Fatalf("TransformPod returned error: %v", err)
	}

	if binding.Namespace != "default" || binding.Name != "train-job" || binding.WorkloadUID != "pod-123" {
		t.Fatalf("binding identity = %#v, want default/train-job/pod-123", binding)
	}
	if binding.Request.Image != "example.com/train:latest" {
		t.Fatalf("image = %q, want example.com/train:latest", binding.Request.Image)
	}
	if binding.Request.Env["MODEL"] != "llama" {
		t.Fatalf("env = %#v, want MODEL=llama", binding.Request.Env)
	}
	if binding.Request.GPUCount != 2 {
		t.Fatalf("gpu count = %d, want 2", binding.Request.GPUCount)
	}
	if binding.Request.RestartPolicy != string(core.RestartPolicyOnFailure) {
		t.Fatalf("restart policy = %q, want OnFailure", binding.Request.RestartPolicy)
	}
	if len(binding.Request.ImagePullSecrets) != 1 || binding.Request.ImagePullSecrets[0] != "registry-creds" {
		t.Fatalf("image pull secrets = %#v, want registry-creds", binding.Request.ImagePullSecrets)
	}
	if len(binding.Request.Ports) != 1 || binding.Request.Ports[0].ContainerPort != 9090 {
		t.Fatalf("ports = %#v, want one 9090 port", binding.Request.Ports)
	}
}

func TestTransformerPreservesUnsupportedCoreV1PodErrors(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "train-job",
			UID:       types.UID("pod-123"),
		},
		Spec: corev1.PodSpec{
			HostNetwork: true,
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "example.com/train:latest",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
						},
					},
				},
			},
		},
	}

	_, err := (Transformer{}).TransformPod(pod)
	var unsupported *core.UnsupportedSpecError
	if !errors.As(err, &unsupported) {
		t.Fatalf("TransformPod error = %v, want UnsupportedSpecError", err)
	}
	if unsupported.Code != "UnsupportedHostNetwork" {
		t.Fatalf("unsupported code = %q, want UnsupportedHostNetwork", unsupported.Code)
	}
}

func TestTransformerAcceptsDeploymentManagedPod(t *testing.T) {
	t.Parallel()

	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "qwen-7d9f8-abcde",
			UID:       types.UID("pod-serve"),
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "qwen-7d9f8", UID: "rs-uid", Controller: &controller},
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{
				{
					Name:  "vllm",
					Image: "vllm/vllm-openai:v0.32.0",
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8000, Protocol: corev1.ProtocolTCP}},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
						},
					},
				},
			},
		},
	}

	binding, err := (Transformer{}).TransformPod(pod)
	if err != nil {
		t.Fatalf("TransformPod() error = %v", err)
	}
	if binding.Request.RestartPolicy != string(core.RestartPolicyAlways) {
		t.Fatalf("restart policy = %q, want Always", binding.Request.RestartPolicy)
	}
}

func TestTransformerResolvesReadinessProbePort(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		probe    *corev1.Probe
		wantCode string
	}{
		{
			name: "named http port",
			probe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("http")},
			}},
		},
		{
			name: "numeric tcp port",
			probe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8000)},
			}},
		},
		{
			name: "unknown named port",
			probe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/health", Port: intstr.FromString("metrics")},
			}},
			wantCode: "UnsupportedReadinessProbe",
		},
		{
			name: "exec",
			probe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"true"}},
			}},
			wantCode: "UnsupportedReadinessProbe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "serve", UID: types.UID("pod-serve")},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:           "vllm",
						Image:          "vllm/vllm-openai:v0.32.0",
						Ports:          []corev1.ContainerPort{{Name: "http", ContainerPort: 8000}},
						ReadinessProbe: tt.probe,
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
							},
						},
					}},
				},
			}

			_, err := (Transformer{}).TransformPod(pod)
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("TransformPod() error = %v", err)
				}
				return
			}
			var unsupported *core.UnsupportedSpecError
			if !errors.As(err, &unsupported) || unsupported.Code != tt.wantCode {
				t.Fatalf("TransformPod() error = %v, want %s", err, tt.wantCode)
			}
		})
	}
}

func TestTransformerIgnoresDefaultServiceAccountProjectedVolume(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "train-job",
			UID:       types.UID("pod-123"),
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes: []corev1.Volume{
				{
					Name: "kube-api-access-abcde",
					VolumeSource: corev1.VolumeSource{
						Projected: &corev1.ProjectedVolumeSource{
							Sources: []corev1.VolumeProjection{
								{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{}},
								{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}},
								{DownwardAPI: &corev1.DownwardAPIProjection{}},
							},
						},
					},
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "main",
					Image: "example.com/train:latest",
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "kube-api-access-abcde",
							MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
							ReadOnly:  true,
						},
					},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
						},
					},
				},
			},
		},
	}

	binding, err := (Transformer{}).TransformPod(pod)
	if err != nil {
		t.Fatalf("TransformPod returned error: %v", err)
	}
	if binding.Request.Image != "example.com/train:latest" {
		t.Fatalf("request image = %q, want example.com/train:latest", binding.Request.Image)
	}
}

func TestTransformerRejectsUnresolvableEnvironmentSources(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		valueFrom *corev1.EnvVarSource
	}{
		{"resource field", &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
		{"pod IP", &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{"host IP", &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"}}},
		{"file key", &corev1.EnvVarSource{FileKeyRef: &corev1.FileKeySelector{VolumeName: "env", Path: "a.env", Key: "A"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Image:     "example.com/train:latest",
				Env:       []corev1.EnvVar{{Name: "LITERAL", Value: "kept"}, {Name: "SOURCE", ValueFrom: tt.valueFrom}},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")}},
			}}}}
			_, err := (Transformer{}).TransformPod(pod)
			var unsupported *core.UnsupportedSpecError
			if !errors.As(err, &unsupported) {
				t.Fatalf("TransformPod error = %v, want UnsupportedSpecError", err)
			}
			if unsupported.Code != "UnsupportedEnvValueFrom" || unsupported.Field != "spec.containers[0].env[1].valueFrom" {
				t.Fatalf("unsupported error = %#v, want UnsupportedEnvValueFrom at env[1]", unsupported)
			}
		})
	}
}

func TestTransformerMapsEnvironmentSources(t *testing.T) {
	t.Parallel()

	optional := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "ml",
			Name:        "serve",
			UID:         types.UID("pod-serve"),
			Labels:      map[string]string{"app": "qwen"},
			Annotations: map[string]string{"team": "inference"},
		},
		Spec: corev1.PodSpec{
			NodeName:           "neocloud-node",
			ServiceAccountName: "serving",
			Containers: []corev1.Container{{
				Image: "vllm/vllm-openai:v0.32.0",
				EnvFrom: []corev1.EnvFromSource{
					{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}}},
					{Prefix: "HF_", SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "hf"}, Optional: &optional,
					}},
				},
				Env: []corev1.EnvVar{
					{Name: "TOKEN", Value: "literal-first"},
					{Name: "TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "hf"}, Key: "token",
					}}},
					{Name: "MODE", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "mode", Optional: &optional,
					}}},
					{Name: "MODE", Value: "literal-last"},
					{Name: "POD_NAME", ValueFrom: fieldRef("metadata.name")},
					{Name: "POD_NAMESPACE", ValueFrom: fieldRef("metadata.namespace")},
					{Name: "POD_UID", ValueFrom: fieldRef("metadata.uid")},
					{Name: "APP", ValueFrom: fieldRef("metadata.labels['app']")},
					{Name: "TEAM", ValueFrom: fieldRef("metadata.annotations['team']")},
					{Name: "NODE", ValueFrom: fieldRef("spec.nodeName")},
					{Name: "SA", ValueFrom: fieldRef("spec.serviceAccountName")},
				},
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")}},
			}},
		},
	}

	binding, err := (Transformer{}).TransformPod(pod)
	if err != nil {
		t.Fatalf("TransformPod() error = %v", err)
	}
	wantEnv := map[string]string{
		"MODE":          "literal-last",
		"POD_NAME":      "serve",
		"POD_NAMESPACE": "ml",
		"POD_UID":       "pod-serve",
		"APP":           "qwen",
		"TEAM":          "inference",
		"NODE":          "neocloud-node",
		"SA":            "serving",
	}
	if !reflect.DeepEqual(binding.Request.Env, wantEnv) {
		t.Fatalf("env = %#v, want %#v", binding.Request.Env, wantEnv)
	}
	wantRefs := []core.EnvRef{{Name: "TOKEN", SecretKeyRef: &core.EnvKeyRef{Name: "hf", Key: "token"}}}
	if !reflect.DeepEqual(binding.Request.EnvRefs, wantRefs) {
		t.Fatalf("env refs = %#v, want %#v", binding.Request.EnvRefs, wantRefs)
	}
	wantEnvFrom := []core.EnvFromRef{
		{ConfigMapName: "settings"},
		{Prefix: "HF_", SecretName: "hf", Optional: true},
	}
	if !reflect.DeepEqual(binding.Request.EnvFrom, wantEnvFrom) {
		t.Fatalf("envFrom = %#v, want %#v", binding.Request.EnvFrom, wantEnvFrom)
	}
}

func fieldRef(path string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: path}}
}
