package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

type CloudInitConfig struct {
	WorkloadID  string
	CallbackURL string
	Request     core.WorkloadRequest
}

type ConfigError struct {
	Code    string
	Message string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func BuildCloudInit(config CloudInitConfig) (string, error) {
	if strings.TrimSpace(config.WorkloadID) == "" {
		return "", &ConfigError{Code: "MissingWorkloadID", Message: "workload ID is required"}
	}
	if strings.TrimSpace(config.CallbackURL) == "" {
		return "", &ConfigError{Code: "MissingCallbackURL", Message: "callback URL is required"}
	}
	if strings.TrimSpace(config.Request.Image) == "" {
		return "", &ConfigError{Code: "MissingImage", Message: "container image is required"}
	}

	command := runnerCommand(config)
	return strings.Join([]string{
		"#cloud-config",
		"package_update: true",
		"packages:",
		"  - docker.io",
		"runcmd:",
		"  - " + strconv.Quote(command),
		"",
	}, "\n"), nil
}

func runnerCommand(config CloudInitConfig) string {
	image := shellQuote(config.Request.Image)

	return strings.Join([]string{
		"set -e",
		"docker pull " + image,
		"set +e",
		dockerRunCommand(config.Request),
		"code=$?",
		"curl -fsS -X POST " + shellQuote(config.CallbackURL) +
			" -H 'Content-Type: application/json'" +
			" -d " + shellQuote(statusPayload(config.WorkloadID, "$code")),
		"exit $code",
	}, "; ")
}

func dockerRunCommand(request core.WorkloadRequest) string {
	parts := []string{"docker run --rm --gpus all --name gpu-burst-workload"}
	for _, env := range sortedEnv(request.Env) {
		parts = append(parts, "-e "+shellQuote(env))
	}
	parts = append(parts, shellQuote(request.Image))
	for _, arg := range request.Command {
		parts = append(parts, shellQuote(arg))
	}
	for _, arg := range request.Args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func sortedEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

func statusPayload(workloadID string, exitCode string) string {
	return fmt.Sprintf(`{"workloadID":%q,"exitCode":%s}`, workloadID, exitCode)
}
