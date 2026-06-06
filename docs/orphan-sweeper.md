# Orphan Sweeper

Date: 2026-06-06

The orphan sweeper is a library-level safety mechanism. It compares durable `CloudPodBinding` state with provider-visible remote resources and deletes only resources that are both:

- recognizable as GPU Burst Node managed resources, and
- not referenced by any active binding.

## Current Code

- `core.BindingStore.List(ctx)` lists durable bindings.
- Kubernetes binding store lists `CloudPodBinding` across namespaces.
- `core.OrphanSweeper` compares binding `RemoteID` values with provider `ListRemoteResources`.
- RunPod and Lambda adapters expose `ListRemoteResources`.
- Dry-run mode reports orphans without deleting.
- The running GPU Burst Node process can start a periodic sweeper with `--orphan-sweeper-interval`.

## Ownership Rule

A remote resource is managed only if:

- `RemoteResource.IdempotencyKey` is set, or
- `RemoteResource.ExternalName` starts with `gpu-burst-node-`.

Manual user resources in the same provider account are ignored.

## Runtime Controls

The production default is conservative:

- `--orphan-sweeper-interval=0` disables the loop.
- `--orphan-sweeper-dry-run=true` reports without deleting.

Set `GPU_BURST_ORPHAN_SWEEPER_INTERVAL` to a Go duration such as `10m` to enable periodic scans. Keep dry-run enabled until logs and metrics show that the ownership filter matches only GPU Burst Node resources. After that, set `GPU_BURST_ORPHAN_SWEEPER_DRY_RUN=false` to allow cleanup.

The app fails during startup if the sweeper is enabled for a provider that does not implement remote list/delete. This prevents a false sense of cleanup coverage.

## Not Yet Wired

- No metrics yet.
- No namespace/provider policy for dry-run vs delete mode yet.
- No event emission for orphan cleanup yet.

The intended production default is dry-run first, then opt-in delete after metrics and logs prove the ownership filter.
