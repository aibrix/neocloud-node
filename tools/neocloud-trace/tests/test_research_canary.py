import datetime as dt
import pathlib
import sys
import tempfile
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.model import CensorReason, ExperimentConfig, TrialCell
from neocloud_trace.providers import CreateResult, DeleteResult, ResourceObservation
from neocloud_trace.runners.canary import CanaryClassifier, CanaryRunner, CanarySample
from neocloud_trace.storage import ResearchStore


UTC = dt.timezone.utc


class CanaryClassifierTest(unittest.TestCase):
    def setUp(self):
        self.start = dt.datetime(2026, 8, 8, 0, 0, tzinfo=UTC)
        self.end = self.start + dt.timedelta(hours=3)
        self.classifier = CanaryClassifier(heartbeat_grace_seconds=90)

    def sample(self, offset_seconds, **kwargs):
        defaults = dict(
            observed_at=self.start + dt.timedelta(seconds=offset_seconds),
            exists=True,
            provider_state="RUNNING",
            provider_ready=True,
            heartbeat_ok=True,
            machine_id="machine-1",
            runtime_uptime_seconds=float(offset_seconds),
            host_healthy=True,
        )
        defaults.update(kwargs)
        return CanarySample(**defaults)

    def test_planned_end_is_right_censored(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.end,
            samples=[self.sample(60)],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.RIGHT_CENSORED_PLANNED_END)

    def test_disappearance_before_end_is_interruption_candidate(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(minutes=10),
            samples=[self.sample(60), self.sample(120, exists=False, provider_state=None, heartbeat_ok=False)],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.PROVIDER_INTERRUPTION_CANDIDATE)

    def test_single_fresh_missing_sample_is_not_yet_an_interruption(self):
        now = self.start + dt.timedelta(seconds=121)
        result = self.classifier.classify(
            planned_end=self.end,
            now=now,
            samples=[
                self.sample(60),
                self.sample(
                    120,
                    exists=False,
                    provider_state=None,
                    heartbeat_ok=False,
                ),
            ],
            operator_cleanup=False,
        )
        self.assertIsNone(result)

    def test_explicit_preempted_state_is_an_interruption_candidate(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(seconds=121),
            samples=[
                self.sample(60),
                self.sample(120, provider_state="preempted", provider_ready=False),
            ],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.PROVIDER_INTERRUPTION_CANDIDATE)

    def test_disappearance_before_first_readiness_is_pre_ready_failure(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(seconds=181),
            samples=[
                self.sample(
                    120,
                    exists=False,
                    provider_state=None,
                    provider_ready=False,
                    heartbeat_ok=False,
                    machine_id=None,
                    runtime_uptime_seconds=None,
                )
            ],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.PROVIDER_PRE_READY_FAILURE)

    def test_heartbeat_failure_with_healthy_host_is_application_failure(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(seconds=200),
            samples=[
                self.sample(60),
                self.sample(100, heartbeat_ok=False),
                self.sample(200, heartbeat_ok=False),
            ],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.APPLICATION_FAILURE)

    def test_machine_change_is_replacement(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(minutes=5),
            samples=[self.sample(60), self.sample(120, machine_id="machine-2", runtime_uptime_seconds=5)],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.MACHINE_REPLACEMENT)

    def test_collector_gap_is_right_censored_not_provider_failure(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(minutes=5),
            samples=[self.sample(60), self.sample(240, exists=False, heartbeat_ok=False, host_healthy=False)],
            operator_cleanup=False,
        )
        self.assertEqual(result, CensorReason.RIGHT_CENSORED_COLLECTOR_LOSS)

    def test_operator_cleanup_has_priority(self):
        result = self.classifier.classify(
            planned_end=self.end,
            now=self.start + dt.timedelta(minutes=5),
            samples=[self.sample(60, exists=False)],
            operator_cleanup=True,
        )
        self.assertEqual(result, CensorReason.OPERATOR_CLEANUP)


class FakeCanaryProvider:
    provider = "runpod"

    def __init__(self):
        self.created_markets = []
        self.deleted = []
        self.verified = []

    def create_replicas(self, cell, requests):
        self.created_markets.append(cell.market)
        return [CreateResult(True, f"pod-{cell.market}", price_usd_hr=0.3)]

    def observe(self, resource_id, cell):
        return ResourceObservation(
            exists=True,
            provider_state="RUNNING",
            provider_ready=True,
            application_ready=True,
            machine_id="machine-1",
            runtime_uptime_seconds=60,
            price_usd_hr=0.3,
        )

    def delete(self, resource_id, cell):
        self.deleted.append(resource_id)
        return DeleteResult(True)

    def verify_deleted(self, resource_id, cell):
        self.verified.append(resource_id)
        return DeleteResult(True, missing=True)


class CanaryRunnerTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.store = ResearchStore(pathlib.Path(self.tempdir.name) / "trace.db")
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

    def tearDown(self):
        self.store.close()
        self.tempdir.cleanup()

    def test_starts_matched_pair_and_recovers_without_duplicate_create(self):
        provider = FakeCanaryProvider()
        runner = CanaryRunner(self.store, provider)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
            image="python:3.12-slim",
        )
        planned_end = dt.datetime.now(UTC) + dt.timedelta(hours=3)

        first = runner.start_pair(self.run_id, cell, planned_end)
        recovered = runner.start_pair(self.run_id, cell, planned_end)

        self.assertEqual(provider.created_markets, ["interruptible", "on-demand"])
        self.assertEqual([lease.id for lease in first], [lease.id for lease in recovered])
        self.assertEqual({lease.interruptible for lease in first}, {True, False})
        self.assertEqual(len(self.store.owned_resources(self.run_id)), 2)

    def test_starts_single_control_without_interruptible_request(self):
        provider = FakeCanaryProvider()
        runner = CanaryRunner(self.store, provider)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
            image="python:3.12-slim",
        )
        planned_end = dt.datetime.now(UTC) + dt.timedelta(hours=1)

        first = runner.start_single(
            self.run_id,
            cell,
            planned_end,
            group_id="control-epoch-1",
        )
        recovered = runner.start_single(
            self.run_id,
            cell,
            planned_end,
            group_id="control-epoch-1",
        )

        self.assertEqual(provider.created_markets, ["on-demand"])
        self.assertEqual(first.id, recovered.id)
        self.assertFalse(first.interruptible)
        self.assertEqual(len(self.store.owned_resources(self.run_id)), 1)

    def test_stop_requires_remote_disappearance_before_releasing_ownership(self):
        provider = FakeCanaryProvider()
        runner = CanaryRunner(self.store, provider)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        lease = runner.start_pair(
            self.run_id, cell, dt.datetime.now(UTC) + dt.timedelta(minutes=1)
        )[0]

        runner.stop(self.run_id, lease, reason=CensorReason.OPERATOR_CLEANUP)

        self.assertEqual(provider.verified, [lease.provider_resource_id])
        remaining = self.store.owned_resources(self.run_id)
        self.assertEqual(len(remaining), 1)

    def test_healthy_heartbeat_records_both_provider_and_application_readiness(self):
        provider = FakeCanaryProvider()
        runner = CanaryRunner(self.store, provider)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        lease = runner.start_pair(
            self.run_id, cell, dt.datetime.now(UTC) + dt.timedelta(minutes=1)
        )[0]

        runner.observe_once(lease)

        row = self.store.canary_row(lease.id)
        self.assertIsNotNone(row["provider_ready_at"])
        self.assertIsNotNone(row["application_ready_at"])


if __name__ == "__main__":
    unittest.main()
