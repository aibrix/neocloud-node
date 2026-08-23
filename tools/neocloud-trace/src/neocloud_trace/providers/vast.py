from __future__ import annotations

import concurrent.futures
import datetime as dt
import json
import math
import os
import re
import threading
import time
import urllib.parse
import uuid
from collections.abc import Callable, Sequence
from typing import Any

from ..model import CatalogQuote, FailureClass, TrialCell
from .common import (
    CatalogFetch,
    CreateResult,
    DeleteResult,
    HeartbeatProbe,
    HTTPResponse,
    ProviderAPICall,
    ProviderBillingRecord,
    ResourceObservation,
    Transport,
    classify_failure,
    probe_heartbeat,
    request_json,
)


VAST_BASE_URL = "https://console.vast.ai/api/v0"
VAST_V1_BASE_URL = "https://console.vast.ai/api/v1"
VAST_DEFAULT_IMAGE = "python:3.12-slim"


def research_health_command() -> str:
    return (
        "mkdir -p /tmp/aibrix-neocloud-health && "
        "printf 'aibrix-neocloud-health-v1\\n' > "
        "/tmp/aibrix-neocloud-health/healthz && "
        "exec python3 -m http.server 8080 --bind 0.0.0.0 "
        "--directory /tmp/aibrix-neocloud-health"
    )


