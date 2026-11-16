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

Lambda logs are pulled on demand over SSH. The controller resolves the instance
address from the current Pod binding and executes `sudo -n docker logs` for the
runner's fixed `neocloud-node-workload` container. Docker handles follow, tail
and time filters; the controller bounds streams, formats timestamps and applies
byte limits. Finite reads wait for Docker to finish. Closing the request closes
the SSH connection. No log shipper, log-upload endpoint, controller log cache or
replay protocol is used.

Configure an existing SSH key whose public key is registered with Lambda and
selected by `GPU_BURST_LAMBDA_SSH_KEY_NAMES`. Populate `known_hosts` with the VM
host keys verified through an independent trusted channel, including each VM's
address. Refresh those entries when instances or their host keys change.

```bash
kubectl -n aibrix-system create secret generic neocloud-lambda-ssh \
  --from-file=ssh-privatekey=./id_ed25519 \
  --from-file=known_hosts=./known_hosts
```

```yaml
lambdaSSH:
  secretName: neocloud-lambda-ssh
  privateKeyKey: ssh-privatekey
  knownHostsKey: known_hosts
  user: ubuntu
```

The SSH user must have noninteractive `sudo docker` access. The controller needs
to reach the VM on TCP 22. Unknown or changed host keys fail verification. An
unencrypted OpenSSH/PEM private key is required; an SSH agent and passphrase prompt
are not supported. Key and known-hosts files are read for each new log session.
Existing sessions are not reauthenticated when these files change.

CLI equivalents are `--lambda-ssh-user`, `--lambda-ssh-private-key-file` and
`--lambda-ssh-known-hosts-file`; their environment variables are
`GPU_BURST_LAMBDA_SSH_USER`, `GPU_BURST_LAMBDA_SSH_PRIVATE_KEY_FILE` and
`GPU_BURST_LAMBDA_SSH_KNOWN_HOSTS_FILE`. Supplying only one file is invalid.

The VM runner retains the workload container after exit and bounds Docker's
json-file logs to two 16 MiB files. Logs are available only while the VM and
container exist and Docker retains them; this is not a durable log archive.
Existing VMs created with automatic container removal need a fresh retained
container to expose completed logs. Reading logs does not allocate a VM.

Lambda's existing lifecycle status callback remains separate from SSH logs. If
not already configured, the chart can expose its internal status Service:

```yaml
runnerCallback:
  enabled: true
  secretName: neocloud-runner
  secretKey: sharedSecret
  url: https://controller.example/neocloud-node/v1/runner/status
```

The callback signing Secret and VM-reachable HTTPS route are operator-managed.
This Service is for status callbacks, not log uploads. No additional callback
route or signing key is required by SSH log reads.
