from __future__ import annotations

import csv
import datetime as dt
import json
import pathlib
from typing import Any

from ..model import TerminalReason
from ..storage import TraceStore


PRE_READY_EVENTS = {
    TerminalReason.LAUNCH_FAILURE.value,
    TerminalReason.PROVIDER_PRE_READY_FAILURE.value,
}
POST_READY_INTERRUPTION_EVENTS = {
    TerminalReason.MACHINE_REPLACEMENT.value,
    TerminalReason.PROVIDER_INTERRUPTION_CANDIDATE.value,
    TerminalReason.PROVIDER_UNHEALTHY.value,
}
RIGHT_CENSOR_REASONS = {
    TerminalReason.OPERATOR_CLEANUP.value,
    TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS.value,
    TerminalReason.RIGHT_CENSORED_PLANNED_END.value,
}


SURVIVAL_FIELDS = (
    "run_id",
    "cohort_name",
    "cohort_design",
    "epoch_sequence",
    "selection_probability",
    "selection_seed",
    "pair_id",
    "lease_id",
    "provider",
    "sku",
    "gpu_type",
    "region",
    "datacenter",
    "resource_class",
    "market",
    "interruptible",
    "group_size",
    "pair_established",
    "created_at",
    "provider_ready_at",
    "application_ready_at",
    "terminal_at",
    "planned_end_at",
    "terminal_reason",
    "time_to_provider_ready_seconds",
    "time_to_application_ready_seconds",
    "pre_provider_ready_exposure_seconds",
    "pre_provider_ready_failure_event",
    "post_provider_ready_exposure_seconds",
    "post_provider_ready_interruption_event",
    "provider_ready_analysis_eligible",
    "pre_ready_exposure_seconds",
    "pre_ready_failure_event",
    "post_ready_exposure_seconds",
    "post_ready_interruption_event",
    "application_failure_event",
    "right_censored",
    "post_ready_analysis_eligible",
)


CAPACITY_DEPTH_FIELDS = (
    "run_id",
    "cohort_name",
    "epoch_sequence",
    "selection_probability",
    "selection_seed",
    "scheduled_at",
    "scheduled_offset_seconds",
    "started_lag_seconds",
    "epoch_status",
    "epoch_stop_reason",
    "trial_id",
    "provider",
    "sku",
    "gpu_type",
    "region",
    "datacenter",
    "resource_class",
    "market",
    "gpu_count_per_instance",
    "target_gpu_depth",
    "target_instance_count",
    "requested_instance_count",
    "accepted_instance_count",
    "provider_ready_instance_count",
    "application_ready_instance_count",
    "cleaned_instance_count",
    "requested_gpu_depth",
    "accepted_gpu_depth",
    "provider_ready_gpu_depth",
    "application_ready_gpu_depth",
    "cleaned_gpu_depth",
    "target_depth",
    "requested_depth",
    "accepted_depth",
    "provider_ready_depth",
    "application_ready_depth",
    "cleaned_depth",
    "provider_depth_success",
    "application_depth_success",
    "depth_success",
    "readiness_requirement",
    "first_failed_stage",
    "failure_class",
    "trial_status",
    "retry_attempts",
    "mean_provider_ready_seconds",
    "mean_application_ready_seconds",
    "stock_status",
    "advertised_available",
    "quoted_price_usd_hr",
    "cleanup_verified",
)


STABILITY_WINDOW_FIELDS = (
    "run_id",
    "campaign_id",
    "cohort_name",
    "window_sequence",
    "scheduled_offset_seconds",
    "scheduled_at",
    "scheduled_end_at",
    "claimed_at",
    "started_at",
    "finished_at",
    "window_status",
    "window_stop_reason",
    "scheduled_duration_seconds",
    "started_lag_seconds",
    "observed_duration_seconds",
    "canary_epoch_count",
    "lease_count",
    "interruptible_lease_count",
    "matched_control_lease_count",
    "provider_ready_lease_count",
    "application_ready_lease_count",
    "terminal_lease_count",
    "cleanup_verified",
)


