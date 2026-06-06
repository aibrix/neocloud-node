# Placement Mechanism

Date: 2026-06-06

The Phase 5 placement planner is a provider-neutral mechanism. It does not replace the Kubernetes default scheduler. Kubernetes still schedules the Pod to a GPU Burst Node virtual node; the controller then chooses the external cloud offer.

## Inputs

- `WorkloadRequest.GPUCount`
- `WorkloadRequest.GPUType`
- `WorkloadRequest.MaxPrice`
- `WorkloadRequest.MarketType`
- `WorkloadRequest.GPUMemory`
- provider offers
- provider error classes from prior attempts
- optional provider order
- optional region order

## Selection Order

The planner first rejects offers that cannot satisfy the request:

1. GPU type mismatch.
2. Insufficient GPU count.
3. Offer unavailable.
4. Price above `WorkloadRequest.MaxPrice`.

It then sorts matching offers by:

1. Provider order.
2. Region order.
3. Lower hourly price.

This keeps the first implementation deterministic and explainable.

The next implementation must rank by risk-adjusted cost rather than raw hourly price. See [Spot Market Strategy](spot-market-strategy.md).

## Rejected Reasons

| Reason | Meaning |
| --- | --- |
| `GPUTypeMismatch` | Offer GPU type does not match the workload request. |
| `InsufficientGPUCount` | Offer has fewer GPUs than requested. |
| `Unavailable` | Provider/catalog reports the offer as unavailable. |
| `PriceExceeded` | Offer price is above the workload max price. |
| `UnsupportedCapability` | Offer/provider cannot satisfy required ports, registry auth, storage or runner capability. |
| `RecentlyBlocked` | Offer is temporarily blocked by recent capacity/rate-limit failure. |

## Outputs

`PlacementDecision` contains:

- `Selected`: the chosen offer.
- `Rejected`: rejected offers and reasons.

If no offer matches, `SelectOffer` returns `PlacementUnavailableError` with the rejected candidates. The controller should surface these reasons through Kubernetes Events and binding status.

Every provider create/get/delete failure must be reduced to a stable error class before placement or fallback consumes it. Raw provider messages are diagnostic details, not policy inputs.

## Non-Goals

- No custom Kubernetes scheduler.
- No Kueue integration.
- No CRD policy API.
- No provider catalog refresh in the current code.
- No historical provider scorecard in the current code.
- No automatic retry/backoff cache in the current code.

Those are later layers. Phase 5 only establishes the clean selection mechanism.
