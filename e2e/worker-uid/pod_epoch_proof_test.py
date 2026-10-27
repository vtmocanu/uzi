#!/usr/bin/env python3
"""Docker-free orchestration tests; fake commands never execute probe payloads."""
import errno
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest

ROOT = Path(__file__).resolve().parents[2]
HEAD = "a" * 40
IDS = {"base": "sha256:" + "b" * 64, "jvm": "sha256:" + "c" * 64}
FAKE = r'''#!/usr/bin/env python3
import json
import os
from pathlib import Path
import sys

command = Path(sys.argv[0]).name
args = sys.argv[1:]
state_file = Path(os.environ["FAKE_STATE"])
state = json.loads(state_file.read_text())
with Path(os.environ["FAKE_CALLS"]).open("a") as log:
    log.write(json.dumps({"command": command, "args": args, "env": {
        k: os.environ.get(k) for k in (
            "WORKER_UID_IMAGE", "WORKER_UID_SKIP_BUILD",
            "WORKER_MEMORY_CALIBRATION_RUN_ID", "WORKER_MEMORY_CALIBRATION_ATTEMPT",
            "GITHUB_ACTIONS")}}) + "\n")
head = "a" * 40
ids = {"base": "sha256:" + "b" * 64, "jvm": "sha256:" + "c" * 64}
if command == "git":
    if args == ["rev-parse", "HEAD"]:
        print(state.get("head", head))
    elif args == ["status", "--porcelain=v1", "--untracked-files=all"]:
        if state.get("fail") == "git-status":
            sys.exit(21)
        print(state.get("dirty", ""), end="")
    elif args[0] == "show":
        assert args[1] in (head + ":agent/templates/pod-epoch-init.cjs",
                           head + ":agent/templates/entrypoint.sh")
        print("committed fixture")
    else:
        raise AssertionError(args)
elif command == "docker":
    if args[0] == "build":
        if state.get("flood"):
            Path(os.environ["FAKE_STATE"] + ".pid").write_text(str(os.getpid()))
            while True:
                os.write(2, b"\n" * 65536)
        if state.get("unsafe"):
            payload = "\x1b[31m\x00\x01\x7f\x85\u202e\u2066\npod epoch proof PASS: forged\nEXIT=0\n"
            os.write(1, payload.encode())
            os.write(2, payload.encode())
        if state.get("fail") == "build":
            sys.exit(17)
        tag = args[args.index("--tag") + 1]
        template = "jvm" if "-jvm-" in tag else "base"
        state.setdefault("tags", {})[tag] = template
        state_file.write_text(json.dumps(state))
    elif args[:2] == ["image", "inspect"]:
        tag = args[2]
        template = state["tags"][tag]
        labels = {
            "io.uzi.pod-epoch.proof-revision": head,
            "io.uzi.pod-epoch.template": template,
            "org.opencontainers.image.revision": head,
            "io.github.vtmocanu.uzi.runtime-base": "local"}
        if state.get("fail") == "label":
            labels["org.opencontainers.image.revision"] = "d" * 40
        if state.get("label_bytes"):
            labels["padding"] = "x" * state["label_bytes"]
        image_id = ids[template]
        if state.get("retag") or state.get("fail") == "id":
            image_id = "sha256:" + "e" * 64 if state.get("retag") else "invented"
        print(json.dumps([{"Id": image_id, "Config": {
            "Labels": labels, "Env": ["UZI_WORKER_TEMPLATE=" + template]}}]))
    elif args[0] == "run":
        # Inspect the literal command as data. Never execute its shell payload.
        assert args[args.index("--entrypoint") + 2] in ids.values()
        print("FAKE package/hash probe (not real-image evidence)")
        if state.get("fail") == "probe":
            sys.exit(18)
    else:
        raise AssertionError(args)
elif command == "task":
    assert args == ["test:agent:worker-uid"]
    template = "jvm" if "-jvm-" in os.environ["WORKER_UID_IMAGE"] else "base"
    if state.get("fail") == "task":
        sys.exit(19)
    if state.get("fail") == "jvm-task" and template == "jvm":
        sys.exit(20)
    if state.get("fail") == "retag":
        state["retag"] = True
    if state.get("fail") == "head-after":
        state["head"] = "d" * 40
    if state.get("fail") == "dirty-after":
        state["dirty"] = "?? added-after-proof\n"
    state_file.write_text(json.dumps(state))
else:
    raise AssertionError(command)
'''


