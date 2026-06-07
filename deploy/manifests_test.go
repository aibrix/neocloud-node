package deploy_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestBaseManifestContainsRunnableControlPlaneResources(t *testing.T) {
	objects := readObjects(t, "gpu-burst-node.yaml")

	requireKindName(t, objects, "CustomResourceDefinition", "cloudpodbindings.gpu-burst-node.aibrix.ai")
	requireKindName(t, objects, "ServiceAccount", "gpu-burst-node")
	requireKindName(t, objects, "ClusterRole", "gpu-burst-node")
	requireKindName(t, objects, "ClusterRoleBinding", "gpu-burst-node")
	deployment := requireKindName(t, objects, "Deployment", "gpu-burst-node")

	containers := deployment["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})
	args := containers[0].(map[string]interface{})["args"].([]interface{})
	requireContains(t, args, "--provider=$(GPU_BURST_PROVIDER)")
	requireContains(t, args, "--binding-store=kubernetes")
	requireContains(t, args, "--node-name=$(GPU_BURST_NODE_NAME)")
	requireContains(t, args, "--orphan-sweeper-interval=$(GPU_BURST_ORPHAN_SWEEPER_INTERVAL)")
	requireContains(t, args, "--orphan-sweeper-dry-run=$(GPU_BURST_ORPHAN_SWEEPER_DRY_RUN)")
	requireContains(t, args, "--placement-offers=$(GPU_BURST_PLACEMENT_OFFERS)")
	requireContains(t, args, "--placement-offer-ttl=$(GPU_BURST_PLACEMENT_OFFER_TTL)")
	requireContains(t, args, "--placement-signal-cooldown=$(GPU_BURST_PLACEMENT_SIGNAL_COOLDOWN)")
}

func TestCRDHasStatusSubresourceAndPrinterColumns(t *testing.T) {
	objects := readObjects(t, "gpu-burst-node.yaml")
	crd := requireKindName(t, objects, "CustomResourceDefinition", "cloudpodbindings.gpu-burst-node.aibrix.ai")
	versions := crd["spec"].(map[string]interface{})["versions"].([]interface{})
	version := versions[0].(map[string]interface{})

	if version["name"] != "v1alpha1" {
		t.Fatalf("CRD version = %v, want v1alpha1", version["name"])
	}
	subresources := version["subresources"].(map[string]interface{})
	if _, ok := subresources["status"]; !ok {
		t.Fatalf("CRD subresources = %#v, want status subresource", subresources)
	}
	columns := version["additionalPrinterColumns"].([]interface{})
	if len(columns) < 4 {
		t.Fatalf("printer columns = %#v, want phase/provider/remoteID/deleting columns", columns)
	}
}

func TestExamplePodTargetsGpuBurstNode(t *testing.T) {
	objects := readObjects(t, "examples/fake-pod.yaml")
	pod := requireKindName(t, objects, "Pod", "gpu-burst-fake-smoke")
	spec := pod["spec"].(map[string]interface{})

	nodeSelector := spec["nodeSelector"].(map[string]interface{})
	if nodeSelector["gpu-burst-node.aibrix.ai/provider"] != "fake" {
		t.Fatalf("nodeSelector = %#v, want fake provider selector", nodeSelector)
	}
	tolerations := spec["tolerations"].([]interface{})
	if len(tolerations) != 1 {
		t.Fatalf("tolerations = %#v, want one provider toleration", tolerations)
	}
	toleration := tolerations[0].(map[string]interface{})
	if toleration["key"] != "gpu-burst-node.aibrix.ai/provider" || toleration["value"] != "fake" {
		t.Fatalf("toleration = %#v, want fake provider toleration", toleration)
	}
}

func readObjects(t *testing.T, path string) []map[string]interface{} {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []map[string]interface{}
	for {
		var object map[string]interface{}
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Decode(%q) error = %v", path, err)
		}
		if len(object) > 0 {
			objects = append(objects, object)
		}
	}
	return objects
}

func requireKindName(t *testing.T, objects []map[string]interface{}, kind string, name string) map[string]interface{} {
	t.Helper()

	for _, object := range objects {
		metadata := object["metadata"].(map[string]interface{})
		if object["kind"] == kind && metadata["name"] == name {
			return object
		}
	}
	t.Fatalf("object %s/%s not found in %#v", kind, name, objects)
	return nil
}

func requireContains(t *testing.T, values []interface{}, want string) {
	t.Helper()

	for _, value := range values {
		if value == want {
			return
		}
	}
	t.Fatalf("values = %#v, want %q", values, want)
}
