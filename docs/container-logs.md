# Container logs

RunPod and Lambda have optional container log backends. The kubelet endpoint is
disabled by default. Exec, attach and port-forward remain unsupported.

## Development kubelet endpoint

The controller advertises its Pod IP and port 10250 on the virtual Node. The API
server must be able to reach that IP. Remote GPU addresses are not kubelet addresses.

```yaml
kubeletAPI:
  enabled: true
  authenticationMode: none
```

`none` explicitly enables an unauthenticated endpoint for isolated development.
Restrict access to the control plane using network controls verified for your
cluster. Direct requests to port 10250 bypass API-server RBAC. Do not expose it
publicly. Authentication and authorization are required before shared deployment.
The chart does not create a public kubelet Service.

A self-signed IP-SAN serving certificate is generated in memory. If the API server
verifies kubelet certificates, configure a trusted certificate using
`kubeletAPI.tlsSecretName`. This Secret must contain `tls.crt` and `tls.key`, and
the certificate must cover the controller Pod IP. No CSR is requested or approved;
API-server TLS settings are never changed. After a controller restart, the IP
and self-signed certificate change. Reachability and certificate compatibility
require validation on each target cluster; unit tests do not establish DOKS support.

CLI: `--enable-kubelet-api --kubelet-auth-mode=none --pod-ip=...`. `POD_IP` is also
read from the environment. Supply both `--tls-cert-file` and `--tls-key-file` for
an existing certificate. Invalid supplied files do not fall back to self-signed TLS.

## RunPod

The configured RunPod API key is used to read container-only REST v2 SSE events.
System events such as image-pull diagnostics are excluded.

```bash
kubectl logs <pod>
kubectl logs -f <pod> --tail=100 --timestamps
kubectl logs <pod> --since=10m --limit-bytes=65536
```

RunPod has no confirmed replay-complete marker. A one-shot read ends after a
1-second idle gap following container output, or a 30-second maximum replay
window. An empty stream can take 30 seconds. Delayed or continuous replay can be
truncated: this is a best-effort snapshot. Configure `--log-snapshot-idle` and
`--log-snapshot-max`, or `GPU_BURST_LOG_SNAPSHOT_IDLE` and
`GPU_BURST_LOG_SNAPSHOT_MAX`. Plain follow does not terminate on silence.

Tail accepts 0–5000 lines. Larger values fail instead of silently clamping. An
omitted tail requests all available history using an early since value, instead
of the API default of 100 lines. Upstream retention still applies. The early
query timestamp requires live validation in the gated smoke before release.
`--since` with `--tail=0 --follow` is rejected because RunPod since overrides tail.

## Lambda

Enable logs when launching a new VM; existing VMs are not modified. Configure
runner-reachable URLs and an existing Secret holding the callback signing root:

```yaml
runnerLogs:
  enabled: true
  secretName: neocloud-runner
  secretKey: sharedSecret
  callbackURL: https://controller.example/neocloud-node/v1/runner/status
  url: https://controller.example/neocloud-node/v1/runner/logs
```

The chart creates an internal runner ClusterIP Service on port 9091. Configure a
reachable TLS ingress/proxy for the two paths. HTTP is accepted for isolated
networks, but HMAC signatures do not encrypt log contents. No credentials are
stored in Helm values and no credential Secret is created. Kubelet development
auth settings do not disable log upload authentication.

CLI users configure `--lambda-logs-url`, `--lambda-callback-url`,
`--runner-callback-addr`, and `--runner-callback-secret`. The controller derives
a workload-scoped upload key and validates the binding before accepting records.

The runner keeps a detached workload container and captures timestamped stdout
and stderr. Signed batches and lifecycle callbacks run independently. Sequence
numbers deduplicate retries. A new controller epoch requests retained replay;
reads may report unavailable until it arrives. Missing heartbeats eventually
produce transport errors, not empty successful logs.

The runner supports Python 3.10 and newer. Docker's RFC3339 nanosecond timestamps
are normalized only for Python datetime validation; original timestamps and
nanosecond checkpoint ordering are preserved. CI exercises Python 3.10, 3.11 and
3.12. An existing helper that stopped on a capture error needs an explicit
operator repair or a fresh runner; updating the controller alone does not change
scripts already installed on a VM.

The VM spool retains at most 8 MiB and 5000 records; the controller retains at
most 4 MiB and 5000 records per workload. The app holds at most 32 workloads;
Pod deletion releases the buffer. Docker storage rotates at 16 MiB per file,
two files maximum. The helper remains for 24 hours after completion, allowing
completed logs and restart recovery during that window. VM deletion ends recovery.
Evicted history produces explicit errors for full-history reads; retained tail
or time ranges can remain available when completeness can be proven. Time filters
are inclusive: a cutoff must be strictly after every known evicted timestamp,
or a requested tail must have enough matching retained records. If the VM lost
an unuploaded prefix, only a sufficiently retained tail can prove coverage.
Workloads bypassing controller deletion may
occupy a cache slot until restart.

Docker24+ resumes capture from a verified timestamp suffix. Older or unrecognized
Docker versions replay the full retained Docker log and verify a durable prefix
proof before appending records; they do not use per-record since filtering,
which can lose nonmonotonic timestamps. On this fallback path, Docker rotation
that removes the captured prefix produces an explicit history-loss error.
Upgrading an old spool without that proof requires its complete prefix to still
be retained; incomplete proof is never treated as successful recovery.

## Shared behavior and limits

The current Pod UID, container and virtual node are validated before routing to
the provider and remote resource on the binding. Log reads never allocate remote
resources. Timestamps and time filters use recorded event times. `--previous`
is unsupported until restart-instance history is available.

There are at most 16 concurrent streams across all backends, with a 1 MiB record
limit and 4 MiB tail buffer. Exceeding supported limits fails explicitly. Closing
a stream cancels upstream reads and background tasks. RunPod does not transparently
reconnect after a transport failure.

Container logs can contain application credentials or user data. Output is
returned unchanged to readers; controller diagnostics must not copy raw payloads
or cloud keys. Collect diagnostics before deleting paid resources. No durable
archive independent of the remote resource is introduced.

## Errors through the API-server proxy

One-shot reads buffer up to 4 MiB until the snapshot boundary or requested byte
limit, so source failures before completion return an error before HTTP output
is committed. Use tail or limit-bytes for larger output. Continuous follow
streams cannot change their status after output begins. Some API-server proxy
versions turn a backend read failure into clean EOF even if the controller aborts
its connection. The controller emits a Warning Pod event with reason
`ContainerLogStreamFailed` on source read failures; check `kubectl describe pod`
or events when a follow stream ends unexpectedly. Controller diagnostics are
never appended to the application log body.
