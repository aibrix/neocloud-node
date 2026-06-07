# Spot Market Strategy

Date: 2026-06-06

This document defines the spot/interruptible, fallback, cost-estimation and burn-rate strategy for AIBrix GPU Burst Node. It uses SkyPilot as a reference point, but the product shape is intentionally different: SkyPilot is a CLI/API job broker; this project is a Kubernetes-native resource substrate that can also be reused by AIBrix batch.

## Evidence Base

- SkyPilot's NSDI 2023 paper frames the core system as an intercloud broker with a catalog, tracker, optimizer, provisioner and executor. The broker optimizes placement against user requirements, cloud availability and prices. Source: [SkyPilot: An Intercloud Broker for Sky Computing](https://www.usenix.org/system/files/nsdi23-yang-zongheng.pdf).
- SkyPilot managed jobs support retry, failure recovery, spot preemption recovery and cleanup. Source: [SkyPilot Managed Jobs](https://docs.skypilot.co/en/latest/examples/managed-jobs.html).
- SkyPilot managed spot jobs can use spot and optionally fall back to on-demand/reserved resources through candidate resources. Source: [SkyPilot Spot Jobs](https://docs.skypilot.co/en/latest/examples/spot-jobs.html).
- SkyPilot catalogs separate instance inventory, GPU metadata, region/zone, price and spot price; the public catalog repo refreshes several providers periodically. Source: [skypilot-catalog](https://github.com/skypilot-org/skypilot-catalog).
- SkyNomad targets deadline-bound AI batch jobs on multi-region spot instances. It combines lightweight availability probing, spot lifetime prediction, migration cost and deadline pressure, and reports 1.25-3.96x cost savings in real deployments while meeting deadlines. Source: [SkyNomad: On Using Multi-Region Spot Instances to Minimize AI Batch Job Cost](https://arxiv.org/abs/2601.06520).
- RunPod REST Pod creation exposes native fields for `interruptible`, `cloudType`, `gpuTypeIds`, `dataCenterIds`, `templateId`, `networkVolumeId`, `containerRegistryAuthId` and `ports`. Source: [RunPod Create Pod API](https://docs.runpod.io/api-reference/pods/POST/pods).
- Lambda Cloud launch requires `region_name`, `instance_type_name`, `ssh_key_names` and supports cloud-init `user_data`, images, file systems and firewall rulesets. It reports capacity/quota/auth errors and instance states including `preempted`. Source: [Lambda Cloud API](https://docs-api.lambda.ai/api/cloud).

## Product Position

Do not implement "SkyPilot inside Kubernetes". The stronger architecture is:

- Kubernetes remains the control plane and user API for VK-backed workloads.
- AIBrix batch and GPU Burst Node share a provider-neutral resource manager library.
- Provider-specific launch details stay inside adapters.
- Placement decisions are persisted as attempts/conditions, not hidden in logs.
- Cost and risk are first-class status and metrics, not post-hoc billing reports.

The differentiator is not "cheapest GPU search". It is production-grade, Kubernetes-native cloud GPU acquisition with transparent state, fallback lineage, and reusable strategy logic for both online serving and batch.

## SkyNomad Takeaways

SkyNomad is the more relevant paper for our spot policy than generic cloud cost optimization because it models exactly the failure mode we care about: a GPU batch job can wait, migrate, checkpoint, and fall back to on-demand before missing a deadline.

The parts to adopt:

- Treat provider/location/market as the optimization unit. RunPod datacenters, Lambda regions and future cloud VM zones should all be ranked the same way.
- Use deadline slack as an admission signal. When remaining runtime plus cold start leaves too little slack, stop admitting spot and use the cheapest feasible on-demand fallback.
- Account for checkpoint or data migration as real cost. A cheaper remote region is only cheaper after transfer cost is amortized over remaining runtime.
- Avoid volatile spot locations where expected lifetime is too close to cold start time. Short-lived spot capacity can produce negative goodput.
- Maintain region-level freshness signals from probes, list calls, capacity errors and preemptions. Static catalogs are useful but insufficient for spot.

The parts not to copy into MVP:

- Do not put online probing loops inside the hot `CreatePod` path.
- Do not implement survival analysis until we have enough provider observations.
- Do not let VK pause arbitrary Kubernetes Pods waiting for future spot capacity. That belongs in AIBrix batch where deadlines, checkpoints and lineage are explicit.
- Do not assume all workloads can migrate. Standard Pods need explicit recovery hints before spot migration is allowed.

## Core Model

### Workload Intent

`WorkloadIntent` is the shared input from either a Kubernetes Pod or an AIBrix BatchJob:

- `resource_shape`: GPU count, GPU type, GPU memory, CPU, memory, disk.
- `runtime_shape`: container image, command, env, ports, volume needs, registry auth.
- `market`: on-demand, spot, either, or reserved.
- `slo`: deadline, max startup latency, min runtime, serving availability target.
- `recovery`: restartable, checkpointed, stateless, or non-recoverable.
- `budget`: max hourly price, max total cost, namespace/account budget.
- `constraints`: provider allow/deny list, region/datacenter policy, data gravity constraints.
- `progress`: remaining runtime estimate, checkpoint size, current checkpoint location.

### Market Offer

`MarketOffer` is the normalized provider/capacity unit:

- provider: RunPod, Lambda, Vast, Kubernetes, cloud VM.
- location: region, zone, datacenter.
- accelerator: type, memory, count, interconnect if known.
- price: on-demand, spot, adjusted hourly cost, storage add-ons.
- capacity: available, scarce, unknown, blocked, stale.
- risk: interruption class, observed failure rate, startup uncertainty.
- progress cost: expected lifetime, cold start, migration cost from current checkpoint location.
- capabilities: ports, registry auth, network volume, file system, public IP, cloud-init, callback.
- freshness: when the offer was observed and how it was probed.

### Decision

`PlacementDecision` must be explainable:

- selected offer and provider adapter.
- rejected offers with reason and class.
- fallback plan ordered by risk-adjusted score.
- budget projection and burn-rate estimate.
- required cleanup and orphan-sweeper labels.

The decision is durable:

- VK writes it to `CloudPodBinding.status`.
- AIBrix batch writes it to `JobStore`.
- Metrics emit provider, offer, price, risk and fallback attempt labels with bounded cardinality.

## Strategy

### 1. Guarded Portfolio Selection

The first production strategy should not choose the single cheapest offer. It should choose from a bounded portfolio:

- filter unsupported capabilities first.
- reject offers exceeding hard budget or data/location policy.
- rank remaining offers by `risk_adjusted_cost`, not raw hourly price.
- maintain at least one fallback offer when market is spot/either.
- prefer providers with fresh availability signals over stale catalog-only entries.

Formula:

```text
risk_adjusted_cost =
  hourly_price
  + expected_restart_cost
  + expected_startup_delay_cost
  + amortized_migration_cost
  + storage_attach_cost
  + provider_failure_penalty
```

This is intentionally simpler than SkyPilot's full optimizer, but more useful for VK: every term can be explained in Kubernetes status.

### 2. Spot Admission Policy

Spot/interruptible should be admitted only when one of the following is true:

- workload is stateless and can restart from scratch.
- workload declares checkpoint or external durable state.
- workload has no strict deadline and budget policy allows retries.
- policy allows fallback to on-demand when spot capacity/preemption fails.

Default policy:

- batch/offline: spot allowed if `market-type=spot` or namespace default enables it.
- online serving: spot allowed only with more than one replica or an explicit availability policy.
- non-checkpointed long training: require max total cost and retry limit.
- deadline-bound batch: spot allowed while slack exceeds the fallback buffer; otherwise select on-demand.

SkyNomad-style inputs map into current code as:

- `Intent.Deadline` and `Intent.RemainingRun` decide whether spot is still admissible.
- `Policy.DeadlineFallbackBuffer` encodes the safety-net window for fallback.
- `Offer.ColdStart` and `Offer.ExpectedLifetime` reject spot offers that are unlikely to make useful progress.
- `Offer.MigrationCost` is amortized over remaining runtime and added to risk-adjusted cost.

### 3. Fallback

Fallback should consume provider error classes, not raw strings:

| Error class | Retry same offer | Try same provider different location | Try different provider | Notes |
| --- | --- | --- | --- | --- |
| `Capacity` | no | yes | yes | Main path for RunPod/Lambda/Vast fallback. |
| `RateLimited` | yes, backoff | maybe | yes | Keep attempt in lineage. |
| `Transient` | yes, bounded | yes | yes | Includes 5xx and network failures. |
| `Timeout` | yes, bounded | yes | yes | Create response lost must list/find before retrying. |
| `RemoteNotFound` | no | no | maybe | Sync path means remote manually deleted or provider cleanup. |
| `Auth` | no | no | no by default | Avoid leaking workload to another provider with wrong credentials. |
| `Quota` | no | maybe | yes if policy permits | User/account policy dependent. |
| `UnsupportedSpec` | no | no | only if capability matrix says supported | Never silently downgrade. |

Current code hooks:

- `core.LifecycleService` records each provider create failure as a durable `core.Attempt`.
- `core.ProviderError` and `core.ClassifyError` normalize RunPod/Lambda/API failures into stable classes.
- `resource.BlockedOffersFromAttempts` converts recent fallback-eligible attempts into provider/location cooldown blocks.
- `resource.Policy.WithFailureSignals` lets VK, AIBrix batch or a future resource manager merge those cooldowns into a placement policy without depending on provider-specific code.
- `resource.ObservedCatalog` provides the catalog substitute for providers like RunPod where inventory is assembled from static bootstrap rows, list calls, probes, billing and failure observations instead of one official catalog endpoint.

This is intentionally scoped to short-term capacity freshness. Longer-lived provider scoring should be a separate scorecard fed by metrics, not hidden inside adapter code.

### 4. Burn Rate

Burn-rate is a live metric, not just an estimate at launch:

- `estimated_hourly_cost_usd`: selected offer price plus storage/runtime add-ons.
- `current_burn_rate_usd_per_hour`: provider billing status when available, otherwise estimate.
- `projected_total_cost_usd`: burn rate times elapsed plus expected remaining runtime if known.
- `spot_savings_usd`: on-demand baseline minus current projection.
- `fallback_cost_delta_usd`: selected fallback price minus original selected price.

RunPod exposes cost fields in Pod responses and billing history; Lambda exposes instance type price in instance metadata. Use provider-native billing when available and catalog estimate otherwise.

Current code maps:

- RunPod `adjustedCostPerHr` / `costPerHr` into `RemoteResource.HourlyCost`.
- Lambda `instance_type.price_cents_per_hour` into `RemoteResource.HourlyCost`.
- `pkg/resource.EstimateBurnRate` into elapsed spend and projected total.

Prometheus export is still a follow-up.

### 5. Recovery Without Managed Jobs

We do not need a SkyPilot-style managed job feature in MVP. We do need persistent execution lineage:

- every acquire attempt is recorded.
- every provider failure has a class and message.
- every fallback creates a new attempt linked to the prior failure.
- restart attempts are bounded by policy.
- orphan sweeper can distinguish "still owned", "delete pending", and "leaked".

This gives the important part of managed jobs for infrastructure correctness without creating a second batch queue inside GPU Burst Node.

## Interfaces

```go
type MarketCatalog interface {
    ListOffers(ctx context.Context, intent WorkloadIntent) ([]MarketOffer, error)
}

type CapacityProbe interface {
    Probe(ctx context.Context, offer MarketOffer) (CapacitySignal, error)
}

type PriceSource interface {
    Price(ctx context.Context, offer MarketOffer) (PriceQuote, error)
}

type PlacementEngine interface {
    Plan(ctx context.Context, intent WorkloadIntent, policy PlacementPolicy) (PlacementDecision, error)
}

type FailureClassifier interface {
    Classify(error) ProviderFailure
}
```

Provider adapters own launch/delete/list/status. Strategy components own offer scoring and fallback selection. Persistence is injected by the caller.

## Metrics

- `gpu_burst_placement_attempts_total{provider,class,result}`
- `gpu_burst_provider_errors_total{provider,operation,class}`
- `gpu_burst_offer_blocked{provider,location,gpu_type,class}`
- `gpu_burst_estimated_hourly_cost_usd{provider,market}`
- `gpu_burst_current_burn_rate_usd_per_hour{provider,market}`
- `gpu_burst_projected_total_cost_usd{provider,market}`
- `gpu_burst_fallback_attempts_total{from_provider,to_provider,class}`
- `gpu_burst_orphans_found_total{provider}`
- `gpu_burst_orphans_deleted_total{provider}`

## Implementation Order

1. Durable attempts, provider error classes and fallback lineage.
2. Normalized offer model, catalog interface, observed catalog and deterministic planner.
3. RunPod and Lambda cost extraction for launch-time burn-rate estimates.
4. SkyNomad-inspired deadline, cold-start, expected-lifetime and migration-cost planner inputs.
5. Recent attempt to cooldown signal bridge for capacity/rate-limit/transient/timeout failures.
6. Wire planner into VK lifecycle once real provider offer sources are stable.
7. Reuse the same planner from AIBrix batch runtime/resource manager.
8. Add Prometheus burn-rate metrics and orphan sweeper.
9. Add provider probing, scorecards and lifetime prediction after enough observations exist.
10. Keep real provider e2e smoke tests gated by explicit API-key environment variables.
