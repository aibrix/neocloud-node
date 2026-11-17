# NeoCloud Node Helm chart

Requires Helm 3+ and permissions to create cluster-scoped RBAC and CRDs.
Install from a checkout of this repository (the chart is not yet published to a Helm repository):

```bash
export KUBECONFIG=/tmp/k8s-aibrix-neocloud-kubeconfig.yaml
helm upgrade --install neocloud ./charts/neocloud-node \
  --namespace aibrix-system --create-namespace --wait --timeout 3m
kubectl get node neocloud-node
```

The default provider is `fake`: it simulates lifecycle state without running a
remote container. The controller always has one replica and uses `Recreate`;
multiple controllers must not reconcile the same virtual node. For another
release, choose a distinct `nodeName`.

## Configuration

| Value | Default | Purpose |
| --- | --- | --- |
| `image.tag` | `0.1.0` | Release version or full commit SHA published by post-submit CI |
| `provider` | `fake` | `fake`, `runpod`, `vast`, or `lambda` |
| `nodeName` | `neocloud-node` | Cluster-wide unique virtual node name |
| `credentials.<provider>.secretName` | empty | Existing Secret in the release namespace |
| `credentials.<provider>.key` | `apiKey` | API key entry in that Secret |
| `env` | See values.yaml | Controller settings, including placement and Lambda callback settings |
| `resources` | 100m CPU / 128Mi request | Controller resources |
| `nodeSelector`, `tolerations` | empty | Placement of the controller Pod on physical nodes |
| `imagePullSecrets` | empty | Registry credential references for the controller image |

Do not put API keys in Helm values: Helm stores values in its release metadata.
For example, with an existing Vast Secret named `neocloud-vast`:

```bash
helm upgrade --install neocloud ./charts/neocloud-node \
  -n aibrix-system --create-namespace --wait \
  --set provider=vast --set credentials.vast.secretName=neocloud-vast
```

Drain and delete existing remote workloads before switching providers or node
names. Real provider workloads create paid cloud resources.

## User-managed provider Secrets

The chart does not create provider Secrets. Users create them in the same namespace
as the release and configure only the Secret name and key in values:

```yaml
provider: runpod
credentials:
  runpod:
    secretName: neocloud-runpod
    key: apiKey
  lambda:
    secretName: neocloud-lambda
    key: apiKey
```

Provider Secret references are optional. Configured references are read directly
from the release namespace using Kubernetes GET requests; credentials no longer
enter the controller through environment variables. Missing Secrets, missing keys
and empty values keep the controller running with new allocations disabled.
Creating or updating the Secret activates/reloads the provider without a rollout.
Empty `secretName` entries are not polled. Shared names are read once per cycle.

```yaml
credentialPolling:
  interval: 15s
  requestTimeout: 5s
```

Use a controller image containing Secret polling when upgrading this chart;
release 0.1.0 and older commit images do not understand these references. Follow
the existing merge → main CI → commit image → Helm upgrade workflow.
The chart's appVersion/default image tag is not proof that a newer feature is
included; explicitly select the published commit image for this feature.
See [credential rotation](../../docs/credentials.md) for refresh timing, failure
handling, CLI configuration and the same-account rotation boundary.

Maintain one release (`neocloud`) in `aibrix-system` and upgrade it in place.
Delete test workloads and verify remote GPU cleanup after each run; keep this
shared release and user-managed Secrets unless explicitly uninstalling them.

## Smoke validation and DOKS

```bash
kubectl apply -f config/samples/fake-pod.yaml
kubectl get pod neocloud-node-fake-smoke
kubectl get cloudpodbindings
```

DOKS may add `readiness.k8s.io/DOKSCriticalComponentsReady=pending:NoSchedule`
to the virtual node. The sample workload then needs this additional toleration:

```yaml
tolerations:
  - key: cloud.aibrix.ai/provider
    operator: Equal
    value: fake
    effect: NoSchedule
  - key: readiness.k8s.io/DOKSCriticalComponentsReady
    operator: Equal
    value: pending
    effect: NoSchedule
```

This belongs on remote workload Pods, not the chart's controller `tolerations`.
Inspect the virtual node and platform requirements before tolerating platform
taints. Helm `--wait` verifies the controller Deployment, not provider readiness.

## Upgrades and removal

Helm installs missing CRDs from `crds/`, but does not upgrade or remove them.
Before upgrading a chart with CRD changes, review and apply the CRD explicitly:

```bash
kubectl apply -f charts/neocloud-node/crds/
helm upgrade neocloud ./charts/neocloud-node -n aibrix-system -f my-values.yaml
```

Before uninstalling, delete remote workload Pods while the controller is still
running and verify that their provider resources and bindings are cleaned up.
Then uninstall the release and remove its virtual Node:

```bash
helm uninstall neocloud -n aibrix-system
kubectl delete node neocloud-node
```

Never delete the CRD while another release or remote workload still uses it.

## Migrating the legacy YAML installation

Do not run two controllers with `nodeName=neocloud-node`. For the existing
`aibrix-neocloud-node` YAML installation, first clean up remote workloads, then
scale the old Deployment to zero. Install this chart as shown above. It uses
new release-scoped resource names and reuses the existing CRD and virtual node.
Verify the new controller and a smoke workload before removing only these old
resources:

```bash
kubectl -n aibrix-system scale deployment aibrix-neocloud-node --replicas=0
helm upgrade --install neocloud ./charts/neocloud-node \
  -n aibrix-system --create-namespace --wait --timeout 3m
# Verify the new controller and smoke workload before cleanup:
kubectl -n aibrix-system delete deployment,serviceaccount aibrix-neocloud-node
kubectl delete clusterrolebinding,clusterrole aibrix-neocloud-node
```

Do not run `kubectl delete -f deploy/neocloud-node.yaml`: it would also delete
the shared namespace and CRD. If the new controller fails, scale it to zero
before restoring the old controller to one replica.

## Container logs

Set `kubeletAPI.enabled=true` and explicitly set
`kubeletAPI.authenticationMode=none` for an isolated development deployment.
The controller advertises its Pod IP; verify API-server reachability and restrict
access to port 10250. An optional `kubeletAPI.tlsSecretName` mounts an existing
serving certificate; otherwise the controller generates a self-signed certificate.

Lambda reads logs over SSH. Set `lambdaSSH.secretName` to an existing Secret
containing `ssh-privatekey` and verified `known_hosts` entries. The default SSH user
is `ubuntu`; it needs noninteractive `sudo docker` access, and the controller
must reach VM TCP port 22. No log-upload endpoint or log signing Secret is needed.

The existing Lambda lifecycle callback can be configured independently with
`runnerCallback.enabled`, `runnerCallback.secretName`, and `runnerCallback.url`.
The chart creates an internal status Service on port 9091; supply a VM-reachable
TLS proxy for that route. See the [container log guide](../../docs/container-logs.md)
for credentials, host verification and Docker retention details.
