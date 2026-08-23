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

from .runners.canary import CanaryRunner, CanarySample
from .runners.capacity import CapacityRunner, CapacityTrialResult
from .model import (
    TerminalReason,
    ExperimentConfig,
    TrialCell,
    validate_capacity_target,
)
from .runners.passive import HostMonitor, PassiveWorker
from .providers import DeleteResult
from .storage import TraceStore


UTC = dt.timezone.utc


class ActiveRunDisabled(RuntimeError):
    pass


class BudgetExceeded(RuntimeError):
    pass


class UnresolvedResources(RuntimeError):
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
            raise ValueError("capacity settlement safety multiplier must be at least one")

    @property
    def budget_upper_bound_usd(self) -> float:
        billable_seconds = max(self.max_lease_seconds, self.billing_minimum_seconds)
        return (
            self.target_depth // self.cell.gpu_count
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
    selection_strategy: str = "weighted_random"

    def __post_init__(self) -> None:
        if not self.name.strip():
            raise ValueError("capacity cohort name is required")
        if not self.cells:
            raise ValueError("capacity cohort must contain at least one cell")
        if self.max_trials <= 0:
            raise ValueError("capacity cohort max_trials must be positive")
        if self.initial_delay_seconds < 0 or self.jitter_seconds < 0:
            raise ValueError("capacity schedule delays must not be negative")
        if self.interval_seconds <= 0 or self.min_remaining_seconds <= 0:
            raise ValueError("capacity schedule intervals must be positive")
        if self.jitter_seconds * 2 >= self.interval_seconds:
            raise ValueError("capacity jitter must be less than half the interval")
        if self.selection_strategy not in {"weighted_random", "balanced_cycle"}:
            raise ValueError("unsupported capacity selection strategy")

    def select(self, rng: random.Random) -> tuple[CapacityTrialPlan, float]:
        total_weight = sum(cell.weight for cell in self.cells)
        selected = rng.choices(self.cells, weights=[cell.weight for cell in self.cells], k=1)[0]
        return selected, selected.weight / total_weight

    def select_sequence(
        self,
        rng: random.Random,
    ) -> tuple[tuple[CapacityTrialPlan, float], ...]:
        if self.selection_strategy == "weighted_random":
            return tuple(self.select(rng) for _ in range(self.max_trials))
        selected: list[tuple[CapacityTrialPlan, float]] = []
        probability = 1.0 / len(self.cells)
        while len(selected) < self.max_trials:
            cycle = list(self.cells)
            rng.shuffle(cycle)
            selected.extend((cell, probability) for cell in cycle)
        return tuple(selected[: self.max_trials])


@dataclasses.dataclass(frozen=True)
class CompletionWindowArmPlan:
    name: str
    launch_fraction: float
    weight: float = 1.0

    def __post_init__(self) -> None:
        if not self.name.strip():
            raise ValueError("completion-window arm name is required")
        if not 0 <= self.launch_fraction <= 1:
            raise ValueError("completion-window launch_fraction must be in [0, 1]")
        if self.weight <= 0:
            raise ValueError("completion-window arm weight must be positive")


@dataclasses.dataclass(frozen=True)
class CompletionWindowCohortPlan:
    name: str
    cells: tuple[CapacityTrialPlan, ...]
    arms: tuple[CompletionWindowArmPlan, ...]
    max_opportunities: int
    selection_seed: int
    initial_delay_seconds: float
    opportunity_interval_seconds: float
    jitter_seconds: float
    completion_window_seconds: float
    required_runtime_seconds: float
    startup_safety_seconds: float
    min_remaining_seconds: float
    selection_strategy: str = "balanced_cycle"

    def __post_init__(self) -> None:
        if not self.name.strip():
            raise ValueError("completion-window cohort name is required")
        if not self.cells or not self.arms:
            raise ValueError("completion-window cohort requires cells and arms")
        if len({arm.name for arm in self.arms}) != len(self.arms):
            raise ValueError("completion-window arm names must be unique")
        if self.max_opportunities <= 0:
            raise ValueError("completion-window max_opportunities must be positive")
        if self.initial_delay_seconds < 0 or self.jitter_seconds < 0:
            raise ValueError("completion-window schedule delays must not be negative")
        if self.opportunity_interval_seconds <= 0 or self.min_remaining_seconds <= 0:
            raise ValueError("completion-window schedule intervals must be positive")
        if self.jitter_seconds * 2 >= self.opportunity_interval_seconds:
            raise ValueError("completion-window jitter must be less than half the interval")
        if self.completion_window_seconds <= 0 or self.required_runtime_seconds < 0:
            raise ValueError("completion window must be positive and runtime non-negative")
        if self.startup_safety_seconds < 0:
            raise ValueError("completion-window startup safety must not be negative")
        if self.maximum_wait_seconds < 0:
            raise ValueError(
                "completion window must cover required runtime and startup safety"
            )
        longest_trial = max(cell.max_lease_seconds for cell in self.cells)
        if self.opportunity_interval_seconds < self.maximum_wait_seconds + longest_trial:
            raise ValueError(
                "completion-window opportunities must not overlap in the initial protocol"
            )
        if self.selection_strategy not in {"weighted_random", "balanced_cycle"}:
            raise ValueError("unsupported completion-window selection strategy")

    @property
    def maximum_wait_seconds(self) -> float:
        return (
            self.completion_window_seconds
            - self.required_runtime_seconds
            - self.startup_safety_seconds
        )

    def select_sequence(
        self,
        rng: random.Random,
    ) -> tuple[tuple[CapacityTrialPlan, CompletionWindowArmPlan, float], ...]:
        combinations = [
            (cell, arm)
            for cell in self.cells
            for arm in self.arms
        ]
        if self.selection_strategy == "weighted_random":
            weights = [cell.weight * arm.weight for cell, arm in combinations]
            total_weight = sum(weights)
            return tuple(
                (*selected, selected[0].weight * selected[1].weight / total_weight)
                for selected in rng.choices(
                    combinations,
                    weights=weights,
                    k=self.max_opportunities,
                )
            )
        selected: list[tuple[CapacityTrialPlan, CompletionWindowArmPlan, float]] = []
        probability = 1.0 / len(combinations)
        while len(selected) < self.max_opportunities:
            cycle = list(combinations)
            rng.shuffle(cycle)
            selected.extend((*item, probability) for item in cycle)
        return tuple(selected[: self.max_opportunities])


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
        return (
            hourly
            * billable_seconds
            * self.settlement_safety_multiplier
            / 3600.0
        )


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
        selected = rng.choices(self.cells, weights=[cell.weight for cell in self.cells], k=1)[0]
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
class ControlTrialPlan:
    cell: TrialCell
    duration_seconds: float
    interval_seconds: float
    estimated_price_usd_hr: float
    weight: float = 1.0
    billing_minimum_seconds: float = 0.0
    settlement_safety_multiplier: float = 1.0

    def __post_init__(self) -> None:
        if self.cell.market != "on-demand":
            raise ValueError("control lifecycle cells must use the on-demand market")
        if self.duration_seconds <= 0 or self.interval_seconds <= 0:
            raise ValueError("control duration and interval must be positive")
        if self.estimated_price_usd_hr <= 0 or self.weight <= 0:
            raise ValueError("control price and weight must be positive")
        if self.billing_minimum_seconds < 0:
            raise ValueError("control billing minimum must not be negative")
        if self.settlement_safety_multiplier < 1:
            raise ValueError("control settlement safety multiplier must be at least one")

    @property
    def budget_upper_bound_usd(self) -> float:
        billable_seconds = max(self.duration_seconds, self.billing_minimum_seconds)
        return (
            self.estimated_price_usd_hr
            * billable_seconds
            * self.settlement_safety_multiplier
            / 3600.0
        )


@dataclasses.dataclass(frozen=True)
class ControlCohortPlan:
    name: str
    cells: tuple[ControlTrialPlan, ...]
    max_leases: int
    selection_seed: int
    replacement_delay_seconds: float
    min_remaining_seconds: float
    selection_strategy: str = "balanced_cycle"

    def __post_init__(self) -> None:
        if not self.name.strip() or not self.cells:
            raise ValueError("control cohort name and cells are required")
        if self.max_leases <= 0 or self.min_remaining_seconds <= 0:
            raise ValueError("control cohort limits must be positive")
        if self.replacement_delay_seconds < 0:
            raise ValueError("control replacement delay must not be negative")
        if self.selection_strategy not in {"weighted_random", "balanced_cycle"}:
            raise ValueError("unsupported control selection strategy")

    def select(self, rng: random.Random) -> tuple[ControlTrialPlan, float]:
        total_weight = sum(cell.weight for cell in self.cells)
        selected = rng.choices(
            self.cells,
            weights=[cell.weight for cell in self.cells],
            k=1,
        )[0]
        return selected, selected.weight / total_weight

    def select_sequence(
        self,
        rng: random.Random,
    ) -> tuple[tuple[ControlTrialPlan, float], ...]:
        if self.selection_strategy == "weighted_random":
            return tuple(self.select(rng) for _ in range(self.max_leases))
        selected: list[tuple[ControlTrialPlan, float]] = []
        probability = 1.0 / len(self.cells)
        while len(selected) < self.max_leases:
            cycle = list(self.cells)
            rng.shuffle(cycle)
            selected.extend((trial, probability) for trial in cycle)
        return tuple(selected[: self.max_leases])


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
class CompletionWindowScheduleItem:
    window_epoch_id: str
    capacity_epoch_id: str
    trial: CapacityTrialPlan
    arm: CompletionWindowArmPlan
    release_offset_seconds: float
    launch_offset_seconds: float
    allocation_deadline_at: dt.datetime


@dataclasses.dataclass(frozen=True)
class PilotPlan:
    experiment: ExperimentConfig
    duration_seconds: float
    passive_interval_seconds: float
    host_interval_seconds: float
    canary_interval_seconds: float
    network_control_url: str | None
    gpu_counts: tuple[int, ...]
    capacity_trials: tuple[CapacityTrialPlan, ...]
    capacity_cohorts: tuple[CapacityCohortPlan, ...]
    completion_window_cohorts: tuple[CompletionWindowCohortPlan, ...]
    canary_trials: tuple[CanaryTrialPlan, ...]
    canary_cohorts: tuple[CanaryCohortPlan, ...]
    stability_schedule: StabilitySchedulePlan
    control_cohorts: tuple[ControlCohortPlan, ...]
    raw_document: dict[str, Any]

    def __post_init__(self) -> None:
        for value, name in (
            (self.duration_seconds, "duration_seconds"),
            (self.passive_interval_seconds, "passive_interval_seconds"),
            (self.host_interval_seconds, "host_interval_seconds"),
            (self.canary_interval_seconds, "canary_interval_seconds"),
        ):
            if value <= 0:
                raise ValueError(f"{name} must be positive")
        if not self.gpu_counts or any(count <= 0 for count in self.gpu_counts):
            raise ValueError("gpu_counts must contain positive values")
        for trial in self.capacity_trials:
            if trial.target_depth > self.experiment.max_active_gpus_per_provider:
                raise ValueError("capacity target exceeds provider GPU cap")
            if trial.cell.provider not in self.experiment.provider_budgets:
                raise ValueError(f"missing provider budget for {trial.cell.provider}")
        capacity_cohort_names = [cohort.name for cohort in self.capacity_cohorts]
        if len(capacity_cohort_names) != len(set(capacity_cohort_names)):
            raise ValueError("capacity cohort names must be unique")
        for cohort in self.capacity_cohorts:
            for trial in cohort.cells:
                provider = trial.cell.provider
                if trial.target_depth > self.experiment.max_active_gpus_per_provider:
                    raise ValueError("capacity cohort target exceeds provider GPU cap")
                if provider not in self.experiment.provider_budgets:
                    raise ValueError(f"missing provider budget for {provider}")
                if trial.budget_upper_bound_usd > self.experiment.provider_budgets[provider] + 1e-9:
                    raise ValueError(
                        f"one {cohort.name} trial exceeds the {provider} provider budget"
                    )
                if trial.budget_upper_bound_usd > self.experiment.total_budget_usd + 1e-9:
                    raise ValueError(f"one {cohort.name} trial exceeds the total budget")
        completion_cohort_names = [
            cohort.name for cohort in self.completion_window_cohorts
        ]
        if len(completion_cohort_names) != len(set(completion_cohort_names)):
            raise ValueError("completion-window cohort names must be unique")
        for cohort in self.completion_window_cohorts:
            for trial in cohort.cells:
                provider = trial.cell.provider
                if trial.target_depth > self.experiment.max_active_gpus_per_provider:
                    raise ValueError("completion-window target exceeds provider GPU cap")
                if provider not in self.experiment.provider_budgets:
                    raise ValueError(f"missing provider budget for {provider}")
                if trial.budget_upper_bound_usd > self.experiment.provider_budgets[provider] + 1e-9:
                    raise ValueError(
                        f"one {cohort.name} trial exceeds the {provider} provider budget"
                    )
                if trial.budget_upper_bound_usd > self.experiment.total_budget_usd + 1e-9:
                    raise ValueError(f"one {cohort.name} trial exceeds the total budget")
        for trial in self.canary_trials:
            if self.experiment.max_active_gpus_per_provider < 2:
                raise ValueError("matched canaries require a provider GPU cap of at least two")
            if trial.cell.provider not in self.experiment.provider_budgets:
                raise ValueError(f"missing provider budget for {trial.cell.provider}")
        cohort_names = [cohort.name for cohort in self.canary_cohorts]
        if len(cohort_names) != len(set(cohort_names)):
            raise ValueError("canary cohort names must be unique")
        for cohort in self.canary_cohorts:
            for trial in cohort.cells:
                if self.experiment.max_active_gpus_per_provider < 2:
                    raise ValueError("matched canaries require a provider GPU cap of at least two")
                provider = trial.cell.provider
                if provider not in self.experiment.provider_budgets:
                    raise ValueError(f"missing provider budget for {provider}")
                if trial.budget_upper_bound_usd > self.experiment.provider_budgets[provider] + 1e-9:
                    raise ValueError(
                        f"one {cohort.name} pair exceeds the {provider} provider budget"
                    )
                if trial.budget_upper_bound_usd > self.experiment.total_budget_usd + 1e-9:
                    raise ValueError(f"one {cohort.name} pair exceeds the total budget")
        control_cohort_names = [cohort.name for cohort in self.control_cohorts]
        if len(control_cohort_names) != len(set(control_cohort_names)):
            raise ValueError("control cohort names must be unique")
        for cohort in self.control_cohorts:
            for trial in cohort.cells:
                provider = trial.cell.provider
                if provider not in self.experiment.provider_budgets:
                    raise ValueError(f"missing provider budget for {provider}")
                if trial.budget_upper_bound_usd > self.experiment.provider_budgets[provider] + 1e-9:
                    raise ValueError(
                        f"one {cohort.name} lease exceeds the {provider} provider budget"
                    )
                if trial.budget_upper_bound_usd > self.experiment.total_budget_usd + 1e-9:
                    raise ValueError(f"one {cohort.name} lease exceeds the total budget")
        provider_costs: dict[str, float] = {}
        for trial in (*self.capacity_trials, *self.canary_trials):
            provider = trial.cell.provider
            provider_costs[provider] = provider_costs.get(provider, 0.0) + trial.budget_upper_bound_usd
        for provider, amount in provider_costs.items():
            if amount > self.experiment.provider_budgets[provider] + 1e-9:
                raise ValueError(
                    f"planned {provider} cost upper bound {amount:.4f} exceeds provider budget"
                )
        if sum(provider_costs.values()) > self.experiment.total_budget_usd + 1e-9:
            raise ValueError("planned cost upper bound exceeds total budget")
        if self.capacity_cohorts and (
            self.capacity_trials
            or self.completion_window_cohorts
            or self.canary_trials
            or self.canary_cohorts
            or self.control_cohorts
        ):
            raise ValueError(
                "capacity cohorts must run in a dedicated plan to preserve their absolute schedule"
            )
        if self.completion_window_cohorts and (
            self.capacity_trials
            or self.capacity_cohorts
            or self.canary_trials
            or self.canary_cohorts
            or self.control_cohorts
        ):
            raise ValueError(
                "completion-window cohorts must run in a dedicated randomized plan"
            )
        if self.control_cohorts and (
            self.capacity_trials
            or self.capacity_cohorts
            or self.completion_window_cohorts
            or self.canary_trials
            or self.canary_cohorts
        ):
            raise ValueError(
                "control cohorts must run in a dedicated cross-provider plan"
            )
        offsets = self.stability_schedule.offsets()
        for previous, current in zip(offsets, offsets[1:]):
            if current < previous + self.duration_seconds:
                raise ValueError(
                    "stability windows must not overlap; increase interval_seconds "
                    "or reduce jitter_seconds"
                )

    @property
    def has_active_work(self) -> bool:
        return bool(
            self.capacity_trials
            or self.capacity_cohorts
            or self.completion_window_cohorts
            or self.canary_trials
            or self.canary_cohorts
            or self.control_cohorts
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
        require_application_ready=bool(
            document.get("require_application_ready", True)
        ),
        weight=float(document.get("weight") or 1.0),
        billing_minimum_seconds=float(
            document.get("billing_minimum_seconds") or 0
        ),
        settlement_safety_multiplier=float(
            document.get("settlement_safety_multiplier") or 1.0
        ),
    )


def _control_trial_plan(document: Mapping[str, Any]) -> ControlTrialPlan:
    return ControlTrialPlan(
        cell=_trial_cell(document),
        duration_seconds=float(document["duration_seconds"]),
        interval_seconds=float(document["interval_seconds"]),
        estimated_price_usd_hr=float(document["estimated_price_usd_hr"]),
        weight=float(document.get("weight") or 1.0),
        billing_minimum_seconds=float(
            document.get("billing_minimum_seconds") or 0
        ),
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
    capacity_trials = tuple(
        _capacity_trial_plan(item)
        for item in raw.get("capacity_trials") or []
    )
    capacity_cohorts = tuple(
        CapacityCohortPlan(
            name=str(item["name"]),
            cells=tuple(
                _capacity_trial_plan(cell) for cell in item.get("cells") or []
            ),
            max_trials=int(item["max_trials"]),
            selection_seed=int(item["selection_seed"]),
            initial_delay_seconds=float(
                item.get("initial_delay_seconds") or 0
            ),
            interval_seconds=float(item["interval_seconds"]),
            jitter_seconds=float(item.get("jitter_seconds") or 0),
            min_remaining_seconds=float(item["min_remaining_seconds"]),
            selection_strategy=str(
                item.get("selection_strategy") or "weighted_random"
            ),
        )
        for item in raw.get("capacity_cohorts") or []
    )
    completion_window_cohorts = tuple(
        CompletionWindowCohortPlan(
            name=str(item["name"]),
            cells=tuple(
                _capacity_trial_plan(cell) for cell in item.get("cells") or []
            ),
            arms=tuple(
                CompletionWindowArmPlan(
                    name=str(arm["name"]),
                    launch_fraction=float(arm["launch_fraction"]),
                    weight=float(arm.get("weight") or 1.0),
                )
                for arm in item.get("arms") or []
            ),
            max_opportunities=int(item["max_opportunities"]),
            selection_seed=int(item["selection_seed"]),
            initial_delay_seconds=float(item.get("initial_delay_seconds") or 0),
            opportunity_interval_seconds=float(
                item["opportunity_interval_seconds"]
            ),
            jitter_seconds=float(item.get("jitter_seconds") or 0),
            completion_window_seconds=float(item["completion_window_seconds"]),
            required_runtime_seconds=float(item["required_runtime_seconds"]),
            startup_safety_seconds=float(item["startup_safety_seconds"]),
            min_remaining_seconds=float(item["min_remaining_seconds"]),
            selection_strategy=str(
                item.get("selection_strategy") or "balanced_cycle"
            ),
        )
        for item in raw.get("completion_window_cohorts") or []
    )
    default_canary_interval = float(raw.get("canary_interval_seconds") or 60)
    canary_trials = tuple(
        _canary_trial_plan(item, default_interval_seconds=default_canary_interval)
        for item in raw.get("canary_trials") or []
    )
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
            replacement_delay_seconds=float(
                item.get("replacement_delay_seconds") or 0
            ),
            min_remaining_seconds=float(
                item.get("min_remaining_seconds") or default_canary_interval
            ),
        )
        for item in raw.get("canary_cohorts") or []
    )
    control_cohorts = tuple(
        ControlCohortPlan(
            name=str(item["name"]),
            cells=tuple(
                _control_trial_plan(cell) for cell in item.get("cells") or []
            ),
            max_leases=int(item["max_leases"]),
            selection_seed=int(item["selection_seed"]),
            replacement_delay_seconds=float(
                item.get("replacement_delay_seconds") or 0
            ),
            min_remaining_seconds=float(item["min_remaining_seconds"]),
            selection_strategy=str(
                item.get("selection_strategy") or "balanced_cycle"
            ),
        )
        for item in raw.get("control_cohorts") or []
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
        initial_delay_seconds=float(
            schedule_document.get("initial_delay_seconds", 0)
        ),
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
        canary_interval_seconds=float(raw.get("canary_interval_seconds") or 0),
        network_control_url=raw.get("network_control_url"),
        gpu_counts=tuple(int(value) for value in (raw.get("gpu_counts") or [1])),
        capacity_trials=capacity_trials,
        capacity_cohorts=capacity_cohorts,
        completion_window_cohorts=completion_window_cohorts,
        canary_trials=canary_trials,
        canary_cohorts=canary_cohorts,
        stability_schedule=stability_schedule,
        control_cohorts=control_cohorts,
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

    def reserve(self, provider: str, amount_usd: float, phase: str, reference: str) -> None:
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
            provider_after = self.store.provider_spend(self.run_id, provider) + amount_usd
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
            rows = self.store.budget_reference_rows(
                self.run_id, provider, reference
            )
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
        campaign_id: str,
        enable_active: bool = False,
        now: dt.datetime | None = None,
    ) -> dict[str, Any]:
        if not enable_active:
            raise ActiveRunDisabled("capacity-tick requires --enable-active")
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
        selected = cohort.select_sequence(rng)
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
        run_id, campaign_created = self.store.ensure_capacity_campaign(
            plan.experiment,
            plan.raw_document,
            campaign_id=campaign_id,
            cohort_name=cohort.name,
            started_at=current.isoformat(),
            epochs=epochs,
            python_version=platform.python_version(),
        )

        campaign = self.store.capacity_campaign_row(campaign_id)
        if campaign["status"] == "complete":
            return {
                "campaign_id": campaign_id,
                "campaign_created": campaign_created,
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
            campaign_id,
            due_at=current.isoformat(),
            expired_before=(
                current - dt.timedelta(seconds=cohort.interval_seconds)
            ).isoformat(),
        )
        if claim_status == "complete":
            self.store.finish_capacity_campaign_if_complete(campaign_id)
        if claim_status != "claimed" or epoch is None:
            return {
                "campaign_id": campaign_id,
                "campaign_created": campaign_created,
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
                scheduled_target_monotonic=(
                    self.monotonic() - started_lag_seconds
                ),
            )
        except Exception:
            self._cleanup_owned(run_id)
            raise
        campaign_complete = self.store.finish_capacity_campaign_if_complete(
            campaign_id
        )
        return {
            "campaign_id": campaign_id,
            "campaign_created": campaign_created,
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
            "campaign_complete": campaign_complete,
        }

    def stability_run(
        self,
        plan: PilotPlan,
        *,
        campaign_id: str,
        enable_active: bool = False,
        now: dt.datetime | None = None,
    ) -> dict[str, Any]:
        if not enable_active:
            raise ActiveRunDisabled("stability run requires --enable-active")
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
        run_id, campaign_created = self.store.ensure_stability_campaign(
            plan.experiment,
            plan.raw_document,
            campaign_id=campaign_id,
            cohort_name=cohort.name,
            started_at=current.isoformat(),
            windows=windows,
            python_version=platform.python_version(),
        )
        campaign = self.store.stability_campaign_row(campaign_id)
        if campaign["status"] != "running":
            return {
                "campaign_id": campaign_id,
                "campaign_created": campaign_created,
                "run_id": run_id,
                "cohort_name": cohort.name,
                "status": str(campaign["status"]),
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
            campaign_id,
            due_at=current.isoformat(),
        )
        if claim_status == "complete":
            self.store.finish_stability_campaign_if_complete(campaign_id)
        if claim_status != "claimed" or window is None:
            return {
                "campaign_id": campaign_id,
                "campaign_created": campaign_created,
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
        remaining_seconds = (
            scheduled_end.astimezone(UTC) - current
        ).total_seconds()
        if remaining_seconds <= 0:
            raise RuntimeError("claimed stability window has already ended")
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
        for thread in threads:
            thread.start()

        window_status = "complete"
        stop_reason: str | None = "window_elapsed"
        caught_error: Exception | None = None
        try:
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
            join_timeout = max(
                plan.host_interval_seconds,
                plan.passive_interval_seconds,
            ) + 5
            for thread in threads:
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

        campaign_finish: dict[str, str | None] = {}
        if window_status == "budget_exhausted":
            campaign_finish = {
                "campaign_status": "budget_exhausted",
                "run_status": "budget_exhausted",
                "termination_reason": "budget_exhausted",
            }
        elif window_status == "stopped":
            campaign_finish = {
                "campaign_status": "stopped",
                "run_status": "stopped",
                "termination_reason": "operator_stop",
            }
        elif window_status == "cleanup_incomplete":
            campaign_finish = {
                "campaign_status": "cleanup_incomplete",
                "run_status": "cleanup_incomplete",
                "termination_reason": "provider resources remain unresolved",
            }
        elif window_status == "complete_with_sampling_errors":
            campaign_finish = {
                "campaign_status": "complete_with_sampling_errors",
                "run_status": "complete_with_sampling_errors",
                "termination_reason": stop_reason,
            }
        elif window_status == "failed":
            campaign_finish = {
                "campaign_status": "failed",
                "run_status": "failed",
                "termination_reason": stop_reason,
            }
        campaign_complete = self.store.finish_stability_campaign_if_complete(
            campaign_id,
            **campaign_finish,
        )
        if caught_error is not None:
            raise caught_error
        return {
            "campaign_id": campaign_id,
            "campaign_created": campaign_created,
            "run_id": run_id,
            "cohort_name": cohort.name,
            "status": "executed",
            "window_id": window["id"],
            "window_sequence": sequence,
            "window_status": window_status,
            "campaign_complete": campaign_complete,
        }

    def run(self, plan: PilotPlan, *, enable_active: bool = False) -> str:
        if plan.has_active_work and not enable_active:
            raise ActiveRunDisabled("billable phases require --enable-active")
        unresolved = self.store.all_owned_resources()
        if unresolved:
            raise UnresolvedResources(
                f"database contains {len(unresolved)} unresolved provider resources; run cleanup first"
            )
        canary_cohort_trials = tuple(
            trial for cohort in plan.canary_cohorts for trial in cohort.cells
        )
        capacity_cohort_trials = tuple(
            trial for cohort in plan.capacity_cohorts for trial in cohort.cells
        )
        completion_window_trials = tuple(
            trial
            for cohort in plan.completion_window_cohorts
            for trial in cohort.cells
        )
        control_trials = tuple(
            trial for cohort in plan.control_cohorts for trial in cohort.cells
        )
        missing = sorted(
            {
                trial.cell.provider
                for trial in (
                    *plan.capacity_trials,
                    *capacity_cohort_trials,
                    *completion_window_trials,
                    *plan.canary_trials,
                    *canary_cohort_trials,
                    *control_trials,
                )
                if trial.cell.provider not in self.providers
            }
        )
        if missing:
            raise ValueError(f"missing provider clients: {', '.join(missing)}")

        run_id = self.store.start_run(
            plan.experiment,
            plan.raw_document,
            python_version=platform.python_version(),
        )
        guard = BudgetGuard(self.store, run_id, plan.experiment)
        passive = PassiveWorker(self.store, self.providers.values())
        host = HostMonitor(
            self.store,
            network_control_url=plan.network_control_url,
        )
        experiment_started_monotonic = self.monotonic()
        experiment_started_wall = dt.datetime.now(UTC)
        deadline = experiment_started_monotonic + plan.duration_seconds
        initial_host = host.sample(run_id, self.monotonic())
        with self._latest_host_lock:
            self._latest_host_id = initial_host.id
            self._latest_host_healthy = initial_host.healthy
        passive.run_once(
            run_id,
            gpu_counts=plan.gpu_counts,
            host_observation_id=initial_host.id,
        )
        threads = [
            threading.Thread(
                target=self._host_loop,
                args=(run_id, host, plan.host_interval_seconds),
                name="neocloud-host",
                daemon=True,
            ),
            threading.Thread(
                target=self._passive_loop,
                args=(run_id, passive, plan.passive_interval_seconds, plan.gpu_counts),
                name="neocloud-passive",
                daemon=True,
            ),
        ]
        for thread in threads:
            thread.start()

        status = "complete"
        reason = None
        try:
            for index, trial in enumerate(plan.capacity_trials):
                if self.stop_event.is_set() or self.monotonic() >= deadline:
                    break
                guard.reserve(
                    trial.cell.provider,
                    trial.budget_upper_bound_usd,
                    "capacity",
                    f"capacity-{index}",
                )
                runner = self.capacity_runner_factory(
                    self.store,
                    self.providers[trial.cell.provider],
                    ready_timeout_seconds=trial.ready_timeout_seconds,
                    require_application_ready=trial.require_application_ready,
                )
                runner.run_trial(run_id, trial.cell, target_depth=trial.target_depth)

            for cohort in plan.capacity_cohorts:
                if self.stop_event.is_set() or self.monotonic() >= deadline:
                    break
                self._run_capacity_cohort(
                    run_id,
                    cohort,
                    guard=guard,
                    experiment_started_monotonic=experiment_started_monotonic,
                    experiment_started_wall=experiment_started_wall,
                    hard_deadline=deadline,
                )

            for cohort in plan.completion_window_cohorts:
                if self.stop_event.is_set() or self.monotonic() >= deadline:
                    break
                self._run_completion_window_cohort(
                    run_id,
                    cohort,
                    guard=guard,
                    experiment_started_monotonic=experiment_started_monotonic,
                    experiment_started_wall=experiment_started_wall,
                    hard_deadline=deadline,
                )

            for index, trial in enumerate(plan.canary_trials):
                if self.stop_event.is_set() or self.monotonic() >= deadline:
                    break
                completed = self._execute_canary_epoch(
                    run_id,
                    trial,
                    cohort_name="static",
                    sequence=index,
                    selection_probability=1.0,
                    selection_seed=None,
                    guard=guard,
                    hard_deadline=deadline,
                )
                if not completed:
                    break

            for cohort in plan.canary_cohorts:
                if self.stop_event.is_set() or self.monotonic() >= deadline:
                    break
                self._run_canary_cohort(
                    run_id,
                    cohort,
                    guard=guard,
                    hard_deadline=deadline,
                )

            for cohort in plan.control_cohorts:
                if self.stop_event.is_set() or self.monotonic() >= deadline:
                    break
                self._run_control_cohort(
                    run_id,
                    cohort,
                    guard=guard,
                    hard_deadline=deadline,
                )

            remaining = max(0.0, deadline - self.monotonic())
            self.stop_event.wait(remaining)
            if self.stop_event.is_set():
                status = "stopped"
                reason = "operator_stop"
        except Exception as error:
            status = "failed"
            reason = f"{type(error).__name__}: {error}"
            raise
        finally:
            self.stop_event.set()
            for thread in threads:
                thread.join(timeout=max(plan.host_interval_seconds, plan.passive_interval_seconds) + 5)
            orphan_reason = (
                TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS
                if status == "failed"
                else TerminalReason.OPERATOR_CLEANUP
            )
            self._censor_unfinished_canaries(run_id, orphan_reason)
            self._cleanup_owned(run_id)
            self._finish_unfinished_capacity(
                run_id,
                status=("collector_loss" if status == "failed" else "operator_stop"),
                reason=orphan_reason.value,
            )
            self._finish_unfinished_completion_windows(
                run_id,
                status=("collector_loss" if status == "failed" else "operator_stop"),
                reason=orphan_reason.value,
            )
            self._settle_cleaned_canary_epochs(run_id, guard)
            if self.store.owned_resources(run_id):
                status = "cleanup_incomplete"
                reason = reason or "provider resources remain unresolved"
            if self._worker_errors and status == "complete":
                status = "complete_with_sampling_errors"
                reason = "; ".join(self._worker_errors[-5:])
            self.store.finish_run(run_id, status, reason)
        return run_id

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

    def _run_capacity_cohort(
        self,
        run_id: str,
        cohort: CapacityCohortPlan,
        *,
        guard: BudgetGuard,
        experiment_started_monotonic: float,
        experiment_started_wall: dt.datetime,
        hard_deadline: float,
    ) -> None:
        rng = random.Random(cohort.selection_seed)
        schedule: list[tuple[str, CapacityTrialPlan, float]] = []
        for sequence, (trial, probability) in enumerate(
            cohort.select_sequence(rng)
        ):
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
            scheduled_at = experiment_started_wall + dt.timedelta(seconds=offset)
            epoch_id = self.store.schedule_capacity_epoch(
                run_id,
                cohort_name=cohort.name,
                sequence=sequence,
                cell=trial.cell,
                target_depth=trial.target_depth,
                scheduled_offset_seconds=offset,
                scheduled_at=scheduled_at.isoformat(),
                selection_probability=probability,
                selection_seed=cohort.selection_seed,
                details={
                    "gpu_count_per_instance": trial.cell.gpu_count,
                    "target_gpu_depth": trial.target_depth,
                    "target_instance_count": trial.target_instance_count,
                    "estimated_price_usd_hr": trial.estimated_price_usd_hr,
                    "max_lease_seconds": trial.max_lease_seconds,
                    "billing_minimum_seconds": trial.billing_minimum_seconds,
                    "settlement_safety_multiplier": trial.settlement_safety_multiplier,
                    "selection_weight": trial.weight,
                },
            )
            schedule.append((epoch_id, trial, offset))

        for index, (epoch_id, trial, offset) in enumerate(schedule):
            if self.stop_event.is_set():
                self._stop_capacity_schedule(
                    schedule[index:],
                    status="not_run_operator_stop",
                    reason="operator_stop",
                )
                return
            target = experiment_started_monotonic + offset
            if target >= hard_deadline:
                self._stop_capacity_schedule(
                    schedule[index:],
                    status="not_run_deadline",
                    reason="scheduled_after_deadline",
                )
                return
            self.stop_event.wait(max(0.0, target - self.monotonic()))
            if self.stop_event.is_set():
                self._stop_capacity_schedule(
                    schedule[index:],
                    status="not_run_operator_stop",
                    reason="operator_stop",
                )
                return
            if hard_deadline - self.monotonic() < cohort.min_remaining_seconds:
                self._stop_capacity_schedule(
                    schedule[index:],
                    status="not_run_deadline",
                    reason="insufficient_remaining_time",
                )
                return
            execution = self._execute_capacity_epoch(
                run_id,
                epoch_id,
                trial,
                guard=guard,
                scheduled_target_monotonic=target,
            )
            if execution.stop_reason == "budget_exhausted":
                self._stop_capacity_schedule(
                    schedule[index + 1 :],
                    status="not_run_budget_exhausted",
                    reason="budget_exhausted",
                )
                return
            if not execution.cleanup_verified:
                self._stop_capacity_schedule(
                    schedule[index + 1 :],
                    status="not_run_cleanup_incomplete",
                    reason="cleanup_incomplete",
                )
                return

    def _run_completion_window_cohort(
        self,
        run_id: str,
        cohort: CompletionWindowCohortPlan,
        *,
        guard: BudgetGuard,
        experiment_started_monotonic: float,
        experiment_started_wall: dt.datetime,
        hard_deadline: float,
    ) -> None:
        rng = random.Random(cohort.selection_seed)
        schedule: list[CompletionWindowScheduleItem] = []
        for sequence, (trial, arm, probability) in enumerate(
            cohort.select_sequence(rng)
        ):
            jitter = (
                rng.uniform(-cohort.jitter_seconds, cohort.jitter_seconds)
                if cohort.jitter_seconds
                else 0.0
            )
            release_offset = max(
                0.0,
                cohort.initial_delay_seconds
                + sequence * cohort.opportunity_interval_seconds
                + jitter,
            )
            launch_offset = (
                release_offset
                + arm.launch_fraction * cohort.maximum_wait_seconds
            )
            release_at = experiment_started_wall + dt.timedelta(
                seconds=release_offset
            )
            allocation_deadline_at = release_at + dt.timedelta(
                seconds=(
                    cohort.completion_window_seconds
                    - cohort.required_runtime_seconds
                )
            )
            planned_launch_at = experiment_started_wall + dt.timedelta(
                seconds=launch_offset
            )
            window_epoch_id = self.store.schedule_completion_window_epoch(
                run_id,
                cohort_name=cohort.name,
                sequence=sequence,
                cell=trial.cell,
                target_depth=trial.target_depth,
                arm_name=arm.name,
                launch_fraction=arm.launch_fraction,
                assignment_probability=probability,
                assignment_seed=cohort.selection_seed,
                release_offset_seconds=release_offset,
                release_at=release_at.isoformat(),
                allocation_deadline_at=allocation_deadline_at.isoformat(),
                planned_launch_at=planned_launch_at.isoformat(),
                details={
                    "gpu_count_per_instance": trial.cell.gpu_count,
                    "target_gpu_depth": trial.target_depth,
                    "target_instance_count": trial.target_instance_count,
                    "completion_window_seconds": cohort.completion_window_seconds,
                    "required_runtime_seconds": cohort.required_runtime_seconds,
                    "startup_safety_seconds": cohort.startup_safety_seconds,
                    "estimated_price_usd_hr": trial.estimated_price_usd_hr,
                    "require_application_ready": trial.require_application_ready,
                    "cell_selection_weight": trial.weight,
                    "arm_selection_weight": arm.weight,
                },
            )
            capacity_epoch_id = self.store.schedule_capacity_epoch(
                run_id,
                cohort_name=f"completion-window:{cohort.name}",
                sequence=sequence,
                cell=trial.cell,
                target_depth=trial.target_depth,
                scheduled_offset_seconds=launch_offset,
                scheduled_at=planned_launch_at.isoformat(),
                selection_probability=probability,
                selection_seed=cohort.selection_seed,
                details={
                    "completion_window_epoch_id": window_epoch_id,
                    "gpu_count_per_instance": trial.cell.gpu_count,
                    "target_gpu_depth": trial.target_depth,
                    "target_instance_count": trial.target_instance_count,
                    "arm_name": arm.name,
                    "launch_fraction": arm.launch_fraction,
                },
            )
            schedule.append(
                CompletionWindowScheduleItem(
                    window_epoch_id=window_epoch_id,
                    capacity_epoch_id=capacity_epoch_id,
                    trial=trial,
                    arm=arm,
                    release_offset_seconds=release_offset,
                    launch_offset_seconds=launch_offset,
                    allocation_deadline_at=allocation_deadline_at,
                )
            )

        for index, item in enumerate(schedule):
            if self.stop_event.is_set():
                self._stop_completion_window_schedule(
                    schedule[index:],
                    status="not_run_operator_stop",
                    reason="operator_stop",
                )
                return
            release_target = (
                experiment_started_monotonic + item.release_offset_seconds
            )
            launch_target = experiment_started_monotonic + item.launch_offset_seconds
            if launch_target >= hard_deadline:
                self._stop_completion_window_schedule(
                    schedule[index:],
                    status="not_run_deadline",
                    reason="scheduled_after_deadline",
                )
                return
            self.stop_event.wait(max(0.0, release_target - self.monotonic()))
            if self.stop_event.is_set():
                self._stop_completion_window_schedule(
                    schedule[index:],
                    status="not_run_operator_stop",
                    reason="operator_stop",
                )
                return
            release_at = (
                experiment_started_wall
                + dt.timedelta(seconds=item.release_offset_seconds)
            ).isoformat()
            release_quote_id = self.store.latest_quote_id(
                run_id,
                item.trial.cell,
                observed_at_or_before=release_at,
            )
            self.store.mark_completion_window_released(
                item.window_epoch_id,
                release_quote_id,
            )
            self.stop_event.wait(max(0.0, launch_target - self.monotonic()))
            if self.stop_event.is_set():
                self._stop_completion_window_schedule(
                    schedule[index:],
                    status="not_run_operator_stop",
                    reason="operator_stop",
                )
                return
            if hard_deadline - self.monotonic() < cohort.min_remaining_seconds:
                self._stop_completion_window_schedule(
                    schedule[index:],
                    status="not_run_deadline",
                    reason="insufficient_remaining_time",
                )
                return
            decision_quote_id = self.store.latest_quote_id(run_id, item.trial.cell)
            self.store.mark_completion_window_decided(
                item.window_epoch_id,
                decision_quote_id=decision_quote_id,
                decision_lag_seconds=max(0.0, self.monotonic() - launch_target),
                capacity_epoch_id=item.capacity_epoch_id,
            )
            execution = self._execute_capacity_epoch(
                run_id,
                item.capacity_epoch_id,
                item.trial,
                guard=guard,
                scheduled_target_monotonic=launch_target,
            )
            if execution.trial_result is None:
                self.store.finish_completion_window_epoch(
                    item.window_epoch_id,
                    status=execution.stop_reason,
                    stop_reason=execution.stop_reason,
                )
            else:
                provider_sla_met, application_sla_met = (
                    self._completion_window_sla_outcome(
                        item.capacity_epoch_id,
                        item.trial,
                        item.allocation_deadline_at,
                    )
                )
                self.store.finish_completion_window_epoch(
                    item.window_epoch_id,
                    status=execution.trial_result.status,
                    stop_reason=execution.stop_reason,
                    provider_sla_met=provider_sla_met,
                    application_sla_met=application_sla_met,
                    details={
                        "estimated_cost_usd": execution.estimated_cost_usd,
                        "cleanup_verified": execution.cleanup_verified,
                        "accepted_depth": execution.trial_result.accepted_depth,
                        "provider_ready_depth": (
                            execution.trial_result.provider_ready_depth
                        ),
                        "application_ready_depth": (
                            execution.trial_result.application_ready_depth
                        ),
                    },
                )
            if execution.stop_reason == "budget_exhausted":
                self._stop_completion_window_schedule(
                    schedule[index + 1 :],
                    status="not_run_budget_exhausted",
                    reason="budget_exhausted",
                )
                return
            if not execution.cleanup_verified:
                self._stop_completion_window_schedule(
                    schedule[index + 1 :],
                    status="not_run_cleanup_incomplete",
                    reason="cleanup_incomplete",
                )
                return

    def _stop_completion_window_schedule(
        self,
        schedule: list[CompletionWindowScheduleItem],
        *,
        status: str,
        reason: str,
    ) -> None:
        for item in schedule:
            self.store.finish_capacity_epoch(
                item.capacity_epoch_id,
                status=status,
                stop_reason=reason,
            )
            self.store.finish_completion_window_epoch(
                item.window_epoch_id,
                status=status,
                stop_reason=reason,
            )

    def _completion_window_sla_outcome(
        self,
        trial_id: str,
        trial: CapacityTrialPlan,
        allocation_deadline_at: dt.datetime,
    ) -> tuple[bool, bool | None]:
        row = self.store.connection.execute(
            """
            SELECT
              COUNT(*) FILTER (WHERE provider_ready_at IS NOT NULL) AS provider_ready,
              COUNT(*) FILTER (WHERE application_ready_at IS NOT NULL) AS application_ready,
              MAX(provider_ready_at) AS provider_ready_at,
              MAX(application_ready_at) AS application_ready_at
            FROM capacity_replicas
            WHERE trial_id=? AND provider_resource_id IS NOT NULL
            """,
            (trial_id,),
        ).fetchone()
        provider_ready_at = (
            dt.datetime.fromisoformat(row["provider_ready_at"])
            if row["provider_ready_at"]
            else None
        )
        provider_sla_met = bool(
            row["provider_ready"] >= trial.target_instance_count
            and provider_ready_at
            and provider_ready_at <= allocation_deadline_at
        )
        if not trial.require_application_ready:
            return provider_sla_met, None
        application_ready_at = (
            dt.datetime.fromisoformat(row["application_ready_at"])
            if row["application_ready_at"]
            else None
        )
        application_sla_met = bool(
            row["application_ready"] >= trial.target_instance_count
            and application_ready_at
            and application_ready_at <= allocation_deadline_at
        )
        return provider_sla_met, application_sla_met

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
                "application_ready_gpu_depth": (
                    result.application_ready_gpu_depth
                ),
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

    def _stop_capacity_schedule(
        self,
        schedule: list[tuple[str, CapacityTrialPlan, float]],
        *,
        status: str,
        reason: str,
    ) -> None:
        for epoch_id, _trial, _offset in schedule:
            self.store.finish_capacity_epoch(
                epoch_id,
                status=status,
                stop_reason=reason,
            )

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

    def _run_control_cohort(
        self,
        run_id: str,
        cohort: ControlCohortPlan,
        *,
        guard: BudgetGuard,
        hard_deadline: float,
    ) -> None:
        rng = random.Random(cohort.selection_seed)
        for sequence, (trial, probability) in enumerate(
            cohort.select_sequence(rng)
        ):
            remaining = hard_deadline - self.monotonic()
            if self.stop_event.is_set() or remaining < cohort.min_remaining_seconds:
                return
            completed = self._execute_control_epoch(
                run_id,
                trial,
                cohort_name=cohort.name,
                sequence=sequence,
                selection_probability=probability,
                selection_seed=cohort.selection_seed,
                guard=guard,
                hard_deadline=hard_deadline,
            )
            if not completed:
                return
            if cohort.replacement_delay_seconds:
                self.stop_event.wait(cohort.replacement_delay_seconds)

    def _execute_control_epoch(
        self,
        run_id: str,
        trial: ControlTrialPlan,
        *,
        cohort_name: str,
        sequence: int,
        selection_probability: float,
        selection_seed: int,
        guard: BudgetGuard,
        hard_deadline: float,
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
                "design": "unmatched_on_demand_control",
                "on_demand_price_usd_hr": trial.estimated_price_usd_hr,
                "selection_weight": trial.weight,
                "billing_minimum_seconds": trial.billing_minimum_seconds,
                "settlement_safety_multiplier": trial.settlement_safety_multiplier,
            },
        )
        try:
            guard.reserve(
                trial.cell.provider,
                trial.budget_upper_bound_usd,
                "control",
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
            result = self._run_control_trial(
                run_id,
                trial,
                group_id=epoch_id,
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
            stop_reason="control_terminal",
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

    def _run_control_trial(
        self,
        run_id: str,
        trial: ControlTrialPlan,
        *,
        group_id: str,
        hard_deadline: float,
    ) -> CanaryTrialResult:
        runner = self.canary_runner_factory(
            self.store,
            self.providers[trial.cell.provider],
        )
        now = dt.datetime.now(UTC)
        remaining = max(0.0, hard_deadline - self.monotonic())
        duration = min(trial.duration_seconds, remaining)
        if duration <= 0:
            raise RuntimeError("control epoch has no remaining observation time")
        planned_end = now + dt.timedelta(seconds=duration)
        self.store.mark_canary_epoch_started(group_id, planned_end.isoformat())
        lease = runner.start_single(
            run_id,
            trial.cell,
            planned_end,
            group_id=group_id,
        )
        samples: list[CanarySample] = []
        active = bool(lease.provider_resource_id)
        while active and not self.stop_event.is_set() and dt.datetime.now(UTC) < planned_end:
            with self._latest_host_lock:
                host_id = self._latest_host_id
                host_healthy = self._latest_host_healthy
            sample = runner.observe_once(
                lease,
                host_observation_id=host_id,
                host_healthy=host_healthy,
            )
            samples.append(sample)
            reason = runner.classifier.classify(
                planned_end=planned_end,
                now=sample.observed_at,
                samples=samples,
                operator_cleanup=False,
            )
            if reason is not None:
                runner.stop(run_id, lease, reason=reason)
                active = False
            elif active:
                self.stop_event.wait(trial.interval_seconds)

        if active:
            final_reason = (
                TerminalReason.OPERATOR_CLEANUP
                if self.stop_event.is_set()
                else TerminalReason.RIGHT_CENSORED_PLANNED_END
            )
            runner.stop(
                run_id,
                lease,
                reason=final_reason,
                operator_cleanup=self.stop_event.is_set(),
            )
        estimated_cost, cleanup_verified = self._canary_pair_cost(group_id)
        reasons = tuple(
            str(row["censor_reason"])
            for row in self.store.canary_rows_for_pair(group_id)
            if row["censor_reason"]
        )
        return CanaryTrialResult(
            pair_id=group_id,
            cleanup_verified=cleanup_verified,
            estimated_cost_usd=estimated_cost,
            censor_reasons=reasons,
        )

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
        while active and not self.stop_event.is_set() and dt.datetime.now(UTC) < planned_end:
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
                    details={"cleanup_verified": True, "settled_after_final_cleanup": True},
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
                failure_class=result.failure_class.value if result.failure_class else None,
                details={"missing": result.missing, "message": result.message, "raw": result.raw},
            )
            if not (result.accepted or result.missing):
                continue
            verified = result if result.missing else provider.verify_deleted(owned.provider_resource_id, cell)
            self._record_cleanup_api_call(run_id, owned.provider, verified.api_call)
            self.store.record_cleanup(
                run_id,
                owner_type=owned.owner_type,
                owner_id=owned.owner_id,
                provider=owned.provider,
                provider_resource_id=owned.provider_resource_id,
                action="supervisor_verify_deleted",
                status="confirmed" if verified.accepted or verified.missing else "failed",
                failure_class=verified.failure_class.value if verified.failure_class else None,
                details={"missing": verified.missing, "message": verified.message, "raw": verified.raw},
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

    def _finish_unfinished_completion_windows(
        self,
        run_id: str,
        *,
        status: str,
        reason: str,
    ) -> None:
        for epoch in self.store.completion_window_epoch_rows(run_id):
            if epoch["status"] == "scheduled":
                terminal_status = f"not_run_{status}"
            elif epoch["status"] in {"released", "decided"}:
                terminal_status = status
            else:
                continue
            self.store.finish_completion_window_epoch(
                epoch["id"],
                status=terminal_status,
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
            failure_class=(
                call.failure_class.value if call.failure_class else None
            ),
        )

    def cleanup_run(self, run_id: str) -> list[object]:
        run = self.store.run_row(run_id)
        stability_campaign = self.store.stability_campaign_for_run(run_id)
        self._censor_unfinished_canaries(run_id, TerminalReason.OPERATOR_CLEANUP)
        self._cleanup_owned(run_id)
        remaining = list(self.store.owned_resources(run_id))
        capacity_status = "cleanup_incomplete" if remaining else "operator_stop"
        self._finish_unfinished_capacity(
            run_id,
            status=capacity_status,
            reason=TerminalReason.OPERATOR_CLEANUP.value,
        )
        self._finish_unfinished_completion_windows(
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
            stability_campaign is not None
            and stability_campaign["status"] == "running"
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
            self.store.finish_stability_campaign_if_complete(
                stability_campaign["campaign_id"],
                campaign_status=(
                    "cleanup_incomplete" if remaining else "stopped"
                ),
                run_status=("cleanup_incomplete" if remaining else "stopped"),
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
        capacity_campaign = self.store.capacity_campaign_for_run(run_id)
        stability_campaign = self.store.stability_campaign_for_run(run_id)
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
            preserve_scheduled=capacity_campaign is not None,
        )
        self._finish_unfinished_completion_windows(
            run_id,
            status=("recovery_cleanup_incomplete" if remaining else "collector_loss"),
            reason=TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS.value,
        )
        self._finish_unfinished_canary_epochs(
            run_id,
            status=("recovery_cleanup_incomplete" if remaining else "collector_loss"),
            reason=TerminalReason.RIGHT_CENSORED_COLLECTOR_LOSS.value,
            cleanup_verified=not remaining,
        )
        if (
            stability_campaign is not None
            and stability_campaign["status"] == "running"
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
                self.store.resume_stability_campaign(run_id)
            elif not has_future:
                self.store.finish_stability_campaign_if_complete(
                    stability_campaign["campaign_id"],
                    campaign_status=(
                        "recovery_cleanup_incomplete"
                        if remaining
                        else "recovered_after_collector_loss"
                    ),
                    run_status=(
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
        if stability_campaign is not None:
            if remaining:
                self.store.finish_run(
                    run_id,
                    "recovery_cleanup_incomplete",
                    "provider resources remain unresolved",
                )
        elif capacity_campaign is not None and not remaining:
            self.store.resume_capacity_campaign(run_id)
        else:
            self.store.finish_run(
                run_id,
                "recovery_cleanup_incomplete" if remaining else "recovered_after_collector_loss",
                "provider resources remain unresolved" if remaining else "collector process loss",
            )
        return remaining
