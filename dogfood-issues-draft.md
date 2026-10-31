# Draft issues: dogfooding NeoCloud Node with AIBrix workloads

Target repo: `aibrix/neocloud-node`. Create #0 first, then #1–#9 as its sub-issues.
Code links are pinned to neocloud-node `772c97f` and vllm-project/aibrix `68a2d49`.
Findings come from reading the code. None has been reproduced on a live provider yet.

## Coordination with the "New Cloud provider gap analysis" session

That session files its own engineering backlog: I-1 to I-9 for the OSPP engineering intern, plus U-1 and U-2 for the RunPod v1 to v2 migration. Three of my drafts duplicate its issues, so I don't file them separately:

| Mine | Theirs | Resolution |
|---|---|---|
| #1 Expose remote endpoints | I-5 Surface remote endpoints (public IP / port mappings) | Don't file. After I-5 exists, post the #1 text below as a comment on it, adding the dogfood requirements: set `status.podIP`, handle remapped ports, and the ModelClaim consumer |
| #3 Exit reported without exit code | I-8 RunPod `EXITED` mapped to `Succeeded` hides spot reclaim | Don't file. Post a comment on I-8: the same mapping breaks batch exit codes, and Vast has the opposite bug (`exited` → `Failed`). This angle can be verified without spot credits |
| #4 `valueFrom` / `envFrom` dropped | I-1, same bug | Drop. I-1 covers it. Secret projection is on their core-team list |

Related, but not duplicates:
- #7 multi-cluster orphan sweeper and I-2 "orphan sweeper only covers the primary provider": both change `pkg/core/orphan_sweeper.go`. File both, link them to each other, and do them together or one after the other.
- #2 `restartPolicy: Always` and their gap "`OnFailure` is accepted but nothing recreates the workload": both are about restart semantics. Link them.
- #5 readiness, #8 budget and cost, plus Secret projection: these are on their core-team list but not among the I-issues. My drafts can serve as those issues.

Ordering: U-1 (RunPod v2, v1 retires around 2026-11-15) changes `pkg/providers/runpod/client.go`. Do the RunPod parts of I-5, I-8, and #9 against v2, not v1. According to the other session's reading of the v2 OpenAPI spec, v2 includes port info with the public IP and a `/v2/pods/{id}/logs` endpoint. I haven't verified either myself.

## What to file from here

