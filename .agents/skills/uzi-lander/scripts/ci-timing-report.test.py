#!/usr/bin/env python3
"""Hermetic timing instrument tests: no API, timers, Docker or test-lane execution."""

import contextlib
import copy
import datetime as dt
import importlib.util
import io
import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch


HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("ci_timing_report", HERE / "ci-timing-report.py")
MOD = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MOD)
FIXTURES = json.loads((HERE / "ci-timing-report.fixtures.json").read_text())
ORIGIN = dt.datetime(2026, 10, 9, tzinfo=dt.timezone.utc)


def at(seconds):
    return (ORIGIN + dt.timedelta(seconds=seconds)).isoformat().replace("+00:00", "Z")


def job(name, ready, start, end, conclusion="success", steps=None):
    return {"name": name, "created_at": at(ready), "started_at": at(start),
            "completed_at": at(end), "status": "completed", "conclusion": conclusion,
            "steps": steps or []}


def run(ident=1, end=100, attempt=1, conclusion="success"):
    return {"id": ident, "created_at": at(0), "updated_at": at(end),
            "run_started_at": at(0), "run_attempt": attempt, "status": "completed",
            "conclusion": conclusion, "head_branch": "main", "event": "push", "head_sha": "a" * 40}


def bundle(jobs=None):
    metadata = run()
    return {"required_contexts": ["test-api"], "target": {"run": metadata,
            "attempts": [{"number": 1, "run": metadata,
                          "jobs": jobs if jobs is not None else [job("test-api", 1, 3, 99)]}]},
            "samples": []}


