# Shared Resource Manager Architecture

Date: 2026-06-06

This document defines how GPU Burst Node should remain a library-friendly system on top of Virtual Kubelet while sharing provider strategy with AIBrix batch.

## Architecture Decision

GPU Burst Node should not become a monolithic controller that owns all cloud business logic. The clean split is:

- VK layer: Kubernetes node/pod lifecycle, PodStatus projection, `CloudPodBinding` persistence.
- Resource manager library: provider-neutral placement, fallback, cost and recovery strategy.
- Provider adapters: RunPod, Lambda, Vast, cloud VM, Kubernetes.
- Product callers: GPU Burst Node and AIBrix batch use the same resource manager through different persistence backends.

The key boundary is that the resource manager never imports Virtual Kubelet or Kubernetes clients. It consumes typed intent and emits typed decisions/events.

## Why This Matches AIBrix

The AIBrix batch refactor already moved toward a runtime seam:

- a single job driver owns the lifecycle template.
- `Runtime` is the "where compute lives" axis.
- runtime implementations include Kubernetes, Kubernetes Job, Lambda Cloud, RunPod, External and noop.
- persistence is owned by `JobStore`, not by the runtime itself.

That is the same shape we need here. GPU Burst Node's `ProviderAdapter` is the VK-facing lifecycle adapter; AIBrix batch's `Runtime` is the batch-facing lifecycle adapter. Both should call a shared resource manager before provisioning.

## Layers

```text
Kubernetes Pod             AIBrix BatchJob
      |                         |
      v                         v
Pod Transformer           Batch Runtime Builder
      |                         |
      +----------+--------------+
                 v
          WorkloadIntent
                 |
                 v
       Shared Resource Manager
       - placement planner
       - catalog/cache
       - price/burn-rate
       - fallback policy
       - failure classifier
                 |
                 v
          Provider Adapter
       RunPod / Lambda / Vast / VM
                 |
                 v
       Provider-native resource
```

## Shared Types

### WorkloadIntent

The common intent should be provider-neutral:

- container image, command, args, env.
- GPU count, type, memory.
- CPU, memory, disk when available.
- ports and network exposure.
- private registry requirement.
- persistent storage requirement.
- market type: on-demand, spot, either.
- cost constraints.
- recovery/checkpointability.
- placement constraints.

Kubernetes annotations and AIBrix batch specs are just two frontends for this intent.

### Allocation

Provider adapters return allocation state:

- provider and location.
- remote resource id.
- external name/idempotency key.
- phase.
- endpoint/ports/public IP when available.
- current cost and billing source when available.
- provider-native diagnostic fields.

### Attempt

Attempts are durable and product-owned:

- provider, location and offer id.
- operation: create, get, delete, recover, sweep.
- error class and provider message.
- retryable/fallback flags.
- selected fallback if any.
- timestamp.

GPU Burst Node persists this in `CloudPodBinding.status.attempts`. AIBrix batch persists the equivalent in `JobStore`.

## Interfaces

```go
type ResourceManager interface {
    Plan(ctx context.Context, intent WorkloadIntent, policy PlacementPolicy) (Plan, error)
    Acquire(ctx context.Context, plan Plan) (Allocation, error)
    Observe(ctx context.Context, allocation Allocation) (Observation, error)
    Release(ctx context.Context, allocation Allocation) error
}

type ProviderAdapter interface {
    Catalog(ctx context.Context) ([]MarketOffer, error)
    Create(ctx context.Context, request ProviderCreateRequest) (Allocation, error)
    Get(ctx context.Context, id string) (Allocation, error)
    List(ctx context.Context, filter ListFilter) ([]Allocation, error)
    Delete(ctx context.Context, id string) error
    Classify(error) ProviderFailure
}

type PersistenceSink interface {
    RecordDecision(ctx context.Context, decision PlacementDecision) error
    RecordAttempt(ctx context.Context, attempt Attempt) error
    RecordCondition(ctx context.Context, condition Condition) error
}
```

The `PersistenceSink` is deliberately caller-owned. This prevents the shared library from choosing Kubernetes CRDs or a database.

## VK Integration

GPU Burst Node maps:

- `CreatePod` -> transform Pod -> plan -> acquire -> write `CloudPodBinding` -> project PodStatus.
- `GetPodStatus` -> observe allocation -> update `CloudPodBinding` -> project PodStatus.
- `DeletePod` -> release allocation -> finalizer cleanup.
- orphan sweeper -> provider list -> compare against `CloudPodBinding` ownership -> delete or report.

The VK provider should stay thin. It should not implement pricing, spot policy or cross-provider fallback directly.

## AIBrix Batch Integration

AIBrix batch maps:

- runtime builder -> transform BatchJob compute spec -> plan -> runtime-specific acquire.
- `Runtime._provision` -> call shared resource manager acquire.
- `Runtime._wait_ready` -> observe allocation or runner callback.
- `Runtime._teardown` -> release allocation.
- `JobStore` -> persistence sink for decisions, attempts and costs.

This lets batch use the same RunPod/Lambda/Vast strategy without depending on Kubernetes virtual-node semantics.

## Provider Adapter Responsibilities

Adapters own:

- provider API authentication.
- request mapping to native fields.
- create/get/list/delete.
- external name/idempotency handling.
- native status mapping.
- native pricing/billing extraction.
- native error classification.

Adapters do not own:

- global fallback policy.
- cross-provider scoring.
- budget policy.
- Kubernetes conditions/events.
- AIBrix job state transitions.

## MVP Boundary

Implement now:

- provider error classes.
- durable attempts and conditions.
- RunPod advanced create/list/status fields.
- Lambda API adapter skeleton with launch/get/list/terminate.
- `pkg/resource` package with pure planner types and deterministic tests.
- spot strategy document and metrics names.

Defer:

- managed job queue.
- custom Kubernetes scheduler.
- Kueue integration.
- user-facing policy CRDs.
- real e2e with API keys until integration setup is ready.

## Correctness Criteria

- The same `WorkloadIntent` can be constructed from a Kubernetes Pod and from an AIBrix BatchJob.
- Planner tests are deterministic and do not require provider credentials.
- Provider adapter tests use fake HTTP servers for API contracts.
- Every provider error maps to a stable class.
- Every fallback attempt is durable before the next provider create is attempted.
- No provider adapter imports Kubernetes packages.
- No VK provider imports AIBrix batch packages.
