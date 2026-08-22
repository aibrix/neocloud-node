#!/usr/bin/env python3
"""NeoCloud GPU trace collector.

This collector is intentionally dependency-free. It supports:

- passive-once: collect provider catalog/API signals only.
- probe-once: create one short-lived GPU lease and delete it.
- pilot: run passive collection periodically, with optional active probes.

Credentials are read from environment variables only. They are never written to
the database or logs.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import sqlite3
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any


DEFAULT_USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126 Safari/537.36 "
    "aibrix-neocloud-trace/0.1"
)

LAMBDA_BASE_URL = "https://cloud.lambda.ai/api/v1"
RUNPOD_REST_URL = "https://rest.runpod.io/v1"
RUNPOD_GRAPHQL_URL = "https://api.runpod.io/graphql"

HIGH_END_MARKERS = ("H100", "H200", "B200", "B300", "GH200", "MI300")


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


SENSITIVE_KEY_MARKERS = (
    "token",
    "secret",
    "authorization",
    "api_key",
    "apikey",
    "password",
    "jupyter_url",
)


def redact(value: Any) -> Any:
    if isinstance(value, dict):
        out = {}
        for key, item in value.items():
            lowered = str(key).lower()
            if any(marker in lowered for marker in SENSITIVE_KEY_MARKERS):
                out[key] = "[REDACTED]"
            else:
                out[key] = redact(item)
        return out
    if isinstance(value, list):
        return [redact(item) for item in value]
    return value


def compact_json(value: Any) -> str:
    return json.dumps(redact(value), sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def safe_label(value: str) -> str:
    label = re.sub(r"[^a-zA-Z0-9-]+", "-", value.strip().lower()).strip("-")
    return label[:32] or "probe"


def raw_hash(value: Any) -> str:
    return hashlib.sha256(compact_json(value).encode("utf-8")).hexdigest()


def classify_error(status: Any, payload: Any) -> str:
    text = json.dumps(payload, ensure_ascii=False).lower()
    if status in (401, 403) or "unauthorized" in text or "forbidden" in text or "auth" in text:
        if "cloudflare" in text or "access denied" in text:
            return "waf"
        return "auth"
    if status == 429 or "rate" in text:
        return "rate_limited"
    if "quota" in text or "limit" in text:
        return "quota"
    if "capacity" in text or "available" in text or "out of stock" in text:
        return "capacity"
    if "timeout" in text or status == "timeout":
        return "timeout"
    if isinstance(status, int) and 500 <= status <= 599:
        return "transient"
    return "unknown"


class HTTPResult:
    def __init__(
        self,
        status: Any,
        latency_ms: int,
        payload: Any = None,
        error: Any = None,
        content_type: str = "",
    ) -> None:
        self.status = status
        self.latency_ms = latency_ms
        self.payload = payload
        self.error = error
        self.content_type = content_type

    @property
    def ok(self) -> bool:
        return isinstance(self.status, int) and 200 <= self.status <= 299

    def body(self) -> Any:
        return self.payload if self.ok else self.error


def request_json(
    method: str,
    url: str,
    headers: dict[str, str] | None = None,
    body: Any = None,
    timeout: int = 30,
) -> HTTPResult:
    req_headers = dict(headers or {})
    req_headers.setdefault("Accept", "application/json")
    req_headers.setdefault("User-Agent", DEFAULT_USER_AGENT)
    data = None
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        req_headers.setdefault("Content-Type", "application/json")
    req = urllib.request.Request(url, data=data, headers=req_headers, method=method)
    started = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read(4 * 1024 * 1024)
            latency_ms = round((time.time() - started) * 1000)
            payload = decode_body(raw)
            return HTTPResult(resp.status, latency_ms, payload=payload, content_type=resp.headers.get("content-type", ""))
    except urllib.error.HTTPError as exc:
        raw = exc.read(4 * 1024 * 1024)
        latency_ms = round((time.time() - started) * 1000)
        return HTTPResult(exc.code, latency_ms, error=decode_body(raw), content_type=exc.headers.get("content-type", ""))
    except TimeoutError as exc:
        return HTTPResult("timeout", round((time.time() - started) * 1000), error=str(exc))
    except Exception as exc:  # noqa: BLE001 - collector must record provider/runtime failures
        return HTTPResult("exception", round((time.time() - started) * 1000), error=f"{type(exc).__name__}: {exc}")


def decode_body(raw: bytes) -> Any:
    if not raw:
        return None
    try:
        return json.loads(raw.decode("utf-8"))
    except Exception:
        return {"_raw": raw[:2000].decode("utf-8", errors="replace")}


class Store:
    def __init__(self, path: str) -> None:
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        self.conn = sqlite3.connect(path)
        self.conn.execute("PRAGMA journal_mode=WAL")
        self.init()

    def init(self) -> None:
        self.conn.executescript(
            """
            CREATE TABLE IF NOT EXISTS provider_api_observations (
                id TEXT PRIMARY KEY,
                observed_at TEXT NOT NULL,
                provider TEXT NOT NULL,
                operation TEXT NOT NULL,
                status TEXT NOT NULL,
                latency_ms INTEGER,
                error_class TEXT,
                error_message TEXT,
                raw_hash TEXT,
                raw_json TEXT
            );

            CREATE TABLE IF NOT EXISTS offer_observations (
                id TEXT PRIMARY KEY,
                observed_at TEXT NOT NULL,
                provider TEXT NOT NULL,
                source TEXT NOT NULL,
                sku TEXT,
                gpu_type TEXT,
                gpu_count INTEGER,
                gpu_memory_gb REAL,
                vcpus INTEGER,
                memory_gib REAL,
                storage_gib REAL,
                price_usd_hr REAL,
                spot_price_usd_hr REAL,
                region TEXT,
                datacenter TEXT,
                resource_class TEXT,
                available INTEGER,
                raw_json TEXT
            );

            CREATE TABLE IF NOT EXISTS provision_attempts (
                id TEXT PRIMARY KEY,
                started_at TEXT NOT NULL,
                finished_at TEXT,
                provider TEXT NOT NULL,
                name TEXT NOT NULL,
                sku TEXT,
                gpu_type TEXT,
                region TEXT,
                datacenter TEXT,
                resource_class TEXT,
                status TEXT NOT NULL,
                failure_class TEXT,
                create_latency_ms INTEGER,
                ready_latency_ms INTEGER,
                delete_latency_ms INTEGER,
                realized_price_usd_hr REAL,
                estimated_cost_usd REAL,
                external_id TEXT,
                details_json TEXT
            );

            CREATE TABLE IF NOT EXISTS lease_events (
                id TEXT PRIMARY KEY,
                observed_at TEXT NOT NULL,
                provider TEXT NOT NULL,
                attempt_id TEXT,
                external_id TEXT,
                event_type TEXT NOT NULL,
                state TEXT,
                message TEXT,
                raw_json TEXT
            );

            CREATE TABLE IF NOT EXISTS budget_ledger (
                id TEXT PRIMARY KEY,
                observed_at TEXT NOT NULL,
                provider TEXT NOT NULL,
                amount_usd REAL NOT NULL,
                kind TEXT NOT NULL,
                reference TEXT,
                details_json TEXT
            );
            """
        )
        self.conn.commit()

    def api_observation(self, provider: str, operation: str, result: HTTPResult) -> None:
        body = result.body()
        self.conn.execute(
            """
            INSERT INTO provider_api_observations
            (id, observed_at, provider, operation, status, latency_ms, error_class, error_message, raw_hash, raw_json)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                str(uuid.uuid4()),
                utc_now(),
                provider,
                operation,
                str(result.status),
                result.latency_ms,
                None if result.ok else classify_error(result.status, body),
                None if result.ok else compact_json(body)[:2000],
                raw_hash(body),
                compact_json(body),
            ),
        )
        self.conn.commit()

    def offer(self, provider: str, source: str, **kwargs: Any) -> None:
        self.conn.execute(
            """
            INSERT INTO offer_observations
            (id, observed_at, provider, source, sku, gpu_type, gpu_count, gpu_memory_gb, vcpus,
             memory_gib, storage_gib, price_usd_hr, spot_price_usd_hr, region, datacenter,
             resource_class, available, raw_json)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                str(uuid.uuid4()),
                utc_now(),
                provider,
                source,
                kwargs.get("sku"),
                kwargs.get("gpu_type"),
                kwargs.get("gpu_count"),
                kwargs.get("gpu_memory_gb"),
                kwargs.get("vcpus"),
                kwargs.get("memory_gib"),
                kwargs.get("storage_gib"),
                kwargs.get("price_usd_hr"),
                kwargs.get("spot_price_usd_hr"),
                kwargs.get("region"),
                kwargs.get("datacenter"),
                kwargs.get("resource_class"),
                1 if kwargs.get("available") else 0,
                compact_json(kwargs.get("raw", {})),
            ),
        )
        self.conn.commit()

    def attempt(self, **kwargs: Any) -> None:
        self.conn.execute(
            """
            INSERT INTO provision_attempts
            (id, started_at, finished_at, provider, name, sku, gpu_type, region, datacenter,
             resource_class, status, failure_class, create_latency_ms, ready_latency_ms,
             delete_latency_ms, realized_price_usd_hr, estimated_cost_usd, external_id, details_json)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                kwargs["id"],
                kwargs["started_at"],
                kwargs.get("finished_at"),
                kwargs["provider"],
                kwargs["name"],
                kwargs.get("sku"),
                kwargs.get("gpu_type"),
                kwargs.get("region"),
                kwargs.get("datacenter"),
                kwargs.get("resource_class"),
                kwargs["status"],
                kwargs.get("failure_class"),
                kwargs.get("create_latency_ms"),
                kwargs.get("ready_latency_ms"),
                kwargs.get("delete_latency_ms"),
                kwargs.get("realized_price_usd_hr"),
                kwargs.get("estimated_cost_usd"),
                kwargs.get("external_id"),
                compact_json(kwargs.get("details", {})),
            ),
        )
        self.conn.commit()

    def lease_event(self, provider: str, event_type: str, **kwargs: Any) -> None:
        self.conn.execute(
            """
            INSERT INTO lease_events
            (id, observed_at, provider, attempt_id, external_id, event_type, state, message, raw_json)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                str(uuid.uuid4()),
                utc_now(),
                provider,
                kwargs.get("attempt_id"),
                kwargs.get("external_id"),
                event_type,
                kwargs.get("state"),
                kwargs.get("message"),
                compact_json(kwargs.get("raw", {})),
            ),
        )
        self.conn.commit()

    def budget(self, provider: str, amount_usd: float, kind: str, reference: str, details: Any = None) -> None:
        self.conn.execute(
            """
            INSERT INTO budget_ledger
            (id, observed_at, provider, amount_usd, kind, reference, details_json)
            VALUES (?, ?, ?, ?, ?, ?, ?)
            """,
            (str(uuid.uuid4()), utc_now(), provider, amount_usd, kind, reference, compact_json(details or {})),
        )
        self.conn.commit()

    def provider_spend(self, provider: str) -> float:
        row = self.conn.execute("SELECT COALESCE(SUM(amount_usd), 0) FROM budget_ledger WHERE provider = ?", (provider,)).fetchone()
        return float(row[0] or 0)


class LambdaClient:
    def __init__(self) -> None:
        self.key = os.environ.get("LAMBDA_CLOUD_API_KEY", "")
        self.base = os.environ.get("LAMBDA_CLOUD_BASE_URL", LAMBDA_BASE_URL).rstrip("/")
        self.ssh_keys = [x.strip() for x in os.environ.get("LAMBDA_CLOUD_SSH_KEYS", "").split(",") if x.strip()]

    def headers(self) -> dict[str, str]:
        if not self.key:
            raise RuntimeError("LAMBDA_CLOUD_API_KEY is required")
        return {"Authorization": f"Bearer {self.key}"}

    def instance_types(self) -> HTTPResult:
        return request_json("GET", self.base + "/instance-types", self.headers())

    def instances(self) -> HTTPResult:
        return request_json("GET", self.base + "/instances", self.headers())

    def launch(self, region: str, instance_type: str, name: str) -> HTTPResult:
        if not self.ssh_keys:
            raise RuntimeError("LAMBDA_CLOUD_SSH_KEYS is required for active probes")
        return request_json(
            "POST",
            self.base + "/instance-operations/launch",
            self.headers(),
            {
                "region_name": region,
                "instance_type_name": instance_type,
                "ssh_key_names": self.ssh_keys,
                "name": name,
            },
        )

    def get_instance(self, instance_id: str) -> HTTPResult:
        return request_json("GET", self.base + "/instances/" + instance_id, self.headers())

    def terminate(self, instance_id: str) -> HTTPResult:
        return request_json(
            "POST",
            self.base + "/instance-operations/terminate",
            self.headers(),
            {"instance_ids": [instance_id]},
        )

    def find_by_name(self, name: str) -> HTTPResult:
        result = self.instances()
        if not result.ok:
            return result
        data = (result.payload or {}).get("data") or []
        matches = [item for item in data if item.get("name") == name]
        result.payload = {"data": matches}
        return result


class RunPodClient:
    def __init__(self) -> None:
        self.key = os.environ.get("RUNPOD_API_KEY", "")
        self.rest = os.environ.get("RUNPOD_BASE_URL", RUNPOD_REST_URL).rstrip("/")
        self.graphql = os.environ.get("RUNPOD_GRAPHQL_URL", RUNPOD_GRAPHQL_URL).rstrip("/")

    def headers(self) -> dict[str, str]:
        if not self.key:
            raise RuntimeError("RUNPOD_API_KEY is required")
        return {"Authorization": f"Bearer {self.key}"}

    def gql(self, query: str) -> HTTPResult:
        return request_json("POST", self.graphql, self.headers(), {"query": query})

    def gpu_types(self) -> HTTPResult:
        return self.gql(
            """
            query {
              gpuTypes {
                id
                displayName
                memoryInGb
                secureCloud
                communityCloud
                lowestPrice(input: {gpuCount: 1}) {
                  minimumBidPrice
                  uninterruptablePrice
                }
              }
            }
            """
        )

    def datacenters(self) -> HTTPResult:
        return self.gql("query { dataCenters { id name location } }")

    def pods(self, name: str | None = None, include_machine: bool = False) -> HTTPResult:
        url = self.rest + "/pods"
        params = {}
        if name:
            params["name"] = name
        if include_machine:
            params["includeMachine"] = "true"
        if params:
            url += "?" + urllib.parse.urlencode(params)
        return request_json("GET", url, self.headers())

    def templates(self) -> HTTPResult:
        return request_json("GET", self.rest + "/templates", self.headers())

    def create_pod(self, body: dict[str, Any]) -> HTTPResult:
        return request_json("POST", self.rest + "/pods", self.headers(), body)

    def delete_pod(self, pod_id: str) -> HTTPResult:
        return request_json("DELETE", self.rest + "/pods/" + pod_id, self.headers())


def collect_lambda_catalog(store: Store) -> dict[str, Any]:
    client = LambdaClient()
    res = client.instance_types()
    store.api_observation("lambda", "instance-types", res)
    if not res.ok:
        return {"ok": False, "status": res.status}
    data = (res.payload or {}).get("data") or {}
    offers = 0
    for name, entry in data.items():
        it = entry.get("instance_type") or entry
        specs = it.get("specs") or {}
        if int(specs.get("gpus") or 0) <= 0:
            continue
        regions = entry.get("regions_with_capacity_available") or []
        region_names = [r.get("name") for r in regions if isinstance(r, dict) and r.get("name")]
        base = {
            "sku": name,
            "gpu_type": specs.get("gpu_description") or it.get("gpu_description"),
            "gpu_count": specs.get("gpus"),
            "vcpus": specs.get("vcpus"),
            "memory_gib": specs.get("memory_gib"),
            "storage_gib": specs.get("storage_gib"),
            "price_usd_hr": float(it.get("price_cents_per_hour") or 0) / 100.0,
            "resource_class": "on-demand",
            "raw": entry,
        }
        if region_names:
            for region in region_names:
                store.offer("lambda", "instance-types", **base, region=region, available=True)
                offers += 1
        else:
            store.offer("lambda", "instance-types", **base, region=None, available=False)
            offers += 1
    return {"ok": True, "offers": offers}


def collect_runpod_catalog(store: Store) -> dict[str, Any]:
    client = RunPodClient()
    gpu_res = client.gpu_types()
    store.api_observation("runpod", "graphql.gpuTypes", gpu_res)
    offers = 0
    if gpu_res.ok:
        data = ((gpu_res.payload or {}).get("data") or {}).get("gpuTypes") or []
        for item in data:
            price = item.get("lowestPrice") or {}
            common = {
                "sku": item.get("id"),
                "gpu_type": item.get("displayName") or item.get("id"),
                "gpu_count": 1,
                "gpu_memory_gb": item.get("memoryInGb"),
                "price_usd_hr": price.get("uninterruptablePrice"),
                "spot_price_usd_hr": price.get("minimumBidPrice"),
                "raw": item,
            }
            if item.get("secureCloud"):
                store.offer("runpod", "graphql.gpuTypes", **common, resource_class="secure", available=price.get("uninterruptablePrice") is not None)
                offers += 1
            if item.get("communityCloud"):
                store.offer("runpod", "graphql.gpuTypes", **common, resource_class="community", available=price.get("uninterruptablePrice") is not None)
                offers += 1
    dc_res = client.datacenters()
    store.api_observation("runpod", "graphql.dataCenters", dc_res)
    pods_res = client.pods(include_machine=True)
    store.api_observation("runpod", "rest.pods", pods_res)
    tmpl_res = client.templates()
    store.api_observation("runpod", "rest.templates", tmpl_res)
    return {"ok": gpu_res.ok, "offers": offers, "datacenters_status": dc_res.status}


def collect_passive_once(store: Store, providers: list[str]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    if "lambda" in providers:
        result["lambda"] = collect_lambda_catalog(store)
    if "runpod" in providers:
        result["runpod"] = collect_runpod_catalog(store)
    return result


def find_lambda_price(store: Store, instance_type: str, region: str | None) -> float | None:
    row = store.conn.execute(
        """
        SELECT price_usd_hr FROM offer_observations
        WHERE provider='lambda' AND sku=? AND (region=? OR ? IS NULL)
        ORDER BY observed_at DESC LIMIT 1
        """,
        (instance_type, region, region),
    ).fetchone()
    return float(row[0]) if row and row[0] is not None else None


def find_runpod_price(store: Store, gpu_type: str, resource_class: str) -> float | None:
    row = store.conn.execute(
        """
        SELECT price_usd_hr FROM offer_observations
        WHERE provider='runpod' AND sku=? AND resource_class=?
        ORDER BY observed_at DESC LIMIT 1
        """,
        (gpu_type, resource_class.lower()),
    ).fetchone()
    return float(row[0]) if row and row[0] is not None else None


def require_budget(store: Store, provider: str, estimate: float, provider_budget: float) -> None:
    spent = store.provider_spend(provider)
    if spent + estimate > provider_budget:
        raise RuntimeError(f"{provider} budget would be exceeded: spent={spent:.4f}, estimate={estimate:.4f}, budget={provider_budget:.4f}")


def assert_high_end_allowed(name: str, allow_high_end: bool) -> None:
    if allow_high_end:
        return
    upper = name.upper()
    if any(marker in upper for marker in HIGH_END_MARKERS):
        raise RuntimeError(f"{name} looks high-end; pass --allow-high-end to probe it")


def probe_lambda_once(store: Store, args: argparse.Namespace) -> dict[str, Any]:
    client = LambdaClient()
    assert_high_end_allowed(args.lambda_instance_type, args.allow_high_end)
    if args.refresh_catalog:
        collect_lambda_catalog(store)
    hourly = find_lambda_price(store, args.lambda_instance_type, args.lambda_region) or args.default_hourly_price
    estimate = hourly * args.max_lease_seconds / 3600.0
    require_budget(store, "lambda", estimate, args.provider_budget_usd)

    attempt_id = str(uuid.uuid4())
    name = f"{args.name_prefix}-{dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d%H%M%S')}"
    started_at = utc_now()
    external_id = None
    create_latency = None
    ready_latency = None
    delete_latency = None
    cleanup_latency = None
    states = []
    status = "failed"
    failure_class = None
    t0 = time.time()
    try:
        create = client.launch(args.lambda_region, args.lambda_instance_type, name)
        store.api_observation("lambda", "launch", create)
        create_latency = create.latency_ms
        if not create.ok:
            failure_class = classify_error(create.status, create.body())
            raise RuntimeError(compact_json(create.body())[:1000])
        ids = ((create.payload or {}).get("data") or {}).get("instance_ids") or []
        external_id = ids[0] if ids else None
        if not external_id:
            failure_class = "unknown"
            raise RuntimeError("lambda launch returned no instance id")
        store.lease_event("lambda", "created", attempt_id=attempt_id, external_id=external_id, raw=create.payload)

        deadline = time.time() + args.max_lease_seconds
        while time.time() < deadline:
            time.sleep(args.poll_seconds)
            get = client.get_instance(external_id)
            store.api_observation("lambda", "get-instance", get)
            if not get.ok:
                states.append({"t_sec": round(time.time() - t0), "error": get.body()})
                continue
            data = (get.payload or {}).get("data") or {}
            state = {
                "t_sec": round(time.time() - t0),
                "status": data.get("status"),
                "ip_present": bool(data.get("ip")),
                "private_ip_present": bool(data.get("private_ip")),
            }
            states.append(state)
            store.lease_event("lambda", "poll", attempt_id=attempt_id, external_id=external_id, state=data.get("status"), raw=data)
            if data.get("status") in ("active", "unhealthy", "terminated"):
                ready_latency = round((time.time() - t0) * 1000)
                break
        status = "ready" if states and states[-1].get("status") == "active" else "timeout"
        if status == "timeout":
            failure_class = "timeout"
    except Exception as exc:  # noqa: BLE001
        if failure_class is None:
            failure_class = classify_error("exception", str(exc))
        states.append({"exception": str(exc)})
    finally:
        if external_id:
            term = client.terminate(external_id)
            store.api_observation("lambda", "terminate", term)
            delete_latency = term.latency_ms
            store.lease_event("lambda", "terminate-requested", attempt_id=attempt_id, external_id=external_id, state=str(term.status), raw=term.body())
            cleanup_started = time.time()
            cleanup_deadline = cleanup_started + args.cleanup_timeout_seconds
            while time.time() < cleanup_deadline:
                time.sleep(args.cleanup_poll_seconds)
                found = client.find_by_name(name)
                store.api_observation("lambda", "cleanup-list-by-name", found)
                matches = ((found.payload or {}).get("data") or []) if found.ok else []
                if not matches or all(item.get("status") == "terminated" for item in matches):
                    cleanup_latency = round((time.time() - cleanup_started) * 1000)
                    store.lease_event("lambda", "cleanup-terminal", attempt_id=attempt_id, external_id=external_id, state="terminal-or-missing", raw={"matches": matches})
                    break
        elapsed = max(time.time() - t0, 1)
        estimated_cost = hourly * elapsed / 3600.0
        store.budget("lambda", estimated_cost, "estimated-active-probe", attempt_id, {"hourly": hourly, "elapsed_seconds": elapsed, "name": name})
        details = {"states": states, "cleanup_latency_ms": cleanup_latency}
        if getattr(args, "probe_plan_entry", None):
            details["probe_plan_entry"] = args.probe_plan_entry
        store.attempt(
            id=attempt_id,
            started_at=started_at,
            finished_at=utc_now(),
            provider="lambda",
            name=name,
            sku=args.lambda_instance_type,
            gpu_type=args.lambda_instance_type,
            region=args.lambda_region,
            resource_class="on-demand",
            status=status,
            failure_class=failure_class,
            create_latency_ms=create_latency,
            ready_latency_ms=ready_latency,
            delete_latency_ms=delete_latency,
            details=details,
            realized_price_usd_hr=hourly,
            estimated_cost_usd=estimated_cost,
            external_id=external_id,
        )
    return {"provider": "lambda", "attempt_id": attempt_id, "name": name, "status": status, "states": states, "estimated_cost_usd": estimated_cost}


def probe_runpod_once(store: Store, args: argparse.Namespace) -> dict[str, Any]:
    client = RunPodClient()
    assert_high_end_allowed(args.runpod_gpu_type, args.allow_high_end)
    if args.refresh_catalog:
        collect_runpod_catalog(store)
    class_name = args.runpod_cloud_type.lower()
    hourly = find_runpod_price(store, args.runpod_gpu_type, class_name) or args.default_hourly_price
    estimate = hourly * args.max_lease_seconds / 3600.0
    require_budget(store, "runpod", estimate, args.provider_budget_usd)

    attempt_id = str(uuid.uuid4())
    name = f"{args.name_prefix}-{dt.datetime.now(dt.timezone.utc).strftime('%Y%m%d%H%M%S')}"
    started_at = utc_now()
    pod_id = None
    create_latency = None
    ready_latency = None
    delete_latency = None
    cleanup_latency = None
    realized_price = None
    datacenter = None
    states = []
    status = "failed"
    failure_class = None
    t0 = time.time()
    try:
        body = {
            "name": name,
            "computeType": "GPU",
            "cloudType": args.runpod_cloud_type.upper(),
            "imageName": args.runpod_image,
            "gpuCount": 1,
            "gpuTypeIds": [args.runpod_gpu_type],
            "gpuTypePriority": "availability",
            "containerDiskInGb": args.runpod_container_disk_gb,
            "volumeInGb": 0,
            "supportPublicIp": args.runpod_public_ip,
            "dockerEntrypoint": ["/bin/bash", "-lc"],
            "dockerStartCmd": [f"sleep {max(args.max_lease_seconds + 600, 900)}"],
        }
        create = client.create_pod(body)
        store.api_observation("runpod", "create-pod", create)
        create_latency = create.latency_ms
        if not create.ok:
            failure_class = classify_error(create.status, create.body())
            raise RuntimeError(compact_json(create.body())[:1000])
        pod = create.payload or {}
        pod_id = pod.get("id")
        realized_price = pod.get("costPerHr")
        if not pod_id:
            failure_class = "unknown"
            raise RuntimeError("runpod create returned no pod id")
        store.lease_event("runpod", "created", attempt_id=attempt_id, external_id=pod_id, raw=pod)

        deadline = time.time() + args.max_lease_seconds
        while time.time() < deadline:
            time.sleep(args.poll_seconds)
            listed = client.pods(name=name, include_machine=True)
            store.api_observation("runpod", "list-pods-by-name", listed)
            item = None
            if listed.ok and isinstance(listed.payload, list) and listed.payload:
                item = listed.payload[0]
            elif listed.ok and isinstance(listed.payload, dict):
                data = listed.payload.get("data")
                if isinstance(data, list) and data:
                    item = data[0]
            if not item:
                states.append({"t_sec": round(time.time() - t0), "missing": True})
                continue
            machine = item.get("machine") or {}
            datacenter = machine.get("dataCenterId") or item.get("dataCenterId") or datacenter
            realized_price = item.get("costPerHr") or realized_price
            state = {
                "t_sec": round(time.time() - t0),
                "desiredStatus": item.get("desiredStatus"),
                "machine_id_present": bool(item.get("machineId")),
                "datacenter": datacenter,
                "costPerHr": realized_price,
            }
            states.append(state)
            store.lease_event("runpod", "poll", attempt_id=attempt_id, external_id=pod_id, state=item.get("desiredStatus"), raw=item)
            if item.get("desiredStatus") in ("RUNNING", "EXITED", "TERMINATED") and item.get("machineId"):
                ready_latency = round((time.time() - t0) * 1000)
                break
        status = "ready" if states and states[-1].get("desiredStatus") == "RUNNING" else "timeout"
        if status == "timeout":
            failure_class = "timeout"
    except Exception as exc:  # noqa: BLE001
        if failure_class is None:
            failure_class = classify_error("exception", str(exc))
        states.append({"exception": str(exc)})
    finally:
        if pod_id:
            delete = client.delete_pod(pod_id)
            store.api_observation("runpod", "delete-pod", delete)
            delete_latency = delete.latency_ms
            store.lease_event("runpod", "delete-requested", attempt_id=attempt_id, external_id=pod_id, state=str(delete.status), raw=delete.body())
            cleanup_started = time.time()
            cleanup_deadline = cleanup_started + args.cleanup_timeout_seconds
            while time.time() < cleanup_deadline:
                time.sleep(args.cleanup_poll_seconds)
                listed = client.pods(name=name, include_machine=True)
                store.api_observation("runpod", "cleanup-list-by-name", listed)
                matches: list[Any] = []
                if listed.ok and isinstance(listed.payload, list):
                    matches = listed.payload
                elif listed.ok and isinstance(listed.payload, dict) and isinstance(listed.payload.get("data"), list):
                    matches = listed.payload["data"]
                if not matches:
                    cleanup_latency = round((time.time() - cleanup_started) * 1000)
                    store.lease_event("runpod", "cleanup-terminal", attempt_id=attempt_id, external_id=pod_id, state="missing", raw={"matches": []})
                    break
        elapsed = max(time.time() - t0, 1)
        bill_hourly = float(realized_price or hourly or args.default_hourly_price)
        estimated_cost = bill_hourly * elapsed / 3600.0
        store.budget("runpod", estimated_cost, "estimated-active-probe", attempt_id, {"hourly": bill_hourly, "elapsed_seconds": elapsed, "name": name})
        details = {"states": states, "cleanup_latency_ms": cleanup_latency}
        if getattr(args, "probe_plan_entry", None):
            details["probe_plan_entry"] = args.probe_plan_entry
        store.attempt(
            id=attempt_id,
            started_at=started_at,
            finished_at=utc_now(),
            provider="runpod",
            name=name,
            sku=args.runpod_gpu_type,
            gpu_type=args.runpod_gpu_type,
            datacenter=datacenter,
            resource_class=args.runpod_cloud_type.lower(),
            status=status,
            failure_class=failure_class,
            create_latency_ms=create_latency,
            ready_latency_ms=ready_latency,
            delete_latency_ms=delete_latency,
            details=details,
            realized_price_usd_hr=bill_hourly,
            estimated_cost_usd=estimated_cost,
            external_id=pod_id,
        )
    return {"provider": "runpod", "attempt_id": attempt_id, "name": name, "status": status, "states": states, "estimated_cost_usd": estimated_cost}


def run_probe_once(store: Store, args: argparse.Namespace) -> dict[str, Any]:
    if args.provider == "lambda":
        return probe_lambda_once(store, args)
    if args.provider == "runpod":
        return probe_runpod_once(store, args)
    raise RuntimeError(f"unsupported provider {args.provider}")


def load_active_plan(path: str) -> list[dict[str, Any]]:
    with open(path, "r", encoding="utf-8") as handle:
        text = handle.read().strip()
    if not text:
        raise RuntimeError(f"active plan is empty: {path}")
    if text[0] == "[":
        items = json.loads(text)
    else:
        items = [json.loads(line) for line in text.splitlines() if line.strip() and not line.lstrip().startswith("#")]
    if not isinstance(items, list) or not items:
        raise RuntimeError(f"active plan must contain a non-empty list: {path}")
    for i, item in enumerate(items):
        if not isinstance(item, dict):
            raise RuntimeError(f"active plan entry {i} is not an object")
        if item.get("provider") not in ("lambda", "runpod"):
            raise RuntimeError(f"active plan entry {i} has unsupported provider: {item.get('provider')}")
    return items


def probe_args_from_plan(base: argparse.Namespace, entry: dict[str, Any]) -> argparse.Namespace:
    probe_args = argparse.Namespace(**vars(base))
    probe_args.provider = entry["provider"]
    probe_args.probe_plan_entry = entry
    label = entry.get("label")
    if label:
        probe_args.name_prefix = f"{base.name_prefix}-{safe_label(str(label))}"
    if "allow_high_end" in entry:
        probe_args.allow_high_end = bool(entry["allow_high_end"])
    if entry["provider"] == "lambda":
        if "instance_type" in entry:
            probe_args.lambda_instance_type = entry["instance_type"]
        if "region" in entry:
            probe_args.lambda_region = entry["region"]
    if entry["provider"] == "runpod":
        if "gpu_type" in entry:
            probe_args.runpod_gpu_type = entry["gpu_type"]
        if "cloud_type" in entry:
            probe_args.runpod_cloud_type = str(entry["cloud_type"]).upper()
        if "image" in entry:
            probe_args.runpod_image = entry["image"]
        if "container_disk_gb" in entry:
            probe_args.runpod_container_disk_gb = int(entry["container_disk_gb"])
        if "public_ip" in entry:
            probe_args.runpod_public_ip = bool(entry["public_ip"])
    return probe_args


def run_pilot(store: Store, args: argparse.Namespace) -> None:
    end = time.time() + args.hours * 3600
    next_passive = 0.0
    next_active = 0.0
    active_index = 0
    providers = args.providers.split(",")
    active_plan = load_active_plan(args.active_plan) if getattr(args, "active_plan", None) else None
    while time.time() < end:
        now = time.time()
        if now >= next_passive:
            print(json.dumps({"event": "passive", "at": utc_now(), "result": collect_passive_once(store, providers)}, ensure_ascii=False))
            next_passive = now + args.passive_interval_seconds
        if args.enable_active and now >= next_active:
            if active_plan:
                entry = active_plan[active_index % len(active_plan)]
                active_index += 1
                probe_args = probe_args_from_plan(args, entry)
                print(json.dumps({"event": "active", "at": utc_now(), "plan_entry": entry, "result": run_probe_once(store, probe_args)}, ensure_ascii=False))
            else:
                sequence = [p.strip() for p in args.active_sequence.split(",") if p.strip()]
                if sequence:
                    provider = sequence[active_index % len(sequence)]
                    active_index += 1
                    probe_args = argparse.Namespace(**vars(args))
                    probe_args.provider = provider
                    probe_args.probe_plan_entry = None
                    print(json.dumps({"event": "active", "at": utc_now(), "result": run_probe_once(store, probe_args)}, ensure_ascii=False))
            next_active = now + args.active_interval_seconds
        time.sleep(min(30, max(1, min(next_passive, next_active if args.enable_active else next_passive) - time.time())))


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="NeoCloud GPU trace collector")
    parser.add_argument("--db", default="data/neocloud-trace/neocloud-trace.db")
    parser.add_argument("--providers", default="lambda,runpod")
    parser.add_argument("--name-prefix", default="aibrix-neocloud-probe")
    parser.add_argument("--provider-budget-usd", type=float, default=10.0)
    parser.add_argument("--default-hourly-price", type=float, default=10.0)
    parser.add_argument("--allow-high-end", action="store_true")
    parser.add_argument("--refresh-catalog", action=argparse.BooleanOptionalAction, default=True)
    parser.add_argument("--poll-seconds", type=int, default=20)
    parser.add_argument("--max-lease-seconds", type=int, default=600)
    parser.add_argument("--cleanup-poll-seconds", type=int, default=15)
    parser.add_argument("--cleanup-timeout-seconds", type=int, default=180)

    parser.add_argument("--lambda-instance-type", default="gpu_1x_a10")
    parser.add_argument("--lambda-region", default="us-east-1")

    parser.add_argument("--runpod-gpu-type", default="NVIDIA A40")
    parser.add_argument("--runpod-cloud-type", default="SECURE", choices=["SECURE", "COMMUNITY"])
    parser.add_argument("--runpod-image", default="ubuntu:22.04")
    parser.add_argument("--runpod-container-disk-gb", type=int, default=20)
    parser.add_argument("--runpod-public-ip", action=argparse.BooleanOptionalAction, default=False)

    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("passive-once", help="collect catalog/API signals once")
    probe = sub.add_parser("probe-once", help="run one active create/delete probe")
    probe.add_argument("--provider", choices=["lambda", "runpod"], required=True)
    pilot = sub.add_parser("pilot", help="run a bounded pilot loop")
    pilot.add_argument("--hours", type=float, default=24.0)
    pilot.add_argument("--passive-interval-seconds", type=int, default=300)
    pilot.add_argument("--active-interval-seconds", type=int, default=21600)
    pilot.add_argument("--enable-active", action="store_true")
    pilot.add_argument("--active-sequence", default="lambda,runpod")
    pilot.add_argument("--active-plan", help="JSON/JSONL probe plan. Overrides --active-sequence when set.")
    return parser


def main() -> int:
    args = build_parser().parse_args()
    store = Store(args.db)
    if args.command == "passive-once":
        print(json.dumps(collect_passive_once(store, [p.strip() for p in args.providers.split(",") if p.strip()]), indent=2, ensure_ascii=False))
    elif args.command == "probe-once":
        print(json.dumps(run_probe_once(store, args), indent=2, ensure_ascii=False))
    elif args.command == "pilot":
        run_pilot(store, args)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
