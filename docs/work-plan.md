# Cloud Virtual Kubelet 工作计划

日期：2026-06-05

命名说明：项目名选定为 **AIBrix GPU Burst Node**。历史文本里的 `cvk` 只代表早期临时代码名，后续实现应逐步替换为 `gpu-burst-node`。

这份计划把项目拆成可以逐阶段验证的工程路线。当前共识是：MVP 保持简单，不引入定制 scheduler、不引入 Kueue、不引入一组策略 CRD；用户入口仍然是标准 `Pod` / `Job` 加少量 annotations；只新增一个内部状态 CRD `CloudPodBinding`；`BindingStore`、`Transformer`、`ProviderAdapter` 都是代码内部接口，不是 CRD。

## 总体阶段

| 阶段 | 目标 | Done criterion | 正确性评估 | 借鉴对象 |
| --- | --- | --- | --- | --- |
| Phase 0 | 建立 CVK 骨架和可测控制面 | `cvk` binary 能在 kind 注册 virtual node，fake provider 能跑通 Pod create/status/delete，`CloudPodBinding` CRD 可读写 | fake provider E2E、controller restart/idempotency 测试、`go test ./...` | upstream Virtual Kubelet |
| Phase 1 | 定义 Pod transformation contract | 明确支持和拒绝的 PodSpec 子集，unsupported spec 有 condition/event，不 silent ignore | Transformer table tests、golden provider request、兼容性矩阵 | SkyPilot capability matrix、SaladCloud limitation docs、BSVogler annotations |
| Phase 2 | RunPod lifecycle thin slice | 一个 Kubernetes Job 可以创建一个 RunPod pod，状态同步，删除清理，无重复创建 | RunPod adapter contract tests、real integration tests、故障注入 | BSVogler RunPod proof、RunPod API |
| Phase 3 | RunPod production MVP | Helm/RBAC/metrics/orphan sweeper/cost guard/logs/link/docs 完成，可给早期用户试用 | upgrade/restart/soak/leak/RBAC audit/成本保护测试 | SkyPilot cost metrics、provider catalog、RunPod template/network volume |
| Phase 4 | Lambda Cloud VM runner | 第二 provider 跑通 VM launch + runner + container + terminate | runner contract tests、VM boot timeout、callback loss、real Lambda smoke test | SkyPilot Lambda support、VM provisioning model |
| Phase 5 | Placement/fallback mechanism | 在 provider 接口稳定后启动可解释的 provider/region/price/capacity fallback，并记录 attempts | deterministic placement tests、capacity failure simulation、fallback lineage check | SkyPilot optimizer、managed jobs recovery |

## Phase 0: CVK 骨架和正确性底座

目标不是接真实云，而是先把 Kubernetes 控制面闭环做对。这个阶段如果做不好，后面接 RunPod 只会把问题放大。

### Scope

- 新建我们自己的 `cvk` Go module 和 binary。
- 导入 upstream `github.com/virtual-kubelet/virtual-kubelet`，使用其 public `node` / `nodeutil` 扩展点。
- 实现一个 Kubernetes-aware provider wrapper。
- 定义 `CloudPodBinding` CRD，作为唯一 MVP CRD。
- 实现 `BindingStore` 接口，MVP backend 是 host cluster apiserver。
- 实现 fake `ProviderAdapter`，不访问任何真实云 API。
- 在 kind 或本地集群中注册一个 virtual node。

### Done Criterion

- `kubectl get node` 能看到 CVK virtual node，状态为 Ready。
- 带 `nodeSelector` / toleration 的测试 Pod 能被 default scheduler 绑定到 CVK node。
- fake provider 能完整执行 `CreatePod -> Running -> Succeeded/Failed -> DeletePod`。
- 每个 Pod UID 对应一个 `CloudPodBinding`，包含 provider、remote id、phase、attempts、finalizer、last error。
- 重启 `cvk` 后不会重复创建 fake remote resource。
- 删除 Pod 时，remote delete 成功或 remote not found 后才移除 finalizer。
- `go test ./...` 通过。
- CRD manifest 和 Helm template 可以渲染。

### 正确性评估