class VastResearchClient:
    provider = "vast"

    def __init__(
        self,
        *,
        api_key: str | None = None,
        base_url: str | None = None,
        v1_base_url: str | None = None,
        transport: Transport = request_json,
        timeout: int = 30,
        heartbeat_probe: HeartbeatProbe = probe_heartbeat,
        max_workers: int = 16,
        disk_gb: float = 20.0,
        minimum_reliability: float = 0.95,
        catalog_offer_limit: int = 1000,
        sleep: Callable[[float], None] = time.sleep,
        monotonic: Callable[[], float] = time.monotonic,
        wall_time: Callable[[], float] = time.time,
        cleanup_poll_interval_seconds: float = 5.0,
        cleanup_timeout_seconds: float = 120.0,
    ) -> None:
        self.api_key = (
            api_key if api_key is not None else os.environ.get("VAST_API_KEY", "")
        )
        self.base_url = (
            base_url or os.environ.get("VAST_BASE_URL") or VAST_BASE_URL
        ).rstrip("/")
        self.v1_base_url = (
            v1_base_url or _versioned_base_url(self.base_url, "v1")
        ).rstrip("/")
        self.transport = transport
        self.timeout = timeout
        self.heartbeat_probe = heartbeat_probe
        if max_workers <= 0:
            raise ValueError("max_workers must be positive")
        if disk_gb <= 0:
            raise ValueError("disk_gb must be positive")
        if not 0 <= minimum_reliability <= 1:
            raise ValueError("minimum_reliability must be between zero and one")
        if catalog_offer_limit <= 0:
            raise ValueError("catalog_offer_limit must be positive")
        self.max_workers = max_workers
        self.disk_gb = disk_gb
        self.minimum_reliability = minimum_reliability
        self.catalog_offer_limit = catalog_offer_limit
        self.sleep = sleep
        self.monotonic = monotonic
        self.wall_time = wall_time
        self.cleanup_poll_interval_seconds = cleanup_poll_interval_seconds
        self.cleanup_timeout_seconds = cleanup_timeout_seconds

    def headers(self) -> dict[str, str]:
        if not self.api_key:
            raise RuntimeError("VAST_API_KEY is required")
        return {"Authorization": f"Bearer {self.api_key}"}

    def _request(self, method: str, path: str, body: Any = None) -> HTTPResponse:
        return self.transport(
            method,
            self.base_url + path,
            self.headers(),
            body,
            self.timeout,
        )

    def _request_v1(
        self, method: str, path: str, body: Any = None
    ) -> HTTPResponse:
        return self.transport(
            method,
            self.v1_base_url + path,
            self.headers(),
            body,
            self.timeout,
        )

    def catalog_fetches(
        self, gpu_counts: tuple[int, ...] = (1,)
    ) -> list[CatalogFetch]:
        fetches: list[CatalogFetch] = []
        requested_counts = tuple(sorted(set(gpu_counts)))
        for market in ("on-demand", "interruptible"):
            body: dict[str, Any] = {
                "limit": self.catalog_offer_limit,
                "type": _vast_market_type(market),
                "external": {"eq": False},
                "rentable": {"eq": True},
                "rented": {"eq": False},
                "num_gpus": {"in": list(requested_counts)},
                "order": [["dph_total", "asc"]],
                "allocated_storage": self.disk_gb,
            }
            response = self._request("POST", "/bundles/", body)
            quotes: list[CatalogQuote] = []
            if response.ok:
                for offer in _offers(response.payload):
                    gpu_count = _int_or_none(offer.get("num_gpus"))
                    if gpu_count not in requested_counts:
                        continue
                    quotes.append(_catalog_quote(offer, market, body))
            fetches.append(
                CatalogFetch(
                    self.provider,
                    f"search_offers.{_vast_market_type(market)}",
                    response,
                    tuple(quotes),
                )
            )
        return fetches

    def billing_records(
        self,
        *,
        start_time: str,
        resource_ids: Sequence[str],
        bucket_size: str = "hour",
    ) -> list[ProviderBillingRecord]:
        del bucket_size
        wanted = {str(resource_id) for resource_id in resource_ids}
        if not wanted:
            return []
        select_filters = {
            "day": {
                "gte": _epoch_seconds(start_time),
                "lte": int(self.wall_time()),
            },
            "type": {"in": ["instance"]},
        }
        records: list[ProviderBillingRecord] = []
        after_token: str | None = None
        seen_tokens: set[str] = set()
        while True:
            query: dict[str, Any] = {
                "select_filters": json.dumps(
                    select_filters, sort_keys=True, separators=(",", ":")
                ),
                "format": "table",
                "latest_first": "false",
                "limit": 500,
            }
            if after_token:
                query["after_token"] = after_token
            response = self._request(
                "GET", "/charges/?" + urllib.parse.urlencode(query)
            )
            call = _api_call("show_charges", response)
            payload = response.payload or {}
            if not response.ok or (
                isinstance(payload, dict) and payload.get("success") is False
            ):
                raise RuntimeError(
                    "Vast billing history request failed: "
                    f"status={response.status}"
                )
            for item in payload.get("results") or []:
                resource_id = _charge_resource_id(item)
                if resource_id not in wanted:
                    continue
                amount = _float_or_none(item.get("amount"))
                start = _float_or_none(item.get("start"))
                end = _float_or_none(item.get("end"))
                if amount is None or start is None:
                    continue
                identity = json.dumps(
                    {
                        "resource_id": resource_id,
                        "start": start,
                        "end": end,
                        "type": item.get("type"),
                        "source": item.get("source"),
                    },
                    sort_keys=True,
                )
                records.append(
                    ProviderBillingRecord(
                        provider=self.provider,
                        provider_resource_id=resource_id,
                        period_start=_iso_from_epoch(start),
                        amount_usd=amount,
                        billed_duration_ms=_charge_duration_ms(item),
                        source="vast_charges_api",
                        source_record_id=str(
                            uuid.uuid5(uuid.NAMESPACE_URL, identity)
                        ),
                        raw=item,
                        api_call=call,
                    )
                )
            next_token = payload.get("next_token")
            if not next_token or next_token in seen_tokens:
                break
            seen_tokens.add(str(next_token))
            after_token = str(next_token)
        return records

    def create_replicas(
        self, cell: TrialCell, requests: Sequence[Any]
    ) -> list[CreateResult]:
        if not requests:
            return []
        try:
            search_body = self._create_search_body(cell, len(requests))
        except ValueError as error:
            return [
                CreateResult(
                    False,
                    failure_class=FailureClass.UNSUPPORTED_SPEC,
                    message=str(error),
                )
                for _ in requests
            ]
        response = self._request("POST", "/bundles/", search_body)
        search_call = _api_call("search_offers_for_create", response)
        payload = response.payload if response.ok else response.body
        semantic_failure = (
            isinstance(payload, dict) and payload.get("success") is False
        )
        if not response.ok or semantic_failure:
            failure = _classify_vast_failure(response.status, payload)
            search_call = _api_call(
                "search_offers_for_create", response, failure=failure
            )
            return [
                CreateResult(
                    False,
                    failure_class=failure,
                    message=_message(payload),
                    raw=payload,
                    api_call=search_call,
                    request_payload=search_body,
                )
                for _ in requests
            ]

        offers = sorted(
            _deduplicate_offers(_offers(response.payload)),
            key=_offer_sort_key,
        )
        assigned_count = min(len(offers), len(requests))
        assigned_requests = requests[:assigned_count]
        assigned_offers = offers[:assigned_count]
        results: list[CreateResult] = []
        for start in range(0, assigned_count, self.max_workers):
            request_batch = assigned_requests[start : start + self.max_workers]
            offer_batch = assigned_offers[start : start + self.max_workers]
            barrier = threading.Barrier(len(request_batch))
            with concurrent.futures.ThreadPoolExecutor(
                max_workers=len(request_batch)
            ) as executor:
                futures = [
                    executor.submit(
                        self._create_one,
                        cell,
                        request,
                        offer,
                        barrier,
                    )
                    for request, offer in zip(
                        request_batch, offer_batch, strict=True
                    )
                ]
                results.extend(future.result() for future in futures)
        for request in requests[assigned_count:]:
            results.append(
                CreateResult(
                    False,
                    failure_class=FailureClass.CAPACITY,
                    message="Vast search returned fewer distinct offers than requested",
                    raw={
                        "requested_name": request.name,
                        "offer_count": len(offers),
                    },
                    api_call=search_call,
                    request_payload=search_body,
                )
            )
        return results

    def _create_search_body(
        self, cell: TrialCell, request_count: int
    ) -> dict[str, Any]:
        verified = _verified_filter(cell.resource_class)
        body: dict[str, Any] = {
            "limit": max(100, min(1000, request_count * 8)),
            "type": _vast_market_type(cell.market),
            "external": {"eq": False},
            "rentable": {"eq": True},
            "rented": {"eq": False},
            "num_gpus": {"eq": cell.gpu_count},
            "reliability": {"gte": self.minimum_reliability},
            "disk_space": {"gte": self.disk_gb},
            "direct_port_count": {"gte": 1},
            "order": [["dph_total", "asc"], ["reliability", "desc"]],
            "allocated_storage": self.disk_gb,
        }
        if cell.sku.isdigit():
            body["id"] = {"eq": int(cell.sku)}
        else:
            gpu_names = sorted({cell.sku, cell.sku.replace("_", " ")})
            body["gpu_name"] = (
                {"eq": gpu_names[0]}
                if len(gpu_names) == 1
                else {"in": gpu_names}
            )
        if verified is not None:
            body["verified"] = {"eq": verified}
        if cell.region:
            body["geolocation"] = {"eq": cell.region}
        if cell.datacenter:
            try:
                body["machine_id"] = {"eq": int(cell.datacenter)}
            except ValueError as error:
                raise ValueError(
                    "Vast datacenter must be a numeric machine_id"
                ) from error
        return body

    def _create_one(
        self,
        cell: TrialCell,
        request: Any,
        offer: dict[str, Any],
        barrier: threading.Barrier,
    ) -> CreateResult:
        offer_id = offer.get("id") or offer.get("ask_contract_id")
        body: dict[str, Any] = {
            "client_id": "me",
            "image": cell.image or VAST_DEFAULT_IMAGE,
            "env": {"-p 8080:8080": "1"},
            "disk": self.disk_gb,
            "label": request.name[:64],
            "runtype": "args",
            "args": ["/bin/sh", "-lc", research_health_command()],
            "cancel_unavail": True,
        }
        if _vast_market_type(cell.market) == "bid":
            bid_price = _float_or_none(offer.get("min_bid"))
            if bid_price is None or bid_price <= 0:
                return CreateResult(
                    False,
                    failure_class=FailureClass.UNSUPPORTED_SPEC,
                    message="Vast interruptible offer did not include a positive min_bid",
                    request_payload={"offer_id": offer_id, **body},
                    raw=offer,
                )
            body["price"] = bid_price
        try:
            barrier.wait(timeout=10)
        except threading.BrokenBarrierError:
            return CreateResult(
                False,
                failure_class=FailureClass.TRANSIENT,
                message="create barrier failed",
                request_payload={"offer_id": offer_id, **body},
            )
        response = self._request(
            "PUT",
            "/asks/" + urllib.parse.quote(str(offer_id), safe="") + "/",
            body,
        )
        payload = response.payload if response.ok else response.body
        semantic_failure = (
            isinstance(payload, dict) and payload.get("success") is False
        )
        failure = (
            _classify_vast_failure(response.status, payload)
            if not response.ok or semantic_failure
            else None
        )
        call = _api_call("create_instance", response, failure=failure)
        if failure is not None:
            return CreateResult(
                False,
                failure_class=failure,
                message=_message(payload),
                request_payload={"offer_id": offer_id, **body},
                raw=payload,
                api_call=call,
            )
        instance_id = (payload or {}).get("new_contract")
        if instance_id is None:
            return CreateResult(
                False,
                failure_class=FailureClass.UNKNOWN,
                message="Vast create returned no instance ID",
                request_payload={"offer_id": offer_id, **body},
                raw=payload,
                api_call=call,
            )
        return CreateResult(
            True,
            provider_resource_id=str(instance_id),
            price_usd_hr=_offer_price(offer),
            location=offer.get("geolocation") or cell.region,
            machine_id=(
                str(offer["machine_id"])
                if offer.get("machine_id") is not None
                else None
            ),
            request_payload={"offer_id": offer_id, **body},
            raw={"response": payload, "offer": offer},
            api_call=call,
        )

    def lookup_replica(self, cell: TrialCell, name: str) -> CreateResult:
        matches: list[dict[str, Any]] = []
        after_token: str | None = None
        seen_tokens: set[str] = set()
        call: ProviderAPICall | None = None
        while True:
            query: dict[str, Any] = {
                "select_filters": json.dumps(
                    {"label": {"in": [name]}}, separators=(",", ":")
                ),
                "order_by": json.dumps(
                    [{"col": "id", "dir": "asc"}], separators=(",", ":")
                ),
                "limit": 25,
            }
            if after_token:
                query["after_token"] = after_token
            response = self._request_v1(
                "GET", "/instances/?" + urllib.parse.urlencode(query)
            )
            failure = (
                None
                if response.ok
                else _classify_vast_failure(response.status, response.body)
            )
            call = _api_call(
                "list_instances_for_create", response, failure=failure
            )
            if failure is not None:
                return CreateResult(
                    False,
                    failure_class=failure,
                    message=_message(response.body),
                    raw=response.body,
                    api_call=call,
                )
            payload = response.payload or {}
            items = payload.get("instances") or []
            if isinstance(items, dict):
                items = [items]
            matches.extend(
                item
                for item in items
                if item.get("label") == name
                and str(item.get("actual_status") or "").lower()
                not in {"destroyed", "terminated"}
            )
            next_token = payload.get("next_token")
            if not next_token or str(next_token) in seen_tokens:
                break
            seen_tokens.add(str(next_token))
            after_token = str(next_token)
        if not matches:
            return CreateResult(
                False,
                failure_class=FailureClass.REMOTE_NOT_FOUND,
                message="no instance matched the deterministic request label",
                raw={"label": name},
                api_call=call,
            )
        if len(matches) > 1:
            return CreateResult(
                False,
                failure_class=FailureClass.UNKNOWN,
                message="multiple instances matched the deterministic request label",
                raw={"matches": matches},
                api_call=call,
            )
        item = matches[0]
        return CreateResult(
            True,
            provider_resource_id=str(item["id"]),
            price_usd_hr=_float_or_none(item.get("dph_total")),
            location=item.get("geolocation") or cell.region,
            machine_id=(
                str(item["machine_id"])
                if item.get("machine_id") is not None
                else None
            ),
            raw=item,
            api_call=call,
        )

    def observe(self, resource_id: str, cell: TrialCell) -> ResourceObservation:
        response = self._request(
            "GET",
            "/instances/" + urllib.parse.quote(resource_id, safe="") + "/",
        )
        call = _api_call("show_instance", response)
        if response.status == 404:
            return ResourceObservation(
                False, None, False, raw=response.body, api_call=call
            )
        if not response.ok:
            return ResourceObservation(
                True, "api-error", False, raw=response.body, api_call=call
            )
        item = _instance(response.payload)
        if not item:
            return ResourceObservation(
                False, None, False, raw=response.payload, api_call=call
            )
        has_actual_status = "actual_status" in item
        if has_actual_status:
            state = str(item.get("actual_status") or "provisioning")
        else:
            state = str(
                item.get("cur_state") or item.get("intended_status") or "unknown"
            )
        state_lower = state.lower()
        exists = state_lower not in {"destroyed", "terminated", "deleted"}
        provider_ready = (
            str(item.get("actual_status") or "").lower() == "running"
            if has_actual_status
            else state_lower == "running"
        )
        heartbeat_ok = False
        heartbeat_latency = None
        heartbeat_version = None
        if provider_ready:
            endpoint = _health_endpoint(item)
            if endpoint:
                heartbeat_ok, heartbeat_latency, heartbeat_version = (
                    self.heartbeat_probe(endpoint, 5)
                )
        return ResourceObservation(
            exists=exists,
            provider_state=state,
            provider_ready=provider_ready,
            application_ready=heartbeat_ok,
            application_latency_ms=heartbeat_latency,
            heartbeat_version=heartbeat_version,
            location=item.get("geolocation") or cell.region,
            machine_id=(
                str(item["machine_id"])
                if item.get("machine_id") is not None
                else None
            ),
            runtime_uptime_seconds=_runtime_seconds(item, self.wall_time()),
            price_usd_hr=_float_or_none(item.get("dph_total")),
            raw=item,
            api_call=call,
        )

    def delete(self, resource_id: str, cell: TrialCell) -> DeleteResult:
        del cell
        response = self._request(
            "DELETE",
            "/instances/" + urllib.parse.quote(resource_id, safe="") + "/",
        )
        payload = response.payload if response.ok else response.body
        if response.status == 404 or _is_missing_payload(payload):
            call = _api_call("destroy_instance", response)
            return DeleteResult(True, missing=True, raw=payload, api_call=call)
        semantic_failure = (
            isinstance(payload, dict) and payload.get("success") is False
        )
        failure = (
            _classify_vast_failure(response.status, payload)
            if not response.ok or semantic_failure
            else None
        )
        call = _api_call("destroy_instance", response, failure=failure)
        if failure is None:
            return DeleteResult(True, raw=payload, api_call=call)
        return DeleteResult(
            False,
            failure_class=failure,
            message=_message(payload),
            raw=payload,
            api_call=call,
        )

    def verify_deleted(self, resource_id: str, cell: TrialCell) -> DeleteResult:
        deadline = self.monotonic() + self.cleanup_timeout_seconds
        last_observation: ResourceObservation | None = None
        while True:
            last_observation = self.observe(resource_id, cell)
            if not last_observation.exists:
                return DeleteResult(
                    True,
                    missing=True,
                    raw={
                        "provider_state": last_observation.provider_state,
                        "observation": last_observation.raw,
                    },
                    api_call=last_observation.api_call,
                )
            if self.monotonic() >= deadline:
                return DeleteResult(
                    False,
                    failure_class=FailureClass.TIMEOUT,
                    message="provider resource still exists after delete timeout",
                    raw={
                        "provider_state": last_observation.provider_state,
                        "observation": last_observation.raw,
                    },
                    api_call=last_observation.api_call,
                )
            self.sleep(self.cleanup_poll_interval_seconds)


