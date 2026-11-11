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

All provider Secret references are optional. With the updated controller image,
`GPU_BURST_ALLOW_MISSING_CREDENTIALS=true` (the chart default) keeps the controller
running when a Secret, its key, or its non-empty value is missing. The unconfigured
primary provider advertises zero GPUs and logs an actionable message. It does not
send unauthenticated requests or silently switch to the fake provider. A workload
explicitly bound to the node receives a `MissingCredentials` authentication error.
Missing credentials never count as proof that a remote allocation was deleted.

Create/update the Secret independently, then restart the controller to reload its
environment variables:

```bash
kubectl -n aibrix-system rollout restart deployment/neocloud-neocloud-node
kubectl -n aibrix-system rollout status deployment/neocloud-neocloud-node
```

Secret contents are not watched or hot-reloaded. An optional missing Secret does
not populate an already-running container when it is later created. Set
`env.GPU_BURST_ALLOW_MISSING_CREDENTIALS=false` for strict startup validation; the
standalone CLI remains strict by default. Images from release 0.1.0 do not support
the tolerant-startup option; use the updated image with this chart.

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
