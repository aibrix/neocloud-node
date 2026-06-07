package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/vllm-project/aibrix/pkg/gpu-burst-node/pkg/core"
)

type CloudInitConfig struct {
	WorkloadID     string
	CallbackURL    string
	CallbackSecret string
	Request        core.WorkloadRequest
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
		"  - curl",
		"  - docker.io",
		"runcmd:",
		"  - " + strconv.Quote(command),
		"",
	}, "\n"), nil
}

func runnerCommand(config CloudInitConfig) string {
	image := shellQuote(config.Request.Image)
	payloadFormat := shellQuote(`{"workloadID":%s,"exitCode":%s}`)
	workloadID := shellQuote(strconv.Quote(config.WorkloadID))
	callbackHeaders := signedCallbackHeaders(config.CallbackSecret)

	return strings.Join([]string{
		"set -e",
		"docker pull " + image,
		"set +e",
		dockerRunCommand(config.Request),
		"code=$?",
		"payload=$(printf " + payloadFormat + " " + workloadID + ` "$code")`,
		signCallbackCommand(config.CallbackSecret),
		"curl -fsS --retry 60 --retry-all-errors --retry-delay 10 --max-time 10 -X POST " + shellQuote(config.CallbackURL) +
			" -H 'Content-Type: application/json'" +
			callbackHeaders +
			` -d "$payload"`,
		"exit $code",
	}, "; ")
}

func signCallbackCommand(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return ":"
	}
	python := `import hashlib,hmac,os; print("v1="+hmac.new(os.environ["SECRET"].encode(), (os.environ["TS"]+"."+os.environ["PAYLOAD"]).encode(), hashlib.sha256).hexdigest())`
	return "ts=$(date +%s); sig=$(SECRET=" + shellQuote(secret) + ` TS="$ts" PAYLOAD="$payload" python3 -c ` + shellQuote(python) + ")"
}

func signedCallbackHeaders(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	return ` -H "X-GPU-Burst-Runner-Timestamp: $ts" -H "X-GPU-Burst-Runner-Signature: $sig"`
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