def _parse(value: str | None) -> dt.datetime | None:
    return dt.datetime.fromisoformat(value) if value else None


def _duration(start: dt.datetime | None, end: dt.datetime | None) -> float | None:
    if start is None or end is None:
        return None
    return max(0.0, (end - start).total_seconds())


def survival_rows(store: TraceStore, run_id: str) -> list[dict[str, Any]]:
    store.run_row(run_id)
    leases = store.connection.execute(
        """
        SELECT l.*, e.cohort_name, e.sequence epoch_sequence,
               e.selection_probability, e.selection_seed,
               e.details_json epoch_details_json
        FROM canary_leases l
        JOIN canary_epochs e ON e.id=l.pair_id
        WHERE l.run_id=?
        ORDER BY l.created_at, l.interruptible DESC
        """,
        (run_id,),
    ).fetchall()
    pairs: dict[str, list[Any]] = {}
    for row in leases:
        pairs.setdefault(str(row["pair_id"]), []).append(row)
    pair_ready = {
        pair_id: (
            len(peers) == 2
            and all(peer["application_ready_at"] is not None for peer in peers)
        )
        for pair_id, peers in pairs.items()
    }

    result: list[dict[str, Any]] = []
    for row in leases:
        created = _parse(row["created_at"])
        provider_ready = _parse(row["provider_ready_at"])
        application_ready = _parse(row["application_ready_at"])
        terminal = _parse(row["event_at"] or row["cleaned_at"])
        reason = row["censor_reason"]
        pre_ready_terminal = application_ready or terminal
        epoch_details = json.loads(row["epoch_details_json"])
        group_size = len(pairs[str(row["pair_id"])])
        result.append(
            {
                "run_id": row["run_id"],
                "cohort_name": row["cohort_name"],
                "cohort_design": epoch_details["design"],
                "epoch_sequence": row["epoch_sequence"],
                "selection_probability": row["selection_probability"],
                "selection_seed": row["selection_seed"],
                "pair_id": row["pair_id"],
                "lease_id": row["id"],
                "provider": row["provider"],
                "sku": row["sku"],
                "gpu_type": row["gpu_type"],
                "region": row["region"],
                "datacenter": row["datacenter"],
                "resource_class": row["resource_class"],
                "market": row["market"],
                "interruptible": int(row["interruptible"]),
                "group_size": group_size,
                "pair_established": int(pair_ready[str(row["pair_id"])]),
                "created_at": row["created_at"],
                "provider_ready_at": row["provider_ready_at"],
                "application_ready_at": row["application_ready_at"],
                "terminal_at": row["event_at"] or row["cleaned_at"],
                "planned_end_at": row["planned_end_at"],
                "terminal_reason": reason,
                "time_to_provider_ready_seconds": _duration(created, provider_ready),
                "time_to_application_ready_seconds": _duration(
                    created, application_ready
                ),
                "pre_provider_ready_exposure_seconds": _duration(
                    created,
                    provider_ready or terminal,
                ),
                "pre_provider_ready_failure_event": int(
                    provider_ready is None and reason in PRE_READY_EVENTS
                ),
                "post_provider_ready_exposure_seconds": _duration(
                    provider_ready,
                    terminal,
                ),
                "post_provider_ready_interruption_event": int(
                    provider_ready is not None
                    and reason in POST_READY_INTERRUPTION_EVENTS
                ),
                "provider_ready_analysis_eligible": int(
                    provider_ready is not None
                    and reason
                    not in {
                        TerminalReason.LAUNCH_FAILURE.value,
                        TerminalReason.PAIR_SETUP_FAILURE.value,
                    }
                ),
                "pre_ready_exposure_seconds": _duration(created, pre_ready_terminal),
                "pre_ready_failure_event": int(reason in PRE_READY_EVENTS),
                "post_ready_exposure_seconds": _duration(application_ready, terminal),
                "post_ready_interruption_event": int(
                    reason in POST_READY_INTERRUPTION_EVENTS
                ),
                "application_failure_event": int(
                    reason == TerminalReason.APPLICATION_FAILURE.value
                ),
                "right_censored": int(reason in RIGHT_CENSOR_REASONS),
                "post_ready_analysis_eligible": int(
                    application_ready is not None
                    and reason
                    not in {
                        TerminalReason.LAUNCH_FAILURE.value,
                        TerminalReason.PAIR_SETUP_FAILURE.value,
                    }
                ),
            }
        )
    return result


