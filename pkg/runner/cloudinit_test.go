package runner

import (
	"errors"
	"strings"
	"testing"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

func TestBuildCloudInit(t *testing.T) {
	t.Parallel()

	cloudInit, err := BuildCloudInit(CloudInitConfig{
		WorkloadID:  "default/pod-123",
		CallbackURL: "https://controller.example.com/v1/runner/status",
		Request: core.WorkloadRequest{
			Image:    "example.com/train:latest",
			Command:  []string{"python"},
			Args:     []string{"train.py", "--model", "llama 3"},
			Env:      map[string]string{"MODEL": "llama"},
			GPUCount: 1,
		},
	})
	if err != nil {
		t.Fatalf("BuildCloudInit returned error: %v", err)
	}

	assertContains(t, cloudInit, "#cloud-config")
	assertContains(t, cloudInit, "docker.io")
	assertContains(t, cloudInit, "curl")
	assertContains(t, cloudInit, "docker pull 'example.com/train:latest'")
	assertContains(t, cloudInit, "--gpus all")
	assertContains(t, cloudInit, "-e 'MODEL=llama'")
	assertContains(t, cloudInit, "'example.com/train:latest' 'python' 'train.py' '--model' 'llama 3'")
	assertContains(t, cloudInit, "https://controller.example.com/v1/runner/status")
	assertContains(t, cloudInit, "--retry 60 --retry-all-errors --retry-delay 10 --max-time 10")
	assertContains(t, cloudInit, "default/pod-123")
}

func TestRunnerCommandExpandsExitCodeInStatusPayload(t *testing.T) {
	t.Parallel()

	command := runnerCommand(CloudInitConfig{
		WorkloadID:  "default/pod-123",
		CallbackURL: "https://controller.example.com/v1/runner/status",
		Request: core.WorkloadRequest{
			Image: "example.com/train:latest",
		},
	})

	assertContains(t, command, `payload=$(printf '{"workloadID":%s,"exitCode":%s}' '"default/pod-123"' "$code")`)
	assertContains(t, command, `-d "$payload"`)
	if strings.Contains(command, shellQuote(statusPayload("default/pod-123", "$code"))) {
		t.Fatalf("runner command keeps exit code in a single-quoted JSON payload:\n%s", command)
	}
}

func TestBuildCloudInitValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		config CloudInitConfig
		code   string
	}{
		{
			name: "missing workload id",
			config: CloudInitConfig{
				CallbackURL: "https://controller.example.com/v1/runner/status",
				Request:     core.WorkloadRequest{Image: "example.com/train:latest"},
			},
			code: "MissingWorkloadID",
		},
		{
			name: "missing callback url",
			config: CloudInitConfig{
				WorkloadID: "default/pod-123",
				Request:    core.WorkloadRequest{Image: "example.com/train:latest"},
			},
			code: "MissingCallbackURL",
		},
		{
			name: "missing image",
			config: CloudInitConfig{
				WorkloadID:  "default/pod-123",
				CallbackURL: "https://controller.example.com/v1/runner/status",
			},
			code: "MissingImage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := BuildCloudInit(tt.config)
			var configErr *ConfigError
			if !errors.As(err, &configErr) {
				t.Fatalf("BuildCloudInit error = %v, want ConfigError", err)
			}
			if configErr.Code != tt.code {
				t.Fatalf("config error code = %q, want %q", configErr.Code, tt.code)
			}
		})
	}
}

func assertContains(t *testing.T, s string, substr string) {
	t.Helper()

	if !strings.Contains(s, substr) {
		t.Fatalf("cloud-init does not contain %q:\n%s", substr, s)
	}
}
