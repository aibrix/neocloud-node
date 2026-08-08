package deploy_test

import (
	"bytes"
	"io"
	"os"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestBaseManifestContainsRunnableControlPlaneResources(t *testing.T) {
	objects := readObjects(t, "neocloud-node.yaml")

	requireKindName(t, objects, "Namespace", "aibrix-system")
	requireKindName(t, objects, "CustomResourceDefinition", "cloudpodbindings.cloud.aibrix.ai")
	serviceAccount := requireKindName(t, objects, "ServiceAccount", "aibrix-neocloud-node")
	requireKindName(t, objects, "ClusterRole", "aibrix-neocloud-node")
	clusterRoleBinding := requireKindName(t, objects, "ClusterRoleBinding", "aibrix-neocloud-node")
	deployment := requireKindName(t, objects, "Deployment", "aibrix-neocloud-node")

	if serviceAccount["metadata"].(map[string]interface{})["namespace"] != "aibrix-system" {
		t.Fatalf("ServiceAccount namespace = %v, want aibrix-system", serviceAccount["metadata"].(map[string]interface{})["namespace"])
	}
	subjects := clusterRoleBinding["subjects"].([]interface{})
	subject := subjects[0].(map[string]interface{})
	if subject["name"] != "aibrix-neocloud-node" || subject["namespace"] != "aibrix-system" {
		t.Fatalf("ClusterRoleBinding subject = %#v, want aibrix-neocloud-node in aibrix-system", subject)
	}
	if deployment["metadata"].(map[string]interface{})["namespace"] != "aibrix-system" {
		t.Fatalf("Deployment namespace = %v, want aibrix-system", deployment["metadata"].(map[string]interface{})["namespace"])
	}

	podSpec := deployment["spec"].(map[string]interface{})["template"].(map[string]interface{})["spec"].(map[string]interface{})
	if podSpec["serviceAccountName"] != "aibrix-neocloud-node" {
		t.Fatalf("serviceAccountName = %v, want aibrix-neocloud-node", podSpec["serviceAccountName"])
	}
	containers := podSpec["containers"].([]interface{})
	container := containers[0].(map[string]interface{})
	if container["name"] != "aibrix-neocloud-node" {
		t.Fatalf("container name = %v, want aibrix-neocloud-node", container["name"])
	}
	args := container["args"].([]interface{})
	requireContains(t, args, "--provider=$(GPU_BURST_PROVIDER)")
	requireContains(t, args, "--binding-store=kubernetes")
	requireContains(t, args, "--node-name=$(GPU_BURST_NODE_NAME)")
	requireContains(t, args, "--orphan-sweeper-interval=$(GPU_BURST_ORPHAN_SWEEPER_INTERVAL)")
	requireContains(t, args, "--orphan-sweeper-dry-run=$(GPU_BURST_ORPHAN_SWEEPER_DRY_RUN)")
	requireContains(t, args, "--placement-offers=$(GPU_BURST_PLACEMENT_OFFERS)")
	requireContains(t, args, "--placement-offer-ttl=$(GPU_BURST_PLACEMENT_OFFER_TTL)")
	requireContains(t, args, "--placement-refresh-interval=$(GPU_BURST_PLACEMENT_REFRESH_INTERVAL)")
	requireContains(t, args, "--placement-signal-cooldown=$(GPU_BURST_PLACEMENT_SIGNAL_COOLDOWN)")

	env := container["env"].([]interface{})
	requireSecretRef(t, env, "RUNPOD_API_KEY", "aibrix-neocloud-node-runpod")
	requireSecretRef(t, env, "LAMBDA_API_KEY", "aibrix-neocloud-node-lambda")
}

func TestBaseManifestUsesSingletonAndRestrictedSecurityContext(t *testing.T) {
	objects := readObjects(t, "neocloud-node.yaml")
	deployment := requireKindName(t, objects, "Deployment", "aibrix-neocloud-node")
	spec := deployment["spec"].(map[string]interface{})

	strategy := spec["strategy"].(map[string]interface{})
	if strategy["type"] != "Recreate" {
		t.Fatalf("deployment strategy = %#v, want Recreate without leader election", strategy)
	}

	podSpec := spec["template"].(map[string]interface{})["spec"].(map[string]interface{})
	securityContext := podSpec["securityContext"].(map[string]interface{})
	if securityContext["runAsNonRoot"] != true {
		t.Fatalf("pod securityContext = %#v, want runAsNonRoot", securityContext)
	}
	seccomp := securityContext["seccompProfile"].(map[string]interface{})
	if seccomp["type"] != "RuntimeDefault" {
		t.Fatalf("seccompProfile = %#v, want RuntimeDefault", seccomp)
	}

	container := podSpec["containers"].([]interface{})[0].(map[string]interface{})
	containerSecurity := container["securityContext"].(map[string]interface{})
	if containerSecurity["readOnlyRootFilesystem"] != true || containerSecurity["allowPrivilegeEscalation"] != false {
		t.Fatalf("container securityContext = %#v, want read-only root and no privilege escalation", containerSecurity)
	}
}

func TestBaseManifestRBACMatchesControllerOperations(t *testing.T) {
	objects := readObjects(t, "neocloud-node.yaml")
	role := requireKindName(t, objects, "ClusterRole", "aibrix-neocloud-node")
	rules := role["rules"].([]interface{})

	podRule := requireRule(t, rules, "", "pods")
	requireContains(t, podRule["verbs"].([]interface{}), "delete")

	nodeRule := requireRule(t, rules, "", "nodes")
	requireSet(t, nodeRule["verbs"].([]interface{}), "create", "get")

	leaseRule := requireRule(t, rules, "coordination.k8s.io", "leases")
	requireSet(t, leaseRule["verbs"].([]interface{}), "create", "get", "update")

	for _, item := range rules {
		rule := item.(map[string]interface{})
		requireNotContains(t, rule["verbs"].([]interface{}), "*")
		requireNotContains(t, rule["resources"].([]interface{}), "*")
	}
}

func TestCRDHasStatusSubresourceAndPrinterColumns(t *testing.T) {
	objects := readObjects(t, "neocloud-node.yaml")
	crd := requireKindName(t, objects, "CustomResourceDefinition", "cloudpodbindings.cloud.aibrix.ai")
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
	objects := readObjects(t, "../config/samples/fake-pod.yaml")
	pod := requireKindName(t, objects, "Pod", "neocloud-node-fake-smoke")
	spec := pod["spec"].(map[string]interface{})

	nodeSelector := spec["nodeSelector"].(map[string]interface{})
	if nodeSelector["cloud.aibrix.ai/provider"] != "fake" {
		t.Fatalf("nodeSelector = %#v, want fake provider selector", nodeSelector)
	}
	tolerations := spec["tolerations"].([]interface{})
	if len(tolerations) != 1 {
		t.Fatalf("tolerations = %#v, want one provider toleration", tolerations)
	}
	toleration := tolerations[0].(map[string]interface{})
	if toleration["key"] != "cloud.aibrix.ai/provider" || toleration["value"] != "fake" {
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

func requireNotContains(t *testing.T, values []interface{}, forbidden string) {
	t.Helper()

	for _, value := range values {
		if value == forbidden {
			t.Fatalf("values = %#v, must not contain %q", values, forbidden)
		}
	}
}

func requireSet(t *testing.T, values []interface{}, want ...string) {
	t.Helper()

	if len(values) != len(want) {
		t.Fatalf("values = %#v, want exactly %#v", values, want)
	}
	for _, value := range want {
		requireContains(t, values, value)
	}
}

func requireRule(t *testing.T, rules []interface{}, apiGroup string, resource string) map[string]interface{} {
	t.Helper()

	for _, item := range rules {
		rule := item.(map[string]interface{})
		apiGroups := rule["apiGroups"].([]interface{})
		resources := rule["resources"].([]interface{})
		if contains(apiGroups, apiGroup) && contains(resources, resource) {
			return rule
		}
	}
	t.Fatalf("RBAC rule for %q/%q not found in %#v", apiGroup, resource, rules)
	return nil
}

func contains(values []interface{}, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func requireSecretRef(t *testing.T, env []interface{}, name string, secretName string) {
	t.Helper()

	for _, item := range env {
		entry := item.(map[string]interface{})
		if entry["name"] != name {
			continue
		}
		valueFrom := entry["valueFrom"].(map[string]interface{})
		secretRef := valueFrom["secretKeyRef"].(map[string]interface{})
		if secretRef["name"] != secretName {
			t.Fatalf("%s secret ref = %#v, want %s", name, secretRef, secretName)
		}
		return
	}
	t.Fatalf("env = %#v, want %s secret ref", env, name)
}
