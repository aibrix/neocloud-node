import pathlib
import sys
import threading
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.model import FailureClass, TrialCell
from neocloud_trace.providers import HTTPResponse, LambdaResearchClient, RunPodResearchClient
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


if __name__ == "__main__":
    unittest.main()