def export_survival_csv(
    store: TraceStore,
    run_id: str,
    output: str | pathlib.Path,
) -> int:
    rows = survival_rows(store, run_id)
    path = pathlib.Path(output)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=SURVIVAL_FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    return len(rows)


def stability_window_rows(
    store: TraceStore,
    run_id: str,
) -> list[dict[str, Any]]:
    store.run_row(run_id)
    rows = store.connection.execute(
        """
        SELECT w.*, c.campaign_id,
               COUNT(DISTINCT l.epoch_id) AS canary_epoch_count,
               COUNT(cl.id) AS lease_count,
               COALESCE(SUM(CASE WHEN cl.interruptible=1 THEN 1 ELSE 0 END), 0)
                 AS interruptible_lease_count,
               COALESCE(SUM(CASE WHEN cl.interruptible=0 THEN 1 ELSE 0 END), 0)
                 AS matched_control_lease_count,
               COALESCE(SUM(CASE WHEN cl.provider_ready_at IS NOT NULL THEN 1 ELSE 0 END), 0)
                 AS provider_ready_lease_count,
               COALESCE(SUM(CASE WHEN cl.application_ready_at IS NOT NULL THEN 1 ELSE 0 END), 0)
                 AS application_ready_lease_count,
               COALESCE(SUM(CASE WHEN cl.censor_reason IS NOT NULL THEN 1 ELSE 0 END), 0)
                 AS terminal_lease_count
        FROM stability_windows w
        JOIN stability_campaigns c ON c.run_id=w.run_id
        LEFT JOIN stability_window_epochs l ON l.window_id=w.id
        LEFT JOIN canary_leases cl ON cl.pair_id=l.epoch_id
        WHERE w.run_id=?
        GROUP BY w.id
        ORDER BY w.sequence
        """,
        (run_id,),
    ).fetchall()
    result: list[dict[str, Any]] = []
    for row in rows:
        details = json.loads(row["details_json"])
        result.append(
            {
                "run_id": row["run_id"],
                "campaign_id": row["campaign_id"],
                "cohort_name": row["cohort_name"],
                "window_sequence": row["sequence"],
                "scheduled_offset_seconds": row["scheduled_offset_seconds"],
                "scheduled_at": row["scheduled_at"],
                "scheduled_end_at": row["scheduled_end_at"],
                "claimed_at": row["claimed_at"],
                "started_at": row["started_at"],
                "finished_at": row["finished_at"],
                "window_status": row["status"],
                "window_stop_reason": row["stop_reason"],
                "scheduled_duration_seconds": _duration(
                    _parse(row["scheduled_at"]),
                    _parse(row["scheduled_end_at"]),
                ),
                "started_lag_seconds": _duration(
                    _parse(row["scheduled_at"]),
                    _parse(row["started_at"]),
                ),
                "observed_duration_seconds": _duration(
                    _parse(row["started_at"]),
                    _parse(row["finished_at"]),
                ),
                "canary_epoch_count": row["canary_epoch_count"],
                "lease_count": row["lease_count"],
                "interruptible_lease_count": row["interruptible_lease_count"],
                "matched_control_lease_count": row["matched_control_lease_count"],
                "provider_ready_lease_count": row["provider_ready_lease_count"],
                "application_ready_lease_count": row["application_ready_lease_count"],
                "terminal_lease_count": row["terminal_lease_count"],
                "cleanup_verified": details.get("cleanup_verified"),
            }
        )
    return result