class TimingTests(unittest.TestCase):
    def test_ordinary_job_and_initial_queue(self):
        report = MOD.report(bundle())
        target = report["target"]
        actual = target["attempts"][0]["jobs"][0]
        self.assertEqual(target["workflow_wall_seconds"], 100)
        self.assertEqual(actual["execution_seconds"], 96)
        self.assertEqual(actual["queue_seconds"], 2)
        self.assertEqual(actual["activation_delay_seconds"], 1)
        self.assertEqual(target["reported_required_ci_completion_seconds"], 99)

    def test_dependency_activation_and_ready_job_queue_are_distinct(self):
        data = bundle([job("build", 50, 70, 90)])
        data["required_contexts"] = ["build"]
        actual = MOD.report(data)["target"]["attempts"][0]["jobs"][0]
        self.assertEqual(actual["activation_delay_seconds"], 50)
        self.assertEqual(actual["queue_seconds"], 20)
        self.assertEqual(actual["execution_seconds"], 20)

    def test_partial_rerun_retains_old_success_without_mixed_attempt_duration(self):
        data = bundle([job("test-api", 0, 1, 40), job("test-agent", 0, 1, 60, "failure")])
        data["required_contexts"] = ["test-api", "test-agent"]
        data["target"]["run"] = run(end=150, attempt=2)
        rerun = run(end=150, attempt=2)
        rerun["run_started_at"] = at(100)
        data["target"]["attempts"].append({"number": 2, "run": rerun,
                                         "jobs": [job("test-agent", 100, 105, 149)]})
        actual = MOD.report(data)["target"]
        self.assertEqual(actual["attempts"][0]["jobs"][0]["execution_seconds"], 59)
        self.assertEqual(actual["attempts"][1]["jobs"][0]["execution_seconds"], 44)
        self.assertEqual(actual["reported_required_ci_completion_seconds"], 149)
        self.assertEqual(actual["attempts"][1]["elapsed_seconds"], 50)

    def test_skipped_job_with_negative_stale_dates_is_untimed(self):
        data = bundle([job("test-api", 100, 90, 20, "skipped")])
        actual = MOD.report(data)["target"]
        skipped = actual["attempts"][0]["jobs"][0]
        self.assertIsNone(skipped["execution_seconds"])
        self.assertIsNone(skipped["queue_seconds"])
        self.assertIsNone(skipped["completion_offset_seconds"])
        self.assertIsNone(actual["reported_required_ci_completion_seconds"])
        self.assertEqual(actual["required_readiness"], "unknown")

    def test_missing_bad_or_naive_timestamps_are_unknown_not_zero(self):
        for value in [None, "", "invalid", "2026-10-09T00:00:00"]:
            with self.subTest(value=value):
                data = bundle()
                data["target"]["run"]["updated_at"] = value
                data["target"]["attempts"][0]["jobs"][0]["created_at"] = value
                actual = MOD.report(data)["target"]
                self.assertIsNone(actual["workflow_wall_seconds"])
                self.assertIsNone(actual["attempts"][0]["jobs"][0]["queue_seconds"])

    def test_unordered_and_missing_step_timestamps(self):
        steps = [job("short", 0, 20, 22), {"name": "missing", "status": "completed"},
                 job("long", 0, 10, 19)]
        actual = MOD.report(bundle([job("test-api", 0, 1, 99, steps=steps)]))
        self.assertEqual([s["seconds"] for s in actual["target"]["attempts"][0]["jobs"][0]["top_steps"]],
                         [9, 2, None])

    def test_live_job_workflow_and_steps_are_not_timed_to_fake_zero(self):
        data = bundle()
        data["target"]["run"]["status"] = "in_progress"
        data["target"]["attempts"][0]["jobs"][0]["status"] = "in_progress"
        actual = MOD.report(data)["target"]
        self.assertIsNone(actual["workflow_wall_seconds"])
        self.assertIsNone(actual["attempts"][0]["jobs"][0]["execution_seconds"])

    def test_required_context_is_ruleset_evidence_not_job_presence(self):
        data = bundle([job("test-api", 0, 1, 40), job("optional", 0, 1, 90)])
        actual = MOD.report(data)["target"]
        self.assertEqual(actual["reported_required_ci_completion_seconds"], 40)
        self.assertEqual(actual["reported_required_contexts"], ["test-api"])
        data["required_contexts"].append("other-workflow")
        actual = MOD.report(data)["target"]
        self.assertEqual(actual["missing_required_contexts"], ["other-workflow"])
        self.assertEqual(actual["required_readiness"], "unknown")

    def test_cancelled_required_check_is_not_ready_but_report_is_advisory(self):
        data = bundle([job("test-api", 0, 1, 90, "cancelled")])
        self.assertEqual(MOD.report(data)["target"]["required_readiness"], "not successful")
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "input.json"
            source.write_text(json.dumps(data))
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(MOD.main(["--input", str(source)]), 0)

    def test_agent_threshold_strictly_greater_than_sixty(self):
        for gap, expected in [(60, False), (61, True)]:
            actual = MOD.report(bundle([job("test-api", 0, 1, 101),
                                       job("test-agent-shard (1/2)", 0, 1, 101 + gap)]))
            self.assertEqual(actual["target"]["rebalance_advisory"], expected)

    def test_worker_uid_lane_counts_as_next_longest_other_job(self):
        actual = MOD.report(bundle([job("test-api", 0, 1, 101),
                                   job("test-agent-shard (1/2)", 0, 1, 162),
                                   job("test-agent-worker-uid", 0, 1, 181)]))
        self.assertEqual(actual["target"]["agent_gap_seconds"], -19)
        self.assertFalse(actual["target"]["rebalance_advisory"])

    def test_saved_measurements_234_721_and_413_queue(self):
        self.assertEqual(MOD.report(FIXTURES["baseline"])["target"]["workflow_wall_seconds"], 234)
        self.assertEqual(MOD.report(FIXTURES["latest"])["target"]["workflow_wall_seconds"], 721)
        queued = MOD.report(FIXTURES["queued_failure"])["target"]["attempts"][0]["jobs"]
        aggregator = next(j for j in queued if j["name"] == "test-agent")
        self.assertEqual(aggregator["queue_seconds"], 413)

    def test_main_sample_exact_size_dates_and_median(self):
        data = bundle()
        for ident, wall in [(2, 200), (3, 250), (4, 300)]:
            metadata = run(ident, wall)
            data["samples"].append({"run": metadata, "attempts": [
                {"number": 1, "run": metadata, "jobs": [job("test-api", 0, 1, wall - 1)]}]})
        actual = MOD.report(data)["comparison"]
        self.assertEqual(actual["sample_size"], 3)
        self.assertEqual(actual["timed_sample_size"], 3)
        self.assertEqual(actual["workflow_median_seconds"], 250)
        self.assertEqual(actual["workflow_delta_seconds"], -150)
        self.assertEqual(actual["samples"][0]["created_at"], at(0))
        self.assertEqual(actual["job_medians"][0]["execution_median_seconds"], 248)

    def test_small_sample_has_no_median_and_names_its_comparison_runs(self):
        for count in range(3):
            data = bundle()
            for ident in range(2, count + 2):
                metadata = run(ident)
                data["samples"].append({"run": metadata, "attempts": [
                    {"number": 1, "run": metadata, "jobs": [job("test-api", 0, 1, 99)]}]})
            actual = MOD.report(data)
            self.assertEqual(actual["comparison"]["status"], "insufficient sample")
            self.assertIsNone(actual["comparison"]["workflow_median_seconds"])
            with contextlib.redirect_stdout(io.StringIO()) as output:
                MOD.render(actual)
            self.assertIn("insufficient sample", output.getvalue())
            self.assertIn("Compared against:", output.getvalue())

    def test_rerun_failure_and_non_main_samples_are_rejected(self):
        for change in [{"run_attempt": 2}, {"conclusion": "failure"},
                       {"head_branch": "feature"}, {"event": "pull_request"}]:
            data = bundle()
            metadata = run(2)
            metadata.update(change)
            data["samples"] = [{"run": metadata, "attempts": [{"number": 1, "run": metadata, "jobs": []}]}]
            with self.assertRaises(MOD.InstrumentError):
                MOD.report(data)

    def test_cache_action_time_is_not_invented_transfer_or_hit(self):
        actual = MOD.report(bundle([job("test-api", 0, 1, 99, steps=[
            job("Run ./.github/actions/go-cache", 0, 2, 20)])]))
        timing = actual["target"]["attempts"][0]["jobs"][0]
        self.assertEqual(timing["cache_action_steps"][0]["seconds"], 18)
        self.assertIsNone(timing["cache_transfer_seconds"])

    def test_existing_junit_only_and_invalid_xml_blocks_instrument(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            path = root / "shard-1.xml"
            path.write_text('<testsuites><testcase name="case" file="test/a.test.ts"/></testsuites>')
            self.assertEqual(MOD.junit_report(root)[0]["files"], 1)
            path.write_text('<testsuites><testcase name="no file metadata"/></testsuites>')
            self.assertIsNone(MOD.junit_report(root)[0]["files"])
            path.write_text("<invalid")
            with self.assertRaises(MOD.InstrumentError):
                MOD.junit_report(root)

    def test_gh_failure_or_bad_json_is_instrument_failure(self):
        for result in [SimpleNamespace(returncode=1, stdout="", stderr="sensitive output"),
                       SimpleNamespace(returncode=0, stdout="bad json", stderr="")]:
            with patch.object(MOD.subprocess, "run", return_value=result):
                with contextlib.redirect_stderr(io.StringIO()) as captured:
                    self.assertEqual(MOD.main(["owner/repo"]), 2)
                self.assertNotIn("sensitive output", captured.getvalue())

    def test_live_collector_only_uses_get_and_keeps_attempts(self):
        metadata = run()
        endpoints = {
            "repos/owner/repo/actions/workflows/ci.yml/runs?branch=main&event=push&status=success&per_page=100":
                {"workflow_runs": [metadata]},
            "repos/owner/repo/actions/runs/1": metadata,
            "repos/owner/repo/rules/branches/main": [{"type": "required_status_checks",
                "parameters": {"required_status_checks": [{"context": "test-api"}]}}],
            "repos/owner/repo/actions/runs/1/attempts/1": metadata,
            "repos/owner/repo/actions/runs/1/attempts/1/jobs?per_page=100":
                [{"jobs": [job("test-api", 0, 1, 99)]}],
        }
        calls = []
        def read_only(args, **_):
            self.assertEqual(args[:4], ["gh", "api", "--method", "GET"])
            calls.append(args)
            return SimpleNamespace(returncode=0, stdout=json.dumps(endpoints[args[4]]), stderr="")
        with patch.object(MOD.subprocess, "run", side_effect=read_only):
            actual = MOD.report(MOD.collect("owner/repo", 1, 5))
        self.assertEqual(len(calls), 5)
        self.assertEqual(actual["target"]["workflow_wall_seconds"], 100)

    def test_offline_cli_performs_no_subprocess_and_latency_does_not_gate(self):
        data = copy.deepcopy(FIXTURES["latest"])
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "input.json"
            source.write_text(json.dumps(data))
            with (patch.object(MOD.subprocess, "run", side_effect=AssertionError("offline network")),
                  contextlib.redirect_stdout(io.StringIO()) as captured):
                self.assertEqual(MOD.main(["--input", str(source), "--json"]), 0)
            self.assertTrue(json.loads(captured.getvalue())["advisory_only"])

    def test_historical_target_compares_only_earlier_main_runs(self):
        target, earlier, later = run(1, 1100), run(2, 100), run(3, 2100)
        target.update(created_at=at(1000), run_started_at=at(1000))
        later.update(created_at=at(2000), run_started_at=at(2000))
        selected = []
        def saved(endpoint, pages=False):
            if "/workflows/" in endpoint:
                return {"workflow_runs": [later, target, earlier]}
            if "/rules/" in endpoint:
                return [{"type": "required_status_checks", "parameters": {
                    "required_status_checks": [{"context": "test-api"}]}}]
            selected.append(endpoint)
            ident = int(endpoint.split("/runs/")[1].split("/")[0])
            metadata = {1: target, 2: earlier, 3: later}[ident]
            if pages:
                return [{"jobs": []}]
            return metadata
        with patch.object(MOD, "gh_json", side_effect=saved):
            actual = MOD.collect("owner/repo", 1, 5)
        self.assertEqual([s["run"]["id"] for s in actual["samples"]], [2])
        self.assertFalse(any("/runs/3/" in endpoint for endpoint in selected))

    def test_invalid_shapes_fail_as_instrument_errors(self):
        for apply in [lambda d: d["target"].update(attempts=[None]),
                      lambda d: d["target"]["attempts"][0].update(jobs=[None]),
                      lambda d: d["target"]["attempts"][0]["jobs"][0].update(steps="invalid")]:
            data = bundle()
            apply(data)
            with self.assertRaises(MOD.InstrumentError):
                MOD.report(data)


if __name__ == "__main__":
    unittest.main()
