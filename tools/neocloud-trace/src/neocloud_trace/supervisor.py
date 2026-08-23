from __future__ import annotations

import dataclasses
import datetime as dt
import json
import pathlib
import platform
import random
import threading
import time
from collections.abc import Callable, Mapping
from typing import Any

from .model import (
    ExperimentConfig,
    TerminalReason,
    TrialCell,
    validate_capacity_target,
)
from .providers import DeleteResult
from .runners.canary import CanaryRunner, CanarySample
from .runners.capacity import CapacityRunner, CapacityTrialResult
from .runners.passive import HostMonitor, PassiveWorker
from .storage import TraceStore


UTC = dt.timezone.utc


class ExecutionDisabled(RuntimeError):
    pass


class BudgetExceeded(RuntimeError):
    pass


@dataclasses.dataclass(frozen=True)
class CapacityTrialPlan:
    cell: TrialCell
    target_depth: int
    ready_timeout_seconds: float
    max_lease_seconds: float
    estimated_price_usd_hr: float
    require_application_ready: bool
    weight: float = 1.0
    billing_minimum_seconds: float = 0.0
    settlement_safety_multiplier: float = 1.0

    def __post_init__(self) -> None:
        validate_capacity_target(self.target_depth, self.cell.gpu_count)
        if self.ready_timeout_seconds <= 0 or self.max_lease_seconds <= 0:
            raise ValueError("capacity timeouts must be positive")
        if self.max_lease_seconds < self.ready_timeout_seconds:
            raise ValueError("capacity max lease must cover the ready timeout")
        if self.estimated_price_usd_hr <= 0:
            raise ValueError("capacity estimated price must be positive")
        if self.weight <= 0:
            raise ValueError("capacity selection weight must be positive")
        if self.billing_minimum_seconds < 0:
            raise ValueError("capacity billing minimum must not be negative")
        if self.settlement_safety_multiplier < 1:
            raise ValueError(
                "capacity settlement safety multiplier must be at least one"
            )

    @property
    def budget_upper_bound_usd(self) -> float:
        billable_seconds = max(self.max_lease_seconds, self.billing_minimum_seconds)
        return (
            self.target_depth
            // self.cell.gpu_count
            * self.estimated_price_usd_hr
            * billable_seconds
            * self.settlement_safety_multiplier
            / 3600.0
        )

    @property
    def target_instance_count(self) -> int:
        return self.target_depth // self.cell.gpu_count


@dataclasses.dataclass(frozen=True)
class CapacityCohortPlan:
    name: str
    cells: tuple[CapacityTrialPlan, ...]
    max_trials: int
    selection_seed: int
    initial_delay_seconds: float
    interval_seconds: float
    jitter_seconds: float
    min_remaining_seconds: float

    def __post_init__(self) -> None:
        if not self.name.strip():
            raise ValueError("capacity cohort name is required")
        if not self.cells:
            raise ValueError("capacity cohort must contain at least one cell")
        if self.max_trials != 1:
            raise ValueError("capacity cohort max_trials must be one")
        if self.initial_delay_seconds < 0 or self.jitter_seconds < 0:
            raise ValueError("capacity schedule delays must not be negative")
        if self.interval_seconds <= 0 or self.min_remaining_seconds <= 0:
            raise ValueError("capacity schedule intervals must be positive")
        if self.jitter_seconds * 2 >= self.interval_seconds:
            raise ValueError("capacity jitter must be less than half the interval")

    def select(self, rng: random.Random) -> tuple[CapacityTrialPlan, float]:
        total_weight = sum(cell.weight for cell in self.cells)
        selected = rng.choices(
            self.cells, weights=[cell.weight for cell in self.cells], k=1
        )[0]
        return selected, selected.weight / total_weight


@dataclasses.dataclass(frozen=True)
class CanaryTrialPlan:
    cell: TrialCell
    duration_seconds: float
    interval_seconds: float
    interruptible_price_usd_hr: float
    on_demand_price_usd_hr: float
    weight: float = 1.0
    billing_minimum_seconds: float = 0.0
    settlement_safety_multiplier: float = 1.0

    def __post_init__(self) -> None:
        if self.duration_seconds <= 0 or self.interval_seconds <= 0:
            raise ValueError("canary duration and interval must be positive")
        if self.interruptible_price_usd_hr <= 0 or self.on_demand_price_usd_hr <= 0:
            raise ValueError("canary estimated prices must be positive")
        if self.weight <= 0:
            raise ValueError("canary selection weight must be positive")
        if self.billing_minimum_seconds < 0:
            raise ValueError("billing_minimum_seconds must not be negative")
        if self.settlement_safety_multiplier < 1:
            raise ValueError("settlement_safety_multiplier must be at least one")

    @property
    def budget_upper_bound_usd(self) -> float:
        hourly = self.interruptible_price_usd_hr + self.on_demand_price_usd_hr
        billable_seconds = max(self.duration_seconds, self.billing_minimum_seconds)
        return hourly * billable_seconds * self.settlement_safety_multiplier / 3600.0


@dataclasses.dataclass(frozen=True)
class CanaryCohortPlan:
    name: str
    cells: tuple[CanaryTrialPlan, ...]
    max_pairs: int
    selection_seed: int
    replacement_delay_seconds: float
    min_remaining_seconds: float

    def __post_init__(self) -> None:
        if not self.name.strip():
            raise ValueError("canary cohort name is required")
        if not self.cells:
            raise ValueError("canary cohort must contain at least one cell")
        if self.max_pairs <= 0:
            raise ValueError("canary cohort max_pairs must be positive")
        if self.replacement_delay_seconds < 0:
            raise ValueError("replacement_delay_seconds must not be negative")
        if self.min_remaining_seconds <= 0:
            raise ValueError("min_remaining_seconds must be positive")

    def select(self, rng: random.Random) -> tuple[CanaryTrialPlan, float]:
        total_weight = sum(cell.weight for cell in self.cells)
        selected = rng.choices(
            self.cells, weights=[cell.weight for cell in self.cells], k=1
        )[0]
        return selected, selected.weight / total_weight


@dataclasses.dataclass(frozen=True)
class StabilitySchedulePlan:
    window_count: int
    schedule_seed: int
    initial_delay_seconds: float
    interval_seconds: float
    jitter_seconds: float

    def __post_init__(self) -> None:
        if self.window_count <= 0:
            raise ValueError("stability window_count must be positive")
        if self.initial_delay_seconds < 0:
            raise ValueError("stability initial_delay_seconds must not be negative")
        if self.interval_seconds <= 0:
            raise ValueError("stability interval_seconds must be positive")
        if self.jitter_seconds < 0:
            raise ValueError("stability jitter_seconds must not be negative")

    def offsets(self) -> tuple[float, ...]:
        rng = random.Random(self.schedule_seed)
        return tuple(
            max(
                0.0,
                self.initial_delay_seconds
                + sequence * self.interval_seconds
                + (
                    rng.uniform(-self.jitter_seconds, self.jitter_seconds)
                    if self.jitter_seconds
                    else 0.0
                ),
            )
            for sequence in range(self.window_count)
        )


@dataclasses.dataclass(frozen=True)
class CanaryTrialResult:
    pair_id: str
    cleanup_verified: bool
    estimated_cost_usd: float | None
    censor_reasons: tuple[str, ...]


