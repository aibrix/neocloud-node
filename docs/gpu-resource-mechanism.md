# GPU Resource Mechanism

Date: 2026-06-07

This document summarizes the reusable GPU acquisition mechanism behind catalog, price estimate, fallback, fault tolerance and provider launch. The key decision is that this layer should be a Kubernetes-free resource manager library. GPU Burst Node, AIBrix batch and AIBrix online serving can all call it, but persistence and user-facing surfaces remain owned by each product.

Diagram: [Mermaid source](../figures/gpu-resource-mechanism.md), [SVG](../figures/gpu-resource-mechanism.svg), [PNG](../figures/gpu-resource-mechanism.png).

## One-Sentence Model

Normalize workload demand into `ResourceIntent`, normalize provider supply into `OfferCatalog`, pick an explainable `AcquirePlan`, execute it through idempotent `ProviderAdapter` attempts, and feed observations back into catalog, cost and failure signals.

```text
ResourceIntent
  -> OfferCatalog + PriceEstimator + FailureSignals + PlacementPolicy
  -> PlacementPlanner
  -> AcquirePlan(selected + fallback candidates)
  -> AcquireOrchestrator
  -> ProviderAdapter
  -> Allocation / Observation / Attempt / Cost
```

The important boundary is that `ResourceIntent`, `OfferCatalog`, `PlacementPlanner`, `AcquirePlan`, `ProviderAdapter`, `Attempt` and `CostStatus` are reusable mechanism concepts. Pod annotations, `CloudPodBinding`, Kubernetes Events, AIBrix `JobStore` and online-serving replica state are product-side adapters.

## Current Implementation Mapping

| Mechanism | Current code | Notes |
| --- | --- | --- |
| Workload intent | `pkg/resource.Intent`, `IntentFromWorkloadRequest` | Today it starts from `core.WorkloadRequest`; batch/serving should build the same intent directly. |
| Offer model | `pkg/resource.Offer` | Captures provider, location, GPU, market, price, capabilities, cold start, expected lifetime and migration cost. |
| Catalog | `CompositeCatalog`, `ObservedCatalog`, `ProviderOfferSource` | Lambda has native `/instance-types`; RunPod uses bootstrap/list/observation because there is no stable catalog path in this implementation. |
| Placement | `resource.Planner`, `resource.Engine`, `LifecycleCandidatePlanner` | Deterministic filter and risk-adjusted ranking, with fallback candidates. |
| Failure feedback | `core.Attempt`, `Policy.WithFailureSignals`, `BlockedOffersFromAttempts` | Capacity/rate-limit/transient/timeout attempts become temporary provider/location cooldowns. |
| Provider execution | `core.LifecycleService`, `ProviderAdapter` | Idempotent create path, classified failures and fallback between candidates. |
| Cost | `core.CostStatus`, `CreateCandidate.EstimatedHourlyCost`, `RemoteResource.HourlyCost` | Catalog estimate is now persisted; provider billing overrides it when available. |
| Surfaces | `CloudPodBinding`, Pod conditions, metrics | VK writes Kubernetes-native status. Batch/serving should write equivalent records into their own stores. |

## Mechanism Details

### Catalog And Capacity

The catalog should not mean only a provider's official inventory API. It should be a normalized stream of offers from several sources:

- static bootstrap rows supplied by config.
- provider-native catalog, such as Lambda instance types and regions with capacity.
- list/get observations from resources we already created.
- capacity probes when the provider API supports cheap dry-run or availability checks.
- failure observations, for example capacity exhausted in a datacenter.
- future scorecard data, such as launch success rate and time-to-running.

This is where our model intentionally differs from a pure static price table. RunPod-style providers may not expose a clean catalog API, so the production answer is an observed catalog plus TTL, not hardcoded create-time guessing.

### Price Estimate And Burn Rate

There are two different prices:

- `estimated_hourly_cost`: selected offer price from catalog or provider metadata.
- `current_burn_rate`: provider-observed billing for the running allocation.

The lifecycle should persist the estimate at acquire time so users can see expected cost immediately. When RunPod/Lambda later return native billing or instance-type price, that provider value overrides the estimate. This gives batch and serving the same cost model:

- batch can project total cost from elapsed time and remaining runtime.
- serving can compute current fleet burn rate and scale decisions.
- fallback can report cost delta from original candidate to selected candidate.

### Fallback And Fault Tolerance

Fallback must consume stable error classes, not provider strings. The core policy is:

| Class | Same offer | Different location | Different provider | Default meaning |
| --- | --- | --- | --- | --- |
| `Capacity` | No | Yes | Yes | Normal scarce GPU path. |
| `RateLimited` | Bounded retry | Maybe | Yes | Avoid immediate hot loop. |
| `Transient` | Bounded retry | Yes | Yes | Provider 5xx/network instability. |
| `Timeout` | Verify by list/get first | Yes | Yes | Create response may be lost. |
| `Auth` | No | No | No | Configuration problem, do not spread. |
| `UnsupportedSpec` | No | No | Only if capability matrix says supported | Never silently downgrade workload semantics. |
| `RemoteNotFound` | No | No | Maybe | Reconcile path; remote manually deleted or provider cleanup. |

The durable attempt log is the bridge between execution and strategy. A failed create writes an attempt with provider, location, class and fallback flags. The next planning pass converts recent fallback-eligible attempts into cooldown blocks, so the planner avoids repeatedly choosing a known-bad offer.

### Spot And Interruptible

Spot should be a policy decision, not a provider annotation passthrough. The shared model needs:

