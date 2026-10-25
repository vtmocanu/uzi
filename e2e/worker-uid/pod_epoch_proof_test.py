#!/usr/bin/env python3
"""Docker-free orchestration tests; fake commands never execute probe payloads."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
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
                            "apk info -v flock", "/usr/local/lib/uzi/pod-epoch-init.cjs",
                            "/app/templates/pod-epoch-init.cjs", "/usr/local/sbin/uzi-entrypoint",
                            "sha256sum -c -"):
                self.assertIn(literal, probe[21])
        evidence, = (self.root / ".uzi/scratch").glob("pod-epoch-proof.*")
        self.assertEqual((evidence / "result").read_text(), "EXIT=0\n")
        for template in ("base", "jvm"):
            self.assertEqual((evidence / (template + "-final.id")).read_text().strip(), IDS[template])
            self.assertTrue((evidence / (template + "-worker-uid.log")).exists())

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
                self.assertNotIn("pod epoch proof PASS", result.stdout)
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
