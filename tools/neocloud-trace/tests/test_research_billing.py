import pathlib
import sys
import tempfile
import unittest


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace.analysis.billing import import_billing_file, reconcile_costs
from neocloud_trace.model import ExperimentConfig, TrialCell
from neocloud_trace.providers import ProviderBillingRecord
from neocloud_trace.storage import TraceStore


class BillingReconciliationTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.store = TraceStore(pathlib.Path(self.tempdir.name) / "trace.db")
        self.run_id = self.store.start_run(
            ExperimentConfig(
                protocol_version="3.0-billing",
                host_id="host",
                vantage_id="vantage",
                provider_budgets={"runpod": 5},
                total_budget_usd=5,
                max_active_gpus_per_provider=1,
            ),
            config_document={"mode": "test"},
        )

    def tearDown(self):
        self.store.close()
        self.tempdir.cleanup()

    def test_reconciles_billed_amount_to_capacity_resource(self):
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA A40",
            gpu_type="A40",
            region=None,
            resource_class="secure",
            market="on-demand",
        )
        trial_id = self.store.start_capacity_trial(self.run_id, cell, target_gpu_depth=1)
        replica_id = self.store.start_replica(trial_id, stage_target=1, ordinal=0)
        self.store.mark_replica_created(replica_id, "pod-1", 0.3)
        self.store.connection.execute(
            "UPDATE capacity_replicas SET accepted_at=?, cleaned_at=? WHERE id=?",
            (
                "2026-08-10T00:00:00+00:00",
                "2026-08-10T01:00:00+00:00",
                replica_id,
            ),
        )
        self.store.record_provider_billing(
            self.run_id,
            ProviderBillingRecord(
                provider="runpod",
                provider_resource_id="pod-1",
                period_start="2026-08-10T00:00:00Z",
                amount_usd=0.35,
                billed_duration_ms=3600000,
                source="runpod_billing_api",
                source_record_id="pod-1-hour-0",
            ),
        )

        rows = reconcile_costs(self.store, self.run_id)

        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["owner_type"], "capacity_replica")
        self.assertEqual(rows[0]["owner_id"], replica_id)
        self.assertAlmostEqual(rows[0]["estimated_amount_usd"], 0.3)
        self.assertAlmostEqual(rows[0]["billed_amount_usd"], 0.35)
        self.assertAlmostEqual(rows[0]["variance_usd"], 0.05)
        self.assertEqual(rows[0]["status"], "reconciled")

        persisted = self.store.cost_reconciliation_rows(self.run_id)
        self.assertEqual(len(persisted), 1)
        self.assertEqual(persisted[0]["provider_resource_id"], "pod-1")

    def test_imports_manual_invoice_csv_without_resource_id(self):
        path = pathlib.Path(self.tempdir.name) / "lambda-invoice.csv"
        path.write_text(
            "period_start,amount_usd,currency,description\n"
            "2026-08-10T00:00:00Z,4.50,USD,weekly ODC usage\n",
            encoding="utf-8",
        )

        count = import_billing_file(
            self.store,
            self.run_id,
            provider="lambda",
            path=path,
            source="lambda_weekly_invoice",
        )

        self.assertEqual(count, 1)
        row = self.store.provider_billing_rows(self.run_id)[0]
        self.assertIsNone(row["provider_resource_id"])
        self.assertEqual(row["amount_usd"], 4.5)
        self.assertEqual(row["source"], "lambda_weekly_invoice")

        reconciled = reconcile_costs(self.store, self.run_id)
        aggregate = next(
            item for item in reconciled if item["status"] == "aggregate_unattributed"
        )
        self.assertEqual(aggregate["provider"], "lambda")
        self.assertEqual(aggregate["billed_amount_usd"], 4.5)


if __name__ == "__main__":
    unittest.main()