def _api_call(
    operation: str,
    response: HTTPResponse,
    *,
    failure: FailureClass | None = None,
) -> ProviderAPICall:
    return ProviderAPICall(
        id=str(uuid.uuid4()),
        operation=operation,
        status=response.status,
        latency_ms=response.latency_ms,
        failure_class=(
            failure
            if failure is not None
            else (
                None
                if response.ok
                else _classify_vast_failure(response.status, response.body)
            )
        ),
        raw=response.body,
    )


def _versioned_base_url(base_url: str, version: str) -> str:
    prefix, separator, suffix = base_url.rpartition("/api/")
    if separator and suffix.startswith("v"):
        return prefix + "/api/" + version
    return VAST_V1_BASE_URL if version == "v1" else base_url


def _classify_vast_failure(status: int | str, payload: Any) -> FailureClass:
    text = json.dumps(payload, ensure_ascii=False, default=str).lower()
    if status == 410 or "no_such_ask" in text or "no such ask" in text:
        return FailureClass.CAPACITY
    if "not available" in text or "already rented" in text:
        return FailureClass.CAPACITY
    if "insufficient credit" in text or "insufficient balance" in text:
        return FailureClass.QUOTA
    if status == 400 and (
        "invalid_args" in text or "invalid args" in text or "invalid_request" in text
    ):
        return FailureClass.UNSUPPORTED_SPEC
    return classify_failure(status, payload)


