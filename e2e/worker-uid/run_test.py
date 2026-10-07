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
if args == ["e2e/worker-uid/inventory.mjs"]:
    print(os.environ["EXPECTED"])
elif args[0] == "e2e/worker-uid/pattern.mjs":
    assert json.loads(Path(args[1]).read_text()) == json.loads(os.environ["EXPECTED"])
    print("^suite leaf$")
else:
    raise AssertionError(args)
""")
        self.stub("docker", """
if args[0] == "run":
    print("container stderr fixture", file=sys.stderr)
    if os.environ["XML_MODE"] == "missing":
        reports = list(Path(".uzi/scratch").glob("worker-uid.*/results.xml"))
        assert len(reports) == 1
        reports[0].unlink()
    else:
        print(os.environ["XML"])
    sys.exit(int(os.environ["CONTAINER_RC"]))
assert args[0] in ("build", "rm"), args
""")
        self.stub("timeout", """
assert args[:2] == ["--kill-after=30s", "7"], args
assert args[2:4] == ["docker", "run"], args
rc = subprocess.run(args[2:], check=False, timeout=5).returncode
if os.environ["TIMEOUT_RC"]:
    print("watchdog stderr fixture", file=sys.stderr)
    rc = int(os.environ["TIMEOUT_RC"])
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

    def run_lane(self, xml=PASS, container_rc=0, timeout_rc="", xml_mode="", skip_build="1"):
        env = {
            "PATH": str(self.bin) + os.pathsep + os.defpath,
            "CALLS": str(self.calls), "EXPECTED": json.dumps(EXPECTED),
            "XML": xml, "XML_MODE": xml_mode, "CONTAINER_RC": str(container_rc),
            "TIMEOUT_RC": str(timeout_rc), "WORKER_UID_TIMEOUT": "7",
            "WORKER_UID_SKIP_BUILD": skip_build, "WORKER_UID_IMAGE": "fixture-image",
            "PYTHONDONTWRITEBYTECODE": "1",
        }
        result = subprocess.run(["bash", str(self.repo / "e2e/worker-uid/run.sh")],
                                env=env, capture_output=True, text=True, timeout=10, check=False)
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
        runs = [call for call in calls if call[:2] == ["docker", "run"]]
        self.assertEqual(len(runs), 1, result.stderr)
        run = runs[0]
        name = run[run.index("--name") + 1]
        self.assertTrue(name.startswith("wuid-2134-"), name)
        self.assertEqual([call for call in calls if call[:2] == ["docker", "rm"]],
                         [["docker", "rm", "-f", name]])
        self.assertIn("container stderr fixture", result.stderr)
        report = next(line.removeprefix("worker UID reports: ") for line in result.stdout.splitlines()
                      if line.startswith("worker UID reports: "))
        self.assertEqual(Path(report).parent, self.repo / ".uzi/scratch")
        self.assertTrue(Path(report, "expected.json").is_file())
        self.assertEqual(run[run.index("--test-name-pattern") + 1], "^suite leaf$")
        self.assertIn("--test-timeout=120000", run)
        self.assertIn("--test-reporter=junit", run)
        return result, calls

    def test_success_and_build(self):
        result, calls = self.run_lane(skip_build="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("1 required leaves passed", result.stdout)
        self.assertEqual([call for call in calls if call[:2] == ["docker", "build"]],
                         [["docker", "build", "-f", "agent/templates/base/Dockerfile",
                           "-t", "fixture-image", "."]])

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
