# Placement Mechanism

日期：2026-06-05

The Phase 5 placement planner is a provider-neutral mechanism. It does not replace the Kubernetes default scheduler. Kubernetes still schedules the Pod to a GPU Burst Node virtual node; the controller then chooses the external cloud offer.

## Inputs

- `WorkloadRequest.GPUCount`
- `WorkloadRequest.GPUType`
- `WorkloadRequest.MaxPrice`
- provider offers
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

## Rejected Reasons

| Reason | Meaning |
| --- | --- |
| `GPUTypeMismatch` | Offer GPU type does not match the workload request. |
| `InsufficientGPUCount` | Offer has fewer GPUs than requested. |
| `Unavailable` | Provider/catalog reports the offer as unavailable. |
| `PriceExceeded` | Offer price is above the workload max price. |

## Outputs

`PlacementDecision` contains:

- `Selected`: the chosen offer.
- `Rejected`: rejected offers and reasons.

If no offer matches, `SelectOffer` returns `PlacementUnavailableError` with the rejected candidates. The controller should surface these reasons through Kubernetes Events and binding status.

## Non-Goals

- No custom Kubernetes scheduler.
- No Kueue integration.
- No CRD policy API.
- No provider catalog refresh.
- No historical provider scorecard.
- No automatic retry/backoff cache.

Those are later layers. Phase 5 only establishes the clean selection mechanism.

