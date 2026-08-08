package kube

import (
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
			RestartPolicy: corev1.RestartPolicyAlways,
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
	if unsupported.Code != "UnsupportedRestartPolicy" {
		t.Fatalf("unsupported code = %q, want UnsupportedRestartPolicy", unsupported.Code)
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