def _offers(payload: Any) -> list[dict[str, Any]]:
    if not isinstance(payload, dict):
        return []
    offers = payload.get("offers") or []
    if isinstance(offers, dict):
        return [offers]
    return [offer for offer in offers if isinstance(offer, dict)]


def _deduplicate_offers(offers: Sequence[dict[str, Any]]) -> list[dict[str, Any]]:
    unique: list[dict[str, Any]] = []
    seen: set[str] = set()
    for offer in offers:
        offer_id = offer.get("id") or offer.get("ask_contract_id")
        if offer_id is None or str(offer_id) in seen:
            continue
        seen.add(str(offer_id))
        unique.append(offer)
    return unique


def _catalog_quote(
    offer: dict[str, Any], market: str, query: dict[str, Any]
) -> CatalogQuote:
    gpu_name = str(offer.get("gpu_name") or "unknown")
    gpu_count = int(offer.get("num_gpus") or 0)
    return CatalogQuote(
        provider="vast",
        sku=gpu_name,
        gpu_type=gpu_name.replace("_", " "),
        gpu_count=gpu_count,
        region=offer.get("geolocation"),
        datacenter=(
            str(offer["machine_id"])
            if offer.get("machine_id") is not None
            else None
        ),
        resource_class=("verified" if _is_verified(offer) else "community"),
        market=market,
        quoted_price_usd_hr=_offer_price(offer),
        spot_price_usd_hr=(
            _offer_price(offer)
            if market == "interruptible"
            else _float_or_none(offer.get("min_bid"))
        ),
        stock_status="available",
        available_gpu_counts=(gpu_count,),
        available=True,
        vcpus=_int_or_none(offer.get("cpu_cores_effective")),
        memory_gib=_mib_to_gib(offer.get("cpu_ram")),
        storage_gib=_float_or_none(offer.get("disk_space")),
        query={
            "request": query,
            "offer_id": offer.get("id") or offer.get("ask_contract_id"),
        },
        raw=offer,
    )


