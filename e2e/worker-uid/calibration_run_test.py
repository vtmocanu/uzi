"""Fake-Docker host regressions: no daemon, cgroup load, build or install."""
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import calibration
import calibration_check as check

HARNESS = Path(__file__).resolve().parent
SCRATCH = HARNESS.parents[1] / ".uzi/scratch"


def fixture_calls(calls):
    """Keep seed/fixture accounting separate from independently owned diagnostics."""
    names = {c[c.index("--name") + 1] for c in calls if c[:2] == ["docker", "create"]
             and "-diag-" in c[c.index("--name") + 1]}
    handles = names | {hashlib.sha256(name.encode()).hexdigest() for name in names}
    return [c for c in calls if not any(arg in handles for arg in c)]

# These programs execute only the literal inert fixture operations below.
STUB = r'''
import hashlib, json, os, sys
from pathlib import Path
args = sys.argv[1:]
kind = Path(sys.argv[0]).name
root = Path(os.environ["FAKE_ROOT"])
with open(root / "calls", "a") as stream:
    stream.write(json.dumps([kind, *args]) + "\n")
mode = os.environ.get("FAKE_MODE", "")
if kind == "git":
    print(str(root.parent) if mode == "export" and args[-1] == "--show-toplevel"
          else str(root) if args[-1] == "--show-toplevel" else "a" * 40)
    sys.exit(0)
if kind == "timeout":
    assert args[0] == "--kill-after=5s"
    assert float(args[1][:-1]) <= 90  # the guarded leg bound (DIAG_GUARDED_SECONDS)
    if mode == "timeout" and args[2:5] == ["docker", "start", "--attach"] and (root / "probe-done").exists():
        sys.exit(124)
    if args[2:5] == ["docker", "start", "--attach"]:
        (root / "probe-done").touch()
    os.execv(str(root / "bin/docker"), [str(root / "bin/docker"), *args[3:]])
assert kind == "docker"
state_file = root / "state.json"
state = json.loads(state_file.read_text()) if state_file.exists() else {}
def save():
    state_file.write_text(json.dumps(state))
if args[:2] == ["image", "inspect"]:
    if mode == "metadata": sys.exit(12)
    print(json.dumps([{"Id": "sha256:" + "c" * 64}]))
elif args[:2] == ["volume", "create"]:
    name = args[-1]
    token = args[args.index("--label") + 1].split("=", 1)[1]
    state[name] = {"Name": name, "Labels": {"io.uzi.worker-uid.invocation": token}}
    save()
    if mode == "volume-lost-ack": sys.exit(9)
    print(name)
elif args[:2] == ["volume", "inspect"]:
    print(json.dumps([state[args[-1]]]))
elif args[:2] == ["volume", "rm"]:
    assert args[-1] in state
    state.pop(args[-1]); save(); print(args[-1])
elif args[0] == "create":
    name = args[args.index("--name") + 1]
    labels = dict(args[i+1].split("=", 1) for i, arg in enumerate(args) if arg == "--label")
    mounts = [dict(part.split("=", 1) for part in args[i+1].split(",") if "=" in part)
              for i, arg in enumerate(args) if arg == "--mount"]
    cid = hashlib.sha256(name.encode()).hexdigest()
    item = {"Id": cid, "Image": "sha256:" + "c" * 64, "Config": {"User": "10003:10003", "Labels": labels},
            "State": {"Running": False}, "HostConfig": {
                "NetworkMode": "none", "CgroupnsMode": "private", "Memory": int(args[args.index("--memory")+1]),
                "MemorySwap": int(args[args.index("--memory-swap")+1]), "CapDrop": ["ALL"],
                "SecurityOpt": ["no-new-privileges"], "CapAdd": []},
            "Mounts": [{"Destination": m["dst"], "Source": m["src"], "RW": "readonly" not in args[i+1]}
                       for i, arg in enumerate(args) if arg == "--mount"
                       for m in [dict(part.split("=", 1) for part in args[i+1].split(",") if "=" in part)]]}
    if mode == "ownership": item["Config"]["Labels"] = {}
    if mode == "caps": item["HostConfig"]["CapAdd"] = ["SYS_ADMIN"]
    masked = {args[i + 1].split(":")[0] for i, arg in enumerate(args) if arg == "--tmpfs"}
    for volume in ("/data", "/nix"):
        if volume not in masked:
            state["anonymous:" + cid + volume] = {"container": cid}
    state[name] = item
    save()
    fixture_number = 1 if name.endswith("-1073741824") else 2
    if mode == "lost-ack" or (mode == "fixture-lost-ack-" + str(fixture_number) and not name.endswith("-seed")):
        journal = next((root / ".uzi/scratch").glob("*/requests.jsonl")).read_text()
        assert name in journal
        sys.exit(8)
    print(cid)
elif args[0] == "inspect":
    found = [v for k, v in state.items() if k == args[-1] or v.get("Id") == args[-1]]
    assert len(found) == 1
    print(json.dumps(found))
elif args[0] == "rm":
    assert len(args[-1]) == 64
    found = [k for k,v in state.items() if v.get("Id") == args[-1]]
    assert len(found) == 1
    state.pop(found[0])
    if "--volumes" in args:
        state = {k:v for k,v in state.items() if v.get("container") != args[-1]}
    save()
    print("wrong" if mode == "remove-ack" else args[-1])
elif args[0] == "start":
    item = next(v for v in state.values() if v.get("Id") == args[-1])
    name = next(k for k, v in state.items() if v is item or v == item)
    if "-diag-" in name:
        workload = name.split("-diag-")[1].rsplit("-", 1)[0]
        if mode == "diag-garbage": print("not a diagnostic"); sys.exit(0)
        if workload.startswith("guarded-"):
            if mode == "guarded-killed": sys.exit(137)
            if mode == "guarded-hung": sys.exit(124)
            tool = {"toolUseId": "toolu_x", "isError": True, "outcome": "guard_acted", "pressureToolIdMatches": True,
                    "pressure": {"code": "worker_memory_pressure", "tool_id": "toolu_x", "outcome": "confirmed_drained"}, "head": None}
            if mode == "guarded-oom-before-guard":
                tool.update(outcome="oom_before_guard", pressure=None, pressureToolIdMatches=None, head="Exit code: 137")
            if mode.startswith("guarded-bad-"):
                tool.update(outcome=mode[len("guarded-bad-"):], pressure=None, pressureToolIdMatches=None, head="x")
            if mode == "guarded-wrong-tool": tool["pressure"]["tool_id"] = "toolu_other"
            cleanup = {"liveCountBeforeQuiesce": 0, "liveCountAfter": 0, "quiesceConfirmed": True, "poisoned": False,
                       "registryEligibleEntries": 0, "commandPidsGone": True, "otherProcessPidsRemaining": [],
                       "oomKillBefore": 0, "oomKillAfter": 1 if mode == "guarded-oom-before-guard" else 0, "memoryPeakBytes": 4096}
            if mode == "guarded-leak": cleanup["otherProcessPidsRemaining"] = [77]
            if mode == "guarded-unquiesced": cleanup["quiesceConfirmed"] = False
            print("UZI_NODE_DIAGNOSTIC_BEGIN")
            print(json.dumps({"schemaVersion": 1, "workload": workload, "leg": "guarded", "nodeVersion": "v24.0.0",
                              "psiSynthesized": True, "baselineBytes": 100,
                              "growth": {"series": [[1, 3.0, 100], [2, 250.0, 700]], "sampleCount": 2, "peakSeriesBytes": 700},
                              "timeline": {"crossingSampleCompletedMs": 250.0}, "budget": {"crossingToReserveRequestMs": 5.0,
                              "cancelToConfirmedDrainMs": 55.0, "crossingToConfirmedDrainMs": 110.0},
                              "tool": tool, "cleanup": cleanup}))
            print("UZI_NODE_DIAGNOSTIC_END")
            sys.exit(0 if mode != "diag-nonzero" else 3)
        if mode == "diag-two-lines":
            print("UZI_NODE_DIAGNOSTIC_BEGIN\n{}\n{}\nUZI_NODE_DIAGNOSTIC_END"); sys.exit(0)
        child = {"exitCode": 0, "signal": None, "timedOut": False, "wallMs": 700,
                 "reported": {"outcome": "assertion", "messageBytes": 12345, "mismatches": None, "maxRssKb": 1, "ms": 5}}
        cgroup = {"memoryPeakBytes": 4096, "oomKill": 0}
        if mode == "diag-oom":
            child.update(exitCode=None, signal="SIGKILL", reported=None); cgroup["oomKill"] = 1
        if mode == "diag-timeout":
            child.update(exitCode=None, signal="SIGKILL", timedOut=True, reported=None)
        print("UZI_NODE_DIAGNOSTIC_BEGIN")
        print(json.dumps({"schemaVersion": 1, "workload": workload, "nodeVersion": "v24.0.0", "pairs": 1,
                          "childDeadlineMs": 1, "child": child, "cgroup": cgroup}))
        print("UZI_NODE_DIAGNOSTIC_END")
        sys.exit(0 if mode != "diag-nonzero" else 3)
    if item["Config"]["Labels"].get("io.uzi.memory.fixture") is None:
        sys.exit(23 if mode == "layout-collision" else 0)
    assert args[1] == "--attach"
    item = next(v for v in state.values() if v.get("Id") == args[-1])
    mount = next(m for m in item["Mounts"] if m["Destination"] == "/expected.json")
    manifest = json.loads(Path(mount["Source"]).read_text())
    expected = next(f for f in manifest["fixtures"] if f["containerId"] == args[-1])
    if mode == "missing": sys.exit(0)
    if mode == "malformed": print("UZI_MEMORY_CALIBRATION_REPORT_BEGIN\n{"); sys.exit(0)
    report = {"schemaVersion": 1, **manifest["identity"], **expected, "provenance": "measured",
              "status": "FAILED", "reasons": ["actual default reader unavailable: absent"],
              "observed": None, "guard": None, "plan": None, "clock": None, "bounds": None,
              "baseline": [], "samples": [], "rawSamples": [{"sample": {"available": False, "reason": "absent"},
                                                          "raw": {"/sys/fs/cgroup/memory.pressure": None}, "events": None}],
              "response": None, "derivation": None, "trees": [], "settlement": None, "cleanup": None,
              "outcome": {"status": "failed", "successor": "aborted"}, "finalCleanup": {"handles": 0}}
    if mode == "active-failure": report["finalCleanup"]["handles"] = 2
    if mode == "invalid-status":
        report["status"] = os.environ["POISON"]
        report["reasons"] = []
    print("UZI_MEMORY_CALIBRATION_REPORT_BEGIN")
    print(json.dumps(report, separators=(",", ":")))
    print("UZI_MEMORY_CALIBRATION_REPORT_END")
    sys.exit(0 if mode == "zero-failed" else 7)
else:
    raise AssertionError(args)
'''


