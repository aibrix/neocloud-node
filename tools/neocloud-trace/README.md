# NeoCloud Trace

NeoCloud Trace is a command-line tool for measuring GPU cloud availability and
provisioning behavior. It can:

- sample provider catalogs and prices without creating resources;
- measure how much GPU capacity can be provisioned at once;
- monitor interruptible and on-demand GPU leases over time;
- run scheduled allocation-window experiments;
- persist every observation, resource, cleanup action, and budget decision in
  SQLite;
- export analysis-ready CSV files and reconcile provider billing data.

The supported providers are Lambda Cloud and RunPod. The implementation uses
the Python standard library and SQLite; no runtime Python dependencies are
required.

## Requirements

- Python 3.9 or newer
- Provider API credentials for catalog collection or active experiments
- A host that remains online for the full duration of long-running experiments

## Installation

From the repository root, install the tool in editable mode:

```bash
python3 -m pip install --editable tools/neocloud-trace
```

This installs the `neocloud-trace` command while keeping it connected to the
source checkout.

For development, the package can also be run without installation:

```bash
cd tools/neocloud-trace
PYTHONPATH=src python3 -m neocloud_trace --help
```

Run the test suite from the repository root with:

```bash
python3 -m pytest tools/neocloud-trace/tests
```

## Credentials

Set credentials only in the environment. Credentials are not written to the
trace database.

```bash
export LAMBDA_CLOUD_API_KEY=...
export LAMBDA_CLOUD_SSH_KEYS=public-key-name
export RUNPOD_API_KEY=...
```

`LAMBDA_CLOUD_SSH_KEYS` is a comma-separated list and is required when the tool
creates Lambda instances.

The following endpoint overrides are optional:

```bash
export LAMBDA_CLOUD_BASE_URL=https://cloud.lambda.ai/api/v1
export RUNPOD_BASE_URL=https://rest.runpod.io/v1
export RUNPOD_GRAPHQL_URL=https://api.runpod.io/graphql
```

## Safety Model

Active experiments create billable cloud resources. The tool applies several
guards:

- plans must define per-provider and total budget limits;
- active experiment commands require a real host identity and network vantage
  identity;
- most commands that create or delete resources require `--enable-active`;
- a new active run is blocked while the database contains an unresolved cloud
  resource;
- resource deletion is not considered complete until the provider reports the
  resource terminal or missing;
- `SIGINT` and `SIGTERM` trigger bounded cleanup.

The `capacity` command is an intentionally active scheduler entry point. It
does not require `--enable-active`; invoking it with a due epoch may immediately
create billable resources.

Use a stable host for long-running experiments. If the host sleeps or loses
power, remote resources may continue billing until recovery is run.

## Quick Start

The following example validates a RunPod plan, collects one passive snapshot,
and then runs the configured experiment.

```bash
neocloud-trace \
  --db data/neocloud-trace/runpod-pilot.db \
  --plan tools/neocloud-trace/plans/research-runpod-pilot-3h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  init

neocloud-trace \
  --db data/neocloud-trace/runpod-pilot.db \
  --plan tools/neocloud-trace/plans/research-runpod-pilot-3h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  passive --once

neocloud-trace \
  --db data/neocloud-trace/runpod-pilot.db \
  --plan tools/neocloud-trace/plans/research-runpod-pilot-3h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  pilot
```

`init` validates the plan, initializes the SQLite schema, and prints the
planned cost upper bounds. It does not create cloud resources.

## Plans

A plan is a JSON file that defines:

- `host_id` and `vantage_id` for identifying the collector and its network
  location;
- experiment duration and sampling intervals;
- GPU counts and the maximum simultaneous GPUs allowed per provider;
- provider-specific and total budget limits;
- capacity trials, canary trials, or scheduled cohorts.

Use `--host-id` and `--vantage-id` to override those values without editing the
plan. Active commands reject placeholder metadata.

Always validate a plan before running it:

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  --plan /path/to/plan.json \
  init
```

Bundled plans provide working examples for each experiment type:

| Plan | Purpose |
| --- | --- |
| `research-runpod-pilot-3h.json` | RunPod capacity and matched lease pilot |
| `research-pilot-3h.json` | Lambda and RunPod capacity plus matched lease monitoring |
| `research-combined-pilot-4h.json` | Combined multi-provider capacity and lease run |
| `research-runpod-expansion-6h.json` | Repeated RunPod matched lease trials |
| `research-runpod-survival-cohort-24h.json` | Continuous RunPod interruptible/on-demand replacement cohort |
| `research-capacity-depth-cohort-24h.json` | Scheduled Lambda and RunPod capacity-depth cohort |
| `research-capacity-fragmentation-d32-pilot.json` | Multi-instance and multi-GPU capacity-depth experiment |
| `research-completion-window-cohort-24h.json` | Randomized allocation-window experiment |
| `research-lambda-control-cohort-24h.json` | Lambda on-demand lifecycle control cohort |

Treat bundled plans as examples. Review GPU types, regions, durations, and
budgets before enabling active execution.

## Passive Catalog Collection

Passive collection records provider API health, normalized catalog quotes,
prices, and host/network health. It never creates cloud resources.

Collect one snapshot:

```bash
neocloud-trace \
  --db data/neocloud-trace/catalog.db \
  --plan /path/to/plan.json \
  passive --once
