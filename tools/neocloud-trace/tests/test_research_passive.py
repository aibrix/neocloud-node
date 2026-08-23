import pathlib
import sys
import tempfile
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.model import CollectionConfig, ExperimentConfig
from neocloud_trace.providers import (
    CatalogFetch,
    HTTPResponse,
    LambdaResearchClient,
    RunPodResearchClient,
)
from neocloud_trace.runners.passive import HostMonitor, PassiveCollector, PassiveWorker
from neocloud_trace.storage import TraceStore


class ExactCatalogTest(unittest.TestCase):
    def test_runpod_queries_secure_and_community_quotes_separately(self):
        queries = []

        def transport(method, url, headers, body, timeout):
            self.assertEqual(method, "POST")
            query = body["query"]
            queries.append(query)
            secure = "secureCloud: true" in query
            price = 0.50 if secure else 0.30
            stock = "Medium" if secure else "Low"
            return HTTPResponse(
                status=200,
                latency_ms=12,
                payload={
                    "data": {
                        "gpuTypes": [
                            {
                                "id": "NVIDIA A40",
                                "displayName": "A40",
                                "memoryInGb": 48,
                                "lowestPrice": {
                                    "stockStatus": stock,
                                    "availableGpuCounts": [1, 2, 4] if secure else [1],
                                    "uninterruptablePrice": price,
                                    "minimumBidPrice": price - 0.05,
                                },
                            }
                        ]
                    }
                },
            )

        client = RunPodResearchClient(api_key="unused", transport=transport)
        fetches = client.catalog_fetches(gpu_counts=(1,))

        self.assertEqual(len(queries), 2)
        quotes = [quote for fetch in fetches for quote in fetch.quotes]
        self.assertEqual({quote.resource_class for quote in quotes}, {"secure", "community"})
        by_class = {quote.resource_class: quote for quote in quotes}
        self.assertEqual(by_class["secure"].quoted_price_usd_hr, 0.50)
        self.assertEqual(by_class["secure"].stock_status, "Medium")
        self.assertEqual(by_class["secure"].available_gpu_counts, (1, 2, 4))
        self.assertEqual(by_class["community"].quoted_price_usd_hr, 0.30)
        self.assertEqual(by_class["community"].available_gpu_counts, (1,))

    def test_runpod_stock_status_is_available_when_gpu_counts_are_omitted(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                status=200,
                latency_ms=10,
                payload={
                    "data": {
                        "gpuTypes": [
                            {
                                "id": "NVIDIA A40",
                                "displayName": "A40",
                                "memoryInGb": 48,
                                "lowestPrice": {
                                    "stockStatus": "High",
                                    "availableGpuCounts": None,
                                    "uninterruptablePrice": 0.44,
                                    "minimumBidPrice": 0.44,
                                },
                            }
                        ]
                    }
                },
            )

        quotes = [
            quote
            for fetch in RunPodResearchClient(
                api_key="unused", transport=transport
            ).catalog_fetches(gpu_counts=(1,))
            for quote in fetch.quotes
        ]

        self.assertTrue(all(quote.available for quote in quotes))

    def test_lambda_expands_capacity_regions_without_inventing_missing_regions(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                status=200,
                latency_ms=8,
                payload={
                    "data": {
                        "gpu_1x_a10": {
                            "instance_type": {
                                "name": "gpu_1x_a10",
                                "gpu_description": "A10 (24 GB PCIe)",
                                "price_cents_per_hour": 129,
                                "specs": {
                                    "gpus": 1,
                                    "vcpus": 30,
                                    "memory_gib": 200,
                                    "storage_gib": 1400,
                                },
                            },
                            "regions_with_capacity_available": [
                                {"name": "us-east-1"},
                                {"name": "us-west-1"},
                            ],
                        }
                    }
                },
            )

        client = LambdaResearchClient(api_key="unused", ssh_key_names=(), transport=transport)
        fetch = client.catalog_fetches()[0]

        self.assertEqual({quote.region for quote in fetch.quotes}, {"us-east-1", "us-west-1"})
        self.assertTrue(all(quote.available for quote in fetch.quotes))
        self.assertTrue(all(quote.quoted_price_usd_hr == 1.29 for quote in fetch.quotes))


class PassiveWorkerTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.store = TraceStore(pathlib.Path(self.tempdir.name) / "trace.db")
        self.run_id = self.store.start_run(
            ExperimentConfig(
                protocol_version="2.0",
                host_id="host",
                vantage_id="vantage",
                provider_budgets={"runpod": 1.0},
                total_budget_usd=1.0,
            ),
            config_document={"mode": "test"},
        )

    def tearDown(self):
        self.store.close()
        self.tempdir.cleanup()

    def test_passive_worker_persists_api_and_quote_rows(self):
        def transport(method, url, headers, body, timeout):
            secure = "secureCloud: true" in body["query"]
            return HTTPResponse(
                status=200,
                latency_ms=10,
                payload={
                    "data": {
                        "gpuTypes": [
                            {
                                "id": "NVIDIA A40",
                                "displayName": "A40",
                                "memoryInGb": 48,
                                "lowestPrice": {
                                    "stockStatus": "High",
                                    "availableGpuCounts": [1, 2, 4, 8],
                                    "uninterruptablePrice": 0.44 if secure else 0.34,
                                    "minimumBidPrice": 0.30,
                                },
                            }
                        ]
                    }
                },
            )

        client = RunPodResearchClient(api_key="unused", transport=transport)
        result = PassiveWorker(self.store, [client]).run_once(self.run_id, gpu_counts=(1,))

        self.assertEqual(result.api_calls, 2)
        self.assertEqual(result.quotes, 2)
        self.assertEqual(
            self.store.connection.execute("SELECT count(*) FROM provider_api_observations_v2").fetchone()[0],
            2,
        )
        self.assertEqual(
            self.store.connection.execute("SELECT count(*) FROM catalog_quotes_v2").fetchone()[0],
            2,
        )

    def test_passive_worker_classifies_auth_failure(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                status=401,
                latency_ms=10,
                error={"error": {"code": "global/invalid-api-key"}},
            )

        client = LambdaResearchClient(
            api_key="invalid", ssh_key_names=(), transport=transport
        )

        result = PassiveWorker(self.store, [client]).run_once(self.run_id)

        self.assertEqual(result.failures, 1)
        row = self.store.connection.execute(
            "SELECT failure_class FROM provider_api_observations_v2"
        ).fetchone()
        self.assertEqual(row["failure_class"], "auth")

    def test_host_monitor_marks_large_scheduler_drift_unhealthy(self):
        monitor = HostMonitor(self.store, drift_threshold_seconds=5.0, monotonic=lambda: 110.0)

        observation = monitor.sample(self.run_id, expected_monotonic=100.0)

        self.assertFalse(observation.healthy)
        self.assertEqual(observation.scheduler_drift_ms, 10000)

    def test_collector_starts_and_finishes_a_passive_run(self):
        class Provider:
            def catalog_fetches(self, gpu_counts):
                self.gpu_counts = gpu_counts
                return [
                    CatalogFetch(
                        provider="runpod",
                        operation="catalog",
                        response=HTTPResponse(200, 5, payload={}),
                        quotes=(),
                    )
                ]

        provider = Provider()
        config = CollectionConfig(
            protocol_version="1.0-passive-catalog",
            host_id="collector-host",
            vantage_id="collector-host",
            providers=("runpod",),
            gpu_counts=(1, 2),
        )

        run_id = PassiveCollector(self.store, [provider]).run(config, once=True)

        run = self.store.run_row(run_id)
        self.assertEqual(run["status"], "complete")
        self.assertEqual(run["host_id"], "collector-host")
        self.assertEqual(run["vantage_id"], "collector-host")
        self.assertEqual(provider.gpu_counts, (1, 2))
        self.assertEqual(
            self.store.connection.execute(
                "SELECT COUNT(*) FROM host_observations WHERE run_id=?",
                (run_id,),
            ).fetchone()[0],
            1,
        )
        self.assertEqual(
            self.store.connection.execute(
                "SELECT COUNT(*) FROM provider_api_observations_v2 WHERE run_id=?",
                (run_id,),
            ).fetchone()[0],
            1,
        )


if __name__ == "__main__":
    unittest.main()
