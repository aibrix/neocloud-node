# NeoCloud Trace

NeoCloud Trace is a command-line tool for measuring GPU-cloud catalog
availability, capacity, and Spot stability. It stores observations, experiment
state, cleanup evidence, budgets, and billing records in SQLite and exports
analysis-ready CSV files.

Lambda Cloud and RunPod are supported. The runtime uses only the Python standard
library and SQLite.

## Installation

Python 3.10 or newer is required. From the repository root:

```bash
pip install tools/neocloud-trace
neocloud-trace --help
```

Editable setup, direct module execution, and tests are documented in
[DEVELOPMENT.md](DEVELOPMENT.md).

## Workflows

The three collection commands have separate contracts:

| Command | Function | Active plan input | Scheduled unit |
| --- | --- | --- | --- |
| `collect` | Passive catalog, price, API, host, and network observations | None | None |
| `tick` | One due capacity epoch | One `capacity_cohorts` entry with `max_trials=1` | `capacity_epochs` |
| `run` | One due Stability sampling window | One `canary_cohorts` entry | `stability_windows` |

`tick` and `run` require every other active plan section to be empty. Invalid or
mixed plans fail before a provider client is constructed or a database is
opened.

A database can contain multiple runs and workflow types; three database files
are not required. A campaign must keep using the same plan, database, and
campaign ID. Use `--run-id` when querying or exporting a database containing
several runs.

## Credentials and Safety

Credentials are read from environment variables and are not written to SQLite:

```bash
export LAMBDA_CLOUD_API_KEY=...
export LAMBDA_CLOUD_SSH_KEYS=public-key-name
export RUNPOD_API_KEY=...
```

`LAMBDA_CLOUD_SSH_KEYS` is a comma-separated list required for Lambda instance
creation. Optional endpoint overrides are:

```bash
export LAMBDA_CLOUD_BASE_URL=https://cloud.lambda.ai/api/v1
export RUNPOD_BASE_URL=https://rest.runpod.io/v1
export RUNPOD_GRAPHQL_URL=https://api.runpod.io/graphql
```

Commands that can create or delete resources require `--enable-active`: `run`,
`tick`, `cleanup`, and `recover`. Active execution enforces provider and total
budgets, GPU concurrency limits, verified cleanup, and a block on new work while
unresolved provider resources remain.

Review provider, SKU, GPU type, region, duration, price assumptions, and budget
before starting a campaign. If a collector process is lost, run `recover`
promptly to censor observations and clean up remote resources.

## Collector Identity

Every run stores `host_id` and `vantage_id`.

For `collect`, `host_id` is the local hostname. `vantage_id` defaults to that
hostname and can be overridden with `--vantage-id` to identify a stable network
or physical location.

For plan-based commands, `host_id` is resolved in this order:

1. `--host-id`;
2. a non-placeholder plan value;
3. the local hostname.

`vantage_id` is resolved from `--vantage-id` and then the plan. Plan-based
commands reject unresolved values such as `replace-with-network-vantage`.

```bash
neocloud-trace init \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/trace.db \
  --host-id trace-host-01 \
  --vantage-id us-west-office
```

## Validate a Plan

`init` parses a plan, validates its data model, creates the idempotent SQLite
schema, and prints experiment counts and cost bounds. It does not contact a
provider or create resources.

```bash
neocloud-trace init \
  --plan /path/to/plan.json \
  --db data/neocloud-trace/trace.db \
  --vantage-id us-west-office
```

The examples in `plans/` include current and historical experiment shapes.
`run` and `tick` apply their stricter command contracts even when a plan passes
general `init` validation.

## Passive Collection

`collect` takes provider and sampling settings directly. It does not accept a
plan and never creates cloud resources.

```bash
# One snapshot
neocloud-trace collect \
  --db data/neocloud-trace/trace.db \
  --provider runpod \
  --once

# Timed collection
neocloud-trace collect \
  --db data/neocloud-trace/trace.db \
  --provider lambda \
  --provider runpod \
  --vantage-id us-west-office \
  --duration-seconds 3600
```

Without `--duration-seconds`, one snapshot is collected. Timed catalog and host
sampling default to 300 and 30 seconds; override them with
`--catalog-interval-seconds` and `--host-interval-seconds`.

## Capacity Epochs

`tick` is intended for cron, systemd timers, Kubernetes CronJobs, or another
external scheduler. A valid plan contains exactly one `capacity_cohorts` entry,
sets `max_trials` to `1`, and leaves all other active sections empty.

```bash
neocloud-trace tick \
  --plan /path/to/capacity-plan.json \
  --db data/neocloud-trace/trace.db \
  --vantage-id us-west-office \
  --enable-active
```

The first invocation creates the campaign and its scheduled epoch. Each
invocation atomically claims at most one due epoch, records missed slots instead
of replaying a backlog, verifies cleanup, and exits. Result statuses include
`idle`, `busy`, `cleanup_blocked`, `executed`, and `complete`.

