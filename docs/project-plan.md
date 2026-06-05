# Cloud Virtual Kubelet 项目规划

日期：2026-06-05

详细阶段推进、done criterion 和正确性评估见：[Cloud Virtual Kubelet 工作计划](work-plan.md)。

命名说明：项目名选定为 **AIBrix GPU Burst Node**。历史文本里的 `cvk` 只代表早期临时代码名，后续实现应逐步替换为 `gpu-burst-node`。

## 结论

这个项目有前景，但前提是不要把目标定成“任意标准 Pod 在任意云上完整等价运行”。正确定位应该是：

> 一个 Kubernetes-native 的云 GPU burst 层，把 Kubernetes Pod/Job 作为用户入口，把可远程运行的 AI/ML batch、训练、推理工作负载调度到 RunPod、Lambda Cloud、Vast、Salad 等云 GPU provider，并把状态、事件、成本和生命周期同步回 Kubernetes。

这个方向已经被验证过：Virtual Kubelet 本身是 CNCF sandbox 项目；SkyPilot 已经证明“跨云 GPU 调度”是强需求；RunPod 和 SaladCloud 已经出现 Virtual Kubelet 方向的实现。但现有方案要么是 SkyPilot 这种独立控制面和 YAML/CLI，要么是单 provider、许可受限或功能不完整的 Virtual Kubelet provider。我们的机会在于做一个开源、Kubernetes 原生、多 provider、明确 feature matrix 的实现。

不建议一开始对标 SkyPilot 的全部能力。SkyPilot 是“AI workload on any infra”的上层系统；我们应该对标它的一部分：跨云 GPU 可用性、成本选择、失败重试，但入口保持 Kubernetes API、GitOps、RBAC、Argo、Kubeflow/Ray/KServe 等 Kubernetes 生态。Kueue、策略 CRD、服务高可用这些能力应作为后续增强，不作为 MVP 前置依赖。

## 证据

### Virtual Kubelet 已经证明技术路径成立

Virtual Kubelet 的官方描述是一个“伪装成 kubelet 的开源 kubelet 实现”，让 Kubernetes node 可以由 serverless container platform 等 provider 支撑。它已有 AWS Fargate、Azure Container Instances、Alibaba ECI、Nomad、Liqo、InterLink 等 provider 列表，并明确说它的场景是把 Kubernetes API 扩展到 serverless container 平台。

参考：
- https://virtual-kubelet.io/
- https://github.com/virtual-kubelet/virtual-kubelet
- https://virtual-kubelet.io/docs/architecture/

Virtual Kubelet 的上游 README 还给出了一句很贴切的定位：“Kubernetes API on top, programmable back”。这正是我们要做的事情。

### SkyPilot 已经证明跨云 GPU 调度需求很强

SkyPilot 当前文档把 Kubernetes、Slurm、云 VM、existing machines 统一成 compute pool，支持 batch、development、pretraining、finetuning、batch inference 和 online serving；它还显式支持 `runpod` 和 `lambda` 作为 infra。RunPod 官方文档也有 “Running Runpod on SkyPilot”，说明 RunPod 已经接受 SkyPilot 作为生态入口。

参考：
- https://docs.skypilot.co/en/latest/overview.html
- https://docs.runpod.io/integrations/skypilot/
- https://github.com/skypilot-org/skypilot

这说明“跨云 GPU 调度”不是伪需求。但 SkyPilot 的入口是 SkyPilot CLI/YAML/API server。对 Kubernetes-first 团队来说，它是另一个控制面；我们的切入点是让 Kubernetes 自己继续作为控制面。

### RunPod/Lambda 的 API 足以做 provider，但语义差异很大

RunPod REST API 提供 Pods、Serverless endpoints、Network volumes、Templates、Container registry auth、Billing 等资源。它的 Create Pod API 可以直接传入 image、entrypoint、command、env、GPU type、GPU count、ports、network volume 等字段，所以 RunPod 是最适合作为第一个 MVP provider 的目标。

参考：
- https://docs.runpod.io/api-reference/overview
- https://docs.runpod.io/api-reference/pods/POST/pods