- 单元测试覆盖 `BindingStore` 的 create/update/delete/finalizer/idempotency。
- fake provider 支持故障注入：create timeout、delete 5xx、remote not found、status unknown。
- E2E 测试验证控制器重启后能从 `CloudPodBinding` 恢复状态。
- 检查 invariant：同一个 Pod UID 最多只有一个 remote resource。
- 检查 invariant：`ProviderAdapter` 不直接访问 Kubernetes API。

### 借鉴点

- 借鉴 upstream Virtual Kubelet 的 pod lifecycle controller、node status、pod notifier 和 kubelet API route helper。
- 不 fork upstream Virtual Kubelet；除非 public API 出现明确 blocker，才考虑 upstream PR。
- 暂不借鉴 SkyPilot 的优化器，也不接 RunPod，避免在生命周期模型没稳定前引入云 API 噪声。

## Phase 1: Pod Transformation Contract

这个阶段决定“标准 Pod 转云实例”到底意味着什么。核心原则是：支持面可以小，但不能 silent ignore。

### Scope

- 实现 `Transformer`，把 Kubernetes `Pod` 转成 provider-neutral `RunRequest`。
- 定义 MVP 支持的 PodSpec 子集。
- 定义明确的 reject reason 和 Pod condition/event。
- 建立兼容性矩阵文档。
- 先在 `CreatePod` / `Transformer` 路径校验；验证稳定后再加 validating admission webhook。

### MVP 支持

- 单主容器。
- `image`、`command`、`args`。
- `env`、`envFrom`、`ConfigMapKeyRef`、`SecretKeyRef`。
- `resources.requests/limits` 中的 CPU、memory、`nvidia.com/gpu`。
- `restartPolicy: Never` 和 `OnFailure`。
- 基础 ports 映射。
- `imagePullSecrets`，但只能映射到 provider 支持的 registry auth 机制。
- 少量 provider hints annotations，例如 GPU type、region/datacenter、max price、provider preference。

### MVP 拒绝

- 多主容器和 sidecar。
- 任意 volume mount，除非显式支持为 provider-native volume。
- hostPath、PVC、CSI、projected service account token。
- privileged、hostNetwork、hostPID、hostIPC。
- Kubernetes Service selector 直接访问 virtual pod。
- 需要完整 CNI/CSI/kube-proxy 语义的 workload。

### Done Criterion

- 每个支持字段都有 transformation 测试。
- 每个拒绝字段都有稳定错误码、condition reason 和 event message。
- 兼容性矩阵写入 docs，用户能提前知道能不能跑。
- `Transformer` 输出 provider-neutral request，不包含 RunPod 或 Lambda 专有类型。
- annotations 的 schema 和优先级明确：Pod annotations > namespace/default config > provider default。

### 正确性评估

- table-driven tests：输入 PodSpec，输出 `RunRequest` 或明确 reject reason。
- golden tests：固定 PodSpec 生成固定 provider-neutral request。
- Secret/ConfigMap 测试：required missing 时失败，optional missing 时按 Kubernetes 语义处理。
- annotation fuzz-ish tests：非法 GPU type、价格、region、fallback order 都能稳定拒绝或降级。
- conformance docs 与测试用例一一对应。

### 借鉴点

- 借鉴 SkyPilot 的 capability matrix 思路：不同 provider 能力不同，但用户要看得见。
- 借鉴 BSVogler/k8s-runpod-kubelet 的 annotation vocabulary，尤其是 GPU/datacenter/template/ports 类 hints。
- 借鉴 SaladCloud Virtual Kubelet 文档的诚实边界：不支持 CNI/CSI/Service/logs/exec 时明确写出来。

## Phase 2: RunPod Lifecycle Thin Slice

这个阶段只做一个真实云闭环：一个 Kubernetes Job 对应一个 RunPod pod。先把生命周期做可靠，再扩功能。

### Scope

- 实现 `RunPodAdapter`。
- 支持 create/get/delete/list-by-external-name。
- 将 provider 状态映射到 Pod phase、container status、Pod condition 和 `CloudPodBinding.status`。
- 用 Pod UID 作为 idempotency key 或 external name。
- 支持 finalizer 驱动的删除。
- 支持 RunPod API credential 通过 Helm values / Secret / env 注入到 CVK deployment。