def _is_verified(offer: dict[str, Any]) -> bool:
    if isinstance(offer.get("verified"), bool):
        return bool(offer["verified"])
    return str(offer.get("verification") or "").lower() == "verified"


def _verified_filter(resource_class: str) -> bool | None:
    normalized = resource_class.strip().lower().replace("_", "-")
    if normalized in {"verified", "secure", "on-demand", "ondemand"}:
        return True
    if normalized in {"community", "unverified"}:
        return False
    if normalized in {"any", "marketplace"}:
        return None
    raise ValueError(
        "Vast resource_class must be verified, community, or marketplace"
    )


def _vast_market_type(market: str) -> str:
    normalized = market.strip().lower().replace("_", "-")
    if normalized in {"on-demand", "ondemand"}:
        return "on-demand"
    if normalized in {"interruptible", "spot", "bid"}:
        return "bid"
    raise ValueError("Vast market must be on-demand or interruptible")


def _offer_price(offer: dict[str, Any]) -> float | None:
    return (
        _float_or_none(offer.get("dph_total"))
        or _float_or_none(offer.get("dph_total_adj"))
        or _float_or_none(offer.get("min_bid"))
    )


def _offer_sort_key(offer: dict[str, Any]) -> tuple[float, float, str]:
    price = _offer_price(offer)
    reliability = _float_or_none(offer.get("reliability")) or 0.0
    offer_id = str(offer.get("id") or offer.get("ask_contract_id") or "")
    return (price if price is not None else math.inf, -reliability, offer_id)


