# NeoCloud Trace Development

This document contains the Python setup, direct module execution, testing, and
local verification workflow for NeoCloud Trace. User-facing commands and
experiment behavior are documented in `README.md`.

## Requirements

- Python 3.10 or newer;
- SQLite support in the Python standard library;
- `pytest` for the test suite;
- provider credentials only for manual provider integration tests or real runs.

The runtime package has no third-party Python dependencies.

## Editable Setup

From the repository root, create an isolated environment and install the package
in editable mode:

```bash
python3 -m venv .venv
source .venv/bin/activate
python3 -m pip install --upgrade pip
python3 -m pip install --editable tools/neocloud-trace
```

Install the development test dependency when it is not already available:

```bash
python3 -m pip install pytest
```

The console entry point should now resolve to the source checkout:

```bash
neocloud-trace --help
neocloud-trace run --help
```

## Run the Python Module Directly

The package uses a `src/` layout. To run it without installing, make `src` the
working directory:

```bash
cd tools/neocloud-trace/src
python3 -m neocloud_trace --help
python3 -m neocloud_trace run --help
```

Run a real command in the same way:

```bash
python3 -m neocloud_trace init \
  --plan ../plans/capacity-epoch.json \
  --db /tmp/neocloud-trace-dev.db \
  --vantage-id local-development
```

The normal installed form remains preferred for user documentation:

```bash
neocloud-trace init \
  --plan tools/neocloud-trace/plans/capacity-epoch.json \
  --db /tmp/neocloud-trace-dev.db \
  --vantage-id local-development
```

All command-specific options come after the command name.

## Run Tests

Run the full suite from the repository root:

```bash
python3 -m pytest tools/neocloud-trace/tests
```

Run one test module:

```bash
python3 -m pytest tools/neocloud-trace/tests/test_research_supervisor.py
```

Run one test by name:

```bash
python3 -m pytest \
  tools/neocloud-trace/tests/test_research_supervisor.py \
  -k metadata
```

The package's `pyproject.toml` configures `src` on the test path, so tests do not
depend on an editable install.

Provider clients are tested with fakes and mocked HTTP responses. The normal
unit suite must not require provider credentials or create cloud resources.

## Project Layout

```text
tools/neocloud-trace/
├── README.md                  user-facing installation and commands
├── DEVELOPMENT.md             local Python and test workflow
├── pyproject.toml             package and test configuration
├── plans/                     example experiment plans
├── sql/                       reusable analysis queries
├── src/neocloud_trace/
│   ├── cli.py                 argument parsing and command dispatch
│   ├── model.py               shared data models and invariants
│   ├── supervisor.py          experiment orchestration and recovery
│   ├── providers/             provider clients and normalized responses
│   ├── runners/               passive, capacity, and canary execution
│   ├── storage/               SQLite schema and persistence operations
│   └── analysis/              CSV export and billing reconciliation
├── tests/                     unit and workflow tests
```

## CLI Development

The public parser is built in `src/neocloud_trace/cli.py`.

When adding or changing a command:

1. place operational options on the command parser, not on the top-level parser;
2. require `--enable-active` for every path that can create or delete a provider
   resource;
3. load plan metadata through the shared host/vantage resolution path for
   plan-based commands;
4. keep stdout JSON stable and machine-readable;
5. add parser/help and execution tests;
6. update `README.md` when user-visible behavior changes.

Do not add a new top-level command merely to select another plan section. Prefer
a single-purpose plan and a shared execution path.

The active command contracts are intentionally narrow:

- `tick` accepts one `capacity_cohorts` entry with `max_trials=1`;
- `run` accepts one `canary_cohorts` entry and an optional top-level
  `stability_schedule`;
- both reject every other active section before opening SQLite or constructing
  provider clients.

## Storage and Data Changes

The schema is defined in `src/neocloud_trace/storage/schema.sql`; persistence
operations are implemented in `storage/store.py`. Opening a database runs the
idempotent schema statements and enables WAL mode.

For schema or exporter changes:

1. state the row grain and primary linkage before adding columns;
2. preserve scheduled and failed rows, not only successful observations;
3. keep provider-ready and application-ready fields separate;
4. keep instance counts, GPU depth, estimates, and provider bills explicit;
5. update fixture tests and user-facing documentation;
6. verify old databases or provide a deliberate migration path.

Stability has two distinct row grains. `stability_windows` is the scheduled
outer sampling unit and must include missed windows. `canary_epochs` is an
actually selected pair inside a window; `stability_window_epochs` provides the
explicit link. Do not derive the outer denominator from canary rows.

Campaign tests should cover schedule materialization, idempotent claims from two
SQLite connections, missed slots, recovery with future work preserved, cleanup
failure, and exports containing zero-observation scheduled rows.

Local databases, `-wal`/`-shm` files, exports, logs, and temporary results are
ignored by Git. Use a temporary or `data/neocloud-trace/` path for development
runs.

## Safe Manual Testing

Start with commands that cannot create resources:

```bash
neocloud-trace init \
  --plan /path/to/plan.json \
  --db /tmp/neocloud-trace-dev.db \
  --vantage-id local-development

neocloud-trace collect \
  --db /tmp/neocloud-trace-dev.db \
  --provider runpod \
  --once
```

Do not put real credentials in plans, fixtures, shell history examples, or test
output. Active manual tests require an explicit reviewed plan, budget, cleanup
procedure, and `--enable-active`.

After an active test, always inspect ownership:

```bash
neocloud-trace status --db /tmp/neocloud-trace-dev.db
```

A nonempty `owned_resources` list means the test is not finished.

## Final Verification

Before handing off a change:

```bash
python3 -m pytest tools/neocloud-trace/tests -q
git diff --check
```

For storage-resource changes, also build a wheel and verify that
`neocloud_trace/storage/schema.sql` is present in it.

Also inspect the public help for every changed command:

```bash
cd tools/neocloud-trace/src
python3 -m neocloud_trace --help
python3 -m neocloud_trace COMMAND --help
```

Review the Git diff and stage only NeoCloud Trace files that belong to the
change. Runtime databases, credentials, exports, logs, and unrelated workspace
files must remain untracked.