Lambda Cloud 的 On-Demand Cloud 更像 VM provider：可以用 API 列出 instance type、启动 instance、终止 instance，也支持 cloud-init 做启动期配置。官方文档明确说 instance 启动可能需要几分钟。它不是一个直接运行容器的 API，所以 Lambda provider 需要一个 VM runner/agent：启动 VM 后安装或使用 Docker/containerd，拉镜像，运行容器，回传状态和日志，最后终止 VM。

参考：
- https://docs.lambda.ai/public-cloud/on-demand/creating-managing-instances/
- https://docs-api.lambda.ai/api/cloud

### 已经有人在做相近方案，但还没有形成理想的开源标准解

已有一个 RunPod Virtual Kubelet 项目：`BSVogler/k8s-runpod-kubelet`。它把 RunPod 暴露成 Kubernetes virtual node，并支持通过 Pod annotations 指定 GPU memory、template、datacenter、ports 等。但它有明显限制：非官方集成、生产使用需要 GPU Conduit license、non-commercial license、无直接 exec/attach、日志支持受 RunPod API 限制。

参考：
- https://github.com/BSVogler/k8s-runpod-kubelet

SaladCloud 也有官方 Virtual Kubelet 方案，用 Kubernetes API 在 SaladCloud 创建 GPU container group。它的文档同时把限制写得很清楚：不实现 CNI/CSI，Kubernetes Services 不能直接暴露 virtual pods，volume mounting 不支持，`kubectl logs` 和 `kubectl exec` 不支持。

参考：
- https://docs.salad.com/container-engine/explanation/platform-integrations/kubernetes-integration

这两个案例证明方向有需求，但也说明我们的产品必须公开承认语义边界，不能承诺完整 Kubernetes 兼容。

### Karpenter/Kueue/Volcano 是相邻生态，不是直接替代

Karpenter 是 Kubernetes node lifecycle management：它监听 unschedulable pods，按 pod 的资源和调度约束创建真实节点，再在节点不需要时移除。它适合能把真实 node 加入集群的 cloud provider；如果 provider 是 RunPod 这种容器 API，或 Lambda 这种临时 VM 但不想完整加入集群，Virtual Kubelet 路径更直接。

参考：
- https://karpenter.sh/docs/
- https://github.com/kubernetes-sigs/karpenter

Kueue 是 Kubernetes-native batch/HPC/AI/ML job queueing 系统，负责多租户 quota、队列和何时运行，不负责把外部云 GPU 暴露成运行环境。它是后续可以集成的上层入口，不是 MVP 依赖。

参考：
- https://kueue.sigs.k8s.io/

Volcano 是 batch/AI/big data scheduler，支持 Ray、TensorFlow、PyTorch、Spark、Flink 等，并兼容 Kubernetes workload。它适合本集群或多集群调度；我们的重点是 provider adapter 和外部云生命周期。

参考：
- https://volcano.sh/docs/keyfeatures/unifiedscheduling/

## 项目定位

项目名：**AIBrix GPU Burst Node**。

一句话定位：

> Use Kubernetes as the control plane for burst GPU workloads across specialized cloud providers.

核心用户：

- 已经把训练、batch inference、数据处理、模型服务工作流放在 Kubernetes 上的团队。
- GPU 需求有明显 burst 特征，不想长期持有昂贵 GPU node。
- 愿意接受远程 GPU workloads 的语义子集：不要求 cluster-internal Service、CNI、CSI、DaemonSet、hostPath、privileged、完整 exec/logs 等。
- 想通过 GitOps、Kueue、Argo Workflows、Kubeflow、RayJob 等 Kubernetes API 提交任务，而不是迁移到另一个 CLI/control plane。

非目标：

- 不做完整 Kubernetes federation。
- 不保证任意 PodSpec 100% 兼容。
- 不替代 Karpenter 管理真实 Kubernetes node。
- 不替代 SkyPilot 的完整工作流、SSH sync、cluster lifecycle、spot recovery 和 serving API。
- 不优先服务低延迟、强 cluster networking 依赖的在线微服务。

## 竞品对比

