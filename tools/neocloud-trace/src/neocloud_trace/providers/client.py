from __future__ import annotations

import dataclasses
import concurrent.futures
import json
import os
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Callable, Iterable, Optional, Sequence

from ..model import CatalogQuote, FailureClass, TrialCell


LAMBDA_BASE_URL = "https://cloud.lambda.ai/api/v1"
RUNPOD_REST_URL = "https://rest.runpod.io/v1"
RUNPOD_GRAPHQL_URL = "https://api.runpod.io/graphql"
USER_AGENT = "aibrix-neocloud-research-trace/2.0"


@dataclasses.dataclass(frozen=True)
class HTTPResponse:
    status: int | str
    latency_ms: int
    payload: Any = None
    error: Any = None

    @property
    def ok(self) -> bool:
        return isinstance(self.status, int) and 200 <= self.status <= 299

    @property
    def body(self) -> Any:
        return self.payload if self.ok else self.error


@dataclasses.dataclass(frozen=True)
class CatalogFetch:
    provider: str
    operation: str
    response: HTTPResponse
    quotes: tuple[CatalogQuote, ...]


@dataclasses.dataclass(frozen=True)
class ProviderAPICall:
    id: str
    operation: str
    status: int | str
    latency_ms: int
    failure_class: FailureClass | None
    raw: Any


@dataclasses.dataclass(frozen=True)
class CreateResult:
    accepted: bool
    provider_resource_id: str | None = None
    failure_class: FailureClass | None = None
    message: str | None = None
    price_usd_hr: float | None = None
    location: str | None = None
    machine_id: str | None = None
    raw: Any = None
    api_call: ProviderAPICall | None = None
    request_payload: Any = None


@dataclasses.dataclass(frozen=True)
class ResourceObservation:
    exists: bool
    provider_state: str | None
    provider_ready: bool
    application_ready: bool | None = None
    application_latency_ms: int | None = None
    heartbeat_version: str | None = None
    location: str | None = None
    machine_id: str | None = None
    runtime_uptime_seconds: float | None = None
    price_usd_hr: float | None = None
    raw: Any = None
    api_call: ProviderAPICall | None = None


@dataclasses.dataclass(frozen=True)
class DeleteResult:
    accepted: bool
    missing: bool = False
    failure_class: FailureClass | None = None
    message: str | None = None
    raw: Any = None
    api_call: ProviderAPICall | None = None


@dataclasses.dataclass(frozen=True)
class ProviderBillingRecord:
    provider: str
    provider_resource_id: str | None
    period_start: str
    amount_usd: float
    billed_duration_ms: int | None
    source: str
    source_record_id: str
    currency: str = "USD"
    gpu_type: str | None = None
    disk_space_billed_gb: float | None = None
    raw: Any = None
    api_call: ProviderAPICall | None = None


Transport = Callable[[str, str, dict[str, str], Any, int], HTTPResponse]
HeartbeatProbe = Callable[[str, int], tuple[bool, Optional[int], Optional[str]]]


def _decode(raw: bytes) -> Any:
    if not raw:
        return None
    try:
        return json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return {"_raw": raw[:2000].decode("utf-8", errors="replace")}


def request_json(
    method: str,
    url: str,
    headers: dict[str, str],
    body: Any,
    timeout: int,
) -> HTTPResponse:
    request_headers = dict(headers)
    request_headers.setdefault("Accept", "application/json")
    request_headers.setdefault("User-Agent", USER_AGENT)
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        request_headers.setdefault("Content-Type", "application/json")
    request = urllib.request.Request(url, data=data, headers=request_headers, method=method)
    started = time.monotonic()
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read(4 * 1024 * 1024)
            return HTTPResponse(
                status=response.status,
                latency_ms=round((time.monotonic() - started) * 1000),
                payload=_decode(raw),
            )
    except urllib.error.HTTPError as error:
        return HTTPResponse(
            status=error.code,
            latency_ms=round((time.monotonic() - started) * 1000),
            error=_decode(error.read(4 * 1024 * 1024)),
        )
    except TimeoutError as error:
        return HTTPResponse(
            status="timeout",
            latency_ms=round((time.monotonic() - started) * 1000),
            error=str(error),
        )
    except Exception as error:  # noqa: BLE001 - provider failures are trace data
        return HTTPResponse(
            status="exception",
            latency_ms=round((time.monotonic() - started) * 1000),
            error=f"{type(error).__name__}: {error}",
        )


