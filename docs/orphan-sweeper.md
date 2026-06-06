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

## Ownership Rule

A remote resource is managed only if:

- `RemoteResource.IdempotencyKey` is set, or
- `RemoteResource.ExternalName` starts with `gpu-burst-node-`.

Manual user resources in the same provider account are ignored.

## Not Yet Wired

- No periodic sweeper loop in the running VK process yet.
- No metrics yet.
- No namespace/provider policy for dry-run vs delete mode yet.
- No event emission for orphan cleanup yet.

The intended production default is dry-run first, then opt-in delete after metrics and logs prove the ownership filter.

