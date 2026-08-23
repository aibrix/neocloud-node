from __future__ import annotations

import dataclasses
import time
from collections.abc import Callable, Sequence
from typing import Protocol

from ..model import FailureClass, TrialCell, capacity_stages
from ..providers import CreateResult, DeleteResult, ProviderAPICall, ResourceObservation
from ..storage import TraceStore


@dataclasses.dataclass(frozen=True)
class ReplicaRequest:
    replica_id: str
    name: str
    stage_target: int
    ordinal: int
    attempt: int = 0


@dataclasses.dataclass(frozen=True)
class CapacityTrialResult:
    trial_id: str
    gpu_count_per_instance: int
    target_gpu_depth: int
    target_instance_count: int
    requested_instance_count: int
    accepted_instance_count: int
    provider_ready_instance_count: int
    application_ready_instance_count: int
    cleaned_instance_count: int
    first_failed_stage: int | None
    failure_class: FailureClass | None
    status: str

    @property
    def requested_gpu_depth(self) -> int:
        return self.requested_instance_count * self.gpu_count_per_instance

    @property
    def accepted_gpu_depth(self) -> int:
        return self.accepted_instance_count * self.gpu_count_per_instance

    @property
    def provider_ready_gpu_depth(self) -> int:
        return self.provider_ready_instance_count * self.gpu_count_per_instance

    @property
    def application_ready_gpu_depth(self) -> int:
        return self.application_ready_instance_count * self.gpu_count_per_instance

    @property
    def cleaned_gpu_depth(self) -> int:
        return self.cleaned_instance_count * self.gpu_count_per_instance


class CapacityProvider(Protocol):
    provider: str

    def create_replicas(
        self, cell: TrialCell, requests: Sequence[ReplicaRequest]
    ) -> Sequence[CreateResult]: ...

    def observe(self, resource_id: str, cell: TrialCell) -> ResourceObservation: ...

    def delete(self, resource_id: str, cell: TrialCell) -> DeleteResult: ...

    def verify_deleted(self, resource_id: str, cell: TrialCell) -> DeleteResult: ...


HARD_STOP_FAILURES = {
    FailureClass.CAPACITY,
    FailureClass.QUOTA,
    FailureClass.AUTH,
    FailureClass.UNSUPPORTED_SPEC,
}


