from __future__ import annotations

import argparse
import dataclasses
import json
import pathlib
import random
import signal
import socket
import sys
from typing import Any

from .analysis import (
    capacity_trial_payload,
    export_capacity_depth_csv,
    export_completion_window_csv,
    export_stability_window_csv,
    export_survival_csv,
)
from .analysis.billing import import_billing_file, reconcile_costs
from .model import CollectionConfig
from .providers import LambdaResearchClient, RunPodResearchClient
from .runners import PassiveCollector
from .storage import TraceStore
from .supervisor import ActiveRunDisabled, PilotSupervisor, load_plan


ACTIVE_COMMANDS = {
    "run",
    "tick",
    "cleanup",
    "recover",
}
SUPPORTED_PROVIDERS = ("lambda", "runpod")
PASSIVE_PROTOCOL_VERSION = "1.0-passive-catalog"
ACTIVE_PLAN_SECTIONS = (
    "capacity_trials",
    "capacity_cohorts",
    "completion_window_cohorts",
    "canary_trials",
    "canary_cohorts",
    "control_cohorts",
)


def _add_database_argument(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--db", required=True, help="SQLite trace database")


def _add_plan_arguments(parser: argparse.ArgumentParser) -> None:
    _add_database_argument(parser)
    parser.add_argument("--plan", required=True, help="experiment plan JSON")
    parser.add_argument(
        "--host-id",
        help="collector identity (default: plan value or local hostname)",
    )
    parser.add_argument(
        "--vantage-id",
        help="network/location identity (default: plan value)",
    )


def _add_active_argument(parser: argparse.ArgumentParser) -> None:
    parser.add_argument(
        "--enable-active",
        action="store_true",
        help="allow billable create/delete API calls",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="neocloud-trace",
        description="Budget-bounded NeoCloud availability and capacity trace runner"
    )
    subparsers = parser.add_subparsers(
        dest="command",
        metavar="COMMAND",
        title="commands",
        required=True,
    )

    init = subparsers.add_parser(
        "init",
        help="validate a plan and initialize its database",
    )
    _add_plan_arguments(init)

    collect = subparsers.add_parser(
        "collect",
        help="collect passive catalog and host observations",
    )
    _add_database_argument(collect)
    collect.add_argument(
        "--provider",
        action="append",
        choices=SUPPORTED_PROVIDERS,
        required=True,
        help="provider to query; repeat to collect more than one",
    )
    collect.add_argument(
        "--vantage-id",
        help="network/location group (default: local hostname)",
    )
    collect.add_argument(
        "--gpu-count",
        action="append",
        type=int,
        dest="gpu_counts",
        help="GPU shape to query; repeat as needed (default: 1, 2, 4, 8)",
    )
    collect.add_argument(
        "--catalog-interval-seconds",
        type=float,
        default=300.0,
    )
    collect.add_argument(
        "--host-interval-seconds",
        type=float,
        default=30.0,
    )
    collect.add_argument("--network-control-url")
    collection_window = collect.add_mutually_exclusive_group()
    collection_window.add_argument("--duration-seconds", type=float)
    collection_window.add_argument(
        "--once",
        action="store_true",
        help="collect one snapshot and exit (also the default without a duration)",
    )

    run = subparsers.add_parser(
        "run",
        help="run one due stability sampling window",
    )
    _add_plan_arguments(run)
    _add_active_argument(run)
    run.add_argument(
        "--campaign-id",
        help="stable campaign identity (default: reuse the database campaign)",
    )

    tick = subparsers.add_parser(
        "tick",
        help="advance one due capacity-cohort epoch",
    )
    _add_plan_arguments(tick)
    _add_active_argument(tick)
    tick.add_argument(
        "--campaign-id",
        help="stable campaign identity (default: reuse the database campaign)",
    )

    status = subparsers.add_parser("status", help="show persisted run status")
    _add_database_argument(status)
    status.add_argument("--run-id")

    cleanup = subparsers.add_parser("cleanup", help="delete and verify owned resources")
    _add_database_argument(cleanup)
    _add_active_argument(cleanup)
    cleanup.add_argument("--run-id")

    recover = subparsers.add_parser(
        "recover",
        help="right-censor a lost collector run and verify remote cleanup",
    )
    _add_database_argument(recover)
    _add_active_argument(recover)
    recover.add_argument("--run-id")

    trial = subparsers.add_parser(
        "trial",
        help="show a detailed capacity-trial payload",
    )
    _add_database_argument(trial)
    trial.add_argument("--trial-id", help="default: most recent capacity trial")

    export = subparsers.add_parser(
        "export",
        help="export analysis-ready CSV data",
    )
    export_types = export.add_subparsers(
        dest="export_type",
        metavar="TYPE",
        required=True,
    )
    for export_type, help_text in (
        ("survival", "export one row per canary lease"),
        ("depth", "export one row per scheduled capacity epoch"),
        ("window", "export one row per completion-window opportunity"),
        ("stability", "export one row per scheduled stability window"),
    ):
        export_command = export_types.add_parser(export_type, help=help_text)
        _add_database_argument(export_command)
        export_command.add_argument("--run-id")
        export_command.add_argument("--output", required=True)

    billing = subparsers.add_parser(
        "billing",
        help="sync, import, or reconcile provider billing",
    )
    billing_actions = billing.add_subparsers(
        dest="billing_action",
        metavar="ACTION",
        required=True,
    )
    sync_billing = billing_actions.add_parser(
        "sync",
        help="fetch provider billing records",
    )
    _add_database_argument(sync_billing)
    sync_billing.add_argument("--run-id")
    sync_billing.add_argument(
        "--provider",
        choices=("runpod", "lambda"),
        required=True,
    )
    sync_billing.add_argument("--start-time")

    import_billing = billing_actions.add_parser(
        "import",
        help="import a provider invoice or usage file",
    )
    _add_database_argument(import_billing)
    import_billing.add_argument("--run-id")
    import_billing.add_argument("--provider", required=True)
    import_billing.add_argument("--source", required=True)
    import_billing.add_argument("--input", required=True)

    reconcile = billing_actions.add_parser(
        "reconcile",
        help="attribute billing records to trace resources",
    )
    _add_database_argument(reconcile)
    reconcile.add_argument("--run-id")
    return parser


def require_active(args: argparse.Namespace) -> None:
    if args.command in ACTIVE_COMMANDS and not args.enable_active:
        raise ActiveRunDisabled(f"{args.command} requires --enable-active")


def _provider_clients(names: set[str]) -> dict[str, object]:
    clients: dict[str, object] = {}
    if "lambda" in names:
        clients["lambda"] = LambdaResearchClient()
    if "runpod" in names:
        clients["runpod"] = RunPodResearchClient()
    unsupported = names - clients.keys()
    if unsupported:
        raise ValueError(f"unsupported providers: {', '.join(sorted(unsupported))}")
    return clients


def _is_placeholder(value: str) -> bool:
    return value.lower().startswith(("replace-", "unknown", "placeholder"))


def _load_cli_plan(
    path: str,
    *,
    host_id: str | None,
    vantage_id: str | None,
):
    detected_host = socket.gethostname()
    plan = load_plan(
        path,
        default_host_id=host_id or detected_host,
        default_vantage_id=vantage_id,
    )
    resolved_host_id = host_id or plan.experiment.host_id
    if _is_placeholder(resolved_host_id):
        resolved_host_id = detected_host
    resolved_vantage_id = vantage_id or plan.experiment.vantage_id
    experiment = dataclasses.replace(
        plan.experiment,
        host_id=resolved_host_id,
        vantage_id=resolved_vantage_id,
    )
    raw = dict(plan.raw_document)
    raw["host_id"] = experiment.host_id
    raw["vantage_id"] = experiment.vantage_id
    plan = dataclasses.replace(plan, experiment=experiment, raw_document=raw)
    _require_real_metadata(plan)
    return plan


def _capacity_campaign_id(store: TraceStore) -> str:
    rows = store.connection.execute(
        "SELECT campaign_id FROM capacity_campaigns ORDER BY created_at"
    ).fetchall()
    if len(rows) > 1:
        raise ValueError(
            "capacity database contains multiple experiments; use one database "
            "per experiment"
        )
    return str(rows[0]["campaign_id"]) if rows else "capacity-experiment"


def _stability_campaign_id(store: TraceStore) -> str:
    rows = store.connection.execute(
        "SELECT campaign_id FROM stability_campaigns ORDER BY created_at"
    ).fetchall()
    if len(rows) > 1:
        raise ValueError(
            "stability database contains multiple experiments; pass --campaign-id"
        )
    return str(rows[0]["campaign_id"]) if rows else "stability-experiment"


def _capacity_cohort_summary(cohort) -> dict[str, Any]:
    sequence = cohort.select_sequence(random.Random(cohort.selection_seed))
    provider_bounds: dict[str, float] = {}
    for trial, _selection_probability in sequence:
        provider_bounds[trial.cell.provider] = (
            provider_bounds.get(trial.cell.provider, 0.0)
            + trial.budget_upper_bound_usd
        )
    return {
        "name": cohort.name,
        "max_trials": cohort.max_trials,
        "min_trial_upper_bound_usd": round(
            min(cell.budget_upper_bound_usd for cell in cohort.cells),
            6,
        ),
        "max_trial_upper_bound_usd": round(
            max(cell.budget_upper_bound_usd for cell in cohort.cells),
            6,
        ),
        "planned_cohort_upper_bound_usd": round(
            sum(provider_bounds.values()),
            6,
        ),
        "planned_provider_upper_bounds_usd": {
            provider: round(amount, 6)
            for provider, amount in sorted(provider_bounds.items())
        },
    }


def _require_real_metadata(plan) -> None:
    if _is_placeholder(plan.experiment.host_id):
        raise ValueError("host_id must identify the collector host")
    if _is_placeholder(plan.experiment.vantage_id):
        raise ValueError("vantage_id must identify the collector network/location")


def _install_signal_handlers(supervisor: PilotSupervisor):
    previous = {}

    def stop(_signum, _frame):
        supervisor.request_stop()

    for signum in (signal.SIGINT, signal.SIGTERM):
        previous[signum] = signal.signal(signum, stop)
    return previous


def _restore_signal_handlers(previous) -> None:
    for signum, handler in previous.items():
        signal.signal(signum, handler)


def _active_section_counts(plan) -> dict[str, int]:
    return {
        section: len(getattr(plan, section))
        for section in ACTIVE_PLAN_SECTIONS
        if getattr(plan, section)
    }


def _validate_tick_plan(plan) -> None:
    sections = _active_section_counts(plan)
    if sections != {"capacity_cohorts": 1}:
        found = ", ".join(
            f"{section}={count}" for section, count in sections.items()
        ) or "no active sections"
        raise ValueError(
            "tick requires exactly one capacity_cohorts entry and no other "
            f"active sections; found {found}"
        )
    cohort = plan.capacity_cohorts[0]
    if cohort.max_trials != 1:
        raise ValueError("tick requires capacity_cohorts[0].max_trials=1")


def _validate_run_plan(plan) -> None:
    sections = _active_section_counts(plan)
    if sections != {"canary_cohorts": 1}:
        found = ", ".join(
            f"{section}={count}" for section, count in sections.items()
        ) or "no active sections"
        raise ValueError(
            "run requires exactly one canary_cohorts entry and no other "
            f"active sections; found {found}"
        )
def _run_capacity_tick(
    store: TraceStore,
    plan,
    *,
    campaign_id: str,
    enable_active: bool,
) -> dict[str, Any]:
    cohort = plan.capacity_cohorts[0]
    providers = _provider_clients({trial.cell.provider for trial in cohort.cells})
    supervisor = PilotSupervisor(store, providers)
    previous = _install_signal_handlers(supervisor)
    try:
        return supervisor.capacity_tick(
            plan,
            campaign_id=campaign_id,
            enable_active=enable_active,
        )
    finally:
        _restore_signal_handlers(previous)


def _run_stability_window(
    store: TraceStore,
    plan,
    *,
    campaign_id: str,
    enable_active: bool,
) -> dict[str, Any]:
    cohort = plan.canary_cohorts[0]
    providers = _provider_clients({trial.cell.provider for trial in cohort.cells})
    supervisor = PilotSupervisor(store, providers)
    previous = _install_signal_handlers(supervisor)
    try:
        return supervisor.stability_run(
            plan,
            campaign_id=campaign_id,
            enable_active=enable_active,
        )
    finally:
        _restore_signal_handlers(previous)


def _collection_config(args: argparse.Namespace) -> CollectionConfig:
    host_id = socket.gethostname()
    providers = tuple(dict.fromkeys(args.provider))
    return CollectionConfig(
        protocol_version=PASSIVE_PROTOCOL_VERSION,
        host_id=host_id,
        vantage_id=args.vantage_id or host_id,
        providers=providers,
        gpu_counts=tuple(args.gpu_counts or (1, 2, 4, 8)),
        catalog_interval_seconds=args.catalog_interval_seconds,
        host_interval_seconds=args.host_interval_seconds,
        duration_seconds=args.duration_seconds,
        network_control_url=args.network_control_url,
    )


def _collect(
    store: TraceStore,
    args: argparse.Namespace,
    config: CollectionConfig,
) -> dict[str, Any]:
    clients = _provider_clients(set(config.providers))
    collector = PassiveCollector(store, clients.values())
    previous = _install_signal_handlers(collector)
    try:
        run_id = collector.run(
            config,
            once=args.once or args.duration_seconds is None,
        )
    finally:
        _restore_signal_handlers(previous)
    return _status(store, run_id)


def _status(store: TraceStore, run_id: str | None) -> dict[str, Any]:
    selected = run_id or store.latest_run_id()
    if not selected:
        return {"run_id": None, "status": "empty"}
    run = dict(store.run_row(selected))
    counts = {}
    for table in (
        "host_observations",
        "provider_api_observations_v2",
        "catalog_quotes_v2",
        "capacity_trials",
        "capacity_epochs",
        "stability_windows",
        "completion_window_epochs",
        "capacity_replicas",
        "canary_leases",
        "canary_epochs",
        "canary_observations",
        "cleanup_events",
        "provider_billing_records",
        "cost_reconciliations",
    ):
        if table in ("capacity_replicas",):
            query = (
                "SELECT COUNT(*) FROM capacity_replicas r "
                "JOIN capacity_trials t ON t.id=r.trial_id WHERE t.run_id=?"
            )
        elif table == "canary_observations":
            query = (
                "SELECT COUNT(*) FROM canary_observations o "
                "JOIN canary_leases l ON l.id=o.lease_id WHERE l.run_id=?"
            )
        else:
            query = f"SELECT COUNT(*) FROM {table} WHERE run_id=?"
        counts[table] = store.connection.execute(query, (selected,)).fetchone()[0]
    exposure = {
        row["provider"]: row["amount"]
        for row in store.connection.execute(
            "SELECT provider, SUM(amount_usd) amount FROM budget_ledger_v2 "
            "WHERE run_id=? GROUP BY provider",
            (selected,),
        ).fetchall()
    }
    return {
        "run_id": selected,
        "status": run["status"],
        "started_at": run["started_at"],
        "finished_at": run["finished_at"],
        "termination_reason": run["termination_reason"],
        "budget_exposure_usd": exposure,
        "counts": counts,
        "owned_resources": [
            dataclasses.asdict(item) for item in store.owned_resources(selected)
        ],
    }


def _latest_run_id(store: TraceStore, action: str) -> str:
    run_id = store.latest_run_id()
    if not run_id:
        raise ValueError(f"no run exists to {action}")
    return run_id


def _export_data(store: TraceStore, args: argparse.Namespace) -> dict[str, Any]:
    run_id = args.run_id or _latest_run_id(store, "export")
    exporters = {
        "survival": export_survival_csv,
        "depth": export_capacity_depth_csv,
        "window": export_completion_window_csv,
        "stability": export_stability_window_csv,
    }
    count = exporters[args.export_type](store, run_id, args.output)
    return {
        "run_id": run_id,
        "output": str(pathlib.Path(args.output).resolve()),
        "rows": count,
    }


def _trial_payload(store: TraceStore, trial_id: str | None) -> dict[str, Any]:
    if not trial_id:
        row = store.connection.execute(
            "SELECT id FROM capacity_trials "
            "ORDER BY started_at DESC, rowid DESC LIMIT 1"
        ).fetchone()
        trial_id = row["id"] if row else None
    if not trial_id:
        raise ValueError("no capacity trial exists")
    return capacity_trial_payload(store, trial_id)


def _sync_billing(store: TraceStore, args: argparse.Namespace) -> dict[str, Any]:
    run_id = args.run_id or _latest_run_id(store, "sync")
    run = store.run_row(run_id)
    if args.provider == "lambda":
        raise ValueError(
            "Lambda has no public usage API; use billing import with a "
            "console or invoice export"
        )
    resource_ids = {
        row[0]
        for row in store.connection.execute(
            """
            SELECT r.provider_resource_id
            FROM capacity_replicas r
            JOIN capacity_trials t ON t.id=r.trial_id
            WHERE t.run_id=? AND t.provider=?
              AND r.provider_resource_id IS NOT NULL
            UNION
            SELECT provider_resource_id
            FROM canary_leases
            WHERE run_id=? AND provider=?
              AND provider_resource_id IS NOT NULL
            """,
            (run_id, args.provider, run_id, args.provider),
        ).fetchall()
    }
    before = len(store.provider_billing_rows(run_id))
    client = _provider_clients({args.provider})[args.provider]
    records = client.billing_records(
        start_time=args.start_time or run["started_at"],
        resource_ids=tuple(sorted(resource_ids)),
    )
    seen_calls: set[str] = set()
    for record in records:
        store.record_provider_billing(run_id, record)
        call = record.api_call
        if call is None or call.id in seen_calls:
            continue
        seen_calls.add(call.id)
        store.record_api_observation(
            run_id,
            provider=args.provider,
            operation=call.operation,
            status=str(call.status),
            latency_ms=call.latency_ms,
            raw=call.raw,
            failure_class=(call.failure_class.value if call.failure_class else None),
        )
    after = len(store.provider_billing_rows(run_id))
    return {
        "run_id": run_id,
        "provider": args.provider,
        "resource_count": len(resource_ids),
        "records_seen": len(records),
        "records_imported": after - before,
    }


def _billing(store: TraceStore, args: argparse.Namespace) -> dict[str, Any]:
    if args.billing_action == "sync":
        return _sync_billing(store, args)
    if args.billing_action == "import":
        run_id = args.run_id or _latest_run_id(store, "import into")
        imported = import_billing_file(
            store,
            run_id,
            provider=args.provider,
            path=args.input,
            source=args.source,
        )
        return {
            "run_id": run_id,
            "provider": args.provider,
            "records_imported": imported,
        }
    run_id = args.run_id or _latest_run_id(store, "reconcile")
    rows = reconcile_costs(store, run_id)
    return {
        "run_id": run_id,
        "resources": len(rows),
        "reconciled": sum(row["status"] == "reconciled" for row in rows),
        "missing_billing": sum(row["status"] == "missing_billing" for row in rows),
        "unallocated_billing": sum(
            row["status"] == "unallocated_billing" for row in rows
        ),
        "aggregate_unattributed": sum(
            row["status"] == "aggregate_unattributed" for row in rows
        ),
        "estimated_amount_usd": sum(
            float(row["estimated_amount_usd"] or 0) for row in rows
        ),
        "billed_amount_usd": sum(
            float(row["billed_amount_usd"] or 0) for row in rows
        ),
    }


def _plan_summary(plan, db_path: str) -> dict[str, Any]:
    static_upper_bound = sum(
        trial.budget_upper_bound_usd
        for trial in (*plan.capacity_trials, *plan.canary_trials)
    )
    return {
        "database": str(pathlib.Path(db_path).resolve()),
        "protocol_version": plan.experiment.protocol_version,
        "host_id": plan.experiment.host_id,
        "vantage_id": plan.experiment.vantage_id,
        "capacity_trials": len(plan.capacity_trials),
        "capacity_cohort_count": len(plan.capacity_cohorts),
        "capacity_cohorts": [
            _capacity_cohort_summary(cohort) for cohort in plan.capacity_cohorts
        ],
        "completion_window_cohort_count": len(plan.completion_window_cohorts),
        "completion_window_cohorts": [
            {
                "name": cohort.name,
                "max_opportunities": cohort.max_opportunities,
                "arms": [arm.name for arm in cohort.arms],
                "max_wait_seconds": cohort.maximum_wait_seconds,
                "min_trial_upper_bound_usd": round(
                    min(cell.budget_upper_bound_usd for cell in cohort.cells),
                    6,
                ),
                "max_trial_upper_bound_usd": round(
                    max(cell.budget_upper_bound_usd for cell in cohort.cells),
                    6,
                ),
            }
            for cohort in plan.completion_window_cohorts
        ],
        "control_cohort_count": len(plan.control_cohorts),
        "control_cohorts": [
            {
                "name": cohort.name,
                "max_leases": cohort.max_leases,
                "min_lease_upper_bound_usd": round(
                    min(cell.budget_upper_bound_usd for cell in cohort.cells),
                    6,
                ),
                "max_lease_upper_bound_usd": round(
                    max(cell.budget_upper_bound_usd for cell in cohort.cells),
                    6,
                ),
            }
            for cohort in plan.control_cohorts
        ],
        "canary_trials": len(plan.canary_trials),
        "canary_cohort_count": len(plan.canary_cohorts),
        "stability_schedule": {
            "window_count": plan.stability_schedule.window_count,
            "schedule_seed": plan.stability_schedule.schedule_seed,
            "initial_delay_seconds": plan.stability_schedule.initial_delay_seconds,
            "interval_seconds": plan.stability_schedule.interval_seconds,
            "jitter_seconds": plan.stability_schedule.jitter_seconds,
            "scheduled_offsets_seconds": list(plan.stability_schedule.offsets()),
        },
        "canary_cohorts": [
            {
                "name": cohort.name,
                "max_pairs": cohort.max_pairs,
                "min_pair_upper_bound_usd": round(
                    min(cell.budget_upper_bound_usd for cell in cohort.cells),
                    6,
                ),
                "max_pair_upper_bound_usd": round(
                    max(cell.budget_upper_bound_usd for cell in cohort.cells),
                    6,
                ),
            }
            for cohort in plan.canary_cohorts
        ],
        "static_planned_upper_bound_usd": static_upper_bound,
        "provider_budgets_usd": plan.experiment.provider_budgets,
        "total_budget_usd": plan.experiment.total_budget_usd,
    }


def _cleanup_or_recover(
    store: TraceStore,
    args: argparse.Namespace,
) -> dict[str, Any]:
    action = "clean" if args.command == "cleanup" else "recover"
    run_id = args.run_id or _latest_run_id(store, action)
    providers = _provider_clients(
        {item.provider for item in store.owned_resources(run_id)}
    )
    supervisor = PilotSupervisor(store, providers)
    if args.command == "cleanup":
        remaining = supervisor.cleanup_run(run_id)
        result: dict[str, Any] = {"run_id": run_id}
    else:
        remaining = supervisor.recover_run(run_id)
        result = {"run_id": run_id, "status": store.run_row(run_id)["status"]}
    result["remaining_owned_resources"] = [
        dataclasses.asdict(item) for item in remaining
    ]
    return result


def execute(args: argparse.Namespace) -> dict[str, Any]:
    require_active(args)
    plan = None
    collection_config = None
    if args.command in {"init", "tick", "run"}:
        plan = _load_cli_plan(
            args.plan,
            host_id=args.host_id,
            vantage_id=args.vantage_id,
        )
        if args.command == "tick":
            _validate_tick_plan(plan)
        elif args.command == "run":
            _validate_run_plan(plan)
    elif args.command == "collect":
        collection_config = _collection_config(args)

    store = TraceStore(args.db)
    try:
        if args.command == "status":
            return _status(store, args.run_id)
        if args.command in {"cleanup", "recover"}:
            return _cleanup_or_recover(store, args)
        if args.command == "trial":
            return _trial_payload(store, args.trial_id)
        if args.command == "export":
            return _export_data(store, args)
        if args.command == "billing":
            return _billing(store, args)
        if args.command == "collect":
            assert collection_config is not None
            return _collect(store, args, collection_config)

        assert plan is not None
        if args.command == "init":
            return _plan_summary(plan, args.db)
        if args.command == "tick":
            return _run_capacity_tick(
                store,
                plan,
                campaign_id=args.campaign_id or _capacity_campaign_id(store),
                enable_active=args.enable_active,
            )
        if args.command == "run":
            return _run_stability_window(
                store,
                plan,
                campaign_id=args.campaign_id or _stability_campaign_id(store),
                enable_active=args.enable_active,
            )
        raise ValueError(f"unsupported command: {args.command}")
    finally:
        store.close()


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        result = execute(args)
    except (ActiveRunDisabled, ValueError, RuntimeError) as error:
        print(json.dumps({"error": f"{type(error).__name__}: {error}"}), file=sys.stderr)
        return 2
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