- recoverability: stateless, restartable, checkpointed or non-recoverable.
- deadline and remaining runtime.
- cold-start estimate.
- expected lifetime or interruption risk.
- migration/checkpoint transfer cost.
- on-demand safety net and fallback buffer.

The SkyNomad-style rule we should adopt is: admit spot while the workload can still make useful progress and safely fall back before the deadline; switch to on-demand when slack is too tight. For online serving, spot is only reasonable behind redundancy and traffic shifting. For batch, spot can be the default when checkpointing and retry lineage exist.

## SkyPilot Gap Analysis

SkyPilot is still ahead as an end-to-end cloud business control plane. Its current docs present it as a system for development, training, batch jobs and services across clouds, Kubernetes and existing machines. Managed jobs handle retries, failure recovery, spot preemption recovery and cleanup. Its catalog project maintains provider price/inventory data, and SkyPilot Serve has serving-specific placement/recovery work. References: [SkyPilot overview](https://docs.skypilot.co/en/latest/overview.html), [managed jobs](https://docs.skypilot.co/en/latest/examples/managed-jobs.html), [spot jobs](https://docs.skypilot.co/en/latest/examples/spot-jobs.html), [skypilot-catalog](https://github.com/skypilot-org/skypilot-catalog), [SkyServe](https://arxiv.org/abs/2411.01438).

| Area | SkyPilot | GPU Burst Node direction |
| --- | --- | --- |
| Control plane | SkyPilot CLI/API/YAML and its own job/service controller. | Kubernetes-first for VK; shared library for AIBrix batch/serving. |
| Catalog maturity | Broad, refreshed provider catalogs and pricing tables. | Lambda native catalog plus observed catalog; need RunPod/Vast/catalog probes and scorecard. |
| Optimizer | Mature resource filling and cross-cloud cost optimization. | Deterministic, explainable rule engine today; add scorecard and richer objective later. |
| Managed jobs | Built-in retries, spot recovery, cleanup and job lifecycle. | Do not put a job queue in VK; expose durable attempts so AIBrix batch can own lineage. |
| Serving | SkyPilot/SkyServe has serving-specific placement and spot hedging. | Reuse resource manager, but serving needs replica/SLO/traffic-aware policy. |
| Kubernetes integration | Can target Kubernetes as one infra backend. | Kubernetes is the user control plane for GPU Burst Node, not just a backend. |
| Observability | SkyPilot surfaces through its own CLI/API/server. | `CloudPodBinding`, Pod conditions, Events and Prometheus metrics. |

The conclusion is not "reuse SkyPilot directly". The useful reuse is conceptual:

- catalog as a first-class artifact.
- resource intent separated from provider launch.
- optimizer/planner separated from provisioner.
- managed-job recovery ideas expressed as durable attempts and lineage.
- serving placement treated differently from batch placement.

The part we should not copy is the user-facing control plane. AIBrix already has Kubernetes, batch APIs and serving abstractions. A second SkyPilot-like API would dilute the product.

## Reuse Across VK, Batch And Online Serving

This mechanism can be reused if we keep one invariant: the shared resource manager cannot import Kubernetes, Virtual Kubelet or AIBrix batch internals. It should accept typed intent and return typed decisions, observations and attempts.

### GPU Burst Node / VK

VK should use the mechanism narrowly:

- transform a Pod into `ResourceIntent`.
- call planner/acquire.
- persist remote id, placement, attempts, conditions and cost into `CloudPodBinding`.
- project provider state back into Pod status.

VK should not own wait queues, checkpoint migration or serving traffic policy. It can consume those facts only when annotations or owner workload metadata make them explicit.

### AIBrix Batch

Batch is the best home for advanced SkyNomad behavior. It knows deadline, retry budget, checkpoint path, remaining runtime and lineage. Batch can reuse the same catalog/planner/provider adapters, but it can also choose to wait, retry later, migrate from checkpoint, or switch to on-demand before deadline. References: [SkyNomad](https://arxiv.org/abs/2601.06520).

### AIBrix Online Serving

Serving can reuse catalog, cost, provider adapters and error classification, but it needs different policy:

- maintain enough replicas across failure domains.
- treat cold start as SLO risk, not just launch penalty.
- use spot only when redundancy and traffic shifting make interruption acceptable.
- optimize fleet burn rate, not one job's total cost.
- drain or replace replicas before deleting provider resources.

So the reusable unit is not "fallback implementation" alone. It is the portfolio planner plus observation loop. Batch and serving should plug in different objective functions and constraints.

## What Is Still Missing For SkyPilot-Level Capability

1. Provider catalog breadth: RunPod/Vast/VM inventory and price sources need first-class adapters, not only static bootstrap rows.
2. Capacity probes: cheap provider-specific checks before create, with TTL and rate limits.
3. Provider scorecard: launch success, capacity failure, time-to-running, preemption rate, delete success and billing freshness.
4. Account and budget model: provider credentials, quota, namespace budget and max total cost.
5. Batch recovery policy: checkpoint-aware retry, wait-versus-launch, max restarts and deadline safety-net.
6. Serving policy: replica anti-affinity across providers/regions, spot hedge, cold-start SLO and traffic-aware replacement.
7. Stable library API: move shared mechanism into a package that both VK and AIBrix resource manager can import without product dependencies.

The current code is now a credible base for this split: it has typed offers, planner, fallback candidates, classified attempts, observed catalog, Lambda native catalog, provider adapters and persisted cost estimate. The remaining gap is less about "can we launch a GPU" and more about the quality of the market model we build from real catalog, price, capacity and failure data.