def _instance(payload: Any) -> dict[str, Any]:
    if not isinstance(payload, dict):
        return {}
    instance = payload.get("instances")
    if isinstance(instance, dict):
        return instance
    if isinstance(instance, list) and len(instance) == 1:
        return instance[0]
    return {}


def _health_endpoint(item: dict[str, Any]) -> str | None:
    public_ip = item.get("public_ipaddr") or item.get("publicIp")
    ports = item.get("ports") or []
    if isinstance(ports, dict):
        mappings = ports.get("8080/tcp") or ports.get("8080") or []
        if isinstance(mappings, dict):
            mappings = [mappings]
        for mapping in mappings:
            if isinstance(mapping, dict):
                mapped_host = mapping.get("HostIp") or mapping.get("ip")
                host = public_ip or _usable_mapped_host(mapped_host)
                port = mapping.get("HostPort") or mapping.get("publicPort")
                if host and port:
                    return f"http://{host}:{port}/healthz"
    if isinstance(ports, list):
        for mapping in ports:
            if isinstance(mapping, dict):
                private_port = (
                    mapping.get("PrivatePort")
                    or mapping.get("privatePort")
                    or mapping.get("container_port")
                )
                if str(private_port) != "8080":
                    continue
                mapped_host = mapping.get("IP") or mapping.get("ip")
                host = public_ip or _usable_mapped_host(mapped_host)
                port = (
                    mapping.get("PublicPort")
                    or mapping.get("publicPort")
                    or mapping.get("host_port")
                )
                if host and port:
                    return f"http://{host}:{port}/healthz"
        if public_ip and any(str(port).split("/")[0] == "8080" for port in ports):
            return f"http://{public_ip}:8080/healthz"
    return None


