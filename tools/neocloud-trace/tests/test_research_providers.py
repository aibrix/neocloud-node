import json
import pathlib
import sys
import threading
import unittest
import urllib.parse


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.model import FailureClass, TrialCell
from neocloud_trace.providers import (
    HTTPResponse,
    LambdaResearchClient,
    RunPodResearchClient,
    VastResearchClient,
)
from neocloud_trace.runners.capacity import ReplicaRequest


class LambdaCapacityExecutorTest(unittest.TestCase):
    def test_rejects_undocumented_multi_instance_launch_quantity(self):
        with self.assertRaisesRegex(ValueError, "one instance per request"):
            LambdaResearchClient(
                api_key="unused",
                ssh_key_names=("test-key",),
                max_launch_quantity=2,
            )

    def test_default_launch_serializes_requests_as_quantity_one(self):
        calls = []

        def transport(method, url, headers, body, timeout):
            calls.append(body)
            return HTTPResponse(
                200,
                10,
                payload={"data": {"instance_ids": [f"instance-{len(calls)}"]}},
            )

        client = LambdaResearchClient(
            api_key="unused",
            ssh_key_names=("test-key",),
            transport=transport,
            sleep=lambda _: None,
            monotonic=lambda: 100.0,
        )
        cell = TrialCell(
            "lambda", "gpu_1x_a10", "A10", "us-east-1", "on-demand", "on-demand"
        )

        results = client.create_replicas(
            cell,
            [ReplicaRequest("r1", "r1", 4, 0), ReplicaRequest("r2", "r2", 4, 1)],
        )

        self.assertTrue(all("quantity" not in call for call in calls))
        self.assertEqual(
            [result.provider_resource_id for result in results],
            ["instance-1", "instance-2"],
        )
        self.assertEqual(results[0].request_payload, calls[0])
        self.assertEqual(results[1].request_payload, calls[1])
        self.assertNotEqual(results[0].api_call.id, results[1].api_call.id)

    def test_launch_response_without_instance_id_is_unknown(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(200, 10, payload={"data": {"instance_ids": []}})

        client = LambdaResearchClient(
            api_key="unused",
            ssh_key_names=("test-key",),
            transport=transport,
            sleep=lambda _: None,
            monotonic=lambda: 100.0,
        )
        cell = TrialCell("lambda", "gpu_1x_a10", "A10", "us-east-1", "on-demand", "on-demand")

        results = client.create_replicas(
            cell,
            [ReplicaRequest("r1", "r1", 1, 0)],
        )

        self.assertFalse(results[0].accepted)
        self.assertEqual(results[0].failure_class, FailureClass.UNKNOWN)

    def test_preempted_instance_is_terminal(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={"data": {"id": "instance-1", "status": "preempted"}},
            )

        client = LambdaResearchClient(
            api_key="unused", ssh_key_names=(), transport=transport
        )
        cell = TrialCell(
            "lambda", "gpu_1x_a10", "A10", "us-east-1", "on-demand", "on-demand"
        )

        observation = client.observe("instance-1", cell)

        self.assertFalse(observation.exists)
        self.assertEqual(observation.provider_state, "preempted")

    def test_lookup_replica_recovers_response_lost_instance_by_exact_name(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "data": [
                        {
                            "id": "instance-1",
                            "name": "depth-r1",
                            "status": "active",
                            "region": {"name": "us-east-1"},
                            "instance_type": {"price_cents_per_hour": 129},
                        }
                    ]
                },
            )

        client = LambdaResearchClient(
            api_key="unused",
            ssh_key_names=("test-key",),
            transport=transport,
        )
        cell = TrialCell(
            "lambda", "gpu_1x_a10", "A10", "us-east-1", "on-demand", "on-demand"
        )

        result = client.lookup_replica(cell, "depth-r1")

        self.assertTrue(result.accepted)
        self.assertEqual(result.provider_resource_id, "instance-1")
        self.assertEqual(result.price_usd_hr, 1.29)
        self.assertEqual(result.api_call.operation, "list_instances_for_create")

    def test_batch_cleanup_terminates_all_before_waiting_for_inventory_zero(self):
        calls = []
        inventory = [
            {
                "data": [
                    {"id": "instance-1", "status": "terminating"},
                    {"id": "instance-2", "status": "terminating"},
                ]
            },
            {"data": []},
        ]
        now = [0.0]

        def transport(method, url, headers, body, timeout):
            calls.append((method, url, body))
            if method == "POST":
                return HTTPResponse(200, 5, payload={"data": {}})
            return HTTPResponse(200, 5, payload=inventory.pop(0))

        def sleep(seconds):
            now[0] += seconds

        client = LambdaResearchClient(
            api_key="unused",
            ssh_key_names=("test-key",),
            transport=transport,
            sleep=sleep,
            monotonic=lambda: now[0],
            cleanup_poll_interval_seconds=1,
            cleanup_timeout_seconds=10,
        )
        cell = TrialCell(
            "lambda", "gpu_1x_a10", "A10", "us-east-1", "on-demand", "on-demand"
        )

        deleted = client.delete_replicas(("instance-1", "instance-2"), cell)
        verified = client.verify_deleted_replicas(
            ("instance-1", "instance-2"),
            cell,
        )

        self.assertEqual(calls[0][0], "POST")
        self.assertEqual(calls[0][2]["instance_ids"], ["instance-1", "instance-2"])
        self.assertTrue(all(result.accepted for result in deleted))
        self.assertTrue(all(result.missing for result in verified))
        self.assertEqual(len([call for call in calls if call[0] == "POST"]), 1)


