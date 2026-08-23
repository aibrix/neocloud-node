import contextlib
import io
import json
import datetime as dt
import pathlib
import socket
import sys
import tempfile
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
    ExecutionDisabled,
    BudgetExceeded,
    BudgetGuard,
    PilotSupervisor,
    load_plan,
)


def plan_document():
    return {
        "protocol_version": "3.0-capacity",
        "host_id": "test-host",
        "vantage_id": "test-vantage",
        "duration_seconds": 0.15,
        "passive_interval_seconds": 0.01,
        "host_interval_seconds": 0.01,
        "canary_interval_seconds": 0.01,
        "max_active_gpus_per_provider": 8,
        "provider_budgets_usd": {"lambda": 1.0},
        "total_budget_usd": 1.0,
        "capacity_cohorts": [
            {
                "name": "lambda-capacity",
                "max_trials": 1,
                "selection_seed": 17,
                "initial_delay_seconds": 0,
                "interval_seconds": 60,
                "jitter_seconds": 0,
                "min_remaining_seconds": 0.01,
                "cells": [
                    {
                        "provider": "lambda",
                        "sku": "gpu_1x_a10",
                        "gpu_type": "A10",
                        "region": "us-east-1",
                        "resource_class": "on-demand",
                        "market": "on-demand",
                        "target_gpu_depth": 1,
                        "ready_timeout_seconds": 1,
                        "max_lease_seconds": 60,
                        "estimated_price_usd_hr": 0.6,
                        "require_application_ready": False,
                    }
                ],
            }
        ],
        "canary_cohorts": [],
    }


