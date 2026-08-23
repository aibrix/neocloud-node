import contextlib
import io
import json
import datetime as dt
import pathlib
import random
import socket
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock


TRACE_ROOT = pathlib.Path(__file__).resolve().parents[1]
sys.path.insert(0, str(TRACE_ROOT / "src"))

from neocloud_trace import cli as trace_cli
from neocloud_trace.model import CatalogQuote, TrialCell
from neocloud_trace.providers import (
    CatalogFetch,
    CreateResult,
    DeleteResult,
    HTTPResponse,
    ResourceObservation,
)
from neocloud_trace.storage import TraceStore
from neocloud_trace.supervisor import (
    ActiveRunDisabled,
    BudgetExceeded,
    BudgetGuard,
    PilotSupervisor,
    load_plan,
)


def plan_document():
    return {
        "protocol_version": "2.0",
        "host_id": "test-host",
        "vantage_id": "test-vantage",
        "duration_seconds": 0.15,
        "passive_interval_seconds": 0.01,
        "host_interval_seconds": 0.01,
        "canary_interval_seconds": 0.01,
        "max_active_gpus_per_provider": 8,
        "provider_budgets_usd": {"lambda": 1.0},
        "total_budget_usd": 1.0,
        "capacity_trials": [
            {
                "provider": "lambda",
                "sku": "gpu_1x_a10",
                "gpu_type": "A10",
                "region": "us-east-1",
                "resource_class": "on-demand",
                "market": "on-demand",
                "target_depth": 1,
                "ready_timeout_seconds": 1,
                "max_lease_seconds": 60,
                "estimated_price_usd_hr": 0.6,
                "require_application_ready": False,
            }
        ],
        "canary_trials": [],
    }


def stability_plan_document():
    document = plan_document()
    document.update(
        {
            "protocol_version": "3.0-stability",
            "duration_seconds": 0.2,
            "max_active_gpus_per_provider": 2,
            "capacity_trials": [],
            "capacity_cohorts": [],
            "completion_window_cohorts": [],
            "canary_trials": [],
            "control_cohorts": [],
            "canary_cohorts": [
                {
                    "name": "lambda-stability",
                    "max_pairs": 1,
                    "selection_seed": 17,
                    "replacement_delay_seconds": 0,
                    "min_remaining_seconds": 0.01,
                    "cells": [
                        {
                            "provider": "lambda",
                            "sku": "gpu_1x_a10",
                            "gpu_type": "A10",
                            "region": "us-east-1",
                            "resource_class": "on-demand",
                            "market": "on-demand",
                            "duration_seconds": 0.05,
                            "interval_seconds": 0.001,
                            "interruptible_price_usd_hr": 0.3,
                            "on_demand_price_usd_hr": 0.3,
                            "weight": 1,
                        }
                    ],
                }
            ],
        }
    )
    return document


class CountingCatalogProvider:
    provider = "lambda"

    def __init__(self):
        self.calls = 0

    def catalog_fetches(self, gpu_counts=(1,)):
        self.calls += 1
        quote = CatalogQuote(
            provider="lambda",
            sku="gpu_1x_a10",
            gpu_type="A10",
            gpu_count=1,
            resource_class="on-demand",
            market="on-demand",
            quoted_price_usd_hr=0.6,
            region="us-east-1",
            available=True,
        )
        return [CatalogFetch("lambda", "instance-types", HTTPResponse(200, 1, {}), (quote,))]


class FastFailingCanaryProvider:
    provider = "runpod"

    def __init__(self):
        self.created = []

    def catalog_fetches(self, gpu_counts=(1,)):
        return []

    def create_replicas(self, cell, requests):
        resource_id = f"pod-{len(self.created)}"
        self.created.append((resource_id, cell.market))
        return [CreateResult(True, resource_id, price_usd_hr=0.3)]

    def observe(self, resource_id, cell):
        return ResourceObservation(
            exists=True,
            provider_state="PREEMPTED" if cell.market == "interruptible" else "RUNNING",
            provider_ready=cell.market != "interruptible",
            application_ready=cell.market != "interruptible",
            machine_id=resource_id,
            runtime_uptime_seconds=1,
            price_usd_hr=0.3,
        )

    def delete(self, resource_id, cell):
        return DeleteResult(True)

    def verify_deleted(self, resource_id, cell):
        return DeleteResult(True, missing=True)


class SpotLaunchFailProvider(FastFailingCanaryProvider):
    def __init__(self):
        super().__init__()
        self.observed = []

    def create_replicas(self, cell, requests):
        if cell.market == "interruptible":
            return [CreateResult(False)]
        return super().create_replicas(cell, requests)

    def observe(self, resource_id, cell):
        self.observed.append(resource_id)
        return super().observe(resource_id, cell)


class CleanupFailProvider(FastFailingCanaryProvider):
    def delete(self, resource_id, cell):
        return DeleteResult(False)

    def verify_deleted(self, resource_id, cell):
        raise AssertionError("failed deletes must not be verified")


class ReadyCapacityProvider:
    provider = "runpod"

    def __init__(self):
        self.created = []

    def catalog_fetches(self, gpu_counts=(1,)):
        return []

    def create_replicas(self, cell, requests):
        results = []
        for request in requests:
            resource_id = f"depth-{len(self.created)}"
            self.created.append(resource_id)
            results.append(CreateResult(True, resource_id, price_usd_hr=0.3))
        return results

    def observe(self, resource_id, cell):
        return ResourceObservation(
            exists=True,
            provider_state="RUNNING",
            provider_ready=True,
            application_ready=True,
            machine_id=resource_id,
            runtime_uptime_seconds=1,
            price_usd_hr=0.3,
        )

    def delete(self, resource_id, cell):
        return DeleteResult(True)

    def verify_deleted(self, resource_id, cell):
        return DeleteResult(True, missing=True)


class BlockingCapacityRunner:
    entered = threading.Event()
    release = threading.Event()

    def __init__(self, *args, **kwargs):
        pass

    def run_trial(self, run_id, cell, target_depth=8):
        self.entered.set()
        self.release.wait(timeout=1)
        return None


class ResearchSupervisorTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tempdir.name)
        self.plan_path = self.root / "plan.json"
        self.plan_path.write_text(json.dumps(plan_document()), encoding="utf-8")

    def tearDown(self):
        BlockingCapacityRunner.entered.clear()
        BlockingCapacityRunner.release.clear()
        self.tempdir.cleanup()

    def test_load_plan_requires_positive_explicit_budgets(self):
        document = plan_document()
        document["provider_budgets_usd"] = {}
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        with self.assertRaises(ValueError):
            load_plan(self.plan_path)

    def test_capacity_plan_accepts_explicit_d32_gpu_depth_and_shape(self):
        document = plan_document()
        document.update(
            {
                "max_active_gpus_per_provider": 32,
                "provider_budgets_usd": {"runpod": 20.0},
                "total_budget_usd": 20.0,
                "capacity_trials": [
                    {
                        "provider": "runpod",
                        "sku": "NVIDIA A40",
                        "gpu_type": "A40",
                        "resource_class": "secure",
                        "market": "on-demand",
                        "gpu_count": 4,
                        "target_gpu_depth": 32,
                        "ready_timeout_seconds": 600,
                        "max_lease_seconds": 900,
                        "estimated_price_usd_hr": 3.0,
                        "require_application_ready": True,
                        "settlement_safety_multiplier": 1.15,
                    }
                ],
                "canary_trials": [],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")

        plan = load_plan(self.plan_path)
        trial = plan.capacity_trials[0]

        self.assertEqual(trial.target_depth, 32)
        self.assertEqual(trial.cell.gpu_count, 4)
        self.assertEqual(trial.target_instance_count, 8)
        self.assertAlmostEqual(trial.budget_upper_bound_usd, 6.9)

    def test_canary_budget_applies_billing_floor_and_safety_multiplier(self):
        document = plan_document()
        document["capacity_trials"] = []
        document["canary_trials"] = [
            {
                "provider": "lambda",
                "sku": "gpu_1x_a10",
                "gpu_type": "A10",
                "region": "us-east-1",
                "resource_class": "on-demand",
                "market": "on-demand",
                "duration_seconds": 30,
                "interval_seconds": 1,
                "interruptible_price_usd_hr": 0.3,
                "on_demand_price_usd_hr": 0.3,
                "billing_minimum_seconds": 60,
                "settlement_safety_multiplier": 1.2,
            }
        ]
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")

        trial = load_plan(self.plan_path).canary_trials[0]

        self.assertAlmostEqual(trial.budget_upper_bound_usd, 0.012)

        document = plan_document()
        document["max_active_gpus_per_provider"] = 33
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        with self.assertRaises(ValueError):
            load_plan(self.plan_path)

    def test_active_plan_cannot_run_without_explicit_enable_flag(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        supervisor = PilotSupervisor(store, providers={})

        with self.assertRaises(ActiveRunDisabled):
            supervisor.run(plan, enable_active=False)

    def test_cli_marks_every_billable_command_as_active(self):
        module = trace_cli
        parser = module.build_parser()
        database = str(self.root / "trace.db")
        plan = str(self.plan_path)

        for command in ("run", "tick"):
            args = parser.parse_args(
                [command, "--db", database, "--plan", plan]
            )
            with self.assertRaises(ActiveRunDisabled):
                module.require_active(args)

        for command in ("cleanup", "recover"):
            args = parser.parse_args([command, "--db", database])
            with self.assertRaises(ActiveRunDisabled):
                module.require_active(args)

        collect = parser.parse_args(
            ["collect", "--db", database, "--provider", "runpod"]
        )
        module.require_active(collect)
        export = parser.parse_args(
            [
                "export",
                "survival",
                "--db",
                database,
                "--output",
                str(self.root / "survival.csv"),
            ]
        )
        module.require_active(export)
        sync_billing = parser.parse_args(
            [
                "billing",
                "sync",
                "--db",
                database,
                "--provider",
                "runpod",
            ]
        )
        module.require_active(sync_billing)
        reconcile = parser.parse_args(
            ["billing", "reconcile", "--db", database]
        )
        module.require_active(reconcile)

        identified = parser.parse_args(
            [
                "run",
                "--db",
                database,
                "--plan",
                plan,
                "--host-id",
                "stable-host-1",
                "--vantage-id",
                "us-west-office",
                "--enable-active",
            ]
        )
        self.assertEqual(identified.host_id, "stable-host-1")
        self.assertEqual(identified.vantage_id, "us-west-office")

    def test_cli_help_keeps_operational_options_with_their_command(self):
        parser = trace_cli.build_parser()
        top_level_help = parser.format_help()

        self.assertIn("run", top_level_help)
        self.assertIn("tick", top_level_help)
        self.assertNotIn("capacity-once", top_level_help)
        self.assertNotIn("capacity-tick", top_level_help)

        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit):
            parser.parse_args(["run", "--help"])
        run_help = output.getvalue()
        for option in (
            "--db",
            "--plan",
            "--host-id",
            "--vantage-id",
            "--enable-active",
            "--target",
            "--index",
        ):
            self.assertIn(option, run_help)

        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit):
            parser.parse_args(["collect", "--help"])
        collect_help = output.getvalue()
        self.assertIn("--provider", collect_help)
        self.assertIn("--vantage-id", collect_help)
        self.assertNotIn("--plan", collect_help)
        self.assertNotIn("--host-id", collect_help)

    def test_cli_metadata_has_one_resolution_rule_for_every_plan_command(self):
        document = plan_document()
        document["host_id"] = "replace-with-stable-host-id"
        document["vantage_id"] = "replace-with-network-vantage"
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        parser = trace_cli.build_parser()
        database = str(self.root / "trace.db")

        args = parser.parse_args(
            [
                "init",
                "--db",
                database,
                "--plan",
                str(self.plan_path),
                "--vantage-id",
                "us-west-office",
            ]
        )
        result = trace_cli.execute(args)
        self.assertEqual(result["host_id"], socket.gethostname())
        self.assertEqual(result["vantage_id"], "us-west-office")

        unresolved = parser.parse_args(
            ["init", "--db", database, "--plan", str(self.plan_path)]
        )
        with self.assertRaisesRegex(ValueError, "vantage_id must identify"):
            trace_cli.execute(unresolved)

    def test_tick_cli_uses_plan_interface_and_requires_active_opt_in(self):
        module = trace_cli

        args = module.build_parser().parse_args(
            [
                "tick",
                "--db",
                str(self.root / "capacity.db"),
                "--plan",
                str(self.plan_path),
            ]
        )

        self.assertEqual(args.command, "tick")
        self.assertEqual(args.plan, str(self.plan_path))
        self.assertEqual(args.db, str(self.root / "capacity.db"))
        self.assertIsNone(args.campaign_id)
        self.assertEqual(args.index, 0)
        with self.assertRaises(ActiveRunDisabled):
            module.require_active(args)

    def test_capacity_cli_auto_identifies_host_and_reuses_database_experiment(self):
        document = plan_document()
        document.update(
            {
                "host_id": "replace-with-stable-host-id",
                "capacity_trials": [],
                "canary_trials": [],
                "capacity_cohorts": [
                    {
                        "name": "atomic-depth",
                        "max_trials": 1,
                        "selection_seed": 23,
                        "initial_delay_seconds": 0,
                        "interval_seconds": 60,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "lambda",
                                "sku": "gpu_1x_a10",
                                "gpu_type": "A10",
                                "region": "us-east-1",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.6,
                                "require_application_ready": False,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        module = trace_cli
        db_path = self.root / "capacity.db"

        provider = ReadyCapacityProvider()
        provider.provider = "lambda"

        def invoke(plan_path):
            args = module.build_parser().parse_args(
                [
                    "tick",
                    "--db",
                    str(db_path),
                    "--plan",
                    str(plan_path),
                    "--enable-active",
                ]
            )
            with mock.patch.object(
                module,
                "_provider_clients",
                return_value={"lambda": provider},
            ):
                result = module.execute(args)
            return result

        first = invoke(self.plan_path)
        renamed = self.root / "renamed-experiment.json"
        renamed.write_text(self.plan_path.read_text(encoding="utf-8"), encoding="utf-8")
        second = invoke(renamed)

        self.assertEqual(first["status"], "executed")
        self.assertTrue(first["campaign_created"])
        self.assertTrue(first["campaign_complete"])
        self.assertEqual(first["cohort_name"], "atomic-depth")
        self.assertEqual(first["campaign_id"], "capacity-experiment")
        self.assertEqual(second["status"], "complete")
        self.assertFalse(second["campaign_created"])
        self.assertEqual(first["run_id"], second["run_id"])
        self.assertEqual(len(provider.created), 1)
        store = TraceStore(db_path)
        self.addCleanup(store.close)
        run = store.run_row(first["run_id"])
        self.assertEqual(run["host_id"], socket.gethostname())
        self.assertEqual(run["vantage_id"], "test-vantage")

        changed = json.loads(renamed.read_text(encoding="utf-8"))
        changed["capacity_cohorts"][0]["selection_seed"] = 99
        renamed.write_text(json.dumps(changed), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "different experiment"):
            invoke(renamed)

    def test_tick_cli_requires_a_capacity_cohort(self):
        document = plan_document()
        document.update(
            {
                "capacity_trials": [],
                "canary_trials": [],
                "capacity_cohorts": [],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        module = trace_cli
        args = module.build_parser().parse_args(
            [
                "tick",
                "--db",
                str(self.root / "capacity.db"),
                "--plan",
                str(self.plan_path),
                "--enable-active",
            ]
        )

        with self.assertRaisesRegex(ValueError, "exactly one capacity_cohorts"):
            module.execute(args)

    def test_tick_contract_rejects_legacy_trials_before_db_or_provider(self):
        self.plan_path.write_text(json.dumps(plan_document()), encoding="utf-8")
        db_path = self.root / "invalid-capacity.db"
        args = trace_cli.build_parser().parse_args(
            [
                "tick",
                "--db",
                str(db_path),
                "--plan",
                str(self.plan_path),
                "--enable-active",
            ]
        )

        with mock.patch.object(trace_cli, "_provider_clients") as clients:
            with self.assertRaisesRegex(ValueError, "capacity_trials=1"):
                trace_cli.execute(args)

        clients.assert_not_called()
        self.assertFalse(db_path.exists())

    def test_tick_contract_requires_one_trial_in_its_only_cohort(self):
        document = plan_document()
        document.update(
            {
                "capacity_trials": [],
                "canary_trials": [],
                "capacity_cohorts": [
                    {
                        "name": "invalid-batch",
                        "max_trials": 2,
                        "selection_seed": 23,
                        "interval_seconds": 60,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "lambda",
                                "sku": "gpu_1x_a10",
                                "gpu_type": "A10",
                                "region": "us-east-1",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.6,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        db_path = self.root / "invalid-max-trials.db"
        args = trace_cli.build_parser().parse_args(
            [
                "tick",
                "--db",
                str(db_path),
                "--plan",
                str(self.plan_path),
                "--enable-active",
            ]
        )

        with self.assertRaisesRegex(ValueError, "max_trials=1"):
            trace_cli.execute(args)

        self.assertFalse(db_path.exists())

    def test_run_contract_rejects_mixed_plan_before_db_or_provider(self):
        document = stability_plan_document()
        document["capacity_trials"] = plan_document()["capacity_trials"]
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        db_path = self.root / "invalid-stability.db"
        args = trace_cli.build_parser().parse_args(
            [
                "run",
                "--db",
                str(db_path),
                "--plan",
                str(self.plan_path),
                "--enable-active",
            ]
        )

        with mock.patch.object(trace_cli, "_provider_clients") as clients:
            with self.assertRaisesRegex(ValueError, "capacity_trials=1"):
                trace_cli.execute(args)

        clients.assert_not_called()
        self.assertFalse(db_path.exists())

    def test_run_contract_keeps_only_the_stability_target_alias(self):
        self.plan_path.write_text(
            json.dumps(stability_plan_document()),
            encoding="utf-8",
        )
        db_path = self.root / "wrong-target.db"
        args = trace_cli.build_parser().parse_args(
            [
                "run",
                "--db",
                str(db_path),
                "--plan",
                str(self.plan_path),
                "--target",
                "capacity",
                "--enable-active",
            ]
        )

        with self.assertRaisesRegex(ValueError, "all or canary-cohort"):
            trace_cli.execute(args)

        self.assertFalse(db_path.exists())

    def test_init_reports_dynamic_cohort_pair_cost_bounds(self):
        document = plan_document()
        document.update(
            {
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [
                    {
                        "name": "cost-bounds",
                        "max_pairs": 10,
                        "selection_seed": 1,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "lambda",
                                "sku": "cheap",
                                "gpu_type": "A10",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "duration_seconds": 60,
                                "interruptible_price_usd_hr": 0.3,
                                "on_demand_price_usd_hr": 0.3,
                            },
                            {
                                "provider": "lambda",
                                "sku": "expensive",
                                "gpu_type": "A10",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "duration_seconds": 60,
                                "interruptible_price_usd_hr": 0.6,
                                "on_demand_price_usd_hr": 0.6,
                            },
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        module = trace_cli
        args = module.build_parser().parse_args(
            [
                "init",
                "--db",
                str(self.root / "trace.db"),
                "--plan",
                str(self.plan_path),
            ]
        )

        result = module.execute(args)

        self.assertEqual(result["static_planned_upper_bound_usd"], 0)
        self.assertAlmostEqual(result["canary_cohorts"][0]["min_pair_upper_bound_usd"], 0.01)
        self.assertAlmostEqual(result["canary_cohorts"][0]["max_pair_upper_bound_usd"], 0.02)
        self.assertEqual(result["total_budget_usd"], 1.0)

    def test_init_reports_complete_d32_capacity_cohort_cost_bound(self):
        plan_path = (
            TRACE_ROOT
            / "plans"
            / "research-capacity-fragmentation-d32-pilot.json"
        )
        module = trace_cli
        args = module.build_parser().parse_args(
            [
                "init",
                "--db",
                str(self.root / "d32.db"),
                "--plan",
                str(plan_path),
                "--vantage-id",
                "test-vantage",
            ]
        )

        result = module.execute(args)

        cohort = result["capacity_cohorts"][0]
        self.assertAlmostEqual(cohort["planned_cohort_upper_bound_usd"], 46.0)
        self.assertEqual(cohort["planned_provider_upper_bounds_usd"]["runpod"], 18.4)
        self.assertEqual(cohort["planned_provider_upper_bounds_usd"]["lambda"], 27.6)

    def test_show_depth_trial_cli_returns_shape_aware_payload(self):
        db_path = self.root / "payload.db"
        plan = load_plan(self.plan_path)
        store = TraceStore(db_path)
        run_id = store.start_run(plan.experiment, plan.raw_document)
        cell = TrialCell(
            provider="runpod",
            sku="NVIDIA RTX A4000",
            gpu_type="RTX A4000",
            region=None,
            resource_class="secure",
            market="on-demand",
            gpu_count=4,
        )
        trial_id = store.start_capacity_trial(run_id, cell, target_depth=32)
        replica_id = store.start_replica(trial_id, stage_target=4, ordinal=0)
        store.mark_replica_created(
            replica_id,
            "pod-1",
            details={"create_response": {"id": "pod-1"}},
        )
        store.close()

        module = trace_cli
        args = module.build_parser().parse_args(
            [
                "trial",
                "--db",
                str(db_path),
                "--trial-id",
                trial_id,
            ]
        )

        result = module.execute(args)

        self.assertEqual(result["schema_version"], "capacity-trial-payload/v1")
        self.assertEqual(result["trial"]["target_gpu_depth"], 32)
        self.assertEqual(result["trial"]["target_instance_count"], 8)
        self.assertEqual(result["stages"][0]["attempts"][0]["provider_resource_id"], "pod-1")

    def test_budget_guard_records_conservative_hold_and_rejects_overspend(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        run_id = store.start_run(plan.experiment, plan.raw_document)
        guard = BudgetGuard(store, run_id, plan.experiment)

        guard.reserve("lambda", 0.6, "capacity", "trial-1")

        self.assertAlmostEqual(store.provider_spend(run_id, "lambda"), 0.6)
        with self.assertRaises(BudgetExceeded):
            guard.reserve("lambda", 0.5, "capacity", "trial-2")

    def test_budget_settlement_releases_unused_commitment_for_replacement(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        run_id = store.start_run(plan.experiment, plan.raw_document)
        guard = BudgetGuard(store, run_id, plan.experiment)

        guard.reserve("lambda", 0.8, "canary", "pair-1")
        guard.settle("lambda", "pair-1", 0.2, details={"cleanup_verified": True})
        guard.settle("lambda", "pair-1", 0.2, details={"cleanup_verified": True})
        guard.reserve("lambda", 0.8, "canary", "pair-2")

        self.assertAlmostEqual(store.provider_spend(run_id, "lambda"), 1.0)
        settlements = store.connection.execute(
            "SELECT COUNT(*) FROM budget_ledger_v2 "
            "WHERE run_id=? AND reference='pair-1' AND kind='settled_runtime_estimate'",
            (run_id,),
        ).fetchone()[0]
        self.assertEqual(settlements, 1)

    def test_continuous_cohort_replaces_failed_pairs_until_max_pairs(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-survival-cohort",
                "duration_seconds": 0.5,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [
                    {
                        "name": "runpod-spot-survival",
                        "max_pairs": 3,
                        "selection_seed": 17,
                        "replacement_delay_seconds": 0,
                        "min_remaining_seconds": 0.01,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "duration_seconds": 60,
                                "interval_seconds": 0.001,
                                "interruptible_price_usd_hr": 0.3,
                                "on_demand_price_usd_hr": 0.3,
                                "weight": 1,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        provider = FastFailingCanaryProvider()
        supervisor = PilotSupervisor(store, providers={"runpod": provider})

        run_id = supervisor.run(plan, enable_active=True)

        epochs = store.connection.execute(
            "SELECT sequence, status, stop_reason FROM canary_epochs "
            "WHERE run_id=? ORDER BY sequence",
            (run_id,),
        ).fetchall()
        leases = store.connection.execute(
            "SELECT COUNT(*) FROM canary_leases WHERE run_id=?", (run_id,)
        ).fetchone()[0]
        self.assertEqual(len(epochs), 3)
        self.assertEqual([row["sequence"] for row in epochs], [0, 1, 2])
        self.assertTrue(all(row["status"] == "complete" for row in epochs))
        self.assertEqual(leases, 6)
        self.assertEqual(len(provider.created), 6)
        self.assertEqual(store.owned_resources(run_id), [])

    def test_failed_spot_launch_immediately_censors_and_cleans_control(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-survival-cohort",
                "duration_seconds": 0.2,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [
                    {
                        "name": "launch-failure",
                        "max_pairs": 1,
                        "selection_seed": 1,
                        "min_remaining_seconds": 0.01,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "duration_seconds": 60,
                                "interval_seconds": 0.01,
                                "interruptible_price_usd_hr": 0.3,
                                "on_demand_price_usd_hr": 0.3,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        provider = SpotLaunchFailProvider()

        run_id = PilotSupervisor(store, {"runpod": provider}).run(
            plan, enable_active=True
        )

        reasons = {
            row["interruptible"]: row["censor_reason"]
            for row in store.connection.execute(
                "SELECT interruptible, censor_reason FROM canary_leases WHERE run_id=?",
                (run_id,),
            ).fetchall()
        }
        self.assertEqual(reasons[1], "launch_failure")
        self.assertEqual(reasons[0], "pair_setup_failure")
        self.assertEqual(provider.observed, [])
        self.assertEqual(store.owned_resources(run_id), [])

    def test_continuous_cohort_stops_replacement_when_cleanup_is_unresolved(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-survival-cohort",
                "duration_seconds": 0.2,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [
                    {
                        "name": "cleanup-guard",
                        "max_pairs": 3,
                        "selection_seed": 1,
                        "min_remaining_seconds": 0.01,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "duration_seconds": 60,
                                "interval_seconds": 0.01,
                                "interruptible_price_usd_hr": 0.3,
                                "on_demand_price_usd_hr": 0.3,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)

        run_id = PilotSupervisor(store, {"runpod": CleanupFailProvider()}).run(
            plan, enable_active=True
        )

        epoch_count = store.connection.execute(
            "SELECT COUNT(*) FROM canary_epochs WHERE run_id=?", (run_id,)
        ).fetchone()[0]
        self.assertEqual(epoch_count, 1)
        self.assertEqual(len(store.owned_resources(run_id)), 2)
        self.assertEqual(store.run_row(run_id)["status"], "cleanup_incomplete")

    def test_recovery_right_censors_collector_loss_before_cleanup(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        run_id = store.start_run(plan.experiment, plan.raw_document)
        cell = TrialCell(
            provider="lambda",
            sku="gpu_1x_a10",
            gpu_type="A10",
            region="us-east-1",
            resource_class="on-demand",
            market="on-demand",
        )
        epoch_id = store.start_canary_epoch(
            run_id,
            cohort_name="recovery-test",
            sequence=0,
            cell=cell,
            planned_duration_seconds=600,
            interval_seconds=30,
            selection_probability=1,
            selection_seed=1,
        )
        store.mark_canary_epoch_started(epoch_id, "2026-08-10T01:00:00+00:00")
        lease_id = store.start_canary(
            run_id,
            cell,
            pair_id=epoch_id,
            interruptible=True,
            planned_end_at="2026-08-10T01:00:00+00:00",
        )
        store.mark_canary_created(lease_id, provider_resource_id="instance-1")
        legacy_lease_id = store.start_canary(
            run_id,
            cell,
            pair_id="legacy-pair-without-epoch",
            interruptible=False,
            planned_end_at="2026-08-10T01:00:00+00:00",
        )
        store.mark_canary_created(
            legacy_lease_id,
            provider_resource_id="instance-legacy",
        )
        capacity_epoch_id = store.schedule_capacity_epoch(
            run_id,
            cohort_name="recovery-depth",
            sequence=0,
            cell=cell,
            target_depth=1,
            scheduled_offset_seconds=0,
            scheduled_at="2026-08-10T00:00:00+00:00",
            selection_probability=1,
            selection_seed=1,
        )
        store.mark_capacity_epoch_started(
            capacity_epoch_id,
            trial_id=capacity_epoch_id,
            started_lag_seconds=0,
        )
        store.start_capacity_trial(
            run_id,
            cell,
            target_depth=1,
            trial_id=capacity_epoch_id,
        )
        replica_id = store.start_replica(
            capacity_epoch_id,
            stage_target=1,
            ordinal=0,
        )
        store.mark_replica_created(replica_id, "depth-instance-1", 0.3)
        provider = FastFailingCanaryProvider()
        provider.provider = "lambda"

        remaining = PilotSupervisor(store, {"lambda": provider}).recover_run(run_id)

        self.assertEqual(remaining, [])
        self.assertEqual(
            store.canary_row(lease_id)["censor_reason"],
            "right_censored_collector_loss",
        )
        self.assertEqual(
            store.canary_row(legacy_lease_id)["censor_reason"],
            "right_censored_collector_loss",
        )
        self.assertEqual(
            store.canary_epoch_row(epoch_id)["status"],
            "collector_loss",
        )
        self.assertEqual(
            store.capacity_trial_row(capacity_epoch_id)["status"],
            "collector_loss",
        )
        capacity_epoch = next(
            row
            for row in store.capacity_epoch_rows(run_id)
            if row["id"] == capacity_epoch_id
        )
        self.assertEqual(capacity_epoch["status"], "collector_loss")
        self.assertEqual(
            store.run_row(run_id)["status"],
            "recovered_after_collector_loss",
        )

    def test_operator_cleanup_closes_unfinished_capacity_and_canary_epochs(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        run_id = store.start_run(plan.experiment, plan.raw_document)
        cell = TrialCell(
            provider="lambda",
            sku="gpu_1x_a10",
            gpu_type="A10",
            region="us-east-1",
            resource_class="on-demand",
            market="on-demand",
        )
        capacity_epoch_id = store.schedule_capacity_epoch(
            run_id,
            cohort_name="operator-depth",
            sequence=0,
            cell=cell,
            target_depth=1,
            scheduled_offset_seconds=0,
            scheduled_at="2026-08-10T00:00:00+00:00",
            selection_probability=1,
            selection_seed=1,
        )
        store.mark_capacity_epoch_started(
            capacity_epoch_id,
            trial_id=capacity_epoch_id,
            started_lag_seconds=0,
        )
        store.start_capacity_trial(
            run_id,
            cell,
            target_depth=1,
            trial_id=capacity_epoch_id,
        )
        replica_id = store.start_replica(
            capacity_epoch_id,
            stage_target=1,
            ordinal=0,
        )
        store.mark_replica_created(replica_id, "depth-instance-1", 0.3)
        canary_epoch_id = store.start_canary_epoch(
            run_id,
            cohort_name="operator-canary",
            sequence=0,
            cell=cell,
            planned_duration_seconds=600,
            interval_seconds=30,
            selection_probability=1,
            selection_seed=1,
        )
        store.mark_canary_epoch_started(
            canary_epoch_id,
            "2026-08-10T01:00:00+00:00",
        )
        lease_id = store.start_canary(
            run_id,
            cell,
            pair_id=canary_epoch_id,
            interruptible=True,
            planned_end_at="2026-08-10T01:10:00+00:00",
        )
        store.mark_canary_created(lease_id, provider_resource_id="canary-instance-1")
        provider = FastFailingCanaryProvider()
        provider.provider = "lambda"

        remaining = PilotSupervisor(store, {"lambda": provider}).cleanup_run(run_id)

        self.assertEqual(remaining, [])
        self.assertEqual(store.run_row(run_id)["status"], "stopped")
        self.assertEqual(
            store.capacity_trial_row(capacity_epoch_id)["status"],
            "operator_stop",
        )
        capacity_epoch = next(
            row
            for row in store.capacity_epoch_rows(run_id)
            if row["id"] == capacity_epoch_id
        )
        self.assertEqual(capacity_epoch["status"], "operator_stop")
        self.assertEqual(
            store.canary_row(lease_id)["censor_reason"],
            "operator_cleanup",
        )
        self.assertEqual(
            store.canary_epoch_row(canary_epoch_id)["status"],
            "operator_stop",
        )

    def test_capacity_cohort_runs_precomputed_depth_epochs(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-capacity-cohort",
                "duration_seconds": 0.35,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [],
                "capacity_cohorts": [
                    {
                        "name": "runpod-depth",
                        "max_trials": 3,
                        "selection_seed": 23,
                        "initial_delay_seconds": 0,
                        "interval_seconds": 0.03,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 0.01,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.3,
                                "require_application_ready": True,
                                "billing_minimum_seconds": 60,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        provider = ReadyCapacityProvider()

        run_id = PilotSupervisor(store, {"runpod": provider}).run(
            plan, enable_active=True
        )

        epochs = store.connection.execute(
            "SELECT sequence, status, selection_probability, trial_id "
            "FROM capacity_epochs WHERE run_id=? ORDER BY sequence",
            (run_id,),
        ).fetchall()
        self.assertEqual(len(epochs), 3)
        self.assertEqual([row["sequence"] for row in epochs], [0, 1, 2])
        self.assertTrue(all(row["status"] == "complete" for row in epochs))
        self.assertTrue(all(row["selection_probability"] == 1 for row in epochs))
        self.assertTrue(all(row["trial_id"] is not None for row in epochs))
        self.assertEqual(len(provider.created), 3)
        self.assertEqual(store.owned_resources(run_id), [])

    def test_capacity_tick_reuses_campaign_and_executes_at_most_one_due_epoch(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "5.0-capacity-tick",
                "duration_seconds": 180,
                "max_active_gpus_per_provider": 1,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [],
                "capacity_cohorts": [
                    {
                        "name": "atomic-depth",
                        "max_trials": 2,
                        "selection_seed": 23,
                        "initial_delay_seconds": 0,
                        "interval_seconds": 60,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.3,
                                "require_application_ready": True,
                                "billing_minimum_seconds": 0,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "capacity-tick.db")
        self.addCleanup(store.close)
        provider = ReadyCapacityProvider()
        supervisor = PilotSupervisor(store, {"runpod": provider})
        started_at = dt.datetime(2026, 8, 11, 0, 0, tzinfo=dt.timezone.utc)

        first = supervisor.capacity_tick(
            plan,
            campaign_id="capacity-60d",
            enable_active=True,
            now=started_at,
        )
        duplicate = supervisor.capacity_tick(
            plan,
            campaign_id="capacity-60d",
            enable_active=True,
            now=started_at,
        )

        self.assertEqual(first["status"], "executed")
        self.assertTrue(first["campaign_created"])
        self.assertEqual(duplicate["status"], "idle")
        self.assertFalse(duplicate["campaign_created"])
        self.assertEqual(first["run_id"], duplicate["run_id"])
        self.assertEqual(len(provider.created), 1)
        epochs = store.capacity_epoch_rows(first["run_id"])
        self.assertEqual([row["status"] for row in epochs], ["complete", "scheduled"])

        second = supervisor.capacity_tick(
            plan,
            campaign_id="capacity-60d",
            enable_active=True,
            now=started_at + dt.timedelta(seconds=61),
        )
        completed = supervisor.capacity_tick(
            plan,
            campaign_id="capacity-60d",
            enable_active=True,
            now=started_at + dt.timedelta(seconds=62),
        )

        self.assertEqual(second["status"], "executed")
        self.assertEqual(completed["status"], "complete")
        self.assertEqual(len(provider.created), 2)
        self.assertEqual(
            store.run_row(first["run_id"])["status"],
            "complete",
        )
        self.assertEqual(
            store.connection.execute("SELECT COUNT(*) FROM experiment_runs").fetchone()[0],
            1,
        )

    def test_capacity_tick_rejects_campaign_plan_changes(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "5.0-capacity-tick",
                "duration_seconds": 180,
                "capacity_trials": [],
                "canary_trials": [],
                "capacity_cohorts": [
                    {
                        "name": "atomic-depth",
                        "max_trials": 1,
                        "selection_seed": 23,
                        "initial_delay_seconds": 60,
                        "interval_seconds": 60,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "lambda",
                                "sku": "gpu_1x_a10",
                                "gpu_type": "A10",
                                "region": "us-east-1",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.6,
                                "require_application_ready": False,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        store = TraceStore(self.root / "campaign-change.db")
        self.addCleanup(store.close)
        supervisor = PilotSupervisor(store, {"lambda": CountingCatalogProvider()})
        now = dt.datetime(2026, 8, 11, 0, 0, tzinfo=dt.timezone.utc)
        supervisor.capacity_tick(
            load_plan(self.plan_path),
            campaign_id="fixed-campaign",
            enable_active=True,
            now=now,
        )

        document["capacity_cohorts"][0]["selection_seed"] = 99
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "different experiment"):
            supervisor.capacity_tick(
                load_plan(self.plan_path),
                campaign_id="fixed-campaign",
                enable_active=True,
                now=now,
            )

    def test_capacity_tick_marks_missed_slots_instead_of_replaying_backlog(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "5.0-capacity-tick",
                "duration_seconds": 300,
                "max_active_gpus_per_provider": 1,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [],
                "capacity_cohorts": [
                    {
                        "name": "atomic-depth",
                        "max_trials": 3,
                        "selection_seed": 23,
                        "initial_delay_seconds": 0,
                        "interval_seconds": 60,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.3,
                                "require_application_ready": True,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "missed-slots.db")
        self.addCleanup(store.close)
        provider = ReadyCapacityProvider()
        supervisor = PilotSupervisor(store, {"runpod": provider})
        started_at = dt.datetime(2026, 8, 11, 0, 0, tzinfo=dt.timezone.utc)

        first = supervisor.capacity_tick(
            plan,
            campaign_id="capacity-missed-slots",
            enable_active=True,
            now=started_at,
        )
        after_outage = supervisor.capacity_tick(
            plan,
            campaign_id="capacity-missed-slots",
            enable_active=True,
            now=started_at + dt.timedelta(seconds=181),
        )

        epochs = store.capacity_epoch_rows(first["run_id"])
        self.assertEqual(len(provider.created), 1)
        self.assertEqual(after_outage["status"], "complete")
        self.assertEqual(
            [row["status"] for row in epochs],
            ["complete", "not_run_missed_slot", "not_run_missed_slot"],
        )
        self.assertEqual(
            [row["stop_reason"] for row in epochs[1:]],
            ["external_scheduler_late", "external_scheduler_late"],
        )

    def test_capacity_campaign_recovery_preserves_future_schedule(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "5.0-capacity-tick",
                "duration_seconds": 180,
                "capacity_trials": [],
                "canary_trials": [],
                "capacity_cohorts": [
                    {
                        "name": "recoverable-depth",
                        "max_trials": 2,
                        "selection_seed": 23,
                        "initial_delay_seconds": 60,
                        "interval_seconds": 60,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                "provider": "lambda",
                                "sku": "gpu_1x_a10",
                                "gpu_type": "A10",
                                "region": "us-east-1",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 1,
                                "max_lease_seconds": 60,
                                "estimated_price_usd_hr": 0.6,
                                "require_application_ready": False,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "campaign-recovery.db")
        self.addCleanup(store.close)
        provider = CountingCatalogProvider()
        supervisor = PilotSupervisor(store, {"lambda": provider})
        now = dt.datetime(2026, 8, 11, 0, 0, tzinfo=dt.timezone.utc)

        initialized = supervisor.capacity_tick(
            plan,
            campaign_id="recoverable-campaign",
            enable_active=True,
            now=now - dt.timedelta(seconds=1),
        )
        run_id = initialized["run_id"]
        # Simulate collector loss after the next epoch was claimed and started.
        claim_at = now + dt.timedelta(seconds=60)
        claim_status, claimed = store.claim_due_capacity_epoch(
            "recoverable-campaign",
            due_at=claim_at.isoformat(),
            expired_before=(claim_at - dt.timedelta(seconds=60)).isoformat(),
        )
        self.assertEqual(claim_status, "claimed")
        store.mark_capacity_epoch_started(
            claimed["id"],
            trial_id=claimed["id"],
            started_lag_seconds=0,
        )
        store.start_capacity_trial(
            run_id,
            plan.capacity_cohorts[0].cells[0].cell,
            target_depth=1,
            trial_id=claimed["id"],
        )

        remaining = supervisor.recover_run(run_id)

        recovered_epochs = store.capacity_epoch_rows(run_id)
        self.assertEqual(remaining, [])
        self.assertEqual(recovered_epochs[0]["status"], "collector_loss")
        self.assertEqual(recovered_epochs[1]["status"], "scheduled")
        self.assertEqual(store.run_row(run_id)["status"], "running")
        self.assertEqual(
            store.capacity_campaign_row("recoverable-campaign")["status"],
            "running",
        )

    def test_capacity_balanced_cycle_covers_every_cell_before_repeating(self):
        document = plan_document()
        cell = {
            "provider": "lambda",
            "gpu_type": "A10",
            "region": "us-east-1",
            "resource_class": "on-demand",
            "market": "on-demand",
            "target_depth": 1,
            "ready_timeout_seconds": 1,
            "max_lease_seconds": 60,
            "estimated_price_usd_hr": 0.3,
        }
        document.update(
            {
                "capacity_trials": [],
                "capacity_cohorts": [
                    {
                        "name": "balanced",
                        "max_trials": 4,
                        "selection_seed": 3,
                        "selection_strategy": "balanced_cycle",
                        "interval_seconds": 60,
                        "jitter_seconds": 0,
                        "min_remaining_seconds": 1,
                        "cells": [
                            {**cell, "sku": "cell-a"},
                            {**cell, "sku": "cell-b"},
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        cohort = load_plan(self.plan_path).capacity_cohorts[0]

        selected = cohort.select_sequence(random.Random(cohort.selection_seed))

        self.assertEqual({item[0].cell.sku for item in selected[:2]}, {"cell-a", "cell-b"})
        self.assertEqual({item[0].cell.sku for item in selected[2:]}, {"cell-a", "cell-b"})
        self.assertTrue(all(item[1] == 0.5 for item in selected))

    def test_completion_window_balanced_cycle_covers_every_cell_and_arm(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-completion-window",
                "capacity_trials": [],
                "canary_trials": [],
                "completion_window_cohorts": [
                    {
                        "name": "window-randomization",
                        "max_opportunities": 8,
                        "selection_seed": 31,
                        "selection_strategy": "balanced_cycle",
                        "initial_delay_seconds": 0,
                        "opportunity_interval_seconds": 100,
                        "jitter_seconds": 0,
                        "completion_window_seconds": 60,
                        "required_runtime_seconds": 20,
                        "startup_safety_seconds": 10,
                        "min_remaining_seconds": 10,
                        "arms": [
                            {"name": "immediate", "launch_fraction": 0, "weight": 1},
                            {"name": "lazy", "launch_fraction": 1, "weight": 1},
                        ],
                        "cells": [
                            {
                                "provider": "lambda",
                                "sku": "gpu_1x_a10",
                                "gpu_type": "A10",
                                "region": "us-east-1",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 5,
                                "max_lease_seconds": 10,
                                "estimated_price_usd_hr": 0.6,
                                "require_application_ready": False,
                            },
                            {
                                "provider": "lambda",
                                "sku": "gpu_1x_a10-west",
                                "gpu_type": "A10",
                                "region": "us-west-1",
                                "resource_class": "on-demand",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 5,
                                "max_lease_seconds": 10,
                                "estimated_price_usd_hr": 0.6,
                                "require_application_ready": False,
                            },
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")

        cohort = load_plan(self.plan_path).completion_window_cohorts[0]
        sequence = cohort.select_sequence(random.Random(cohort.selection_seed))

        self.assertEqual(len(sequence), 8)
        first_cycle = {(cell.cell.sku, arm.name) for cell, arm, _ in sequence[:4]}
        second_cycle = {(cell.cell.sku, arm.name) for cell, arm, _ in sequence[4:]}
        expected = {
            ("gpu_1x_a10", "immediate"),
            ("gpu_1x_a10", "lazy"),
            ("gpu_1x_a10-west", "immediate"),
            ("gpu_1x_a10-west", "lazy"),
        }
        self.assertEqual(first_cycle, expected)
        self.assertEqual(second_cycle, expected)
        self.assertTrue(
            all(probability == 0.25 for _cell, _arm, probability in sequence)
        )

    def test_completion_window_cohort_persists_randomized_outcomes(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-completion-window",
                "duration_seconds": 0.25,
                "max_active_gpus_per_provider": 1,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [],
                "completion_window_cohorts": [
                    {
                        "name": "window-execution",
                        "max_opportunities": 2,
                        "selection_seed": 37,
                        "selection_strategy": "balanced_cycle",
                        "initial_delay_seconds": 0,
                        "opportunity_interval_seconds": 0.08,
                        "jitter_seconds": 0,
                        "completion_window_seconds": 0.1,
                        "required_runtime_seconds": 0.01,
                        "startup_safety_seconds": 0.05,
                        "min_remaining_seconds": 0.01,
                        "arms": [
                            {"name": "immediate", "launch_fraction": 0},
                            {"name": "lazy", "launch_fraction": 1},
                        ],
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "target_depth": 1,
                                "ready_timeout_seconds": 0.005,
                                "max_lease_seconds": 0.01,
                                "estimated_price_usd_hr": 0.3,
                                "require_application_ready": True,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        provider = ReadyCapacityProvider()

        run_id = PilotSupervisor(store, {"runpod": provider}).run(
            plan,
            enable_active=True,
        )

        windows = store.completion_window_epoch_rows(run_id)
        self.assertEqual(len(windows), 2)
        self.assertEqual({row["arm_name"] for row in windows}, {"immediate", "lazy"})
        self.assertTrue(all(row["status"] == "complete" for row in windows))
        self.assertTrue(all(row["provider_sla_met"] == 1 for row in windows))
        self.assertTrue(all(row["application_sla_met"] == 1 for row in windows))
        self.assertTrue(all(row["capacity_epoch_id"] for row in windows))
        self.assertEqual(len(store.capacity_epoch_rows(run_id)), 2)
        self.assertEqual(len(provider.created), 2)
        self.assertEqual(store.owned_resources(run_id), [])

    def test_control_cohort_runs_single_on_demand_lifecycle_per_epoch(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "3.0-cross-provider-control",
                "duration_seconds": 0.16,
                "max_active_gpus_per_provider": 1,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "capacity_trials": [],
                "canary_trials": [],
                "canary_cohorts": [],
                "control_cohorts": [
                    {
                        "name": "runpod-control",
                        "max_leases": 2,
                        "selection_seed": 43,
                        "replacement_delay_seconds": 0,
                        "min_remaining_seconds": 0.01,
                        "cells": [
                            {
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                                "market": "on-demand",
                                "duration_seconds": 0.03,
                                "interval_seconds": 0.005,
                                "estimated_price_usd_hr": 0.3,
                                "weight": 1,
                            }
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        provider = FastFailingCanaryProvider()

        run_id = PilotSupervisor(store, {"runpod": provider}).run(
            plan,
            enable_active=True,
        )

        epochs = store.canary_epoch_rows(run_id)
        leases = store.canary_rows(run_id)
        self.assertEqual(len(epochs), 2)
        self.assertEqual(len(leases), 2)
        self.assertTrue(all(row["interruptible"] == 0 for row in leases))
        self.assertTrue(all(row["status"] == "complete" for row in epochs))
        self.assertEqual(
            {json.loads(row["details_json"])["design"] for row in epochs},
            {"unmatched_on_demand_control"},
        )
        self.assertEqual(store.owned_resources(run_id), [])

    def test_control_balanced_cycle_covers_each_provider_cell(self):
        document = plan_document()
        cell = {
            "gpu_type": "A10",
            "resource_class": "on-demand",
            "market": "on-demand",
            "duration_seconds": 60,
            "interval_seconds": 5,
            "estimated_price_usd_hr": 0.6,
        }
        document.update(
            {
                "capacity_trials": [],
                "canary_trials": [],
                "provider_budgets_usd": {"lambda": 1, "runpod": 1},
                "total_budget_usd": 2,
                "control_cohorts": [
                    {
                        "name": "balanced-controls",
                        "max_leases": 4,
                        "selection_seed": 53,
                        "selection_strategy": "balanced_cycle",
                        "min_remaining_seconds": 1,
                        "cells": [
                            {
                                **cell,
                                "provider": "lambda",
                                "sku": "gpu_1x_a10",
                                "region": "us-east-1",
                            },
                            {
                                **cell,
                                "provider": "runpod",
                                "sku": "NVIDIA A40",
                                "gpu_type": "A40",
                                "resource_class": "secure",
                            },
                        ],
                    }
                ],
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        cohort = load_plan(self.plan_path).control_cohorts[0]

        sequence = cohort.select_sequence(random.Random(cohort.selection_seed))

        self.assertEqual(
            {trial.cell.provider for trial, _probability in sequence[:2]},
            {"lambda", "runpod"},
        )
        self.assertEqual(
            {trial.cell.provider for trial, _probability in sequence[2:]},
            {"lambda", "runpod"},
        )
        self.assertTrue(all(probability == 0.5 for _trial, probability in sequence))

    def test_passive_sampling_continues_while_capacity_trial_is_blocked(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "trace.db")
        self.addCleanup(store.close)
        provider = CountingCatalogProvider()
        supervisor = PilotSupervisor(
            store,
            providers={"lambda": provider},
            capacity_runner_factory=BlockingCapacityRunner,
        )
        result = {}

        thread = threading.Thread(
            target=lambda: result.setdefault("run_id", supervisor.run(plan, enable_active=True)),
            daemon=True,
        )
        thread.start()
        self.assertTrue(BlockingCapacityRunner.entered.wait(timeout=1))
        time.sleep(0.06)
        observed_while_blocked = provider.calls
        BlockingCapacityRunner.release.set()
        thread.join(timeout=2)

        self.assertFalse(thread.is_alive())
        self.assertGreaterEqual(observed_while_blocked, 2)
        self.assertIn("run_id", result)
        quote_count = store.connection.execute("SELECT COUNT(*) FROM catalog_quotes_v2").fetchone()[0]
        host_count = store.connection.execute("SELECT COUNT(*) FROM host_observations").fetchone()[0]
        self.assertGreaterEqual(quote_count, 2)
        self.assertGreaterEqual(host_count, 2)


if __name__ == "__main__":
    unittest.main()
