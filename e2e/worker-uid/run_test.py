"""Docker-free integration tests for run.sh using the real JUnit checker."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


HARNESS = Path(__file__).resolve().parent
SCRATCH = HARNESS.parents[1] / ".uzi/scratch"
EXPECTED = [{"file": "agent/test/example.test.ts", "names": ["suite", "leaf"]}]
COMMAND_EXPECTED = [{"file": "e2e/worker-uid/memory-command-real.test.mts", "names": ["command leaf"]}]
COMMAND_PASS = ('<testsuites><testcase name="command leaf" '
                'file="/proof/e2e/worker-uid/memory-command-real.test.mts"/></testsuites>')
PASS = ('<testsuites><testsuite name="suite"><testcase name="leaf" '
        'file="/app/test/example.test.ts"/></testsuite></testsuites>')


class RunnerTest(unittest.TestCase):
    def setUp(self):
        SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="worker-uid-test.", dir=SCRATCH)
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        harness = self.repo / "e2e/worker-uid"
        harness.mkdir(parents=True)
        for name in ("run.sh", "check.py", "check_test.py"):
            shutil.copyfile(HARNESS / name, harness / name)
        self.bin = self.repo / "bin"
        self.bin.mkdir()
        (self.bin / "python3").symlink_to(sys.executable)
        self.calls = self.repo / "calls.jsonl"
        self.stub("node", """
if args[0] == "e2e/worker-uid/inventory.mjs":
    print(os.environ["COMMAND_EXPECTED"] if args[1:] == ["command"] else os.environ["EXPECTED"])
elif args[0] == "e2e/worker-uid/pattern.mjs":
    command = Path(args[1]).name == "command-expected.json"
    assert json.loads(Path(args[1]).read_text()) == json.loads(os.environ["COMMAND_EXPECTED" if command else "EXPECTED"])
    print("^command leaf$" if command else "^suite leaf$")
else:
    raise AssertionError(args)
""")
        self.stub("docker", """
if args[0] == "run":
    name = args[args.index("--name") + 1]
    if name.endswith("-provision"):
        sys.exit(int(os.environ["PROVISION_RC"]))
    command = name.endswith("-command")
    prefix = "COMMAND_" if command else ""
    print("container stderr fixture", file=sys.stderr)
    if os.environ[prefix + "XML_MODE"] == "missing":
        filename = "command-results.xml" if command else "results.xml"
        reports = list(Path(".uzi/scratch").glob("worker-uid.*/" + filename))
        assert len(reports) == 1
        reports[0].unlink()
    else:
        print(os.environ[prefix + "XML"])
    sys.exit(int(os.environ[prefix + "CONTAINER_RC"]))
assert args[0] in ("build", "rm", "image", "volume"), args
if args[0] == "image":
    sys.exit(int(os.environ["METADATA_RC"]))
if args[0] == "build":
    sys.exit(int(os.environ["BUILD_RC"]))
if args[0] == "rm":
    sys.exit(int(os.environ["CLEANUP_RC"]))
""")
        self.stub("timeout", """
assert args[2] == "docker", args
normal = args[:2] == ["--kill-after=30s", "7"]
assert normal or args[:2] in (["--kill-after=5s", "10s"], ["--kill-after=5s", "30s"],
                             ["--kill-after=5s", "60s"], ["--kill-after=5s", "115s"]), args
operation = args[3]
if operation == "run":
    name = args[args.index("--name") + 1]
    expected = ["--kill-after=5s", "30s"] if name.endswith("-provision") else (\n        ["--kill-after=5s", "115s"] if name.endswith("-command") else ["--kill-after=30s", "7"])
else:
    expected = ["--kill-after=5s", "60s" if operation == "build" else "10s"]
