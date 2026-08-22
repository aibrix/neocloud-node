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
    export_survival_csv,
)
from .analysis.billing import import_billing_file, reconcile_costs
from .providers import LambdaResearchClient, RunPodResearchClient
from .storage import ResearchStore
from .supervisor import ActiveRunDisabled, PilotSupervisor, load_plan


PROJECT_ROOT = pathlib.Path(__file__).resolve().parents[2]
DEFAULT_PLAN = PROJECT_ROOT / "plans" / "research-pilot-3h.json"
ACTIVE_COMMANDS = {
    "capacity-once",
    "capacity-tick",
    "canary",
    "cohort",
    "depth-cohort",
    "window-cohort",
    "control-cohort",
    "pilot",
    "cleanup",
    "recover",
}


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Budget-bounded NeoCloud availability and capacity trace runner"
    )
    parser.add_argument("--db", help="SQLite trace database")
    parser.add_argument("--plan", default=str(DEFAULT_PLAN), help="research protocol JSON")
    parser.add_argument("--host-id", help="override the collector host identity")
    parser.add_argument("--vantage-id", help="override the network/location identity")
    parser.add_argument(
        "--enable-active",
        action="store_true",
        help="allow billable create/delete API calls",
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    capacity_experiment = subparsers.add_parser(
        "capacity",
        help="atomically advance one capacity experiment",
    )
    capacity_experiment.add_argument(
        "--experiment",
        required=True,
        help="capacity experiment JSON",
    )
    capacity_experiment.add_argument(
        "--db",
        dest="capacity_db",
        required=True,
        help="SQLite trace database",
    )
    subparsers.add_parser("init", help="validate the plan and initialize the database")
    passive = subparsers.add_parser("passive", help="collect passive catalog and host trace")
    passive.add_argument("--duration-seconds", type=float)
    passive.add_argument("--once", action="store_true")
    capacity = subparsers.add_parser("capacity-once", help="run one configured depth ladder")
    capacity.add_argument("--index", type=int, default=0)
    capacity_tick = subparsers.add_parser(
        "capacity-tick",
        help="initialize or advance one externally scheduled capacity campaign",
    )
    capacity_tick.add_argument("--campaign-id", required=True)
    capacity_tick.add_argument("--index", type=int, default=0)
    canary = subparsers.add_parser("canary", help="run one configured matched canary pair")
    canary.add_argument("--index", type=int, default=0)
    cohort = subparsers.add_parser("cohort", help="run one continuous replacement cohort")
    cohort.add_argument("--index", type=int, default=0)
    depth_cohort = subparsers.add_parser(
        "depth-cohort",
        help="run one pre-scheduled capacity-depth cohort",
    )
    depth_cohort.add_argument("--index", type=int, default=0)
    window_cohort = subparsers.add_parser(
        "window-cohort",
        help="run one randomized completion-window cohort",
    )
    window_cohort.add_argument("--index", type=int, default=0)
    control_cohort = subparsers.add_parser(
        "control-cohort",
        help="run one cross-provider on-demand lifecycle control cohort",
    )
    control_cohort.add_argument("--index", type=int, default=0)
    subparsers.add_parser("pilot", help="run the complete bounded protocol")
    status = subparsers.add_parser("status", help="show persisted run status")
    status.add_argument("--run-id")
    cleanup = subparsers.add_parser("cleanup", help="delete and verify owned resources")
    cleanup.add_argument("--run-id")
    recover = subparsers.add_parser(
        "recover",
        help="right-censor a lost collector run and verify remote cleanup",
    )
    recover.add_argument("--run-id")
    export = subparsers.add_parser(
        "export-survival",
        help="export one analysis-ready row per canary lease",
    )
    export.add_argument("--run-id")
    export.add_argument("--output", required=True)
    depth_export = subparsers.add_parser(
        "export-depth",
        help="export one analysis-ready row per scheduled depth epoch",
    )
    depth_export.add_argument("--run-id")
    depth_export.add_argument("--output", required=True)
    depth_trial = subparsers.add_parser(
        "show-depth-trial",
        help="show one detailed capacity trial payload as JSON",
    )
    depth_trial.add_argument("--trial-id")
    window_export = subparsers.add_parser(
        "export-window",
        help="export one analysis-ready row per completion-window opportunity",
    )
    window_export.add_argument("--run-id")
    window_export.add_argument("--output", required=True)
    sync_billing = subparsers.add_parser(
        "sync-billing",
        help="sync resource-attributable provider billing records",
    )
    sync_billing.add_argument("--run-id")
    sync_billing.add_argument("--provider", choices=("runpod", "lambda"), required=True)
    sync_billing.add_argument("--start-time")
    import_billing = subparsers.add_parser(
        "import-billing",
        help="import a provider invoice or usage CSV/JSON snapshot",
    )
    import_billing.add_argument("--run-id")
    import_billing.add_argument("--provider", required=True)
    import_billing.add_argument("--source", required=True)
    import_billing.add_argument("--input", required=True)
    reconcile = subparsers.add_parser(
        "reconcile-cost",
        help="attribute provider billing records to trace resources",
    )
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


def _plan_providers(plan) -> set[str]:
    return set(plan.experiment.provider_budgets)


def _apply_metadata_overrides(plan, args: argparse.Namespace):
    experiment = dataclasses.replace(
        plan.experiment,
        host_id=args.host_id or plan.experiment.host_id,
        vantage_id=args.vantage_id or plan.experiment.vantage_id,
    )
    raw = dict(plan.raw_document)
    raw["host_id"] = experiment.host_id
    raw["vantage_id"] = experiment.vantage_id
    return dataclasses.replace(plan, experiment=experiment, raw_document=raw)


def _is_placeholder(value: str) -> bool:
    return value.lower().startswith(("replace-", "unknown", "placeholder"))


def _load_capacity_experiment(path: str):
    detected_host = socket.gethostname()
    plan = load_plan(path, default_host_id=detected_host)
    host_id = plan.experiment.host_id
    if _is_placeholder(host_id):
        experiment = dataclasses.replace(plan.experiment, host_id=detected_host)
        raw = dict(plan.raw_document)
        raw["host_id"] = detected_host
        plan = dataclasses.replace(plan, experiment=experiment, raw_document=raw)
    return plan


def _capacity_campaign_id(store: ResearchStore) -> str:
    rows = store.connection.execute(
        "SELECT campaign_id FROM capacity_campaigns ORDER BY created_at"
    ).fetchall()
    if len(rows) > 1:
        raise ValueError(
            "capacity database contains multiple experiments; use one database "
            "per experiment"
        )
    return str(rows[0]["campaign_id"]) if rows else "capacity-experiment"


def _run_capacity_experiment(
    store: ResearchStore,
    experiment_path: str,
) -> dict[str, Any]:
    plan = _load_capacity_experiment(experiment_path)
    if len(plan.capacity_cohorts) != 1:
        raise ValueError("capacity experiment requires exactly one capacity cohort")
    if (
        plan.capacity_trials
        or plan.completion_window_cohorts
        or plan.canary_trials
        or plan.canary_cohorts
        or plan.control_cohorts
    ):
        raise ValueError(
            "capacity experiment cannot contain other active experiment types"
        )
    _require_real_metadata(plan, "capacity-tick")
    result = _run_capacity_tick(
        store,
        plan,
        campaign_id=_capacity_campaign_id(store),
        cohort_index=0,
        enable_active=True,
    )
    public = dict(result)
    public.pop("campaign_id", None)
    if "campaign_created" in public:
        public["experiment_initialized"] = public.pop("campaign_created")
    if "campaign_complete" in public:
        public["experiment_complete"] = public.pop("campaign_complete")
    if "cohort_name" in public:
        public["experiment_name"] = public.pop("cohort_name")
    public["experiment"] = str(pathlib.Path(experiment_path).resolve())
    return public


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


def _require_real_metadata(plan, command: str) -> None:
    if command not in {
        "capacity-once",
        "capacity-tick",
        "canary",
        "cohort",
        "depth-cohort",
        "window-cohort",
        "control-cohort",
        "pilot",
    }:
        return
    placeholders = ("replace-", "unknown", "placeholder")
    if plan.experiment.host_id.lower().startswith(placeholders):
        raise ValueError("active runs require a real --host-id")
    if plan.experiment.vantage_id.lower().startswith(placeholders):
        raise ValueError("active runs require a real --vantage-id")


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


def _run_supervisor(store: ResearchStore, plan, *, enable_active: bool) -> str:
    providers = _provider_clients(_plan_providers(plan))
    supervisor = PilotSupervisor(store, providers)
    previous = _install_signal_handlers(supervisor)
    try:
        return supervisor.run(plan, enable_active=enable_active)
    finally:
        _restore_signal_handlers(previous)


def _run_capacity_tick(
    store: ResearchStore,
    plan,
    *,
    campaign_id: str,
    cohort_index: int,
    enable_active: bool,
) -> dict[str, Any]:
    try:
        cohort = plan.capacity_cohorts[cohort_index]
    except IndexError as error:
        raise ValueError("capacity cohort index is out of range") from error
    providers = _provider_clients({trial.cell.provider for trial in cohort.cells})
    supervisor = PilotSupervisor(store, providers)
    previous = _install_signal_handlers(supervisor)
    try:
        return supervisor.capacity_tick(
            plan,
            campaign_id=campaign_id,
            cohort_index=cohort_index,
            enable_active=enable_active,
        )
    finally:
        _restore_signal_handlers(previous)


def _status(store: ResearchStore, run_id: str | None) -> dict[str, Any]:
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


def execute(args: argparse.Namespace) -> dict[str, Any]:
    require_active(args)
    db_path = (
        args.capacity_db
        if args.command == "capacity"
        else args.db
    )
    if not db_path:
        raise ValueError("--db is required")
    store = ResearchStore(db_path)
    try:
        if args.command == "capacity":
            return _run_capacity_experiment(store, args.experiment)
        if args.command == "status":
            return _status(store, args.run_id)
        if args.command == "cleanup":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to clean")
            providers = _provider_clients(
                {item.provider for item in store.owned_resources(run_id)}
            )
            supervisor = PilotSupervisor(store, providers)
            remaining = supervisor.cleanup_run(run_id)
            return {
                "run_id": run_id,
                "remaining_owned_resources": [dataclasses.asdict(item) for item in remaining],
            }
        if args.command == "recover":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to recover")
            providers = _provider_clients(
                {item.provider for item in store.owned_resources(run_id)}
            )
            supervisor = PilotSupervisor(store, providers)
            remaining = supervisor.recover_run(run_id)
            return {
                "run_id": run_id,
                "status": store.run_row(run_id)["status"],
                "remaining_owned_resources": [
                    dataclasses.asdict(item) for item in remaining
                ],
            }
        if args.command == "export-survival":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to export")
            count = export_survival_csv(store, run_id, args.output)
            return {
                "run_id": run_id,
                "output": str(pathlib.Path(args.output).resolve()),
                "rows": count,
            }
        if args.command == "export-depth":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to export")
            count = export_capacity_depth_csv(store, run_id, args.output)
            return {
                "run_id": run_id,
                "output": str(pathlib.Path(args.output).resolve()),
                "rows": count,
            }
        if args.command == "show-depth-trial":
            trial_id = args.trial_id
            if not trial_id:
                row = store.connection.execute(
                    "SELECT id FROM capacity_trials ORDER BY started_at DESC, rowid DESC LIMIT 1"
                ).fetchone()
                trial_id = row["id"] if row else None
            if not trial_id:
                raise ValueError("no capacity trial exists")
            return capacity_trial_payload(store, trial_id)
        if args.command == "export-window":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to export")
            count = export_completion_window_csv(store, run_id, args.output)
            return {
                "run_id": run_id,
                "output": str(pathlib.Path(args.output).resolve()),
                "rows": count,
            }
        if args.command == "sync-billing":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to sync")
            run = store.run_row(run_id)
            if args.provider == "lambda":
                raise ValueError(
                    "Lambda has no public usage API; use import-billing with a "
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
                    failure_class=(
                        call.failure_class.value if call.failure_class else None
                    ),
                )
            after = len(store.provider_billing_rows(run_id))
            return {
                "run_id": run_id,
                "provider": args.provider,
                "resource_count": len(resource_ids),
                "records_seen": len(records),
                "records_imported": after - before,
            }
        if args.command == "import-billing":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to import into")
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
        if args.command == "reconcile-cost":
            run_id = args.run_id or store.latest_run_id()
            if not run_id:
                raise ValueError("no run exists to reconcile")
            rows = reconcile_costs(store, run_id)
            return {
                "run_id": run_id,
                "resources": len(rows),
                "reconciled": sum(row["status"] == "reconciled" for row in rows),
                "missing_billing": sum(
                    row["status"] == "missing_billing" for row in rows
                ),
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

        plan = _apply_metadata_overrides(load_plan(args.plan), args)
        _require_real_metadata(plan, args.command)
        if args.command == "capacity-tick":
            return _run_capacity_tick(
                store,
                plan,
                campaign_id=args.campaign_id,
                cohort_index=args.index,
                enable_active=args.enable_active,
            )
        if args.command == "init":
            static_upper_bound = sum(
                trial.budget_upper_bound_usd
                for trial in (*plan.capacity_trials, *plan.canary_trials)
            )
            return {
                "database": str(pathlib.Path(args.db).resolve()),
                "protocol_version": plan.experiment.protocol_version,
                "capacity_trials": len(plan.capacity_trials),
                "capacity_cohort_count": len(plan.capacity_cohorts),
                "capacity_cohorts": [
                    _capacity_cohort_summary(cohort)
                    for cohort in plan.capacity_cohorts
                ],
                "completion_window_cohort_count": len(
                    plan.completion_window_cohorts
                ),
                "completion_window_cohorts": [
                    {
                        "name": cohort.name,
                        "max_opportunities": cohort.max_opportunities,
                        "arms": [arm.name for arm in cohort.arms],
                        "max_wait_seconds": cohort.maximum_wait_seconds,
                        "min_trial_upper_bound_usd": round(
                            min(
                                cell.budget_upper_bound_usd
                                for cell in cohort.cells
                            ),
                            6,
                        ),
                        "max_trial_upper_bound_usd": round(
                            max(
                                cell.budget_upper_bound_usd
                                for cell in cohort.cells
                            ),
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
                            min(
                                cell.budget_upper_bound_usd
                                for cell in cohort.cells
                            ),
                            6,
                        ),
                        "max_lease_upper_bound_usd": round(
                            max(
                                cell.budget_upper_bound_usd
                                for cell in cohort.cells
                            ),
                            6,
                        ),
                    }
                    for cohort in plan.control_cohorts
                ],
                "canary_trials": len(plan.canary_trials),
                "canary_cohort_count": len(plan.canary_cohorts),
                "canary_cohorts": [
                    {
                        "name": cohort.name,
                        "max_pairs": cohort.max_pairs,
                        "min_pair_upper_bound_usd": round(
                            min(
                                cell.budget_upper_bound_usd
                                for cell in cohort.cells
                            ),
                            6,
                        ),
                        "max_pair_upper_bound_usd": round(
                            max(
                                cell.budget_upper_bound_usd
                                for cell in cohort.cells
                            ),
                            6,
                        ),
                    }
                    for cohort in plan.canary_cohorts
                ],
                "static_planned_upper_bound_usd": static_upper_bound,
                "provider_budgets_usd": plan.experiment.provider_budgets,
                "total_budget_usd": plan.experiment.total_budget_usd,
            }
        if args.command == "passive":
            duration = (
                0.01
                if args.once
                else args.duration_seconds or plan.duration_seconds
            )
            selected = dataclasses.replace(
                plan,
                duration_seconds=duration,
                capacity_trials=(),
                capacity_cohorts=(),
                completion_window_cohorts=(),
                canary_trials=(),
                canary_cohorts=(),
                control_cohorts=(),
            )
        elif args.command == "capacity-once":
            try:
                trial = plan.capacity_trials[args.index]
            except IndexError as error:
                raise ValueError("capacity trial index is out of range") from error
            selected = dataclasses.replace(
                plan,
                duration_seconds=1.0,
                capacity_trials=(trial,),
                capacity_cohorts=(),
                completion_window_cohorts=(),
                canary_trials=(),
                canary_cohorts=(),
                control_cohorts=(),
            )
        elif args.command == "canary":
            try:
                trial = plan.canary_trials[args.index]
            except IndexError as error:
                raise ValueError("canary trial index is out of range") from error
            selected = dataclasses.replace(
                plan,
                duration_seconds=trial.duration_seconds,
                capacity_trials=(),
                capacity_cohorts=(),
                completion_window_cohorts=(),
                canary_trials=(trial,),
                canary_cohorts=(),
                control_cohorts=(),
            )
        elif args.command == "cohort":
            try:
                cohort = plan.canary_cohorts[args.index]
            except IndexError as error:
                raise ValueError("canary cohort index is out of range") from error
            selected = dataclasses.replace(
                plan,
                capacity_trials=(),
                capacity_cohorts=(),
                completion_window_cohorts=(),
                canary_trials=(),
                canary_cohorts=(cohort,),
                control_cohorts=(),
            )
        elif args.command == "depth-cohort":
            try:
                cohort = plan.capacity_cohorts[args.index]
            except IndexError as error:
                raise ValueError("capacity cohort index is out of range") from error
            selected = dataclasses.replace(
                plan,
                capacity_trials=(),
                capacity_cohorts=(cohort,),
                completion_window_cohorts=(),
                canary_trials=(),
                canary_cohorts=(),
                control_cohorts=(),
            )
        elif args.command == "window-cohort":
            try:
                cohort = plan.completion_window_cohorts[args.index]
            except IndexError as error:
                raise ValueError(
                    "completion-window cohort index is out of range"
                ) from error
            selected = dataclasses.replace(
                plan,
                capacity_trials=(),
                capacity_cohorts=(),
                completion_window_cohorts=(cohort,),
                canary_trials=(),
                canary_cohorts=(),
                control_cohorts=(),
            )
        elif args.command == "control-cohort":
            try:
                cohort = plan.control_cohorts[args.index]
            except IndexError as error:
                raise ValueError("control cohort index is out of range") from error
            selected = dataclasses.replace(
                plan,
                capacity_trials=(),
                capacity_cohorts=(),
                completion_window_cohorts=(),
                canary_trials=(),
                canary_cohorts=(),
                control_cohorts=(cohort,),
            )
        else:
            selected = plan
        run_id = _run_supervisor(store, selected, enable_active=args.enable_active)
        return _status(store, run_id)
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
