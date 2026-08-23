from __future__ import annotations

import dataclasses
import datetime as dt
import hashlib
import importlib.resources
import json
import pathlib
import sqlite3
import threading
import uuid
from typing import Any

from ..model import (
    CatalogQuote,
    CatalogConfig,
    ExperimentConfig,
    OwnedResource,
    TrialCell,
    validate_capacity_target,
)
from ..providers import ProviderBillingRecord


SECRET_KEYS = {
    "api_key",
    "apikey",
    "authorization",
    "callback_secret",
    "env",
    "jupyter_token",
    "jupyter_url",
    "password",
    "private_key",
    "secret",
    "token",
}


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def redact(value: Any) -> Any:
    if isinstance(value, dict):
        return {
            key: "[REDACTED]" if key.lower() in SECRET_KEYS else redact(item)
            for key, item in value.items()
        }
    if isinstance(value, list):
        return [redact(item) for item in value]
    if isinstance(value, tuple):
        return [redact(item) for item in value]
    return value


def json_text(value: Any) -> str:
    return json.dumps(redact(value), sort_keys=True, separators=(",", ":"), default=str)


def merge_json_text(existing: str, updates: dict[str, Any] | None) -> str:
    merged = json.loads(existing) if existing else {}
    merged.update(updates or {})
    return json_text(merged)


def same_config_ignoring_legacy_vantage(
    existing_config_json: str,
    current_config_json: str,
) -> bool:
    try:
        existing = json.loads(existing_config_json)
        current = json.loads(current_config_json)
    except json.JSONDecodeError:
        return False
    for document in (existing, current):
        if not isinstance(document, dict):
            return False
        experiment = document.get("experiment")
        if isinstance(experiment, dict):
            experiment.pop("vantage_id", None)
        raw = document.get("document")
        if isinstance(raw, dict):
            raw.pop("vantage_id", None)
    return json_text(existing) == json_text(current)


SCHEMA = (
    importlib.resources.files(__package__)
    .joinpath("schema.sql")
    .read_text(encoding="utf-8")
)


