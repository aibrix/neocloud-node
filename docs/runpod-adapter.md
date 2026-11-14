# RunPod Adapter

The RunPod adapter uses REST v2 at `https://api.runpod.io/v2`. Its contract tests
use local `httptest` servers; the optional smoke test creates a paid GPU Pod.

API references:

- [Migration guide](https://docs.runpod.io/api-reference-v2/migrate-from-v1)
- [Official OpenAPI schema](https://api.runpod.io/v2/openapi.json), specifically
  `CreatePodRequest`, `BaseContainerConfig`, `Pod`, and `ListPodsResponse`.

Set `GPU_BURST_RUNPOD_BASE_URL` only when overriding the API endpoint. Remove old
`/v1` overrides. The separate `Config.V2BaseURL` runtime reader has been removed;
`Config.BaseURL` now serves every operation.

## Request mapping

- The idempotency key becomes a sanitized `name` with the `neocloud-node-` prefix.
- `Image` maps to `image`; environment variables map to `env`.
- `GPUType` and `GPUCount` map to `gpu.id` and `gpu.count`. An explicit single GPU
  type is required. Missing types and comma-separated fallback lists are rejected.
- Ports map to strings such as `9090/tcp`. No implicit SSH or HTTP port is added.
- `runpod-cloud-type` maps to `cloud` (`SECURE` or `COMMUNITY`). Omission uses
  RunPod's `SECURE` default.
- `runpod-template-id` maps to `templateId`. RunPod copies template settings at
  creation; later template changes do not update the Pod.
- `runpod-datacenter-ids` maps to `dataCenterIds`. These constrain placement;
  the adapter does not promise that their order controls scheduling.
- `runpod-registry-auth-id` maps to `registry`, a credential ID string in the
  current schema. Kubernetes `imagePullSecrets` are not converted automatically.
- `runpod-network-volume-id` maps to `mounts.network[0].volumeId`;
  `runpod-volume-mount-path` supplies its `path`, defaulting to `/workspace`.

All annotation names above have the `cloud.aibrix.ai/` prefix.

`market-type: spot` and `runpod-datacenter-priority` have no v2 equivalent and
return `UnsupportedSpecError` before any HTTP request. They are not silently
converted to on-demand requests or unordered placement.

Without a template, the adapter sends `disk: 50`, preserving the
[documented v1 container-disk default](https://rest.runpod.io/v1/openapi.json).
Although the v2 schema leaves `disk` optional, live template-free creation was
rejected without it. Templates retain their own container-disk settings.

Without a template or network volume, the adapter explicitly requests a 20 GB
persistent mount at `/workspace` (or the annotated mount path), preserving v1's
storage default. v2 otherwise creates no persistent volume. A template supplies
its own storage unless a network volume is explicitly requested. A mount-path
annotation with a template but without a network volume is rejected: v2 requires
both size and path for a persistent mount, and the adapter cannot infer the
intended template volume size.

## Command and args

The adapter uses exec-form arrays, avoiding shell quoting or string joining:

- Neither set: omit `entrypoint` and `cmd`, preserving image/template defaults.
- Only args set: send `cmd`, preserving the image ENTRYPOINT.
- Both set: send `entrypoint` and `cmd` unchanged.
- Only command set: send the entire command as `entrypoint` and `cmd: []`.

The v2 schema explicitly supports an empty array to clear CMD. This replaces the
v1 workaround that split the command and could append an unwanted image CMD.

## Lifecycle and recovery

`POST /pods` returns 201; provisioning is asynchronous. Status mappings are:

- `PROVISIONING`, `STARTING`, unknown or empty: Pending.
- `RUNNING`: Running.
- `ERROR`: Failed with reason `RemoteError`.
- `EXITED`: Failed with reason `RemoteExited`; an exit code is unavailable.
- `TERMINATED`: Deleted.

`EXITED` is not evidence of successful completion. The stopped Pod can still
retain billable storage until deletion.

`GET /pods` returns a `pods` array with cursor pagination. The adapter follows
`pagination.nextCursor` while `hasNextPage` is true, then filters names locally.
No v1 name or include query parameters are sent. Missing pagination or repeated
cursors return an error, never a partial successful listing. This also protects
the orphan sweeper from interpreting an incomplete response as a complete list.

After an uncertain create response, recovery looks up the exact external name
across all pages. A terminated Pod is not adopted; multiple matching live Pods
are reported as a conflict rather than selecting one arbitrarily. Name lookup
is recovery logic, not a provider-side atomic idempotency guarantee.

`Stop` sends `POST /pods/{id}/action` with `{"action":"stop"}`. It releases compute
and keeps storage. `Delete` sends `DELETE /pods/{id}`; both 204 and 404 are
successful cleanup. After a transient deletion error, a GET returning 404 also
confirms cleanup. Deleting a Pod destroys its host-local persistent disk but only
detaches existing network volumes; those volumes are not deleted by the adapter.

## Runtime and endpoints

`Get` reads `runtime.uptime` from the same v2 response while status is `RUNNING`.
A null runtime or nonpositive uptime produces no restart observation. Failed GETs
are returned as errors rather than hidden by a second, optional runtime request.
List responses have null runtime; they cannot supply restart or endpoint data.

The lifecycle compares successive uptime observations to detect containers
restarted in place. See [restart semantics](compatibility-matrix.md#restart-semantics).

Public TCP entries in `runtime.ports` map `private` to the container port and
`ip`/`public` to the reachable address. Entries without an IP or public port,
invalid ports, and non-TCP entries are not exposed as TCP endpoints.

`cost` supplies the hourly compute rate and source `runpod.cost`. This is a
quoted rate, not accumulated billing or proof that a stopped Pod is billing
compute. Disk charges are separate.

## Catalog Semantics

The adapter implements `resource.ProviderOfferSource` using
`GET /catalog/gpus?include=AVAILABILITY&product=POD`. It makes one request for
each cloud tier, `SECURE` and `COMMUNITY`, with `count` set to the requested GPU
count (one when no intent is supplied). Availability is specific to that tier
and count; it is not inferred from the GPU's presence in the catalog.

Each supported GPU type, tier, and returned datacenter becomes an on-demand
offer. `GPUType` is the exact RunPod GPU ID used by Pod creation, not its display
name. The tier's per-GPU USD hourly list price is multiplied by the GPU count;
storage charges and account discounts are not included. Missing, null, zero,
or negative tier prices are skipped. Requests beyond the tier's `maxCount`
ceiling are skipped; that ceiling is not a measure of free stock.

Both overall and datacenter stock must be `LOW`, `MEDIUM`, or `HIGH` for an offer
to be available. `NONE`, missing, and unknown stock values produce unavailable
offers. Missing datacenters produce no offers; no location is invented.
Live API reads on 2026-11-14 returned Community GPUs with overall `LOW` stock
but no `dataCenters` entries. Those GPUs therefore produce no placement offers,
even though RunPod reports stock at the tier level.
Offer IDs include GPU ID, tier, datacenter, and count, and remain stable when
price or availability changes. Provider hints carry `runpod-cloud-type` and
`runpod-datacenter-ids` through placement into the create request. Offers support
ports and registry credentials. Only Secure Cloud offers advertise network
volumes, following RunPod's [network volume restriction](https://docs.runpod.io/storage/network-volumes#network-volumes-for-pods).
Spot offers are never emitted.

HTTP failures use the existing provider error classes. A missing/null `gpus`
array or invalid JSON is a transient response error; `gpus: []` is a valid empty
catalog. If either tier request fails, no partial catalog is returned.

The app selects the catalog source instead of deriving additional offers from
existing RunPod Pods. Existing allocations do not establish stock for a new Pod.
Periodic refresh uses an empty intent and therefore records single-GPU offers,
as with Lambda. Multi-GPU placement needs an explicit intent-based catalog query
or suitable bootstrap offers. The observed catalog owns TTL and refresh timing;
stock can change between a snapshot and creation, so a quote does not guarantee
capacity. Entries omitted by a later refresh remain until their existing TTL
expires.

## Errors

RFC 9457 `title`, `detail`, and validation `errors` are included in provider
errors. HTTP status determines authentication (401/403), not found (404),
conflict (409), rate limiting (429), insufficient balance (402), schema or body
size rejection (422/413), and transient failures (5xx).

RunPod documents no machine-readable capacity code. For other errors, capacity
and quota messages are classified from their text; an ordinary 400 is an
unsupported request. A 422 remains a schema rejection even when its validation
message mentions capacity, and a 5xx remains transient.

## Validation and limitations

Run `go test ./pkg/providers/runpod` for contract tests and
`python3 hack/e2e/runpod-smoke_test.py` for the cleanup-helper tests. The gated
`hack/e2e/runpod-smoke.sh` provisions one A40 by default, verifies status and
confirmed deletion, and checks all list pages for matching leftovers. See the
[development guide](development.md#runpod) for options. Cleanup lookup failures
fail the smoke test; they do not count as an empty account.

On 2026-11-14, a live adapter check queried one- and two-GPU offers, passed a
single-GPU offer through `LifecycleCandidatePlanner`, and created a Secure Cloud
RTX 2000 Ada in `EU-RO-1`. The Pod reported Running at the catalog's $0.24/hour
compute rate. Deletion was independently verified by ID (404) and a complete
name-filtered listing. This checked the Go adapter and placement request mapping;
it did not deploy a new controller or test inference or GPU performance.

Logs and billing-history polling are outside the adapter's scope. Contract tests
cover catalog mapping and lifecycle behavior; use the gated smoke test to
validate lifecycle changes against live RunPod. Catalog tests do not establish
live capacity or the ability to create a Pod at a quoted price.
