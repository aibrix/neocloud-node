# VM Runner Bootstrap

日期：2026-06-05

The VM runner bootstrap is the provider-neutral base for VM-backed GPU providers such as Lambda Cloud. It converts a `core.WorkloadRequest` into cloud-init that can be passed when launching a VM instance.

Lambda Cloud's public documentation states that API-launched instances can use cloud-init for launch-time configuration, and that instances might take several minutes to launch. This makes a VM runner model more appropriate than pretending Lambda Cloud is a direct container API.

## Model

1. Provider adapter launches one VM for one workload.
2. Provider adapter passes generated cloud-init to the VM launch request.
3. cloud-init installs Docker.
4. cloud-init pulls the requested image.
5. cloud-init runs the container with `--gpus all`.
6. cloud-init posts a completion payload to the controller callback URL.
7. Provider adapter terminates the VM when the workload completes or the Kubernetes workload is deleted.

## Generated Cloud-Init

Phase 4 cloud-init contains:

- `package_update: true`
- `docker.io` package installation
- `docker pull <image>`
- `docker run --rm --gpus all`
- literal env var propagation
- command and args
- callback `curl` with workload ID and exit code

## Callback Contract

GPU Burst Node serves a provider-neutral runner callback endpoint:

`POST /gpu-burst-node/v1/runner/status`

Payload:

```json
{
  "workloadID": "default/pod-uid",
  "exitCode": 0,
  "message": "optional detail"
}
```

The controller resolves `workloadID` as `<namespace>/<podUID>` and updates the durable binding:

- `exitCode == 0` -> `CloudPodBinding.status.phase=Succeeded`
- `exitCode != 0` -> `CloudPodBinding.status.phase=Failed` with a `RunnerExitCode` condition

### Authentication

Unsigned callbacks are accepted only when no callback secret is configured. Production deployments should set `--runner-callback-secret` or `GPU_BURST_RUNNER_CALLBACK_SECRET`.

When a secret is configured, the runner must send:

| Header | Value |
| --- | --- |
| `X-GPU-Burst-Runner-Timestamp` | Unix seconds at signing time |
| `X-GPU-Burst-Runner-Signature` | `v1=<hex hmac-sha256>` |

The signed message is:

```text
<timestamp>.<raw request body>
```

The HMAC key is the configured callback secret. The controller rejects missing signatures, invalid signatures and timestamps outside a 10 minute clock-skew window. This protects against accidental cross-workload callback writes and basic replay.

### Provider Integration Rules

- VM-backed providers such as Lambda pass the callback URL and optional secret into cloud-init.
- Container-backed providers such as RunPod should not rely on provider pod lifecycle alone for batch completion. RunPod direct pods currently expose a durable pod resource state (`RUNNING`) rather than a reliable container exit state through the REST pod object. Batch completion should therefore use the same runner callback contract, either through a generated wrapper command or a small runner binary.
- Serving workloads can skip runner completion callbacks and remain `Running` until deleted.

## Validation

`BuildCloudInit` rejects:

| Missing field | Error code |
| --- | --- |
| workload ID | `MissingWorkloadID` |
| callback URL | `MissingCallbackURL` |
| container image | `MissingImage` |

## Current Limitations

- No SSH fallback.
- No log streaming.
- Callback signing is supported, but secret delivery is still operator-managed env/flag configuration.
- No checkpoint hook.
- No retry or timeout supervisor on the VM.
- No heartbeat protocol yet.
- No structured runner binary yet; Phase 4 uses shell/cloud-init only.