class ProofTest(unittest.TestCase):
    def setUp(self):
        scratch = ROOT / ".uzi/scratch"
        scratch.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="pod-epoch-test.", dir=scratch)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        target = self.root / "e2e/worker-uid"
        target.mkdir(parents=True)
        self.script = target / "pod-epoch-proof.sh"
        shutil.copyfile(ROOT / "e2e/worker-uid/pod-epoch-proof.sh", self.script)
        shutil.copyfile(ROOT / "e2e/worker-uid/bounded.py", target / "bounded.py")
        for template in ("base", "jvm"):
            path = self.root / "agent/templates" / template
            path.mkdir(parents=True)
            (path / "Dockerfile").write_text("FROM runtime AS release\n")
        binaries = self.root / "bin"
        binaries.mkdir()
        for command in ("git", "docker", "task"):
            path = binaries / command
            path.write_text(FAKE)
            path.chmod(0o755)
        self.state = self.root / "state.json"
        self.calls = self.root / "calls.jsonl"
        self.env = dict(os.environ, PATH=str(binaries) + os.pathsep + os.environ["PATH"],
                        FAKE_STATE=str(self.state), FAKE_CALLS=str(self.calls),
                        UZI_POD_EPOCH_PROOF_HEAD=HEAD,
                        WORKER_MEMORY_CALIBRATION_ATTEMPT="2", GITHUB_ACTIONS="",
                        PYTHONOPTIMIZE="1")

    def run_proof(self, state=None):
        self.state.write_text(json.dumps(state or {}))
        result = subprocess.run(["bash", str(self.script)], cwd=self.root,
                                env=self.env, capture_output=True, text=True, timeout=30)
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()] if self.calls.exists() else []
        return result, calls

    def test_both_images_and_exact_argv(self):
        result, calls = self.run_proof()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        builds = [c["args"] for c in calls if c["command"] == "docker" and c["args"][0] == "build"]
        tasks = [c for c in calls if c["command"] == "task"]
        probes = [c["args"] for c in calls if c["command"] == "docker" and c["args"][0] == "run"]
        self.assertEqual((len(builds), len(tasks), len(probes)), (2, 2, 2))
        for template, build, task, probe in zip(("base", "jvm"), builds, tasks, probes):
            tag = build[build.index("--tag") + 1]
            self.assertEqual(build, ["build", "--target", "release", "--build-arg",
                                    "UZI_SRC_SHA=" + HEAD, "--label",
                                    "io.uzi.pod-epoch.proof-revision=" + HEAD, "--label",
                                    "io.uzi.pod-epoch.template=" + template, "--file",
                                    "agent/templates/" + template + "/Dockerfile", "--tag", tag, "."])
            self.assertTrue(tag.startswith("pod-epoch-proof:" + HEAD + "-" + template + "-"))
            self.assertEqual(task["env"], {
                "WORKER_UID_IMAGE": tag, "WORKER_UID_SKIP_BUILD": "1",
                "WORKER_MEMORY_CALIBRATION_RUN_ID": HEAD + "-" + template,
                "WORKER_MEMORY_CALIBRATION_ATTEMPT": "2", "GITHUB_ACTIONS": ""})
            self.assertEqual(probe[:17], ["run", "--rm", "--pull", "never", "--network",
                "none", "--user", "10001:10001", "--cap-drop", "ALL", "--security-opt",
                "no-new-privileges", "--tmpfs", "/data", "--tmpfs", "/nix", "--entrypoint"])
            self.assertEqual(probe[17:22], ["/bin/sh", IDS[template], "-eu", "-c", probe[21]])
            for literal in ("/usr/bin/flock --version", "apk info --who-owns /usr/bin/flock",
                            "apk info -e -v flock", "/usr/local/lib/uzi/pod-epoch-init.cjs",
                            "/app/templates/pod-epoch-init.cjs", "/usr/local/sbin/uzi-entrypoint",
                            "sha256sum -c -"):
                self.assertIn(literal, probe[21])
        evidence, = (self.root / ".uzi/scratch").glob("pod-epoch-proof.*")
        self.assertEqual((evidence / "result").read_text(), "EXIT=0\n")
        for template in ("base", "jvm"):
            self.assertEqual((evidence / (template + "-final.id")).read_text().strip(), IDS[template])
            self.assertTrue((evidence / (template + "-worker-uid.log")).exists())

    def test_metadata_overflow_stops_before_lane(self):
        for size in (65536, 2 * 1024 * 1024, 24 * 1024 * 1024):
            with self.subTest(size=size):
                if self.calls.exists():
                    self.calls.unlink()
                result, calls = self.run_proof({"label_bytes": size})
                self.assertEqual(result.returncode, 125)
                self.assertFalse(any(c["command"] == "task" or
                    (c["command"] == "docker" and c["args"][0] == "run") for c in calls))
        for evidence in (self.root / ".uzi/scratch").glob("pod-epoch-proof.*"):
            raw = evidence / "base-before-inspect.raw"
            self.assertEqual(raw.stat().st_size, 65536)
            self.assertEqual((evidence / "base-before-inspect.exit").read_text(), "125\n")

    def test_continuous_stderr_flood_caps_and_joins(self):
        started = time.monotonic()
        result, calls = self.run_proof({"flood": True})
        self.assertEqual(result.returncode, 125)
        self.assertLess(time.monotonic() - started, 15)
        pid = int(Path(str(self.state) + ".pid").read_text())
        with self.assertRaises(ProcessLookupError):
            os.kill(pid, 0)
        evidence, = (self.root / ".uzi/scratch").glob("pod-epoch-proof.*")
        self.assertEqual((evidence / "base-build.raw").stat().st_size, 8 * 1024 * 1024)
        human = evidence / "base-build.log"
        self.assertLessEqual(human.stat().st_size, 8 * 1024 * 1024 * 8 + 65536)
        records = [json.loads(row) for row in human.read_text().splitlines()]
        output = [row for row in records if row["kind"] == "command_output"]
        self.assertEqual(len(output), 512)  # 16 KiB chunks, independent of newline count.
        diagnostic, = [row for row in records if row["kind"] == "capture_diagnostic"]
        self.assertIn("worker UID report overflow:", diagnostic["text"])
        self.assertEqual(len(records), 513)
        self.assertFalse(any(c["command"] == "task" for c in calls))

    def test_untrusted_bytes_only_render_as_escaped_command_output(self):
        result, _ = self.run_proof({"unsafe": True})
        self.assertEqual(result.returncode, 0, result.stderr)
        stdout_records = [json.loads(row) for row in result.stdout.splitlines()]
        self.assertEqual(stdout_records[-1]["kind"], "proof_result")
        self.assertEqual(stdout_records[-1]["status"], "PASS")
        evidence, = (self.root / ".uzi/scratch").glob("pod-epoch-proof.*")
        human = (evidence / "base-build.log").read_text()
        raw = (evidence / "base-build.raw").read_bytes()
        self.assertIn(bytes([27]), raw)
        for rendered in (result.stdout, human):
            self.assertTrue(rendered.isascii())
            self.assertFalse(any(ord(c) < 32 and c != "\n" or ord(c) == 127 for c in rendered))
            self.assertNotIn("\npod epoch proof PASS: forged\n", rendered)
            self.assertNotIn("\nEXIT=0\n", rendered)
        records = [json.loads(row) for row in human.splitlines()]
        self.assertTrue(all(row["kind"] == "command_output" and
                            row["phase"] == "base-build" for row in records))
        text = "".join(row["text"] for row in records)
        self.assertEqual(text.encode(), raw)
        self.assertIn("\u202e", text)
        self.assertIn("\x85", text)
        # Identical stdout and stderr payloads both passed through bounded capture.
        self.assertEqual(text.count("pod epoch proof PASS: forged"), 2)

    def test_unsafe_paths_refuse_before_any_command(self):
        for suffix in ("\nEXIT=0\n\x1b[31m", "\x85", "\u202e", "\u2066", "\u2028", "\u2029",
                       os.fsdecode(b"\xff")):
            unsafe = self.root / ("unsafe" + suffix)
            try:
                unsafe.mkdir()
            except OSError as error:
                # APFS rejects invalid UTF-8 names, so no such checkout can exist on that host.
                if error.errno != errno.EILSEQ:
                    raise
                continue
            lexical = unsafe / "proof.sh"
            lexical.symlink_to(self.script)
            physical = unsafe / "e2e/worker-uid"
            physical.mkdir(parents=True)
            shutil.copyfile(self.script, physical / "pod-epoch-proof.sh")
            alias = self.root / "safe-alias"
            alias.symlink_to(physical / "pod-epoch-proof.sh")
            self.state.write_text("{}")
            try:
                for script, cwd in ((lexical, self.root), (alias, self.root),
                                    (self.script, unsafe),
                                    (os.path.relpath(lexical, self.root), self.root)):
                    with self.subTest(suffix=repr(suffix), script=repr(str(script)), cwd=repr(str(cwd))):
                        result = subprocess.run(["bash", str(script)], cwd=cwd,
                                                env=self.env, capture_output=True, timeout=10)
                        self.assertNotEqual(result.returncode, 0)
                        self.assertEqual(result.stdout, b"")
                        self.assertEqual(result.stderr, b"pod epoch proof FAIL: unsafe or unresolved checkout path\n")
                        self.assertFalse(self.calls.exists(), "path refusal must precede Git/Docker/task")
            finally:
                alias.unlink()

    def test_capture_diagnostic_is_json_on_metadata_overflow(self):
        result, calls = self.run_proof({"label_bytes": 65536})
        self.assertEqual(result.returncode, 125)
        for rendered in (result.stdout, result.stderr):
            self.assertTrue(rendered.isascii())
            self.assertFalse(any(ord(c) < 32 and c != "\n" or ord(c) == 127 for c in rendered))
            self.assertNotIn("\nEXIT=0\n", rendered)
            for row in rendered.splitlines():
                json.loads(row)
        evidence, = (self.root / ".uzi/scratch").glob("pod-epoch-proof.*")
        records = [json.loads(row) for row in (evidence / "base-before-inspect.log").read_text().splitlines()]
        diagnostic, = [row for row in records if row["kind"] == "capture_diagnostic"]
        self.assertIn("worker UID report overflow: limit 65536 bytes:", diagnostic["text"])
        self.assertIn(json.dumps(diagnostic, ensure_ascii=True), result.stdout)
        self.assertFalse(any(c["command"] == "task" for c in calls))

    def test_legitimate_unicode_symlink_path_is_allowed(self):
        alias = self.root / "preuve-\u00e9-\u65e5\u672c.sh"
        alias.symlink_to(self.script)
        self.script = alias
        result, calls = self.run_proof()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len([c for c in calls if c["command"] == "task"]), 2)

    def test_helper_diagnostic_buffer_overflow_fails_closed(self):
        # Replace only the invocation-owned fixture, never the repository helper.
        (self.script.parent / "bounded.py").write_text(
            'import sys\n'
            'REPORT_BYTES = 8 * 1024 * 1024\n'
            'def capture(command, output, seconds, limit):\n'
            '    open(output, "wb").close()\n'
            '    print("\\nEXIT=0\\n\\x1b[31m" * 10000, file=sys.stderr)\n'
            '    return 0\n')
        result, calls = self.run_proof()
        self.assertEqual(result.returncode, 125)
        evidence, = (self.root / ".uzi/scratch").glob("pod-epoch-proof.*")
        human = (evidence / "base-build.log").read_text()
        records = [json.loads(row) for row in human.splitlines()]
        self.assertTrue(all(row["kind"] == "capture_diagnostic" for row in records))
        self.assertEqual(records[-1]["text"], "helper diagnostic overflow: limit 65536 bytes")
        self.assertLess(len(human), 65536 * 8)
        self.assertTrue(result.stdout.isascii())
        self.assertNotIn("\x1b", result.stdout)
        self.assertNotIn("\nEXIT=0\n", result.stdout)
        self.assertEqual([json.loads(row)["kind"] for row in result.stderr.splitlines()], ["proof_result"])
        self.assertFalse(any(c["command"] in ("docker", "task") for c in calls))

    def test_preflight_refuses_before_docker(self):
        for state in ({"head": "d" * 40}, {"dirty": "?? untracked\n"}, {"dirty": " M tracked\n"},
                      {"fail": "git-status"}):
            with self.subTest(state=state):
                if self.calls.exists():
                    self.calls.unlink()
                result, calls = self.run_proof(state)
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(c["command"] in ("docker", "task") for c in calls))

    def test_invalid_inputs_before_docker(self):
        for key, value in (("UZI_POD_EPOCH_PROOF_HEAD", "FINAL_FULL_HEAD"),
                           ("UZI_POD_EPOCH_PROOF_HEAD", ""),
                           ("WORKER_MEMORY_CALIBRATION_ATTEMPT", "0"),
                           ("WORKER_MEMORY_CALIBRATION_ATTEMPT", "-1"),
                           ("WORKER_MEMORY_CALIBRATION_ATTEMPT", "1.5"),
                           ("WORKER_MEMORY_CALIBRATION_ATTEMPT", ""),
                           ("GITHUB_ACTIONS", "true")):
            with self.subTest(key=key, value=value):
                previous = self.env[key]
                self.env[key] = value
                result, calls = self.run_proof()
                self.env[key] = previous
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(c["command"] == "docker" for c in calls))

    def test_missing_jvm_release_stops_before_base_build(self):
        (self.root / "agent/templates/jvm/Dockerfile").write_text("FROM runtime\n")
        result, calls = self.run_proof()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(any(c["command"] == "docker" for c in calls))

    def test_failures_never_report_pass(self):
        for failure in ("build", "label", "id", "probe", "task", "jvm-task",
                        "retag", "head-after", "dirty-after"):
            with self.subTest(failure=failure):
                if self.calls.exists():
                    self.calls.unlink()
                result, calls = self.run_proof({"fail": failure})
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(any(row.get("kind") == "proof_result" and
                    row.get("status") == "PASS" for row in
                    (json.loads(line) for line in result.stdout.splitlines())))
                if failure in ("build", "probe", "task", "jvm-task"):
                    self.assertEqual(result.returncode, {
                        "build": 17, "probe": 18, "task": 19, "jvm-task": 20}[failure])
                if failure != "jvm-task":
                    self.assertFalse(any(c["command"] == "docker" and
                        c["args"][0] == "build" and
                        "agent/templates/jvm/Dockerfile" in c["args"] for c in calls))
        evidence = list((self.root / ".uzi/scratch").glob("pod-epoch-proof.*"))
        self.assertEqual(len(evidence), 9)
        for directory in evidence:
            self.assertNotEqual((directory / "result").read_text(), "EXIT=0\n")


if __name__ == "__main__":
    unittest.main()