| 方案 | 强项 | 弱点 | 我们的位置 |
| --- | --- | --- | --- |
| SkyPilot | provider 广、成本/可用性优化、AI workload 体验好 | 入口是 SkyPilot CLI/YAML/API server，不是原生 Pod/Job API | 学习它的 provider breadth 和优化策略，但入口保持 Kubernetes |
| Karpenter | 创建真实 node，Kubernetes 语义完整 | provider 必须能启动并加入 Kubernetes node；对 RunPod 这类容器 API 不自然 | 对能加入集群的 provider 可互补，不强行替代 |
| Kueue/Volcano | 队列、公平调度、batch/AI scheduler | 不负责外部 provider 生命周期 | 后续作为可选集成；MVP 直接用 default scheduler |
| 单 provider VK | 快速对接某个 provider | 不统一、许可/维护/功能差异大 | 做多 provider 框架和 conformance matrix |
| 直接调用 provider SDK | 简单、语义清晰 | 绕过 Kubernetes 控制面，RBAC/GitOps/可观测性割裂 | 用 Kubernetes 统一入口 |

## MVP 边界

第一版必须收窄：只做 RunPod provider，支持单容器 Pod 和 Kubernetes Job。

MVP 的硬约束：

- host cluster 只用 Kubernetes default scheduler，不做 custom scheduler。
- 用户入口只用标准 `Pod` / `Job` 加少量 annotations。
- 只新增一个内部状态 CRD：`CloudPodBinding`。
- `CloudProviderAccount`、`CloudPlacementPolicy`、`CloudNodeClass`、`CloudServicePolicy`、Kueue integration 都不放进 MVP。
- provider 配置先通过 Helm values / Secret / env 管理，后续再抽成 CRD。

支持：

- `Pod` / `Job`，优先 `restartPolicy: Never` 和 `OnFailure`。
- 单主容器：`image`、`command`、`args`、`env`、resource requests/limits。
- GPU 请求：`nvidia.com/gpu` 映射到 provider GPU count。
- provider hints：通过 annotations 指定 GPU types、datacenter、max price、interruptible、ports、network volume、registry auth。
- Pod phase/status：Pending、Running、Succeeded、Failed。
- Kubernetes Events：容量不足、provider API 失败、镜像拉取/启动失败、超预算、删除成功。
- 删除 Pod 时终止远程资源。
- Helm 安装。
- conformance matrix：明确每个 provider 支持哪些 PodSpec 字段。

暂不支持：

- 多容器 Pod、init containers、sidecars。
- `kubectl exec`、attach、port-forward。
- 完整 `kubectl logs`，除非 provider 有可靠日志 API；否则只能提供 best-effort 或 provider console 链接。
- Kubernetes Service/ClusterIP 直接访问 virtual pod。
- PVC/CSI、hostPath、emptyDir 以外的存储语义。
- privileged、hostNetwork、hostPID、hostIPC、DaemonSet。
- 需要低延迟 east-west traffic 的服务。

## 技术方案

### 架构

组件：

- `cvk`：我们自己的 binary，消费 upstream `virtual-kubelet` library，组装 node controller、pod controller 和 kubelet HTTP routes。
- `cvk-provider`：实现 Virtual Kubelet 的 `PodLifecycleHandler` / `PodNotifier` / `NodeProvider`，负责 Pod 生命周期桥接。
- `BindingStore`：内部接口，MVP 实现是 `KubernetesBindingStore`，读写 `CloudPodBinding` CRD。
- `Transformer`：内部纯逻辑，把 `corev1.Pod` 转成 `CloudWorkloadRequest`，并返回 unsupported field errors。
- `ProviderAdapter`：内部云 API 抽象，MVP 先实现 `RunPodAdapter`。
- `CloudPodBinding`：唯一新增 CRD，记录 Kubernetes Pod UID 到 provider resource ID 的绑定、价格、region、状态、错误和删除状态。
- `provider-lambda`：第二阶段实现，把 PodSpec 转成 Lambda Cloud VM + cloud-init + runner agent。

MVP 可以先把 PodSpec validation 放在 `Transformer` 和 `CreatePod` 路径里，用 Events/Pod condition 明确失败原因；验证逻辑稳定后再加 validating admission webhook，把不支持的 Pod 在调度前拒绝。

