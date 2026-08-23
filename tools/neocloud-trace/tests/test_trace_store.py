import json
import datetime as dt
import pathlib
import sqlite3
import sys
import tempfile
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.model import CatalogQuote, ExperimentConfig, TrialCell
from neocloud_trace.providers import ProviderBillingRecord
from neocloud_trace.storage import TraceStore


class TraceStoreTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.db_path = pathlib.Path(self.tempdir.name) / "trace.db"
        self.store = TraceStore(self.db_path)

    def tearDown(self):
        self.store.close()
        self.tempdir.cleanup()

    def config(self):
        return ExperimentConfig(
            protocol_version="2.0",
            host_id="test-host",
            vantage_id="test-vantage",
            provider_budgets={"lambda": 2.5, "runpod": 3.0},
            total_budget_usd=5.5,
            max_active_gpus_per_provider=8,
        )

    def test_creates_all_v2_tables_and_run_metadata(self):
        run_id = self.store.start_run(self.config(), config_document={"mode": "test"})

        tables = {
            row[0]
            for row in self.store.connection.execute(
                "SELECT name FROM sqlite_master WHERE type='table'"
            )
        }
        self.assertTrue(
            {
                "experiment_runs",
                "host_observations",
                "provider_api_observations_v2",
                "catalog_quotes_v2",
                "capacity_trials",
                "capacity_replicas",
                "canary_leases",
                "canary_observations",
                "cleanup_events",
                "budget_ledger_v2",
            }.issubset(tables)
        )
        row = self.store.connection.execute(
            "SELECT protocol_version, host_id, vantage_id, status FROM experiment_runs WHERE id=?",
            (run_id,),
        ).fetchone()
        self.assertEqual(tuple(row), ("2.0", "test-host", "test-vantage", "running"))

    def test_redacts_nested_secrets_before_persisting_raw_provider_data(self):
        run_id = self.store.start_run(self.config(), config_document={"mode": "test"})
        quote = CatalogQuote(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            gpu_count=1,
            resource_class="secure",
            market="on-demand",
            quoted_price_usd_hr=0.44,
            raw={
                "api_key": "top-secret",
                "nested": {
                    "Authorization": "Bearer hidden",
                    "jupyter_url": "https://secret.example/token",
                    "safe": "preserved",
                },
                "env": {
                    "PUBLIC_KEY": "not-secret",
                    "PASSWORD": "provider-secret",
                },
            },
        )

        self.store.record_quote(run_id, quote)

        raw_json = self.store.connection.execute(
            "SELECT raw_json FROM catalog_quotes_v2 WHERE run_id=?", (run_id,)
        ).fetchone()[0]
        persisted = json.loads(raw_json)
        self.assertEqual(persisted["api_key"], "[REDACTED]")
        self.assertEqual(persisted["nested"]["Authorization"], "[REDACTED]")
        self.assertEqual(persisted["nested"]["jupyter_url"], "[REDACTED]")
        self.assertEqual(persisted["nested"]["safe"], "preserved")
        self.assertEqual(persisted["env"], "[REDACTED]")
        self.assertNotIn("top-secret", raw_json)
        self.assertNotIn("Bearer hidden", raw_json)
        self.assertNotIn("provider-secret", raw_json)

    def test_tracks_budget_and_unfinished_provider_ownership(self):
        run_id = self.store.start_run(self.config(), config_document={"mode": "test"})
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        trial_id = self.store.start_capacity_trial(run_id, cell, target_depth=8)
        replica_id = self.store.start_replica(trial_id, stage_target=1, ordinal=0)
        self.store.mark_replica_created(
            replica_id,
            provider_resource_id="pod-123",
            realized_price_usd_hr=0.44,
        )
        self.store.record_budget(
            run_id,
            provider="runpod",
            amount_usd=0.12,
            kind="capacity-trial",
            reference=replica_id,
            details={"api_key": "hidden"},
        )

        self.assertAlmostEqual(self.store.provider_spend(run_id, "runpod"), 0.12)
        owned = self.store.owned_resources(run_id, provider="runpod")
        self.assertEqual(len(owned), 1)
        self.assertEqual(owned[0].provider_resource_id, "pod-123")

        self.store.mark_replica_cleaned(replica_id)
        self.assertEqual(self.store.owned_resources(run_id, provider="runpod"), [])

    def test_capacity_campaign_claim_is_idempotent_across_store_connections(self):
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        now = dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc)
        epochs = [
            {
                "sequence": 0,
                "cell": cell,
                "target_depth": 1,
                "scheduled_offset_seconds": 0,
                "scheduled_at": now.isoformat(),
                "selection_probability": 1,
                "selection_seed": 7,
            }
        ]
        first_run, first_created = self.store.ensure_capacity_campaign(
            self.config(),
            {"mode": "capacity-tick"},
            campaign_id="shared-campaign",
            cohort_name="depth",
            started_at=now.isoformat(),
            epochs=epochs,
        )
        other = TraceStore(self.db_path)
        self.addCleanup(other.close)
        second_run, second_created = other.ensure_capacity_campaign(
            self.config(),
            {"mode": "capacity-tick"},
            campaign_id="shared-campaign",
            cohort_name="depth",
            started_at=now.isoformat(),
            epochs=epochs,
        )

        first_status, first_epoch = self.store.claim_due_capacity_epoch(
            "shared-campaign",
            due_at=now.isoformat(),
            expired_before=(now - dt.timedelta(minutes=1)).isoformat(),
        )
        second_status, second_epoch = other.claim_due_capacity_epoch(
            "shared-campaign",
            due_at=now.isoformat(),
            expired_before=(now - dt.timedelta(minutes=1)).isoformat(),
        )

        self.assertTrue(first_created)
        self.assertFalse(second_created)
        self.assertEqual(first_run, second_run)
        self.assertEqual(first_status, "claimed")
        self.assertEqual(second_status, "busy")
        self.assertEqual(first_epoch["id"], second_epoch["id"])

    def test_persists_completion_window_assignment_and_decision(self):
        run_id = self.store.start_run(self.config(), config_document={"mode": "test"})
        cell = TrialCell(
            provider="lambda",
            sku="gpu_1x_a10",
            gpu_type="A10",
            region="us-east-1",
            resource_class="on-demand",
            market="on-demand",
        )
        quote = CatalogQuote(
            provider="lambda",
            sku="gpu_1x_a10",
            gpu_type="A10",
            gpu_count=1,
            region="us-east-1",
            resource_class="on-demand",
            market="on-demand",
            quoted_price_usd_hr=0.6,
            available=True,
        )
        release_quote_id = self.store.record_quote(run_id, quote)
        decision_quote_id = self.store.record_quote(run_id, quote)

        epoch_id = self.store.schedule_completion_window_epoch(
            run_id,
            cohort_name="window-test",
            sequence=0,
            cell=cell,
            target_depth=1,
            arm_name="lazy",
            launch_fraction=1,
            assignment_probability=0.5,
            assignment_seed=17,
            release_offset_seconds=10,
            release_at="2026-08-10T00:00:10+00:00",
            allocation_deadline_at="2026-08-10T00:01:00+00:00",
            planned_launch_at="2026-08-10T00:00:50+00:00",
        )
        self.store.mark_completion_window_released(epoch_id, release_quote_id)
        self.store.mark_completion_window_decided(
            epoch_id,
            decision_quote_id=decision_quote_id,
            decision_lag_seconds=0.25,
            capacity_epoch_id="capacity-epoch-1",
        )
        self.store.finish_completion_window_epoch(
            epoch_id,
            status="complete",
            stop_reason="trial_terminal",
            provider_sla_met=True,
            application_sla_met=None,
        )

        row = self.store.completion_window_epoch_rows(run_id)[0]
        self.assertEqual(row["arm_name"], "lazy")
        self.assertEqual(row["release_quote_id"], release_quote_id)
        self.assertEqual(row["decision_quote_id"], decision_quote_id)
        self.assertEqual(row["capacity_epoch_id"], "capacity-epoch-1")
        self.assertEqual(row["provider_sla_met"], 1)
        self.assertIsNone(row["application_sla_met"])
        self.assertEqual(row["status"], "complete")

    def test_provider_billing_import_is_idempotent_and_resource_attributable(self):
        run_id = self.store.start_run(self.config(), config_document={"mode": "test"})
        record = ProviderBillingRecord(
            provider="runpod",
            provider_resource_id="pod-billed-1",
            period_start="2026-08-10T00:00:00Z",
            amount_usd=0.42,
            billed_duration_ms=3600000,
            source="runpod_billing_api",
            source_record_id="billing-record-1",
            gpu_type="NVIDIA A40",
            disk_space_billed_gb=20,
            raw={"amount": 0.42, "token": "must-redact"},
        )

        first = self.store.record_provider_billing(run_id, record)
        second = self.store.record_provider_billing(run_id, record)

        self.assertEqual(first, second)
        rows = self.store.provider_billing_rows(run_id)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["provider_resource_id"], "pod-billed-1")
        self.assertEqual(rows[0]["amount_usd"], 0.42)
        self.assertNotIn("must-redact", rows[0]["raw_json"])

    def test_rejects_invalid_experiment_limits(self):
        with self.assertRaisesRegex(ValueError, "max_active_gpus_per_provider"):
            ExperimentConfig(
                protocol_version="2.0",
                host_id="test-host",
                vantage_id="test-vantage",
                provider_budgets={"runpod": 3.0},
                total_budget_usd=3.0,
                max_active_gpus_per_provider=0,
            )


if __name__ == "__main__":
    unittest.main()