### Done Criterion

- 提交一个 Kubernetes Job 后，RunPod 创建出一个 pod。
- `CloudPodBinding.status.remoteID` 和 provider placement 信息正确写入。
- RunPod Running 映射到 Kubernetes Pod Running。
- RunPod Completed/Failed 映射到 Kubernetes Pod Succeeded/Failed，并保留 exit/error 信息。
- 删除 Kubernetes Pod/Job 后，远端 RunPod pod 被终止。
- ambiguous create error 后 reconcile 不会重复创建多个 RunPod pod。
- 手工删除远端 RunPod pod 后，本地状态能进入明确 Failed/Deleted/Unknown 路径，而不是无限 Pending。
- `kubectl describe pod` 能看到关键 Events。

### 正确性评估

- RunPod adapter contract tests 用 fake HTTP server 覆盖 create/get/delete/status/list。
- gated real integration tests：只有设置 `RUNPOD_API_KEY` 才运行。
- 故障注入：429、5xx、network timeout、create 成功但 response lost、delete timeout、remote not found。
- leak test：测试完成后 RunPod 上没有残留资源。
- restart test：在 create 后、status sync 前、delete 前分别重启 `cvk`，验证不重复、不泄漏。

### 借鉴点

- 借鉴 BSVogler 项目已经跑通过的 RunPod lifecycle、GPU/datacenter 参数、status mapping、Job annotation fallback。
- 规避 BSVogler 的问题：不使用内存 map 做 durable state，不依赖 Conduit，不隐藏 create/delete error，不 force delete 用户 Pod。
- 借鉴 RunPod API 的 native 能力：pods、templates、registry auth、network volumes、ports。

## Phase 3: RunPod Production MVP

这个阶段的目标是给早期用户试用，而不是继续做 provider breadth。重点是可运维、可观测、可控成本。

### Scope

- Helm chart 和最小 RBAC。
- controller metrics：create latency、delete latency、provider errors、active remote resources、cost estimate、reconcile retries。
- orphan sweeper：定期扫描有 CVK external name/label 但没有活跃 binding 的远端资源。
- 成本保护：max runtime、max price、TTL、namespace-level default。
- 日志策略：如果 RunPod API 支持，接 `kubectl logs`；如果不支持，写 event 和 provider console link。
- provider capability docs。
- 示例：plain Pod、Job、Argo Workflow、Kubeflow/RayJob 兼容路径。

### Done Criterion

- Helm install 后无需手工 patch 即可注册 virtual node。
- RBAC 不允许 provider adapter 任意读取全 cluster secrets。
- 关键状态都能通过 Pod condition、Events、`CloudPodBinding`、Prometheus metrics 查到。
- orphan sweeper 在安全 dry-run 模式和实际 cleanup 模式都可用。
- 成本 guard 能阻止未设置上限或超过策略的任务。
- 文档中明确列出 MVP 支持/不支持项。
- 至少一个真实用户工作流可以端到端跑通。

### 正确性评估

- Helm template、RBAC audit、least privilege review。
- upgrade test：从前一个 chart 版本升级后 bindings 不丢。
- soak test：连续运行 N 个短 Job，确认没有 goroutine/resource leak 和远端 orphan。
- cost guard tests：超过 max price/max runtime 的任务被拒绝或终止，并有明确 reason。
- private registry tests：验证 imagePullSecrets 映射不会泄漏 namespace 外 secrets。

### 借鉴点

- 借鉴 SkyPilot 的 cost/burn-rate metrics，但用 Kubernetes metrics 和 `CloudPodBinding.status.cost` 表达。
- 借鉴 SkyPilot catalog/capability 思路，形成 RunPod provider scorecard。
- 借鉴 RunPod native template、network volume、registry auth，但只在语义清楚时暴露。
- 借鉴 SaladCloud 的限制说明，把 Service/CNI/CSI/logs/exec 的边界写清楚。

## Phase 4: Lambda Cloud VM Runner

