from __future__ import annotations

import concurrent.futures
import dataclasses
import json
import os
import threading
import time
import urllib.parse
import uuid
from typing import Any, Callable, Sequence

from ..model import CatalogQuote, FailureClass, TrialCell
from .common import (
    CatalogFetch,
    CreateResult,
    DeleteResult,
    HeartbeatProbe,
    HTTPResponse,
    ProviderBillingRecord,
    ResourceObservation,
    Transport,
    classify_failure,
    probe_heartbeat,
    provider_api_call,
    request_json,
)


RUNPOD_REST_URL = "https://api.runpod.io/v2"
RUNPOD_GRAPHQL_URL = "https://api.runpod.io/graphql"


def research_health_command() -> str:
    return (
        "mkdir -p /tmp/aibrix-neocloud-health && "
        "printf 'aibrix-neocloud-health-v1\\n' > /tmp/aibrix-neocloud-health/healthz && "
        "exec python3 -m http.server 8080 --bind 0.0.0.0 "
        "--directory /tmp/aibrix-neocloud-health"
    )


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
        if self.rest_url.endswith("/v1"):
            raise ValueError("RunPod REST v2 base URL is required; remove the old /v1 override")
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
            payload = response.payload or {}
            if not isinstance(payload, dict) or not isinstance(payload.get("records"), list):
                raise RuntimeError("RunPod billing history response has no records list")
            payload = payload["records"]
            for item in payload:
                pod_id = item.get("podId") or resource_id
                period_start = str(item.get("startTime") or "")
                amount = _float_or_none(item.get("totalAmount"))
                if not period_start or amount is None:
                    continue
                identity = json.dumps(
                    {
                        "pod_id": pod_id,
                        "start_time": period_start,
                        "end_time": item.get("endTime"),
                    },
                    sort_keys=True,
                )
                records.append(
                    ProviderBillingRecord(
                        provider=self.provider,
                        provider_resource_id=str(pod_id) if pod_id else None,
                        period_start=period_start,
                        amount_usd=amount,
                        billed_duration_ms=None,
                        source="runpod_billing_api",
                        source_record_id=str(
                            uuid.uuid5(uuid.NAMESPACE_URL, identity)
                        ),
                        gpu_type=None,
                        disk_space_billed_gb=None,
                        raw=item,
                        api_call=call,
                    )
                )
        return records

    def create_replicas(self, cell: TrialCell, requests: Sequence[Any]) -> list[CreateResult]:
        if not requests:
            return []
        if cell.market != "on-demand":
            return [
                CreateResult(
                    False,
                    failure_class=FailureClass.UNSUPPORTED_SPEC,
                    message="RunPod REST v2 supports only on-demand Pods",
                )
                for _ in requests
            ]
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
            "cloud": cell.resource_class.upper(),
            "image": cell.image or "python:3.12-slim",
            "gpu": {"id": cell.sku, "count": cell.gpu_count},
            "disk": 20,
            "ports": ["8080/http"],
            "entrypoint": ["/bin/sh", "-lc"],
            "cmd": [research_health_command()],
        }
        if cell.datacenter:
            body["dataCenterIds"] = [cell.datacenter]
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
        return CreateResult(
            True,
            provider_resource_id=str(pod_id),
            price_usd_hr=_float_or_none(pod.get("cost")),
            location=pod.get("dataCenterId"),
            request_payload=body,
            raw=pod,
            api_call=call,
        )

    def observe(self, resource_id: str, cell: TrialCell) -> ResourceObservation:
        url = self.rest_url + "/pods/" + urllib.parse.quote(resource_id, safe="")
        response = self._request("GET", url)
        call = provider_api_call("get_pod", response)
        if response.status == 404:
            return ResourceObservation(False, None, False, raw=response.body, api_call=call)
        if not response.ok:
            return ResourceObservation(True, "api-error", False, raw=response.body, api_call=call)
        item = response.payload or {}
        state = item.get("status")
        provider_ready = state == "RUNNING"
        heartbeat_ok = False
        heartbeat_latency = None
        heartbeat_version = None
        if provider_ready:
            endpoint = _runpod_health_endpoint(item)
            if endpoint:
                heartbeat_ok, heartbeat_latency, heartbeat_version = self.heartbeat_probe(endpoint, 5)
        runtime = item.get("runtime") or {}
        return ResourceObservation(
            exists=state != "TERMINATED",
            provider_state=state,
            provider_ready=provider_ready,
            application_ready=heartbeat_ok,
            application_latency_ms=heartbeat_latency,
            heartbeat_version=heartbeat_version,
            location=item.get("dataCenterId") or cell.datacenter,
            machine_id=None,
            runtime_uptime_seconds=_float_or_none(runtime.get("uptime")),
            price_usd_hr=_float_or_none(item.get("cost")),
            raw=item,
            api_call=call,
        )

    def lookup_replica(self, cell: TrialCell, name: str) -> CreateResult:
        del cell
        cursor = None
        seen_cursors: set[str] = set()
        matches: list[dict[str, Any]] = []
        total_latency = 0
        last_call = None
        while True:
            url = self.rest_url + "/pods"
            if cursor:
                url += "?cursor=" + urllib.parse.quote(cursor, safe="")
            response = self._request("GET", url)
            total_latency += response.latency_ms
            last_call = provider_api_call("list_pods_for_create", response)
            if not response.ok:
                return CreateResult(
                    False,
                    failure_class=last_call.failure_class or FailureClass.UNKNOWN,
                    message="RunPod list request failed",
                    api_call=last_call,
                )
            page = response.payload or {}
            if not isinstance(page, dict) or not isinstance(page.get("pods"), list):
                return CreateResult(False, failure_class=FailureClass.UNKNOWN,
                                    message="RunPod list response has no pods list")
            matches.extend(
                pod for pod in page["pods"]
                if pod.get("name") == name and pod.get("status") != "TERMINATED"
            )
            pagination = page.get("pagination") or {}
            if not pagination.get("hasNextPage"):
                break
            cursor = pagination.get("nextCursor")
            if not cursor or cursor in seen_cursors:
                return CreateResult(False, failure_class=FailureClass.UNKNOWN,
                                    message="RunPod list pagination cursor is missing or repeated")
            seen_cursors.add(cursor)
        # List responses include all account Pods and their env; persist only match metadata.
        call = dataclasses.replace(last_call, latency_ms=total_latency,
                                   raw={"requested_name": name, "match_count": len(matches)})
        if len(matches) != 1:
            return CreateResult(
                False,
                failure_class=(FailureClass.REMOTE_NOT_FOUND if not matches else FailureClass.UNKNOWN),
                message=("no Pod matched the request name" if not matches else
                         "multiple Pods matched the request name"),
                api_call=call,
            )
        pod = matches[0]
        return CreateResult(
            True,
            provider_resource_id=str(pod["id"]),
            price_usd_hr=_float_or_none(pod.get("cost")),
            location=pod.get("dataCenterId"),
            raw={"id": pod["id"], "name": name},
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
    runtime = item.get("runtime") or {}
    for port_item in runtime.get("ports") or []:
        if port_item.get("private") == 8080 and port_item.get("ip") and port_item.get("public"):
            return f"http://{port_item['ip']}:{port_item['public']}/healthz"
    pod_id = item.get("id")
    exposed_ports = item.get("ports") or []
    if pod_id and any(str(value).lower() == "8080/http" for value in exposed_ports):
        return f"https://{pod_id}-8080.proxy.runpod.net/healthz"
    return None