class RunPodCapacityExecutorTest(unittest.TestCase):
    def test_default_worker_pool_can_issue_the_d32_final_stage(self):
        client = RunPodResearchClient(api_key="unused")

        self.assertGreaterEqual(client.max_workers, 16)

    def test_billing_history_returns_resource_attributable_records(self):
        calls = []

        def transport(method, url, headers, body, timeout):
            calls.append((method, url, body))
            return HTTPResponse(
                200,
                7,
                payload=[
                    {
                        "amount": 0.42,
                        "diskSpaceBilledGb": 20,
                        "gpuTypeId": "NVIDIA A40",
                        "podId": "pod-1",
                        "time": "2026-08-10T00:00:00Z",
                        "timeBilledMs": 3600000,
                    }
                ],
            )

        client = RunPodResearchClient(api_key="unused", transport=transport)

        records = client.billing_records(
            start_time="2026-08-10T00:00:00Z",
            resource_ids=("pod-1",),
        )

        self.assertEqual(len(records), 1)
        self.assertEqual(records[0].provider_resource_id, "pod-1")
        self.assertEqual(records[0].amount_usd, 0.42)
        self.assertEqual(records[0].billed_duration_ms, 3600000)
        self.assertEqual(records[0].source, "runpod_billing_api")
        self.assertIn("/billing/pods?", calls[0][1])
        self.assertIn("grouping=podId", calls[0][1])
        self.assertIn("podId=pod-1", calls[0][1])

    def test_parallel_create_uses_unique_names_and_research_health_probe(self):
        bodies = []
        lock = threading.Lock()

        def transport(method, url, headers, body, timeout):
            with lock:
                bodies.append(body)
                index = len(bodies)
            return HTTPResponse(
                201,
                10,
                payload={
                    "id": f"pod-{index}",
                    "costPerHr": 0.30,
                    "desiredStatus": "RUNNING",
                },
            )

        client = RunPodResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="interruptible",
            image="python:3.12-slim",
        )
        requests = [
            ReplicaRequest("r1", "depth-r1", 2, 0),
            ReplicaRequest("r2", "depth-r2", 2, 1),
        ]

        results = client.create_replicas(cell, requests)

        self.assertEqual(len(results), 2)
        self.assertTrue(all(result.accepted for result in results))
        self.assertEqual({body["name"] for body in bodies}, {"depth-r1", "depth-r2"})
        self.assertTrue(all(body["interruptible"] for body in bodies))
        self.assertTrue(all(body["cloudType"] == "SECURE" for body in bodies))
        self.assertTrue(all(body["gpuCount"] == 1 for body in bodies))
        self.assertTrue(all("8080/http" in body["ports"] for body in bodies))
        self.assertTrue(all("http.server" in " ".join(body["dockerStartCmd"]) for body in bodies))
        self.assertEqual(len({result.api_call.id for result in results}), 2)
        self.assertTrue(all(result.api_call.operation == "create_pod" for result in results))

    def test_parallel_create_preserves_multi_gpu_instance_shape(self):
        bodies = []

        def transport(method, url, headers, body, timeout):
            bodies.append(body)
            return HTTPResponse(201, 10, payload={"id": "pod-1", "costPerHr": 1.2})

        client = RunPodResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
            gpu_count=4,
        )

        result = client.create_replicas(
            cell,
            [ReplicaRequest("r1", "depth-r1", 4, 0)],
        )[0]

        self.assertEqual(bodies[0]["gpuCount"], 4)
        self.assertEqual(result.request_payload, bodies[0])

    def test_create_capacity_and_schema_errors_are_classified(self):
        responses = [
            HTTPResponse(500, 5, error={"error": "There are no instances currently available"}),
            HTTPResponse(400, 5, error={"problems": ["gpuTypeIds enum schema mismatch"]}),
        ]

        def transport(method, url, headers, body, timeout):
            return responses.pop(0)

        client = RunPodResearchClient(api_key="unused", transport=transport)
        cell = TrialCell("runpod", "NVIDIA A40", "A40", None, "secure", "on-demand")

        capacity = client.create_replicas(cell, [ReplicaRequest("r1", "r1", 1, 0)])[0]
        schema = client.create_replicas(cell, [ReplicaRequest("r2", "r2", 1, 0)])[0]

        self.assertEqual(capacity.failure_class, FailureClass.CAPACITY)
        self.assertEqual(schema.failure_class, FailureClass.UNSUPPORTED_SPEC)

    def test_delete_treats_missing_resource_as_clean(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(404, 5, error={"error": "not found"})

        client = RunPodResearchClient(api_key="unused", transport=transport)
        cell = TrialCell("runpod", "NVIDIA A40", "A40", None, "secure", "on-demand")

        result = client.delete("missing-pod", cell)

        self.assertTrue(result.accepted)
        self.assertTrue(result.missing)

    def test_observe_uses_http_proxy_when_public_ip_mapping_is_absent(self):
        probed = []

        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "id": "pod-http",
                    "desiredStatus": "RUNNING",
                    "machineId": "machine-1",
                    "ports": ["8080/http"],
                    "publicIp": "",
                    "portMappings": {},
                },
            )

        def heartbeat(url, timeout):
            probed.append(url)
            return True, 12, "aibrix-neocloud-health-v1"

        client = RunPodResearchClient(
            api_key="unused", transport=transport, heartbeat_probe=heartbeat
        )
        cell = TrialCell(
            "runpod", "NVIDIA A40", "A40", None, "secure", "on-demand"
        )

        observation = client.observe("pod-http", cell)

        self.assertTrue(observation.application_ready)
        self.assertEqual(
            probed, ["https://pod-http-8080.proxy.runpod.net/healthz"]
        )

    def test_exited_pod_is_terminal(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "id": "pod-exited",
                    "desiredStatus": "EXITED",
                    "machineId": "machine-1",
                },
            )

        client = RunPodResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "runpod", "NVIDIA A40", "A40", None, "secure", "on-demand"
        )

        observation = client.observe("pod-exited", cell)

        self.assertFalse(observation.exists)
        self.assertEqual(observation.provider_state, "EXITED")


