"""Fake-Docker host regressions: no daemon, cgroup load, build or install."""
import contextlib
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
    assert float(args[1][:-1]) <= 55
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
        fixtures = [c for c in calls if c[:2] == ["docker", "create"] and "-seed" not in c[c.index("--name") + 1]]
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
        self.assertEqual(len([c for c in calls if c[0:2] == ["docker", "start"]]), 3)
        self.assertTrue(all(r["status"] == "FAILED" and r["hostCleanup"]["confirmed"] for r in reports))
        directory = next((self.root / ".uzi/scratch").iterdir())
        self.assertEqual(len(list(directory.glob("*.stdout"))), 3)  # two raw + final stdout
        self.assertEqual(len(list(directory.glob("*.status"))), 2)
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

    def test_failed_or_unparseable_or_timeout_never_pass_and_stop_next_size(self):
        for mode in ("missing", "malformed", "timeout", "active-failure", "zero-failed"):
            with self.subTest(mode=mode):
                (self.root / "probe-done").unlink(missing_ok=True)
                result, calls = self.invoke(mode)
                self.assertNotEqual(result.returncode, 0, result.stderr)
                reports = check.reports(result.stdout)
                self.assertTrue(all(r["status"] == "FAILED" for r in reports))
                starts = [c for c in calls if c[0] == "timeout" and c[3:5] == ["docker", "start"]]
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
                self.assertEqual(len(entries), number + 1)
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
        self.assertEqual(len([c for c in calls if c[:2] == ["docker", "start"]]), 2)
        self.assertNotIn(b"CALIBRATION PASSED", result.stderr)
        self.assertNotIn(b"\x1b", result.stderr)
        directory = next((self.root / ".uzi/scratch").iterdir())
        raw = next(p for p in directory.glob("*.stdout") if p.name != "reports.stdout").read_bytes()
        self.assertEqual(check.reports(raw, count=1)[0]["status"], poison)
        self.assertEqual(next(directory.glob("*.status")).read_text(), "EXIT=7\n")
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


if __name__ == "__main__":
    unittest.main()
