from __future__ import annotations

import dataclasses
import json
import time
import urllib.error
import urllib.request
import uuid
from typing import Any, Callable, Optional

from ..model import CatalogQuote, FailureClass


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
