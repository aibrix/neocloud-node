package deploy_test

import (
	"bytes"
	"os/exec"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestHelmProviderSecretReferences(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm is required for chart rendering")
	}
	command := exec.Command(helm, "template", "neocloud", "../charts/neocloud-node", "--namespace", "team-system", "--set", "provider=runpod", "--set", "credentials.runpod.secretName=shared", "--set", "credentials.runpod.key=runpod", "--set", "credentials.lambda.secretName=shared", "--set", "credentials.lambda.key=lambda")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, output)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(output), 4096)
	var deployment map[string]interface{}
	for {
		var object map[string]interface{}
		if err := decoder.Decode(&object); err != nil {
			break
		}
		if object["kind"] == "Secret" {
			t.Fatal("chart created a provider Secret")
		}
		if object["kind"] == "Deployment" {
			deployment = object
		}
	}
	if deployment == nil {
		t.Fatal("no controller Deployment rendered")
	}
	spec := deployment["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	container := spec["containers"].([]interface{})[0].(map[string]interface{})
	env := make(map[string]map[string]interface{})
	for _, item := range container["env"].([]interface{}) {
		entry := item.(map[string]interface{})
		env[entry["name"].(string)] = entry
	}
	for key, want := range map[string]string{"RUNPOD_API_KEY_SECRET_NAME": "shared", "RUNPOD_API_KEY_SECRET_KEY": "runpod", "LAMBDA_API_KEY_SECRET_NAME": "shared", "LAMBDA_API_KEY_SECRET_KEY": "lambda"} {
		if entry := env[key]; entry == nil || entry["value"] != want {
			t.Fatalf("%s reference not rendered", key)
		}
	}
	for _, name := range []string{"RUNPOD_API_KEY", "LAMBDA_API_KEY", "VAST_API_KEY_SECRET_NAME"} {
		if env[name] != nil {
			t.Fatalf("unexpected credential env %s", name)
		}
	}
	namespace := env["GPU_BURST_PROVIDER_SECRET_NAMESPACE"]
	if namespace == nil {
		t.Fatal("Secret namespace missing")
	}
	field := namespace["valueFrom"].(map[string]interface{})["fieldRef"].(map[string]interface{})["fieldPath"]
	if field != "metadata.namespace" {
		t.Fatalf("namespace field = %v", field)
	}
}