Lambda Cloud 和 RunPod 不同，它更像 VM provider。这个阶段验证 `ProviderAdapter` 抽象能不能同时覆盖 container API 和 VM-backed API。

### Scope

- 实现 `LambdaAdapter`。
- 启动 Lambda instance。
- 通过 cloud-init 或 SSH bootstrap 一个轻量 runner。
- runner 拉镜像、运行容器、采集 exit code、回传状态和日志。
- 任务结束或删除时 terminate VM。
- 记录 VM instance id、region、instance type、runner state 到 `CloudPodBinding`。

### Done Criterion

- 一个 Kubernetes Job 能在 Lambda VM 上启动容器并完成。
- VM boot timeout 有明确状态和 retry/cleanup 行为。
- runner crash 或 callback loss 后，controller 能进入明确 recovery 路径。
- 删除 Pod 时 VM 被 terminate。
- Lambda provider 与 RunPod provider 共用同一个 `Transformer` 和 `ProviderAdapter` contract。

### 正确性评估

- fake Lambda API 覆盖 launch/get/terminate/list。
- runner contract tests：start、heartbeat、status、logs、exit code。
- 故障注入：VM launch timeout、cloud-init failure、runner never starts、container pull failure、terminate 5xx。
- real smoke test：用最小镜像跑一个短任务，验证无残留 VM。

### 借鉴点

- 借鉴 SkyPilot 对 Lambda instance type/capacity 的处理经验。
- 借鉴 SkyPilot VM provisioning 的重试、bootstrap、status 经验，但不引入 Ray cluster 和 SkyPilot YAML 作为控制面。
- 把 VM runner 作为 provider implementation detail，不暴露成用户必须理解的新 workload API。

## Phase 5: Placement/Fallback Mechanism

这是一个横切 mechanism，不是必须等所有 provider 都做完才开始。更好的时机是：Phase 1 把 `ProviderAdapter` / provider-neutral request 接口定下来后，就先用 fake provider 和 RunPod 实现第一版 placement；Phase 4 有 Lambda 作为第二 provider 后，再验证真正的跨 provider fallback。

仍然不需要 custom scheduler。default scheduler 只负责把 Pod 放到 virtual node，真实 provider placement 在控制器内部完成，并记录到 binding。

### Scope

- provider-neutral placement request。
- provider capability cache：GPU type、region、price、quota、unsupported feature。
- 简单 placement policy：provider order、region order、max price、allow fallback、spot/on-demand preference。
- 错误分类：capacity、quota、auth、invalid spec、price exceeded、retryable infra error。
- `CloudPodBinding.status.attempts` 记录每次 provider/region 尝试和失败原因。
- blocked-offer cache：短时间内避免反复尝试已知无容量组合。

### Done Criterion

- 用户可以通过 annotations 或 Helm default 指定 provider preference 和 fallback order。
- RunPod 容量不足时可以 fallback 到 Lambda，前提是 PodSpec 被 Lambda provider 支持。
- invalid spec 不触发 fallback。
- auth/quota 类错误按配置决定是否 fallback，默认保守处理并清晰报错。
- 每次 fallback 都写入 `CloudPodBinding.status.attempts` 和 Events。
- placement 结果可解释：为什么选这个 provider、为什么跳过其他 provider。

### 正确性评估

- deterministic placement unit tests：相同输入和 catalog 输出相同选择。
- provider error classifier tests。
- simulated capacity failure E2E：第一个 provider 返回 capacity exhausted，第二 provider 成功。
- no-silent-downgrade tests：不能把需要 volume/port/security 特性的 Pod 悄悄降级到不支持的 provider。
- blocked-offer cache tests：cache 有 TTL，不能永久拉黑某个 GPU/region。

### 借鉴点

- 借鉴 SkyPilot optimizer/catalog 的思想，但先实现简单、可解释的规则引擎。
- 借鉴 SkyPilot managed jobs recovery，把失败 lineage 写进 Kubernetes-native status。
- 暂不引入复杂 CRD；当 annotations 和 Helm defaults 表达不下去时，先收敛结构化配置模型，再决定是否需要新 API。

## 正确性模型