def export_stability_window_csv(
    store: TraceStore,
    run_id: str,
    output: str | pathlib.Path,
) -> int:
    rows = stability_window_rows(store, run_id)
    path = pathlib.Path(output)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=STABILITY_WINDOW_FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    return len(rows)


def capacity_depth_rows(store: TraceStore, run_id: str) -> list[dict[str, Any]]:
    store.run_row(run_id)
    epochs = [dict(row) for row in store.capacity_epoch_rows(run_id)]
    trials = {
        row["id"]: dict(row)
        for row in store.connection.execute(
            "SELECT * FROM capacity_trials WHERE run_id=? ORDER BY started_at",
            (run_id,),
        ).fetchall()
    }
    sources: list[tuple[dict[str, Any] | None, dict[str, Any] | None]] = [
        (epoch, trials.get(epoch["trial_id"])) for epoch in epochs
    ]

    result: list[dict[str, Any]] = []
    for epoch, trial in sources:
        trial_id = trial["id"] if trial else None
        replicas = (
            [dict(row) for row in store.capacity_replica_rows(trial_id)]
            if trial_id
            else []
        )
        delivered = [row for row in replicas if row["provider_resource_id"]]
        retry_attempts = sum(int(row["attempt"] > 0) for row in replicas)
        provider_latencies = [
            _duration(_parse(row["requested_at"]), _parse(row["provider_ready_at"]))
            for row in delivered
            if row["provider_ready_at"]
        ]
        application_latencies = [
            _duration(
                _parse(row["requested_at"]),
                _parse(row["application_ready_at"]),
            )
            for row in delivered
            if row["application_ready_at"]
        ]
        quote = None
        if trial and trial["catalog_quote_id"]:
            quote = store.connection.execute(
                "SELECT * FROM catalog_quotes_v2 WHERE id=?",
                (trial["catalog_quote_id"],),
            ).fetchone()
        trial_details = json.loads(trial["details_json"]) if trial else {}
        requirement = (
            "application"
            if trial_details.get("require_application_ready")
            else "provider"
        )
        target_gpu_depth = (
            int(trial["target_depth"]) if trial else int(epoch["target_depth"])
        )
        epoch_details = json.loads(epoch["details_json"]) if epoch else {}
        gpu_count_per_instance = (
            int(trial["gpu_count"])
            if trial
            else int(epoch_details.get("gpu_count_per_instance") or 1)
        )
        target_instance_count = target_gpu_depth // gpu_count_per_instance
        requested_instances = int(trial["requested_depth"]) if trial else 0
        accepted_instances = int(trial["accepted_depth"]) if trial else 0
        provider_ready_instances = int(trial["provider_ready_depth"]) if trial else 0
        application_ready_instances = (
            int(trial["application_ready_depth"]) if trial else 0
        )
        cleaned_instances = int(trial["cleaned_depth"]) if trial else 0
        provider_ready_gpu_depth = provider_ready_instances * gpu_count_per_instance
        application_ready_gpu_depth = (
            application_ready_instances * gpu_count_per_instance
        )
        provider_success = (
            int(provider_ready_gpu_depth >= target_gpu_depth) if trial else None
        )
        application_success = (
            int(application_ready_gpu_depth >= target_gpu_depth) if trial else None
        )
        result.append(
            {
                "run_id": run_id,
                "cohort_name": epoch["cohort_name"],
                "epoch_sequence": epoch["sequence"],
                "selection_probability": epoch["selection_probability"],
                "selection_seed": epoch["selection_seed"],
                "scheduled_at": epoch["scheduled_at"],
                "scheduled_offset_seconds": epoch["scheduled_offset_seconds"],
                "started_lag_seconds": epoch["started_lag_seconds"],
                "epoch_status": epoch["status"],
                "epoch_stop_reason": epoch["stop_reason"],
                "trial_id": trial_id,
                "provider": trial["provider"] if trial else epoch["provider"],
                "sku": trial["sku"] if trial else epoch["sku"],
                "gpu_type": trial["gpu_type"] if trial else epoch["gpu_type"],
                "region": trial["region"] if trial else epoch["region"],
                "datacenter": trial["datacenter"] if trial else epoch["datacenter"],
                "resource_class": (
                    trial["resource_class"] if trial else epoch["resource_class"]
                ),
                "market": trial["market"] if trial else None,
                "gpu_count_per_instance": gpu_count_per_instance,
                "target_gpu_depth": target_gpu_depth,
                "target_instance_count": target_instance_count,
                "requested_instance_count": requested_instances if trial else None,
                "accepted_instance_count": accepted_instances if trial else None,
                "provider_ready_instance_count": (
                    provider_ready_instances if trial else None
                ),
                "application_ready_instance_count": (
                    application_ready_instances if trial else None
                ),
                "cleaned_instance_count": cleaned_instances if trial else None,
                "requested_gpu_depth": (
                    requested_instances * gpu_count_per_instance if trial else None
                ),
                "accepted_gpu_depth": (
                    accepted_instances * gpu_count_per_instance if trial else None
                ),
                "provider_ready_gpu_depth": (
                    provider_ready_gpu_depth if trial else None
                ),
                "application_ready_gpu_depth": (
                    application_ready_gpu_depth if trial else None
                ),
                "cleaned_gpu_depth": (
                    cleaned_instances * gpu_count_per_instance if trial else None
                ),
                "target_depth": target_gpu_depth,
                "requested_depth": requested_instances if trial else None,
                "accepted_depth": accepted_instances if trial else None,
                "provider_ready_depth": provider_ready_instances if trial else None,
                "application_ready_depth": (
                    application_ready_instances if trial else None
                ),
                "cleaned_depth": cleaned_instances if trial else None,
                "provider_depth_success": provider_success,
                "application_depth_success": application_success,
                "depth_success": (
                    application_success
                    if requirement == "application"
                    else provider_success
                ),
                "readiness_requirement": requirement if trial else None,
                "first_failed_stage": trial["first_failed_stage"] if trial else None,
                "failure_class": trial["failure_class"] if trial else None,
                "trial_status": trial["status"] if trial else None,
                "retry_attempts": retry_attempts if trial else None,
                "mean_provider_ready_seconds": (
                    sum(provider_latencies) / len(provider_latencies)
                    if provider_latencies
                    else None
                ),
                "mean_application_ready_seconds": (
                    sum(application_latencies) / len(application_latencies)
                    if application_latencies
                    else None
                ),
                "stock_status": quote["stock_status"] if quote else None,
                "advertised_available": quote["available"] if quote else None,
                "quoted_price_usd_hr": (
                    quote["quoted_price_usd_hr"] if quote else None
                ),
                "cleanup_verified": (
                    int(trial["cleaned_depth"] >= trial["accepted_depth"])
                    if trial
                    else None
                ),
            }
        )
    return result