```

Collect for the duration configured in the plan:

```bash
neocloud-trace \
  --db data/neocloud-trace/catalog.db \
  --plan /path/to/plan.json \
  passive
```

Override the duration for a particular run:

```bash
neocloud-trace \
  --db data/neocloud-trace/catalog.db \
  --plan /path/to/plan.json \
  passive --duration-seconds 3600
```

## Capacity Measurement

Capacity trials create resources in cumulative depth stages. For example, a
single-GPU cell targeting eight GPUs attempts cumulative depths of 1, 2, 4,
and 8. Each resource is tracked from request through provider readiness,
application readiness, and verified cleanup.

### Run One Configured Trial

```bash
neocloud-trace \
  --db data/neocloud-trace/capacity.db \
  --plan /path/to/plan.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  capacity-once --index 0
```

`--index` selects an entry from the plan's `capacity_trials` list.

### Run a Scheduled Capacity Cohort

`depth-cohort` keeps one process running and executes the full precomputed
schedule:

```bash
neocloud-trace \
  --db data/neocloud-trace/capacity-cohort.db \
  --plan tools/neocloud-trace/plans/research-capacity-depth-cohort-24h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  depth-cohort
```

Only one provider is active at a time. Cleanup must be verified before the next
scheduled epoch can start.

### Advance Capacity from Cron or Another Scheduler

The `capacity` command is the idempotent entry point for external schedulers.
The first invocation persists the complete schedule. Every invocation samples
the catalog, claims at most one due epoch, executes it through cleanup, and
exits.

```bash
neocloud-trace capacity \
  --experiment /path/to/capacity-experiment.json \
  --db data/neocloud-trace/capacity-experiment.db
```

Run exactly the same command on every scheduler invocation. Possible statuses
include:

- `idle`: no epoch is currently due;
- `busy`: another invocation owns the due epoch;
- `cleanup_blocked`: an earlier resource has not been verified deleted;
- `executed`: one epoch ran;
- `complete`: no scheduled epochs remain.

An external-scheduler experiment must contain exactly one `capacity_cohorts`
entry and no other active experiment type. Use one database per experiment.
The tool detects the collector hostname for this command, but `vantage_id` must
be set to a real value in the experiment file.

`capacity-tick` exposes the same mechanism with an explicit campaign ID and is
available for integrations that manage campaign identities themselves:

```bash
neocloud-trace \
  --db data/neocloud-trace/capacity.db \
  --plan /path/to/plan.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  capacity-tick --campaign-id capacity-2026-01 --index 0
```

## Lease Survival Monitoring

A matched canary starts two leases for the same provider and GPU cell:

- one interruptible lease;
- one on-demand control lease.

The tool records provider state, machine identity, runtime uptime, application
heartbeat, readiness times, terminal events, and verified cleanup.

Run one configured pair:

```bash
neocloud-trace \
  --db data/neocloud-trace/canary.db \
  --plan /path/to/plan.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  canary --index 0
```

Run a continuous replacement cohort:

```bash
neocloud-trace \
  --db data/neocloud-trace/survival.db \
  --plan tools/neocloud-trace/plans/research-runpod-survival-cohort-24h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  cohort
```

When a pair ends, the cohort cleans it and starts a replacement until the
deadline, pair limit, stop request, or budget limit is reached.

## Completion-Window Experiments

A completion-window cohort assigns provider/GPU cells to configured launch-time
arms. It records catalog state at release and decision time, the actual launch
delay, provisioning outcome, readiness SLA outcome, cost estimate, and cleanup
status.

```bash
neocloud-trace \
  --db data/neocloud-trace/completion-window.db \
  --plan tools/neocloud-trace/plans/research-completion-window-cohort-24h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  window-cohort
```

## On-Demand Control Cohorts

Use a control cohort when a provider does not expose a comparable interruptible
market. Each epoch runs one on-demand lease and records the same lifecycle and
readiness signals used by the survival exports.

```bash
neocloud-trace \
  --db data/neocloud-trace/lambda-control.db \
  --plan tools/neocloud-trace/plans/research-lambda-control-cohort-24h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  control-cohort
```

## Combined Plans

`pilot` executes the active work contained in a plan while host and catalog
sampling continue in background threads.

```bash
neocloud-trace \
  --db data/neocloud-trace/combined.db \
  --plan tools/neocloud-trace/plans/research-combined-pilot-4h.json \
  --host-id trace-host-01 \
  --vantage-id us-west-office \
  --enable-active \
  pilot
```

## Status, Cleanup, and Recovery

### Inspect a Run

`status` is read-only and does not require provider credentials:

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  status
```

Use `--run-id` to inspect an earlier run. Without it, the latest run is used.
The output includes run state, observation counts, budget exposure, and owned
resources.

