from __future__ import annotations

import dataclasses
import enum
from typing import Any


class FailureClass(str, enum.Enum):
    CAPACITY = "capacity"
    QUOTA = "quota"
    AUTH = "auth"
    RATE_LIMIT = "rate_limit"
    TRANSIENT = "transient"
    TIMEOUT = "timeout"
    UNSUPPORTED_SPEC = "unsupported_spec"
    REMOTE_NOT_FOUND = "remote_not_found"
    UNKNOWN = "unknown"


# Values are persisted in canary_leases.censor_reason. Renaming the Python type
# does not change this serialization contract.
class TerminalReason(str, enum.Enum):
    PROVIDER_INTERRUPTION_CANDIDATE = "provider_interruption_candidate"
    PROVIDER_PRE_READY_FAILURE = "provider_pre_ready_failure"
    APPLICATION_FAILURE = "application_failure"
    MACHINE_REPLACEMENT = "machine_replacement"
    PROVIDER_UNHEALTHY = "provider_unhealthy"
    RIGHT_CENSORED_PLANNED_END = "right_censored_planned_end"
    RIGHT_CENSORED_COLLECTOR_LOSS = "right_censored_collector_loss"
    OPERATOR_CLEANUP = "operator_cleanup"
    LAUNCH_FAILURE = "launch_failure"
    PAIR_SETUP_FAILURE = "pair_setup_failure"


@dataclasses.dataclass(frozen=True)
class CapacityStage:
    target_gpu_depth: int
    target_instance_count: int
    increment_instances: int


MAX_CAPACITY_GPU_DEPTH = 32
SUPPORTED_INSTANCE_GPU_COUNTS = (1, 2, 4, 8)


def _is_power_of_two(value: int) -> bool:
    return value > 0 and value & (value - 1) == 0


def validate_capacity_target(
    target_gpu_depth: int,
    gpu_count_per_instance: int,
) -> None:
    if not _is_power_of_two(target_gpu_depth) or target_gpu_depth > MAX_CAPACITY_GPU_DEPTH:
        raise ValueError(
            f"target_gpu_depth must be a power of two up to {MAX_CAPACITY_GPU_DEPTH}"
        )
    if not _is_power_of_two(gpu_count_per_instance):
        raise ValueError("gpu_count_per_instance must be a power of two")
    if target_gpu_depth < gpu_count_per_instance:
        raise ValueError("target_gpu_depth must be at least gpu_count_per_instance")


def capacity_stages(
    *,
    target_gpu_depth: int,
    gpu_count_per_instance: int,
) -> tuple[CapacityStage, ...]:
    validate_capacity_target(target_gpu_depth, gpu_count_per_instance)
    stages: list[CapacityStage] = []
    target_instances = 1
    previous_instances = 0
    while target_instances * gpu_count_per_instance <= target_gpu_depth:
        stages.append(
            CapacityStage(
                target_gpu_depth=target_instances * gpu_count_per_instance,
                target_instance_count=target_instances,
                increment_instances=target_instances - previous_instances,
            )
        )
        previous_instances = target_instances
        target_instances *= 2
    return tuple(stages)


@dataclasses.dataclass(frozen=True)
class ExperimentConfig:
    protocol_version: str
    host_id: str
    vantage_id: str
    provider_budgets: dict[str, float]
    total_budget_usd: float
    max_active_gpus_per_provider: int = 8

    def __post_init__(self) -> None:
        if not self.protocol_version.strip():
            raise ValueError("protocol_version is required")
        if not self.host_id.strip():
            raise ValueError("host_id is required")
        if not self.vantage_id.strip():
            raise ValueError("vantage_id is required")
        if self.total_budget_usd <= 0:
            raise ValueError("total_budget_usd must be positive")
        if self.max_active_gpus_per_provider <= 0:
            raise ValueError("max_active_gpus_per_provider must be positive")
        if self.max_active_gpus_per_provider > MAX_CAPACITY_GPU_DEPTH:
            raise ValueError(
                "max_active_gpus_per_provider cannot exceed "
                f"{MAX_CAPACITY_GPU_DEPTH}"
            )
        if not self.provider_budgets:
            raise ValueError("provider_budgets must not be empty")
        for provider, budget in self.provider_budgets.items():
            if not provider.strip() or budget <= 0:
                raise ValueError("provider budgets require a provider name and positive amount")


