package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aibrix/neocloud-node/pkg/core"
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
	assertNotContains(t, cloudInit, "package_update: true")
	assertContains(t, cloudInit, "docker pull 'example.com/train:latest'")
	assertContains(t, cloudInit, "--gpus all")
	assertContains(t, cloudInit, "-e 'MODEL=llama'")
	assertContains(t, cloudInit, "--entrypoint 'python' 'example.com/train:latest' 'train.py' '--model' 'llama 3'")
	assertContains(t, cloudInit, "https://controller.example.com/v1/runner/status")
	assertContains(t, cloudInit, "--retry 60 --retry-all-errors --retry-delay 10 --max-time 10")
	assertContains(t, cloudInit, "default/pod-123")
}

func TestRunnerCommandBootstrapsOnlyWhenDockerOrNvidiaRuntimeIsMissing(t *testing.T) {
	t.Parallel()

	command := runnerCommand(CloudInitConfig{
		WorkloadID:  "default/pod-123",
		CallbackURL: "https://controller.example.com/v1/runner/status",
		Request:     core.WorkloadRequest{Image: "example.com/train:latest"},
	})

	assertContains(t, command, "if command -v docker >/dev/null 2>&1 && command -v nvidia-container-runtime >/dev/null 2>&1; then")
	assertContains(t, command, "runner_message='using preinstalled Docker and NVIDIA runtime'")
	assertContains(t, command, "apt-get install -y curl docker.io nvidia-container-runtime")
	assertContains(t, command, "runner_message='installed Docker and the NVIDIA runtime'")
	assertContains(t, command, "systemctl enable --now docker")
	assertContains(t, command, `payload=$(printf '{"workloadID":%s,"phase":"Running","message":"%s"}' '"default/pod-123"' "$runner_message")`)
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

	assertContains(t, command, `payload=$(printf '{"workloadID":%s,"phase":"Running","message":"%s"}' '"default/pod-123"' "$runner_message")`)
	assertContains(t, command, `if [ "$code" -eq 0 ]; then phase=Succeeded; else phase=Failed; fi`)
	assertContains(t, command, `payload=$(printf '{"workloadID":%s,"phase":"%s","exitCode":%s}' '"default/pod-123"' "$phase" "$code")`)
	assertContains(t, command, `-d "$payload"`)
	if strings.Contains(command, shellQuote(statusPayload("default/pod-123", "$code"))) {
		t.Fatalf("runner command keeps exit code in a single-quoted JSON payload:\n%s", command)
	}
}

func TestRunnerCommandSignsCallbackWhenSecretConfigured(t *testing.T) {
	t.Parallel()

	command := runnerCommand(CloudInitConfig{
		WorkloadID:     "default/pod-123",
		CallbackURL:    "https://controller.example.com/v1/runner/status",
		CallbackSecret: "shared-secret",
		Request: core.WorkloadRequest{
			Image: "example.com/train:latest",
		},
	})

	assertContains(t, command, `ts=$(date +%s)`)
	assertContains(t, command, `SECRET='shared-secret' TS="$ts" PAYLOAD="$payload" python3 -c`)
	assertContains(t, command, `X-NeoCloud-Runner-Timestamp: $ts`)
	assertContains(t, command, `X-NeoCloud-Runner-Signature: $sig`)
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

func assertNotContains(t *testing.T, s string, substr string) {
	t.Helper()

	if strings.Contains(s, substr) {
		t.Fatalf("cloud-init unexpectedly contains %q:\n%s", substr, s)
	}
}

func TestRetainedDockerLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		launch, wait int
		want         int
	}{
		{"container failure", 0, 0, 7}, {"launch failure", 9, 0, 9}, {"wait failure", 0, 1, 125},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("docker", "#!/bin/sh\ncase \"$1\" in\n run) printf '%s\\n' \"$@\" > \"$RUN_ARGS\"; exit \"$LAUNCH_EXIT\";;\n wait) echo 7; exit \"$WAIT_EXIT\";;\n esac\n")
			write("nvidia-container-runtime", "#!/bin/sh\nexit 0\n")
			write("curl", "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$CALLBACKS\"\n")
			cmd := exec.Command("sh", "-c", runnerCommand(CloudInitConfig{WorkloadID: "ns/uid", CallbackURL: "http://controller/status", Request: core.WorkloadRequest{Image: "image"}}))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "RUN_ARGS="+filepath.Join(dir, "args"), "CALLBACKS="+filepath.Join(dir, "callbacks"), fmt.Sprintf("LAUNCH_EXIT=%d", tc.launch), fmt.Sprintf("WAIT_EXIT=%d", tc.wait))
			out, err := cmd.CombinedOutput()
			if err == nil || cmd.ProcessState.ExitCode() != tc.want {
				t.Fatalf("exit=%v want=%d output=%s", err, tc.want, out)
			}
			args, err := os.ReadFile(filepath.Join(dir, "args"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"-d\n", "--log-driver\njson-file\n", "--log-opt\nmax-size=16m\n", "--log-opt\nmax-file=2\n"} {
				if !strings.Contains(string(args), want) {
					t.Fatalf("missing Docker retention option %q: %s", want, args)
				}
			}
			if strings.Contains(string(args), "--rm\n") {
				t.Fatal("exited container would lose its logs")
			}
			callbacks, err := os.ReadFile(filepath.Join(dir, "callbacks"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(callbacks), fmt.Sprintf(`"phase":"Failed","exitCode":%d`, tc.want)) {
				t.Fatalf("terminal callback mismatch: %s", callbacks)
			}
		})
	}
}

