# NeoCloud Trace

NeoCloud Trace is a command-line tool for measuring GPU cloud availability and
provisioning behavior. It collects provider catalogs, runs budget-bounded
capacity and lease experiments, stores observations in SQLite, and exports
analysis-ready data.

The supported providers are Lambda Cloud and RunPod. The runtime uses only the
Python standard library and SQLite.

## Installation

Python 3.10 or newer is required. From the repository root:

```bash
pip install tools/neocloud-trace
```

This installs the `neocloud-trace` command. Verify it with:

```bash
neocloud-trace --help
```

Editable installation, direct Python module execution, and test commands are
documented in [DEVELOPMENT.md](DEVELOPMENT.md).

## Core Concepts

Active experiments use two files:

- a plan JSON file describing providers, experiment types, durations, GPU
  shapes, and budget limits;
- a SQLite database containing run state, observations, owned resources,
  cleanup events, and billing records.

Passive `collect` does not need an experiment plan. It takes provider and
sampling options directly and writes to the same SQLite format.

Commands place their options after the command name:

```text
neocloud-trace COMMAND [OPTIONS]
```

For example:

```bash
neocloud-trace init \
  --plan tools/neocloud-trace/plans/research-runpod-pilot-3h.json \
  --db data/neocloud-trace/runpod-pilot.db \
  --vantage-id us-west
```

Run `neocloud-trace COMMAND --help` for command-specific options. The top-level
commands are:

| Command | Purpose |
| --- | --- |
| `init` | Validate a plan and initialize its database |
| `collect` | Collect passive catalog and host observations |
| `run` | Run all or one selected experiment from a plan |
| `tick` | Advance one scheduled capacity epoch and exit |
| `status` | Show persisted run status and owned resources |
| `cleanup` | Delete and verify resources owned by a run |
| `recover` | Close a lost run and verify remote cleanup |
| `trial` | Show the detailed payload for a capacity trial |
| `export` | Export survival, capacity-depth, or window CSV data |
| `billing` | Sync, import, or reconcile provider billing |

## Collector Identity

Every persisted run has a `host_id` and a `vantage_id`.

For `collect`, `host_id` is always the local hostname. `vantage_id` defaults to
the same value, so a normal control-plane snapshot needs no identity options.
Set `--vantage-id` only when several collector hosts should be grouped under a
stable network or physical location for API-latency analysis.

Plan-based commands resolve identity differently. `host_id` is resolved in this
order:

1. `--host-id`;
2. a non-placeholder `host_id` in the plan;
3. the local system hostname.

For plan-based commands, `vantage_id` is resolved in this order:

1. `--vantage-id`;
2. a non-placeholder `vantage_id` in the plan.

Plan-based commands reject unresolved placeholder values such as
`replace-with-network-vantage`.

Use stable, human-readable values when comparing runs from different machines
or networks:

```bash
neocloud-trace init \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/trace.db \
  --host-id trace-host-01 \
  --vantage-id us-west-office
```

## Credentials

Provider credentials are read from environment variables and are not written to
the trace database:

```bash
export LAMBDA_CLOUD_API_KEY=...
export LAMBDA_CLOUD_SSH_KEYS=public-key-name
export RUNPOD_API_KEY=...
```

`LAMBDA_CLOUD_SSH_KEYS` is a comma-separated list and is required when creating
Lambda instances.

Optional endpoint overrides are:

```bash
export LAMBDA_CLOUD_BASE_URL=https://cloud.lambda.ai/api/v1
export RUNPOD_BASE_URL=https://rest.runpod.io/v1
export RUNPOD_GRAPHQL_URL=https://api.runpod.io/graphql
```

## Safety

Commands that can create or delete cloud resources require
`--enable-active`. This applies consistently to `run`, `tick`, `cleanup`, and
`recover`.

```bash
neocloud-trace run \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/trace.db \
  --vantage-id us-west-office \
  --enable-active
```

Active operations also enforce the limits in the plan:

- per-provider and total budget limits;
- maximum simultaneous GPUs per provider;
- no new active run while an unresolved owned resource remains;
- verified deletion before a resource is considered cleaned up;
- bounded cleanup after `SIGINT` or `SIGTERM`.

Review provider, GPU type, region, duration, and budget fields before enabling an
active command. A collector host that sleeps or loses power can leave remote
resources billing until `recover` is run.

## Validate a Plan

`init` validates the plan, initializes the SQLite schema, and reports experiment
counts and conservative cost bounds. It does not create cloud resources.

```bash
neocloud-trace init \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/trace.db \
  --vantage-id us-west-office
```

A plan defines:

- protocol version and collector metadata;
- experiment duration and sampling intervals;
- provider budgets and the total budget;
- GPU counts and concurrency limits;
- static trials or scheduled cohorts;
- provider, SKU, region, resource class, and market for each experiment cell.

Bundled examples are available under `plans/`:

| Plan | Purpose |
| --- | --- |
| `research-runpod-pilot-3h.json` | RunPod capacity and matched lease pilot |
| `research-pilot-3h.json` | Lambda and RunPod capacity plus matched leases |
| `research-combined-pilot-4h.json` | Combined multi-provider pilot |
| `research-runpod-expansion-6h.json` | Repeated RunPod lease trials |
| `research-runpod-survival-cohort-24h.json` | Continuous RunPod survival cohort |
| `research-capacity-depth-cohort-24h.json` | Scheduled capacity-depth cohort |
| `research-capacity-fragmentation-d32-pilot.json` | Capacity fragmentation up to 32 GPUs |
| `research-completion-window-cohort-24h.json` | Randomized allocation-window cohort |
| `research-lambda-control-cohort-24h.json` | Lambda lifecycle control cohort |