def classify_failure(status: int | str, payload: Any) -> FailureClass:
    text = json.dumps(payload, ensure_ascii=False, default=str).lower()
    if status in (401, 403) or "unauthorized" in text or "invalid-api-key" in text:
        return FailureClass.AUTH
    if status == 429 or "rate limit" in text or "rate_limit" in text:
        return FailureClass.RATE_LIMIT
    if "quota" in text or "limit exceeded" in text:
        return FailureClass.QUOTA
    if "capacity" in text or "no instances currently available" in text or "out of stock" in text:
        return FailureClass.CAPACITY
    if status == "timeout" or "timeout" in text or "timed out" in text:
        return FailureClass.TIMEOUT
    if status == 400 and ("schema" in text or "enum" in text or "invalid-parameters" in text):
        return FailureClass.UNSUPPORTED_SPEC
    if status == 404 or "not found" in text or "does-not-exist" in text:
        return FailureClass.REMOTE_NOT_FOUND
    if status == "exception" or isinstance(status, int) and status >= 500:
        return FailureClass.TRANSIENT
    return FailureClass.UNKNOWN


def provider_api_call(operation: str, response: HTTPResponse) -> ProviderAPICall:
    return ProviderAPICall(
        id=str(uuid.uuid4()),
        operation=operation,
        status=response.status,
        latency_ms=response.latency_ms,
        failure_class=(
            None
            if response.ok
            else classify_failure(response.status, response.body)
        ),
        raw=response.body,
    )


def probe_heartbeat(url: str, timeout: int = 5) -> tuple[bool, int | None, str | None]:
    started = time.monotonic()
    try:
        request = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read(256).decode("utf-8", errors="replace").strip()
            latency = round((time.monotonic() - started) * 1000)
            return 200 <= response.status <= 299, latency, body or None
    except Exception:  # noqa: BLE001 - heartbeat failure is trace data
        return False, round((time.monotonic() - started) * 1000), None


def research_health_command() -> str:
    return (
        "mkdir -p /tmp/aibrix-neocloud-health && "
        "printf 'aibrix-neocloud-health-v1\\n' > /tmp/aibrix-neocloud-health/healthz && "
        "exec python3 -m http.server 8080 --bind 0.0.0.0 "
        "--directory /tmp/aibrix-neocloud-health"
    )


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