@dataclasses.dataclass(frozen=True)
class CapacityEpochExecution:
    trial_result: CapacityTrialResult | None
    estimated_cost_usd: float | None
    cleanup_verified: bool
    stop_reason: str


@dataclasses.dataclass(frozen=True)
class PilotPlan:
    experiment: ExperimentConfig
    duration_seconds: float
    passive_interval_seconds: float
    host_interval_seconds: float
    network_control_url: str | None
    gpu_counts: tuple[int, ...]
    capacity_cohorts: tuple[CapacityCohortPlan, ...]
    canary_cohorts: tuple[CanaryCohortPlan, ...]
    stability_schedule: StabilitySchedulePlan
    raw_document: dict[str, Any]

    def __post_init__(self) -> None:
        for value, name in (
            (self.duration_seconds, "duration_seconds"),
            (self.passive_interval_seconds, "passive_interval_seconds"),
            (self.host_interval_seconds, "host_interval_seconds"),
        ):
            if value <= 0:
                raise ValueError(f"{name} must be positive")
        if not self.gpu_counts or any(count <= 0 for count in self.gpu_counts):
            raise ValueError("gpu_counts must contain positive values")
        if len(self.capacity_cohorts) > 1 or len(self.canary_cohorts) > 1:
            raise ValueError("a plan accepts at most one active cohort")
        if bool(self.capacity_cohorts) == bool(self.canary_cohorts):
            raise ValueError(
                "a plan requires exactly one capacity_cohorts or canary_cohorts entry"
            )
        for cohort in self.capacity_cohorts:
            for trial in cohort.cells:
                provider = trial.cell.provider
                if trial.target_depth > self.experiment.max_active_gpus_per_provider:
                    raise ValueError("capacity cohort target exceeds provider GPU cap")
                if provider not in self.experiment.provider_budgets:
                    raise ValueError(f"missing provider budget for {provider}")
                if (
                    trial.budget_upper_bound_usd
                    > self.experiment.provider_budgets[provider] + 1e-9
                ):
                    raise ValueError(
                        f"one {cohort.name} trial exceeds the {provider} provider budget"
                    )
                if (
                    trial.budget_upper_bound_usd
                    > self.experiment.total_budget_usd + 1e-9
                ):
                    raise ValueError(
                        f"one {cohort.name} trial exceeds the total budget"
                    )
        for cohort in self.canary_cohorts:
            for trial in cohort.cells:
                if self.experiment.max_active_gpus_per_provider < 2:
                    raise ValueError(
                        "matched canaries require a provider GPU cap of at least two"
                    )
                provider = trial.cell.provider
                if provider not in self.experiment.provider_budgets:
                    raise ValueError(f"missing provider budget for {provider}")
                if (
                    trial.budget_upper_bound_usd
                    > self.experiment.provider_budgets[provider] + 1e-9
                ):
                    raise ValueError(
                        f"one {cohort.name} pair exceeds the {provider} provider budget"
                    )
                if (
                    trial.budget_upper_bound_usd
                    > self.experiment.total_budget_usd + 1e-9
                ):
                    raise ValueError(f"one {cohort.name} pair exceeds the total budget")
        if self.canary_cohorts:
            offsets = self.stability_schedule.offsets()
            for previous, current in zip(offsets, offsets[1:]):
                if current < previous + self.duration_seconds:
                    raise ValueError(
                        "stability windows must not overlap; increase "
                        "interval_seconds or reduce jitter_seconds"
                    )


def _trial_cell(document: Mapping[str, Any]) -> TrialCell:
    return TrialCell(
        provider=str(document["provider"]),
        sku=str(document["sku"]),
        gpu_type=str(document["gpu_type"]),
        region=document.get("region"),
        datacenter=document.get("datacenter"),
        resource_class=str(document["resource_class"]),
        market=str(document["market"]),
        gpu_count=int(document.get("gpu_count", 1)),
        image=document.get("image"),
    )


def _canary_trial_plan(
    document: Mapping[str, Any],
    *,
    default_interval_seconds: float,
) -> CanaryTrialPlan:
    return CanaryTrialPlan(
        cell=_trial_cell(document),
        duration_seconds=float(document["duration_seconds"]),
        interval_seconds=float(
            document.get("interval_seconds") or default_interval_seconds
        ),
        interruptible_price_usd_hr=float(document["interruptible_price_usd_hr"]),
        on_demand_price_usd_hr=float(document["on_demand_price_usd_hr"]),
        weight=float(document.get("weight") or 1.0),
        billing_minimum_seconds=float(document.get("billing_minimum_seconds") or 0),
        settlement_safety_multiplier=float(
            document.get("settlement_safety_multiplier") or 1.0
        ),
    )


def _capacity_trial_plan(document: Mapping[str, Any]) -> CapacityTrialPlan:
    target_gpu_depth = document.get("target_gpu_depth")
    legacy_target_depth = document.get("target_depth")
    if target_gpu_depth is None:
        if legacy_target_depth is None:
            raise ValueError("capacity cell requires target_gpu_depth")
        if int(document.get("gpu_count", 1)) != 1:
            raise ValueError(
                "multi-GPU capacity cells must use target_gpu_depth, not target_depth"
            )
        target_gpu_depth = legacy_target_depth
    elif legacy_target_depth is not None and int(legacy_target_depth) != int(
        target_gpu_depth
    ):
        raise ValueError("target_gpu_depth and target_depth disagree")
    return CapacityTrialPlan(
        cell=_trial_cell(document),
        target_depth=int(target_gpu_depth),
        ready_timeout_seconds=float(document["ready_timeout_seconds"]),
        max_lease_seconds=float(document["max_lease_seconds"]),
        estimated_price_usd_hr=float(document["estimated_price_usd_hr"]),
        require_application_ready=bool(document.get("require_application_ready", True)),
        weight=float(document.get("weight") or 1.0),
        billing_minimum_seconds=float(document.get("billing_minimum_seconds") or 0),
        settlement_safety_multiplier=float(
            document.get("settlement_safety_multiplier") or 1.0
        ),
    )