所有阶段都要围绕几个 invariant 做验证。

### Lifecycle Invariants

- 一个 Kubernetes Pod UID 最多对应一个活跃 remote resource。
- `CloudPodBinding` 是状态恢复来源，不能只靠内存 map。
- 远端资源删除成功或 remote not found 后，才能移除 finalizer。
- create/delete/status 的 retry 必须幂等。
- unsupported PodSpec 必须被拒绝或明确标记失败，不能 silent ignore。
- provider transient error 和 terminal error 必须区分。
- `ProviderAdapter` 不能直接访问 Kubernetes API；Kubernetes 读写集中在 CVK provider wrapper / `BindingStore`。
- controller 重启后，能从 `CloudPodBinding` 和 provider list/get 恢复 reconcile。
- 任何远端付费资源都必须能从 Kubernetes object、external name 或 provider tag 追溯回来。

### Testing Pyramid

| 层级 | 覆盖内容 | 运行频率 |
| --- | --- | --- |
| Unit tests | `Transformer`、`BindingStore`、error classifier、status mapper | 每次 CI |
| Fake provider E2E | Pod lifecycle、finalizer、restart、failure injection | 每次 CI |
| Provider contract tests | RunPod/Lambda API client against fake HTTP server | 每次 CI |
| Gated real integration tests | RunPod/Lambda 真 API smoke tests | nightly 或手工 |
| Soak/leak tests | 多 Job、重启、删除、orphan sweeper | release 前 |
| Upgrade tests | CRD/chart/controller 升级兼容 | release 前 |

## Go / No-Go Gates

### Gate after Phase 2

如果 RunPod create/delete 不能做到幂等、可恢复、无 orphan，先停下来修生命周期，不进入多 provider。

### Gate after Phase 3

如果早期用户不能接受 MVP 的 PodSpec 子集、日志限制或 Service/CNI 限制，要重新评估产品定位：继续做 batch GPU burst，还是转向更重的 VM/node join 模型。

### Gate after Phase 4

如果 Lambda VM runner 运维成本过高，先把它标记为 experimental，不要让它拖累 RunPod production path。

### Gate after Phase 5

如果简单规则引擎已经无法解释 placement/fallback，先收敛 provider capability model 和 error taxonomy。不要急着把 policy、account、budget、service HA 这些产品层概念提升为 CRD。

## 借鉴项目落点

| 来源项目 | 具体优点 | 落地阶段 | 我们的取舍 |
| --- | --- | --- | --- |
| upstream Virtual Kubelet | Kubernetes node/pod lifecycle abstraction | Phase 0 | import library，不 fork |
| BSVogler/k8s-runpod-kubelet | RunPod lifecycle proof、annotation hints、status mapping 经验 | Phase 1-2 | 只借鉴经验，不复用 Conduit/in-memory state/silent errors |
| SkyPilot catalog/capability | provider 能力、价格、GPU 规格抽象 | Phase 1、3、5 | Kubernetes-native status/metrics，不引入 SkyPilot YAML |
| SkyPilot optimizer | provider/region/cost/capacity 选择 | Phase 5 | 先规则引擎，后优化器 |
| SkyPilot managed jobs | failure recovery、attempt lineage | Phase 5 | 写入 `CloudPodBinding.status.attempts` |
| SaladCloud Virtual Kubelet | 清晰披露 VK provider 限制 | Phase 1、3 | 用 docs 和 condition 明确不支持项 |

## 近期 Issue 切分

1. Scaffold `cvk` Go module and binary.
2. Wire upstream Virtual Kubelet public provider interfaces.
3. Add `CloudPodBinding` CRD and generated client.
4. Implement `KubernetesBindingStore`.
5. Implement fake `ProviderAdapter`.
6. Implement Pod lifecycle reconcile with finalizer and idempotency key.
7. Implement `Transformer` MVP contract.
8. Add fake provider E2E tests in kind/envtest.
9. Implement RunPod API client behind contract tests.
10. Add RunPod thin-slice integration test gated by `RUNPOD_API_KEY`.
11. Add Helm chart and least-privilege RBAC.
12. Add compatibility matrix and first examples.