Treat these files as examples. Their placeholder metadata must be resolved with
CLI options or replaced in a copied plan.

## Passive Collection

`collect` records provider API health, normalized catalog quotes, prices, and
collector host/network observations. It never creates cloud resources and does
not accept `--enable-active`.

Collect one snapshot:

```bash
neocloud-trace collect \
  --db data/neocloud-trace/trace.db \
  --provider runpod \
  --once
```

Collect for a fixed duration:

```bash
neocloud-trace collect \
  --db data/neocloud-trace/trace.db \
  --provider lambda \
  --provider runpod \
  --vantage-id us-west-office \
  --duration-seconds 3600
```

Without `--duration-seconds`, `collect` records one snapshot and exits. During a
timed collection, catalog and host sampling default to 300 and 30 seconds;
`--catalog-interval-seconds` and `--host-interval-seconds` override them.

## Run Experiments

`run` executes the experiment sections in a plan. The default target is `all`:

```bash
neocloud-trace run \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/trace.db \
  --vantage-id us-west-office \
  --enable-active
```

For a focused run, use `--target` and an optional zero-based `--index`:

```bash
neocloud-trace run \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/capacity.db \
  --vantage-id us-west-office \
  --enable-active \
  --target capacity-cohort \
  --index 0
```

Targets map directly to plan sections:

| Target | Plan section |
| --- | --- |
| `all` | All configured experiment sections |
| `capacity` | `capacity_trials` |
| `canary` | `canary_trials` |
| `canary-cohort` | `canary_cohorts` |
| `capacity-cohort` | `capacity_cohorts` |
| `window-cohort` | `completion_window_cohorts` |
| `control-cohort` | `control_cohorts` |

## Scheduled Capacity Ticks

`tick` is for cron, systemd timers, Kubernetes CronJobs, or another external
scheduler. Each invocation claims at most one due capacity epoch, executes it,
persists the result, verifies cleanup, and exits.

```bash
neocloud-trace tick \
  --plan /path/to/capacity-plan.json \
  --db data/neocloud-trace/capacity.db \
  --vantage-id us-west-office \
  --enable-active
```

Use the same plan and database on every invocation. The campaign identity is
reused from the database; `--campaign-id` can set an explicit stable identity.
`--index` selects a capacity cohort when the plan contains more than one.

Possible statuses include `idle`, `busy`, `cleanup_blocked`, `executed`, and
`complete`.

## Status, Cleanup, and Recovery

Show the latest run or a specific run:

```bash
neocloud-trace status --db data/neocloud-trace/trace.db
neocloud-trace status --db data/neocloud-trace/trace.db --run-id RUN_ID
```

Delete and verify resources still owned by a run:

```bash
neocloud-trace cleanup \
  --db data/neocloud-trace/trace.db \
  --run-id RUN_ID \
  --enable-active
```

Use `recover` after the original collector process was lost. It right-censors
open observations, records a recovery termination reason, and verifies remote
cleanup:

```bash
neocloud-trace recover \
  --db data/neocloud-trace/trace.db \
  --run-id RUN_ID \
  --enable-active
```

If `--run-id` is omitted, these commands select the most recent run.

## Inspect and Export Data

Show a detailed capacity trial as JSON:

```bash
neocloud-trace trial \
  --db data/neocloud-trace/trace.db \
  --trial-id TRIAL_ID
```

Without `--trial-id`, the most recent capacity trial is selected.

Export analysis-ready CSV files:

```bash
neocloud-trace export survival \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/survival.csv

neocloud-trace export depth \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/capacity-depth.csv

neocloud-trace export window \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/completion-window.csv
```

Each export accepts `--run-id`; otherwise it uses the most recent run. Reusable
SQL queries are available in `sql/research-trace-queries.sql`.

## Billing

Sync RunPod billing records that can be attributed to resources in a run:

```bash
neocloud-trace billing sync \
  --db data/neocloud-trace/trace.db \
  --provider runpod
```

Lambda does not expose a public usage endpoint. Import an invoice or console
export instead:

```bash
neocloud-trace billing import \
  --db data/neocloud-trace/trace.db \
  --provider lambda \
  --source invoice-2026-08 \
  --input /path/to/lambda-usage.csv
```

The expected CSV columns are shown in `billing-import-template.csv`. Reconcile
recorded or imported charges with trace resources using:

```bash
neocloud-trace billing reconcile \
  --db data/neocloud-trace/trace.db
```

Billing commands accept `--run-id`; otherwise they use the most recent run.

## Stored Data

SQLite records include:

- experiment configuration and run lifecycle;
- provider API observations and normalized catalog quotes;
- collector host and network observations;
- capacity campaigns, epochs, trials, stages, and replicas;
- canary leases and lease observations;
- budget reservations, settlements, and exposure;
- owned resources and cleanup verification;
- provider billing records and cost reconciliation.

Database files and their `-wal` and `-shm` companions are runtime data and are
excluded from Git.

## Development and Legacy Code

See [DEVELOPMENT.md](DEVELOPMENT.md) for the editable setup, direct module
execution, test workflow, and change checklist.

The original single-file collector remains under `legacy/` for existing
databases and historical workflows. New work should use the packaged
`neocloud-trace` command.
