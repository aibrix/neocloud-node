# Provider credential rotation

Helm keeps the existing `credentials.<provider>.secretName` and `key` structure.
Each provider has one reference, and all references use the release namespace.
The chart creates no provider Secrets and sends only names/keys to the controller.
RunPod, Lambda and Vast use the same polling mechanism.

```yaml
credentials:
  runpod:
    secretName: neocloud-runpod
    key: apiKey
  lambda:
    secretName: neocloud-lambda
    key: apiKey
  vast:
    secretName: ''
    key: apiKey
credentialPolling:
  interval: 15s
  requestTimeout: 5s
```

The controller immediately reads each configured Secret at startup, then polls
every 15 seconds by default. Each distinct namespace/name is read once per cycle,
even if several providers use different keys in the same Secret. Separate
controller releases poll independently. An empty name is skipped; a configured
but absent Secret continues to be checked. Reference name/key changes require
a Helm upgrade; Secret content changes do not require a rollout.

With a healthy API, propagation takes at most a poll interval plus one cycle's
GET time. Reads are sequential, with a 5 second timeout per distinct Secret.
API outages have no fixed recovery bound. A failed read blocks new provider
operations for that reference immediately; the next successful poll recovers.
The controller remains healthy. No unavailable/empty credential sends an
unauthenticated request or falls back to a stale environment value.

The primary provider's GPU capacity and allocatable become zero while its
credentials are unavailable and return to the configured advertised count when
restored. `ProviderCredentialsReady` reports the availability separately from
`Ready`, so credential loss does not mark the controller unhealthy. This does
not introduce SKU/stock-driven nodes. Nonempty credentials are initially treated
as configured; provider authentication failures disable new allocations until
credentials change. Cached placement offers are filtered while unavailable.

Each complete operation pins one immutable client, including create recovery and
paginated requests. Rotation affects subsequent operations, not requests already
in flight. Old authentication failures cannot disable a new credential version.
Log sessions keep their original connection until closed. Read errors are
sanitized; upstream provider error bodies are not written into binding status.

Bindings and remote IDs survive missing credentials, failed status reads and
failed deletes. Create attempts look up the original provider external name
before allocating, recovering resources whose create response was lost. This
retains workload UID identity across rotation and controller restarts.

## Account boundary and cleanup

Only key rotation within the same provider account is supported. Do not replace
a Secret with credentials for another account while bindings or unconfirmed
create attempts exist. Account migration is not automatic.

A first 404 for an existing remote ID with a newly loaded credential is ambiguous:
the resource could be gone, or the key could belong to the wrong account. The
controller retains the binding and reports `CredentialScopeUnverified` instead
of declaring the resource deleted. DELETE verifies visibility first because
provider APIs commonly treat a missing resource as a successful delete.

This also means an allocation already deleted externally before its first
successful GET in the current controller/credential lifetime requires operator
verification and explicit binding cleanup. Do not remove bindings until the old
account has been checked for remaining paid resources. This conservative behavior
also applies after controller restarts; no credential/account proof is persisted.

## Standalone configuration

Without a Secret reference, existing static API key flags/environment variables
retain their behavior and require a restart to change. To use polling directly:

```bash
neocloud-node --provider=runpod \
  --provider-secret-namespace=aibrix-system \
  --runpod-api-key-secret-name=neocloud-runpod \
  --runpod-api-key-secret-key=apiKey \
  --provider-secret-poll-interval=15s \
  --provider-secret-request-timeout=5s
```

Environment equivalents: `GPU_BURST_PROVIDER_SECRET_NAMESPACE`,
`GPU_BURST_PROVIDER_SECRET_POLL_INTERVAL`,
`GPU_BURST_PROVIDER_SECRET_REQUEST_TIMEOUT`, and
`RUNPOD_API_KEY_SECRET_NAME` / `RUNPOD_API_KEY_SECRET_KEY` (or `LAMBDA` / `VAST`).
An explicit reference takes precedence over static credentials, including when
the Secret/key is missing. Reference configuration requires a namespace and key.

The polling path only needs `get` on the named Secrets in their namespace.
Existing VK/workload environment resolution RBAC already grants broader Secret
reads; adding a narrow Role would not restrict those existing permissions.

## Release validation

After merge and main CI image publication, upgrade one shared release to the
published commit image. Verify missing → created → rotated → emptied/deleted →
recreated Secret transitions without a Deployment restart, node capacity changes,
and successful cleanup of a budget-capped GPU workload. Keep the user-owned
Secret and shared controller after cleanup. Unit/HTTP tests do not establish
live provider behavior; this paid smoke is a separate gated release check.
