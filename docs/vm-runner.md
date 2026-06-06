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

When the kubelet API server is enabled, GPU Burst Node serves:

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
- No secure secret delivery model.
- No checkpoint hook.
- No retry or timeout supervisor on the VM.
- No heartbeat protocol yet.
- No structured runner binary yet; Phase 4 uses shell/cloud-init only.