def load_plan(
    path: str | pathlib.Path,
    *,
    default_host_id: str | None = None,
    default_vantage_id: str | None = None,
) -> PilotPlan:
    raw = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
    if not isinstance(raw, dict):
        raise ValueError("pilot plan must be a JSON object")
    removed_sections = (
        "capacity_trials",
        "canary_trials",
        "completion_window_cohorts",
        "control_cohorts",
    )
    present = [section for section in removed_sections if raw.get(section)]
    if present:
        raise ValueError("unsupported active plan sections: " + ", ".join(present))
    provider_budgets = {
        str(provider): float(amount)
        for provider, amount in (raw.get("provider_budgets_usd") or {}).items()
    }
    experiment = ExperimentConfig(
        protocol_version=str(raw.get("protocol_version") or ""),
        host_id=str(raw.get("host_id") or default_host_id or ""),
        vantage_id=str(raw.get("vantage_id") or default_vantage_id or ""),
        provider_budgets=provider_budgets,
        total_budget_usd=float(raw.get("total_budget_usd") or 0),
        max_active_gpus_per_provider=int(raw.get("max_active_gpus_per_provider") or 0),
    )
    capacity_cohorts = tuple(
        CapacityCohortPlan(
            name=str(item["name"]),
            cells=tuple(_capacity_trial_plan(cell) for cell in item.get("cells") or []),
            max_trials=int(item["max_trials"]),
            selection_seed=int(item["selection_seed"]),
            initial_delay_seconds=float(item.get("initial_delay_seconds") or 0),
            interval_seconds=float(item["interval_seconds"]),
            jitter_seconds=float(item.get("jitter_seconds") or 0),
            min_remaining_seconds=float(item["min_remaining_seconds"]),
        )
        for item in raw.get("capacity_cohorts") or []
    )
    default_canary_interval = float(raw.get("canary_interval_seconds") or 60)
    canary_cohorts = tuple(
        CanaryCohortPlan(
            name=str(item["name"]),
            cells=tuple(
                _canary_trial_plan(
                    cell,
                    default_interval_seconds=default_canary_interval,
                )
                for cell in item.get("cells") or []
            ),
            max_pairs=int(item["max_pairs"]),
            selection_seed=int(item["selection_seed"]),
            replacement_delay_seconds=float(item.get("replacement_delay_seconds") or 0),
            min_remaining_seconds=float(
                item.get("min_remaining_seconds") or default_canary_interval
            ),
        )
        for item in raw.get("canary_cohorts") or []
    )
    duration_seconds = float(raw.get("duration_seconds") or 0)
    schedule_document = raw.get("stability_schedule") or {}
    if not isinstance(schedule_document, dict):
        raise ValueError("stability_schedule must be an object")
    stability_schedule = StabilitySchedulePlan(
        window_count=int(schedule_document.get("window_count", 1)),
        schedule_seed=int(
            schedule_document.get("schedule_seed")
            if schedule_document.get("schedule_seed") is not None
            else 0
        ),
        initial_delay_seconds=float(schedule_document.get("initial_delay_seconds", 0)),
        interval_seconds=float(
            schedule_document.get("interval_seconds", duration_seconds)
        ),
        jitter_seconds=float(schedule_document.get("jitter_seconds", 0)),
    )
    return PilotPlan(
        experiment=experiment,
        duration_seconds=duration_seconds,
        passive_interval_seconds=float(raw.get("passive_interval_seconds") or 0),
        host_interval_seconds=float(raw.get("host_interval_seconds") or 0),
        network_control_url=raw.get("network_control_url"),
        gpu_counts=tuple(int(value) for value in (raw.get("gpu_counts") or [1])),
        capacity_cohorts=capacity_cohorts,
        canary_cohorts=canary_cohorts,
        stability_schedule=stability_schedule,
        raw_document=raw,
    )


class BudgetGuard:
    def __init__(
        self,
        store: TraceStore,
        run_id: str,
        config: ExperimentConfig,
    ) -> None:
        self.store = store
        self.run_id = run_id
        self.config = config
        self._lock = threading.Lock()

    def reserve(
        self, provider: str, amount_usd: float, phase: str, reference: str
    ) -> None:
        if amount_usd < 0:
            raise ValueError("reservation must not be negative")
        if provider not in self.config.provider_budgets:
            raise BudgetExceeded(f"provider {provider} has no configured budget")
        with self._lock:
            existing = self.store.budget_reference_rows(
                self.run_id, provider, reference
            )
            if any(row["kind"] == "settled_runtime_estimate" for row in existing):
                raise ValueError(f"budget reference {reference} is already settled")
            if any(row["kind"] == "committed_upper_bound" for row in existing):
                return
            provider_after = (
                self.store.provider_spend(self.run_id, provider) + amount_usd
            )
            total_after = self.store.total_spend(self.run_id) + amount_usd
            if provider_after > self.config.provider_budgets[provider] + 1e-9:
                raise BudgetExceeded(
                    f"{provider} upper-bound spend {provider_after:.4f} exceeds "
                    f"budget {self.config.provider_budgets[provider]:.4f}"
                )
            if total_after > self.config.total_budget_usd + 1e-9:
                raise BudgetExceeded(
                    f"total upper-bound spend {total_after:.4f} exceeds "
                    f"budget {self.config.total_budget_usd:.4f}"
                )
            self.store.record_budget(
                self.run_id,
                provider,
                amount_usd,
                "committed_upper_bound",
                reference,
                {"phase": phase},
            )

    def settle(
        self,
        provider: str,
        reference: str,
        actual_amount_usd: float,
        *,
        details: dict[str, Any] | None = None,
    ) -> None:
        if actual_amount_usd < 0:
            raise ValueError("settled budget amount must not be negative")
        with self._lock:
            rows = self.store.budget_reference_rows(self.run_id, provider, reference)
            if any(row["kind"] == "settled_runtime_estimate" for row in rows):
                return
            committed = sum(
                float(row["amount_usd"])
                for row in rows
                if row["kind"] == "committed_upper_bound"
            )
            if committed <= 0:
                raise ValueError(f"budget reference {reference} has no commitment")
            release = max(0.0, committed - actual_amount_usd)
            overage = max(0.0, actual_amount_usd - committed)
            settlement_details = {
                **(details or {}),
                "committed_upper_bound_usd": committed,
                "actual_amount_usd": actual_amount_usd,
            }
            if release:
                self.store.record_budget_release(
                    self.run_id,
                    provider,
                    release,
                    reference,
                    settlement_details,
                )
            if overage:
                self.store.record_budget(
                    self.run_id,
                    provider,
                    overage,
                    "runtime_overage",
                    reference,
                    settlement_details,
                )
            self.store.record_budget(
                self.run_id,
                provider,
                0.0,
                "settled_runtime_estimate",
                reference,
                settlement_details,
            )