class VastResearchClientTest(unittest.TestCase):
    def test_catalog_records_offer_level_on_demand_and_interruptible_quotes(self):
        bodies = []

        def transport(method, url, headers, body, timeout):
            self.assertEqual(method, "POST")
            self.assertTrue(url.endswith("/bundles/"))
            bodies.append(body)
            is_bid = body["type"] == "bid"
            return HTTPResponse(
                200,
                8,
                payload={
                    "offers": [
                        {
                            "id": 101 if not is_bid else 102,
                            "gpu_name": "RTX_4090",
                            "num_gpus": 1,
                            "verification": "verified",
                            "geolocation": "US",
                            "machine_id": 77,
                            "dph_total": 0.31 if not is_bid else 0.22,
                            "min_bid": 0.20,
                            "cpu_cores_effective": 8,
                            "cpu_ram": 32768,
                            "disk_space": 100,
                        }
                    ]
                },
            )

        fetches = VastResearchClient(
            api_key="unused", transport=transport
        ).catalog_fetches(gpu_counts=(1, 4))

        self.assertEqual([body["type"] for body in bodies], ["on-demand", "bid"])
        self.assertEqual(len(fetches), 2)
        quotes = [quote for fetch in fetches for quote in fetch.quotes]
        self.assertEqual({quote.market for quote in quotes}, {"on-demand", "interruptible"})
        self.assertTrue(all(quote.sku == "RTX_4090" for quote in quotes))
        self.assertTrue(all(quote.resource_class == "verified" for quote in quotes))
        self.assertTrue(all(quote.datacenter == "77" for quote in quotes))
        self.assertEqual(quotes[0].memory_gib, 32.0)
        self.assertEqual(quotes[0].query["offer_id"], 101)
        self.assertEqual(quotes[1].quoted_price_usd_hr, 0.22)
        self.assertEqual(quotes[1].spot_price_usd_hr, 0.22)

    def test_interruptible_create_assigns_unique_offers_and_passes_bid_floor(self):
        create_calls = []
        lock = threading.Lock()

        def transport(method, url, headers, body, timeout):
            if method == "POST":
                return HTTPResponse(
                    200,
                    5,
                    payload={
                        "offers": [
                            {
                                "id": 202,
                                "gpu_name": "RTX_4090",
                                "num_gpus": 1,
                                "machine_id": 2,
                                "geolocation": "US",
                                "dph_total": 0.24,
                                "min_bid": 0.22,
                                "reliability": 0.99,
                            },
                            {
                                "id": 201,
                                "gpu_name": "RTX_4090",
                                "num_gpus": 1,
                                "machine_id": 1,
                                "geolocation": "US",
                                "dph_total": 0.20,
                                "min_bid": 0.18,
                                "reliability": 0.98,
                            },
                        ]
                    },
                )
            with lock:
                create_calls.append((url, body))
            offer_id = url.rstrip("/").rsplit("/", 1)[-1]
            return HTTPResponse(
                200,
                7,
                payload={"success": True, "new_contract": int(offer_id) + 1000},
            )

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast",
            "RTX_4090",
            "RTX 4090",
            "US",
            "verified",
            "interruptible",
        )
        requests = [
            ReplicaRequest("r1", "vast-r1", 2, 0),
            ReplicaRequest("r2", "vast-r2", 2, 1),
        ]

        results = client.create_replicas(cell, requests)

        self.assertTrue(all(result.accepted for result in results))
        self.assertEqual(
            {result.provider_resource_id for result in results}, {"1201", "1202"}
        )
        self.assertEqual(
            {url.rstrip("/").rsplit("/", 1)[-1] for url, _ in create_calls},
            {"201", "202"},
        )
        self.assertEqual({body["price"] for _, body in create_calls}, {0.18, 0.22})
        self.assertEqual({body["label"] for _, body in create_calls}, {"vast-r1", "vast-r2"})
        self.assertTrue(all(body["runtype"] == "args" for _, body in create_calls))
        self.assertTrue(
            all("http.server 8080" in " ".join(body["args"]) for _, body in create_calls)
        )

    def test_create_reports_capacity_when_distinct_offers_are_exhausted(self):
        def transport(method, url, headers, body, timeout):
            if method == "POST":
                return HTTPResponse(
                    200,
                    5,
                    payload={
                        "offers": [
                            {
                                "id": 301,
                                "gpu_name": "RTX_4090",
                                "num_gpus": 1,
                                "dph_total": 0.3,
                                "reliability": 0.99,
                            }
                        ]
                    },
                )
            return HTTPResponse(
                200, 5, payload={"success": True, "new_contract": 1301}
            )

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        results = client.create_replicas(
            cell,
            [ReplicaRequest("r1", "r1", 2, 0), ReplicaRequest("r2", "r2", 2, 1)],
        )

        self.assertTrue(results[0].accepted)
        self.assertFalse(results[1].accepted)
        self.assertEqual(results[1].failure_class, FailureClass.CAPACITY)
        self.assertEqual(results[1].api_call.operation, "search_offers_for_create")

    def test_search_failure_returns_one_result_per_requested_replica(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(401, 5, error={"error": "invalid api key"})

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        results = client.create_replicas(
            cell,
            [ReplicaRequest("r1", "r1", 2, 0), ReplicaRequest("r2", "r2", 2, 1)],
        )

        self.assertEqual(len(results), 2)
        self.assertTrue(all(not result.accepted for result in results))
        self.assertTrue(
            all(result.failure_class == FailureClass.AUTH for result in results)
        )

    def test_search_semantic_failure_is_recorded_on_api_call(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={"success": False, "msg": "offer search failed"},
            )

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        result = client.create_replicas(
            cell, [ReplicaRequest("r1", "r1", 1, 0)]
        )[0]

        self.assertFalse(result.accepted)
        self.assertEqual(result.failure_class, FailureClass.UNKNOWN)
        self.assertEqual(result.api_call.failure_class, FailureClass.UNKNOWN)

    def test_create_batches_do_not_break_barrier_at_worker_limit(self):
        create_calls = []

        def transport(method, url, headers, body, timeout):
            if method == "POST":
                return HTTPResponse(
                    200,
                    5,
                    payload={
                        "offers": [
                            {"id": 351, "dph_total": 0.3},
                            {"id": 352, "dph_total": 0.4},
                        ]
                    },
                )
            create_calls.append(url)
            offer_id = int(url.rstrip("/").rsplit("/", 1)[-1])
            return HTTPResponse(
                200,
                5,
                payload={"success": True, "new_contract": offer_id + 1000},
            )

        client = VastResearchClient(
            api_key="unused", transport=transport, max_workers=1
        )
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        results = client.create_replicas(
            cell,
            [ReplicaRequest("r1", "r1", 2, 0), ReplicaRequest("r2", "r2", 2, 1)],
        )

        self.assertTrue(all(result.accepted for result in results))
        self.assertEqual(len(create_calls), 2)

    def test_stale_offer_is_classified_as_capacity(self):
        def transport(method, url, headers, body, timeout):
            if method == "POST":
                return HTTPResponse(
                    200,
                    5,
                    payload={
                        "offers": [
                            {
                                "id": 401,
                                "gpu_name": "RTX_4090",
                                "num_gpus": 1,
                                "dph_total": 0.3,
                                "reliability": 0.99,
                            }
                        ]
                    },
                )
            return HTTPResponse(
                410,
                5,
                error={"success": False, "error": "no_such_ask"},
            )

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        result = client.create_replicas(
            cell, [ReplicaRequest("r1", "r1", 1, 0)]
        )[0]

        self.assertFalse(result.accepted)
        self.assertEqual(result.failure_class, FailureClass.CAPACITY)
        self.assertEqual(result.api_call.failure_class, FailureClass.CAPACITY)

    def test_lookup_uses_paginated_v1_instances_filtered_by_label(self):
        urls = []

        def transport(method, url, headers, body, timeout):
            urls.append(url)
            if len(urls) == 1:
                return HTTPResponse(
                    200,
                    5,
                    payload={"instances": [], "next_token": "page-2"},
                )
            return HTTPResponse(
                200,
                5,
                payload={
                    "instances": [
                        {
                            "id": 451,
                            "label": "vast-recovery",
                            "actual_status": "running",
                            "machine_id": 55,
                            "dph_total": 0.35,
                            "geolocation": "US",
                        }
                    ],
                    "next_token": None,
                },
            )

        client = VastResearchClient(
            api_key="unused",
            base_url="https://example.test/api/v0",
            transport=transport,
        )
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        result = client.lookup_replica(cell, "vast-recovery")

        self.assertTrue(result.accepted)
        self.assertEqual(result.provider_resource_id, "451")
        self.assertTrue(
            all(url.startswith("https://example.test/api/v1/instances/?") for url in urls)
        )
        first_query = urllib.parse.parse_qs(urllib.parse.urlparse(urls[0]).query)
        self.assertEqual(
            json.loads(first_query["select_filters"][0]),
            {"label": {"in": ["vast-recovery"]}},
        )
        second_query = urllib.parse.parse_qs(urllib.parse.urlparse(urls[1]).query)
        self.assertEqual(second_query["after_token"], ["page-2"])

    def test_observe_maps_running_instance_and_probes_mapped_health_port(self):
        probed = []

        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "instances": {
                        "id": 501,
                        "actual_status": "running",
                        "machine_id": 91,
                        "geolocation": "CA",
                        "dph_total": 0.42,
                        "uptime_mins": 2,
                        "public_ipaddr": "203.0.113.4",
                        "ports": [
                            {
                                "PrivatePort": 8080,
                                "PublicPort": 18080,
                                "IP": "203.0.113.4",
                            }
                        ],
                    }
                },
            )

        def heartbeat(url, timeout):
            probed.append(url)
            return True, 11, "aibrix-neocloud-health-v1"

        client = VastResearchClient(
            api_key="unused", transport=transport, heartbeat_probe=heartbeat
        )
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        observation = client.observe("501", cell)

        self.assertTrue(observation.exists)
        self.assertTrue(observation.provider_ready)
        self.assertTrue(observation.application_ready)
        self.assertEqual(observation.runtime_uptime_seconds, 120.0)
        self.assertEqual(observation.machine_id, "91")
        self.assertEqual(probed, ["http://203.0.113.4:18080/healthz"])

    def test_observe_uses_public_ip_for_vast_port_mapping_dictionary(self):
        probed = []

        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "instances": {
                        "id": 502,
                        "actual_status": "running",
                        "public_ipaddr": "203.0.113.5",
                        "ports": {
                            "8080/tcp": [
                                {"HostIp": "0.0.0.0", "HostPort": "28080"}
                            ]
                        },
                    }
                },
            )

        def heartbeat(url, timeout):
            probed.append(url)
            return True, 9, "aibrix-neocloud-health-v1"

        client = VastResearchClient(
            api_key="unused", transport=transport, heartbeat_probe=heartbeat
        )
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        observation = client.observe("502", cell)

        self.assertTrue(observation.application_ready)
        self.assertEqual(probed, ["http://203.0.113.5:28080/healthz"])

    def test_observe_does_not_treat_null_actual_status_as_running(self):
        probed = []

        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "instances": {
                        "id": 503,
                        "actual_status": None,
                        "intended_status": "running",
                    }
                },
            )

        def heartbeat(url, timeout):
            probed.append(url)
            return True, 9, "aibrix-neocloud-health-v1"

        client = VastResearchClient(
            api_key="unused", transport=transport, heartbeat_probe=heartbeat
        )
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        observation = client.observe("503", cell)

        self.assertTrue(observation.exists)
        self.assertEqual(observation.provider_state, "provisioning")
        self.assertFalse(observation.provider_ready)
        self.assertEqual(probed, [])

    def test_null_instance_is_absent_for_delete_verification(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(200, 5, payload={"instances": None})

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
        )

        observation = client.observe("missing", cell)
        verified = client.verify_deleted("missing", cell)

        self.assertFalse(observation.exists)
        self.assertTrue(verified.accepted)
        self.assertTrue(verified.missing)

    def test_delete_treats_not_found_as_clean(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                404,
                5,
                error={"success": False, "error": "not_found"},
            )

        result = VastResearchClient(
            api_key="unused", transport=transport
        ).delete(
            "missing",
            TrialCell(
                "vast", "RTX_4090", "RTX 4090", None, "verified", "on-demand"
            ),
        )

        self.assertTrue(result.accepted)
        self.assertTrue(result.missing)

    def test_billing_pages_and_filters_trace_owned_instances(self):
        urls = []

        def transport(method, url, headers, body, timeout):
            urls.append(url)
            if len(urls) == 1:
                return HTTPResponse(
                    200,
                    5,
                    payload={
                        "success": True,
                        "next_token": "next-page",
                        "results": [
                            {
                                "start": 1786320000,
                                "end": 1786323600,
                                "type": "instance",
                                "source": "instance-601",
                                "amount": 0.42,
                            },
                            {
                                "start": 1786320000,
                                "end": 1786323600,
                                "type": "instance",
                                "source": "instance-other",
                                "amount": 9.99,
                            },
                        ],
                    },
                )
            return HTTPResponse(
                200,
                5,
                payload={
                    "success": True,
                    "next_token": None,
                    "results": [
                        {
                            "start": 1786323600,
                            "end": 1786325400,
                            "type": "instance",
                            "source": "instance-601",
                            "amount": 0.21,
                        }
                    ],
                },
            )

        client = VastResearchClient(
            api_key="unused",
            transport=transport,
            wall_time=lambda: 1786406400,
        )

        records = client.billing_records(
            start_time="2026-08-10T00:00:00+00:00",
            resource_ids=("601",),
        )

        self.assertEqual(len(records), 2)
        self.assertTrue(all(record.provider_resource_id == "601" for record in records))
        self.assertEqual([record.amount_usd for record in records], [0.42, 0.21])
        self.assertEqual([record.billed_duration_ms for record in records], [3600000, 1800000])
        self.assertTrue(all(record.source == "vast_charges_api" for record in records))
        second_query = urllib.parse.parse_qs(urllib.parse.urlparse(urls[1]).query)
        self.assertEqual(second_query["after_token"], ["next-page"])

    def test_billing_reads_gpu_hours_from_vast_daily_charge_details(self):
        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "success": True,
                    "next_token": None,
                    "results": [
                        {
                            "start": 1787443200,
                            "end": 1787443200,
                            "type": "instance",
                            "source": "instance-701",
                            "amount": 0.001,
                            "items": [
                                {
                                    "type": "gpu",
                                    "description": "0.0120148 hours at $0.047/hour",
                                },
                                {
                                    "type": "disk",
                                    "description": "0.015 hours at $0.006/hour",
                                },
                            ],
                        }
                    ],
                },
            )

        records = VastResearchClient(
            api_key="unused",
            transport=transport,
            wall_time=lambda: 1787446800,
        ).billing_records(
            start_time="2026-08-23T00:00:00+00:00",
            resource_ids=("701",),
        )

        self.assertEqual(len(records), 1)
        self.assertEqual(records[0].billed_duration_ms, 43253)

    def test_billing_record_identity_is_stable_while_charge_settles(self):
        amount = [0.001]

        def transport(method, url, headers, body, timeout):
            return HTTPResponse(
                200,
                5,
                payload={
                    "success": True,
                    "next_token": None,
                    "results": [
                        {
                            "start": 1787443200,
                            "end": 1787443200,
                            "type": "instance",
                            "source": "instance-702",
                            "amount": amount[0],
                        }
                    ],
                },
            )

        client = VastResearchClient(
            api_key="unused",
            transport=transport,
            wall_time=lambda: 1787446800,
        )
        first = client.billing_records(
            start_time="2026-08-23T00:00:00+00:00",
            resource_ids=("702",),
        )[0]
        amount[0] = 0.003
        settled = client.billing_records(
            start_time="2026-08-23T00:00:00+00:00",
            resource_ids=("702",),
        )[0]

        self.assertEqual(first.source_record_id, settled.source_record_id)
        self.assertNotEqual(first.amount_usd, settled.amount_usd)

    def test_invalid_machine_id_is_rejected_without_contacting_provider(self):
        calls = []

        def transport(method, url, headers, body, timeout):
            calls.append(url)
            raise AssertionError("transport must not be called")

        client = VastResearchClient(api_key="unused", transport=transport)
        cell = TrialCell(
            "vast",
            "RTX_4090",
            "RTX 4090",
            None,
            "verified",
            "on-demand",
            datacenter="not-a-machine-id",
        )

        result = client.create_replicas(
            cell, [ReplicaRequest("r1", "r1", 1, 0)]
        )[0]

        self.assertEqual(result.failure_class, FailureClass.UNSUPPORTED_SPEC)
        self.assertEqual(calls, [])


if __name__ == "__main__":
    unittest.main()
