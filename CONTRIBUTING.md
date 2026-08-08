# Contributing to AIBrix NeoCloud Node

Thanks for your interest in AIBrix NeoCloud Node. The project is experimental,
so the best contributions are small, well-scoped fixes that improve reliability,
provider compatibility, tests, and documentation.

## Development Setup

Run the local verification suite before sending changes:

```bash
go test -count=1 ./...
go build -o neocloud-node ./cmd/neocloud-node
```

The real RunPod and Lambda Cloud smoke tests are gated because they create paid
cloud resources. Do not run them unless you have an account, a cleanup plan, and
the required environment variables set.

## Contribution Guidelines

- Keep provider-specific behavior inside the provider adapter packages.
- Keep Kubernetes API access in the Virtual Kubelet/provider wrapper and binding
  store layers.
- Do not silently ignore unsupported Pod fields. Return a clear unsupported
  reason and surface it through status or events.
- Preserve idempotency. A controller restart or provider retry must not create
  duplicate paid resources for the same Pod UID.
- Add or update tests for lifecycle, transformation, provider error
  classification, and cleanup behavior.
- Never commit API keys, cloud credentials, provider logs, local trace outputs,
  or paid resource identifiers.

## Good First Areas

- Documentation consistency and examples.
- Helm chart and packaging.
- Provider metrics and orphan sweeper events.
- RunPod logs or provider console links.
- Lambda runner heartbeat timeout and diagnostics.
- More provider contract tests against fake HTTP servers.