@dataclasses.dataclass(frozen=True)
class CatalogConfig:
    protocol_version: str
    host_id: str
    vantage_id: str
    providers: tuple[str, ...]
    gpu_counts: tuple[int, ...] = (1, 2, 4, 8)
    catalog_interval_seconds: float = 300.0
    host_interval_seconds: float = 30.0
    duration_seconds: float | None = None
    network_control_url: str | None = None

    def __post_init__(self) -> None:
        if not self.protocol_version.strip():
            raise ValueError("protocol_version is required")
        if not self.host_id.strip():
            raise ValueError("host_id is required")
        if not self.vantage_id.strip():
            raise ValueError("vantage_id is required")
        if not self.providers or any(not provider.strip() for provider in self.providers):
            raise ValueError("providers must contain at least one provider")
        if len(self.providers) != len(set(self.providers)):
            raise ValueError("providers must be unique")
        if not self.gpu_counts or any(count <= 0 for count in self.gpu_counts):
            raise ValueError("gpu_counts must contain positive values")
        if len(self.gpu_counts) != len(set(self.gpu_counts)):
            raise ValueError("gpu_counts must be unique")
        if self.catalog_interval_seconds <= 0:
            raise ValueError("catalog_interval_seconds must be positive")
        if self.host_interval_seconds <= 0:
            raise ValueError("host_interval_seconds must be positive")
        if self.duration_seconds is not None and self.duration_seconds <= 0:
            raise ValueError("duration_seconds must be positive")


@dataclasses.dataclass(frozen=True)
class TrialCell:
    provider: str
    sku: str
    gpu_type: str
    region: str | None
    resource_class: str
    market: str
    gpu_count: int = 1
    datacenter: str | None = None
    image: str | None = None

    def __post_init__(self) -> None:
        if not self.provider.strip() or not self.sku.strip():
            raise ValueError("provider and sku are required")
        if self.gpu_count not in SUPPORTED_INSTANCE_GPU_COUNTS:
            raise ValueError(
                "gpu_count must be one of "
                + ", ".join(str(value) for value in SUPPORTED_INSTANCE_GPU_COUNTS)
            )


@dataclasses.dataclass(frozen=True)
class CatalogQuote:
    provider: str
    sku: str
    gpu_type: str
    gpu_count: int
    resource_class: str
    market: str
    quoted_price_usd_hr: float | None
    region: str | None = None
    datacenter: str | None = None
    stock_status: str | None = None
    available_gpu_counts: tuple[int, ...] = ()
    spot_price_usd_hr: float | None = None
    available: bool | None = None
    vcpus: int | None = None
    memory_gib: float | None = None
    storage_gib: float | None = None
    query: dict[str, Any] = dataclasses.field(default_factory=dict)
    raw: dict[str, Any] = dataclasses.field(default_factory=dict)


@dataclasses.dataclass(frozen=True)
class OwnedResource:
    owner_type: str
    owner_id: str
    run_id: str
    provider: str
    provider_resource_id: str
    resource_kind: str


@dataclasses.dataclass(frozen=True)
class HostObservation:
    id: str
    expected_monotonic: float
    actual_monotonic: float
    scheduler_drift_ms: int
    healthy: bool
    network_status: str | None = None
    network_latency_ms: int | None = None


@dataclasses.dataclass(frozen=True)
class CanaryLease:
    id: str
    run_id: str
    pair_id: str
    provider: str
    sku: str
    gpu_type: str
    region: str | None
    datacenter: str | None
    resource_class: str
    market: str
    interruptible: bool
    provider_resource_id: str | None
    machine_id: str | None
    planned_end_at: str
    status: str