class PilotSupervisor:
    def __init__(
        self,
        store: TraceStore,
        providers: Mapping[str, object],
        *,
        capacity_runner_factory: Callable[..., object] = CapacityRunner,
        canary_runner_factory: Callable[..., CanaryRunner] = CanaryRunner,
        monotonic: Callable[[], float] = time.monotonic,
    ) -> None:
        self.store = store
        self.providers = dict(providers)
        self.capacity_runner_factory = capacity_runner_factory
        self.canary_runner_factory = canary_runner_factory
        self.monotonic = monotonic
        self.stop_event = threading.Event()
        self._latest_host_lock = threading.Lock()
        self._latest_host_id: str | None = None
        self._latest_host_healthy = True
        self._worker_errors: list[str] = []
        self._seen_cleanup_api_calls: set[str] = set()

    def request_stop(self) -> None:
        self.stop_event.set()

    def capacity_tick(
        self,
        plan: PilotPlan,
        *,
        experiment_id: str,
        execute: bool = False,
        now: dt.datetime | None = None,
    ) -> dict[str, Any]:
        if not execute:
            raise ExecutionDisabled("capacity tick requires execute=True")
        try:
            cohort = plan.capacity_cohorts[0]
        except IndexError as error:
            raise ValueError("capacity tick requires one capacity cohort") from error
        required_providers = {trial.cell.provider for trial in cohort.cells}
        missing = sorted(required_providers - self.providers.keys())
        if missing:
            raise ValueError(f"missing provider clients: {', '.join(missing)}")
        current = now or dt.datetime.now(UTC)
        if current.tzinfo is None:
            raise ValueError("capacity-tick now must be timezone-aware")
        current = current.astimezone(UTC)

        rng = random.Random(cohort.selection_seed)
        selected = (cohort.select(rng),)
        epochs: list[dict[str, Any]] = []
        for sequence, (trial, probability) in enumerate(selected):
            jitter = (
                rng.uniform(-cohort.jitter_seconds, cohort.jitter_seconds)
                if cohort.jitter_seconds
                else 0.0
            )
            offset = max(
                0.0,
                cohort.initial_delay_seconds
                + sequence * cohort.interval_seconds
                + jitter,
            )
            epochs.append(
                {
                    "sequence": sequence,
                    "cell": trial.cell,
                    "target_depth": trial.target_depth,
                    "scheduled_offset_seconds": offset,
                    "scheduled_at": (
                        current + dt.timedelta(seconds=offset)
                    ).isoformat(),
                    "selection_probability": probability,
                    "selection_seed": cohort.selection_seed,
                    "details": {
                        "gpu_count_per_instance": trial.cell.gpu_count,
                        "target_gpu_depth": trial.target_depth,
                        "target_instance_count": trial.target_instance_count,
                        "estimated_price_usd_hr": trial.estimated_price_usd_hr,
                        "max_lease_seconds": trial.max_lease_seconds,
                        "billing_minimum_seconds": trial.billing_minimum_seconds,
                        "settlement_safety_multiplier": (
                            trial.settlement_safety_multiplier
                        ),
                        "selection_weight": trial.weight,
                    },
                }
            )
        run_id, experiment_created = self.store.ensure_capacity_experiment(
            plan.experiment,
            plan.raw_document,
            experiment_id=experiment_id,
            cohort_name=cohort.name,
            started_at=current.isoformat(),
            epochs=epochs,
            python_version=platform.python_version(),
        )

        experiment = self.store.capacity_experiment_row(experiment_id)
        if experiment["status"] == "complete":
            return {
                "experiment_id": experiment_id,
                "experiment_created": experiment_created,
                "run_id": run_id,
                "cohort_name": cohort.name,
                "status": "complete",
                "epoch_id": None,
            }

        expected_monotonic = self.monotonic()
        host = HostMonitor(
            self.store,
            network_control_url=plan.network_control_url,
            monotonic=self.monotonic,
        ).sample(run_id, expected_monotonic)
        PassiveWorker(self.store, self.providers.values()).run_once(
            run_id,
            gpu_counts=plan.gpu_counts,
            host_observation_id=host.id,
        )
        claim_status, epoch = self.store.claim_due_capacity_epoch(
            experiment_id,
            due_at=current.isoformat(),
            expired_before=(
                current - dt.timedelta(seconds=cohort.interval_seconds)
            ).isoformat(),
        )
        if claim_status == "complete":
            self.store.finish_capacity_experiment_if_complete(experiment_id)
        if claim_status != "claimed" or epoch is None:
            return {
                "experiment_id": experiment_id,
                "experiment_created": experiment_created,
                "run_id": run_id,
                "cohort_name": cohort.name,
                "status": claim_status,
                "epoch_id": epoch["id"] if epoch is not None else None,
            }

        sequence = int(epoch["sequence"])
        try:
            trial = selected[sequence][0]
        except IndexError as error:
            raise RuntimeError(
                f"persisted capacity epoch sequence {sequence} is not in the plan"
            ) from error
        if (
            epoch["provider"] != trial.cell.provider
            or epoch["sku"] != trial.cell.sku
            or int(epoch["target_depth"]) != trial.target_depth
        ):
            raise RuntimeError("persisted capacity epoch does not match the plan")

        scheduled_at = dt.datetime.fromisoformat(epoch["scheduled_at"])
        started_lag_seconds = max(
            0.0,
            (current - scheduled_at.astimezone(UTC)).total_seconds(),
        )
        guard = BudgetGuard(self.store, run_id, plan.experiment)
        try:
            execution = self._execute_capacity_epoch(
                run_id,
                epoch["id"],
                trial,
                guard=guard,
                scheduled_target_monotonic=(self.monotonic() - started_lag_seconds),
            )
        except Exception:
            self._cleanup_owned(run_id)
            raise
        experiment_complete = self.store.finish_capacity_experiment_if_complete(
            experiment_id
        )
        return {
            "experiment_id": experiment_id,
            "experiment_created": experiment_created,
            "run_id": run_id,
            "cohort_name": cohort.name,
            "status": "executed",
            "epoch_id": epoch["id"],
            "epoch_sequence": sequence,
            "trial_status": (
                execution.trial_result.status
                if execution.trial_result is not None
                else execution.stop_reason
            ),
            "cleanup_verified": execution.cleanup_verified,
            "experiment_complete": experiment_complete,
        }

    def stability_run(
        self,
        plan: PilotPlan,
        *,
        experiment_id: str,
        execute: bool = False,
        now: dt.datetime | None = None,
    ) -> dict[str, Any]:
        if not execute:
            raise ExecutionDisabled("stability run requires execute=True")
        if len(plan.canary_cohorts) != 1:
            raise ValueError("stability run requires exactly one canary cohort")
        cohort = plan.canary_cohorts[0]
        required_providers = {trial.cell.provider for trial in cohort.cells}
        missing = sorted(required_providers - self.providers.keys())
        if missing:
            raise ValueError(f"missing provider clients: {', '.join(missing)}")
        current = now or dt.datetime.now(UTC)
        if current.tzinfo is None:
            raise ValueError("stability run now must be timezone-aware")
        current = current.astimezone(UTC)

        windows = []
        for sequence, offset in enumerate(plan.stability_schedule.offsets()):
            scheduled_at = current + dt.timedelta(seconds=offset)
            windows.append(
                {
                    "sequence": sequence,
                    "scheduled_offset_seconds": offset,
                    "scheduled_at": scheduled_at.isoformat(),
                    "scheduled_end_at": (
                        scheduled_at + dt.timedelta(seconds=plan.duration_seconds)
                    ).isoformat(),
                    "details": {
                        "schedule_seed": plan.stability_schedule.schedule_seed,
                        "window_duration_seconds": plan.duration_seconds,
                        "max_pairs": cohort.max_pairs,
                    },
                }
            )
        run_id, experiment_created = self.store.ensure_stability_experiment(
            plan.experiment,
            plan.raw_document,
            experiment_id=experiment_id,
            cohort_name=cohort.name,
            started_at=current.isoformat(),
            windows=windows,
            python_version=platform.python_version(),
        )
        experiment = self.store.stability_experiment_row(experiment_id)
        if experiment["status"] != "running":
            return {
                "experiment_id": experiment_id,
                "experiment_created": experiment_created,
                "run_id": run_id,
                "cohort_name": cohort.name,
                "status": str(experiment["status"]),
                "window_id": None,
            }

        expected_monotonic = self.monotonic()
        host_monitor = HostMonitor(
            self.store,
            network_control_url=plan.network_control_url,
            monotonic=self.monotonic,
        )
        passive = PassiveWorker(self.store, self.providers.values())
        initial_host = host_monitor.sample(run_id, expected_monotonic)
        with self._latest_host_lock:
            self._latest_host_id = initial_host.id
            self._latest_host_healthy = initial_host.healthy
        passive.run_once(
            run_id,
            gpu_counts=plan.gpu_counts,
            host_observation_id=initial_host.id,
        )

        claim_status, window = self.store.claim_due_stability_window(
            experiment_id,
            due_at=current.isoformat(),
        )
        if claim_status == "complete":
            self.store.finish_stability_experiment_if_complete(experiment_id)
        if claim_status != "claimed" or window is None:
            return {
                "experiment_id": experiment_id,
                "experiment_created": experiment_created,
                "run_id": run_id,
                "cohort_name": cohort.name,
                "status": claim_status,
                "window_id": window["id"] if window is not None else None,
            }

        sequence = int(window["sequence"])
        if sequence < 0 or sequence >= plan.stability_schedule.window_count:
            raise RuntimeError(
                f"persisted stability window sequence {sequence} is not in the plan"
            )
        expected_offset = plan.stability_schedule.offsets()[sequence]
        if abs(float(window["scheduled_offset_seconds"]) - expected_offset) > 1e-6:
            raise RuntimeError("persisted stability window does not match the plan")
        scheduled_end = dt.datetime.fromisoformat(window["scheduled_end_at"])
        remaining_seconds = (scheduled_end.astimezone(UTC) - current).total_seconds()
        if remaining_seconds <= 0:
            raise RuntimeError("claimed stability window has already ended")
        if remaining_seconds < cohort.min_remaining_seconds:
            self.store.finish_stability_window(
                window["id"],
                status="missed",
                stop_reason="external_scheduler_late",
                details={
                    "remaining_seconds_at_claim": remaining_seconds,
                    "minimum_remaining_seconds": cohort.min_remaining_seconds,
                },
            )
            experiment_complete = self.store.finish_stability_experiment_if_complete(
                experiment_id
            )
            return {
                "experiment_id": experiment_id,
                "experiment_created": experiment_created,
                "run_id": run_id,
                "cohort_name": cohort.name,
                "status": "missed",
                "window_id": window["id"],
                "window_sequence": sequence,
                "window_status": "missed",
                "experiment_complete": experiment_complete,
            }
        hard_deadline = self.monotonic() + remaining_seconds
        self.store.mark_stability_window_started(
            window["id"],
            started_at=current.isoformat(),
        )

        guard = BudgetGuard(self.store, run_id, plan.experiment)
        threads = [
            threading.Thread(
                target=self._host_loop,
                args=(run_id, host_monitor, plan.host_interval_seconds),
                name="neocloud-stability-host",
                daemon=True,
            ),
            threading.Thread(
                target=self._passive_loop,
                args=(
                    run_id,
                    passive,
                    plan.passive_interval_seconds,
                    plan.gpu_counts,
                ),
                name="neocloud-stability-passive",
                daemon=True,
            ),
        ]
        window_status = "complete"
        stop_reason: str | None = "window_elapsed"
        caught_error: Exception | None = None
        started_threads: list[threading.Thread] = []
        try:
            for thread in threads:
                thread.start()
                started_threads.append(thread)
            completed = self._run_canary_cohort(
                run_id,
                cohort,
                guard=guard,
                hard_deadline=hard_deadline,
                sequence_start=len(self.store.canary_epoch_rows(run_id)),
                stability_window_id=window["id"],
            )
            if not completed:
                epochs = self.store.stability_window_epoch_rows(window["id"])
                last_status = epochs[-1]["status"] if epochs else "failed"
                if last_status == "budget_exhausted":
                    window_status = "budget_exhausted"
                    stop_reason = "budget_exhausted"
                    self.store.stop_remaining_stability_windows(
                        run_id,
                        status="not_run_budget_exhausted",
                        stop_reason="budget_exhausted",
                    )
                else:
                    window_status = "cleanup_incomplete"
                    stop_reason = "provider resources remain unresolved"
            if window_status == "complete":
                self.stop_event.wait(max(0.0, hard_deadline - self.monotonic()))
                if self.stop_event.is_set():
                    window_status = "stopped"
                    stop_reason = "operator_stop"
                    self.store.stop_remaining_stability_windows(
                        run_id,
                        status="not_run_operator_stop",
                        stop_reason="operator_stop",
                    )
        except Exception as error:
            window_status = "failed"
            stop_reason = f"{type(error).__name__}: {error}"
            caught_error = error
        finally:
            self.stop_event.set()
            join_timeout = (
                max(
                    plan.host_interval_seconds,
                    plan.passive_interval_seconds,
                )
                + 5
            )
            for thread in started_threads:
                thread.join(timeout=join_timeout)
            censor_reason = (
                TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS
                if window_status == "failed"
                else TerminalReason.OPERATOR_CLEANUP
            )
            self._censor_unfinished_canaries(run_id, censor_reason)
            self._cleanup_owned(run_id)
            unfinished_status = (
                "collector_loss"
                if window_status == "failed"
                else (
                    "operator_stop"
                    if window_status == "stopped"
                    else "cleanup_incomplete"
                )
            )
            self._finish_unfinished_canary_epochs(
                run_id,
                status=unfinished_status,
                reason=censor_reason.value,
                cleanup_verified=not self.store.owned_resources(run_id),
            )
            self._settle_cleaned_canary_epochs(run_id, guard)
            if self.store.owned_resources(run_id):
                window_status = "cleanup_incomplete"
                stop_reason = "provider resources remain unresolved"
            elif self._worker_errors and window_status == "complete":
                window_status = "complete_with_sampling_errors"
                stop_reason = "; ".join(self._worker_errors[-5:])
            self.store.finish_stability_window(
                window["id"],
                status=window_status,
                stop_reason=stop_reason,
                details={
                    "worker_errors": self._worker_errors,
                    "cleanup_verified": not self.store.owned_resources(run_id),
                },
            )

        finish_options: dict[str, str | None] = {}
        if window_status == "budget_exhausted":
            finish_options = {
                "status": "budget_exhausted",
                "termination_reason": "budget_exhausted",
            }
        elif window_status == "stopped":
            finish_options = {
                "status": "stopped",
                "termination_reason": "operator_stop",
            }
        elif window_status == "cleanup_incomplete":
            finish_options = {
                "status": "cleanup_incomplete",
                "termination_reason": "provider resources remain unresolved",
            }
        elif window_status == "complete_with_sampling_errors":
            finish_options = {
                "status": "complete_with_sampling_errors",
                "termination_reason": stop_reason,
            }
        elif window_status == "failed":
            finish_options = {
                "status": "failed",
                "termination_reason": stop_reason,
            }
        experiment_complete = self.store.finish_stability_experiment_if_complete(
            experiment_id,
            **finish_options,
        )
        if caught_error is not None:
            raise caught_error
        return {
            "experiment_id": experiment_id,
            "experiment_created": experiment_created,
            "run_id": run_id,
            "cohort_name": cohort.name,
            "status": "executed",
            "window_id": window["id"],
            "window_sequence": sequence,
            "window_status": window_status,
            "experiment_complete": experiment_complete,
        }

    def _host_loop(self, run_id: str, monitor: HostMonitor, interval: float) -> None:
        expected = self.monotonic()
        while not self.stop_event.is_set():
            try:
                observation = monitor.sample(run_id, expected)
                with self._latest_host_lock:
                    self._latest_host_id = observation.id
                    self._latest_host_healthy = observation.healthy
            except Exception as error:  # noqa: BLE001 - persisted run reports worker faults
                self._worker_errors.append(f"host:{type(error).__name__}:{error}")
            expected += interval
            self.stop_event.wait(max(0.0, expected - self.monotonic()))

    def _passive_loop(
        self,
        run_id: str,
        worker: PassiveWorker,
        interval: float,
        gpu_counts: tuple[int, ...],
    ) -> None:
        next_sample = self.monotonic()
        while not self.stop_event.is_set():
            try:
                with self._latest_host_lock:
                    host_id = self._latest_host_id
                worker.run_once(
                    run_id,
                    gpu_counts=gpu_counts,
                    host_observation_id=host_id,
                )
            except Exception as error:  # noqa: BLE001 - provider/API faults are trace data
                self._worker_errors.append(f"passive:{type(error).__name__}:{error}")
            next_sample += interval
            self.stop_event.wait(max(0.0, next_sample - self.monotonic()))

    def _execute_capacity_epoch(
        self,
        run_id: str,
        epoch_id: str,
        trial: CapacityTrialPlan,
        *,
        guard: BudgetGuard,
        scheduled_target_monotonic: float,
    ) -> CapacityEpochExecution:
        try:
            guard.reserve(
                trial.cell.provider,
                trial.budget_upper_bound_usd,
                "capacity",
                epoch_id,
            )
        except BudgetExceeded as error:
            self.store.finish_capacity_epoch(
                epoch_id,
                status="budget_exhausted",
                stop_reason="budget_exhausted",
                details={"message": str(error)},
            )
            return CapacityEpochExecution(None, None, True, "budget_exhausted")

        started_lag = max(0.0, self.monotonic() - scheduled_target_monotonic)
        self.store.mark_capacity_epoch_started(
            epoch_id,
            trial_id=epoch_id,
            started_lag_seconds=started_lag,
        )
        runner = self.capacity_runner_factory(
            self.store,
            self.providers[trial.cell.provider],
            ready_timeout_seconds=trial.ready_timeout_seconds,
            require_application_ready=trial.require_application_ready,
        )
        try:
            result = runner.run_trial(
                run_id,
                trial.cell,
                target_depth=trial.target_depth,
                trial_id=epoch_id,
            )
        except Exception as error:
            self.store.finish_capacity_epoch(
                epoch_id,
                status="failed",
                stop_reason="runner_error",
                details={"error": f"{type(error).__name__}: {error}"},
            )
            raise

        estimated_cost, cleanup_verified = self._capacity_trial_cost(
            epoch_id,
            trial,
        )
        self.store.finish_capacity_epoch(
            epoch_id,
            status=result.status,
            stop_reason="trial_terminal",
            details={
                "estimated_cost_usd": estimated_cost,
                "cleanup_verified": cleanup_verified,
                "gpu_count_per_instance": result.gpu_count_per_instance,
                "target_gpu_depth": result.target_gpu_depth,
                "target_instance_count": result.target_instance_count,
                "accepted_instance_count": result.accepted_instance_count,
                "accepted_gpu_depth": result.accepted_gpu_depth,
                "provider_ready_gpu_depth": result.provider_ready_gpu_depth,
                "application_ready_gpu_depth": (result.application_ready_gpu_depth),
                "first_failed_gpu_stage": result.first_failed_stage,
            },
        )
        if not cleanup_verified or estimated_cost is None:
            return CapacityEpochExecution(
                result,
                estimated_cost,
                False,
                "cleanup_incomplete",
            )
        guard.settle(
            trial.cell.provider,
            epoch_id,
            estimated_cost,
            details={"cleanup_verified": True},
        )
        return CapacityEpochExecution(result, estimated_cost, True, "trial_terminal")

    def _capacity_trial_cost(
        self,
        trial_id: str,
        trial: CapacityTrialPlan,
    ) -> tuple[float | None, bool]:
        rows = self.store.capacity_replica_rows(trial_id)
        if any(row["provider_resource_id"] and not row["cleaned_at"] for row in rows):
            return None, False
        total = 0.0
        for row in rows:
            if not row["provider_resource_id"]:
                continue
            if not row["cleaned_at"]:
                return None, False
            price = row["realized_price_usd_hr"] or trial.estimated_price_usd_hr
            started_at = row["accepted_at"] or row["requested_at"]
            held_seconds = max(
                trial.billing_minimum_seconds,
                (
                    dt.datetime.fromisoformat(row["cleaned_at"])
                    - dt.datetime.fromisoformat(started_at)
                ).total_seconds(),
            )
            total += (
                held_seconds
                * float(price)
                * trial.settlement_safety_multiplier
                / 3600.0
            )
        return total, True

    def _execute_canary_epoch(
        self,
        run_id: str,
        trial: CanaryTrialPlan,
        *,
        cohort_name: str,
        sequence: int,
        selection_probability: float,
        selection_seed: int | None,
        guard: BudgetGuard,
        hard_deadline: float,
        stability_window_id: str | None = None,
    ) -> bool:
        epoch_id = self.store.start_canary_epoch(
            run_id,
            cohort_name=cohort_name,
            sequence=sequence,
            cell=trial.cell,
            planned_duration_seconds=trial.duration_seconds,
            interval_seconds=trial.interval_seconds,
            selection_probability=selection_probability,
            selection_seed=selection_seed,
            details={
                "interruptible_price_usd_hr": trial.interruptible_price_usd_hr,
                "on_demand_price_usd_hr": trial.on_demand_price_usd_hr,
                "selection_weight": trial.weight,
                "billing_minimum_seconds": trial.billing_minimum_seconds,
                "settlement_safety_multiplier": trial.settlement_safety_multiplier,
            },
        )
        if stability_window_id is not None:
            self.store.link_stability_window_epoch(stability_window_id, epoch_id)
        try:
            guard.reserve(
                trial.cell.provider,
                trial.budget_upper_bound_usd,
                "canary",
                epoch_id,
            )
        except BudgetExceeded as error:
            self.store.finish_canary_epoch(
                epoch_id,
                status="budget_exhausted",
                stop_reason="budget_exhausted",
                details={"message": str(error)},
            )
            return False

        try:
            result = self._run_canary_trial(
                run_id,
                trial,
                pair_id=epoch_id,
                hard_deadline=hard_deadline,
            )
        except Exception as error:
            self.store.finish_canary_epoch(
                epoch_id,
                status="failed",
                stop_reason="runner_error",
                details={"error": f"{type(error).__name__}: {error}"},
            )
            raise

        status = "complete" if result.cleanup_verified else "cleanup_incomplete"
        self.store.finish_canary_epoch(
            epoch_id,
            status=status,
            stop_reason="pair_terminal",
            details={
                "censor_reasons": list(result.censor_reasons),
                "estimated_cost_usd": result.estimated_cost_usd,
                "cleanup_verified": result.cleanup_verified,
            },
        )
        if result.cleanup_verified and result.estimated_cost_usd is not None:
            guard.settle(
                trial.cell.provider,
                epoch_id,
                result.estimated_cost_usd,
                details={"cleanup_verified": True},
            )
        return result.cleanup_verified

    def _run_canary_cohort(
        self,
        run_id: str,
        cohort: CanaryCohortPlan,
        *,
        guard: BudgetGuard,
        hard_deadline: float,
        sequence_start: int = 0,
        stability_window_id: str | None = None,
    ) -> bool:
        rng = random.Random(cohort.selection_seed)
        for _ in range(sequence_start):
            cohort.select(rng)
        for local_sequence in range(cohort.max_pairs):
            remaining = hard_deadline - self.monotonic()
            if self.stop_event.is_set() or remaining < cohort.min_remaining_seconds:
                return True
            trial, probability = cohort.select(rng)
            completed = self._execute_canary_epoch(
                run_id,
                trial,
                cohort_name=cohort.name,
                sequence=sequence_start + local_sequence,
                selection_probability=probability,
                selection_seed=cohort.selection_seed,
                guard=guard,
                hard_deadline=hard_deadline,
                stability_window_id=stability_window_id,
            )
            if not completed:
                return False
            if cohort.replacement_delay_seconds:
                self.stop_event.wait(cohort.replacement_delay_seconds)
        return True

    def _run_canary_trial(
        self,
        run_id: str,
        trial: CanaryTrialPlan,
        *,
        pair_id: str,
        hard_deadline: float,
    ) -> CanaryTrialResult:
        runner = self.canary_runner_factory(
            self.store, self.providers[trial.cell.provider]
        )
        now = dt.datetime.now(UTC)
        remaining = max(0.0, hard_deadline - self.monotonic())
        duration = min(trial.duration_seconds, remaining)
        if duration <= 0:
            raise RuntimeError("canary epoch has no remaining observation time")
        planned_end = now + dt.timedelta(seconds=duration)
        self.store.mark_canary_epoch_started(pair_id, planned_end.isoformat())
        leases = runner.start_pair(
            run_id,
            trial.cell,
            planned_end,
            pair_id=pair_id,
        )
        samples: dict[str, list[CanarySample]] = {lease.id: [] for lease in leases}
        active = {lease.id: lease for lease in leases if lease.provider_resource_id}
        pair_setup_failed = len(active) != len(leases)
        if pair_setup_failed:
            for lease in active.values():
                runner.stop(
                    run_id,
                    lease,
                    reason=TerminalReason.PAIR_SETUP_FAILURE,
                )
            active.clear()
        while (
            active
            and not self.stop_event.is_set()
            and dt.datetime.now(UTC) < planned_end
        ):
            with self._latest_host_lock:
                host_id = self._latest_host_id
                host_healthy = self._latest_host_healthy
            for lease_id, lease in list(active.items()):
                sample = runner.observe_once(
                    lease,
                    host_observation_id=host_id,
                    host_healthy=host_healthy,
                )
                samples[lease_id].append(sample)
                reason = runner.classifier.classify(
                    planned_end=planned_end,
                    now=sample.observed_at,
                    samples=samples[lease_id],
                    operator_cleanup=False,
                )
                if reason is not None:
                    runner.stop(run_id, lease, reason=reason)
                    active.pop(lease_id, None)
                    pair_established = all(
                        any(sample.heartbeat_ok is True for sample in samples[item.id])
                        for item in leases
                    )
                    if not pair_established:
                        for peer_id, peer in list(active.items()):
                            runner.stop(
                                run_id,
                                peer,
                                reason=TerminalReason.PAIR_SETUP_FAILURE,
                            )
                            active.pop(peer_id, None)
                        pair_setup_failed = True
                        break
            if pair_setup_failed:
                break
            if active:
                self.stop_event.wait(trial.interval_seconds)

        final_reason = (
            TerminalReason.OPERATOR_CLEANUP
            if self.stop_event.is_set()
            else TerminalReason.RIGHT_CENSORED_PLANNED_END
        )
        for lease in active.values():
            runner.stop(
                run_id,
                lease,
                reason=final_reason,
                operator_cleanup=self.stop_event.is_set(),
            )
        estimated_cost, cleanup_verified = self._canary_pair_cost(
            pair_id,
            trial=trial,
        )
        reasons = tuple(
            str(row["censor_reason"])
            for row in self.store.canary_rows_for_pair(pair_id)
            if row["censor_reason"]
        )
        return CanaryTrialResult(
            pair_id=pair_id,
            cleanup_verified=cleanup_verified,
            estimated_cost_usd=estimated_cost,
            censor_reasons=reasons,
        )

    def _canary_pair_cost(
        self,
        pair_id: str,
        *,
        trial: CanaryTrialPlan | None = None,
    ) -> tuple[float | None, bool]:
        rows = self.store.canary_rows_for_pair(pair_id)
        if not rows:
            return None, False
        if any(row["provider_resource_id"] and not row["cleaned_at"] for row in rows):
            return None, False
        epoch = self.store.canary_epoch_row(pair_id)
        epoch_details = json.loads(epoch["details_json"])
        billing_minimum_seconds = float(
            epoch_details.get("billing_minimum_seconds") or 0
        )
        safety_multiplier = float(
            epoch_details.get("settlement_safety_multiplier") or 1.0
        )
        total = 0.0
        for row in rows:
            if not row["provider_resource_id"]:
                continue
            terminal_at = row["cleaned_at"] or row["event_at"]
            if not terminal_at:
                return None, False
            lease_details = json.loads(row["details_json"])
            price = lease_details.get("price_usd_hr")
            if price is None:
                key = (
                    "interruptible_price_usd_hr"
                    if row["interruptible"]
                    else "on_demand_price_usd_hr"
                )
                price = epoch_details.get(key)
            if price is None and trial is not None:
                price = (
                    trial.interruptible_price_usd_hr
                    if row["interruptible"]
                    else trial.on_demand_price_usd_hr
                )
            if price is None:
                return None, True
            started = dt.datetime.fromisoformat(row["created_at"])
            finished = dt.datetime.fromisoformat(terminal_at)
            held_seconds = max(
                billing_minimum_seconds,
                max(0.0, (finished - started).total_seconds()),
            )
            total += held_seconds * float(price) * safety_multiplier / 3600.0
        return total, True

    def _settle_cleaned_canary_epochs(
        self,
        run_id: str,
        guard: BudgetGuard,
    ) -> None:
        for epoch in self.store.canary_epoch_rows(run_id):
            budget_rows = self.store.budget_reference_rows(
                run_id, epoch["provider"], epoch["id"]
            )
            if not budget_rows or any(
                row["kind"] == "settled_runtime_estimate" for row in budget_rows
            ):
                continue
            estimated_cost, cleanup_verified = self._canary_pair_cost(epoch["id"])
            if cleanup_verified and estimated_cost is not None:
                guard.settle(
                    epoch["provider"],
                    epoch["id"],
                    estimated_cost,
                    details={
                        "cleanup_verified": True,
                        "settled_after_final_cleanup": True,
                    },
                )

    def _cleanup_owned(self, run_id: str) -> None:
        for owned in reversed(self.store.owned_resources(run_id)):
            provider = self.providers.get(owned.provider)
            if provider is None:
                continue
            cell = TrialCell(
                provider=owned.provider,
                sku="cleanup",
                gpu_type="unknown",
                region=None,
                resource_class="unknown",
                market="unknown",
            )
            result: DeleteResult = provider.delete(owned.provider_resource_id, cell)
            self._record_cleanup_api_call(run_id, owned.provider, result.api_call)
            self.store.record_cleanup(
                run_id,
                owner_type=owned.owner_type,
                owner_id=owned.owner_id,
                provider=owned.provider,
                provider_resource_id=owned.provider_resource_id,
                action="supervisor_delete",
                status="accepted" if result.accepted or result.missing else "failed",
                failure_class=result.failure_class.value
                if result.failure_class
                else None,
                details={
                    "missing": result.missing,
                    "message": result.message,
                    "raw": result.raw,
                },
            )
            if not (result.accepted or result.missing):
                continue
            verified = (
                result
                if result.missing
                else provider.verify_deleted(owned.provider_resource_id, cell)
            )
            self._record_cleanup_api_call(run_id, owned.provider, verified.api_call)
            self.store.record_cleanup(
                run_id,
                owner_type=owned.owner_type,
                owner_id=owned.owner_id,
                provider=owned.provider,
                provider_resource_id=owned.provider_resource_id,
                action="supervisor_verify_deleted",
                status="confirmed"
                if verified.accepted or verified.missing
                else "failed",
                failure_class=verified.failure_class.value
                if verified.failure_class
                else None,
                details={
                    "missing": verified.missing,
                    "message": verified.message,
                    "raw": verified.raw,
                },
            )
            if verified.accepted or verified.missing:
                if owned.owner_type == "capacity_replica":
                    self.store.mark_replica_cleaned(owned.owner_id)
                else:
                    self.store.mark_canary_cleaned(owned.owner_id)

    def _censor_unfinished_canaries(
        self,
        run_id: str,
        reason: TerminalReason,
    ) -> None:
        for lease in self.store.canary_rows(run_id):
            if lease["censor_reason"] is not None:
                continue
            self.store.finish_canary(
                lease["id"],
                reason.value,
                operator_cleanup=reason == TerminalReason.OPERATOR_CLEANUP,
            )
            if not lease["provider_resource_id"]:
                self.store.mark_canary_cleaned(lease["id"])

    def _finish_unfinished_capacity(
        self,
        run_id: str,
        *,
        status: str,
        reason: str,
        preserve_scheduled: bool = False,
    ) -> None:
        running_trials = self.store.connection.execute(
            "SELECT id FROM capacity_trials WHERE run_id=? AND status='running'",
            (run_id,),
        ).fetchall()
        for row in running_trials:
            self.store.finish_capacity_trial(
                row["id"],
                status=status,
                details={"terminal_reason": reason},
            )
        for epoch in self.store.capacity_epoch_rows(run_id):
            if epoch["status"] in {"claimed", "running"}:
                self.store.finish_capacity_epoch(
                    epoch["id"],
                    status=status,
                    stop_reason=reason,
                )
            elif epoch["status"] == "scheduled" and not preserve_scheduled:
                self.store.finish_capacity_epoch(
                    epoch["id"],
                    status=f"not_run_{status}",
                    stop_reason=reason,
                )

    def _finish_unfinished_canary_epochs(
        self,
        run_id: str,
        *,
        status: str,
        reason: str,
        cleanup_verified: bool,
    ) -> None:
        for epoch in self.store.canary_epoch_rows(run_id):
            if epoch["status"] not in {"selected", "active", "cleanup_incomplete"}:
                continue
            self.store.finish_canary_epoch(
                epoch["id"],
                status=status,
                stop_reason=reason,
                details={
                    "recovered": True,
                    "cleanup_verified": cleanup_verified,
                },
            )

    def _record_cleanup_api_call(self, run_id: str, provider: str, call) -> None:
        if call is None or call.id in self._seen_cleanup_api_calls:
            return
        self._seen_cleanup_api_calls.add(call.id)
        self.store.record_api_observation(
            run_id,
            provider=provider,
            operation=call.operation,
            status=str(call.status),
            latency_ms=call.latency_ms,
            raw=call.raw,
            failure_class=(call.failure_class.value if call.failure_class else None),
        )

    def cleanup_run(self, run_id: str) -> list[object]:
        run = self.store.run_row(run_id)
        stability_experiment = self.store.stability_experiment_for_run(run_id)
        self._censor_unfinished_canaries(run_id, TerminalReason.OPERATOR_CLEANUP)
        self._cleanup_owned(run_id)
        remaining = list(self.store.owned_resources(run_id))
        capacity_status = "cleanup_incomplete" if remaining else "operator_stop"
        self._finish_unfinished_capacity(
            run_id,
            status=capacity_status,
            reason=TerminalReason.OPERATOR_CLEANUP.value,
        )
        self._finish_unfinished_canary_epochs(
            run_id,
            status=capacity_status,
            reason=TerminalReason.OPERATOR_CLEANUP.value,
            cleanup_verified=not remaining,
        )
        if (
            stability_experiment is not None
            and stability_experiment["status"] == "running"
        ):
            self.store.finish_active_stability_windows(
                run_id,
                status=capacity_status,
                stop_reason=TerminalReason.OPERATOR_CLEANUP.value,
                details={"cleanup_verified": not remaining},
            )
            self.store.stop_remaining_stability_windows(
                run_id,
                status="not_run_operator_stop",
                stop_reason=TerminalReason.OPERATOR_CLEANUP.value,
            )
            self.store.finish_stability_experiment_if_complete(
                stability_experiment["experiment_id"],
                status=("cleanup_incomplete" if remaining else "stopped"),
                termination_reason=(
                    "provider resources remain unresolved"
                    if remaining
                    else "operator cleanup"
                ),
            )
        if remaining:
            self.store.finish_run(
                run_id,
                "cleanup_incomplete",
                "provider resources remain unresolved",
            )
        elif run["status"] == "running":
            self.store.finish_run(run_id, "stopped", "operator cleanup")
        elif run["status"] in {"cleanup_incomplete", "recovery_cleanup_incomplete"}:
            self.store.finish_run(
                run_id,
                "cleaned_after_operator_cleanup",
                "operator cleanup completed",
            )
        return remaining

    def recover_run(self, run_id: str) -> list[object]:
        self.store.run_row(run_id)
        capacity_experiment = self.store.capacity_experiment_for_run(run_id)
        stability_experiment = self.store.stability_experiment_for_run(run_id)
        self._censor_unfinished_canaries(
            run_id,
            TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS,
        )
        self._cleanup_owned(run_id)
        remaining = list(self.store.owned_resources(run_id))
        self._finish_unfinished_capacity(
            run_id,
            status=("recovery_cleanup_incomplete" if remaining else "collector_loss"),
            reason=TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS.value,
            preserve_scheduled=capacity_experiment is not None,
        )
        self._finish_unfinished_canary_epochs(
            run_id,
            status=("recovery_cleanup_incomplete" if remaining else "collector_loss"),
            reason=TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS.value,
            cleanup_verified=not remaining,
        )
        if (
            stability_experiment is not None
            and stability_experiment["status"] == "running"
        ):
            recovery_status = (
                "recovery_cleanup_incomplete" if remaining else "collector_loss"
            )
            self.store.finish_active_stability_windows(
                run_id,
                status=recovery_status,
                stop_reason=TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS.value,
                details={
                    "recovered": True,
                    "cleanup_verified": not remaining,
                },
            )
            has_future = any(
                row["status"] == "scheduled"
                for row in self.store.stability_window_rows(run_id)
            )
            if not remaining and has_future:
                self.store.resume_stability_experiment(run_id)
            elif not has_future:
                self.store.finish_stability_experiment_if_complete(
                    stability_experiment["experiment_id"],
                    status=(
                        "recovery_cleanup_incomplete"
                        if remaining
                        else "recovered_after_collector_loss"
                    ),
                    termination_reason=(
                        "provider resources remain unresolved"
                        if remaining
                        else "collector process loss"
                    ),
                )
        if stability_experiment is not None:
            if remaining:
                self.store.finish_run(
                    run_id,
                    "recovery_cleanup_incomplete",
                    "provider resources remain unresolved",
                )
        elif capacity_experiment is not None and not remaining:
            self.store.resume_capacity_experiment(run_id)
        else:
            self.store.finish_run(
                run_id,
                "recovery_cleanup_incomplete"
                if remaining
                else "recovered_after_collector_loss",
                "provider resources remain unresolved"
                if remaining
                else "collector process loss",
            )
        return remaining