class RunnerTest(unittest.TestCase):
    def setUp(self):
        SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="calibration-test.", dir=SCRATCH)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        harness = self.root / "e2e/worker-uid"
        harness.mkdir(parents=True)
        for name in ("calibration.py", "calibration_check.py", "bounded.py"):
            shutil.copyfile(HARNESS / name, harness / name)
        (self.root / "bin").mkdir()
        for name in ("git", "docker", "timeout"):
            file = self.root / "bin" / name
            file.write_text("#!" + sys.executable + "\n" + STUB)
            file.chmod(0o700)

    def invoke(self, mode="", ci=False, extra_env=None):
        env = {**os.environ, "PATH": str(self.root / "bin") + os.pathsep + os.defpath,
               "FAKE_ROOT": str(self.root), "FAKE_MODE": mode}
        env.pop("GITHUB_RUN_ID", None)
        env.pop("GITHUB_RUN_ATTEMPT", None)
        env.update(extra_env or {})
        args = [sys.executable, str(self.root / "e2e/worker-uid/calibration.py"),
                "--image", "mem2396-offline:complete", "--mode", "ci" if ci else "local"]
        if not ci:
            args += ["--run-id", "explicit-local", "--attempt", "1"]
        result = subprocess.run(args, env=env, capture_output=True, timeout=20)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        return result, calls

    def test_no_implicit_volumes_survive(self):
        _, calls = self.invoke()
        self.assertTrue(any(c[:2] == ["docker", "create"] for c in calls))
        self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
        for command in [c for c in calls if c[:2] == ["docker", "create"]]:
            mounts = {command[i + 1].split(":")[0] for i, arg in enumerate(command) if arg == "--tmpfs"}
            self.assertTrue({"/data", "/nix"}.issubset(mounts))

    def test_no_dependency_volume_copy(self):
        _, calls = self.invoke()
        self.assertFalse(any(c[:2] == ["docker", "volume"] for c in calls),
                         "calibration must use image-native dependencies without copy-up")
        fixtures = [c for c in fixture_calls(calls) if c[:2] == ["docker", "create"] and "-seed" not in c[c.index("--name") + 1]]
        self.assertEqual(len(fixtures), 2)
        for command in fixtures:
            mounts = [command[i + 1] for i, arg in enumerate(command) if arg == "--mount"]
            code = [m for m in mounts if ",dst=/app/" in m]
            self.assertTrue(code)
            self.assertTrue(all(",dst=/app/uzi-worker-uid-proof/" in m and m.endswith(",readonly") for m in code))
            self.assertIn("/app/node_modules/tsx/dist/loader.mjs", command)

    def test_failed_prerequisite_both_sizes_raw_and_final_cleanup(self):
        result, calls = self.invoke()
        self.assertNotEqual(result.returncode, 0, result.stderr)
        reports = check.reports(result.stdout)
        self.assertEqual(len([c for c in calls if c[0:2] == ["docker", "start"]]), 11)
        self.assertTrue(all(r["status"] == "FAILED" and r["hostCleanup"]["confirmed"] for r in reports))
        directory = next((self.root / ".uzi/scratch").iterdir())
        self.assertEqual(len(list(directory.glob("*.stdout"))), 11)  # two raw + eight diagnostic + final stdout
        self.assertEqual(len(list(directory.glob("*.status"))), 10)
        self.assertEqual(json.loads((directory / "host-cleanup.json").read_text()),
                         {r["fixtureId"]: r["hostCleanup"] for r in reports})
        state = json.loads((self.root / "state.json").read_text())
        self.assertEqual(state, {})
        docker = [c for c in calls if c[0] == "docker"]
        watchdogs = [c for c in calls if c[0] == "timeout"]
        self.assertEqual(len(docker), len(watchdogs))
        for call in docker:
            self.assertNotIn("build", call)
            self.assertNotIn("pull", call)
            if call[1] == "create":
                self.assertEqual(call[call.index("--network") + 1], "none")
                self.assertEqual(call[call.index("--pull") + 1], "never")
                self.assertEqual(call[call.index("--cap-drop") + 1], "ALL")
            if call[1] == "rm":
                self.assertEqual(len(call[-1]), 64)

    def test_failed_prerequisite_still_starts_plain_and_guarded_diagnostics(self):
        result, calls = self.invoke()
        self.assertNotEqual(result.returncode, 0, result.stderr)
        reports = check.reports(result.stdout)
        self.assertTrue(all(any("actual default reader unavailable:" in reason for reason in report["reasons"])
                            for report in reports), "fixture failure remains recorded")
        directory = next((self.root / ".uzi/scratch").iterdir())
        self.assertTrue((directory / "node-diagnostic.json").exists(),
                        "fixture prerequisite failure must still run diagnostics")
        measured = json.loads((directory / "node-diagnostic.json").read_text())
        self.assertEqual([(r["limitBytes"], r["workload"]) for r in measured],
                         [(size, workload) for size in calibration.DIAG_SIZES
                          for workload in calibration.DIAG_WORKLOADS + calibration.DIAG_GUARDED_WORKLOADS])
        guarded = [r for r in measured if r.get("leg") == "guarded"]
        self.assertEqual(len(guarded), 2)
        self.assertTrue(all(r["psiSynthesized"] is True for r in guarded))
        creates = [c for c in calls if c[:2] == ["docker", "create"]
                   and "-diag-" in c[c.index("--name") + 1]]
        starts = {c[-1] for c in calls if c[:2] == ["docker", "start"]}
        self.assertEqual(len(creates), 8)
        for command in creates:
            name = command[command.index("--name") + 1]
            self.assertIn(hashlib.sha256(name.encode()).hexdigest(), starts)
        self.assertEqual(json.loads((self.root / "state.json").read_text()), {})

    def test_failed_or_unparseable_or_timeout_never_pass_and_stop_next_size(self):
        for mode in ("missing", "malformed", "timeout", "active-failure", "zero-failed"):
            with self.subTest(mode=mode):
                (self.root / "probe-done").unlink(missing_ok=True)
                result, calls = self.invoke(mode)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                reports = check.reports(result.stdout)
                self.assertTrue(all(r["status"] == "FAILED" for r in reports))
                starts = [c for c in fixture_calls(calls) if c[0] == "timeout" and c[3:5] == ["docker", "start"]]
                self.assertEqual(len(starts), 3 if mode == "zero-failed" else 2)
                self.assertTrue(all(r["hostCleanup"] is not None for r in reports))
                # A fresh invocation owns different names; previous state must be empty.
                self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
                (self.root / "calls").write_text("")

    def test_creation_lost_ack_and_ownership_fail_closed(self):
        for mode in ("lost-ack", "layout-collision", "ownership", "caps", "metadata", "remove-ack"):
            with self.subTest(mode=mode):
                (self.root / "probe-done").unlink(missing_ok=True)
                result, calls = self.invoke(mode)
                calls = fixture_calls(calls)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                check.reports(result.stdout)  # explicit aborted report for both sizes
                self.assertLessEqual(sum(c[:2] == ["docker", "start"] for c in calls), 1)
                removes = [c for c in calls if c[:2] == ["docker", "rm"]]
                if mode in ("ownership", "metadata"):
                    self.assertEqual(removes, [])
                if mode == "lost-ack":
                    self.assertEqual(len(removes), 1)
                    self.assertEqual(len(removes[0][-1]), 64)
                    directory = sorted((self.root / ".uzi/scratch").iterdir(), key=lambda p: p.stat().st_mtime)[-1]
                    self.assertIn('"kind": "container"', (directory / "requests.jsonl").read_text())
                (self.root / "calls").write_text("")
                (self.root / "state.json").write_text("{}")

    def test_fixture_lost_ack_cleans_observed_cids_without_start_or_retry(self):
        for number in (1, 2):
            with self.subTest(number=number):
                (self.root / "calls").write_text("")
                (self.root / "state.json").write_text("{}")
                result, calls = self.invoke("fixture-lost-ack-" + str(number))
                calls = fixture_calls(calls)
                self.assertNotEqual(result.returncode, 0)
                reports = check.reports(result.stdout)
                self.assertEqual(len(reports), 2)
                self.assertTrue(all(r["status"] == "FAILED" and r["observed"] is None for r in reports))
                self.assertLessEqual(sum(c[:2] == ["docker", "start"] for c in calls), 1)
                creates = [c for c in calls if c[:2] == ["docker", "create"]]
                removes = [c for c in calls if c[:2] == ["docker", "rm"]]
                self.assertEqual(len(creates), number + 1)
                self.assertEqual(len(removes), number + 1)
                self.assertEqual(len({c[-1] for c in removes}), number + 1)
                self.assertTrue(all(len(c[-1]) == 64 for c in removes))
                self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
                self.assertEqual(len([c for c in calls if c[:3] == ["docker", "volume", "rm"]]), 0)
                directory = sorted((self.root / ".uzi/scratch").iterdir(), key=lambda p: p.stat().st_mtime)[-1]
                entries = [json.loads(line) for line in (directory / "requests.jsonl").read_text().splitlines()]
                self.assertEqual(len([entry for entry in entries if "-diag-" not in entry["name"]]), number + 1)
                self.assertTrue((directory / "docker-output").exists())
                for report in reports[:number]:
                    proof = report["hostCleanup"]
                    self.assertTrue(proof["confirmed"])
                    self.assertEqual(proof["containerId"], proof["removedId"])
                    self.assertIn(proof["removedId"], [c[-1] for c in removes])
                self.assertNotIn("containerId", reports[number - 1])
                (self.root / "calls").write_text("")

    def test_cleanup_refuses_known_cid_mismatch_and_foreign_labels(self):
        manifest = calibration.create_manifest("local", "local", "1", "a" * 40)
        fixture = manifest["fixtures"][0]
        for field in ("cid", "fixture", "invocation"):
            with self.subTest(field=field):
                item = {"Id": "b" * 64, "Config": {"Labels": {
                    calibration.LABEL: manifest["invocation"],
                    "io.uzi.memory.fixture": fixture["fixtureId"]}}, "State": {"Running": False}}
                fixture.pop("containerId", None)
                if field == "cid":
                    fixture["containerId"] = "c" * 64
                else:
                    key = calibration.LABEL if field == "invocation" else "io.uzi.memory.fixture"
                    item["Config"]["Labels"][key] = "foreign"
                host = calibration.Host(manifest, self.root, "unused")
                with patch.object(host, "docker", return_value=json.dumps([item]).encode()) as docker:
                    with self.assertRaises(ValueError):
                        host.cleanup_container("planned-name", fixture)
                self.assertEqual(docker.call_count, 1)
                self.assertEqual(host.cleanup_proofs, {})

    def test_invalid_status_cannot_forge_diagnostics_or_success(self):
        poison = "FAILED\nCALIBRATION PASSED\x1b[2J\u009b2J\u202e\u2066\u2028\u2029" + "x" * 10000
        result, calls = self.invoke("invalid-status", extra_env={"POISON": poison})
        self.assertNotEqual(result.returncode, 0)
        reports = check.reports(result.stdout)
        self.assertEqual(len(reports), 2)
        self.assertTrue(all(r["status"] == "FAILED" for r in reports))
        self.assertIn("producer status", reports[0]["reasons"][0])
        self.assertEqual(len([c for c in fixture_calls(calls) if c[:2] == ["docker", "start"]]), 2)
        self.assertNotIn(b"CALIBRATION PASSED", result.stderr)
        self.assertNotIn(b"\x1b", result.stderr)
        directory = next((self.root / ".uzi/scratch").iterdir())
        raw = (directory / (reports[0]["fixtureId"] + ".stdout")).read_bytes()
        self.assertEqual(check.reports(raw, count=1)[0]["status"], poison)
        self.assertEqual((directory / (reports[0]["fixtureId"] + ".status")).read_text(), "EXIT=7\n")
        manifest = json.loads((directory / "expected.json").read_text())
        for report, fixture in zip(reports, manifest["fixtures"]):
            for key, value in {**manifest["identity"], **fixture}.items():
                self.assertEqual(report[key], value)
        self.assertEqual(json.loads((self.root / "state.json").read_text()), {})

    def test_host_diagnostic_rendering_is_single_line_and_byte_bounded(self):
        poison = "error\nforged\r\t\x00\x1b[2J\x7f\u009b2J\u202e\u2066\u200b\u2028\u2029" + "é" * 10000
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            calibration.diagnostic("calibration FAIL: " + poison)
        rendered = stderr.getvalue()
        self.assertEqual(rendered.count("\n"), 1)
        self.assertLessEqual(len(rendered.rstrip("\n").encode()), 2048)
        self.assertIn("[truncated]", rendered)
        for char in ("\r", "\t", "\x00", "\x1b", "\x7f", "\u009b", "\u202e", "\u2066", "\u200b", "\u2028", "\u2029"):
            self.assertNotIn(char, rendered)
        self.assertIn("forged", rendered)

    def test_diagnostics_do_not_truncate_json_failure_evidence(self):
        manifest = calibration.create_manifest("local", "local", "1", "a" * 40)
        directory = self.root / "evidence"
        directory.mkdir()
        host = calibration.Host(manifest, directory, "unused")
        error = ValueError("inherited\n\x1b[2J\u202e\u2029" + "é" * 10000)
        with contextlib.redirect_stdout(io.TextIOWrapper(io.BytesIO(), encoding="utf8")):
            host.publish(error)
        reports = check.reports((directory / "reports.stdout").read_bytes())
        self.assertEqual(len(reports), 2)
        for report in reports:
            self.assertEqual(report["status"], "FAILED")
            self.assertEqual(report["reasons"][-1], "host failure: " + str(error))
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            calibration.diagnostic("calibration FAIL: " + str(error))
        self.assertLessEqual(len(stderr.getvalue().encode()), 2049)
        self.assertNotIn("\x1b", stderr.getvalue())
        self.assertEqual(stderr.getvalue().count("\n"), 1)

    def test_argument_diagnostics_are_also_sanitized(self):
        parser = calibration.DiagnosticParser(prog="calibration")
        parser.add_argument("--mode", choices=("ci", "local"))
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit):
            parser.parse_args(["--mode", "bad\n\x1b[2J\u202e\u2029" + "é" * 10000])
        for line in stderr.getvalue().splitlines():
            self.assertLessEqual(len(line.encode()), 2048)
        for char in ("\x1b", "\u202e", "\u2029"):
            self.assertNotIn(char, stderr.getvalue())

    def test_export_root_refused_before_head_or_docker(self):
        result, calls = self.invoke("export")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"actual checkout root", result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][-1], "--show-toplevel")

    def test_ci_identity_must_come_from_nonempty_host_environment(self):
        result, calls = self.invoke(ci=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c[0] == "docker" for c in calls))
        with patch.dict(os.environ, {"GITHUB_RUN_ID": "host", "GITHUB_RUN_ATTEMPT": "2"}):
            with self.assertRaises(ValueError):
                calibration.create_manifest("ci", "container", "2", "a" * 40)
            manifest = calibration.create_manifest("ci", "host", "2", "a" * 40)
            self.assertEqual(manifest["identity"]["runId"], "host")

    def test_producer_cannot_supply_host_proof(self):
        directory = self.root / "evidence"
        directory.mkdir()
        manifest = calibration.create_manifest("local", "local", "1", "a" * 40)
        host = calibration.Host(manifest, directory, "unused")
        fixture = manifest["fixtures"][0]
        report = host.aborted(fixture, "fixture failure")
        report.update(status="SUCCESS_COMPLETE", reasons=[], hostCleanup={"confirmed": True})
        host.results[fixture["fixtureId"]] = report
        with contextlib.redirect_stdout(io.TextIOWrapper(io.BytesIO(), encoding="utf8")):
            host.publish(ValueError("abort"))
        retained = check.reports((directory / "reports.stdout").read_bytes())
        self.assertEqual(retained[0]["status"], "FAILED")
        self.assertIn("producer supplied", retained[0]["reasons"][0])

    def diagnostic_host(self, mode):
        manifest = calibration.create_manifest("local", "local", "1", "a" * 40)
        manifest["imageId"] = "sha256:" + "c" * 64
        directory = self.root / "diagnostic-evidence"
        directory.mkdir(exist_ok=True)
        (self.root / "calls").write_text("")
        env = {"PATH": str(self.root / "bin") + os.pathsep + os.defpath, "FAKE_ROOT": str(self.root), "FAKE_MODE": mode}
        return calibration.Host(manifest, directory, "unused"), directory, patch.dict(os.environ, env)

    def test_diagnostics_measure_eight_owned_isolated_containers_and_clean_all(self):
        host, directory, env = self.diagnostic_host("")
        with env, contextlib.redirect_stderr(io.StringIO()) as stderr:
            results = host.measure_diagnostics()
        self.assertEqual([(r["limitBytes"], r["workload"]) for r in results],
                         [(size, w) for size in calibration.DIAG_SIZES
                          for w in calibration.DIAG_WORKLOADS + calibration.DIAG_GUARDED_WORKLOADS])
        self.assertEqual(json.loads((directory / "node-diagnostic.json").read_text()), results)
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        creates = [c for c in calls if c[:2] == ["docker", "create"]]
        self.assertEqual(len(creates), 8)
        for command in creates:
            self.assertEqual(command[command.index("--network") + 1], "none")
            self.assertEqual(command[command.index("--user") + 1], "10003:10003")
            self.assertEqual(command[command.index("--cap-drop") + 1], "ALL")
            self.assertEqual(command[command.index("--memory") + 1], command[command.index("--memory-swap") + 1])
            self.assertEqual(command[command.index("--pids-limit") + 1], "128")
            self.assertEqual(command[command.index("--cgroupns") + 1], "private")
            self.assertTrue(command[-2].endswith("calibration-diagnostic.mts"))
        # Only the guarded containers mount the agent sources the real lane imports (read-only).
        guarded = [c for c in creates if c[-1] in calibration.DIAG_GUARDED_WORKLOADS]
        self.assertEqual(len(guarded), 2)
        for command in guarded:
            mounts = [command[i + 1] for i, arg in enumerate(command) if arg == "--mount"]
            self.assertTrue(any(m.endswith("/agent/src,readonly") for m in mounts))
        for command in set(map(tuple, creates)) - set(map(tuple, guarded)):
            self.assertFalse(any(arg.endswith("/agent/src,readonly") for arg in command))
        self.assertEqual(len([c for c in calls if c[0] == "timeout" and c[3:5] == ["docker", "start"]]), 8)
        self.assertEqual(len([c for c in calls if c[:2] == ["docker", "rm"]]), 8)
        self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
        self.assertEqual(host.requests, [])
        self.assertEqual(stderr.getvalue().count("node diagnostic "), 8)
        self.assertEqual(stderr.getvalue().count("cancel_to_drain_ms=55.0"), 2)
        # The node version is measurement only (no required major): captured in the JSON and the summary.
        for result in results:
            self.assertIsInstance(result["nodeVersion"], str)
            self.assertTrue(result["nodeVersion"])
        self.assertEqual(stderr.getvalue().count("node=" + results[0]["nodeVersion"]), 8)
        journal = [json.loads(line) for line in (directory / "requests.jsonl").read_text().splitlines()]
        self.assertEqual(len(journal), 8)
        # The guarded measurement keeps its tool result and says whether PSI was synthesized.
        for result in [r for r in results if r.get("leg") == "guarded"]:
            self.assertEqual(result["tool"]["pressure"]["code"], "worker_memory_pressure")
            self.assertIs(result["psiSynthesized"], True)

    def test_diagnostic_oom_or_timeout_is_a_recorded_measurement_not_failure(self):
        for mode in ("diag-oom", "diag-timeout"):
            with self.subTest(mode=mode):
                host, directory, env = self.diagnostic_host(mode)
                with env, contextlib.redirect_stderr(io.StringIO()) as stderr:
                    results = host.measure_diagnostics()
                self.assertEqual(len(results), 8)
                self.assertIn("oom_killed" if mode == "diag-oom" else "timed_out", stderr.getvalue())
                self.assertEqual(json.loads((self.root / "state.json").read_text()), {})

    def test_guarded_measurement_outcomes_are_recorded_not_failures(self):
        for mode, outcome in (("guarded-oom-before-guard", "oom_before_guard"), ("guarded-killed", "container_killed_no_report")):
            with self.subTest(mode=mode):
                (self.root / "state.json").write_text("{}")
                host, directory, env = self.diagnostic_host(mode)
                with env, contextlib.redirect_stderr(io.StringIO()) as stderr:
                    results = host.measure_diagnostics()
                guarded = [r for r in results if r.get("leg") == "guarded"]
                self.assertEqual([r["tool"]["outcome"] for r in guarded], [outcome, outcome])
                self.assertIn(outcome, stderr.getvalue())
                self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
                self.assertEqual(host.requests, [])

    def test_guarded_harness_faults_fail_after_owned_cleanup(self):
        # Reaching the guarded container means the three plain workloads of the first size ran first.
        for mode in ("guarded-hung", "guarded-leak", "guarded-unquiesced", "guarded-wrong-tool", "guarded-bad-other",
                     "guarded-bad-launch_failed", "guarded-bad-result_mismatch", "guarded-bad-supervision_failed",
                     "guarded-bad-enroll_failed", "guarded-bad-admission_closed", "guarded-bad-workload_not_started"):
            with self.subTest(mode=mode):
                (self.root / "state.json").write_text("{}")
                host, directory, env = self.diagnostic_host(mode)
                with env, contextlib.redirect_stderr(io.StringIO()), self.assertRaises(ValueError):
                    host.measure_diagnostics()
                calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
                creates = [c for c in calls if c[:2] == ["docker", "create"]]
                self.assertEqual(len(creates), 4)
                self.assertTrue(creates[-1][-1] in calibration.DIAG_GUARDED_WORKLOADS)
                self.assertEqual(len([c for c in calls if c[:2] == ["docker", "rm"]]), 4)
                self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
                self.assertTrue((directory / "node-diagnostic.json").exists())

    def test_diagnostic_nonzero_host_exit_is_recorded(self):
        host, _, env = self.diagnostic_host("diag-nonzero")
        with env, contextlib.redirect_stderr(io.StringIO()):
            results = host.measure_diagnostics()
        self.assertTrue(all(r["hostExit"] == 3 for r in results))

    def test_unparseable_diagnostic_fails_after_owned_cleanup(self):
        for mode in ("diag-garbage", "diag-two-lines"):
            with self.subTest(mode=mode):
                (self.root / "state.json").write_text("{}")
                host, directory, env = self.diagnostic_host(mode)
                with env, contextlib.redirect_stderr(io.StringIO()), self.assertRaises(ValueError):
                    host.measure_diagnostics()
                calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
                self.assertEqual(len([c for c in calls if c[:2] == ["docker", "create"]]), 1)
                self.assertEqual(len([c for c in calls if c[:2] == ["docker", "rm"]]), 1)
                self.assertEqual(json.loads((self.root / "state.json").read_text()), {})
                self.assertTrue((directory / "node-diagnostic.json").exists())

    def test_diagnostic_ownership_failure_cleans_nothing_foreign_and_fails(self):
        host, _, env = self.diagnostic_host("ownership")
        with env, contextlib.redirect_stderr(io.StringIO()), self.assertRaises(ValueError):
            host.measure_diagnostics()
        calls = [json.loads(line) for line in (self.root / "calls").read_text().splitlines()]
        self.assertEqual([c for c in calls if c[:2] == ["docker", "rm"]], [])
        self.assertFalse(any(c[:2] == ["docker", "start"] for c in calls))

    def test_diagnostic_failure_leaves_reports_unchanged_when_cleanup_succeeds_but_fails_the_run(self):
        events = []
        class FakeHost:
            def __init__(self, manifest, directory, image):
                self.manifest, self.directory = manifest, directory
            def run(self): events.append("run"); self.manifest["imageId"] = "sha256:" + "c" * 64
            def measure_diagnostics(self): events.append("measure"); raise ValueError("diagnostic harness broke")
            def finish(self): events.append("finish")
            def publish(self, error=None): events.append(("publish", error))
        argv = ["calibration.py", "--image", "x", "--mode", "local", "--run-id", "r", "--attempt", "1"]
        with patch.object(calibration, "Host", FakeHost), patch.object(calibration, "SCRATCH", self.root / "scratch"), \
                patch.object(sys, "argv", argv), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaisesRegex(ValueError, "node diagnostic: diagnostic harness broke"):
                calibration.main()
        self.assertEqual(events, ["run", "measure", "finish", ("publish", None)])

    def test_fixture_failure_only_measures_after_image_resolution(self):
        for provisioned in (False, True):
            with self.subTest(provisioned=provisioned):
                events = []
                class FakeHost:
                    def __init__(self, manifest, directory, image): self.manifest = manifest
                    def run(self):
                        events.append("run")
                        if provisioned: self.manifest["imageId"] = "sha256:" + "c" * 64
                        raise ValueError("fixture failed")
                    def measure_diagnostics(self): events.append("measure")
                    def finish(self): events.append("finish")
                    def publish(self, error=None): events.append(("publish", str(error)))
                argv = ["calibration.py", "--image", "x", "--mode", "local", "--run-id", "r", "--attempt", "1"]
                with patch.object(calibration, "Host", FakeHost), patch.object(calibration, "SCRATCH", self.root / "scratch"), \
                        patch.object(sys, "argv", argv), contextlib.redirect_stderr(io.StringIO()):
                    with self.assertRaisesRegex(ValueError, "fixture failed"):
                        calibration.main()
                self.assertEqual(events, ["run", *(["measure"] if provisioned else []),
                                          "finish", ("publish", "fixture failed")])

    def test_interrupts_cleanup_without_measurement_continuation_or_publication(self):
        for step in ("run", "measure"):
            with self.subTest(step=step):
                events = []
                class FakeHost:
                    def __init__(self, manifest, directory, image): self.manifest = manifest
                    def run(self):
                        events.append("run")
                        self.manifest["imageId"] = "sha256:" + "c" * 64
                        if step == "run": raise KeyboardInterrupt()
                    def measure_diagnostics(self): events.append("measure"); raise KeyboardInterrupt()
                    def finish(self): events.append("finish")
                    def publish(self, error=None): events.append("publish")
                argv = ["calibration.py", "--image", "x", "--mode", "local", "--run-id", "r", "--attempt", "1"]
                with patch.object(calibration, "Host", FakeHost), patch.object(calibration, "SCRATCH", self.root / "scratch"), \
                        patch.object(sys, "argv", argv), self.assertRaises(KeyboardInterrupt):
                    calibration.main()
                self.assertEqual(events, ["run", *(["measure"] if step == "measure" else []), "finish"])

    def test_diagnostic_cleanup_failure_publishes_both_reports_failed(self):
        events = []
        class FakeHost:
            def __init__(self, manifest, directory, image):
                self.manifest, self.directory = manifest, directory
            def run(self): events.append("run"); self.manifest["imageId"] = "sha256:" + "c" * 64
            def measure_diagnostics(self): events.append("measure"); raise ValueError("diagnostic harness broke")
            def finish(self): events.append("finish"); raise ValueError("container cleanup failed")
            def publish(self, error=None): events.append(("publish", str(error)))
        argv = ["calibration.py", "--image", "x", "--mode", "local", "--run-id", "r", "--attempt", "1"]
        with patch.object(calibration, "Host", FakeHost), patch.object(calibration, "SCRATCH", self.root / "scratch"), \
                patch.object(sys, "argv", argv), contextlib.redirect_stderr(io.StringIO()):
            with self.assertRaisesRegex(ValueError, "cleanup: container cleanup failed"):
                calibration.main()
        self.assertEqual(events, ["run", "measure", "finish", ("publish", "None; cleanup: container cleanup failed")])


if __name__ == "__main__":
    unittest.main()
