# Launch Guard

日期：2026-06-05

The launch guard is a local safety gate that runs before `LifecycleService` calls a cloud provider. Its purpose is to prevent obvious unsafe remote GPU launches before any provider API call can create a paid resource.

## Policy Fields

| Field | Behavior | Violation code |
| --- | --- | --- |
| `RequireMaxPrice` | Requires `WorkloadRequest.MaxPrice` to be non-empty. | `MissingMaxPrice` |
| `MaxGPUCount` | Rejects requests whose `GPUCount` is greater than the configured maximum. A value of `0` disables the check. | `GPUCountExceeded` |
| `MaxPorts` | Rejects requests exposing more than the configured number of ports. A value of `0` disables the check. | `PortCountExceeded` |

## Lifecycle Behavior

When the guard rejects a workload:

- `LifecycleService.EnsureCreated` returns `PolicyViolationError`.
- The binding phase is marked `Failed`.
- The binding `LastError` records the violation.
- `ProviderAdapter.Create` is not called.

This preserves the core invariant that validation and spend controls run before remote paid resources are created.

## Non-Goals

The launch guard is not:

- Kubernetes admission control.
- Namespace quota.
- Budget accounting.
- A CRD policy system.
- A scheduler replacement.

Those can be added later if real users need them. Phase 3 keeps the mechanism local and provider-neutral.