### Clean Up a Stopped Run

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  --enable-active \
  cleanup
```

Cleanup sends delete requests and verifies that every resource is terminal or
missing. A failed verification leaves the resource in the owned-resource set
and blocks future active runs.

### Recover After Collector Loss

Use recovery when the collector exited without completing its normal shutdown
path:

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  --enable-active \
  recover
```

Recovery marks unfinished lease observations as collector loss before deleting
and verifying remote resources. This prevents collector downtime from being
reported as a provider interruption.

## Data Export

Export commands are read-only and do not require provider credentials. If
`--run-id` is omitted, they use the latest run in the database.

### Lease Survival Data

```bash
neocloud-trace \
  --db data/neocloud-trace/survival.db \
  export-survival \
  --output data/neocloud-trace/survival.csv
```

The CSV contains one row per lease, including pair establishment, readiness
times, pre-ready failures, post-ready interruption events, application
failures, exposure times, and censoring status.

### Capacity-Depth Data

```bash
neocloud-trace \
  --db data/neocloud-trace/capacity.db \
  export-depth \
  --output data/neocloud-trace/capacity-depth.csv
```

The CSV contains one row per scheduled epoch, including skipped epochs,
selection metadata, advertised availability, delivered depth, readiness,
failure class, retry count, cost estimate, and cleanup status.

Inspect a complete JSON payload for one capacity trial with:

```bash
neocloud-trace \
  --db data/neocloud-trace/capacity.db \
  show-depth-trial --trial-id TRIAL_ID
```

Without `--trial-id`, the latest capacity trial is shown.

### Completion-Window Data

```bash
neocloud-trace \
  --db data/neocloud-trace/completion-window.db \
  export-window \
  --output data/neocloud-trace/completion-window.csv
```

The CSV contains one row per opportunity, including assignment probability,
release and decision catalog snapshots, launch timing, provisioning outcome,
SLA indicators, cost estimate, and cleanup status.

Additional SQLite query examples are available in
`tools/neocloud-trace/sql/research-trace-queries.sql`.

## Billing Reconciliation

### Synchronize RunPod Billing

RunPod billing records can be fetched by Pod ID:

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  sync-billing --provider runpod
```

Use `--start-time` to override the default start time taken from the run.

### Import Provider Billing

Import normalized CSV or JSON billing data when a provider does not expose a
suitable API:

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  import-billing \
  --provider lambda \
  --source lambda-invoice-2026-08 \
  --input /path/to/lambda-invoice.csv
```

The expected CSV columns are shown in
`tools/neocloud-trace/billing-import-template.csv`.

### Reconcile Costs

```bash
neocloud-trace \
  --db data/neocloud-trace/trace.db \
  reconcile-cost
```

Billing rows with an exact provider resource ID are attributed to their
capacity replica or canary lease. Aggregate rows without a resource ID remain
unattributed rather than being distributed across experiments.

## Command Reference

Global options must appear before the command name.

| Command | Creates or deletes resources | Purpose |
| --- | --- | --- |
| `init` | No | Validate a plan and initialize SQLite |
| `passive` | No | Collect catalog and host observations |
| `capacity-once` | Yes | Run one static capacity trial |
| `capacity` | Yes | Advance an externally scheduled capacity experiment |
| `capacity-tick` | Yes | Advance a named capacity campaign |
| `canary` | Yes | Run one matched lease pair |
| `cohort` | Yes | Run a continuous matched lease cohort |
| `depth-cohort` | Yes | Run a scheduled capacity cohort in one process |
| `window-cohort` | Yes | Run a randomized completion-window cohort |
| `control-cohort` | Yes | Run an on-demand lifecycle control cohort |
| `pilot` | Yes | Run all static trials in a plan |
| `status` | No | Show run state and unresolved resources |
| `cleanup` | Deletes only | Clean and verify resources from a run |
| `recover` | Deletes only | Recover after collector loss |
| `export-survival` | No | Export lease survival data |
| `export-depth` | No | Export capacity-depth data |
| `show-depth-trial` | No | Show one detailed capacity trial |
| `export-window` | No | Export completion-window data |
| `sync-billing` | No | Fetch provider billing records |
| `import-billing` | No | Import billing CSV or JSON |
| `reconcile-cost` | No | Attribute billing to trace resources |

Run `neocloud-trace COMMAND --help` for command-specific options.

## Output and Storage

Commands print JSON results to standard output and errors to standard error.
Experiment state is stored in the SQLite database selected by `--db`.

The database includes:

- run configuration and identity;
- host and network health samples;
- provider API observations and catalog quotes;
- capacity schedules, trials, replicas, and readiness states;
- lease cohorts, leases, heartbeats, and terminal classifications;
- cleanup attempts and verified ownership state;
- budget commitments, settlements, billing records, and reconciliations.

Raw provider payloads are retained for auditability. Fields whose keys match
known secret names are redacted before persistence.

## Legacy Collector

The original standalone collector is retained under
`tools/neocloud-trace/legacy/` for existing databases and legacy workflows. New
collection and experiment work should use the `neocloud-trace` command
documented above.