数据流：

1. 用户提交普通 Kubernetes Job/Pod。
2. default scheduler 根据 nodeSelector/tolerations 把 Pod 调度到 `cvk-runpod-*` virtual node。
3. `cvk-provider` 收到 `CreatePod`。
4. `Transformer` 检查 PodSpec 子集并生成 `CloudWorkloadRequest`。
5. `BindingStore` 创建或读取 `CloudPodBinding`，用 Pod UID 作为 idempotency key。
6. `RunPodAdapter` 创建远程资源。
7. `cvk-provider` 持续 reconcile provider 状态，更新 Pod status、events、CloudPodBinding。
8. 用户删除 Pod 或 Job 完成后，finalizer 驱动远程资源终止。

### 内部接口

MVP 内部边界建议抽象为：

```go
type BindingStore interface {
    GetByPodUID(ctx context.Context, namespace string, uid types.UID) (*CloudPodBinding, error)
    CreateOrUpdate(ctx context.Context, binding *CloudPodBinding) error
    MarkDeleting(ctx context.Context, binding *CloudPodBinding) error
}

type Transformer interface {
    Transform(ctx context.Context, pod *corev1.Pod) (*CloudWorkloadRequest, field.ErrorList)
}

type ProviderAdapter interface {
    Name() string
    Capabilities(ctx context.Context) (ProviderCapabilities, error)
    Create(ctx context.Context, req *CloudWorkloadRequest) (*RemoteHandle, error)
    Get(ctx context.Context, handle RemoteHandle) (*RemoteStatus, error)
    Delete(ctx context.Context, handle RemoteHandle) error
    Logs(ctx context.Context, handle RemoteHandle, opts LogOptions) (io.ReadCloser, error)
}
```

关键点：

- `BindingStore` 是代码接口，不是 CRD；MVP 只是把它落到 `CloudPodBinding`。
- `Transformer` 不碰 Kubernetes API，也不碰 provider API。
- `ProviderAdapter` 不碰 Kubernetes API，只处理云侧生命周期。
- `Create` 必须按 Pod UID / external name 做幂等。
- `Capabilities` 后续用于生成 conformance matrix。
- `Logs` 可以返回 `ErrUnsupported`，不能假装支持。
- `Exec` 不放进 MVP；后续如果 provider 支持再做可选接口。

### 调度模型

第一版不用改 scheduler，也不做定制 scheduler。使用 Kubernetes default scheduler 和原生机制：

- virtual node 带 taint：`cloudvk.io/provider=runpod:NoSchedule`
- Pod 加 toleration 才能调度过去。
- Node labels 表示 provider、region、GPU type、interruptible。
- annotations 表示 provider-specific hint。
- `CloudPodBinding` 记录事实状态，不作为用户提交 workload 的入口。

示例：

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: train-on-runpod
  annotations:
    cloudvk.io/provider: runpod
    cloudvk.io/gpu-types: "NVIDIA H100 80GB HBM3,NVIDIA L40S"
    cloudvk.io/datacenters: "US-TX-3,US-CA-2"
    cloudvk.io/max-price-per-hour: "3.50"
    cloudvk.io/interruptible: "true"
spec:
  template:
    spec:
      restartPolicy: Never
      nodeSelector:
        cloudvk.io/provider: runpod
      tolerations:
        - key: cloudvk.io/provider
          operator: Equal
          value: runpod
          effect: NoSchedule
      containers:
        - name: trainer
          image: ghcr.io/example/train:latest
          command: ["python", "train.py"]
          env:
            - name: DATASET_URL
              valueFrom:
                secretKeyRef:
                  name: train-secret
                  key: dataset_url
          resources:
            requests:
              cpu: "8"
              memory: 64Gi
              nvidia.com/gpu: "1"
            limits:
              nvidia.com/gpu: "1"
