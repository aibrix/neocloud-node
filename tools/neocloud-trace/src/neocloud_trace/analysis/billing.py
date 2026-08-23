from __future__ import annotations

import datetime as dt
import csv
import json
import pathlib
import uuid
from collections import defaultdict
from typing import Any

from ..providers import ProviderBillingRecord
from ..storage import TraceStore


def _parse(value: str) -> dt.datetime:
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def _runtime_estimate(
    started_at: str | None,
    finished_at: str | None,
    price_usd_hr: float | None,
) -> float | None:
    if not started_at or not finished_at or price_usd_hr is None:
        return None
    seconds = max(0.0, (_parse(finished_at) - _parse(started_at)).total_seconds())
    return seconds * float(price_usd_hr) / 3600.0


def _optional_float(value: Any) -> float | None:
    if value in (None, ""):
        return None
    return float(value)


def _optional_int(value: Any) -> int | None:
    if value in (None, ""):
        return None
    return int(value)


def import_billing_file(
    store: TraceStore,
    run_id: str,
    *,
    provider: str,
    path: str | pathlib.Path,
    source: str,
) -> int:
    store.run_row(run_id)
    if not provider.strip() or not source.strip():
        raise ValueError("billing provider and source are required")
    source_path = pathlib.Path(path)
    if source_path.suffix.lower() == ".csv":
        with source_path.open(newline="", encoding="utf-8") as handle:
            rows = list(csv.DictReader(handle))
    elif source_path.suffix.lower() == ".json":
        document = json.loads(source_path.read_text(encoding="utf-8"))
        rows = document.get("records", []) if isinstance(document, dict) else document
        if not isinstance(rows, list):
            raise ValueError("billing JSON must be a list or contain a records list")
    else:
        raise ValueError("billing import supports .csv or .json")

    before = len(store.provider_billing_rows(run_id))
    for row in rows:
        if not isinstance(row, dict):
            raise ValueError("every billing record must be an object")
        period_start = str(row.get("period_start") or row.get("time") or "")
        if not period_start or row.get("amount_usd") in (None, ""):
            raise ValueError("billing records require period_start and amount_usd")
        amount = float(row["amount_usd"])
        if amount < 0:
            raise ValueError("billing amount must not be negative")
        identity = json.dumps(
            {"provider": provider, "source": source, "record": row},
            sort_keys=True,
            separators=(",", ":"),
        )
        record = ProviderBillingRecord(
            provider=provider,
            provider_resource_id=(
                str(row["provider_resource_id"])
                if row.get("provider_resource_id")
                else None
            ),
            period_start=period_start,
            amount_usd=amount,
            billed_duration_ms=_optional_int(row.get("billed_duration_ms")),
            source=source,
            source_record_id=str(
                row.get("source_record_id")
                or uuid.uuid5(uuid.NAMESPACE_URL, identity)
            ),
            currency=str(row.get("currency") or "USD"),
            gpu_type=row.get("gpu_type"),
            disk_space_billed_gb=_optional_float(
                row.get("disk_space_billed_gb")
            ),
            raw=row,
        )
        store.record_provider_billing(run_id, record)
    return len(store.provider_billing_rows(run_id)) - before


def _owned_cost_sources(
    store: TraceStore,
    run_id: str,
) -> list[dict[str, Any]]:
    sources: list[dict[str, Any]] = []
    capacity_rows = store.connection.execute(
        """
        SELECT r.*, t.provider
        FROM capacity_replicas r
        JOIN capacity_trials t ON t.id=r.trial_id
        WHERE t.run_id=? AND r.provider_resource_id IS NOT NULL
        """,
        (run_id,),
    ).fetchall()
    for row in capacity_rows:
        sources.append(
            {
                "provider": row["provider"],
                "provider_resource_id": row["provider_resource_id"],
                "owner_type": "capacity_replica",
                "owner_id": row["id"],
                "estimated_amount_usd": _runtime_estimate(
                    row["accepted_at"] or row["requested_at"],
                    row["cleaned_at"],
                    row["realized_price_usd_hr"],
                ),
            }
        )
    canary_rows = store.connection.execute(
        """
        SELECT * FROM canary_leases
        WHERE run_id=? AND provider_resource_id IS NOT NULL
        """,
        (run_id,),
    ).fetchall()
    for row in canary_rows:
        details = json.loads(row["details_json"])
        price = details.get("price_usd_hr")
        if price is None:
            observation = store.connection.execute(
                """
                SELECT realized_price_usd_hr FROM canary_observations
                WHERE lease_id=? AND realized_price_usd_hr IS NOT NULL
                ORDER BY observed_at DESC LIMIT 1
                """,
                (row["id"],),
            ).fetchone()
            price = observation["realized_price_usd_hr"] if observation else None
        sources.append(
            {
                "provider": row["provider"],
                "provider_resource_id": row["provider_resource_id"],
                "owner_type": "canary_lease",
                "owner_id": row["id"],
                "estimated_amount_usd": _runtime_estimate(
                    row["created_at"],
                    row["cleaned_at"] or row["event_at"],
                    price,
                ),
            }
        )
    return sources


def reconcile_costs(
    store: TraceStore,
    run_id: str,
) -> list[dict[str, Any]]:
    store.run_row(run_id)
    billing: dict[tuple[str, str], list[Any]] = defaultdict(list)
    aggregate_billing: list[Any] = []
    for row in store.provider_billing_rows(run_id):
        if row["provider_resource_id"]:
            billing[(row["provider"], row["provider_resource_id"])].append(row)
        else:
            aggregate_billing.append(row)

    known: set[tuple[str, str]] = set()
    for source in _owned_cost_sources(store, run_id):
        key = (source["provider"], source["provider_resource_id"])
        known.add(key)
        records = billing.get(key, [])
        billed = sum(float(row["amount_usd"]) for row in records) if records else None
        store.upsert_cost_reconciliation(
            run_id,
            provider=source["provider"],
            provider_resource_id=source["provider_resource_id"],
            owner_type=source["owner_type"],
            owner_id=source["owner_id"],
            estimated_amount_usd=source["estimated_amount_usd"],
            billed_amount_usd=billed,
            billing_record_count=len(records),
            status="reconciled" if records else "missing_billing",
            method="provider_resource_id_exact",
        )

    for (provider, resource_id), records in billing.items():
        if (provider, resource_id) in known:
            continue
        store.upsert_cost_reconciliation(
            run_id,
            provider=provider,
            provider_resource_id=resource_id,
            owner_type=None,
            owner_id=None,
            estimated_amount_usd=None,
            billed_amount_usd=sum(float(row["amount_usd"]) for row in records),
            billing_record_count=len(records),
            status="unallocated_billing",
            method="provider_resource_id_exact",
        )

    for row in aggregate_billing:
        synthetic_resource_id = (
            f"aggregate:{row['source']}:{row['source_record_id']}"
        )
        store.upsert_cost_reconciliation(
            run_id,
            provider=row["provider"],
            provider_resource_id=synthetic_resource_id,
            owner_type=None,
            owner_id=None,
            estimated_amount_usd=None,
            billed_amount_usd=float(row["amount_usd"]),
            billing_record_count=1,
            status="aggregate_unattributed",
            method="provider_aggregate_record",
            details={
                "billing_record_id": row["id"],
                "period_start": row["period_start"],
                "source": row["source"],
            },
        )

    return [dict(row) for row in store.cost_reconciliation_rows(run_id)]