def stability_plan_document():
    document = plan_document()
    document.update(
        {
            "protocol_version": "3.0-stability",
            "duration_seconds": 0.2,
            "max_active_gpus_per_provider": 2,
            "capacity_cohorts": [],
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
        return [
            CatalogFetch("lambda", "instance-types", HTTPResponse(200, 1, {}), (quote,))
        ]


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


class ResearchSupervisorTest(unittest.TestCase):
    def setUp(self):
        self.tempdir = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tempdir.name)
        self.plan_path = self.root / "plan.json"
        self.plan_path.write_text(json.dumps(plan_document()), encoding="utf-8")

    def tearDown(self):
        self.tempdir.cleanup()

    def test_load_plan_requires_positive_explicit_budgets(self):
        document = plan_document()
        document["provider_budgets_usd"] = {}
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        with self.assertRaises(ValueError):
            load_plan(self.plan_path)

    def test_capacity_plan_accepts_explicit_d32_gpu_depth_and_shape(self):
        document = plan_document()
        document["max_active_gpus_per_provider"] = 32
        document["provider_budgets_usd"] = {"runpod": 20.0}
        document["total_budget_usd"] = 20.0
        document["capacity_cohorts"][0]["cells"] = [
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
        ]
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")

        plan = load_plan(self.plan_path)
        trial = plan.capacity_cohorts[0].cells[0]

        self.assertEqual(trial.target_depth, 32)
        self.assertEqual(trial.cell.gpu_count, 4)
        self.assertEqual(trial.target_instance_count, 8)
        self.assertAlmostEqual(trial.budget_upper_bound_usd, 6.9)

    def test_canary_budget_applies_billing_floor_and_safety_multiplier(self):
        document = stability_plan_document()
        cell = document["canary_cohorts"][0]["cells"][0]
        cell.update(
            {
                "duration_seconds": 30,
                "interval_seconds": 1,
                "billing_minimum_seconds": 60,
                "settlement_safety_multiplier": 1.2,
            }
        )
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")

        trial = load_plan(self.plan_path).canary_cohorts[0].cells[0]

        self.assertAlmostEqual(trial.budget_upper_bound_usd, 0.012)

        document = stability_plan_document()
        document["max_active_gpus_per_provider"] = 33
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        with self.assertRaises(ValueError):
            load_plan(self.plan_path)

    def test_billable_cli_commands_default_to_dry_run_and_accept_execute(self):
        parser = trace_cli.build_parser()
        database = str(self.root / "trace.db")
        plan = str(self.plan_path)

        for command in ("run", "tick"):
            args = parser.parse_args([command, "--db", database, "--plan", plan])
            self.assertFalse(args.execute)
            execute_args = parser.parse_args(
                [command, "--db", database, "--plan", plan, "--execute"]
            )
            self.assertTrue(execute_args.execute)

        for command in ("cleanup", "recover"):
            args = parser.parse_args([command, "--db", database])
            self.assertFalse(args.execute)
            execute_args = parser.parse_args(
                [command, "--db", database, "--execute"]
            )
            self.assertTrue(execute_args.execute)

        catalog = parser.parse_args(
            ["catalog", "--db", database, "--provider", "runpod"]
        )
        self.assertFalse(hasattr(catalog, "execute"))
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
        self.assertFalse(hasattr(export, "execute"))
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
        self.assertFalse(hasattr(sync_billing, "execute"))
        reconcile = parser.parse_args(["billing", "reconcile", "--db", database])
        self.assertFalse(hasattr(reconcile, "execute"))

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
                "--execute",
            ]
        )
        self.assertEqual(identified.host_id, "stable-host-1")
        self.assertEqual(identified.vantage_id, "us-west-office")
        self.assertTrue(identified.execute)

    def test_cli_help_keeps_operational_options_with_their_command(self):
        parser = trace_cli.build_parser()
        top_level_help = parser.format_help()

        self.assertIn("run", top_level_help)
        self.assertIn("tick", top_level_help)
        self.assertIn("catalog", top_level_help)
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
            "--execute",
            "--experiment-id",
        ):
            self.assertIn(option, run_help)
        self.assertNotIn("--target", run_help)
        self.assertNotIn("--index", run_help)
        self.assertNotIn("--campaign-id", run_help)
        self.assertNotIn("--enable-active", run_help)

        output = io.StringIO()
        with contextlib.redirect_stdout(output), self.assertRaises(SystemExit):
            parser.parse_args(["catalog", "--help"])
        catalog_help = output.getvalue()
        self.assertIn("--provider", catalog_help)
        self.assertIn("--vantage-id", catalog_help)
        self.assertNotIn("--plan", catalog_help)
        self.assertNotIn("--host-id", catalog_help)

        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            parser.parse_args(["collect", "--db", "trace.db", "--provider", "runpod"])

    def test_catalog_cli_deduplicates_repeated_query_dimensions(self):
        args = trace_cli.build_parser().parse_args(
            [
                "catalog",
                "--db",
                str(self.root / "catalog.db"),
                "--provider",
                "runpod",
                "--provider",
                "runpod",
                "--gpu-count",
                "1",
                "--gpu-count",
                "4",
                "--gpu-count",
                "1",
            ]
        )

        config = trace_cli._catalog_config(args)

        self.assertEqual(config.providers, ("runpod",))
        self.assertEqual(config.gpu_counts, (1, 4))

    def test_catalog_cli_executes_one_passive_snapshot(self):
        class Provider:
            def catalog_fetches(self, gpu_counts):
                return [
                    CatalogFetch(
                        provider="runpod",
                        operation=f"catalog.g{gpu_counts[0]}",
                        response=HTTPResponse(200, 5, payload={}),
                        quotes=(),
                    )
                ]

        args = trace_cli.build_parser().parse_args(
            [
                "catalog",
                "--db",
                str(self.root / "catalog.db"),
                "--provider",
                "runpod",
                "--gpu-count",
                "1",
                "--once",
            ]
        )
        with mock.patch.object(
            trace_cli,
            "_provider_clients",
            return_value={"runpod": Provider()},
        ):
            result = trace_cli.execute(args)

        self.assertEqual(result["status"], "complete")
        self.assertEqual(result["workflow"], "catalog")
        self.assertIsNone(result["experiment_id"])
        self.assertEqual(result["counts"]["host_observations"], 1)
        self.assertEqual(result["counts"]["provider_api_observations_v2"], 1)

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

    def test_tick_cli_defaults_to_a_non_mutating_plan_preview(self):
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
        self.assertIsNone(args.experiment_id)
        self.assertFalse(hasattr(args, "index"))
        self.assertFalse(args.execute)

        with mock.patch.object(module, "_provider_clients") as clients:
            result = module.execute(args)

        clients.assert_not_called()
        self.assertEqual(result["mode"], "dry-run")
        self.assertEqual(result["status"], "dry-run")
        self.assertEqual(result["workflow"], "capacity")
        self.assertEqual(result["experiment_id"], "capacity-experiment")
        self.assertEqual(result["cohort_name"], "lambda-capacity")
        self.assertFalse(result["provider_mutations"])
        store = TraceStore(args.db)
        self.addCleanup(store.close)
        self.assertEqual(
            store.connection.execute("SELECT COUNT(*) FROM experiment_runs").fetchone()[
                0
            ],
            0,
        )

    def test_supervisor_rejects_execution_without_explicit_opt_in(self):
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "supervisor-guard.db")
        self.addCleanup(store.close)

        with self.assertRaises(ExecutionDisabled):
            PilotSupervisor(store, {"lambda": ReadyCapacityProvider()}).capacity_tick(
                plan,
                experiment_id="guarded-experiment",
            )

        self.assertEqual(
            store.connection.execute("SELECT COUNT(*) FROM experiment_runs").fetchone()[
                0
            ],
            0,
        )

    def test_cleanup_cli_dry_run_lists_resources_without_deleting(self):
        db_path = self.root / "cleanup-preview.db"
        plan = load_plan(self.plan_path)
        store = TraceStore(db_path)
        run_id = store.start_run(plan.experiment, plan.raw_document)
        cell = plan.capacity_cohorts[0].cells[0].cell
        trial_id = store.start_capacity_trial(run_id, cell, target_depth=1)
        replica_id = store.start_replica(trial_id, stage_target=1, ordinal=0)
        store.mark_replica_created(replica_id, "preview-instance", 0.6)
        store.close()
        args = trace_cli.build_parser().parse_args(
            ["cleanup", "--db", str(db_path), "--run-id", run_id]
        )

        with mock.patch.object(trace_cli, "_provider_clients") as clients:
            result = trace_cli.execute(args)

        clients.assert_not_called()
        self.assertEqual(result["mode"], "dry-run")
        self.assertEqual(result["status"], "dry-run")
        self.assertEqual(result["command"], "cleanup")
        self.assertEqual(
            result["would_process_resources"][0]["provider_resource_id"],
            "preview-instance",
        )
        reopened = TraceStore(db_path)
        self.addCleanup(reopened.close)
        self.assertEqual(len(reopened.owned_resources(run_id)), 1)

    def test_capacity_cli_auto_identifies_host_and_reuses_database_experiment(self):
        document = plan_document()
        document.update(
            {
                "host_id": "replace-with-stable-host-id",
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
                    "--execute",
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

        self.assertEqual(first["mode"], "execute")
        self.assertEqual(first["status"], "executed")
        self.assertTrue(first["experiment_created"])
        self.assertTrue(first["experiment_complete"])
        self.assertEqual(first["cohort_name"], "atomic-depth")
        self.assertEqual(first["experiment_id"], "capacity-experiment")
        self.assertEqual(second["status"], "complete")
        self.assertFalse(second["experiment_created"])
        self.assertEqual(first["run_id"], second["run_id"])
        self.assertEqual(len(provider.created), 1)
        store = TraceStore(db_path)
        self.addCleanup(store.close)
        run = store.run_row(first["run_id"])
        self.assertEqual(run["workflow"], "capacity")
        self.assertEqual(run["experiment_id"], "capacity-experiment")
        self.assertEqual(run["cohort_name"], "atomic-depth")
        self.assertEqual(run["host_id"], socket.gethostname())
        self.assertEqual(run["vantage_id"], "test-vantage")

        changed = json.loads(renamed.read_text(encoding="utf-8"))
        changed["capacity_cohorts"][0]["selection_seed"] = 99
        renamed.write_text(json.dumps(changed), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "different experiment"):
            invoke(renamed)

    def test_tick_cli_requires_a_capacity_cohort(self):
        document = plan_document()
        document["capacity_cohorts"] = []
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        module = trace_cli
        args = module.build_parser().parse_args(
            [
                "tick",
                "--db",
                str(self.root / "capacity.db"),
                "--plan",
                str(self.plan_path),
                "--execute",
            ]
        )

        with self.assertRaisesRegex(ValueError, "exactly one capacity_cohorts"):
            module.execute(args)

    def test_tick_contract_rejects_legacy_trials_before_db_or_provider(self):
        document = plan_document()
        document["capacity_trials"] = [document["capacity_cohorts"][0]["cells"][0]]
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        db_path = self.root / "invalid-capacity.db"
        args = trace_cli.build_parser().parse_args(
            [
                "tick",
                "--db",
                str(db_path),
                "--plan",
                str(self.plan_path),
                "--execute",
            ]
        )

        with mock.patch.object(trace_cli, "_provider_clients") as clients:
            with self.assertRaisesRegex(
                ValueError,
                "unsupported active plan sections: capacity_trials",
            ):
                trace_cli.execute(args)

        clients.assert_not_called()
        self.assertFalse(db_path.exists())

    def test_tick_contract_requires_one_trial_in_its_only_cohort(self):
        document = plan_document()
        document.update(
            {
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
                "--execute",
            ]
        )

        with self.assertRaisesRegex(ValueError, "max_trials must be one"):
            trace_cli.execute(args)

        self.assertFalse(db_path.exists())

    def test_run_contract_rejects_mixed_plan_before_db_or_provider(self):
        document = stability_plan_document()
        document["capacity_trials"] = [
            plan_document()["capacity_cohorts"][0]["cells"][0]
        ]
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        db_path = self.root / "invalid-stability.db"
        args = trace_cli.build_parser().parse_args(
            [
                "run",
                "--db",
                str(db_path),
                "--plan",
                str(self.plan_path),
                "--execute",
            ]
        )

        with mock.patch.object(trace_cli, "_provider_clients") as clients:
            with self.assertRaisesRegex(
                ValueError,
                "unsupported active plan sections: capacity_trials",
            ):
                trace_cli.execute(args)

        clients.assert_not_called()
        self.assertFalse(db_path.exists())

    def test_active_commands_have_no_plan_section_selectors(self):
        self.plan_path.write_text(
            json.dumps(stability_plan_document()),
            encoding="utf-8",
        )
        parser = trace_cli.build_parser()
        for command, selector in (("run", "--target"), ("tick", "--index")):
            with contextlib.redirect_stderr(io.StringIO()):
                with self.assertRaises(SystemExit):
                    parser.parse_args(
                        [
                            command,
                            "--db",
                            str(self.root / f"{command}.db"),
                            "--plan",
                            str(self.plan_path),
                            selector,
                            "0",
                            "--execute",
                        ]
                    )

    def test_run_cli_reuses_durable_stability_experiment(self):
        document = stability_plan_document()
        document["duration_seconds"] = 0.08
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        db_path = self.root / "stability-cli.db"
        provider = FastFailingCanaryProvider()

        def invoke():
            args = trace_cli.build_parser().parse_args(
                [
                    "run",
                    "--db",
                    str(db_path),
                    "--plan",
                    str(self.plan_path),
                    "--execute",
                ]
            )
            with mock.patch.object(
                trace_cli,
                "_provider_clients",
                return_value={"lambda": provider},
            ):
                return trace_cli.execute(args)

        first = invoke()
        duplicate = invoke()

        self.assertEqual(first["mode"], "execute")
        self.assertEqual(first["experiment_id"], "stability-experiment")
        self.assertEqual(first["status"], "executed")
        self.assertEqual(first["window_status"], "complete")
        self.assertTrue(first["experiment_complete"])
        self.assertEqual(duplicate["status"], "complete")
        self.assertEqual(first["run_id"], duplicate["run_id"])
        self.assertEqual(len(provider.created), 2)

    def test_init_reports_dynamic_cohort_pair_cost_bounds(self):
        document = stability_plan_document()
        document.update(
            {
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

        self.assertAlmostEqual(
            result["canary_cohorts"][0]["min_pair_upper_bound_usd"], 0.01
        )
        self.assertAlmostEqual(
            result["canary_cohorts"][0]["max_pair_upper_bound_usd"], 0.02
        )
        self.assertEqual(result["total_budget_usd"], 1.0)

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
        self.assertEqual(
            result["stages"][0]["attempts"][0]["provider_resource_id"], "pod-1"
        )

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
        document = stability_plan_document()
        document.update(
            {
                "protocol_version": "3.0-survival-cohort",
                "duration_seconds": 0.5,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
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

        result = supervisor.stability_run(
            plan,
            experiment_id="replacement-test",
            execute=True,
            now=dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc),
        )
        run_id = result["run_id"]

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
        document = stability_plan_document()
        document.update(
            {
                "protocol_version": "3.0-survival-cohort",
                "duration_seconds": 0.2,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
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

        result = PilotSupervisor(store, {"runpod": provider}).stability_run(
            plan,
            experiment_id="pair-setup-test",
            execute=True,
            now=dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc),
        )
        run_id = result["run_id"]

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
        document = stability_plan_document()
        document.update(
            {
                "protocol_version": "3.0-survival-cohort",
                "duration_seconds": 0.2,
                "max_active_gpus_per_provider": 2,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
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

        result = PilotSupervisor(
            store,
            {"runpod": CleanupFailProvider()},
        ).stability_run(
            plan,
            experiment_id="cleanup-guard-test",
            execute=True,
            now=dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc),
        )
        run_id = result["run_id"]

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

    def test_capacity_tick_reuses_experiment_and_executes_at_most_one_due_epoch(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "5.0-capacity-tick",
                "duration_seconds": 180,
                "max_active_gpus_per_provider": 1,
                "provider_budgets_usd": {"runpod": 1.0},
                "total_budget_usd": 1.0,
                "canary_cohorts": [],
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
            experiment_id="capacity-60d",
            execute=True,
            now=started_at,
        )
        duplicate = supervisor.capacity_tick(
            plan,
            experiment_id="capacity-60d",
            execute=True,
            now=started_at,
        )

        self.assertEqual(first["status"], "executed")
        self.assertTrue(first["experiment_created"])
        self.assertEqual(duplicate["status"], "complete")
        self.assertFalse(duplicate["experiment_created"])
        self.assertEqual(first["run_id"], duplicate["run_id"])
        self.assertEqual(len(provider.created), 1)
        epochs = store.capacity_epoch_rows(first["run_id"])
        self.assertEqual([row["status"] for row in epochs], ["complete"])
        self.assertEqual(
            store.run_row(first["run_id"])["status"],
            "complete",
        )
        self.assertEqual(
            store.connection.execute("SELECT COUNT(*) FROM experiment_runs").fetchone()[
                0
            ],
            1,
        )

    def test_capacity_tick_rejects_experiment_plan_changes(self):
        document = plan_document()
        document.update(
            {
                "protocol_version": "5.0-capacity-tick",
                "duration_seconds": 180,
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
        store = TraceStore(self.root / "experiment-change.db")
        self.addCleanup(store.close)
        supervisor = PilotSupervisor(store, {"lambda": CountingCatalogProvider()})
        now = dt.datetime(2026, 8, 11, 0, 0, tzinfo=dt.timezone.utc)
        supervisor.capacity_tick(
            load_plan(self.plan_path),
            experiment_id="fixed-experiment",
            execute=True,
            now=now,
        )

        document["capacity_cohorts"][0]["selection_seed"] = 99
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        with self.assertRaisesRegex(ValueError, "different experiment"):
            supervisor.capacity_tick(
                load_plan(self.plan_path),
                experiment_id="fixed-experiment",
                execute=True,
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
                "canary_cohorts": [],
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
            experiment_id="capacity-missed-slots",
            execute=True,
            now=started_at,
        )
        after_outage = supervisor.capacity_tick(
            plan,
            experiment_id="capacity-missed-slots",
            execute=True,
            now=started_at + dt.timedelta(seconds=181),
        )

        epochs = store.capacity_epoch_rows(first["run_id"])
        self.assertEqual(first["status"], "idle")
        self.assertEqual(len(provider.created), 0)
        self.assertEqual(after_outage["status"], "complete")
        self.assertEqual(
            [row["status"] for row in epochs],
            ["not_run_missed_slot"],
        )
        self.assertEqual(
            epochs[0]["stop_reason"],
            "external_scheduler_late",
        )

    def test_stability_run_persists_all_windows_and_marks_missed_denominators(self):
        document = stability_plan_document()
        document["stability_schedule"] = {
            "window_count": 3,
            "schedule_seed": 29,
            "initial_delay_seconds": 0.05,
            "interval_seconds": 0.3,
            "jitter_seconds": 0,
        }
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "stability-missed.db")
        self.addCleanup(store.close)
        supervisor = PilotSupervisor(
            store,
            {"lambda": FastFailingCanaryProvider()},
        )
        started_at = dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc)

        initialized = supervisor.stability_run(
            plan,
            experiment_id="stability-missed",
            execute=True,
            now=started_at,
        )
        after_outage = PilotSupervisor(
            store,
            {"lambda": FastFailingCanaryProvider()},
        ).stability_run(
            plan,
            experiment_id="stability-missed",
            execute=True,
            now=started_at + dt.timedelta(seconds=0.26),
        )

        windows = store.stability_window_rows(initialized["run_id"])
        self.assertEqual(initialized["status"], "idle")
        self.assertEqual(after_outage["status"], "idle")
        self.assertEqual(len(windows), 3)
        self.assertEqual(
            [row["status"] for row in windows],
            ["missed", "scheduled", "scheduled"],
        )
        self.assertEqual(windows[0]["stop_reason"], "external_scheduler_late")

    def test_stability_run_does_not_complete_a_too_late_zero_pair_window(self):
        document = stability_plan_document()
        document["canary_cohorts"][0]["min_remaining_seconds"] = 0.05
        document["stability_schedule"] = {
            "window_count": 1,
            "schedule_seed": 31,
            "initial_delay_seconds": 0.1,
            "interval_seconds": 0.3,
            "jitter_seconds": 0,
        }
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "stability-too-late.db")
        self.addCleanup(store.close)
        now = dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc)
        provider = FastFailingCanaryProvider()

        initialized = PilotSupervisor(store, {"lambda": provider}).stability_run(
            plan,
            experiment_id="stability-too-late",
            execute=True,
            now=now,
        )
        late = PilotSupervisor(store, {"lambda": provider}).stability_run(
            plan,
            experiment_id="stability-too-late",
            execute=True,
            now=now + dt.timedelta(seconds=0.28),
        )

        windows = store.stability_window_rows(initialized["run_id"])
        self.assertEqual(initialized["status"], "idle")
        self.assertEqual(late["status"], "missed")
        self.assertEqual(late["window_status"], "missed")
        self.assertTrue(late["experiment_complete"])
        self.assertEqual(windows[0]["stop_reason"], "external_scheduler_late")
        self.assertEqual(store.canary_epoch_rows(initialized["run_id"]), [])
        self.assertEqual(provider.created, [])

    def test_stability_run_executes_one_window_and_links_canary_epochs(self):
        document = stability_plan_document()
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "stability-execution.db")
        self.addCleanup(store.close)
        provider = FastFailingCanaryProvider()
        now = dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc)

        result = PilotSupervisor(store, {"lambda": provider}).stability_run(
            plan,
            experiment_id="stability-execution",
            execute=True,
            now=now,
        )
        duplicate = PilotSupervisor(store, {"lambda": provider}).stability_run(
            plan,
            experiment_id="stability-execution",
            execute=True,
            now=now,
        )

        windows = store.stability_window_rows(result["run_id"])
        linked = store.stability_window_epoch_rows(windows[0]["id"])
        self.assertEqual(result["status"], "executed")
        self.assertEqual(result["window_status"], "complete")
        self.assertTrue(result["experiment_complete"])
        self.assertEqual(duplicate["status"], "complete")
        self.assertEqual([row["status"] for row in windows], ["complete"])
        self.assertEqual(len(linked), 1)
        self.assertEqual(linked[0]["sequence"], 0)
        self.assertEqual(len(provider.created), 2)
        self.assertEqual(store.run_row(result["run_id"])["status"], "complete")

    def test_stability_final_window_does_not_hide_cleanup_failure(self):
        document = stability_plan_document()
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "stability-cleanup-failure.db")
        self.addCleanup(store.close)

        result = PilotSupervisor(
            store,
            {"lambda": CleanupFailProvider()},
        ).stability_run(
            plan,
            experiment_id="stability-cleanup-failure",
            execute=True,
            now=dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc),
        )

        self.assertEqual(result["window_status"], "cleanup_incomplete")
        self.assertTrue(result["experiment_complete"])
        self.assertEqual(
            store.stability_experiment_row("stability-cleanup-failure")["status"],
            "cleanup_incomplete",
        )
        self.assertEqual(
            store.run_row(result["run_id"])["status"], "cleanup_incomplete"
        )
        self.assertEqual(len(store.owned_resources(result["run_id"])), 2)

    def test_stability_recovery_preserves_future_windows(self):
        document = stability_plan_document()
        document["stability_schedule"] = {
            "window_count": 2,
            "schedule_seed": 11,
            "initial_delay_seconds": 0.1,
            "interval_seconds": 0.4,
            "jitter_seconds": 0,
        }
        self.plan_path.write_text(json.dumps(document), encoding="utf-8")
        plan = load_plan(self.plan_path)
        store = TraceStore(self.root / "stability-recovery.db")
        self.addCleanup(store.close)
        now = dt.datetime(2026, 8, 11, tzinfo=dt.timezone.utc)
        supervisor = PilotSupervisor(
            store,
            {"lambda": FastFailingCanaryProvider()},
        )
        initialized = supervisor.stability_run(
            plan,
            experiment_id="recoverable-stability",
            execute=True,
            now=now,
        )
        claim_status, claimed = store.claim_due_stability_window(
            "recoverable-stability",
            due_at=(now + dt.timedelta(seconds=0.1)).isoformat(),
        )
        self.assertEqual(claim_status, "claimed")
        store.mark_stability_window_started(
            claimed["id"],
            started_at=(now + dt.timedelta(seconds=0.1)).isoformat(),
        )

        remaining = supervisor.recover_run(initialized["run_id"])

        windows = store.stability_window_rows(initialized["run_id"])
        self.assertEqual(remaining, [])
        self.assertEqual(
            [row["status"] for row in windows],
            ["collector_loss", "scheduled"],
        )
        self.assertEqual(store.run_row(initialized["run_id"])["status"], "running")
        self.assertEqual(
            store.stability_experiment_row("recoverable-stability")["status"],
            "running",
        )


if __name__ == "__main__":
    unittest.main()
