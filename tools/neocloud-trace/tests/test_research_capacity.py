import json
import pathlib
import sys
import tempfile
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.model import (
    CatalogQuote,
    ExperimentConfig,
    FailureClass,
    TrialCell,
    capacity_stages,
)
from neocloud_trace.providers import (
    CreateResult,
    DeleteResult,
    ProviderAPICall,
    ResourceObservation,
)
from neocloud_trace.runners.capacity import CapacityRunner
from neocloud_trace.storage import TraceStore


class FakeCapacityProvider:
    provider = "runpod"

    def __init__(self, stage_results):
        self.stage_results = list(stage_results)
        self.create_calls = []
        self.deleted = []
        self.verified = []
        self.observed = []

    def create_replicas(self, cell, requests):
        self.create_calls.append((cell, tuple(requests)))
        return self.stage_results.pop(0)

    def observe(self, resource_id, cell):
        self.observed.append(resource_id)
        return ResourceObservation(
            exists=True,
            provider_state="RUNNING",
            provider_ready=True,
            application_ready=True,
            machine_id="machine-" + resource_id,
            price_usd_hr=0.44,
            raw={"id": resource_id, "desiredStatus": "RUNNING"},
        )

    def delete(self, resource_id, cell):
        self.deleted.append(resource_id)
        return DeleteResult(accepted=True)

    def verify_deleted(self, resource_id, cell):
        self.verified.append(resource_id)
        return DeleteResult(accepted=True, missing=True)


class CapacityRunnerTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.store = TraceStore(pathlib.Path(self.tempdir.name) / "trace.db")
        self.run_id = self.store.start_run(
            ExperimentConfig(
                protocol_version="2.0",
                host_id="host",
                vantage_id="vantage",
                provider_budgets={"runpod": 5.0},
                total_budget_usd=5.0,
            ),
            config_document={"mode": "test"},
        )
        self.cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
            image="python:3.12-slim",
        )

    def tearDown(self):
        self.store.close()
        self.tempdir.cleanup()

    def test_capacity_stages_scale_total_gpu_depth_for_multi_gpu_instances(self):
        stages = capacity_stages(
            target_gpu_depth=32,
            gpu_count_per_instance=4,
        )

        self.assertEqual(
            [
                (
                    stage.target_gpu_depth,
                    stage.target_instance_count,
                    stage.increment_instances,
                )
                for stage in stages
            ],
            [(4, 1, 1), (8, 2, 1), (16, 4, 2), (32, 8, 4)],
        )

    def test_d32_shape_four_reports_instance_and_gpu_depths(self):
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
            gpu_count=4,
            image="python:3.12-slim",
        )
        provider = FakeCapacityProvider(
            [
                [CreateResult(True, "pod-1")],
                [CreateResult(True, "pod-2")],
                [CreateResult(True, "pod-3"), CreateResult(True, "pod-4")],
                [
                    CreateResult(True, "pod-5"),
                    CreateResult(True, "pod-6"),
                    CreateResult(True, "pod-7"),
                    CreateResult(True, "pod-8"),
                ],
            ]
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, cell, target_depth=32)

        self.assertEqual([len(call[1]) for call in provider.create_calls], [1, 1, 2, 4])
        self.assertEqual(result.gpu_count_per_instance, 4)
        self.assertEqual(result.target_gpu_depth, 32)
        self.assertEqual(result.target_instance_count, 8)
        self.assertEqual(result.requested_instance_count, 8)
        self.assertEqual(result.requested_gpu_depth, 32)
        self.assertEqual(result.provider_ready_gpu_depth, 32)
        self.assertEqual(result.cleaned_gpu_depth, 32)

    def test_capacity_shape_and_depth_must_form_a_power_of_two_ladder(self):
        with self.assertRaisesRegex(ValueError, "gpu_count"):
            TrialCell(
                "runpod",
                "NVIDIA A40",
                "A40",
                None,
                "secure",
                "on-demand",
                gpu_count=3,
            )

        with self.assertRaisesRegex(ValueError, "target_gpu_depth"):
            capacity_stages(target_gpu_depth=16, gpu_count_per_instance=32)

    def test_timeout_create_is_recovered_by_deterministic_name_before_retry(self):
        class RecoveringProvider(FakeCapacityProvider):
            def __init__(self):
                super().__init__(
                    [[CreateResult(False, failure_class=FailureClass.TIMEOUT)]]
                )
                self.lookups = []

            def lookup_replica(self, cell, name):
                self.lookups.append(name)
                return CreateResult(
                    True,
                    "recovered-instance",
                    price_usd_hr=1.29,
                    raw={"id": "recovered-instance", "name": name},
                )

        provider = RecoveringProvider()
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            create_reconcile_timeout_seconds=0,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=1)

        self.assertEqual(len(provider.create_calls), 1)
        self.assertEqual(len(provider.lookups), 1)
        self.assertEqual(result.accepted_depth, 1)
        self.assertEqual(result.cleaned_depth, 1)
        self.assertEqual(result.status, "complete")

    def test_unrecovered_timeout_is_transport_censored(self):
        provider = FakeCapacityProvider(
            [[CreateResult(False, failure_class=FailureClass.TIMEOUT)]]
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            max_transient_retries=0,
            create_reconcile_timeout_seconds=0,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=1)

        self.assertEqual(result.first_failed_stage, 1)
        self.assertEqual(result.failure_class, FailureClass.TIMEOUT)
        self.assertEqual(result.status, "transport_censored")

    def test_cumulative_ladder_records_partial_depth_and_cleans_every_resource(self):
        quote_id = self.store.record_quote(
            self.run_id,
            CatalogQuote(
                provider="runpod",
                sku="NVIDIA A40",
                gpu_type="A40",
                gpu_count=1,
                resource_class="secure",
                market="on-demand",
                quoted_price_usd_hr=0.44,
                stock_status="High",
                available=True,
            ),
        )
        provider = FakeCapacityProvider(
            [
                [
                    CreateResult(
                        True,
                        "pod-1",
                        price_usd_hr=0.44,
                        request_payload={"name": "depth-pod-1"},
                    )
                ],
                [CreateResult(True, "pod-2", price_usd_hr=0.44)],
                [
                    CreateResult(True, "pod-3", price_usd_hr=0.44),
                    CreateResult(True, "pod-4", price_usd_hr=0.44),
                ],
                [
                    CreateResult(True, "pod-5", price_usd_hr=0.44),
                    CreateResult(True, "pod-6", price_usd_hr=0.44),
                    CreateResult(False, failure_class=FailureClass.CAPACITY, message="no capacity"),
                    CreateResult(False, failure_class=FailureClass.CAPACITY, message="no capacity"),
                ],
            ]
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=8)

        self.assertEqual([len(call[1]) for call in provider.create_calls], [1, 1, 2, 4])
        self.assertEqual(result.requested_depth, 8)
        self.assertEqual(result.accepted_depth, 6)
        self.assertEqual(result.provider_ready_depth, 6)
        self.assertEqual(result.application_ready_depth, 6)
        self.assertEqual(result.cleaned_depth, 6)
        self.assertEqual(result.first_failed_stage, 8)
        self.assertEqual(result.failure_class, FailureClass.CAPACITY)
        trial = self.store.capacity_trial_row(result.trial_id)
        self.assertEqual(trial["catalog_quote_id"], quote_id)
        first_replica = self.store.capacity_replica_rows(result.trial_id)[0]
        details = json.loads(first_replica["details_json"])
        self.assertEqual(details["create_request"]["name"], "depth-pod-1")
        self.assertIn("create_response", details)
        self.assertEqual(details["last_observation"]["desiredStatus"], "RUNNING")
        self.assertEqual(provider.deleted, ["pod-6", "pod-5", "pod-4", "pod-3", "pod-2", "pod-1"])
        self.assertEqual(provider.verified, provider.deleted)
        self.assertEqual(self.store.owned_resources(self.run_id), [])

    def test_unverified_delete_remains_owned_and_trial_is_cleanup_incomplete(self):
        provider = FakeCapacityProvider([[CreateResult(True, "pod-1")]])
        provider.verify_deleted = lambda resource_id, cell: DeleteResult(
            accepted=False,
            failure_class=FailureClass.TIMEOUT,
            message="still present",
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=1)

        self.assertEqual(result.status, "cleanup_incomplete")
        self.assertEqual(result.cleaned_depth, 0)
        self.assertEqual(len(self.store.owned_resources(self.run_id)), 1)
        cleanup = self.store.connection.execute(
            "SELECT action, status, failure_class FROM cleanup_events ORDER BY rowid"
        ).fetchall()
        self.assertEqual(
            [(row["action"], row["status"], row["failure_class"]) for row in cleanup],
            [("delete", "accepted", None), ("verify_deleted", "failed", "timeout")],
        )

    def test_quota_failure_stops_before_later_stages(self):
        provider = FakeCapacityProvider(
            [
                [CreateResult(True, "pod-1")],
                [CreateResult(False, failure_class=FailureClass.QUOTA, message="quota exceeded")],
            ]
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=8)

        self.assertEqual([len(call[1]) for call in provider.create_calls], [1, 1])
        self.assertEqual(result.requested_depth, 2)
        self.assertEqual(result.accepted_depth, 1)
        self.assertEqual(result.first_failed_stage, 2)
        self.assertEqual(result.failure_class, FailureClass.QUOTA)

    def test_transient_failure_is_retried_once_as_a_distinct_request(self):
        provider = FakeCapacityProvider(
            [
                [CreateResult(False, failure_class=FailureClass.TRANSIENT, message="temporary 500")],
                [CreateResult(True, "pod-retry")],
            ]
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
            max_transient_retries=1,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=1)

        self.assertEqual(len(provider.create_calls), 2)
        self.assertEqual(result.accepted_depth, 1)
        replica_rows = self.store.connection.execute(
            "SELECT status, retry_of FROM capacity_replicas ORDER BY requested_at, rowid"
        ).fetchall()
        self.assertEqual(len(replica_rows), 2)
        self.assertEqual(replica_rows[0]["status"], "failed")
        self.assertIsNotNone(replica_rows[1]["retry_of"])

    def test_disappearance_before_readiness_marks_trial_as_readiness_failure(self):
        provider = FakeCapacityProvider([[CreateResult(True, "pod-1")]])
        provider.observe = lambda resource_id, cell: ResourceObservation(
            exists=False,
            provider_state=None,
            provider_ready=False,
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
        )

        result = runner.run_trial(self.run_id, self.cell, target_depth=1)

        self.assertEqual(result.status, "readiness_failure")
        self.assertEqual(result.first_failed_stage, 1)
        self.assertEqual(result.failure_class, FailureClass.REMOTE_NOT_FOUND)
        self.assertEqual(result.provider_ready_depth, 0)
        self.assertEqual(result.application_ready_depth, 0)

    def test_records_one_api_observation_per_real_provider_call(self):
        shared_call = ProviderAPICall(
            id="lambda-quantity-call",
            operation="launch_instances",
            status=200,
            latency_ms=25,
            failure_class=None,
            raw={"data": {"instance_ids": ["pod-3", "pod-4"]}},
        )
        provider = FakeCapacityProvider(
            [
                [CreateResult(True, "pod-1")],
                [CreateResult(True, "pod-2")],
                [
                    CreateResult(True, "pod-3", api_call=shared_call),
                    CreateResult(True, "pod-4", api_call=shared_call),
                ],
            ]
        )
        runner = CapacityRunner(
            self.store,
            provider,
            poll_interval_seconds=0,
            ready_timeout_seconds=1,
            sleep=lambda _: None,
        )

        runner.run_trial(self.run_id, self.cell, target_depth=4)

        rows = self.store.connection.execute(
            "SELECT operation, status, latency_ms FROM provider_api_observations_v2 "
            "WHERE operation='launch_instances'"
        ).fetchall()
        self.assertEqual(len(rows), 1)
        self.assertEqual(tuple(rows[0]), ("launch_instances", "200", 25))


if __name__ == "__main__":
    unittest.main()
