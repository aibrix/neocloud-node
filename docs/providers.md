# Provider Configuration

| Provider | `provider` | Secret reference | Also required |
| --- | --- | --- | --- |
| RunPod | `runpod` | `credentials.runpod.secretName` | Nothing |
| Vast.ai | `vast` | `credentials.vast.secretName` | Nothing |
| Lambda Cloud | `lambda` | `credentials.lambda.secretName` | A runner callback URL reachable from the VM, and a region, instance type, and SSH key names. See the [Lambda adapter guide](lambda-adapter.md). |

Use the matching provider toleration and workload annotations. For Vast.ai,
see the [sample Pod](../config/samples/vast-pod.yaml). Provider capabilities differ;
workload examples are not interchangeable across providers.

## Credentials and installation

Create the provider credential Secret in the controller's namespace
(`aibrix-system`). Reference its name through the chart values above; do not
put API keys in Helm values. See the
[chart guide](../charts/neocloud-node/README.md#user-managed-provider-secrets)
for Secret creation and credential updates.

The [Getting Started guide](../README.md#getting-started) installs RunPod.
To install another provider, use its `provider` value and corresponding
`credentials.<provider>.secretName`. Lambda Cloud also needs the callback,
region, instance type, and SSH key settings described in its adapter guide.

Before changing providers on an existing release, delete its workloads and
verify that the remote instances and bindings have been removed. For separate
releases, give each virtual node a distinct `nodeName`.

## Workloads

Run example workloads in the `default` namespace; provider credentials remain
in the controller namespace. For Vast.ai:

```bash
kubectl -n default apply -f config/samples/vast-pod.yaml
kubectl -n default get pod neocloud-node-vast-smoke -w
```

After inspecting the workload, delete it while the controller is running:

```bash
kubectl -n default delete -f config/samples/vast-pod.yaml --wait=true
kubectl -n default get cloudpodbindings \
  -l cloud.aibrix.ai/pod-name=neocloud-node-vast-smoke
```

Confirm the instance is deleted in the provider console. See
[Provider Compatibility](../README.md#provider-compatibility) for supported
features and limitations, and the [RunPod adapter guide](runpod-adapter.md)
for RunPod-specific settings.