The default campaign ID is reused from the database. Use `--campaign-id` for an
explicit stable identity.

## Stability Sampling Windows

`run` accepts exactly one `canary_cohorts` entry and no other active section.
Each canary pair contains a Spot lease and a matched on-demand lease for the
same provider cell. Readiness, replacement, censoring, budget, billing, and
cleanup logic runs inside the claimed outer window.

```bash
neocloud-trace run \
  --plan /path/to/stability-plan.json \
  --db data/neocloud-trace/trace.db \
  --vantage-id us-west-office \
  --enable-active
```

The optional top-level `stability_schedule` controls durable outer windows:

```json
{
  "duration_seconds": 14400,
  "stability_schedule": {
    "window_count": 5,
    "schedule_seed": 20260823,
    "initial_delay_seconds": 3600,
    "interval_seconds": 86400,
    "jitter_seconds": 21600
  }
}
```

This top-level block accompanies the plan's required `canary_cohorts` entry.

`duration_seconds` is each outer window's duration. The schedule uses its seed
to generate deterministic jittered offsets and rejects overlapping windows. If
`stability_schedule` is omitted, the plan has one immediate window.

The first invocation writes the complete schedule to SQLite. Invoke `run` once
before the first scheduled window, then near each window start. Each invocation
claims at most one window. A later invocation marks every expired, unclaimed
window as `missed`; those rows remain in the analysis denominator even though no
canary lease was created.

Use the same plan and database for every invocation. `--campaign-id` can set an
explicit identity; otherwise the single Stability campaign already in the
database is reused.

## Status, Cleanup, and Recovery

```bash
neocloud-trace status --db data/neocloud-trace/trace.db
neocloud-trace status --db data/neocloud-trace/trace.db --run-id RUN_ID

neocloud-trace cleanup \
  --db data/neocloud-trace/trace.db \
  --run-id RUN_ID \
  --enable-active

neocloud-trace recover \
  --db data/neocloud-trace/trace.db \
  --run-id RUN_ID \
  --enable-active
```

`status` includes lifecycle state, row counts, budget exposure, and unresolved
resources. Recovery right-censors unfinished leases, closes the active capacity
epoch or Stability window, and verifies cleanup. Future scheduled work is
preserved when cleanup succeeds. Without `--run-id`, the latest run is selected.

## Export Data

Exports use the latest run unless `--run-id` is supplied:

```bash
neocloud-trace export stability \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/stability-windows.csv

neocloud-trace export survival \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/survival-leases.csv

neocloud-trace export depth \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/capacity-depth.csv

neocloud-trace export window \
  --db data/neocloud-trace/trace.db \
  --output data/neocloud-trace/completion-window.csv
```

| Export | One row per | Scheduled-but-not-run rows |
| --- | --- | --- |
| `stability` | Scheduled Stability window | Included |
| `survival` | Canary lease | Use with the Stability-window export |
| `depth` | Scheduled capacity epoch | Included |
| `window` | Historical completion-window opportunity | Included |

`trial` prints one capacity trial as detailed JSON:

```bash
neocloud-trace trial \
  --db data/neocloud-trace/trace.db \
  --trial-id TRIAL_ID
```

Reusable SQL queries are in `sql/research-trace-queries.sql`.

## Billing

Sync RunPod billing records attributable to resources in a run:

```bash
neocloud-trace billing sync \
  --db data/neocloud-trace/trace.db \
  --provider runpod \
  --run-id RUN_ID
```

Lambda has no public usage endpoint. Import a console or invoice export:

```bash
neocloud-trace billing import \
  --db data/neocloud-trace/trace.db \
  --provider lambda \
  --source invoice-2026-08 \
  --input /path/to/lambda-usage.csv \
  --run-id RUN_ID
```

The CSV columns are shown in `billing-import-template.csv`. Reconcile charges
with trace-owned resources using:

```bash
neocloud-trace billing reconcile \
  --db data/neocloud-trace/trace.db \
  --run-id RUN_ID
```

Quoted price, realized price, budget commitment, runtime estimate, and billed
amount are stored separately.

## Stored Data

SQLite records include:

- run configuration, identity, and lifecycle;
- host/network observations, provider API results, and catalog quotes;
- capacity campaigns, scheduled epochs, trials, stages, and replicas;
- Stability campaigns, scheduled windows, linked canary epochs, matched leases,
  and lease observations;
- budget commitments, releases, runtime settlements, and exposure;
- cleanup attempts and verified deletion;
- provider billing records and cost reconciliation.

Databases and their `-wal` and `-shm` files are runtime data and are excluded
from Git, as are credentials, Python caches, and temporary exports.

## Development and Legacy Code

See [DEVELOPMENT.md](DEVELOPMENT.md) for Python setup, tests, package
verification, and the change checklist.

The original single-file collector remains under `legacy/` for historical
workflows. New work should use the packaged `neocloud-trace` command.
