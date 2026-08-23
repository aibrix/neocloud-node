from __future__ import annotations

import dataclasses
import datetime as dt
import time
import uuid
from collections.abc import Callable, Sequence

from .capacity import ReplicaRequest
from ..model import CanaryLease, CanaryEndReason, TrialCell
from ..providers import DeleteResult, ProviderAPICall, ResourceObservation
from ..storage import TraceStore


UTC = dt.timezone.utc


@dataclasses.dataclass(frozen=True)
class CanarySample:
    observed_at: dt.datetime
    exists: bool
    provider_state: str | None
    provider_ready: bool
    heartbeat_ok: bool | None
    machine_id: str | None
    runtime_uptime_seconds: float | None
    host_healthy: bool


class CanaryClassifier:
    def __init__(
        self,
        *,
        heartbeat_grace_seconds: float = 90.0,
        disappearance_grace_seconds: float = 60.0,
    ) -> None:
        self.heartbeat_grace_seconds = heartbeat_grace_seconds
        self.disappearance_grace_seconds = disappearance_grace_seconds

    def classify(
        self,
        *,
        planned_end: dt.datetime,
        now: dt.datetime,
        samples: Sequence[CanarySample],
        operator_cleanup: bool,
    ) -> CanaryEndReason | None:
        if operator_cleanup:
            return CanaryEndReason.OPERATOR_CLEANUP
        if not samples:
            return (
                CanaryEndReason.RIGHT_CENSORED_PLANNED_END
                if now >= planned_end
                else None
            )
        latest = samples[-1]
        if not latest.host_healthy and (not latest.exists or latest.heartbeat_ok is False):
            return CanaryEndReason.RIGHT_CENSORED_COLLECTOR_LOSS

        prior_machine = None
        prior_uptime = None
        for sample in samples:
            if prior_machine and sample.machine_id and sample.machine_id != prior_machine:
                return CanaryEndReason.MACHINE_REPLACEMENT
            if (
                prior_uptime is not None
                and sample.runtime_uptime_seconds is not None
                and sample.runtime_uptime_seconds + 1 < prior_uptime
            ):
                return CanaryEndReason.MACHINE_REPLACEMENT
            prior_machine = sample.machine_id or prior_machine
            prior_uptime = (
                sample.runtime_uptime_seconds
                if sample.runtime_uptime_seconds is not None
                else prior_uptime
            )

        if latest.provider_state and latest.provider_state.lower() == "unhealthy":
            return CanaryEndReason.PROVIDER_UNHEALTHY
        if latest.provider_state and latest.provider_state.lower() in {
            "exited",
            "preempted",
            "stopped",
            "terminated",
        } and now < planned_end:
            return (
                CanaryEndReason.PROVIDER_INTERRUPTION_CANDIDATE
                if any(sample.provider_ready for sample in samples[:-1])
                else CanaryEndReason.PROVIDER_PRE_READY_FAILURE
            )
        if not latest.exists and now < planned_end:
            first_missing = latest.observed_at
            for sample in reversed(samples[:-1]):
                if sample.exists or not sample.host_healthy:
                    break
                first_missing = sample.observed_at
            if (now - first_missing).total_seconds() >= self.disappearance_grace_seconds:
                return (
                    CanaryEndReason.PROVIDER_INTERRUPTION_CANDIDATE
                    if any(sample.provider_ready for sample in samples)
                    else CanaryEndReason.PROVIDER_PRE_READY_FAILURE
                )

        if latest.provider_ready and latest.heartbeat_ok is False:
            last_healthy = next(
                (sample.observed_at for sample in reversed(samples) if sample.heartbeat_ok is True),
                None,
            )
            failure_start = last_healthy or samples[0].observed_at
            if (now - failure_start).total_seconds() >= self.heartbeat_grace_seconds:
                return CanaryEndReason.APPLICATION_FAILURE

        if now >= planned_end:
            return CanaryEndReason.RIGHT_CENSORED_PLANNED_END
        return None