class RunPodResearchClient:
    provider = "runpod"

    def __init__(
        self,
        *,
        api_key: str | None = None,
        rest_url: str | None = None,
        graphql_url: str | None = None,
        transport: Transport = request_json,
        timeout: int = 30,
        heartbeat_probe: HeartbeatProbe = probe_heartbeat,
        max_workers: int = 16,
        sleep: Callable[[float], None] = time.sleep,
        monotonic: Callable[[], float] = time.monotonic,
        cleanup_poll_interval_seconds: float = 5.0,
        cleanup_timeout_seconds: float = 120.0,
    ) -> None:
        self.api_key = api_key if api_key is not None else os.environ.get("RUNPOD_API_KEY", "")
        self.rest_url = (rest_url or os.environ.get("RUNPOD_BASE_URL") or RUNPOD_REST_URL).rstrip("/")
        self.graphql_url = (graphql_url or os.environ.get("RUNPOD_GRAPHQL_URL") or RUNPOD_GRAPHQL_URL).rstrip("/")
        self.transport = transport
        self.timeout = timeout
        self.heartbeat_probe = heartbeat_probe
        if max_workers <= 0:
            raise ValueError("max_workers must be positive")
        self.max_workers = max_workers
        self.sleep = sleep
        self.monotonic = monotonic
        self.cleanup_poll_interval_seconds = cleanup_poll_interval_seconds
        self.cleanup_timeout_seconds = cleanup_timeout_seconds

    def headers(self) -> dict[str, str]:
        if not self.api_key:
            raise RuntimeError("RUNPOD_API_KEY is required")
        return {"Authorization": f"Bearer {self.api_key}"}

    def _request(self, method: str, url: str, body: Any = None) -> HTTPResponse:
        return self.transport(method, url, self.headers(), body, self.timeout)

    def catalog_fetches(self, gpu_counts: tuple[int, ...] = (1,)) -> list[CatalogFetch]:
        fetches: list[CatalogFetch] = []
        for gpu_count in gpu_counts:
            for resource_class, secure in (("secure", True), ("community", False)):
                secure_literal = "true" if secure else "false"
                query = f"""
                query {{
                  gpuTypes {{
                    id
                    displayName
                    memoryInGb
                    lowestPrice(input: {{gpuCount: {gpu_count}, secureCloud: {secure_literal}}}) {{
                      stockStatus
                      availableGpuCounts
                      uninterruptablePrice
                      minimumBidPrice
                    }}
                  }}
                }}
                """
                response = self._request("POST", self.graphql_url, {"query": query})
                quotes: list[CatalogQuote] = []
                if response.ok:
                    gpu_types = ((response.payload or {}).get("data") or {}).get("gpuTypes") or []
                    for item in gpu_types:
                        price = item.get("lowestPrice") or {}
                        stock_status = price.get("stockStatus")
                        available_counts = tuple(
                            int(value) for value in price.get("availableGpuCounts") or []
                        )
                        available = stock_status not in (None, "None") and (
                            not available_counts or gpu_count in available_counts
                        )
                        quotes.append(
                            CatalogQuote(
                                provider=self.provider,
                                sku=item.get("id") or item.get("displayName") or "unknown",
                                gpu_type=item.get("displayName") or item.get("id") or "unknown",
                                gpu_count=gpu_count,
                                resource_class=resource_class,
                                market="on-demand",
                                quoted_price_usd_hr=price.get("uninterruptablePrice"),
                                spot_price_usd_hr=price.get("minimumBidPrice"),
                                stock_status=stock_status,
                                available_gpu_counts=available_counts,
                                available=available,
                                query={"gpu_count": gpu_count, "secure_cloud": secure},
                                raw=item,
                            )
                        )
                operation = f"graphql.gpuTypes.{resource_class}.g{gpu_count}"
                fetches.append(CatalogFetch(self.provider, operation, response, tuple(quotes)))
        return fetches

    def billing_records(
        self,
        *,
        start_time: str,
        resource_ids: Sequence[str],
        bucket_size: str = "hour",
    ) -> list[ProviderBillingRecord]:
        if bucket_size not in {"hour", "day", "week", "month", "year"}:
            raise ValueError("unsupported RunPod billing bucket size")
        records: list[ProviderBillingRecord] = []
        for resource_id in resource_ids:
            query = urllib.parse.urlencode(
                {
                    "bucketSize": bucket_size,
                    "grouping": "podId",
                    "podId": resource_id,
                    "startTime": start_time,
                }
            )
            response = self._request(
                "GET",
                f"{self.rest_url}/billing/pods?{query}",
            )
            call = provider_api_call("billing_pods", response)
            if not response.ok:
                raise RuntimeError(
                    "RunPod billing history request failed: "
                    f"status={response.status}"
                )
            payload = response.payload or []
            if isinstance(payload, dict):
                payload = payload.get("data") or []
            for item in payload:
                pod_id = item.get("podId") or resource_id
                period_start = str(item.get("time") or "")
                amount = _float_or_none(item.get("amount"))
                if not period_start or amount is None:
                    continue
                identity = json.dumps(
                    {
                        "pod_id": pod_id,
                        "time": period_start,
                        "amount": amount,
                        "time_billed_ms": item.get("timeBilledMs"),
                    },
                    sort_keys=True,
                )
                records.append(
                    ProviderBillingRecord(
                        provider=self.provider,
                        provider_resource_id=str(pod_id) if pod_id else None,
                        period_start=period_start,
                        amount_usd=amount,
                        billed_duration_ms=(
                            int(item["timeBilledMs"])
                            if item.get("timeBilledMs") is not None
                            else None
                        ),
                        source="runpod_billing_api",
                        source_record_id=str(
                            uuid.uuid5(uuid.NAMESPACE_URL, identity)
                        ),
                        gpu_type=item.get("gpuTypeId"),
                        disk_space_billed_gb=_float_or_none(
                            item.get("diskSpaceBilledGb")
                        ),
                        raw=item,
                        api_call=call,
                    )
                )
        return records

    def create_replicas(self, cell: TrialCell, requests: Sequence[Any]) -> list[CreateResult]:
        if not requests:
            return []
        barrier = threading.Barrier(len(requests))
        workers = min(self.max_workers, len(requests))
        with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as executor:
            futures = [
                executor.submit(self._create_one, cell, request, barrier)
                for request in requests
            ]
            return [future.result() for future in futures]

    def _create_one(self, cell: TrialCell, request: Any, barrier: threading.Barrier) -> CreateResult:
        body = {
            "name": request.name,
            "computeType": "GPU",
            "cloudType": cell.resource_class.upper(),
            "imageName": cell.image or "python:3.12-slim",
            "gpuCount": cell.gpu_count,
            "gpuTypeIds": [cell.sku],
            "gpuTypePriority": "availability",
            "containerDiskInGb": 20,
            "volumeInGb": 0,
            "supportPublicIp": True,
            "ports": ["8080/http"],
            "interruptible": cell.market in ("spot", "interruptible"),
            "dockerEntrypoint": ["/bin/sh", "-lc"],
            "dockerStartCmd": [research_health_command()],
        }
        if cell.datacenter:
            body["dataCenterIds"] = [cell.datacenter]
            body["dataCenterPriority"] = "availability"
        try:
            barrier.wait(timeout=10)
        except threading.BrokenBarrierError:
            return CreateResult(
                False,
                failure_class=FailureClass.TRANSIENT,
                message="create barrier failed",
                request_payload=body,
            )
        response = self._request("POST", self.rest_url + "/pods", body)
        call = provider_api_call("create_pod", response)
        if not response.ok:
            failure = classify_failure(response.status, response.body)
            return CreateResult(
                False,
                failure_class=failure,
                message=json.dumps(response.body, ensure_ascii=False, default=str)[:1000],
                request_payload=body,
                raw=response.body,
                api_call=call,
            )
        pod = response.payload or {}
        pod_id = pod.get("id")
        if not pod_id:
            return CreateResult(
                False,
                failure_class=FailureClass.UNKNOWN,
                message="RunPod create returned no pod ID",
                request_payload=body,
                raw=pod,
                api_call=call,
            )
        machine = pod.get("machine") or {}
        return CreateResult(
            True,
            provider_resource_id=str(pod_id),
            price_usd_hr=_float_or_none(pod.get("costPerHr")),
            location=machine.get("dataCenterId") or pod.get("dataCenterId"),
            machine_id=pod.get("machineId"),
            request_payload=body,
            raw=pod,
            api_call=call,
        )

    def observe(self, resource_id: str, cell: TrialCell) -> ResourceObservation:
        url = self.rest_url + "/pods/" + urllib.parse.quote(resource_id, safe="") + "?includeMachine=true"
        response = self._request("GET", url)
        call = provider_api_call("get_pod", response)
        if response.status == 404:
            return ResourceObservation(False, None, False, raw=response.body, api_call=call)
        if not response.ok:
            return ResourceObservation(True, "api-error", False, raw=response.body, api_call=call)
        item = response.payload or {}
        if isinstance(item, dict) and isinstance(item.get("data"), dict):
            item = item["data"]
        state = item.get("desiredStatus")
        machine = item.get("machine") or {}
        machine_id = item.get("machineId")
        provider_ready = state == "RUNNING" and bool(machine_id)
        heartbeat_ok = False
        heartbeat_latency = None
        heartbeat_version = None
        if provider_ready:
            endpoint = _runpod_health_endpoint(item)
            if endpoint:
                heartbeat_ok, heartbeat_latency, heartbeat_version = self.heartbeat_probe(endpoint, 5)
        runtime = item.get("runtime") or {}
        return ResourceObservation(
            exists=state not in ("EXITED", "TERMINATED", None),
            provider_state=state,
            provider_ready=provider_ready,
            application_ready=heartbeat_ok,
            application_latency_ms=heartbeat_latency,
            heartbeat_version=heartbeat_version,
            location=machine.get("dataCenterId") or item.get("dataCenterId") or cell.datacenter,
            machine_id=machine_id,
            runtime_uptime_seconds=_float_or_none(runtime.get("uptimeInSeconds")),
            price_usd_hr=_float_or_none(item.get("costPerHr")),
            raw=item,
            api_call=call,
        )

    def delete(self, resource_id: str, cell: TrialCell) -> DeleteResult:
        del cell
        url = self.rest_url + "/pods/" + urllib.parse.quote(resource_id, safe="")
        response = self._request("DELETE", url)
        call = provider_api_call("delete_pod", response)
        if response.ok:
            return DeleteResult(True, raw=response.payload, api_call=call)
        if response.status == 404:
            return DeleteResult(True, missing=True, raw=response.body, api_call=call)
        failure = classify_failure(response.status, response.body)
        return DeleteResult(
            False,
            failure_class=failure,
            message=json.dumps(response.body, default=str)[:1000],
            raw=response.body,
            api_call=call,
        )

    def verify_deleted(self, resource_id: str, cell: TrialCell) -> DeleteResult:
        return _poll_deleted(
            self,
            resource_id,
            cell,
            sleep=self.sleep,
            monotonic=self.monotonic,
            poll_interval_seconds=self.cleanup_poll_interval_seconds,
            timeout_seconds=self.cleanup_timeout_seconds,
        )


