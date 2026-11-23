# Terminal remote cleanup

Set `--terminal-cleanup-delay=10m` (or `GPU_BURST_TERMINAL_CLEANUP_DELAY=10m`)
to delete the remote resource after its binding has been Succeeded or Failed
for ten minutes. The default is `0`, which disables cleanup. Negative delays
are rejected. With Helm, set `env.GPU_BURST_TERMINAL_CLEANUP_DELAY` to `10m`.
The Kubernetes Pod and binding keep their terminal result.

The delay is also the retention window for remote logs and resource-local files.
Cleanup does not archive them. Copy any required output before expiry. Existing
log streams can end during deletion; new log requests fail once deletion has
been requested. Independently managed network volumes are not deleted by this
feature and may continue to incur charges.

## Scheduling and recovery

The deadline is the persisted `Terminal` condition's `lastTransitionTime` plus
the configured delay. Cleanup is eligible exactly at that deadline. It runs at
startup and on a periodic pass, so deletion can happen later than the deadline.
The pass uses `--status-sync-interval`, or 15 seconds if status sync is disabled.
Each pass processes eligible bindings sequentially and verifies that their Pod
UID and node assignment still match. It does not depend on the in-memory Pod
cache. A missing or unreadable Pod is skipped; normal Pod deletion and orphan
recovery remain separate paths.

Restarting the controller does not reset recorded deadlines. Old terminal
bindings without a `Terminal` timestamp receive a full delay from their first
observation. Creation failures without a remote resource do not need cleanup.
The memory binding store cannot retain bindings across process restarts; use
the default Kubernetes store for restart recovery.

Enabling cleanup or shortening the delay can make many recorded bindings
eligible on the next pass. There is no additional provider rate limiter.
Failed deletions are retried on subsequent passes without moving the deadline.
Increasing the delay does not cancel a deletion already accepted by a provider.

After DELETE succeeds, `RemoteCleanedUp=False` with reason `DeletionRequested`
records that confirmation is pending. Subsequent passes poll for absence or a
provider-reported Deleted state instead of repeatedly issuing DELETE. Once
confirmed, `RemoteCleanedUp=True` is persisted, followed by a Normal
`RemoteCleanedUp` Pod event. Authentication errors, timeouts, and other read
failures do not count as confirmation. The binding's original terminal phase
and failure message are retained.

Deletion is idempotent: a resource already absent satisfies cleanup. A crash
between deletion and persistence can cause a repeated DELETE request. Once
completion is persisted, reconciliation and later Pod deletion skip remote
DELETE. Events are best-effort; a crash after persistence can lose an event.
The persisted condition is the source of truth.

Local lifecycle operations for a binding are serialized to avoid overlapping
cleanup and Pod deletion. Binding updates also reject stale resource versions. Conflicts are retried by the
next reconciliation, rather than writing an obsolete snapshot over newer state.
Late runner callbacks cannot change a terminal result. The supplied deployment
uses one replica and the Recreate strategy; this feature does not add leader
election or support active replicas sharing the same virtual node.

## Live validation

Use a disposable workload on the chosen provider and a short delay, such as
`--terminal-cleanup-delay=2m`. Keep the Pod after completion. Verify:

1. Its binding has the expected terminal result and a `Terminal` timestamp.
2. The remote resource and logs remain accessible before the deadline.
3. Restart the controller before the deadline. The timestamp stays unchanged.
4. After expiry, the provider reports absence or Deleted, the binding records
   `RemoteCleanedUp=True`, and a `RemoteCleanedUp` event appears on the Pod.
5. The Pod still reports its original result. New log requests report cleanup.
6. Delete the Pod and confirm the binding is removed without another remote
   deletion. Check the provider console for leftover billable resources.

Also restart with an already-expired terminal binding, and exercise a controlled
provider API failure to check retries. Fake-provider tests establish controller
behavior; they do not establish provider billing cutoff or live API semantics.