| # | Title | Type / label |
|---|---|---|
| 0 | Dogfood NeoCloud Node with AIBrix workloads on a shared control plane | Task / enhancement |
| 2 | [Feature]: Support long-running serving Pods (restartPolicy: Always) | Feature / enhancement |
| 5 | [Feature]: Derive Pod readiness from readinessProbe instead of remote running state | Feature / enhancement |
| 6 | [Feature]: Map host memory, disk, and shared memory requirements to provider placement | Feature / enhancement |
| 7 | [Bug]: Orphan sweeper treats other clusters' resources in the same provider account as orphans | Bug / bug |
| 8 | [Feature]: Per-namespace cost accounting, budgets, and runtime limits for shared control planes | Feature / enhancement |
| 9 | [Feature]: Container logs and debug access for remote workloads | Feature / enhancement |
| — | Comment on I-5 (text from #1 below) | comment |
| — | Comment on I-8 (text from #3 below) | comment |

Also attach I-1, I-5, and I-8 as sub-issues of #0, and replace the `#1` / `#3` / `#4` references below with the real numbers.

Not filed here: the AIBrix ModelClaim runtime client needs to read the endpoint annotation from I-5
when a provider remaps ports (RunPod, Vast). That change belongs in `vllm-project/aibrix`.

---

## #0 Dogfood NeoCloud Node with AIBrix workloads on a shared control plane

Type: Task · Labels: `enhancement`

### Background

NeoCloud Node has unit and fake-provider coverage, and gated paid smoke tests, but no real workload runs through it on a regular basis. The smoke tests check the resource lifecycle (create, running, delete, no leaks). They do not check that a real workload does useful work and returns a result we can trust. Without users, we are not finding the problems users would hit.

At the same time, community members who need GPUs for AIBrix development are given their own Lambda or RunPod credentials. Budgets are hard to track, and credentials are spread across many people. A common workflow is to rent a GPU VM, install kind on it, and deploy AIBrix there. Every user rebuilds a control plane. The upside is that SSH access to the machine makes debugging easy.

### Proposal

Use NeoCloud Node ourselves as the way community members get GPUs:

- One shared Kubernetes control plane (for example, EKS with a single always-on CPU node) runs AIBrix components and NeoCloud Node.
- Provider credentials live only in the NeoCloud Node Secret. Members get access to a namespace, never to provider credentials.
- GPU workloads are scheduled onto the NeoCloud virtual node and created on demand on Lambda, RunPod, or Vast.
- Traffic to a remote workload uses the endpoint the provider exposes. Pod-to-pod networking across nodes (CNI, VPN) is out of scope.

### First target workload

AIBrix kvcached runtime with ModelClaim ([`samples/modelclaim`](https://github.com/vllm-project/aibrix/tree/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/samples/modelclaim)): one replica, one GPU, no east-west traffic. The only callers are the ModelClaim controller (it calls the runtime agent at `podIP:8080`), metrics scraping, and users.

AIBrix manifests can have NeoCloud-specific variants. The goal is to hide provider details where reasonable, not to guarantee full Pod compatibility.

**Done when:** from the shared control plane, applying a NeoCloud variant of the ModelClaim samples brings both claims to `Active`, and a request through the runtime returns a completion. Deleting the manifests leaves no remote resources, and the cost is attributed to the namespace that submitted the workload.

### Gaps

The gaps below were found by reading the code at 772c97f against this workload. They have not yet been reproduced on a live provider. Each one is tracked as a sub-issue.

- [ ] I-5: Expose remote workload endpoints (filed separately; must also set `status.podIP`)
- [ ] Support long-running serving Pods (`restartPolicy: Always`)
- [ ] I-8: Container exit mapping (filed separately; covers both spot reclaim and batch exit codes)
- [ ] I-1: `valueFrom` and `envFrom` environment variables are silently dropped (filed separately)
- [ ] Derive Pod readiness from `readinessProbe`
- [ ] Map host memory, disk, and shared memory requirements to provider placement
- [ ] Orphan sweeper treats other clusters' resources in the same provider account as orphans
- [ ] Per-namespace cost accounting, budgets, and runtime limits
- [ ] Container logs and debug access

Related existing issues: #2 (orphan cleanup), #4 (VM runner callbacks), #6 (cost and burn-rate metrics), #7 (events and conditions), #21 (workload grouping). The RunPod parts depend on U-1, the RunPod REST v1 to v2 migration.

### Later dogfood scenarios

- Run the existing gated smoke tests (`hack/e2e/*-smoke.sh`) on a nightly schedule with a budget cap, so provider regressions show up without someone running them by hand.
- Use the shared control plane for GPU CI. For example, run ephemeral GitHub Actions runners as Pods on the virtual node. Runners only make outbound connections and use a single container, so they fit the current Pod contract. This would add real-GPU coverage to AIBrix CI, which uses `vllm-mock` today.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #1 → Comment on I-5 (do not file as a separate issue)

Post the text below as a comment on I-5, "Surface remote endpoints (public IP / port mappings)". It adds the requirements from the dogfood workload. For RunPod, build on the v2 API after U-1 instead of the v1 fields cited here.

### Problem

NeoCloud Node never sets `status.podIP`, and nothing in the lifecycle carries a remote workload's address:

- `core.RemoteResource` has no address or port fields ([fake_provider.go#L22-L31](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/fake_provider.go#L22-L31)).
- RunPod: the adapter decodes `publicIp` and `portMappings` from the API response but never uses them ([runpod/client.go#L273-L274](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/providers/runpod/client.go#L273-L274)).
- Vast: the adapter asks Vast to publish each port with `-p <port>:<port>` ([vast/client.go#L828-L846](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/providers/vast/client.go#L828-L846)), but the `instance` struct does not decode the public IP or the port mapping ([vast/client.go#L606](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/providers/vast/client.go#L606)).
- Lambda: the runner starts the container with `docker run` and neither publishes ports nor uses host networking ([runner/cloudinit.go#L96](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/runner/cloudinit.go#L96)), so container ports are unreachable even on the VM's public IP.

Every provider exposes the workload somewhere, but nothing in the cluster can find that address.

### Workload and use case

The AIBrix ModelClaim controller calls the runtime agent at `pod.Status.PodIP:8080` ([wake.go#L228](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/pkg/controller/modelclaim/wake.go#L228), [snapshot.go#L70](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/pkg/controller/modelclaim/snapshot.go#L70); the port is fixed in [runtime_client.go#L45](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/pkg/controller/modelclaim/runtime_client.go#L45)). It skips Pods whose `PodIP` is empty ([wake.go#L171](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/pkg/controller/modelclaim/wake.go#L171)). Selector-based Services, such as the metrics Service in the sample, also resolve to `podIP:targetPort`. With an empty `PodIP`, ModelClaim can never place a claim on a NeoCloud Pod.

### Proposed behavior

- Pass the provider's public address and port mappings from each adapter through `RemoteResource` into `CloudPodBinding.status`.
- Set `status.podIP` and `status.podIPs` to the public address once it is known.
- Record the per-port mapping in a well-known annotation (for example, `cloud.aibrix.ai/endpoints`), because providers may remap ports. RunPod and Vast publish a container TCP port on a different public port; Lambda can keep the same port.
- When the public port equals the container port, Services and direct callers can use `podIP:containerPort` unchanged. When it does not, callers need the annotation. For AIBrix this means a small change in the ModelClaim runtime client, to be tracked in vllm-project/aibrix.
- Document that the exposed port is reachable from the internet. Use provider source-IP firewalls where they exist (Lambda firewall rulesets), and document the risk where they don't.

### Provider portability

The contract is provider-neutral: an address and a port map on `RemoteResource`. Each adapter extracts them from its provider's API.

### Alternatives considered

- Joining remote machines to the cluster network (CNI or VPN): out of scope, and not needed for single-replica workloads without east-west traffic.
- A selector-less Service with an EndpointSlice that NeoCloud manages using the remapped ports: works for Service consumers, but not for callers that dial `PodIP` directly.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #2 [Feature]: Support long-running serving Pods (restartPolicy: Always)

Type: Feature · Labels: `enhancement` · Parent: #0

### Problem

`restartPolicy: Always` is rejected with `UnsupportedRestartPolicy` ([transformer.go#L109](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/transformer.go#L109)). Every Pod created by a Deployment, ReplicaSet, or StatefulSet uses `Always`, so serving workloads can only run as bare Pods.

### Workload and use case

Model serving and runtime pools are Deployments. One example is the AIBrix kvcached runtime used by ModelClaim ([warm-runtime-pool.yaml](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/samples/modelclaim/warm-runtime-pool.yaml)). ModelClaim reads the pool's lifecycle policy from the owning Deployment through owner references ([pool_policy_controller.go#L241-L262](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/pkg/controller/modelclaim/pool_policy_controller.go#L241-L262)). A bare Pod still runs, but it loses that policy.

### Proposed behavior

- Accept `restartPolicy: Always` for single-container GPU Pods.
- Define what happens when the remote container or instance exits. Either report the Pod as `Failed` (never `Succeeded`) so the owning ReplicaSet replaces it, or restart the container in place where the provider supports that. Today RunPod maps `EXITED` to `Succeeded` ([runpod/client.go#L390](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/providers/runpod/client.go#L390)), so a Deployment would be left with a finished Pod instead of a replacement.
- Document rollout behavior: each new Pod creates a new remote resource, and the old resource is deleted along with its Pod.

### Provider portability

The transformer and lifecycle change is provider-neutral. Each adapter maps its own exit states.

### Alternatives considered

Running a bare Pod with `restartPolicy: Never` works today as a manual workaround, but without owner-based behavior.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #3 → Comment on I-8 (do not file as a separate issue)

Post the text below as a comment on I-8, "RunPod `EXITED` mapped to `Succeeded` hides spot reclaim". It adds the batch exit-code angle and the opposite Vast bug. Neither needs spot credits to verify.

### Version

772c97f

### Provider

RunPod, Vast.ai

### What happened?

For container-based providers, NeoCloud Node decides success or failure from the provider's instance status, not from the container's exit code:

- RunPod: `EXITED` becomes `RemoteSucceeded` whatever the exit code ([runpod/client.go#L386-L397](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/providers/runpod/client.go#L386-L397)).
- Vast: `exited` becomes `RemoteFailed` whatever the exit code ([vast/client.go#L757-L768](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/providers/vast/client.go#L757-L768)).

Impact: a batch job such as a CI test, eval, or benchmark that fails on RunPod is reported as `Succeeded`, and one that succeeds on Vast is reported as `Failed`. Automation can't trust either result.

Found by reading the code. Not yet reproduced against a live provider.

### What did you expect?

The Pod phase and the container's `exitCode` should reflect how the container actually exited. If the provider can't report that, the binding should say the outcome is unknown instead of guessing. `docs/vm-runner.md` already proposes using the runner callback for container providers too, either through a wrapper command or a runner binary. #4 covers that protocol for VM providers.

### Reproduction

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: exit-code-check
  annotations:
    cloud.aibrix.ai/provider: runpod
spec:
  restartPolicy: Never
  nodeSelector:
    cloud.aibrix.ai/provider: runpod
  tolerations:
    - key: cloud.aibrix.ai/provider
      operator: Equal
      value: runpod
      effect: NoSchedule
  containers:
    - name: main
      image: busybox:1.36
      command: ["sh", "-c", "exit 1"]
      resources:
        limits:
          nvidia.com/gpu: "1"
```

Expected `Failed` with exit code 1. Based on the code path, RunPod reports `Succeeded`. With `exit 0` on Vast, it reports `Failed`.

### Environment

Found by code inspection at 772c97f.

### Safety check

- [x] I removed credentials and private or billable resource identifiers.
- [x] No remote resources were created while investigating.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #4 → Dropped: duplicate of I-1

I-1 in the provider gap analysis session covers the same bug and the same code location.

---

## #5 [Feature]: Derive Pod readiness from readinessProbe instead of remote running state

Type: Feature · Labels: `enhancement` · Parent: #0

### Problem

A Pod is marked `Ready=True` as soon as the remote resource reports running ([vk/provider.go#L544-L555](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/vk/provider.go#L544-L555)), and container readiness probes are ignored. For serving workloads, the remote instance reports running while the image is still being pulled and the model is still loading, which can take minutes.

### Workload and use case

ModelClaim uses `IsPodReady` to decide which Pods can take claims ([backoff.go#L436](https://github.com/vllm-project/aibrix/blob/68a2d4922316e56ea86080b8ee96c2e3d6e45cb6/pkg/controller/modelclaim/backoff.go#L436)). When the runtime is marked ready too early, calls to it fail and back off. Deployment rollouts and Service endpoints have the same problem.

### Proposed behavior

- If the container has an `httpGet` or `tcpSocket` readiness probe, NeoCloud Node runs it from the controller against the exposed endpoint (depends on I-5). It sets `Ready` from the result and honors the probe's period and thresholds.
- Without a probe, keep the current behavior.
- `exec` probes stay unsupported. Either reject them or document that they are ignored.

### Provider portability

Provider-neutral. Depends on I-5 for the endpoint address.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #6 [Feature]: Map host memory, disk, and shared memory requirements to provider placement

Type: Feature · Labels: `enhancement` · Parent: #0

### Problem

`WorkloadRequest` carries GPU type, count, and memory, but no host memory, CPU, or disk ([core/types.go#L21-L37](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/types.go#L21-L37)). The Pod's `resources.requests.memory` and `ephemeral-storage` are ignored, so placement can pick a machine that is too small:

- Vast defaults to a 20 GiB disk ([app/app.go#L141](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/app/app.go#L141)). A Pod can override it only with the provider-specific `cloud.aibrix.ai/vast-disk-gb` annotation.
- RunPod create requests don't set a container disk size, so the provider default applies.
- The shared memory size (`/dev/shm`) can't be set, and Pod volumes are rejected entirely ([transformer.go#L123-L129](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/transformer.go#L123-L129)).

### Workload and use case

The kvcached runtime uses a large vLLM-based image (building it in AIBrix CI needs about 31 GB of disk), plus model weights. It also needs about 16 GiB of host memory, because sleeping engines keep their weights in RAM. The sample mounts `emptyDir` volumes for `/dev/shm` and for scratch state.

### Proposed behavior

- Map `requests.memory` and `requests.ephemeral-storage` (or a provider-neutral annotation) to provider search filters or create fields, where the provider API supports them. Reject the Pod with a clear condition when no provider can satisfy them.
- Optionally accept `emptyDir` volumes as container-local directories, and an `emptyDir` with `medium: Memory` mounted at `/dev/shm` as the shared memory size. Other volume types stay rejected.

### Alternatives considered

Workarounds in the manifest work for now: drop the volumes and set provider-specific disk annotations. This issue is about not making users learn each provider's defaults.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #7 [Bug]: Orphan sweeper treats other clusters' resources in the same provider account as orphans

Type: Bug · Labels: `bug` · Parent: #0

### Version

772c97f

### Provider

Provider-independent

### What happened?

The orphan sweeper lists every remote resource in the provider account. It treats any resource that has an idempotency key, or whose name starts with `neocloud-node-`, as managed ([orphan_sweeper.go#L102-L104](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/orphan_sweeper.go#L102-L104)). It then compares those resources only against bindings in the local cluster ([orphan_sweeper.go#L44-L100](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/orphan_sweeper.go#L44-L100)). External names are derived from the namespace and Pod UID, with no cluster identity ([external_name.go](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/core/external_name.go)).

When two control planes share one provider account, each one sees the other's live workloads as orphans. Examples: a shared dev cluster and a CI cluster, two concurrent CI runs, or a developer's local cluster. With `--orphan-sweeper-dry-run=false`, the sweeper deletes them. The sweeper is disabled and in dry-run mode by default ([app/app.go#L153-L154](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/app/app.go#L153-L154)), so today this is latent. It blocks turning cleanup on in the shared control plane, though.

Related: #2 asks that the sweeper delete only resources managed by NeoCloud Node. The missing part is "managed by this NeoCloud Node instance". I-2 ("orphan sweeper only covers the primary provider") changes the same code, so the two should be done together or one after the other.

### What did you expect?

- Include a cluster or controller-instance identity in the external name, provider labels, or tags, and sweep only resources whose identity matches.
- Report resources with a missing or foreign identity, but never delete them.
- Consider an expiry tag as well. An ephemeral control plane (for example, a cancelled CI run, where the cluster and its bindings disappear together) can leave resources behind, and an out-of-band janitor could then clean them up.

### Reproduction

1. Run two NeoCloud Node controllers in separate clusters with the same provider API key.
2. Create a GPU Pod in cluster A.
3. In cluster B, enable the sweeper with dry-run on.
4. Cluster B reports cluster A's resource as an orphan. With dry-run off, it would delete it.

### Environment

Found by code inspection at 772c97f.

### Safety check

- [x] I removed credentials and private or billable resource identifiers.
- [x] No remote resources were created while investigating.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #8 [Feature]: Per-namespace cost accounting, budgets, and runtime limits for shared control planes

Type: Feature · Labels: `enhancement` · Parent: #0

### Problem

On a shared control plane, the operator needs to know and limit what each team or member spends. Today NeoCloud Node exports an instantaneous burn-rate gauge per Pod, `gpu_burst_node_current_burn_rate_usd_per_hour` ([vk/provider.go#L463](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/vk/provider.go#L463)). It does not provide:

- Cumulative spend per namespace.
- A budget limit. Nothing stops new GPU Pods after a namespace has spent its allowance.
- A maximum lifetime for GPU Pods, so forgotten Pods keep billing.
- Reconciliation with provider invoices.

### Workload and use case

This is needed to replace per-member provider credentials with a shared control plane (see #0). Per-member budgets can't be tracked today.

### Proposed behavior

- Accumulate spend per binding (hourly price × runtime) and roll it up per namespace. Expose the totals as metrics and in status.
- Optional per-namespace budget: once it is exceeded, reject new GPU Pods with a clear condition and event. GPU concurrency can use a standard `ResourceQuota` on `nvidia.com/gpu`.
- Optional maximum runtime, with a namespace default and a per-Pod annotation override, and possibly honoring `activeDeadlineSeconds`. When it is exceeded, delete the remote resource and fail the Pod.
- Reconcile against provider billing. neocloud-trace already has RunPod and Vast billing sync and a Lambda CSV import that can be reused.

### Provider portability

The ledger, budget, and runtime limit are provider-neutral. Billing reconciliation is per provider.

Related: #6.

---
_Generated by [Claude Code](https://claude.ai/code)_

---

## #9 [Feature]: Container logs and debug access for remote workloads

Type: Feature · Labels: `enhancement` · Parent: #0

### Problem

`kubectl logs`, `exec`, `attach`, and `port-forward` all return "unsupported" ([vk/provider.go#L218-L244](https://github.com/aibrix/neocloud-node/blob/772c97fd93936c423718879f1e87265b2fdb97f7/pkg/vk/provider.go#L218-L244)). When a remote workload fails, users can't see why from Kubernetes. Debugging is the main reason community members prefer renting a raw GPU VM: they can SSH into it.

### Workload and use case

Developing and debugging the AIBrix runtime and ModelClaim on remote GPUs.

### Proposed behavior

Each of these needs investigating per provider:

- Logs: use provider log APIs where they exist. The RunPod v2 API reportedly has `/v2/pods/{id}/logs`, so the RunPod part should follow U-1. Otherwise, have the runner or wrapper ship container output somewhere the controller can serve through `GetContainerLogs`.
- Debug access: where the provider supports SSH, surface the SSH endpoint on the Pod (as an annotation) and inject the requesting user's public key. Members can then debug without holding provider credentials.
- Record which providers support which features in the compatibility matrix.

### Provider portability

The logs interface is provider-neutral. The mechanism is per adapter.

---
_Generated by [Claude Code](https://claude.ai/code)_