// Execute the generated shell so quoting errors and Docker option placement are
// observable at the process boundary, without needing a Docker daemon.
func TestDockerRunEntrypointAndPublishedPorts(t *testing.T) {
	for _, tt := range []struct {
		name          string
		command, args []string
		ports         []core.Port
		want          []string
	}{
		{name: "override", command: []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}, args: []string{"--model", "model's name", "$(false)"}, ports: []core.Port{{ContainerPort: 8000, Protocol: "TCP"}, {ContainerPort: 9000}}, want: []string{"--publish", "8000:8000/tcp", "--publish", "9000:9000/tcp", "--entrypoint", "python3", "image:tag", "-m", "vllm.entrypoints.openai.api_server", "--model", "model's name", "$(false)"}},
		{name: "image entrypoint", args: []string{"--model", "model name"}, want: []string{"image:tag", "--model", "model name"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nprintf '%s\\000' \"$@\"\nexit 9\n"), 0700); err != nil {
				t.Fatal(err)
			}
			request := core.WorkloadRequest{Image: "image:tag", Command: tt.command, Args: tt.args, Ports: tt.ports}
			cmd := exec.Command("sh", "-c", dockerRunCommand(request))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
			out, err := cmd.Output()
			if err == nil || cmd.ProcessState.ExitCode() != 9 {
				t.Fatalf("exit=%v, want 9", err)
			}
			got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
			want := append([]string{"run", "-d", "--log-driver", "json-file", "--log-opt", "max-size=16m", "--log-opt", "max-file=2", "--gpus", "all", "--name", "neocloud-node-workload"}, tt.want...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("docker argv=%#v want=%#v", got, want)
			}
		})
	}
}

func TestBuildCloudInitRejectsInvalidEntrypointAndPorts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command []string
		ports   []core.Port
		code    string
	}{
		{name: "empty entrypoint", command: []string{""}, code: "InvalidCommand"},
		{name: "blank entrypoint", command: []string{"  "}, code: "InvalidCommand"},
		{name: "zero", ports: []core.Port{{ContainerPort: 0}}, code: "InvalidPort"},
		{name: "negative", ports: []core.Port{{ContainerPort: -1}}, code: "InvalidPort"},
		{name: "overflow", ports: []core.Port{{ContainerPort: 65536}}, code: "InvalidPort"},
		{name: "UDP", ports: []core.Port{{ContainerPort: 8000, Protocol: "UDP"}}, code: "UnsupportedProtocol"},
		{name: "SCTP", ports: []core.Port{{ContainerPort: 8000, Protocol: "SCTP"}}, code: "UnsupportedProtocol"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildCloudInit(CloudInitConfig{WorkloadID: "ns/uid", CallbackURL: "http://controller/status", Request: core.WorkloadRequest{Image: "image", Command: tt.command, Ports: tt.ports}})
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Code != tt.code {
				t.Fatalf("error = %v, want ConfigError %s", err, tt.code)
			}
		})
	}
}