assert args[:2] == expected, args
rc = subprocess.run(args[2:], check=False, timeout=5).returncode
key = "TIMEOUT_RC" if normal else "COMMAND_TIMEOUT_RC" if args[1] == "115s" else ""
if key and os.environ[key]:
    print("watchdog stderr fixture", file=sys.stderr)
    rc = int(os.environ[key])
sys.exit(rc)
""")

    def stub(self, name, body):
        script = self.bin / name
        script.write_text(
            f"#!{sys.executable}\n"
            "import json, os, subprocess, sys\nfrom pathlib import Path\n"
            "args = sys.argv[1:]\n"
            "with open(os.environ['CALLS'], 'a', encoding='utf-8') as log:\n"
            f"    log.write(json.dumps([{name!r}, *args]) + '\\n')\n"
            + body
        )
        script.chmod(0o755)

    def run_lane(self, xml=PASS, container_rc=0, timeout_rc="", xml_mode="", skip_build="1",
                 command_xml=COMMAND_PASS, command_rc=0, command_timeout="", command_mode="",
                 metadata_rc=0, provision_rc=0, build_rc=0, cleanup_rc=0):
        env = {
            "PATH": str(self.bin) + os.pathsep + os.defpath,
            "CALLS": str(self.calls), "EXPECTED": json.dumps(EXPECTED),
            "COMMAND_EXPECTED": json.dumps(COMMAND_EXPECTED),
            "COMMAND_XML": command_xml, "COMMAND_XML_MODE": command_mode,
            "COMMAND_CONTAINER_RC": str(command_rc), "COMMAND_TIMEOUT_RC": str(command_timeout),
            "METADATA_RC": str(metadata_rc), "PROVISION_RC": str(provision_rc), "BUILD_RC": str(build_rc),
            "CLEANUP_RC": str(cleanup_rc),
            "XML": xml, "XML_MODE": xml_mode, "CONTAINER_RC": str(container_rc),
            "TIMEOUT_RC": str(timeout_rc), "WORKER_UID_TIMEOUT": "7",
            "WORKER_UID_SKIP_BUILD": skip_build, "WORKER_UID_IMAGE": "fixture-image",
            "PYTHONDONTWRITEBYTECODE": "1",
        }
        result = subprocess.run(["bash", str(self.repo / "e2e/worker-uid/run.sh")],
                                env=env, capture_output=True, text=True, timeout=10, check=False)
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
        runs = [call for call in calls if call[:2] == ["docker", "run"]]
        # Every Docker operation, including failures before either lane, is bounded.
        for index, call in enumerate(calls):
            if call[0] == "docker":
                self.assertEqual(calls[index - 1][0], "timeout", call)
                self.assertEqual(calls[index - 1][3:], call)
        if metadata_rc or provision_rc or build_rc:
            self.assertNotEqual(result.returncode, 0, result.stderr)
            self.assertTrue(any(call[:2] == ["docker", "rm"] for call in calls))
            return result, calls
        self.assertEqual(len(runs), 3, result.stderr)
        provision, run, command = runs
        name = run[run.index("--name") + 1]
        self.assertTrue(name.startswith("wuid-2134-"), name)
        self.assertEqual([call for call in calls if call[:2] == ["docker", "rm"]],
                         [["docker", "rm", "-f", name, name + "-command", name + "-provision"]])
        self.assertEqual([call for call in calls if call[:3] == ["docker", "volume", "rm"]],
                         [["docker", "volume", "rm", name + "-deps"]])
        self.assertEqual(command[command.index("--name") + 1], name + "-command")
        self.assertEqual(command[command.index("--user") + 1], "10003:10003")
        self.assertEqual(command[command.index("--cap-drop") + 1], "ALL")
        self.assertNotIn("--cap-add", command)
        self.assertEqual(command[command.index("--security-opt") + 1], "no-new-privileges")
        self.assertEqual(command[command.index("--network") + 1], "none")
        for invocation in (provision, run, command):
            self.assertNotIn("--rm", invocation)  # Retain exact owned names until the cleanup trap.
            self.assertEqual(invocation[invocation.index("--pull") + 1], "never")
        self.assertEqual(command[command.index("--entrypoint") + 1], "/usr/local/bin/node")
        self.assertIn("--test-timeout=110000", command)
        self.assertEqual(command[-1], "/proof/e2e/worker-uid/memory-command-real.test.mts")
        self.assertEqual(command[command.index("--test-name-pattern") + 1], "^command leaf$")
        mounts = [command[index + 1] for index, arg in enumerate(command) if arg == "--mount"]
        for source, target in (("e2e/worker-uid", "/proof/e2e/worker-uid"),
                               ("agent/src", "/proof/agent/src"),
                               ("agent/tsconfig.json", "/proof/agent/tsconfig.json"),
                               ("agent/package.json", "/proof/agent/package.json"),
                               ("agent/codex/supervisor", "/proof/agent/codex/supervisor")):
            self.assertIn(f"type=bind,src={self.repo}/{source},dst={target},readonly", mounts)
        self.assertIn(f"type=volume,src={name}-deps,dst=/proof/agent/node_modules,readonly", mounts)
        self.assertIn(f"type=volume,src={name}-deps,dst=/app/node_modules", provision)
        self.assertIn("GOPROXY=off", command)
        self.assertIn("GOTOOLCHAIN=local", command)
        self.assertIn("/proof/.uzi/scratch:exec,uid=10003,gid=10003,mode=0700", command)
        self.assertIn("--test-reporter=junit", command)
        self.assertIn("container stderr fixture", result.stderr)
        report = next(line.removeprefix("worker UID reports: ") for line in result.stdout.splitlines()
                      if line.startswith("worker UID reports: "))
        self.assertEqual(Path(report).parent, self.repo / ".uzi/scratch")
        self.assertTrue(Path(report, "expected.json").is_file())
        self.assertTrue(Path(report, "command-expected.json").is_file())
        self.assertEqual(run[run.index("--test-name-pattern") + 1], "^suite leaf$")
        self.assertIn("--test-timeout=120000", run)
        self.assertIn("--test-reporter=junit", run)
        self.assertEqual([arg for arg in run if arg.startswith("/app/test/") and arg.endswith(".test.ts")], [
            "/app/test/codex-launcher.test.ts", "/app/test/codex-executor.test.ts",
            "/app/test/codex-shared-dir.test.ts", "/app/test/entrypoint-migration.test.ts",
            "/app/test/git-planning-diff.test.ts",
        ])
        self.assertEqual(run[run.index("--entrypoint") + 1], "/usr/local/sbin/uzi-entrypoint")
        self.assertEqual(run[run.index("--network") + 1], "none")
        self.assertEqual(run[run.index("--cap-drop") + 1], "ALL")
        self.assertEqual([run[index + 1] for index, arg in enumerate(run) if arg == "--cap-add"],
                         ["CHOWN", "DAC_OVERRIDE", "SETPCAP", "SETUID", "SETGID"])
        self.assertEqual(run[run.index("--security-opt") + 1], "no-new-privileges")
        self.assertEqual([run[index + 1] for index, arg in enumerate(run) if arg == "--tmpfs"],
                         ["/data", "/nix"])
        self.assertEqual(run[run.index("--entrypoint") + 2:run.index("--test")], [
            "fixture-image", "/usr/local/bin/node", "--import", "tsx",
            "--import", "/app/test/setup/hermetic-proc.ts",
        ])
        return result, calls

    def test_success_and_build(self):
        result, calls = self.run_lane(skip_build="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("1 required leaves passed", result.stdout)
        self.assertEqual([call for call in calls if call[:2] == ["docker", "build"]],
                         [["docker", "build", "-f", "agent/templates/base/Dockerfile",
                           "-t", "fixture-image", "."]])

    def test_command_outcomes(self):
        for mode in ("failure", "missing", "skip", "timeout", "status", "malformed", "empty"):
            with self.subTest(mode=mode):
                # Keep one runner invocation's call log per case.
                self.calls.unlink(missing_ok=True)
                kwargs = {}
                if mode in ("failure", "skip"):
                    tag = "failure" if mode == "failure" else "skipped"
                    kwargs["command_xml"] = COMMAND_PASS.replace("/>", f'><{tag} message="command fixture"/></testcase>')
                elif mode == "missing":
                    kwargs["command_mode"] = "missing"
                elif mode == "timeout":
                    kwargs.update(command_xml="", command_timeout=124)
                elif mode == "status":
                    kwargs["command_rc"] = 23
                elif mode == "malformed":
                    kwargs["command_xml"] = "<testsuites>"
                else:
                    kwargs["command_xml"] = "<testsuites/>"
                result, _ = self.run_lane(**kwargs)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertIn("1 required leaves passed", result.stdout)  # Worker proof still runs.
                if mode in ("status", "timeout"):
                    code = 23 if mode == "status" else 124
                    self.assertIn(f"worker UID command node/container exit: {code}", result.stderr)
                else:
                    self.assertIn("worker UID lane FAIL", result.stderr)

    def test_command_status_and_checker_failure(self):
        result, _ = self.run_lane(command_xml="<testsuites/>", command_rc=77)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("expected exactly one result, found 0", result.stderr)
        self.assertIn("worker UID command node/container exit: 77", result.stderr)

    def test_cleanup_failure_still_attempts_volume(self):
        result, _ = self.run_lane(cleanup_rc=124)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("worker UID cleanup exit: 124", result.stderr)

    def test_worker_skip(self):
        result, _ = self.run_lane(xml=PASS.replace("/>", '><skipped/></testcase>'))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("skipped from testcase", result.stderr)

    def test_setup_failures(self):
        for key in ("metadata_rc", "provision_rc", "build_rc"):
            with self.subTest(key=key):
                self.calls.unlink(missing_ok=True)
                result, _ = self.run_lane(skip_build="", **{key: 124})
                self.assertNotEqual(result.returncode, 0)

    def test_checker_failure(self):
        xml = PASS.replace("/>", '><failure type="testCodeFailure" message="assertion fixture">'
                           'stack frame.ts:42</failure></testcase>')
        result, calls = self.run_lane(xml=xml)
        self.assertNotEqual(result.returncode, 0)
        for detail in ("worker UID lane FAIL", "assertion fixture", "stack frame.ts:42"):
            self.assertIn(detail, result.stderr)
        self.assertNotIn("node/container exit", result.stderr)
        self.assertFalse(any(call[:2] == ["docker", "build"] for call in calls))

    def test_container_and_checker_failure(self):
        result, _ = self.run_lane(xml="<testsuites/>", container_rc=77)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("expected exactly one result, found 0", result.stderr)
        self.assertIn("worker UID node/container exit: 77", result.stderr)

    def test_container_failure_with_passing_checker(self):
        result, _ = self.run_lane(container_rc=23)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("1 required leaves passed", result.stdout)
        self.assertIn("worker UID node/container exit: 23", result.stderr)

    def test_malformed_xml(self):
        result, _ = self.run_lane(xml="<testsuites>")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("worker UID lane FAIL: no element found", result.stderr)

    def test_missing_xml(self):
        result, _ = self.run_lane(xml_mode="missing", container_rc=1)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("No such file or directory", result.stderr)
        self.assertIn("worker UID node/container exit: 1", result.stderr)

    def test_timeout_and_stderr(self):
        result, _ = self.run_lane(xml="", timeout_rc=124)
        self.assertNotEqual(result.returncode, 0)
        for detail in ("worker UID lane FAIL", "worker UID node/container exit: 124",
                       "watchdog stderr fixture"):
            self.assertIn(detail, result.stderr)


if __name__ == "__main__":
    unittest.main()