class TraceStore:
    def __init__(self, path: str | pathlib.Path):
        self.path = str(path)
        if self.path != ":memory:":
            pathlib.Path(self.path).parent.mkdir(parents=True, exist_ok=True)
        self.connection = sqlite3.connect(self.path, check_same_thread=False)
        self.connection.row_factory = sqlite3.Row
        self._lock = threading.RLock()
        with self._lock:
            if self.path != ":memory:":
                self.connection.execute("PRAGMA journal_mode=WAL")
            self.connection.executescript(SCHEMA)
            self.connection.commit()
            self._legacy_vantage_column = any(
                row["name"] == "vantage_id"
                for row in self.connection.execute(
                    "PRAGMA table_info(experiment_runs)"
                ).fetchall()
            )

    def close(self) -> None:
        with self._lock:
            self.connection.close()

    def _insert_experiment_run(
        self,
        *,
        run_id: str,
        workflow: str,
        experiment_id: str | None,
        cohort_name: str | None,
        protocol_version: str,
        config_hash: str,
        config_json: str,
        host_id: str,
        collector_revision: str | None,
        python_version: str | None,
        started_at: str,
        total_budget_usd: float,
        max_active_gpus_per_provider: int,
    ) -> None:
        columns = [
            "id",
            "workflow",
            "experiment_id",
            "cohort_name",
            "protocol_version",
            "config_hash",
            "config_json",
            "host_id",
            "collector_revision",
            "python_version",
            "started_at",
            "status",
            "total_budget_usd",
            "max_active_gpus_per_provider",
        ]
        values: list[Any] = [
            run_id,
            workflow,
            experiment_id,
            cohort_name,
            protocol_version,
            config_hash,
            config_json,
            host_id,
            collector_revision,
            python_version,
            started_at,
            "running",
            total_budget_usd,
            max_active_gpus_per_provider,
        ]
        if self._legacy_vantage_column:
            # Older trace databases required this column. Keep them writable
            # without carrying a separate location identity in the public model.
            columns.append("vantage_id")
            values.append(host_id)
        placeholders = ", ".join("?" for _ in columns)
        self.connection.execute(
            f"INSERT INTO experiment_runs ({', '.join(columns)}) "
            f"VALUES ({placeholders})",
            values,
        )

    def start_run(
        self,
        config: ExperimentConfig,
        config_document: dict[str, Any],
        *,
        collector_revision: str | None = None,
        python_version: str | None = None,
    ) -> str:
        config_json = json_text(
            {"experiment": dataclasses.asdict(config), "document": config_document}
        )
        return self._start_trace_run(
            workflow="standalone",
            protocol_version=config.protocol_version,
            config_json=config_json,
            host_id=config.host_id,
            total_budget_usd=config.total_budget_usd,
            max_active_gpus_per_provider=config.max_active_gpus_per_provider,
            collector_revision=collector_revision,
            python_version=python_version,
        )

    def start_catalog(
        self,
        config: CatalogConfig,
        *,
        collector_revision: str | None = None,
        python_version: str | None = None,
    ) -> str:
        return self._start_trace_run(
            workflow="catalog",
            protocol_version=config.protocol_version,
            config_json=json_text({"catalog": dataclasses.asdict(config)}),
            host_id=config.host_id,
            total_budget_usd=0.0,
            max_active_gpus_per_provider=0,
            collector_revision=collector_revision,
            python_version=python_version,
        )

    def _start_trace_run(
        self,
        *,
        workflow: str,
        protocol_version: str,
        config_json: str,
        host_id: str,
        total_budget_usd: float,
        max_active_gpus_per_provider: int,
        collector_revision: str | None,
        python_version: str | None,
    ) -> str:
        run_id = str(uuid.uuid4())
        config_hash = hashlib.sha256(config_json.encode()).hexdigest()
        with self._lock, self.connection:
            self._insert_experiment_run(
                run_id=run_id,
                workflow=workflow,
                experiment_id=None,
                cohort_name=None,
                protocol_version=protocol_version,
                config_hash=config_hash,
                config_json=config_json,
                host_id=host_id,
                collector_revision=collector_revision,
                python_version=python_version,
                started_at=utc_now(),
                total_budget_usd=total_budget_usd,
                max_active_gpus_per_provider=max_active_gpus_per_provider,
            )
        return run_id

    def ensure_capacity_experiment(
        self,
        config: ExperimentConfig,
        config_document: dict[str, Any],
        *,
        experiment_id: str,
        cohort_name: str,
        started_at: str,
        epochs: list[dict[str, Any]],
        collector_revision: str | None = None,
        python_version: str | None = None,
    ) -> tuple[str, bool]:
        if not experiment_id.strip() or not cohort_name.strip():
            raise ValueError("capacity experiment ID and cohort name are required")
        if not epochs:
            raise ValueError("capacity experiment requires scheduled epochs")
        config_json = json_text(
            {"experiment": dataclasses.asdict(config), "document": config_document}
        )
        config_hash = hashlib.sha256(config_json.encode()).hexdigest()
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                existing = self.connection.execute(
                    """
                    SELECT * FROM experiment_runs
                    WHERE workflow='capacity' AND experiment_id=?
                    """,
                    (experiment_id,),
                ).fetchone()
                if existing is not None:
                    config_changed = (
                        existing["config_hash"] != config_hash
                        and not same_config_ignoring_legacy_vantage(
                            existing["config_json"], config_json
                        )
                    )
                    if config_changed or existing["cohort_name"] != cohort_name:
                        raise ValueError(
                            "capacity database already uses a different "
                            "experiment definition"
                        )
                    self.connection.commit()
                    return str(existing["id"]), False

                run_id = str(uuid.uuid4())
                self._insert_experiment_run(
                    run_id=run_id,
                    workflow="capacity",
                    experiment_id=experiment_id,
                    cohort_name=cohort_name,
                    protocol_version=config.protocol_version,
                    config_hash=config_hash,
                    config_json=config_json,
                    host_id=config.host_id,
                    collector_revision=collector_revision,
                    python_version=python_version,
                    started_at=started_at,
                    total_budget_usd=config.total_budget_usd,
                    max_active_gpus_per_provider=(
                        config.max_active_gpus_per_provider
                    ),
                )
                for epoch in epochs:
                    cell = epoch["cell"]
                    validate_capacity_target(int(epoch["target_gpu_depth"]), cell.gpu_count)
                    self.connection.execute(
                        """
                        INSERT INTO capacity_epochs
                          (id, run_id, cohort_name, sequence, provider, sku,
                           gpu_type, region, datacenter, resource_class,
                           target_gpu_depth, scheduled_offset_seconds, scheduled_at,
                           selection_probability, selection_seed, status,
                           details_json)
                        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
                                'scheduled', ?)
                        """,
                        (
                            str(uuid.uuid4()),
                            run_id,
                            cohort_name,
                            int(epoch["sequence"]),
                            cell.provider,
                            cell.sku,
                            cell.gpu_type,
                            cell.region,
                            cell.datacenter,
                            cell.resource_class,
                            int(epoch["target_gpu_depth"]),
                            float(epoch["scheduled_offset_seconds"]),
                            str(epoch["scheduled_at"]),
                            float(epoch["selection_probability"]),
                            int(epoch["selection_seed"]),
                            json_text(epoch.get("details") or {}),
                        ),
                    )
                self.connection.commit()
                return run_id, True
            except Exception:
                self.connection.rollback()
                raise

    def capacity_experiment_row(self, experiment_id: str) -> sqlite3.Row:
        with self._lock:
            row = self.connection.execute(
                """
                SELECT * FROM experiment_runs
                WHERE workflow='capacity' AND experiment_id=?
                """,
                (experiment_id,),
            ).fetchone()
        if row is None:
            raise KeyError(experiment_id)
        return row

    def capacity_experiment_for_run(self, run_id: str) -> sqlite3.Row | None:
        with self._lock:
            return self.connection.execute(
                "SELECT * FROM experiment_runs WHERE id=? AND workflow='capacity'",
                (run_id,),
            ).fetchone()

    def resume_capacity_experiment(self, run_id: str) -> None:
        with self._lock, self.connection:
            experiment = self.connection.execute(
                "SELECT * FROM experiment_runs WHERE id=? AND workflow='capacity'",
                (run_id,),
            ).fetchone()
            if experiment is None:
                raise KeyError(run_id)
            self.connection.execute(
                """
                UPDATE experiment_runs
                SET status='running', finished_at=NULL,
                    termination_reason='resumed_after_collector_loss'
                WHERE id=?
                """,
                (run_id,),
            )

    def claim_due_capacity_epoch(
        self,
        experiment_id: str,
        *,
        due_at: str,
        expired_before: str,
    ) -> tuple[str, sqlite3.Row | None]:
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                experiment = self.connection.execute(
                    """
                    SELECT * FROM experiment_runs
                    WHERE workflow='capacity' AND experiment_id=?
                    """,
                    (experiment_id,),
                ).fetchone()
                if experiment is None:
                    raise KeyError(experiment_id)
                if experiment["status"] != "running":
                    self.connection.commit()
                    return "complete", None
                active = self.connection.execute(
                    """
                    SELECT * FROM capacity_epochs
                    WHERE run_id=? AND cohort_name=?
                      AND status IN ('claimed', 'running')
                    ORDER BY sequence LIMIT 1
                    """,
                    (experiment["id"], experiment["cohort_name"]),
                ).fetchone()
                if active is not None:
                    self.connection.commit()
                    return "busy", active
                unresolved = self.connection.execute(
                    """
                    SELECT
                      (SELECT COUNT(*) FROM capacity_replicas
                       WHERE provider_resource_id IS NOT NULL AND cleaned_at IS NULL)
                      +
                      (SELECT COUNT(*) FROM canary_leases
                       WHERE provider_resource_id IS NOT NULL AND cleaned_at IS NULL)
                    """
                ).fetchone()[0]
                if unresolved:
                    self.connection.commit()
                    return "cleanup_blocked", None
                finished_at = utc_now()
                self.connection.execute(
                    """
                    UPDATE capacity_epochs
                    SET status='not_run_missed_slot',
                        stop_reason='external_scheduler_late',
                        finished_at=?
                    WHERE run_id=? AND cohort_name=? AND status='scheduled'
                      AND scheduled_at<?
                    """,
                    (
                        finished_at,
                        experiment["id"],
                        experiment["cohort_name"],
                        expired_before,
                    ),
                )
                epoch = self.connection.execute(
                    """
                    SELECT * FROM capacity_epochs
                    WHERE run_id=? AND cohort_name=? AND status='scheduled'
                      AND scheduled_at<=?
                    ORDER BY scheduled_at, sequence LIMIT 1
                    """,
                    (experiment["id"], experiment["cohort_name"], due_at),
                ).fetchone()
                if epoch is None:
                    remaining = self.connection.execute(
                        """
                        SELECT COUNT(*) FROM capacity_epochs
                        WHERE run_id=? AND cohort_name=? AND status='scheduled'
                        """,
                        (experiment["id"], experiment["cohort_name"]),
                    ).fetchone()[0]
                    self.connection.commit()
                    return ("idle" if remaining else "complete"), None
                updated = self.connection.execute(
                    """
                    UPDATE capacity_epochs SET status='claimed', started_at=?
                    WHERE id=? AND status='scheduled'
                    """,
                    (utc_now(), epoch["id"]),
                )
                if updated.rowcount != 1:
                    self.connection.rollback()
                    return "busy", None
                claimed = self.connection.execute(
                    "SELECT * FROM capacity_epochs WHERE id=?",
                    (epoch["id"],),
                ).fetchone()
                self.connection.commit()
                return "claimed", claimed
            except Exception:
                self.connection.rollback()
                raise

    def finish_capacity_experiment_if_complete(self, experiment_id: str) -> bool:
        with self._lock, self.connection:
            experiment = self.connection.execute(
                """
                SELECT * FROM experiment_runs
                WHERE workflow='capacity' AND experiment_id=?
                """,
                (experiment_id,),
            ).fetchone()
            if experiment is None:
                raise KeyError(experiment_id)
            remaining = self.connection.execute(
                """
                SELECT COUNT(*) FROM capacity_epochs
                WHERE run_id=? AND cohort_name=?
                  AND status IN ('scheduled', 'claimed', 'running')
                """,
                (experiment["id"], experiment["cohort_name"]),
            ).fetchone()[0]
            if remaining:
                return False
            finished_at = utc_now()
            self.connection.execute(
                """
                UPDATE experiment_runs
                SET status='complete', finished_at=?, termination_reason=NULL
                WHERE id=?
                """,
                (finished_at, experiment["id"]),
            )
        return True

    def ensure_stability_experiment(
        self,
        config: ExperimentConfig,
        config_document: dict[str, Any],
        *,
        experiment_id: str,
        cohort_name: str,
        started_at: str,
        windows: list[dict[str, Any]],
        collector_revision: str | None = None,
        python_version: str | None = None,
    ) -> tuple[str, bool]:
        if not experiment_id.strip() or not cohort_name.strip():
            raise ValueError("stability experiment ID and cohort name are required")
        if not windows:
            raise ValueError("stability experiment requires scheduled windows")
        config_json = json_text(
            {"experiment": dataclasses.asdict(config), "document": config_document}
        )
        config_hash = hashlib.sha256(config_json.encode()).hexdigest()
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                existing = self.connection.execute(
                    """
                    SELECT * FROM experiment_runs
                    WHERE workflow='stability' AND experiment_id=?
                    """,
                    (experiment_id,),
                ).fetchone()
                if existing is not None:
                    config_changed = (
                        existing["config_hash"] != config_hash
                        and not same_config_ignoring_legacy_vantage(
                            existing["config_json"], config_json
                        )
                    )
                    if config_changed or existing["cohort_name"] != cohort_name:
                        raise ValueError(
                            "stability database already uses a different "
                            "experiment definition"
                        )
                    self.connection.commit()
                    return str(existing["id"]), False

                run_id = str(uuid.uuid4())
                self._insert_experiment_run(
                    run_id=run_id,
                    workflow="stability",
                    experiment_id=experiment_id,
                    cohort_name=cohort_name,
                    protocol_version=config.protocol_version,
                    config_hash=config_hash,
                    config_json=config_json,
                    host_id=config.host_id,
                    collector_revision=collector_revision,
                    python_version=python_version,
                    started_at=started_at,
                    total_budget_usd=config.total_budget_usd,
                    max_active_gpus_per_provider=(
                        config.max_active_gpus_per_provider
                    ),
                )
                for window in windows:
                    self.connection.execute(
                        """
                        INSERT INTO stability_windows
                          (id, run_id, cohort_name, sequence,
                           scheduled_offset_seconds, scheduled_at,
                           scheduled_end_at, status, details_json)
                        VALUES (?, ?, ?, ?, ?, ?, ?, 'scheduled', ?)
                        """,
                        (
                            str(uuid.uuid4()),
                            run_id,
                            cohort_name,
                            int(window["sequence"]),
                            float(window["scheduled_offset_seconds"]),
                            str(window["scheduled_at"]),
                            str(window["scheduled_end_at"]),
                            json_text(window.get("details") or {}),
                        ),
                    )
                self.connection.commit()
                return run_id, True
            except Exception:
                self.connection.rollback()
                raise

    def stability_experiment_row(self, experiment_id: str) -> sqlite3.Row:
        with self._lock:
            row = self.connection.execute(
                """
                SELECT * FROM experiment_runs
                WHERE workflow='stability' AND experiment_id=?
                """,
                (experiment_id,),
            ).fetchone()
        if row is None:
            raise KeyError(experiment_id)
        return row

    def stability_experiment_for_run(self, run_id: str) -> sqlite3.Row | None:
        with self._lock:
            return self.connection.execute(
                "SELECT * FROM experiment_runs WHERE id=? AND workflow='stability'",
                (run_id,),
            ).fetchone()

    def resume_stability_experiment(self, run_id: str) -> None:
        with self._lock, self.connection:
            experiment = self.connection.execute(
                "SELECT * FROM experiment_runs WHERE id=? AND workflow='stability'",
                (run_id,),
            ).fetchone()
            if experiment is None:
                raise KeyError(run_id)
            self.connection.execute(
                """
                UPDATE experiment_runs
                SET status='running', finished_at=NULL,
                    termination_reason='resumed_after_collector_loss'
                WHERE id=?
                """,
                (run_id,),
            )

    def claim_due_stability_window(
        self,
        experiment_id: str,
        *,
        due_at: str,
    ) -> tuple[str, sqlite3.Row | None]:
        with self._lock:
            self.connection.execute("BEGIN IMMEDIATE")
            try:
                experiment = self.connection.execute(
                    """
                    SELECT * FROM experiment_runs
                    WHERE workflow='stability' AND experiment_id=?
                    """,
                    (experiment_id,),
                ).fetchone()
                if experiment is None:
                    raise KeyError(experiment_id)
                if experiment["status"] != "running":
                    self.connection.commit()
                    return "complete", None
                active = self.connection.execute(
                    """
                    SELECT * FROM stability_windows
                    WHERE run_id=? AND cohort_name=?
                      AND status IN ('claimed', 'running')
                    ORDER BY sequence LIMIT 1
                    """,
                    (experiment["id"], experiment["cohort_name"]),
                ).fetchone()
                if active is not None:
                    self.connection.commit()
                    return "busy", active
                unresolved = self.connection.execute(
                    """
                    SELECT
                      (SELECT COUNT(*) FROM capacity_replicas
                       WHERE provider_resource_id IS NOT NULL AND cleaned_at IS NULL)
                      +
                      (SELECT COUNT(*) FROM canary_leases
                       WHERE provider_resource_id IS NOT NULL AND cleaned_at IS NULL)
                    """
                ).fetchone()[0]
                if unresolved:
                    self.connection.commit()
                    return "cleanup_blocked", None
                finished_at = utc_now()
                self.connection.execute(
                    """
                    UPDATE stability_windows
                    SET status='missed', stop_reason='external_scheduler_late',
                        finished_at=?
                    WHERE run_id=? AND cohort_name=? AND status='scheduled'
                      AND scheduled_end_at<=?
                    """,
                    (
                        finished_at,
                        experiment["id"],
                        experiment["cohort_name"],
                        due_at,
                    ),
                )
                window = self.connection.execute(
                    """
                    SELECT * FROM stability_windows
                    WHERE run_id=? AND cohort_name=? AND status='scheduled'
                      AND scheduled_at<=? AND scheduled_end_at>?
                    ORDER BY scheduled_at, sequence LIMIT 1
                    """,
                    (
                        experiment["id"],
                        experiment["cohort_name"],
                        due_at,
                        due_at,
                    ),
                ).fetchone()
                if window is None:
                    remaining = self.connection.execute(
                        """
                        SELECT COUNT(*) FROM stability_windows
                        WHERE run_id=? AND cohort_name=? AND status='scheduled'
                        """,
                        (experiment["id"], experiment["cohort_name"]),
                    ).fetchone()[0]
                    self.connection.commit()
                    return ("idle" if remaining else "complete"), None
                updated = self.connection.execute(
                    """
                    UPDATE stability_windows
                    SET status='claimed', claimed_at=?
                    WHERE id=? AND status='scheduled'
                    """,
                    (finished_at, window["id"]),
                )
                if updated.rowcount != 1:
                    self.connection.rollback()
                    return "busy", None
                claimed = self.connection.execute(
                    "SELECT * FROM stability_windows WHERE id=?",
                    (window["id"],),
                ).fetchone()
                self.connection.commit()
                return "claimed", claimed
            except Exception:
                self.connection.rollback()
                raise

    def mark_stability_window_started(
        self,
        window_id: str,
        *,
        started_at: str,
    ) -> None:
        with self._lock, self.connection:
            updated = self.connection.execute(
                """
                UPDATE stability_windows SET status='running', started_at=?
                WHERE id=? AND status='claimed'
                """,
                (started_at, window_id),
            )
            if updated.rowcount != 1:
                raise ValueError("stability window is not claimed")

    def finish_stability_window(
        self,
        window_id: str,
        *,
        status: str,
        stop_reason: str | None,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                "SELECT details_json FROM stability_windows WHERE id=?",
                (window_id,),
            ).fetchone()
            if row is None:
                raise KeyError(window_id)
            merged = json.loads(row["details_json"])
            merged.update(details or {})
            self.connection.execute(
                """
                UPDATE stability_windows
                SET status=?, stop_reason=?, finished_at=?, details_json=?
                WHERE id=?
                """,
                (status, stop_reason, utc_now(), json_text(merged), window_id),
            )

    def finish_active_stability_windows(
        self,
        run_id: str,
        *,
        status: str,
        stop_reason: str,
        details: dict[str, Any] | None = None,
    ) -> int:
        with self._lock:
            rows = self.connection.execute(
                """
                SELECT id, details_json FROM stability_windows
                WHERE run_id=? AND status IN ('claimed', 'running')
                """,
                (run_id,),
            ).fetchall()
            with self.connection:
                for row in rows:
                    self.connection.execute(
                        """
                        UPDATE stability_windows
                        SET status=?, stop_reason=?, finished_at=?, details_json=?
                        WHERE id=?
                        """,
                        (
                            status,
                            stop_reason,
                            utc_now(),
                            merge_json_text(row["details_json"], details),
                            row["id"],
                        ),
                    )
        return len(rows)

    def stop_remaining_stability_windows(
        self,
        run_id: str,
        *,
        status: str,
        stop_reason: str,
    ) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                """
                UPDATE stability_windows
                SET status=?, stop_reason=?, finished_at=?
                WHERE run_id=? AND status='scheduled'
                """,
                (status, stop_reason, utc_now(), run_id),
            )

    def link_stability_window_epoch(self, window_id: str, epoch_id: str) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO stability_window_epochs (window_id, epoch_id)
                VALUES (?, ?)
                """,
                (window_id, epoch_id),
            )

    def stability_window_epoch_rows(self, window_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT e.* FROM canary_epochs e
                JOIN stability_window_epochs l ON l.epoch_id=e.id
                WHERE l.window_id=? ORDER BY e.sequence
                """,
                (window_id,),
            ).fetchall()

    def stability_window_rows(self, run_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM stability_windows
                WHERE run_id=? ORDER BY sequence
                """,
                (run_id,),
            ).fetchall()

    def finish_stability_experiment_if_complete(
        self,
        experiment_id: str,
        *,
        status: str = "complete",
        termination_reason: str | None = None,
    ) -> bool:
        with self._lock, self.connection:
            experiment = self.connection.execute(
                """
                SELECT * FROM experiment_runs
                WHERE workflow='stability' AND experiment_id=?
                """,
                (experiment_id,),
            ).fetchone()
            if experiment is None:
                raise KeyError(experiment_id)
            remaining = self.connection.execute(
                """
                SELECT COUNT(*) FROM stability_windows
                WHERE run_id=? AND cohort_name=?
                  AND status IN ('scheduled', 'claimed', 'running')
                """,
                (experiment["id"], experiment["cohort_name"]),
            ).fetchone()[0]
            if remaining:
                return False
            finished_at = utc_now()
            self.connection.execute(
                """
                UPDATE experiment_runs
                SET status=?, finished_at=?, termination_reason=?
                WHERE id=?
                """,
                (
                    status,
                    finished_at,
                    termination_reason,
                    experiment["id"],
                ),
            )
        return True

    def finish_run(self, run_id: str, status: str, reason: str | None = None) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                "UPDATE experiment_runs SET finished_at=?, status=?, termination_reason=? WHERE id=?",
                (utc_now(), status, reason, run_id),
            )

    def record_provider_billing(
        self,
        run_id: str,
        record: ProviderBillingRecord,
    ) -> str:
        billing_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO provider_billing_records
                  (id, run_id, provider, provider_resource_id, period_start,
                   amount_usd, billed_duration_ms, currency, source,
                   source_record_id, gpu_type, disk_space_billed_gb,
                   imported_at, raw_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(run_id, provider, source, source_record_id) DO UPDATE SET
                  provider_resource_id=excluded.provider_resource_id,
                  period_start=excluded.period_start,
                  amount_usd=excluded.amount_usd,
                  billed_duration_ms=excluded.billed_duration_ms,
                  currency=excluded.currency,
                  gpu_type=excluded.gpu_type,
                  disk_space_billed_gb=excluded.disk_space_billed_gb,
                  imported_at=excluded.imported_at,
                  raw_json=excluded.raw_json
                """,
                (
                    billing_id,
                    run_id,
                    record.provider,
                    record.provider_resource_id,
                    record.period_start,
                    record.amount_usd,
                    record.billed_duration_ms,
                    record.currency,
                    record.source,
                    record.source_record_id,
                    record.gpu_type,
                    record.disk_space_billed_gb,
                    utc_now(),
                    json_text(record.raw or {}),
                ),
            )
            row = self.connection.execute(
                """
                SELECT id FROM provider_billing_records
                WHERE run_id=? AND provider=? AND source=? AND source_record_id=?
                """,
                (
                    run_id,
                    record.provider,
                    record.source,
                    record.source_record_id,
                ),
            ).fetchone()
        return str(row["id"])

    def provider_billing_rows(self, run_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM provider_billing_records
                WHERE run_id=? ORDER BY provider, period_start, source_record_id
                """,
                (run_id,),
            ).fetchall()

    def upsert_cost_reconciliation(
        self,
        run_id: str,
        *,
        provider: str,
        provider_resource_id: str,
        owner_type: str | None,
        owner_id: str | None,
        estimated_amount_usd: float | None,
        billed_amount_usd: float | None,
        billing_record_count: int,
        status: str,
        method: str,
        details: dict[str, Any] | None = None,
    ) -> str:
        reconciliation_id = str(uuid.uuid4())
        variance = (
            billed_amount_usd - estimated_amount_usd
            if billed_amount_usd is not None and estimated_amount_usd is not None
            else None
        )
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO cost_reconciliations
                  (id, run_id, provider, provider_resource_id, owner_type, owner_id,
                   estimated_amount_usd, billed_amount_usd, variance_usd,
                   billing_record_count, status, method, reconciled_at, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(run_id, provider, provider_resource_id) DO UPDATE SET
                  owner_type=excluded.owner_type,
                  owner_id=excluded.owner_id,
                  estimated_amount_usd=excluded.estimated_amount_usd,
                  billed_amount_usd=excluded.billed_amount_usd,
                  variance_usd=excluded.variance_usd,
                  billing_record_count=excluded.billing_record_count,
                  status=excluded.status,
                  method=excluded.method,
                  reconciled_at=excluded.reconciled_at,
                  details_json=excluded.details_json
                """,
                (
                    reconciliation_id,
                    run_id,
                    provider,
                    provider_resource_id,
                    owner_type,
                    owner_id,
                    estimated_amount_usd,
                    billed_amount_usd,
                    variance,
                    billing_record_count,
                    status,
                    method,
                    utc_now(),
                    json_text(details or {}),
                ),
            )
            row = self.connection.execute(
                """
                SELECT id FROM cost_reconciliations
                WHERE run_id=? AND provider=? AND provider_resource_id=?
                """,
                (run_id, provider, provider_resource_id),
            ).fetchone()
        return str(row["id"])

    def cost_reconciliation_rows(self, run_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM cost_reconciliations
                WHERE run_id=? ORDER BY provider, provider_resource_id
                """,
                (run_id,),
            ).fetchall()

    def record_quote(
        self, run_id: str, quote: CatalogQuote, observed_at: str | None = None
    ) -> str:
        quote_id = str(uuid.uuid4())
        raw_json = json_text(quote.raw)
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO catalog_quotes
                  (id, run_id, observed_at, provider, sku, gpu_type, gpu_count,
                   region, datacenter, resource_class, market, stock_status,
                   available_gpu_counts_json, available, quoted_price_usd_hr,
                   spot_price_usd_hr, vcpus, memory_gib, storage_gib, query_json,
                   raw_hash, raw_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    quote_id,
                    run_id,
                    observed_at or utc_now(),
                    quote.provider,
                    quote.sku,
                    quote.gpu_type,
                    quote.gpu_count,
                    quote.region,
                    quote.datacenter,
                    quote.resource_class,
                    quote.market,
                    quote.stock_status,
                    json_text(quote.available_gpu_counts),
                    None if quote.available is None else int(quote.available),
                    quote.quoted_price_usd_hr,
                    quote.spot_price_usd_hr,
                    quote.vcpus,
                    quote.memory_gib,
                    quote.storage_gib,
                    json_text(quote.query),
                    hashlib.sha256(raw_json.encode()).hexdigest(),
                    raw_json,
                ),
            )
        return quote_id

    def latest_quote_id(
        self,
        run_id: str,
        cell: TrialCell,
        *,
        observed_at_or_before: str | None = None,
    ) -> str | None:
        cutoff_clause = "AND observed_at<=?" if observed_at_or_before else ""
        parameters: list[Any] = [
            run_id,
            cell.provider,
            cell.sku,
            cell.gpu_count,
            cell.resource_class,
            cell.market,
            cell.region,
            cell.region,
            cell.datacenter,
            cell.datacenter,
        ]
        if observed_at_or_before:
            parameters.append(observed_at_or_before)
        with self._lock:
            row = self.connection.execute(
                f"""
                SELECT id FROM catalog_quotes
                WHERE run_id=? AND provider=? AND sku=? AND gpu_count=?
                  AND resource_class=? AND market=?
                  AND ((? IS NULL AND region IS NULL) OR region=?)
                  AND ((? IS NULL AND datacenter IS NULL) OR datacenter=?)
                  {cutoff_clause}
                ORDER BY observed_at DESC, rowid DESC LIMIT 1
                """,
                parameters,
            ).fetchone()
        return row["id"] if row else None

    def start_capacity_trial(
        self,
        run_id: str,
        cell: TrialCell,
        target_gpu_depth: int,
        *,
        trial_id: str | None = None,
    ) -> str:
        validate_capacity_target(target_gpu_depth, cell.gpu_count)
        trial_id = trial_id or str(uuid.uuid4())
        catalog_quote_id = self.latest_quote_id(run_id, cell)
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO capacity_trials
                  (id, run_id, provider, sku, gpu_type, gpu_count, region, datacenter,
                   resource_class, market, image, target_gpu_depth, started_at, status,
                   catalog_quote_id, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', ?, '{}')
                """,
                (
                    trial_id,
                    run_id,
                    cell.provider,
                    cell.sku,
                    cell.gpu_type,
                    cell.gpu_count,
                    cell.region,
                    cell.datacenter,
                    cell.resource_class,
                    cell.market,
                    cell.image,
                    target_gpu_depth,
                    utc_now(),
                    catalog_quote_id,
                ),
            )
        return trial_id

    def schedule_capacity_epoch(
        self,
        run_id: str,
        *,
        cohort_name: str,
        sequence: int,
        cell: TrialCell,
        target_gpu_depth: int,
        scheduled_offset_seconds: float,
        scheduled_at: str,
        selection_probability: float,
        selection_seed: int,
        details: dict[str, Any] | None = None,
    ) -> str:
        if not cohort_name.strip():
            raise ValueError("capacity cohort name is required")
        if sequence < 0 or scheduled_offset_seconds < 0:
            raise ValueError("capacity epoch sequence and offset must not be negative")
        validate_capacity_target(target_gpu_depth, cell.gpu_count)
        if not 0 < selection_probability <= 1:
            raise ValueError("selection_probability must be in (0, 1]")
        epoch_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO capacity_epochs
                  (id, run_id, cohort_name, sequence, provider, sku, gpu_type,
                   region, datacenter, resource_class, target_gpu_depth,
                   scheduled_offset_seconds, scheduled_at, selection_probability,
                   selection_seed, status, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'scheduled', ?)
                """,
                (
                    epoch_id,
                    run_id,
                    cohort_name,
                    sequence,
                    cell.provider,
                    cell.sku,
                    cell.gpu_type,
                    cell.region,
                    cell.datacenter,
                    cell.resource_class,
                    target_gpu_depth,
                    scheduled_offset_seconds,
                    scheduled_at,
                    selection_probability,
                    selection_seed,
                    json_text(details or {}),
                ),
            )
        return epoch_id

    def mark_capacity_epoch_started(
        self,
        epoch_id: str,
        *,
        trial_id: str,
        started_lag_seconds: float,
    ) -> None:
        with self._lock, self.connection:
            updated = self.connection.execute(
                """
                UPDATE capacity_epochs
                SET started_at=?, started_lag_seconds=?, trial_id=?, status='running'
                WHERE id=? AND status IN ('scheduled', 'claimed')
                """,
                (utc_now(), started_lag_seconds, trial_id, epoch_id),
            )
            if updated.rowcount != 1:
                raise ValueError(f"capacity epoch {epoch_id} is not claimable")

    def finish_capacity_epoch(
        self,
        epoch_id: str,
        *,
        status: str,
        stop_reason: str,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                "SELECT details_json FROM capacity_epochs WHERE id=?", (epoch_id,)
            ).fetchone()
            if row is None:
                raise KeyError(epoch_id)
            merged_details = json.loads(row["details_json"])
            merged_details.update(details or {})
            self.connection.execute(
                """
                UPDATE capacity_epochs
                SET finished_at=?, status=?, stop_reason=?, details_json=?
                WHERE id=?
                """,
                (utc_now(), status, stop_reason, json_text(merged_details), epoch_id),
            )

    def capacity_epoch_rows(self, run_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM capacity_epochs
                WHERE run_id=? ORDER BY cohort_name, sequence
                """,
                (run_id,),
            ).fetchall()

    def start_replica(
        self,
        trial_id: str,
        stage_target: int,
        ordinal: int,
        *,
        attempt: int = 0,
        retry_of: str | None = None,
    ) -> str:
        replica_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO capacity_replicas
                  (id, trial_id, stage_target, ordinal, attempt, requested_at,
                   status, retry_of, details_json)
                VALUES (?, ?, ?, ?, ?, ?, 'requested', ?, '{}')
                """,
                (
                    replica_id,
                    trial_id,
                    stage_target,
                    ordinal,
                    attempt,
                    utc_now(),
                    retry_of,
                ),
            )
            if retry_of is None:
                self.connection.execute(
                    "UPDATE capacity_trials SET requested_instance_count=requested_instance_count+1 WHERE id=?",
                    (trial_id,),
                )
        return replica_id

    def mark_replica_created(
        self,
        replica_id: str,
        provider_resource_id: str,
        realized_price_usd_hr: float | None = None,
        *,
        provider_location: str | None = None,
        machine_id: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                "SELECT trial_id, accepted_at, details_json "
                "FROM capacity_replicas WHERE id=?",
                (replica_id,),
            ).fetchone()
            if row is None:
                raise KeyError(replica_id)
            newly_accepted = row["accepted_at"] is None
            self.connection.execute(
                """
                UPDATE capacity_replicas
                SET accepted_at=COALESCE(accepted_at, ?), provider_resource_id=?,
                    provider_location=?, machine_id=?, realized_price_usd_hr=?,
                    status='accepted', details_json=?
                WHERE id=?
                """,
                (
                    utc_now(),
                    provider_resource_id,
                    provider_location,
                    machine_id,
                    realized_price_usd_hr,
                    merge_json_text(row["details_json"], details),
                    replica_id,
                ),
            )
            if newly_accepted:
                self.connection.execute(
                    "UPDATE capacity_trials SET accepted_instance_count=accepted_instance_count+1 WHERE id=?",
                    (row["trial_id"],),
                )

    def mark_replica_cleaned(self, replica_id: str) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                "SELECT trial_id, cleaned_at FROM capacity_replicas WHERE id=?",
                (replica_id,),
            ).fetchone()
            if row is None:
                raise KeyError(replica_id)
            newly_cleaned = row["cleaned_at"] is None
            self.connection.execute(
                "UPDATE capacity_replicas SET cleaned_at=?, status='cleaned' WHERE id=?",
                (utc_now(), replica_id),
            )
            if newly_cleaned:
                self.connection.execute(
                    "UPDATE capacity_trials SET cleaned_instance_count=cleaned_instance_count+1 WHERE id=?",
                    (row["trial_id"],),
                )

    def mark_replica_failed(
        self,
        replica_id: str,
        failure_class: str,
        message: str | None,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                "SELECT details_json FROM capacity_replicas WHERE id=?",
                (replica_id,),
            ).fetchone()
            if row is None:
                raise KeyError(replica_id)
            self.connection.execute(
                """
                UPDATE capacity_replicas
                SET failed_at=?, failure_class=?, failure_message=?, status='failed',
                    details_json=?
                WHERE id=?
                """,
                (
                    utc_now(),
                    failure_class,
                    message,
                    merge_json_text(row["details_json"], details),
                    replica_id,
                ),
            )

    def mark_replica_observed(
        self,
        replica_id: str,
        *,
        provider_ready: bool,
        application_ready: bool | None,
        provider_location: str | None = None,
        machine_id: str | None = None,
        realized_price_usd_hr: float | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                """
                SELECT trial_id, provider_ready_at, application_ready_at, details_json
                FROM capacity_replicas WHERE id=?
                """,
                (replica_id,),
            ).fetchone()
            if row is None:
                raise KeyError(replica_id)
            provider_ready_at = (
                utc_now()
                if provider_ready and row["provider_ready_at"] is None
                else row["provider_ready_at"]
            )
            application_ready_at = (
                utc_now()
                if application_ready is True and row["application_ready_at"] is None
                else row["application_ready_at"]
            )
            self.connection.execute(
                """
                UPDATE capacity_replicas
                SET provider_ready_at=?, application_ready_at=?,
                    provider_location=COALESCE(?, provider_location),
                    machine_id=COALESCE(?, machine_id),
                    realized_price_usd_hr=COALESCE(?, realized_price_usd_hr),
                    status=?, details_json=?
                WHERE id=?
                """,
                (
                    provider_ready_at,
                    application_ready_at,
                    provider_location,
                    machine_id,
                    realized_price_usd_hr,
                    "application_ready"
                    if application_ready_at
                    else "provider_ready"
                    if provider_ready_at
                    else "accepted",
                    merge_json_text(row["details_json"], details),
                    replica_id,
                ),
            )
            if provider_ready and row["provider_ready_at"] is None:
                self.connection.execute(
                    "UPDATE capacity_trials SET provider_ready_instance_count=provider_ready_instance_count+1 WHERE id=?",
                    (row["trial_id"],),
                )
            if application_ready is True and row["application_ready_at"] is None:
                self.connection.execute(
                    "UPDATE capacity_trials SET application_ready_instance_count=application_ready_instance_count+1 WHERE id=?",
                    (row["trial_id"],),
                )

    def finish_capacity_trial(
        self,
        trial_id: str,
        *,
        status: str,
        first_failed_stage: int | None = None,
        failure_class: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                """
                UPDATE capacity_trials
                SET finished_at=?, status=?, first_failed_stage=?, failure_class=?,
                    details_json=?
                WHERE id=?
                """,
                (
                    utc_now(),
                    status,
                    first_failed_stage,
                    failure_class,
                    json_text(details or {}),
                    trial_id,
                ),
            )

    def capacity_trial_row(self, trial_id: str) -> sqlite3.Row:
        with self._lock:
            row = self.connection.execute(
                "SELECT * FROM capacity_trials WHERE id=?", (trial_id,)
            ).fetchone()
        if row is None:
            raise KeyError(trial_id)
        return row

    def capacity_replica_rows(self, trial_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM capacity_replicas
                WHERE trial_id=? ORDER BY ordinal, attempt
                """,
                (trial_id,),
            ).fetchall()

    def record_cleanup(
        self,
        run_id: str,
        *,
        owner_type: str,
        owner_id: str,
        provider: str,
        provider_resource_id: str,
        action: str,
        status: str,
        failure_class: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> str:
        event_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO cleanup_events
                  (id, run_id, owner_type, owner_id, provider,
                   provider_resource_id, observed_at, action, status,
                   failure_class, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    event_id,
                    run_id,
                    owner_type,
                    owner_id,
                    provider,
                    provider_resource_id,
                    utc_now(),
                    action,
                    status,
                    failure_class,
                    json_text(details or {}),
                ),
            )
        return event_id

    def record_budget(
        self,
        run_id: str,
        provider: str,
        amount_usd: float,
        kind: str,
        reference: str | None,
        details: dict[str, Any] | None = None,
    ) -> str:
        if amount_usd < 0:
            raise ValueError("budget amount must not be negative")
        entry_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO budget_ledger
                  (id, run_id, observed_at, provider, amount_usd, kind, reference, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    entry_id,
                    run_id,
                    utc_now(),
                    provider,
                    amount_usd,
                    kind,
                    reference,
                    json_text(details or {}),
                ),
            )
        return entry_id

    def record_budget_release(
        self,
        run_id: str,
        provider: str,
        amount_usd: float,
        reference: str,
        details: dict[str, Any] | None = None,
    ) -> str:
        if amount_usd < 0:
            raise ValueError("budget release must not be negative")
        entry_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO budget_ledger
                  (id, run_id, observed_at, provider, amount_usd, kind, reference, details_json)
                VALUES (?, ?, ?, ?, ?, 'released_unused_commitment', ?, ?)
                """,
                (
                    entry_id,
                    run_id,
                    utc_now(),
                    provider,
                    -amount_usd,
                    reference,
                    json_text(details or {}),
                ),
            )
        return entry_id

    def budget_reference_rows(
        self,
        run_id: str,
        provider: str,
        reference: str,
    ) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM budget_ledger
                WHERE run_id=? AND provider=? AND reference=?
                ORDER BY observed_at, id
                """,
                (run_id, provider, reference),
            ).fetchall()

    def provider_spend(self, run_id: str, provider: str) -> float:
        with self._lock:
            row = self.connection.execute(
                "SELECT COALESCE(SUM(amount_usd), 0) FROM budget_ledger WHERE run_id=? AND provider=?",
                (run_id, provider),
            ).fetchone()
        return float(row[0])

    def total_spend(self, run_id: str) -> float:
        with self._lock:
            row = self.connection.execute(
                "SELECT COALESCE(SUM(amount_usd), 0) FROM budget_ledger WHERE run_id=?",
                (run_id,),
            ).fetchone()
        return float(row[0])

    def owned_resources(
        self, run_id: str, provider: str | None = None
    ) -> list[OwnedResource]:
        provider_clause = " AND t.provider=?" if provider else ""
        params: list[Any] = [run_id]
        if provider:
            params.append(provider)
        with self._lock:
            replica_rows = self.connection.execute(
                f"""
                SELECT r.id owner_id, t.run_id, t.provider, r.provider_resource_id
                FROM capacity_replicas r JOIN capacity_trials t ON t.id=r.trial_id
                WHERE t.run_id=? AND r.provider_resource_id IS NOT NULL
                  AND r.cleaned_at IS NULL{provider_clause}
                """,
                params,
            ).fetchall()
            canary_params: list[Any] = [run_id]
            canary_clause = " AND provider=?" if provider else ""
            if provider:
                canary_params.append(provider)
            canary_rows = self.connection.execute(
                f"""
                SELECT id owner_id, run_id, provider, provider_resource_id
                FROM canary_leases
                WHERE run_id=? AND provider_resource_id IS NOT NULL
                  AND cleaned_at IS NULL{canary_clause}
                """,
                canary_params,
            ).fetchall()
        resources = [
            OwnedResource(
                "capacity_replica",
                row["owner_id"],
                row["run_id"],
                row["provider"],
                row["provider_resource_id"],
                "gpu",
            )
            for row in replica_rows
        ]
        resources.extend(
            OwnedResource(
                "canary_lease",
                row["owner_id"],
                row["run_id"],
                row["provider"],
                row["provider_resource_id"],
                "gpu",
            )
            for row in canary_rows
        )
        return resources

    def all_owned_resources(self, provider: str | None = None) -> list[OwnedResource]:
        with self._lock:
            run_ids = [
                row[0]
                for row in self.connection.execute(
                    "SELECT id FROM experiment_runs ORDER BY started_at"
                ).fetchall()
            ]
        resources: list[OwnedResource] = []
        for run_id in run_ids:
            resources.extend(self.owned_resources(run_id, provider=provider))
        return resources

    def run_row(self, run_id: str) -> sqlite3.Row:
        with self._lock:
            row = self.connection.execute(
                "SELECT * FROM experiment_runs WHERE id=?", (run_id,)
            ).fetchone()
        if row is None:
            raise KeyError(run_id)
        return row

    def latest_run_id(self) -> str | None:
        with self._lock:
            row = self.connection.execute(
                "SELECT id FROM experiment_runs ORDER BY started_at DESC LIMIT 1"
            ).fetchone()
        return row[0] if row else None

    def record_host(
        self,
        run_id: str,
        *,
        expected_monotonic: float,
        actual_monotonic: float,
        scheduler_drift_ms: int,
        healthy: bool,
        network_status: str | None = None,
        network_latency_ms: int | None = None,
        details: dict[str, Any] | None = None,
    ) -> str:
        observation_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO host_observations
                  (id, run_id, observed_at, expected_monotonic, actual_monotonic,
                   scheduler_drift_ms, healthy, network_status, network_latency_ms,
                   db_write_ok, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
                """,
                (
                    observation_id,
                    run_id,
                    utc_now(),
                    expected_monotonic,
                    actual_monotonic,
                    scheduler_drift_ms,
                    int(healthy),
                    network_status,
                    network_latency_ms,
                    json_text(details or {}),
                ),
            )
        return observation_id

    def record_api_observation(
        self,
        run_id: str,
        provider: str,
        operation: str,
        status: str,
        latency_ms: int | None,
        raw: Any,
        *,
        failure_class: str | None = None,
        host_observation_id: str | None = None,
    ) -> str:
        observation_id = str(uuid.uuid4())
        raw_json = json_text(raw)
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO provider_api_observations
                  (id, run_id, observed_at, provider, operation, status, latency_ms,
                   failure_class, host_observation_id, response_hash, raw_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    observation_id,
                    run_id,
                    utc_now(),
                    provider,
                    operation,
                    status,
                    latency_ms,
                    failure_class,
                    host_observation_id,
                    hashlib.sha256(raw_json.encode()).hexdigest(),
                    raw_json,
                ),
            )
        return observation_id

    def active_canaries(
        self,
        run_id: str,
        *,
        provider: str,
        sku: str,
        resource_class: str,
    ) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM canary_leases
                WHERE run_id=? AND provider=? AND sku=? AND resource_class=?
                  AND cleaned_at IS NULL
                  AND status IN ('requested','active','provider_ready','application_ready','monitoring')
                ORDER BY interruptible DESC
                """,
                (run_id, provider, sku, resource_class),
            ).fetchall()

    def start_canary_epoch(
        self,
        run_id: str,
        *,
        cohort_name: str,
        sequence: int,
        cell: TrialCell,
        planned_duration_seconds: float,
        interval_seconds: float,
        selection_probability: float,
        selection_seed: int | None,
        details: dict[str, Any] | None = None,
    ) -> str:
        if not cohort_name.strip():
            raise ValueError("cohort_name is required")
        if sequence < 0:
            raise ValueError("canary epoch sequence must not be negative")
        if planned_duration_seconds <= 0 or interval_seconds <= 0:
            raise ValueError("canary epoch durations must be positive")
        if not 0 < selection_probability <= 1:
            raise ValueError("selection_probability must be in (0, 1]")
        epoch_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO canary_epochs
                  (id, run_id, cohort_name, sequence, provider, sku, gpu_type,
                   region, datacenter, resource_class, planned_duration_seconds,
                   interval_seconds, selection_probability, selection_seed,
                   selected_at, status, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'selected', ?)
                """,
                (
                    epoch_id,
                    run_id,
                    cohort_name,
                    sequence,
                    cell.provider,
                    cell.sku,
                    cell.gpu_type,
                    cell.region,
                    cell.datacenter,
                    cell.resource_class,
                    planned_duration_seconds,
                    interval_seconds,
                    selection_probability,
                    selection_seed,
                    utc_now(),
                    json_text(details or {}),
                ),
            )
        return epoch_id

    def mark_canary_epoch_started(self, epoch_id: str, planned_end_at: str) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                """
                UPDATE canary_epochs
                SET started_at=COALESCE(started_at, ?), planned_end_at=?, status='active'
                WHERE id=?
                """,
                (utc_now(), planned_end_at, epoch_id),
            )

    def finish_canary_epoch(
        self,
        epoch_id: str,
        *,
        status: str,
        stop_reason: str,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            row = self.connection.execute(
                "SELECT details_json FROM canary_epochs WHERE id=?", (epoch_id,)
            ).fetchone()
            if row is None:
                raise KeyError(epoch_id)
            merged_details = json.loads(row["details_json"])
            merged_details.update(details or {})
            self.connection.execute(
                """
                UPDATE canary_epochs
                SET finished_at=?, status=?, stop_reason=?, details_json=?
                WHERE id=?
                """,
                (utc_now(), status, stop_reason, json_text(merged_details), epoch_id),
            )

    def canary_epoch_row(self, epoch_id: str) -> sqlite3.Row:
        with self._lock:
            row = self.connection.execute(
                "SELECT * FROM canary_epochs WHERE id=?", (epoch_id,)
            ).fetchone()
        if row is None:
            raise KeyError(epoch_id)
        return row

    def canary_epoch_rows(self, run_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM canary_epochs
                WHERE run_id=? ORDER BY cohort_name, sequence
                """,
                (run_id,),
            ).fetchall()

    def canary_rows_for_pair(self, pair_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM canary_leases
                WHERE pair_id=? ORDER BY interruptible DESC
                """,
                (pair_id,),
            ).fetchall()

    def canary_rows(self, run_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                """
                SELECT * FROM canary_leases
                WHERE run_id=? ORDER BY created_at, interruptible DESC
                """,
                (run_id,),
            ).fetchall()

    def start_canary(
        self,
        run_id: str,
        cell: TrialCell,
        *,
        pair_id: str,
        interruptible: bool,
        planned_end_at: str,
    ) -> str:
        lease_id = str(uuid.uuid4())
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO canary_leases
                  (id, run_id, provider, sku, gpu_type, region, datacenter,
                   resource_class, market, interruptible, pair_id, planned_end_at,
                   created_at, status, details_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'requested', '{}')
                """,
                (
                    lease_id,
                    run_id,
                    cell.provider,
                    cell.sku,
                    cell.gpu_type,
                    cell.region,
                    cell.datacenter,
                    cell.resource_class,
                    cell.market,
                    int(interruptible),
                    pair_id,
                    planned_end_at,
                    utc_now(),
                ),
            )
        return lease_id

    def mark_canary_created(
        self,
        lease_id: str,
        *,
        provider_resource_id: str,
        machine_id: str | None = None,
        datacenter: str | None = None,
        details: dict[str, Any] | None = None,
    ) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                """
                UPDATE canary_leases
                SET provider_resource_id=?, machine_id=?,
                    datacenter=COALESCE(?, datacenter), status='active', details_json=?
                WHERE id=?
                """,
                (
                    provider_resource_id,
                    machine_id,
                    datacenter,
                    json_text(details or {}),
                    lease_id,
                ),
            )

    def record_canary_observation(
        self,
        lease_id: str,
        *,
        provider_state: str | None,
        machine_id: str | None,
        runtime_uptime_seconds: float | None,
        realized_price_usd_hr: float | None,
        provider_location: str | None,
        heartbeat_ok: bool | None,
        heartbeat_latency_ms: int | None,
        heartbeat_version: str | None,
        host_observation_id: str | None = None,
        raw: Any = None,
        observed_at: str | None = None,
    ) -> str:
        observation_id = str(uuid.uuid4())
        at = observed_at or utc_now()
        with self._lock, self.connection:
            self.connection.execute(
                """
                INSERT INTO canary_observations
                  (id, lease_id, observed_at, provider_state, machine_id,
                   runtime_uptime_seconds, realized_price_usd_hr, provider_location,
                   heartbeat_ok, heartbeat_latency_ms, heartbeat_version,
                   host_observation_id, raw_json)
                VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                """,
                (
                    observation_id,
                    lease_id,
                    at,
                    provider_state,
                    machine_id,
                    runtime_uptime_seconds,
                    realized_price_usd_hr,
                    provider_location,
                    None if heartbeat_ok is None else int(heartbeat_ok),
                    heartbeat_latency_ms,
                    heartbeat_version,
                    host_observation_id,
                    json_text(raw or {}),
                ),
            )
            fields = [
                "machine_id=COALESCE(?, machine_id)",
                "datacenter=COALESCE(?, datacenter)",
            ]
            params: list[Any] = [machine_id, provider_location]
            if heartbeat_ok:
                fields.append("provider_ready_at=COALESCE(provider_ready_at, ?)")
                params.append(at)
                fields.append("last_heartbeat_at=?")
                params.append(at)
                fields.append("application_ready_at=COALESCE(application_ready_at, ?)")
                params.append(at)
                fields.append("status='application_ready'")
            elif provider_state in ("RUNNING", "active"):
                fields.append("provider_ready_at=COALESCE(provider_ready_at, ?)")
                params.append(at)
                fields.append(
                    "status=CASE WHEN status='application_ready' THEN status ELSE 'provider_ready' END"
                )
            params.append(lease_id)
            self.connection.execute(
                f"UPDATE canary_leases SET {', '.join(fields)} WHERE id=?",
                params,
            )
        return observation_id

    def finish_canary(
        self,
        lease_id: str,
        reason: str,
        *,
        event_at: str | None = None,
        operator_cleanup: bool = False,
    ) -> None:
        at = event_at or utc_now()
        with self._lock, self.connection:
            self.connection.execute(
                """
                UPDATE canary_leases
                SET event_at=?, terminal_reason=?, status='finished',
                    operator_cleanup_at=CASE WHEN ? THEN ? ELSE operator_cleanup_at END
                WHERE id=?
                """,
                (at, reason, int(operator_cleanup), at, lease_id),
            )

    def mark_canary_cleaned(self, lease_id: str) -> None:
        with self._lock, self.connection:
            self.connection.execute(
                "UPDATE canary_leases SET cleaned_at=?, status='cleaned' WHERE id=?",
                (utc_now(), lease_id),
            )

    def canary_row(self, lease_id: str) -> sqlite3.Row:
        with self._lock:
            row = self.connection.execute(
                "SELECT * FROM canary_leases WHERE id=?", (lease_id,)
            ).fetchone()
        if row is None:
            raise KeyError(lease_id)
        return row

    def canary_observation_rows(self, lease_id: str) -> list[sqlite3.Row]:
        with self._lock:
            return self.connection.execute(
                "SELECT * FROM canary_observations WHERE lease_id=? ORDER BY observed_at",
                (lease_id,),
            ).fetchall()