def _poll_deleted(
    provider: Any,
    resource_id: str,
    cell: TrialCell,
    *,
    sleep: Callable[[float], None],
    monotonic: Callable[[], float],
    poll_interval_seconds: float,
    timeout_seconds: float,
) -> DeleteResult:
    deadline = monotonic() + timeout_seconds
    last_observation: ResourceObservation | None = None
    while True:
        last_observation = provider.observe(resource_id, cell)
        if not last_observation.exists:
            return DeleteResult(
                accepted=True,
                missing=True,
                raw={"provider_state": last_observation.provider_state, "observation": last_observation.raw},
                api_call=last_observation.api_call,
            )
        if monotonic() >= deadline:
            return DeleteResult(
                accepted=False,
                failure_class=FailureClass.TIMEOUT,
                message="provider resource still exists after delete timeout",
                raw={"provider_state": last_observation.provider_state, "observation": last_observation.raw},
                api_call=last_observation.api_call,
            )
        sleep(poll_interval_seconds)


def _float_or_none(value: Any) -> float | None:
    if value is None or value == "":
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _runpod_health_endpoint(item: dict[str, Any]) -> str | None:
    public_ip = item.get("publicIp")
    mappings = item.get("portMappings") or {}
    port = mappings.get("8080") or mappings.get(8080)
    if public_ip and port:
        return f"http://{public_ip}:{port}/healthz"
    pod_id = item.get("id")
    exposed_ports = item.get("ports") or []
    if pod_id and any(str(value).lower() == "8080/http" for value in exposed_ports):
        return f"https://{pod_id}-8080.proxy.runpod.net/healthz"
    runtime = item.get("runtime") or {}
    for port_item in runtime.get("ports") or []:
        if str(port_item.get("privatePort")) == "8080" and port_item.get("ip") and port_item.get("publicPort"):
            return f"http://{port_item['ip']}:{port_item['publicPort']}/healthz"
    return None