def capacity_trial_payload(
    store: TraceStore,
    trial_id: str,
) -> dict[str, Any]:
    row = store.capacity_trial_row(trial_id)
    trial = dict(row)
    gpu_count = int(trial["gpu_count"])
    target_gpu_depth = int(trial["target_depth"])
    replicas = [dict(item) for item in store.capacity_replica_rows(trial_id)]
    for replica in replicas:
        replica["details"] = json.loads(replica.pop("details_json"))

    grouped: dict[int, list[dict[str, Any]]] = {}
    for replica in replicas:
        grouped.setdefault(int(replica["stage_target"]), []).append(replica)
    stages = []
    for stage_target, attempts in sorted(grouped.items()):
        original_attempts = [item for item in attempts if item["attempt"] == 0]
        accepted_attempts = [item for item in attempts if item["provider_resource_id"]]
        stages.append(
            {
                "target_gpu_depth": stage_target,
                "target_instance_count": stage_target // gpu_count,
                "new_instance_count": len(original_attempts),
                "new_gpu_count": len(original_attempts) * gpu_count,
                "accepted_instance_count": len(accepted_attempts),
                "accepted_gpu_count": len(accepted_attempts) * gpu_count,
                "provider_ready_instance_count": sum(
                    item["provider_ready_at"] is not None for item in accepted_attempts
                ),
                "application_ready_instance_count": sum(
                    item["application_ready_at"] is not None
                    for item in accepted_attempts
                ),
                "attempts": attempts,
            }
        )

    quote = None
    if trial["catalog_quote_id"]:
        quote_row = store.connection.execute(
            "SELECT * FROM catalog_quotes_v2 WHERE id=?",
            (trial["catalog_quote_id"],),
        ).fetchone()
        if quote_row:
            quote = dict(quote_row)
            for key in ("available_gpu_counts_json", "query_json", "raw_json"):
                quote[key.removesuffix("_json")] = json.loads(quote.pop(key))

    replica_ids = [replica["id"] for replica in replicas]
    cleanup: list[dict[str, Any]] = []
    if replica_ids:
        placeholders = ",".join("?" for _ in replica_ids)
        cleanup = [
            dict(item)
            for item in store.connection.execute(
                f"SELECT * FROM cleanup_events WHERE owner_id IN ({placeholders}) "
                "ORDER BY observed_at, id",
                replica_ids,
            ).fetchall()
        ]
        for item in cleanup:
            item["details"] = json.loads(item.pop("details_json"))

    budget = [
        dict(item)
        for item in store.connection.execute(
            "SELECT * FROM budget_ledger_v2 WHERE run_id=? AND reference=? "
            "ORDER BY observed_at, id",
            (trial["run_id"], trial_id),
        ).fetchall()
    ]
    for item in budget:
        item["details"] = json.loads(item.pop("details_json"))

    requested_instances = int(trial["requested_depth"])
    accepted_instances = int(trial["accepted_depth"])
    provider_ready_instances = int(trial["provider_ready_depth"])
    application_ready_instances = int(trial["application_ready_depth"])
    cleaned_instances = int(trial["cleaned_depth"])
    trial_details = json.loads(trial.pop("details_json"))
    summary = {
        **trial,
        "details": trial_details,
        "gpu_count_per_instance": gpu_count,
        "target_gpu_depth": target_gpu_depth,
        "target_instance_count": target_gpu_depth // gpu_count,
        "requested_instance_count": requested_instances,
        "accepted_instance_count": accepted_instances,
        "provider_ready_instance_count": provider_ready_instances,
        "application_ready_instance_count": application_ready_instances,
        "cleaned_instance_count": cleaned_instances,
        "requested_gpu_depth": requested_instances * gpu_count,
        "accepted_gpu_depth": accepted_instances * gpu_count,
        "provider_ready_gpu_depth": provider_ready_instances * gpu_count,
        "application_ready_gpu_depth": application_ready_instances * gpu_count,
        "cleaned_gpu_depth": cleaned_instances * gpu_count,
        "first_failed_gpu_stage": trial["first_failed_stage"],
    }
    return {
        "schema_version": "capacity-trial-payload/v1",
        "trial": summary,
        "catalog_quote": quote,
        "stages": stages,
        "cleanup_events": cleanup,
        "budget_ledger": budget,
    }


def export_capacity_depth_csv(
    store: TraceStore,
    run_id: str,
    output: str | pathlib.Path,
) -> int:
    rows = capacity_depth_rows(store, run_id)
    path = pathlib.Path(output)
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", newline="", encoding="utf-8") as handle:
        writer = csv.DictWriter(handle, fieldnames=CAPACITY_DEPTH_FIELDS)
        writer.writeheader()
        writer.writerows(rows)
    return len(rows)