class CanaryRunner:
    def __init__(
        self,
        store: TraceStore,
        provider: object,
        *,
        classifier: CanaryClassifier | None = None,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.store = store
        self.provider = provider
        self.classifier = classifier or CanaryClassifier()
        self.sleep = sleep
        self._seen_api_calls: set[str] = set()

    def start_pair(
        self,
        run_id: str,
        base_cell: TrialCell,
        planned_end: dt.datetime,
        *,
        pair_id: str | None = None,
    ) -> tuple[CanaryLease, CanaryLease]:
        existing = (
            self.store.canary_rows_for_pair(pair_id)
            if pair_id
            else self.store.active_canaries(
                run_id,
                provider=base_cell.provider,
                sku=base_cell.sku,
                resource_class=base_cell.resource_class,
            )
        )
        if len(existing) == 2 and len({row["pair_id"] for row in existing}) == 1:
            leases = tuple(self._lease_from_row(row) for row in existing)
            return leases[0], leases[1]

        pair_id = pair_id or str(uuid.uuid4())
        leases: list[CanaryLease] = []
        for market, interruptible in (("interruptible", True), ("on-demand", False)):
            cell = dataclasses.replace(base_cell, market=market)
            lease_id = self.store.start_canary(
                run_id,
                cell,
                pair_id=pair_id,
                interruptible=interruptible,
                planned_end_at=planned_end.astimezone(UTC).isoformat(),
            )
            request = ReplicaRequest(
                replica_id=lease_id,
                name=f"aibrix-canary-{pair_id[:8]}-{'spot' if interruptible else 'ondemand'}",
                stage_target=1,
                ordinal=int(not interruptible),
            )
            results = list(self.provider.create_replicas(cell, [request]))
            result = results[0] if results else None
            if result:
                self._record_api_call(run_id, cell.provider, result.api_call)
            if result and result.accepted and result.provider_resource_id:
                self.store.mark_canary_created(
                    lease_id,
                    provider_resource_id=result.provider_resource_id,
                    machine_id=result.machine_id,
                    datacenter=result.location,
                    details={"price_usd_hr": result.price_usd_hr, "raw": result.raw},
                )
            else:
                self.store.finish_canary(
                    lease_id,
                    CanaryEndReason.LAUNCH_FAILURE.value,
                )
                self.store.mark_canary_cleaned(lease_id)
            leases.append(self._lease_from_row(self.store.canary_row(lease_id)))
        return leases[0], leases[1]

    def start_single(
        self,
        run_id: str,
        cell: TrialCell,
        planned_end: dt.datetime,
        *,
        group_id: str,
    ) -> CanaryLease:
        existing = self.store.canary_rows_for_pair(group_id)
        if len(existing) == 1:
            return self._lease_from_row(existing[0])
        if existing:
            raise ValueError(f"canary group {group_id} is not a single-lease group")
        interruptible = cell.market == "interruptible"
        lease_id = self.store.start_canary(
            run_id,
            cell,
            pair_id=group_id,
            interruptible=interruptible,
            planned_end_at=planned_end.astimezone(UTC).isoformat(),
        )
        request = ReplicaRequest(
            replica_id=lease_id,
            name=f"aibrix-control-{group_id[:8]}",
            stage_target=1,
            ordinal=0,
        )
        results = list(self.provider.create_replicas(cell, [request]))
        result = results[0] if results else None
        if result:
            self._record_api_call(run_id, cell.provider, result.api_call)
        if result and result.accepted and result.provider_resource_id:
            self.store.mark_canary_created(
                lease_id,
                provider_resource_id=result.provider_resource_id,
                machine_id=result.machine_id,
                datacenter=result.location,
                details={"price_usd_hr": result.price_usd_hr, "raw": result.raw},
            )
        else:
            self.store.finish_canary(
                lease_id,
                CanaryEndReason.LAUNCH_FAILURE.value,
            )
            self.store.mark_canary_cleaned(lease_id)
        return self._lease_from_row(self.store.canary_row(lease_id))

    def observe_once(
        self,
        lease: CanaryLease,
        *,
        host_observation_id: str | None = None,
        host_healthy: bool = True,
        observed_at: dt.datetime | None = None,
    ) -> CanarySample:
        if not lease.provider_resource_id:
            raise ValueError("cannot observe a canary without a provider resource ID")
        cell = TrialCell(
            provider=lease.provider,
            sku=lease.sku,
            gpu_type=lease.gpu_type,
            region=lease.region,
            datacenter=lease.datacenter,
            resource_class=lease.resource_class,
            market=lease.market,
        )
        observation: ResourceObservation = self.provider.observe(
            lease.provider_resource_id, cell
        )
        self._record_api_call(lease.run_id, lease.provider, observation.api_call)
        at = observed_at or dt.datetime.now(UTC)
        self.store.record_canary_observation(
            lease.id,
            provider_state=observation.provider_state,
            machine_id=observation.machine_id,
            runtime_uptime_seconds=observation.runtime_uptime_seconds,
            realized_price_usd_hr=observation.price_usd_hr,
            provider_location=observation.location,
            heartbeat_ok=observation.application_ready,
            heartbeat_latency_ms=observation.application_latency_ms,
            heartbeat_version=observation.heartbeat_version,
            host_observation_id=host_observation_id,
            raw=observation.raw,
            observed_at=at.isoformat(),
        )
        return CanarySample(
            observed_at=at,
            exists=observation.exists,
            provider_state=observation.provider_state,
            provider_ready=observation.provider_ready,
            heartbeat_ok=observation.application_ready,
            machine_id=observation.machine_id,
            runtime_uptime_seconds=observation.runtime_uptime_seconds,
            host_healthy=host_healthy,
        )

    def stop(
        self,
        run_id: str,
        lease: CanaryLease,
        *,
        reason: CanaryEndReason,
        operator_cleanup: bool = False,
    ) -> DeleteResult:
        self.store.finish_canary(
            lease.id,
            reason.value,
            operator_cleanup=operator_cleanup,
        )
        if not lease.provider_resource_id:
            return DeleteResult(True, missing=True)
        cell = TrialCell(
            provider=lease.provider,
            sku=lease.sku,
            gpu_type=lease.gpu_type,
            region=lease.region,
            datacenter=lease.datacenter,
            resource_class=lease.resource_class,
            market=lease.market,
        )
        result: DeleteResult = self.provider.delete(lease.provider_resource_id, cell)
        self._record_api_call(run_id, lease.provider, result.api_call)
        self.store.record_cleanup(
            run_id,
            owner_type="canary_lease",
            owner_id=lease.id,
            provider=lease.provider,
            provider_resource_id=lease.provider_resource_id,
            action="delete",
            status="accepted" if result.accepted else "failed",
            failure_class=result.failure_class.value if result.failure_class else None,
            details={"missing": result.missing, "message": result.message, "raw": result.raw},
        )
        if result.accepted or result.missing:
            if result.missing:
                self.store.mark_canary_cleaned(lease.id)
                return result
            verified: DeleteResult = self.provider.verify_deleted(
                lease.provider_resource_id, cell
            )
            self._record_api_call(run_id, lease.provider, verified.api_call)
            self.store.record_cleanup(
                run_id,
                owner_type="canary_lease",
                owner_id=lease.id,
                provider=lease.provider,
                provider_resource_id=lease.provider_resource_id,
                action="verify_deleted",
                status="confirmed" if verified.accepted or verified.missing else "failed",
                failure_class=verified.failure_class.value if verified.failure_class else None,
                details={
                    "missing": verified.missing,
                    "message": verified.message,
                    "raw": verified.raw,
                },
            )
            if verified.accepted or verified.missing:
                self.store.mark_canary_cleaned(lease.id)
            return verified
        return result

    def _record_api_call(
        self,
        run_id: str,
        provider: str,
        call: ProviderAPICall | None,
    ) -> None:
        if call is None or call.id in self._seen_api_calls:
            return
        self._seen_api_calls.add(call.id)
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

    @staticmethod
    def _lease_from_row(row) -> CanaryLease:
        return CanaryLease(
            id=row["id"],
            run_id=row["run_id"],
            pair_id=row["pair_id"],
            provider=row["provider"],
            sku=row["sku"],
            gpu_type=row["gpu_type"],
            region=row["region"],
            datacenter=row["datacenter"],
            resource_class=row["resource_class"],
            market=row["market"],
            interruptible=bool(row["interruptible"]),
            provider_resource_id=row["provider_resource_id"],
            machine_id=row["machine_id"],
            planned_end_at=row["planned_end_at"],
            status=row["status"],
        )
