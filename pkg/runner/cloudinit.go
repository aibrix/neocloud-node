package runner

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/aibrix/neocloud-node/pkg/core"
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

	if len(config.Request.Command) > 0 && strings.TrimSpace(config.Request.Command[0]) == "" {
		return "", &ConfigError{Code: "InvalidCommand", Message: "container entrypoint must not be empty"}
	}
	for _, port := range config.Request.Ports {
		if port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return "", &ConfigError{Code: "InvalidPort", Message: "container ports must be between 1 and 65535"}
		}
		if port.Protocol != "" && port.Protocol != "TCP" {
			return "", &ConfigError{Code: "UnsupportedProtocol", Message: "VM runner supports only TCP ports"}
		}
	}

	command := runnerCommand(config)
	return strings.Join([]string{
		"#cloud-config",
		"runcmd:",
		"  - " + strconv.Quote(command),
		"",
	}, "\n"), nil
}

func runnerCommand(config CloudInitConfig) string {
	bootstrap := bootstrapRunnerCommand()
	if config.CallbackSecret != "" {
		bootstrap += "; if ! command -v python3 >/dev/null 2>&1; then apt-get update && apt-get install -y python3; fi"
	}
	image := shellQuote(config.Request.Image)
	runningPayloadFormat := shellQuote(`{"workloadID":%s,"phase":"Running","message":"%s"}`)
	terminalPayloadFormat := shellQuote(`{"workloadID":%s,"phase":"%s","exitCode":%s}`)
	workloadID := shellQuote(strconv.Quote(config.WorkloadID))

	return strings.Join([]string{
		"set -e",
		bootstrap,
		callbackCommand(config, "$(printf "+runningPayloadFormat+" "+workloadID+` "$runner_message")`) + " || true",
		"docker pull " + image,
		"set +e",
		dockerRunCommand(config.Request),
		"code=$?",
		`if [ "$code" -eq 0 ]; then code=$(docker wait neocloud-node-workload) || code=125; fi`,
		`if [ "$code" -eq 0 ]; then phase=Succeeded; else phase=Failed; fi`,
		callbackCommand(config, "$(printf "+terminalPayloadFormat+" "+workloadID+` "$phase" "$code")`),
		"exit $code",
	}, "; ")
}

func bootstrapRunnerCommand() string {
	return strings.Join([]string{
		"if command -v docker >/dev/null 2>&1 && command -v nvidia-container-runtime >/dev/null 2>&1; then",
		`runner_message='using preinstalled Docker and NVIDIA runtime';`,
		"else",
		"export DEBIAN_FRONTEND=noninteractive;",
		"apt-get update;",
		"apt-get install -y curl docker.io nvidia-container-runtime;",
		"systemctl enable --now docker;",
		`runner_message='installed Docker and the NVIDIA runtime';`,
		"fi;",
		"if ! command -v curl >/dev/null 2>&1; then apt-get update && apt-get install -y curl; fi",
	}, " ")
}

func callbackCommand(config CloudInitConfig, payloadExpression string) string {
	return "payload=" + payloadExpression + "; " +
		signCallbackCommand(config.CallbackSecret) + "; " +
		"curl -fsS --retry 60 --retry-all-errors --retry-delay 10 --max-time 10 -X POST " + shellQuote(config.CallbackURL) +
		" -H 'Content-Type: application/json'" +
		signedCallbackHeaders(config.CallbackSecret) +
		` -d "$payload"`
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
	return ` -H "X-NeoCloud-Runner-Timestamp: $ts" -H "X-NeoCloud-Runner-Signature: $sig"`
}

func dockerRunCommand(request core.WorkloadRequest) string {
	parts := []string{"docker run -d --log-driver json-file --log-opt max-size=16m --log-opt max-file=2 --gpus all --name neocloud-node-workload"}
	for _, env := range sortedEnv(request.Env) {
		parts = append(parts, "-e "+shellQuote(env))
	}
	for _, port := range request.Ports {
		parts = append(parts, "--publish "+shellQuote(fmt.Sprintf("%d:%d/tcp", port.ContainerPort, port.ContainerPort)))
	}
	command := request.Command
	if len(command) > 0 {
		parts = append(parts, "--entrypoint "+shellQuote(command[0]))
		command = command[1:]
	}
	parts = append(parts, shellQuote(request.Image))
	for _, arg := range command {
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