```

### 网络策略

必须承认 virtual pods 不在集群 CNI 里。MVP 只支持三种访问方式：

- 出站访问：远程 workload 主动访问对象存储、队列、数据库、HTTP API。
- Provider public endpoint：RunPod HTTP/TCP ports 暴露为外部地址，写回 Pod annotation/status。
- 可选隧道：后续做 Tailscale/WireGuard/agent proxy，但不作为 MVP 默认能力。

不要在 MVP 里承诺 Kubernetes Service selector 可以直接选中 virtual pod 并通过 ClusterIP 访问。

### 存储策略

MVP：

- env/config/secret 支持。
- `emptyDir` 映射到 provider 临时盘。
- RunPod network volume 通过 annotation 绑定。

后续：

- 对象存储挂载。
- NFS/SSHFS/rclone 作为 runner 层能力。
- CSI 不作为短期目标。

### Lambda Cloud provider 策略

Lambda Cloud 不是第一阶段，因为它没有直接的 container Pod API。第二阶段方案：

1. controller 调 Lambda API launch instance。
2. cloud-init 安装或启动 `cvk-runner`。
3. `cvk-runner` 拉取镜像，运行容器，采集 exit code、stdout/stderr、heartbeat。
4. runner 通过 Kubernetes API callback、object storage 或 controller endpoint 回传状态。
5. job 完成或 Pod 删除时 terminate instance。

这个 provider 更像 “one Pod per VM”。启动慢、成本高、但语义可控，适合长训练或重 batch，不适合短任务。

## 路线图

### Phase 0：技术 spike，1-2 周

目标：证明 RunPod 单容器 Job 可以通过 Kubernetes Pod 生命周期驱动。

交付：

- 新建自己的 `cvk` Go module/binary，消费 upstream `virtual-kubelet` library，不 fork upstream。
- kind/minikube 上运行一个最小 Virtual Kubelet provider。
- `CloudPodBinding` CRD 和 `BindingStore`。
- fake provider adapter。
- 创建/删除 RunPod Pod。
- Pod status 从 Pending 到 Running/Failed。
- 失败事件可见。
- 最小 README 和 conformance matrix 草案。

### Phase 1：RunPod MVP，4-6 周

目标：能被早期用户试用。

交付：

- Go module、Docker image、Helm chart。
- 支持 Job/Pod、单容器、GPU count、env、command、ports、datacenter、max price、interruptible。
- `Transformer` 内置 validation；validating admission webhook 作为生产前增强，不阻塞第一个可跑版本。
- CloudPodBinding CRD。
- 预算和 TTL 防护。
- e2e 测试：fake provider + 可选 real RunPod integration test。
- 示例：普通 Kubernetes Job、Argo Workflow。

### Phase 2：Provider framework + Lambda VM runner，6-8 周

目标：证明同一套 provider interface 能同时覆盖 container provider 和 VM provider。

交付：

- provider interface 稳定。
- Lambda Cloud provider。
- `cvk-runner` agent。
- 日志/状态 callback。
- VM cleanup finalizer。
- provider conformance matrix 自动生成。

### Phase 3：多 provider placement/fallback mechanism，8-12 周

目标：在 provider 接口稳定后，建立干净、可解释的 provider/region/price/capacity 选择机制。

交付：

- provider price/capacity cache。
- fallback：RunPod 容量不足时尝试 Lambda/Vast/Salad。
- max price / TTL。
- retry/backoff、capacity error classification。
- `CloudPodBinding.status.attempts` 记录每一次 provider/region 尝试。
- metrics：launch latency、success rate、cost/hour、orphan resource count。
- provider conformance suite。
- 文档站和 compatibility table。

## 成功标准

技术成功：

- 单个 Kubernetes Job 可以在 RunPod 上创建、运行、终止，Pod status 与远端状态一致。
- 远程资源不会孤儿化。
- unsupported PodSpec 会被拒绝，而不是静默错误。
- provider API 容量不足、认证失败、超预算等错误能转成 Kubernetes Events。

产品成功：

- 用户不用学习新 DSL，只需要普通 Pod/Job + 少量 annotations。
- 能和 Argo/Kubeflow/RayJob 的现有 Kubernetes 工作流组合；Kueue 集成作为后续增强。
- 至少两个 provider 可用，其中一个 direct-container provider，一个 VM provider。
- 有清晰 provider conformance matrix。

社区成功：

- Apache-2.0 或同等宽松开源协议。
- provider adapter 可独立贡献。
- 文档不夸大 Kubernetes 兼容性，直接列限制。

## 最大风险与缓解

### 风险 1：Kubernetes 语义不完整

远程 provider 很难支持 CNI、CSI、Service、exec、logs、sidecar 等完整语义。

缓解：

- `Transformer` / `CreatePod` validation 严格拒绝 unsupported features 并写 Events；后续再加 admission webhook 做调度前拒绝。
- conformance matrix 首屏展示。
- 项目定位为 remote GPU workload adapter，而不是 full node replacement。

### 风险 2：provider API 不稳定或能力不够

RunPod、Vast、Salad 等 specialized GPU cloud 的 API、容量、价格和限制会变化。

缓解：

- provider adapter 独立版本化。
- OpenAPI/SDK 生成 client。
- e2e smoke test 定期跑。
- provider failure 分类进入 retry/fallback 逻辑。

### 风险 3：成本失控

Kubernetes 用户可能误把 Deployment 扩到很多副本，导致远程 GPU 成本爆炸。

缓解：

- 默认 taint，必须显式 toleration。
- Helm/annotation 层的 max price、TTL、max replicas；namespace 级策略后续再做。
- 对 Deployment 默认拒绝或强警告，MVP 优先 Job。

### 风险 4：冷启动太慢

RunPod 拉大镜像、Lambda 启动 VM 都可能很慢。

缓解：

- 面向 batch/long-running inference，不承诺秒级启动。
- 支持 template/prebaked image/network volume。
- 指标化 launch latency。
- 后续支持 warm pool。

### 风险 5：安全边界复杂

Secret、registry credentials、provider API key、远程 workload callback 都需要清晰隔离。

缓解：

- MVP 先用 Helm 指定的 Kubernetes Secret / env 管理 provider API key。
- 更细的 namespace 级 credential isolation 后续再做。
- 最小 RBAC。
- `cvk-provider` 可以访问 host apiserver 写 `CloudPodBinding`、Pod status 和 Events；`ProviderAdapter` 不直接访问 Kubernetes API server。

## 推荐第一步

先做 RunPod-only 开源 MVP，不要一上来做“所有 cloud provider”。

原因：

- RunPod API 与 PodSpec 映射最直接。
- 已有第三方 RunPod VK 证明可行，但许可和生产可用性留下空间。
- 这个 MVP 能最快验证 Kubernetes-first 用户是否愿意用标准 Job/Pod 调远程 GPU。

第一批 issue：

1. 初始化 Go 项目和 Helm chart。
2. 新建自己的 `cvk` binary，import upstream `virtual-kubelet` library，不 fork upstream。
3. 实现 fake provider，先跑本地 e2e。
4. 实现 RunPod client：create/get/delete pod。
5. 实现 PodSpec -> RunPod request 映射。
6. 实现 CloudPodBinding CRD 和 finalizer。
7. 实现 `BindingStore` / `Transformer` / `ProviderAdapter` 内部接口。
8. 写 conformance matrix。
9. 写第一个 Job 示例。
10. 增加 orphan sweeper。

## Go / No-Go 判断

Go：

- 目标用户是 Kubernetes-first 的 ML infra 团队。
- 主要 workload 是 GPU batch、训练、batch inference、异步推理。
- 用户能接受 remote workload 不具备完整 cluster networking/storage 语义。
- 你愿意把项目做成 provider conformance + Kubernetes lifecycle sync，而不是“万能云调度器”。

No-Go：

- 目标是完全替代 SkyPilot。
- 目标是任意 PodSpec 无改动跨云运行。
- 目标 workload 依赖 Service mesh、PVC、低延迟 pod-to-pod 通信、privileged、复杂 sidecar。
- 你无法持续跟进 provider API 变化。

最终建议：值得做，但应以 “Kubernetes-native GPU cloud burst adapter” 切入；先 RunPod + default scheduler + 一个 `CloudPodBinding` CRD 跑通可靠生命周期，后 Lambda VM runner，再做多 provider placement/fallback mechanism 和 provider conformance。
