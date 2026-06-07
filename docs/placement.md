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
- No dynamic provider catalog refresh in the current code.
- No historical provider scorecard in the current code.
- No cross-workload retry/backoff cache in the current code.
- No survival-analysis lifetime predictor in the current code.

Those are later layers. Phase 5 only establishes the clean selection mechanism.
