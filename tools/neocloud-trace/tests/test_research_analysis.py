import csv
import datetime as dt
import pathlib
import sys
import tempfile
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.analysis import (
    capacity_depth_rows,
    capacity_trial_payload,
    export_capacity_depth_csv,
    export_stability_window_csv,
    export_survival_csv,
    stability_window_rows,
    survival_rows,
)
from neocloud_trace.analysis.exports import RIGHT_CENSOR_REASONS
from neocloud_trace.model import (
    CatalogQuote,
    ExperimentConfig,
    TerminalReason,
    TrialCell,
)
from neocloud_trace.storage import TraceStore


UTC = dt.timezone.utc


class TerminalReasonAnalysisTest(unittest.TestCase):
    def test_right_censor_reasons_remain_a_statistical_subset(self):
        self.assertEqual(
            RIGHT_CENSOR_REASONS,
            {
                "operator_cleanup",
                "right_censored_collector_loss",
                "right_censored_planned_end",
            },
        )
        self.assertNotIn(
            TerminalReason.PROVIDER_INTERRUPTION_CANDIDATE.value,
            RIGHT_CENSOR_REASONS,
        )


class SurvivalExportTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tempdir.name)
        self.store = TraceStore(self.root / "trace.db")
        self.run_id = self.store.start_run(
            ExperimentConfig(
                protocol_version="3.0-survival-cohort",
                host_id="host",
                vantage_id="vantage",
                provider_budgets={"runpod": 5.0},
                total_budget_usd=5.0,
                max_active_gpus_per_provider=2,
            ),
            config_document={"mode": "test"},
        )

    def tearDown(self):
        self.store.close()
        self.tempdir.cleanup()

    def test_export_distinguishes_post_ready_event_from_right_censor(self):
        start = dt.datetime(2026, 8, 10, 0, 0, tzinfo=UTC)
        planned_end = start + dt.timedelta(minutes=30)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        epoch_id = self.store.start_canary_epoch(
            self.run_id,
            cohort_name="runpod-spot-survival",
            sequence=0,
            cell=cell,
            planned_duration_seconds=1800,
            interval_seconds=30,
            selection_probability=1.0,
            selection_seed=17,
            details={"design": "matched_interruptible_control"},
        )
        self.store.mark_canary_epoch_started(epoch_id, planned_end.isoformat())
        spot_id = self.store.start_canary(
            self.run_id,
            cell,
            pair_id=epoch_id,
            interruptible=True,
            planned_end_at=planned_end.isoformat(),
        )
        control_id = self.store.start_canary(
            self.run_id,
            cell,
            pair_id=epoch_id,
            interruptible=False,
            planned_end_at=planned_end.isoformat(),
        )
        for lease_id, resource_id in ((spot_id, "spot-1"), (control_id, "control-1")):
            self.store.mark_canary_created(
                lease_id,
                provider_resource_id=resource_id,
                details={"price_usd_hr": 0.44},
            )
            self.store.connection.execute(
                "UPDATE canary_leases SET created_at=? WHERE id=?",
                (start.isoformat(), lease_id),
            )
            self.store.record_canary_observation(
                lease_id,
                provider_state="RUNNING",
                machine_id=resource_id,
                runtime_uptime_seconds=60,
                realized_price_usd_hr=0.44,
                provider_location="dc-1",
                heartbeat_ok=True,
                heartbeat_latency_ms=5,
                heartbeat_version="v1",
                observed_at=(start + dt.timedelta(minutes=1)).isoformat(),
            )

        self.store.finish_canary(
            spot_id,
            TerminalReason.PROVIDER_INTERRUPTION_CANDIDATE.value,
            event_at=(start + dt.timedelta(minutes=11)).isoformat(),
        )
        self.store.finish_canary(
            control_id,
            TerminalReason.RIGHT_CENSORED_PLANNED_END.value,
            event_at=planned_end.isoformat(),
        )
        self.store.mark_canary_cleaned(spot_id)
        self.store.mark_canary_cleaned(control_id)
        self.store.finish_canary_epoch(
            epoch_id, status="complete", stop_reason="pair_terminal"
        )

        rows = survival_rows(self.store, self.run_id)
        spot = next(row for row in rows if row["interruptible"] == 1)
        control = next(row for row in rows if row["interruptible"] == 0)
        self.assertEqual(spot["pair_established"], 1)
        self.assertEqual(spot["post_ready_interruption_event"], 1)
        self.assertEqual(spot["right_censored"], 0)
        self.assertAlmostEqual(spot["post_ready_exposure_seconds"], 600.0)
        self.assertEqual(control["post_ready_interruption_event"], 0)
        self.assertEqual(control["right_censored"], 1)

        output = self.root / "survival.csv"
        count = export_survival_csv(self.store, self.run_id, output)
        with output.open(newline="", encoding="utf-8") as handle:
            exported = list(csv.DictReader(handle))
        self.assertEqual(count, 2)
        self.assertEqual(len(exported), 2)
        self.assertIn("selection_probability", exported[0])

    def test_capacity_export_keeps_schedule_and_delivered_depth(self):
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        self.store.record_quote(
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
        epoch_id = self.store.schedule_capacity_epoch(
            self.run_id,
            cohort_name="runpod-depth",
            sequence=0,
            cell=cell,
            target_depth=1,
            scheduled_offset_seconds=300,
            scheduled_at="2026-08-10T00:05:00+00:00",
            selection_probability=1,
            selection_seed=23,
        )
        self.store.mark_capacity_epoch_started(
            epoch_id,
            trial_id=epoch_id,
            started_lag_seconds=2.5,
        )
        self.store.start_capacity_trial(
            self.run_id,
            cell,
            target_depth=1,
            trial_id=epoch_id,
        )
        replica_id = self.store.start_replica(epoch_id, stage_target=1, ordinal=0)
        self.store.mark_replica_created(
            replica_id,
            "pod-1",
            0.44,
            provider_location="dc-1",
        )
        self.store.mark_replica_observed(
            replica_id,
            provider_ready=True,
            application_ready=True,
            realized_price_usd_hr=0.44,
        )
        self.store.mark_replica_cleaned(replica_id)
        self.store.finish_capacity_trial(epoch_id, status="complete")
        self.store.finish_capacity_epoch(
            epoch_id,
            status="complete",
            stop_reason="trial_terminal",
        )

        rows = capacity_depth_rows(self.store, self.run_id)

        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["cohort_name"], "runpod-depth")
        self.assertEqual(rows[0]["stock_status"], "High")
        self.assertEqual(rows[0]["application_ready_depth"], 1)
        self.assertEqual(rows[0]["depth_success"], 1)
        self.assertEqual(rows[0]["started_lag_seconds"], 2.5)

        output = self.root / "depth.csv"
        count = export_capacity_depth_csv(self.store, self.run_id, output)
        with output.open(newline="", encoding="utf-8") as handle:
            exported = list(csv.DictReader(handle))
        self.assertEqual(count, 1)
        self.assertEqual(len(exported), 1)
        self.assertIn("scheduled_at", exported[0])

    def test_capacity_payload_separates_instance_shape_from_total_gpu_depth(self):
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
            gpu_count=4,
        )
        trial_id = self.store.start_capacity_trial(
            self.run_id,
            cell,
            target_depth=32,
        )
        replica_id = self.store.start_replica(
            trial_id,
            stage_target=4,
            ordinal=0,
        )
        self.store.mark_replica_created(
            replica_id,
            "pod-1",
            1.2,
            details={
                "create_request": {"gpuCount": 4, "gpuTypeIds": ["NVIDIA A40"]},
                "create_response": {"id": "pod-1", "costPerHr": 1.2},
            },
        )
        self.store.mark_replica_observed(
            replica_id,
            provider_ready=True,
            application_ready=True,
            details={"last_observation": {"desiredStatus": "RUNNING"}},
        )
        self.store.mark_replica_cleaned(replica_id)
        self.store.finish_capacity_trial(trial_id, status="depth_boundary")

        payload = capacity_trial_payload(self.store, trial_id)

        self.assertEqual(payload["trial"]["gpu_count_per_instance"], 4)
        self.assertEqual(payload["trial"]["target_gpu_depth"], 32)
        self.assertEqual(payload["trial"]["target_instance_count"], 8)
        self.assertEqual(payload["trial"]["requested_instance_count"], 1)
        self.assertEqual(payload["trial"]["requested_gpu_depth"], 4)
        self.assertEqual(payload["stages"][0]["target_gpu_depth"], 4)
        self.assertEqual(payload["stages"][0]["target_instance_count"], 1)
        self.assertEqual(
            payload["stages"][0]["attempts"][0]["provider_resource_id"],
            "pod-1",
        )
        attempt_details = payload["stages"][0]["attempts"][0]["details"]
        self.assertEqual(attempt_details["create_request"]["gpuCount"], 4)
        self.assertEqual(attempt_details["create_response"]["id"], "pod-1")
        self.assertEqual(
            attempt_details["last_observation"]["desiredStatus"],
            "RUNNING",
        )

    def test_export_includes_missed_windows_without_canary_leases(self):
        start = dt.datetime(2026, 8, 10, tzinfo=UTC)
        run_id, _created = self.store.ensure_stability_experiment(
            ExperimentConfig(
                protocol_version="4.0-stability",
                host_id="host",
                vantage_id="vantage",
                provider_budgets={"runpod": 5.0},
                total_budget_usd=5.0,
                max_active_gpus_per_provider=2,
            ),
            {"mode": "stability"},
            experiment_id="stability-analysis",
            cohort_name="runpod-survival",
            started_at=start.isoformat(),
            windows=[
                {
                    "sequence": 0,
                    "scheduled_offset_seconds": 0,
                    "scheduled_at": start.isoformat(),
                    "scheduled_end_at": (start + dt.timedelta(hours=1)).isoformat(),
                },
                {
                    "sequence": 1,
                    "scheduled_offset_seconds": 7200,
                    "scheduled_at": (start + dt.timedelta(hours=2)).isoformat(),
                    "scheduled_end_at": (start + dt.timedelta(hours=3)).isoformat(),
                },
            ],
        )
        claim_status, _window = self.store.claim_due_stability_window(
            "stability-analysis",
            due_at=(start + dt.timedelta(hours=1, minutes=30)).isoformat(),
        )

        rows = stability_window_rows(self.store, run_id)
        output = self.root / "stability-windows.csv"
        count = export_stability_window_csv(self.store, run_id, output)
        with output.open(newline="", encoding="utf-8") as handle:
            exported = list(csv.DictReader(handle))

        self.assertEqual(claim_status, "idle")
        self.assertEqual(len(rows), 2)
        self.assertEqual(rows[0]["experiment_id"], "stability-analysis")
        self.assertEqual(rows[0]["window_status"], "missed")
        self.assertEqual(rows[0]["canary_epoch_count"], 0)
        self.assertEqual(rows[0]["lease_count"], 0)
        self.assertEqual(rows[1]["window_status"], "scheduled")
        self.assertEqual(count, 2)
        self.assertEqual(exported[0]["experiment_id"], "stability-analysis")
        self.assertEqual(exported[0]["window_stop_reason"], "external_scheduler_late")


if __name__ == "__main__":
    unittest.main()
