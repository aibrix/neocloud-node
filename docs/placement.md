# Placement Mechanism

Date: 2026-06-06

The Phase 5 placement planner is a provider-neutral mechanism. It does not replace the Kubernetes default scheduler. Kubernetes still schedules the Pod to a GPU Burst Node virtual node; the controller then chooses the external cloud offer.

## Inputs

- `WorkloadRequest.GPUCount`
- `WorkloadRequest.GPUType`
- `WorkloadRequest.MaxPrice`
- `WorkloadRequest.MarketType`
- `WorkloadRequest.GPUMemory`
- remaining runtime and deadline when the caller can estimate them
- provider offers
- provider error classes from prior attempts, converted into temporary offer blocks by `resource.BlockedOffersFromAttempts`
- optional provider order
- optional region order
- cold start, expected spot lifetime and migration cost when known

Provider offers come from the `resource.OfferCatalog` interface. Provider-specific code implements `ProviderOfferSource`; `CompositeCatalog` merges multiple sources and preserves per-provider catalog errors. This keeps catalog refresh, capacity probing and price collection separate from the risk/cost planner.

For providers without a first-class catalog API, such as RunPod Pods, the code uses `resource.ObservedCatalog` as the normalized input surface. Static bootstrap rows, provider list responses, capacity probes, billing observations and failure feedback can all be recorded as `OfferObservation` entries with a TTL. The planner only consumes fresh normalized offers; it does not need to know whether the signal came from a catalog API or from operational observations.

Bootstrap rows can be supplied to the binary through `--placement-offers` or `GPU_BURST_PLACEMENT_OFFERS`:

```text
provider=runpod,location=US-TX-3,gpu=A40,count=1,market=on-demand,price=0.74;provider=lambda,location=us-west-1,gpu=A10,count=1,market=on-demand,price=1.10,capabilities=cloud-init|public-ip
```

`--placement-offer-ttl` controls how long those rows stay fresh, and `--placement-signal-cooldown` controls how long recent capacity/rate-limit/transient failures block a provider/location candidate.

When `--placement-refresh-interval` is greater than zero, GPU Burst Node also refreshes provider-observed resources into the same catalog. Adapters that implement `core.RemoteResourceLister` are wrapped by `resource.RemoteResourceOfferSource`; their `RemoteResource.Request` shape, region, market and cost become normalized offers with the configured TTL. This is intentionally weaker than a full provider catalog API, but it gives RunPod/Lambda-style integrations a clean path to accumulate capacity, price and failure signals without hard-coding provider logic inside the lifecycle service.

If both `--placement-offers` and `--placement-refresh-interval` are empty or zero, the placement planner stays disabled and lifecycle creation uses the direct primary/fallback provider path.

The planner can now drive lifecycle creation through a narrow hook:

- `core.CreateCandidatePlanner` is the provider-neutral lifecycle interface.
- `resource.LifecycleCandidatePlanner` adapts `OfferCatalog`, `Planner`, `Policy` and recent `core.Attempt` signals into ordered `core.CreateCandidate` values.
- `vk.Config.CandidatePlanner` injects that planner into Virtual Kubelet without making `pkg/core` depend on `pkg/resource`.

This is the boundary that lets GPU Burst Node and AIBrix batch share resource strategy while keeping provider launch/delete logic in adapters.

## Selection Order

The planner first rejects offers that cannot satisfy the request:

1. Recently blocked provider/location signal from prior fallback attempts.
2. GPU type mismatch.
3. GPU memory mismatch.
4. Insufficient GPU count.
5. Market mismatch.
6. Spot without recovery/checkpoint support.
7. Offer unavailable.
8. Price above max price.
9. Unsupported provider capability.
10. Deadline cannot be met.
11. Deadline slack requires on-demand safety net.
12. Spot expected lifetime is too short to amortize cold start.

It then sorts matching offers by:

1. Provider order.
2. Region order.
3. Lower risk-adjusted hourly cost.

This keeps the first implementation deterministic and explainable. The risk-adjusted cost currently includes hourly price, startup penalty, provider failure penalty, interruption penalty and migration cost amortized over remaining runtime.

The deadline/cold-start/migration inputs are inspired by SkyNomad, but the planner remains a pure mechanism. It does not probe providers, predict lifetimes or pause workloads by itself. See [Spot Market Strategy](spot-market-strategy.md).

## Rejected Reasons

| Reason | Meaning |
| --- | --- |
| `GPUTypeMismatch` | Offer GPU type does not match the workload request. |
| `InsufficientGPUCount` | Offer has fewer GPUs than requested. |
| `Unavailable` | Provider/catalog reports the offer as unavailable. |
| `PriceExceeded` | Offer price is above the workload max price. |
| `UnsupportedCapability` | Offer/provider cannot satisfy required ports, registry auth, storage or runner capability. |
| `RecentlyBlocked` | Offer is temporarily blocked by recent capacity/rate-limit failure. |
| `SpotRequiresRecovery` | Spot was requested but the workload does not declare restartability or checkpointing. |
| `DeadlineCannotMeet` | Offer cannot finish remaining runtime before deadline after cold start. |
| `DeadlineRequiresOnDemand` | Deadline slack is below fallback buffer, so spot is no longer admitted. |
| `SpotLifetimeTooShort` | Expected spot lifetime is not long enough to amortize cold start. |

## Outputs

`resource.Engine.Select` returns a `Selection` containing:

- `Decision.Selected`: the chosen offer.
- `Decision.Fallback`: other matching offers in failover order.
- `Decision.Rejected`: rejected offers and reasons.
- `CatalogErrors`: provider catalog failures that occurred while collecting offers.

If one provider catalog fails but another provider returns usable offers, selection can continue and keep the catalog error for Kubernetes Events, metrics and `CloudPodBinding.status.attempts`. If every catalog source fails, `CompositeCatalog` returns structured `CatalogErrors`.

If no offer matches, `Planner.Plan` returns `UnavailableError` with the rejected candidates. The controller should surface these reasons through Kubernetes Events and binding status.

Every provider create/get/delete failure must be reduced to a stable error class before placement or fallback consumes it. Raw provider messages are diagnostic details, not policy inputs.

`Policy.WithFailureSignals` is the bridge from durable execution lineage to placement policy. It takes recent `core.Attempt` entries and appends provider/location-scoped `BlockedOffer` entries for fallback-eligible classes: `Capacity`, `RateLimited`, `Transient` and `Timeout`. `Auth`, `UnsupportedSpec`, expired attempts and incomplete attempts are ignored so they do not poison capacity decisions.

## Non-Goals

- No custom Kubernetes scheduler.
- No Kueue integration.
- No CRD policy API.
- No dynamic provider catalog refresh/probe loop in the current code.
- No historical provider scorecard in the current code.
- No cross-workload retry/backoff cache in the current code.
- No survival-analysis lifetime predictor in the current code.

Those are later layers. Phase 5 only establishes the clean selection mechanism.