def _usable_mapped_host(value: Any) -> str | None:
    host = str(value or "").strip()
    if host in {"", "0.0.0.0", "::", "[::]"}:
        return None
    return host


def _runtime_seconds(item: dict[str, Any], now: float) -> float | None:
    uptime_mins = _float_or_none(item.get("uptime_mins"))
    if uptime_mins is not None:
        return max(0.0, uptime_mins * 60.0)
    client_run_time = _float_or_none(item.get("client_run_time"))
    if client_run_time is not None:
        return max(0.0, client_run_time)
    start_date = _float_or_none(item.get("start_date"))
    if start_date is not None:
        return max(0.0, now - start_date)
    return None


def _charge_resource_id(item: dict[str, Any]) -> str | None:
    metadata = item.get("metadata") or {}
    for key in ("instance_id", "contract_id"):
        if metadata.get(key) is not None:
            return str(metadata[key])
    source = str(item.get("source") or "")
    if source.startswith("instance-"):
        return source.removeprefix("instance-")
    return source or None


def _charge_duration_ms(item: dict[str, Any]) -> int | None:
    start = _float_or_none(item.get("start"))
    end = _float_or_none(item.get("end"))
    if start is not None and end is not None and end > start:
        return round((end - start) * 1000)

    gpu_hours = 0.0
    for detail in item.get("items") or []:
        if not isinstance(detail, dict) or detail.get("type") != "gpu":
            continue
        match = re.search(
            r"([0-9]+(?:\.[0-9]+)?)\s+hours?\b",
            str(detail.get("description") or ""),
            flags=re.IGNORECASE,
        )
        if match:
            gpu_hours += float(match.group(1))
    return round(gpu_hours * 3_600_000) if gpu_hours > 0 else None


def _epoch_seconds(value: str) -> int:
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return int(parsed.timestamp())


def _iso_from_epoch(value: float) -> str:
    return dt.datetime.fromtimestamp(value, tz=dt.timezone.utc).isoformat()


def _is_missing_payload(payload: Any) -> bool:
    if not isinstance(payload, dict):
        return False
    text = json.dumps(payload, ensure_ascii=False, default=str).lower()
    return payload.get("error") == "not_found" or "instance not found" in text


def _message(payload: Any) -> str:
    if isinstance(payload, dict):
        message = payload.get("msg") or payload.get("message") or payload.get("detail")
        if message:
            return str(message)[:1000]
    return json.dumps(payload, ensure_ascii=False, default=str)[:1000]


def _mib_to_gib(value: Any) -> float | None:
    amount = _float_or_none(value)
    return amount / 1024.0 if amount is not None else None


def _float_or_none(value: Any) -> float | None:
    if value is None or value == "":
        return None
    try:
        return float(value)
    except (TypeError, ValueError):
        return None


def _int_or_none(value: Any) -> int | None:
    amount = _float_or_none(value)
    return int(amount) if amount is not None else None
