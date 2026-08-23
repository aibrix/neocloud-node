from __future__ import annotations

import json
import os
import threading
import time
import urllib.parse
import uuid
from typing import Any, Callable, Iterable, Sequence

from ..model import CatalogQuote, FailureClass, TrialCell
from .common import (
    CatalogFetch,
    CreateResult,
    DeleteResult,
    HTTPResponse,
    ProviderAPICall,
    ResourceObservation,
    Transport,
    classify_failure,
    provider_api_call,
    request_json,
)


LAMBDA_BASE_URL = "https://cloud.lambda.ai/api/v1"

class LambdaResearchClient:
    provider = "lambda"

    def __init__(
        self,
        *,
        api_key: str | None = None,
        ssh_key_names: Iterable[str] | None = None,
        base_url: str | None = None,
        transport: Transport = request_json,
        timeout: int = 30,
        sleep: Callable[[float], None] = time.sleep,
        monotonic: Callable[[], float] = time.monotonic,
        launch_interval_seconds: float = 12.0,
        max_launch_quantity: int = 1,
        cleanup_poll_interval_seconds: float = 5.0,
        cleanup_timeout_seconds: float = 600.0,
    ) -> None:
        self.api_key = api_key if api_key is not None else os.environ.get("LAMBDA_CLOUD_API_KEY", "")
        configured_keys = os.environ.get("LAMBDA_CLOUD_SSH_KEYS", "").split(",")
        self.ssh_key_names = tuple(ssh_key_names if ssh_key_names is not None else configured_keys)
        self.ssh_key_names = tuple(key.strip() for key in self.ssh_key_names if key.strip())
        self.base_url = (base_url or os.environ.get("LAMBDA_CLOUD_BASE_URL") or LAMBDA_BASE_URL).rstrip("/")
        self.transport = transport
        self.timeout = timeout
        self.sleep = sleep
        self.monotonic = monotonic
        self.launch_interval_seconds = launch_interval_seconds
        if max_launch_quantity != 1:
            raise ValueError(
                "Lambda public launch API supports one instance per request"
            )
        self.max_launch_quantity = max_launch_quantity
        self.cleanup_poll_interval_seconds = cleanup_poll_interval_seconds
        self.cleanup_timeout_seconds = cleanup_timeout_seconds
        self._last_launch_monotonic: float | None = None
        self._launch_lock = threading.Lock()

    def headers(self) -> dict[str, str]:
        if not self.api_key:
            raise RuntimeError("LAMBDA_CLOUD_API_KEY is required")
        return {"Authorization": f"Bearer {self.api_key}"}

    def _request(self, method: str, path: str, body: Any = None) -> HTTPResponse:
        return self.transport(method, self.base_url + path, self.headers(), body, self.timeout)

    def catalog_fetches(self, gpu_counts: tuple[int, ...] = (1,)) -> list[CatalogFetch]:
        del gpu_counts
        response = self._request("GET", "/instance-types")
        quotes: list[CatalogQuote] = []
        if response.ok:
            for name, entry in ((response.payload or {}).get("data") or {}).items():
                instance_type = entry.get("instance_type") or entry
                specs = instance_type.get("specs") or {}
                gpu_count = int(specs.get("gpus") or 0)
                if gpu_count <= 0:
                    continue
                base = dict(
                    provider=self.provider,
                    sku=name,
                    gpu_type=instance_type.get("gpu_description") or name,
                    gpu_count=gpu_count,
                    resource_class="on-demand",
                    market="on-demand",
                    quoted_price_usd_hr=float(instance_type.get("price_cents_per_hour") or 0) / 100.0,
                    vcpus=specs.get("vcpus"),
                    memory_gib=specs.get("memory_gib"),
                    storage_gib=specs.get("storage_gib"),
                    query={"endpoint": "instance-types"},
                    raw=entry,
                )
                regions = [
                    region.get("name")
                    for region in entry.get("regions_with_capacity_available") or []
                    if isinstance(region, dict) and region.get("name")
                ]
                if regions:
                    quotes.extend(CatalogQuote(**base, region=region, available=True) for region in regions)
                else:
                    quotes.append(CatalogQuote(**base, region=None, available=False))
        return [CatalogFetch(self.provider, "instance-types", response, tuple(quotes))]

    def create_replicas(self, cell: TrialCell, requests: Sequence[Any]) -> list[CreateResult]:
        if not requests:
            return []
        if not cell.region:
            return [
                CreateResult(False, failure_class=FailureClass.UNSUPPORTED_SPEC, message="Lambda region is required")
                for _ in requests
            ]
        if not self.ssh_key_names:
            return [
                CreateResult(False, failure_class=FailureClass.UNSUPPORTED_SPEC, message="Lambda SSH key is required")
                for _ in requests
            ]
        results: list[CreateResult] = []
        for start in range(0, len(requests), self.max_launch_quantity):
            batch = requests[start : start + self.max_launch_quantity]
            results.extend(self._create_batch(cell, batch))
        return results

    def lookup_replica(self, cell: TrialCell, name: str) -> CreateResult:
        response = self._request("GET", "/instances")
        items = (response.payload or {}).get("data") or [] if response.ok else []
        matches = [
            item
            for item in items
            if item.get("name") == name
            and item.get("status") not in ("preempted", "terminated")
        ]
        call = ProviderAPICall(
            id=str(uuid.uuid4()),
            operation="list_instances_for_create",
            status=response.status,
            latency_ms=response.latency_ms,
            failure_class=(
                None
                if response.ok
                else classify_failure(response.status, response.body)
            ),
            raw={"matches": matches} if response.ok else response.body,
        )
        if not response.ok:
            return CreateResult(
                False,
                failure_class=call.failure_class or FailureClass.UNKNOWN,
                message=json.dumps(response.body, default=str)[:1000],
                raw=response.body,
                api_call=call,
            )
        if not matches:
            return CreateResult(
                False,
                failure_class=FailureClass.REMOTE_NOT_FOUND,
                message="no instance matched the deterministic request name",
                raw={"name": name},
                api_call=call,
            )
        if len(matches) > 1:
            return CreateResult(
                False,
                failure_class=FailureClass.UNKNOWN,
                message="multiple instances matched the deterministic request name",
                raw={"matches": matches},
                api_call=call,
            )
        item = matches[0]
        instance_type = item.get("instance_type") or {}
        price = instance_type.get("price_cents_per_hour")
        return CreateResult(
            True,
            provider_resource_id=str(item["id"]),
            price_usd_hr=(float(price) / 100.0 if price is not None else None),
            location=((item.get("region") or {}).get("name") or cell.region),
            machine_id=item.get("id"),
            raw=item,
            api_call=call,
        )

    def _create_batch(
        self,
        cell: TrialCell,
        requests: Sequence[Any],
    ) -> list[CreateResult]:
        if len(requests) != 1:
            raise ValueError(
                "Lambda public launch API supports one instance per request"
            )
        body = {
            "region_name": cell.region,
            "instance_type_name": cell.sku,
            "ssh_key_names": list(self.ssh_key_names),
            "file_system_names": [],
            "name": requests[0].name[:64],
        }
        with self._launch_lock:
            now = self.monotonic()
            if self._last_launch_monotonic is not None:
                wait = self.launch_interval_seconds - (now - self._last_launch_monotonic)
                if wait > 0:
                    self.sleep(wait)
            response = self._request(
                "POST",
                "/instance-operations/launch",
                body,
            )
            self._last_launch_monotonic = self.monotonic()
        call = provider_api_call("launch_instances", response)
        if not response.ok:
            failure = classify_failure(response.status, response.body)
            return [
                CreateResult(
                    False,
                    failure_class=failure,
                    message=json.dumps(response.body, default=str)[:1000],
                    request_payload=body,
                    raw=response.body,
                    api_call=call,
                )
                for _ in requests
            ]
        instance_ids = ((response.payload or {}).get("data") or {}).get("instance_ids") or []
        results: list[CreateResult] = []
        for index, _request in enumerate(requests):
            if index < len(instance_ids) and instance_ids[index]:
                results.append(
                    CreateResult(
                        True,
                        provider_resource_id=str(instance_ids[index]),
                        location=cell.region,
                        request_payload=body,
                        raw=response.payload,
                        api_call=call,
                    )
                )
            else:
                results.append(
                    CreateResult(
                        False,
                        failure_class=FailureClass.UNKNOWN,
                        message="Lambda launch returned fewer instance IDs than requested",
                        request_payload=body,
                        raw=response.payload,
                        api_call=call,
                    )
                )
        return results

    def observe(self, resource_id: str, cell: TrialCell) -> ResourceObservation:
        response = self._request("GET", "/instances/" + urllib.parse.quote(resource_id, safe=""))
        call = provider_api_call("get_instance", response)
        if response.status == 404:
            return ResourceObservation(False, None, False, raw=response.body, api_call=call)
        if not response.ok:
            return ResourceObservation(True, "api-error", False, raw=response.body, api_call=call)
        item = (response.payload or {}).get("data") or {}
        status = item.get("status")
        instance_type = item.get("instance_type") or {}
        price = instance_type.get("price_cents_per_hour")
        return ResourceObservation(
            exists=status not in ("preempted", "terminated", None),
            provider_state=status,
            provider_ready=status == "active",
            application_ready=None,
            location=((item.get("region") or {}).get("name") or cell.region),
            machine_id=item.get("id") or resource_id,
            price_usd_hr=float(price) / 100.0 if price is not None else None,
            raw=item,
            api_call=call,
        )

    def delete(self, resource_id: str, cell: TrialCell) -> DeleteResult:
        return self.delete_replicas((resource_id,), cell)[0]

    def delete_replicas(
        self,
        resource_ids: Sequence[str],
        cell: TrialCell,
    ) -> list[DeleteResult]:
        del cell
        if not resource_ids:
            return []
        response = self._request(
            "POST",
            "/instance-operations/terminate",
            {"instance_ids": list(resource_ids)},
        )
        call = provider_api_call("terminate_instances", response)
        if response.ok:
            return [
                DeleteResult(True, raw=response.payload, api_call=call)
                for _ in resource_ids
            ]
        if response.status == 404:
            return [
                DeleteResult(True, missing=True, raw=response.body, api_call=call)
                for _ in resource_ids
            ]
        failure = classify_failure(response.status, response.body)
        return [
            DeleteResult(
                False,
                failure_class=failure,
                message=json.dumps(response.body, default=str)[:1000],
                raw=response.body,
                api_call=call,
            )
            for _ in resource_ids
        ]

    def verify_deleted(self, resource_id: str, cell: TrialCell) -> DeleteResult:
        return self.verify_deleted_replicas((resource_id,), cell)[0]

    def verify_deleted_replicas(
        self,
        resource_ids: Sequence[str],
        cell: TrialCell,
    ) -> list[DeleteResult]:
        del cell
        ordered = tuple(resource_ids)
        if not ordered:
            return []
        pending = set(ordered)
        deadline = self.monotonic() + self.cleanup_timeout_seconds
        last_call: ProviderAPICall | None = None
        last_states: dict[str, str | None] = {}
        while pending and self.monotonic() <= deadline:
            response = self._request("GET", "/instances")
            items = (response.payload or {}).get("data") or [] if response.ok else []
            targets = [item for item in items if str(item.get("id")) in pending]
            last_states = {
                str(item.get("id")): item.get("status") for item in targets
            }
            last_call = ProviderAPICall(
                id=str(uuid.uuid4()),
                operation="list_instances_for_cleanup",
                status=response.status,
                latency_ms=response.latency_ms,
                failure_class=(
                    None
                    if response.ok
                    else classify_failure(response.status, response.body)
                ),
                raw={"target_states": last_states} if response.ok else response.body,
            )
            if response.ok:
                pending.intersection_update(last_states)
                if not pending:
                    break
            if self.monotonic() >= deadline:
                break
            self.sleep(self.cleanup_poll_interval_seconds)
        return [
            (
                DeleteResult(
                    True,
                    missing=True,
                    raw={"last_target_states": last_states},
                    api_call=last_call,
                )
                if resource_id not in pending
                else DeleteResult(
                    False,
                    failure_class=FailureClass.TIMEOUT,
                    message="provider resource still exists after delete timeout",
                    raw={"last_target_states": last_states},
                    api_call=last_call,
                )
            )
            for resource_id in ordered
        ]
