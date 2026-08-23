from __future__ import annotations

import dataclasses
import time
import urllib.request
from collections.abc import Callable, Iterable

from ..model import HostObservation
from ..providers import CatalogFetch, classify_failure
from ..storage import TraceStore


@dataclasses.dataclass(frozen=True)
class PassiveResult:
    api_calls: int
    quotes: int
    failures: int


class PassiveWorker:
    def __init__(self, store: TraceStore, providers: Iterable[object]) -> None:
        self.store = store
        self.providers = tuple(providers)

    def run_once(
        self,
        run_id: str,
        *,
        gpu_counts: tuple[int, ...] = (1,),
        host_observation_id: str | None = None,
    ) -> PassiveResult:
        api_calls = 0
        quotes = 0
        failures = 0
        for provider in self.providers:
            fetches: list[CatalogFetch] = provider.catalog_fetches(gpu_counts=gpu_counts)
            for fetch in fetches:
                api_calls += 1
                if not fetch.response.ok:
                    failures += 1
                self.store.record_api_observation(
                    run_id,
                    provider=fetch.provider,
                    operation=fetch.operation,
                    status=str(fetch.response.status),
                    latency_ms=fetch.response.latency_ms,
                    raw=fetch.response.body,
                    failure_class=(
                        None
                        if fetch.response.ok
                        else classify_failure(
                            fetch.response.status, fetch.response.body
                        ).value
                    ),
                    host_observation_id=host_observation_id,
                )
                for quote in fetch.quotes:
                    self.store.record_quote(run_id, quote)
                    quotes += 1
        return PassiveResult(api_calls=api_calls, quotes=quotes, failures=failures)


def default_network_probe(url: str, timeout: int = 10) -> tuple[str, int]:
    started = time.monotonic()
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            response.read(128)
            return str(response.status), round((time.monotonic() - started) * 1000)
    except Exception as error:  # noqa: BLE001 - host health stores the failure
        return f"exception:{type(error).__name__}", round((time.monotonic() - started) * 1000)


class HostMonitor:
    def __init__(
        self,
        store: TraceStore,
        *,
        drift_threshold_seconds: float = 5.0,
        monotonic: Callable[[], float] = time.monotonic,
        network_control_url: str | None = None,
        network_probe: Callable[[str], tuple[str, int]] = default_network_probe,
    ) -> None:
        self.store = store
        self.drift_threshold_seconds = drift_threshold_seconds
        self.monotonic = monotonic
        self.network_control_url = network_control_url
        self.network_probe = network_probe

    def sample(self, run_id: str, expected_monotonic: float) -> HostObservation:
        actual = self.monotonic()
        drift = max(0.0, actual - expected_monotonic)
        network_status = None
        network_latency_ms = None
        if self.network_control_url:
            network_status, network_latency_ms = self.network_probe(self.network_control_url)
        healthy = drift <= self.drift_threshold_seconds
        if network_status and not network_status.startswith("2"):
            healthy = False
        observation_id = self.store.record_host(
            run_id,
            expected_monotonic=expected_monotonic,
            actual_monotonic=actual,
            scheduler_drift_ms=round(drift * 1000),
            healthy=healthy,
            network_status=network_status,
            network_latency_ms=network_latency_ms,
        )
        return HostObservation(
            id=observation_id,
            expected_monotonic=expected_monotonic,
            actual_monotonic=actual,
            scheduler_drift_ms=round(drift * 1000),
            healthy=healthy,
            network_status=network_status,
            network_latency_ms=network_latency_ms,
        )