class CapacityRunner:
    def __init__(
        self,
        store: TraceStore,
        provider: CapacityProvider,
        *,
        poll_interval_seconds: float = 20.0,
        ready_timeout_seconds: float = 600.0,
        sleep: Callable[[float], None] = time.sleep,
        monotonic: Callable[[], float] = time.monotonic,
        max_transient_retries: int = 1,
        create_reconcile_timeout_seconds: float = 90.0,
        create_reconcile_poll_interval_seconds: float = 5.0,
        require_application_ready: bool = True,
    ) -> None:
        self.store = store
        self.provider = provider
        self.poll_interval_seconds = poll_interval_seconds
        self.ready_timeout_seconds = ready_timeout_seconds
        self.sleep = sleep
        self.monotonic = monotonic
        self.max_transient_retries = max_transient_retries
        self.create_reconcile_timeout_seconds = create_reconcile_timeout_seconds
        self.create_reconcile_poll_interval_seconds = (
            create_reconcile_poll_interval_seconds
        )
        self.require_application_ready = require_application_ready
        if create_reconcile_timeout_seconds < 0:
            raise ValueError("create reconcile timeout must not be negative")
        if create_reconcile_poll_interval_seconds <= 0:
            raise ValueError("create reconcile poll interval must be positive")

    def run_trial(
        self,
        run_id: str,
        cell: TrialCell,
        *,
        target_gpu_depth: int = 8,
        trial_id: str | None = None,
    ) -> CapacityTrialResult:
        stages = capacity_stages(
            target_gpu_depth=target_gpu_depth,
            gpu_count_per_instance=cell.gpu_count,
        )
        if cell.provider != self.provider.provider:
            raise ValueError("trial cell provider does not match capacity provider")

        trial_id = self.store.start_capacity_trial(
            run_id,
            cell,
            target_gpu_depth,
            trial_id=trial_id,
        )
        accepted: list[tuple[str, str, int]] = []
        first_failed_stage: int | None = None
        failure_class: FailureClass | None = None
        trial_started = self.monotonic()
        seen_api_calls: set[str] = set()

        try:
            logical_ordinal = 0
            for stage in stages:
                requests: list[ReplicaRequest] = []
                for _ in range(stage.increment_instances):
                    replica_id = self.store.start_replica(
                        trial_id,
                        stage_target=stage.target_gpu_depth,
                        ordinal=logical_ordinal,
                    )
                    requests.append(
                        ReplicaRequest(
                            replica_id=replica_id,
                            name=(
                                f"aibrix-depth-{trial_id[:8]}"
                                f"-g{stage.target_gpu_depth}-r{logical_ordinal}"
                            ),
                            stage_target=stage.target_gpu_depth,
                            ordinal=logical_ordinal,
                        )
                    )
                    logical_ordinal += 1

                results = list(self.provider.create_replicas(cell, requests))
                if len(results) != len(requests):
                    results.extend(
                        CreateResult(
                            accepted=False,
                            failure_class=FailureClass.UNKNOWN,
                            message="provider returned fewer create results than requests",
                        )
                        for _ in range(len(requests) - len(results))
                    )
                    results = results[: len(requests)]

                stage_failed = False
                for request, result in zip(requests, results):
                    final_request, final_result = self._resolve_create_result(
                        run_id,
                        trial_id,
                        cell,
                        request,
                        result,
                        seen_api_calls,
                    )
                    if final_result.accepted and final_result.provider_resource_id:
                        self.store.mark_replica_created(
                            final_request.replica_id,
                            final_result.provider_resource_id,
                            final_result.price_usd_hr,
                            provider_location=final_result.location,
                            machine_id=final_result.machine_id,
                            details={
                                "create_request": final_result.request_payload,
                                "create_response": final_result.raw,
                                "create_api": self._api_details(
                                    final_result.api_call
                                ),
                            },
                        )
                        accepted.append(
                            (
                                final_request.replica_id,
                                final_result.provider_resource_id,
                                final_request.stage_target,
                            )
                        )
                    else:
                        stage_failed = True
                        current_class = final_result.failure_class or FailureClass.UNKNOWN
                        self.store.mark_replica_failed(
                            final_request.replica_id,
                            current_class.value,
                            final_result.message,
                            {
                                "create_request": final_result.request_payload,
                                "failure_response": final_result.raw,
                                "failure_api": self._api_details(
                                    final_result.api_call
                                ),
                            },
                        )
                        if first_failed_stage is None:
                            first_failed_stage = stage.target_gpu_depth
                            failure_class = current_class

                if stage_failed:
                    break

            readiness_failure = self._wait_ready(
                run_id, cell, accepted, seen_api_calls
            )
            readiness_failed_trial = readiness_failure is not None and first_failed_stage is None
            if readiness_failed_trial:
                first_failed_stage, failure_class = readiness_failure
        finally:
            self._cleanup(run_id, cell, accepted, seen_api_calls)

        row = self.store.capacity_trial_row(trial_id)
        if row["cleaned_instance_count"] < row["accepted_instance_count"]:
            status = "cleanup_incomplete"
        elif readiness_failed_trial:
            status = "readiness_failure"
        elif failure_class in {
            FailureClass.TIMEOUT,
            FailureClass.TRANSIENT,
            FailureClass.RATE_LIMIT,
        }:
            status = "transport_censored"
        else:
            status = "complete" if first_failed_stage is None else "depth_boundary"
        self.store.finish_capacity_trial(
            trial_id,
            status=status,
            first_failed_stage=first_failed_stage,
            failure_class=failure_class.value if failure_class else None,
            details={
                "elapsed_seconds": max(0.0, self.monotonic() - trial_started),
                "require_application_ready": self.require_application_ready,
            },
        )
        row = self.store.capacity_trial_row(trial_id)
        return CapacityTrialResult(
            trial_id=trial_id,
            gpu_count_per_instance=cell.gpu_count,
            target_gpu_depth=target_gpu_depth,
            target_instance_count=target_gpu_depth // cell.gpu_count,
            requested_instance_count=row["requested_instance_count"],
            accepted_instance_count=row["accepted_instance_count"],
            provider_ready_instance_count=row["provider_ready_instance_count"],
            application_ready_instance_count=row["application_ready_instance_count"],
            cleaned_instance_count=row["cleaned_instance_count"],
            first_failed_stage=row["first_failed_stage"],
            failure_class=FailureClass(row["failure_class"]) if row["failure_class"] else None,
            status=row["status"],
        )

    def _resolve_create_result(
        self,
        run_id: str,
        trial_id: str,
        cell: TrialCell,
        request: ReplicaRequest,
        result: CreateResult,
        seen_api_calls: set[str],
    ) -> tuple[ReplicaRequest, CreateResult]:
        current_request = request
        current_result = result
        self._record_api_call(
            run_id, cell.provider, current_result.api_call, seen_api_calls
        )
        retry_count = 0
        while not current_result.accepted:
            recovered = self._reconcile_ambiguous_create(
                run_id,
                cell,
                current_request,
                current_result,
                seen_api_calls,
            )
            if recovered is not None:
                current_result = recovered
                break
            if (
                current_result.failure_class
                not in {
                    FailureClass.TIMEOUT,
                    FailureClass.TRANSIENT,
                    FailureClass.RATE_LIMIT,
                }
                or retry_count >= self.max_transient_retries
            ):
                break
            self.store.mark_replica_failed(
                current_request.replica_id,
                (current_result.failure_class or FailureClass.UNKNOWN).value,
                current_result.message,
                {
                    "failure_response": current_result.raw,
                    "failure_api": self._api_details(current_result.api_call),
                },
            )
            retry_count += 1
            retry_id = self.store.start_replica(
                trial_id,
                current_request.stage_target,
                current_request.ordinal,
                attempt=retry_count,
                retry_of=current_request.replica_id,
            )
            current_request = dataclasses.replace(
                current_request,
                replica_id=retry_id,
                name=f"{request.name}-a{retry_count}",
                attempt=retry_count,
            )
            retry_results = list(self.provider.create_replicas(cell, [current_request]))
            current_result = retry_results[0] if retry_results else CreateResult(
                False,
                failure_class=FailureClass.UNKNOWN,
                message="provider returned no retry result",
            )
            self._record_api_call(
                run_id, cell.provider, current_result.api_call, seen_api_calls
            )
        return current_request, current_result

    def _reconcile_ambiguous_create(
        self,
        run_id: str,
        cell: TrialCell,
        request: ReplicaRequest,
        result: CreateResult,
        seen_api_calls: set[str],
    ) -> CreateResult | None:
        if result.failure_class not in {FailureClass.TIMEOUT, FailureClass.TRANSIENT}:
            return None
        lookup = getattr(self.provider, "lookup_replica", None)
        if not callable(lookup):
            return None
        deadline = self.monotonic() + self.create_reconcile_timeout_seconds
        while True:
            candidate = lookup(cell, request.name)
            self._record_api_call(
                run_id,
                cell.provider,
                candidate.api_call,
                seen_api_calls,
            )
            if candidate.accepted and candidate.provider_resource_id:
                return dataclasses.replace(
                    candidate,
                    request_payload=(
                        result.request_payload or candidate.request_payload
                    ),
                )
            if self.monotonic() >= deadline:
                return None
            self.sleep(self.create_reconcile_poll_interval_seconds)

    def _wait_ready(
        self,
        run_id: str,
        cell: TrialCell,
        accepted: list[tuple[str, str, int]],
        seen_api_calls: set[str],
    ) -> tuple[int, FailureClass] | None:
        pending = {
            replica_id: (resource_id, stage_target)
            for replica_id, resource_id, stage_target in accepted
        }
        first_failure: tuple[int, FailureClass] | None = None
        deadline = self.monotonic() + self.ready_timeout_seconds
        while pending and self.monotonic() <= deadline:
            completed: list[str] = []
            for replica_id, (resource_id, stage_target) in pending.items():
                observation = self.provider.observe(resource_id, cell)
                self._record_api_call(
                    run_id, cell.provider, observation.api_call, seen_api_calls
                )
                if not observation.exists:
                    self.store.mark_replica_failed(
                        replica_id,
                        FailureClass.REMOTE_NOT_FOUND.value,
                        "provider resource disappeared before readiness",
                        {
                            "last_observation": observation.raw,
                            "last_observe_api": self._api_details(
                                observation.api_call
                            ),
                        },
                    )
                    if first_failure is None:
                        first_failure = (stage_target, FailureClass.REMOTE_NOT_FOUND)
                    completed.append(replica_id)
                    continue
                self.store.mark_replica_observed(
                    replica_id,
                    provider_ready=observation.provider_ready,
                    application_ready=observation.application_ready,
                    provider_location=observation.location,
                    machine_id=observation.machine_id,
                    realized_price_usd_hr=observation.price_usd_hr,
                    details={
                        "provider_state": observation.provider_state,
                        "last_observation": observation.raw,
                        "last_observe_api": self._api_details(
                            observation.api_call
                        ),
                    },
                )
                if observation.provider_ready and (
                    not self.require_application_ready or observation.application_ready is True
                ):
                    completed.append(replica_id)
            for replica_id in completed:
                pending.pop(replica_id, None)
            if pending:
                self.sleep(self.poll_interval_seconds)
        for replica_id, (_resource_id, stage_target) in pending.items():
            self.store.mark_replica_failed(
                replica_id,
                FailureClass.TIMEOUT.value,
                "resource did not become ready before timeout",
            )
            if first_failure is None:
                first_failure = (stage_target, FailureClass.TIMEOUT)
        return first_failure

    def _cleanup(
        self,
        run_id: str,
        cell: TrialCell,
        accepted: list[tuple[str, str, int]],
        seen_api_calls: set[str],
    ) -> None:
        resources = list(reversed(accepted))
        if not resources:
            return
        delete_many = getattr(self.provider, "delete_replicas", None)
        if callable(delete_many):
            delete_results = list(
                delete_many(tuple(item[1] for item in resources), cell)
            )
        else:
            delete_results = [
                self.provider.delete(resource_id, cell)
                for _replica_id, resource_id, _stage_target in resources
            ]
        if len(delete_results) < len(resources):
            delete_results.extend(
                DeleteResult(
                    False,
                    failure_class=FailureClass.UNKNOWN,
                    message="provider returned fewer delete results than resources",
                )
                for _ in range(len(resources) - len(delete_results))
            )
        pending_verification: list[tuple[str, str, int]] = []
        for (replica_id, resource_id, stage_target), result in zip(
            resources,
            delete_results,
        ):
            self._record_api_call(
                run_id, cell.provider, result.api_call, seen_api_calls
            )
            self.store.record_cleanup(
                run_id,
                owner_type="capacity_replica",
                owner_id=replica_id,
                provider=cell.provider,
                provider_resource_id=resource_id,
                action="delete",
                status="accepted" if result.accepted else "failed",
                failure_class=result.failure_class.value if result.failure_class else None,
                details={"missing": result.missing, "message": result.message, "raw": result.raw},
            )
            if result.accepted or result.missing:
                if result.missing:
                    self.store.mark_replica_cleaned(replica_id)
                    continue
                pending_verification.append((replica_id, resource_id, stage_target))

        if not pending_verification:
            return
        verify_many = getattr(self.provider, "verify_deleted_replicas", None)
        if callable(verify_many):
            verified_results = list(
                verify_many(
                    tuple(item[1] for item in pending_verification),
                    cell,
                )
            )
        else:
            verified_results = [
                self.provider.verify_deleted(resource_id, cell)
                for _replica_id, resource_id, _stage_target in pending_verification
            ]
        if len(verified_results) < len(pending_verification):
            verified_results.extend(
                DeleteResult(
                    False,
                    failure_class=FailureClass.UNKNOWN,
                    message="provider returned fewer verification results than resources",
                )
                for _ in range(len(pending_verification) - len(verified_results))
            )
        for (replica_id, resource_id, _stage_target), verified in zip(
            pending_verification,
            verified_results,
        ):
                self._record_api_call(
                    run_id, cell.provider, verified.api_call, seen_api_calls
                )
                self.store.record_cleanup(
                    run_id,
                    owner_type="capacity_replica",
                    owner_id=replica_id,
                    provider=cell.provider,
                    provider_resource_id=resource_id,
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
                    self.store.mark_replica_cleaned(replica_id)

    def _record_api_call(
        self,
        run_id: str,
        provider: str,
        call: ProviderAPICall | None,
        seen_api_calls: set[str],
    ) -> None:
        if call is None or call.id in seen_api_calls:
            return
        seen_api_calls.add(call.id)
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
    def _api_details(call: ProviderAPICall | None) -> dict[str, object] | None:
        if call is None:
            return None
        return {
            "id": call.id,
            "operation": call.operation,
            "status": call.status,
            "latency_ms": call.latency_ms,
            "failure_class": (
                call.failure_class.value if call.failure_class else None
            ),
        }
