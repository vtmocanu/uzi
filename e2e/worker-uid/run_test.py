"""Docker-free integration tests for run.sh using the real JUnit checker."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import bounded
import tool_image


HARNESS = Path(__file__).resolve().parent
SCRATCH = HARNESS.parents[1] / ".uzi/scratch"
EXPECTED = [{"file": "agent/test/example.test.ts", "names": ["suite", "leaf"]}]
COMMAND_EXPECTED = [{"file": "e2e/worker-uid/memory-command-real.test.mts", "names": ["command leaf"]}]
COMMAND_PASS = ('<testsuites><testcase name="command leaf" '
                'file="/app/uzi-worker-uid-proof/e2e/worker-uid/memory-command-real.test.mts"/></testsuites>')
PASS = ('<testsuites><testsuite name="suite"><testcase name="leaf" '
        'file="/app/uzi-worker-uid-proof/agent/test/example.test.ts"/></testsuite></testsuites>')


class ToolImageTest(unittest.TestCase):
    def prepare(self, mode=""):
        token = "1" * 32
        parent_id, child_id = "sha256:" + "a" * 64, "sha256:" + "b" * 64
        layer = "sha256:" + "c" * 64
        parent = {"Id": parent_id, "Os": "linux", "Architecture": "amd64",
                  "RootFS": {"Layers": [layer]}, "RepoTags": ["local-worker:proof"]}
        child = {**parent, "Id": child_id, "RootFS": {"Layers": [layer, "sha256:" + "d" * 64]},
                 "Config": {"Labels": {bounded.LABEL: token, tool_image.PARENT_LABEL: parent_id}}}
        tools = {"schemaVersion": 1, "invocation": token, "parentImageId": parent_id,
                 "goRoot": "/nix/store/" + "g" * 32 + "-go/share/go",
                 "taskBin": "/nix/store/" + "h" * 32 + "-task/bin"}
        calls = []

        def docker(args, *_):
            calls.append(args)
            if args[:2] == ["image", "ls"]: return b"existing" if mode == "collision" else b""
            if args[:2] == ["volume", "ls"]: return b"existing" if mode == "volume-collision" else b""
            if args[:2] == ["volume", "create"]:
                if mode == "volume-lost-ack": raise ValueError("lost volume ACK")
                if mode == "task-lost-ack" and args[-1].endswith("-task"): raise ValueError("lost second volume ACK")
                return b"foreign" if mode == "volume-ack" else args[-1].encode()
            if args[:2] == ["volume", "inspect"]:
                return json.dumps([{"Name": "foreign" if mode == "volume-name" else args[-1], "Labels": {
                    bounded.LABEL: "foreign" if mode == "volume-label" else token}}]).encode()
            if args == ["context", "show"]: return b"default\n"
            if args[0] == "run":
                if mode == "lost-probe-ack": raise ValueError("lost probe ACK")
                return json.dumps(tools).encode()
            self.assertEqual(args[:2], ["image", "inspect"])
            item = child if args[-1].endswith("-tools") else parent
            if mode == "untagged": item = {**item, "RepoTags": []}
            if mode == "tag-moved" and args[-1] == "local-worker:proof": item = {**item, "Id": child_id}
            if item is child:
                if mode == "foreign-label": item = {**item, "Config": {"Labels": {}}}
                if mode == "foreign-layer": item = {**item, "RootFS": {"Layers": ["sha256:" + "e" * 64]}}
                if mode == "wrong-platform": item = {**item, "Architecture": "arm64"}
            return json.dumps([item]).encode()

        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            with mock.patch.object(bounded, "docker", side_effect=docker), \
                 mock.patch.object(bounded, "capture", return_value=124 if mode == "build-timeout" else 0) as build:
                if mode:
                    with self.assertRaises(ValueError): tool_image.prepare(directory, token, parent_id)
                else:
                    tool_image.prepare(directory, token, parent_id)
                    self.assertEqual(json.loads((directory / "command-image.json").read_text()),
                                     {"parentImageId": parent_id, "imageId": child_id,
                                      "goRoot": tools["goRoot"], "taskBin": tools["taskBin"],
                                      "goVolume": "wuid-2134-" + token + "-go",
                                      "taskVolume": "wuid-2134-" + token + "-task"})
                    command = build.call_args.args[0]
                    self.assertIn("BASE_IMAGE=local-worker:proof", command)
                    self.assertIn("--network", command)
                    self.assertIn("none", command)
                    self.assertIn("--pull=false", command)
                    self.assertLess(build.call_args.args[2], 25)
                journal = (directory / "attempted").read_text() if (directory / "attempted").exists() else ""
                if mode in ("untagged", "tag-moved", "collision"):
                    build.assert_not_called()
                    self.assertEqual(journal, "")
                if mode == "lost-probe-ack":
                    self.assertIn("image wuid-2134-" + token + "-tools " + child_id, journal)
                    self.assertIn("container wuid-2134-" + token + "-provision", journal)
                if mode in ("volume-ack", "volume-lost-ack", "volume-label", "volume-name"):
                    self.assertIn("volume wuid-2134-" + token + "-go\n", journal)
                    self.assertNotIn("volume wuid-2134-" + token + "-task", journal)
                if mode == "volume-collision":
                    self.assertNotIn("volume ", journal)
                    self.assertFalse(any(call[:2] == ["volume", "create"] for call in calls))
                if mode == "task-lost-ack":
                    for tool in ("go", "task"):
                        self.assertIn("volume wuid-2134-" + token + "-" + tool + "\n", journal)
            self.assertFalse(any(call[0] in ("tag", "rm") for call in calls))

    def test_preparation_identity_and_failure_journal(self):
        self.prepare()
        for mode in ("untagged", "tag-moved", "collision", "foreign-label", "foreign-layer",
                     "wrong-platform", "build-timeout", "lost-probe-ack", "volume-collision",
                     "volume-ack", "volume-lost-ack", "volume-label", "volume-name", "task-lost-ack"):
            with self.subTest(mode=mode): self.prepare(mode)

    def test_canonical_bound_tool_paths(self):
        item = {"schemaVersion": 1, "invocation": "1" * 32, "parentImageId": "sha256:" + "a" * 64,
                "goRoot": "/nix/store/" + "g" * 32 + "-go/share/go",
                "taskBin": "/nix/store/" + "h" * 32 + "-task/bin"}
        for change in ({"invocation": "2" * 32}, {"parentImageId": "sha256:" + "b" * 64},
                       {"schemaVersion": True}, {"goRoot": "/nix/store/../etc"},
                       {"taskBin": item["goRoot"]}, {"taskBin": item["goRoot"] + "/bin"},
                       {"goRoot": item["goRoot"] + "/../bin"}):
            with self.subTest(change=change), self.assertRaises(ValueError):
                tool_image.tool_paths(json.dumps({**item, **change}), "1" * 32, item["parentImageId"])

    def test_preparation_deadline_prevents_docker(self):
        with tempfile.TemporaryDirectory() as directory, \
             mock.patch.object(tool_image.time, "monotonic", side_effect=[0, 14]), \
             mock.patch.object(bounded, "docker") as docker, self.assertRaises(ValueError):
            tool_image.prepare(Path(directory), "1" * 32, "worker:proof")
        docker.assert_not_called()

    def test_image_cleanup_preserves_parent_and_refuses_foreign(self):
        token, image_id = "1" * 32, "sha256:" + "b" * 64
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            (directory / "attempted").write_text("image invocation-tools " + image_id + "\n")
            for foreign in (False, True):
                item = {"Id": image_id, "Config": {"Labels": {bounded.LABEL: "foreign" if foreign else token}}}
                with mock.patch.object(bounded, "docker", side_effect=[json.dumps([item]).encode(), b""]) as docker:
                    if foreign:
                        with self.assertRaises(ValueError): bounded.cleanup(directory, token)
                        self.assertEqual(docker.call_count, 1)
                    else:
                        bounded.cleanup(directory, token)
                        self.assertEqual(docker.call_args.args[0], ["image", "rm", "--no-prune", image_id])


class CaptureTest(unittest.TestCase):
    def test_complete_output_and_inherited_pipe_deadline(self):
        from bounded import capture
        import time
        SCRATCH.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=SCRATCH) as directory:
            report = Path(directory) / "report"
            rc = capture([sys.executable, "-c", "import sys; sys.stdout.buffer.write(b'x' * 1024)"],
                         report, 10, 1024)
            self.assertEqual(rc, 0)
            self.assertEqual(report.read_bytes(), b"x" * 1024)
            # The direct producer exits, but its child holds stdout and ignores TERM.
            # Completion must use the deadline rather than wait indefinitely for EOF.
            child = ("import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); "
                     "print('ready', flush=True); time.sleep(60)")
            parent = ("import subprocess,sys; "
                      f"subprocess.Popen([sys.executable, '-c', {child!r}])")
            start = time.monotonic()
            rc = capture([sys.executable, "-c", parent], report, 7, 1024)
            self.assertEqual(rc, 124)
            self.assertLess(time.monotonic() - start, 8)
            self.assertIn(b"ready", report.read_bytes())


class RunnerTest(unittest.TestCase):
    def setUp(self):
        SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="worker-uid-test.", dir=SCRATCH)
        self.addCleanup(self.temp.cleanup)
        self.repo = Path(self.temp.name)
        harness = self.repo / "e2e/worker-uid"
        harness.mkdir(parents=True)
        for name in ("run.sh", "bounded.py", "check.py", "check_test.py", "tool_image.py", "tool-image.Dockerfile"):
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
state_path = Path("resources.json")
state = json.loads(state_path.read_text()) if state_path.exists() else {}
if args[:2] == ["volume", "ls"]:
    sys.exit(0)
if args[:2] == ["volume", "rm"]:
    sys.exit(int(os.environ["CLEANUP_RC"]))
if args[0] == "run" or args[:2] == ["volume", "create"]:
    volume = args[0] == "volume"
    if not volume:
        for i, arg in enumerate(args):
            if arg == "--mount":
                parts = args[i + 1].split(",")
                fields = dict(p.split("=", 1) for p in parts if "=" in p)
                readonly = "readonly" in parts or fields.get("readonly") == "true"
                if fields.get("type") == "volume" and readonly and not (fields.get("src") or fields.get("source")):
                    print('invalid mount config for type "volume": must not set ReadOnly mode when using anonymous volumes', file=sys.stderr)
                    sys.exit(125)
    name = args[-1] if volume else args[args.index("--name") + 1]
    token = args[args.index("--label") + 1].split("=", 1)[1]
    state[name] = {"Name": name, "Id": __import__("hashlib").sha256(name.encode()).hexdigest(),
                   "Kind": "volume" if volume else "container",
                   "Labels": {"io.uzi.worker-uid.invocation": token},
                   "Config": {"Labels": {"io.uzi.worker-uid.invocation": token}}}
    if not volume:
        masked = {args[i + 1].split(":")[0] for i, arg in enumerate(args) if arg == "--tmpfs"}
        state[name]["AnonymousVolumes"] = [p for p in ("/data", "/nix") if p not in masked]
    if os.environ["OWNERSHIP_MISMATCH"] and name.endswith(os.environ["OWNERSHIP_MISMATCH"]):
        state[name]["Labels"] = state[name]["Config"]["Labels"] = {}
    state_path.write_text(json.dumps(state))
    if volume:
        print(name)
        sys.exit(int(os.environ["VOLUME_RC"]))
    if name.endswith("-provision"):
        image = args[args.index("--entrypoint") + 2]
        derived = next(v for v in state.values() if v.get("Id") == image)
        print(json.dumps({"schemaVersion": 1, "goRoot": "/nix/store/" + "g" * 32 + "-go/share/go",
                          "taskBin": "/nix/store/" + "h" * 32 + "-task/bin", "invocation": token,
                          "parentImageId": derived["Config"]["Labels"]["io.uzi.worker-uid.parent-image"]}))
        sys.exit(int(os.environ["PROVISION_RC"]))
    command = name.endswith("-command")
    prefix = "COMMAND_" if command else ""
    print("container stderr fixture", file=sys.stderr)
    if os.environ[prefix + "XML_MODE"] == "missing":
        filename = "command-results.xml" if command else "results.xml"
        reports = list(Path(".uzi/scratch").glob("worker-uid.*/" + filename))
        max(reports, key=lambda path: path.stat().st_mtime_ns).unlink()
    else:
        print(os.environ[prefix + "XML"], flush=True)
    if os.environ[prefix + "XML_MODE"] == "overflow":
        import signal, time
        signal.signal(signal.SIGTERM, lambda *_: (Path(name + ".terminated").touch(), sys.exit(143)))
        # Stream indefinitely: the collector must stop this producer before its watchdog.
        while True:
            sys.stdout.write(" " * 65536)
            sys.stdout.flush()
    sys.exit(int(os.environ[prefix + "CONTAINER_RC"]))
if args[0] == "inspect" or args[:2] == ["volume", "inspect"]:
    print(json.dumps([state[args[-1]]]))
    sys.exit(0)
assert args[0] in ("build", "rm", "image", "volume", "context"), args
if args[0] == "image":
    if args[1] == "inspect":
        if int(os.environ["METADATA_RC"]): sys.exit(int(os.environ["METADATA_RC"]))
        parent = {"Id": "sha256:" + "a" * 64, "Os": "linux", "Architecture": "amd64",
                  "RootFS": {"Layers": ["sha256:" + "c" * 64]}, "RepoTags": ["fixture-image"]}
        print(json.dumps([state.get(args[-1], parent)]))
    elif args[1] == "rm":
        assert "--no-prune" in args
        assert args[-1] == "sha256:" + "b" * 64
    elif args[1] != "ls": raise AssertionError(args)
    sys.exit(0)
if args[:2] == ["context", "show"]:
    print("default")
    sys.exit(0)
if args[0] == "build":
    if "--builder" in args:
        name = args[args.index("--tag") + 1]
        labels = dict(args[i + 1].split("=", 1) for i, arg in enumerate(args) if arg == "--label")
        state[name] = {"Id": "sha256:" + "b" * 64, "Os": "linux", "Architecture": "amd64",
                       "RootFS": {"Layers": ["sha256:" + "c" * 64, "sha256:" + "d" * 64]},
                       "Config": {"Labels": labels}}
        state_path.write_text(json.dumps(state))
        sys.exit(0)
    sys.exit(int(os.environ["BUILD_RC"]))
if args[0] == "rm":
    if "--volumes" in args:
        item = next(v for v in state.values() if v.get("Id") == args[-1])
        item["AnonymousVolumes"] = []
        state_path.write_text(json.dumps(state))
    sys.exit(int(os.environ["CLEANUP_RC"]))
""")
        self.stub("timeout", """
assert args[0] == "--kill-after=5s", args
seconds = float(args[1].removesuffix("s"))
if args[2] == "python3":
    assert seconds == 20 and args[3].endswith("/tool_image.py"), args
    sys.exit(subprocess.run(args[2:], check=False).returncode)
assert args[2] == "docker", args
operation = args[3]
if operation == "run":
    name = args[args.index("--name") + 1]
    if name.endswith("-provision"): assert 0 < seconds <= 5, args
    else: assert seconds == (115 if name.endswith("-command") else 7), args
elif operation == "build":
    assert (0 < seconds < 20) if "--builder" in args else seconds == 60, args
else:
    assert 0 < seconds <= 5, args
import signal
proc = subprocess.Popen(args[2:])
signal.signal(signal.SIGTERM, lambda *_: proc.terminate())
rc = proc.wait(timeout=5)
key = "TIMEOUT_RC" if operation == "run" and seconds == 7 else (
    "COMMAND_TIMEOUT_RC" if operation == "run" and seconds == 115 else "")
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
                 metadata_rc=0, provision_rc=0, build_rc=0, cleanup_rc=0,
                 volume_rc=0, ownership_mismatch=""):
        (self.repo / "resources.json").unlink(missing_ok=True)
        env = {
            "PATH": str(self.bin) + os.pathsep + os.defpath,
            "CALLS": str(self.calls), "EXPECTED": json.dumps(EXPECTED),
            "COMMAND_EXPECTED": json.dumps(COMMAND_EXPECTED),
            "COMMAND_XML": command_xml, "COMMAND_XML_MODE": command_mode,
            "COMMAND_CONTAINER_RC": str(command_rc), "COMMAND_TIMEOUT_RC": str(command_timeout),
            "METADATA_RC": str(metadata_rc), "PROVISION_RC": str(provision_rc), "BUILD_RC": str(build_rc),
            "CLEANUP_RC": str(cleanup_rc), "VOLUME_RC": str(volume_rc),
            "OWNERSHIP_MISMATCH": ownership_mismatch,
            "XML": xml, "XML_MODE": xml_mode, "CONTAINER_RC": str(container_rc),
            "TIMEOUT_RC": str(timeout_rc), "WORKER_UID_TIMEOUT": "7",
            "WORKER_UID_SKIP_BUILD": skip_build, "WORKER_UID_IMAGE": "fixture-image",
            "PYTHONDONTWRITEBYTECODE": "1",
        }
        result = subprocess.run(["bash", str(self.repo / "e2e/worker-uid/run.sh")],
                                env=env, capture_output=True, text=True, timeout=20, check=False)
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
        runs = [call for call in calls if call[:2] == ["docker", "run"]]
        # Every Docker operation, including failures before either lane, is bounded.
        for index, call in enumerate(calls):
            if call[0] == "docker":
                self.assertEqual(calls[index - 1][0], "timeout", call)
                self.assertEqual(calls[index - 1][3:], call)
        if metadata_rc or provision_rc or build_rc or volume_rc or ownership_mismatch:
            self.assertNotEqual(result.returncode, 0, result.stderr)
            removals = [call for call in calls if call[:2] == ["docker", "rm"] or call[:3] == ["docker", "volume", "rm"]]
            if metadata_rc or build_rc:
                self.assertEqual(removals, [])
            elif ownership_mismatch == "-deps":
                self.assertEqual(removals, [])
            else:
                self.assertTrue(removals)
            return result, calls
        self.assertNotIn("must not set ReadOnly mode when using anonymous volumes", result.stderr)
        self.assertEqual(len(runs), 3, result.stderr)
        provision, run, command = runs
        name = run[run.index("--name") + 1]
        self.assertTrue(name.startswith("wuid-2134-"), name)
        state = json.loads((self.repo / "resources.json").read_text())
        self.assertEqual([call for call in calls if call[:2] == ["docker", "rm"]],
                         [["docker", "rm", "-f", "--volumes", state[n]["Id"]]
                          for n in (name + "-command", name, name + "-provision")])
        self.assertRegex(name, r"^wuid-2134-[0-9a-f]{32}$")
        for n in (name, name + "-command", name + "-provision"):
            self.assertEqual(state[n]["Labels"]["io.uzi.worker-uid.invocation"], name.removeprefix("wuid-2134-"))
        self.assertEqual([call for call in calls if call[:3] == ["docker", "volume", "rm"]],
                         [["docker", "volume", "rm", name + suffix] for suffix in ("-task", "-go")])
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
        self.assertEqual(command[-1], "/app/uzi-worker-uid-proof/e2e/worker-uid/memory-command-real.test.mts")
        self.assertEqual(command[command.index("--test-name-pattern") + 1], "^command leaf$")
        mounts = [command[index + 1] for index, arg in enumerate(command) if arg == "--mount"]
        self.assertEqual([mount for mount in mounts if mount.startswith("type=volume,")], [
            "type=volume,src=" + name + "-go,dst=/nix/store/" + "g" * 32 + "-go/share/go,readonly",
            "type=volume,src=" + name + "-task,dst=/nix/store/" + "h" * 32 + "-task/bin,readonly",
        ])
        self.assertEqual(command[command.index("--entrypoint") + 2], "sha256:" + "b" * 64)
        for source, target in (("Taskfile.yml", "/app/uzi-worker-uid-proof/Taskfile.yml"),
                               ("homebrew-tap.yml", "/app/uzi-worker-uid-proof/homebrew-tap.yml"),
                               ("e2e/worker-uid", "/app/uzi-worker-uid-proof/e2e/worker-uid"),
                               ("agent/src", "/app/uzi-worker-uid-proof/agent/src"),
                               ("agent/tsconfig.json", "/app/uzi-worker-uid-proof/agent/tsconfig.json"),
                               ("agent/package.json", "/app/uzi-worker-uid-proof/agent/package.json"),
                               ("agent/codex/supervisor", "/app/uzi-worker-uid-proof/agent/codex/supervisor")):
            self.assertIn(f"type=bind,src={self.repo}/{source},dst={target},readonly", mounts)
        self.assertEqual(len([c for c in calls if c[:3] == ["docker", "volume", "create"]]), 2)
        self.assertIn('lstatSync("/app/uzi-worker-uid-proof")', provision[-1])
        self.assertIn('e.code!=="ENOENT"', provision[-1])
        self.assertIn('require.resolve("tsx")', provision[-1])
        self.assertIn("/app/node_modules/tsx/dist/loader.mjs", command)
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
        self.assertEqual([arg for arg in run if arg.startswith("/app/uzi-worker-uid-proof/agent/test/") and arg.endswith(".test.ts")], [
            "/app/uzi-worker-uid-proof/agent/test/codex-launcher.test.ts", "/app/uzi-worker-uid-proof/agent/test/codex-executor.test.ts",
            "/app/uzi-worker-uid-proof/agent/test/codex-shared-dir.test.ts", "/app/uzi-worker-uid-proof/agent/test/entrypoint-migration.test.ts",
            "/app/uzi-worker-uid-proof/agent/test/git-planning-diff.test.ts",
            "/app/uzi-worker-uid-proof/agent/test/git-retained-permissions.test.ts",
            "/app/uzi-worker-uid-proof/agent/test/claude-command-launcher.test.ts",
        ])
        self.assertEqual(run[run.index("--entrypoint") + 1], "/usr/local/sbin/uzi-entrypoint")
        self.assertEqual(run[run.index("--network") + 1], "none")
        self.assertEqual(run[run.index("--cap-drop") + 1], "ALL")
        self.assertEqual([run[index + 1] for index, arg in enumerate(run) if arg == "--cap-add"],
                         ["CHOWN", "DAC_OVERRIDE", "SETPCAP", "SETUID", "SETGID"])
        self.assertEqual(run[run.index("--security-opt") + 1], "no-new-privileges")
        self.assertEqual([run[index + 1] for index, arg in enumerate(run) if arg == "--tmpfs"],
                         ["/data", "/nix", "/app/uzi-worker-uid-proof:exec,uid=10001,gid=10001,mode=0755",
                          "/proof/.uzi/scratch:exec,uid=10001,gid=10001,mode=0700"])
        self.assertEqual(run[run.index("--entrypoint") + 2:run.index("--test")], [
            "sha256:" + "a" * 64, "/usr/local/bin/node", "--import", "/app/node_modules/tsx/dist/loader.mjs",
            "--import", "/app/uzi-worker-uid-proof/e2e/worker-uid/image-layout.mjs",
            "--import", "/app/uzi-worker-uid-proof/agent/test/setup/hermetic-proc.ts",
        ])
        return result, calls

    def test_ownership_failure_and_ambiguous_create(self):
        for options in ({"ownership_mismatch": "-command"}, {"ownership_mismatch": "-go"}, {"provision_rc": 23}):
            with self.subTest(options=options):
                self.calls.unlink(missing_ok=True)
                result, calls = self.run_lane(**options)
                self.assertNotEqual(result.returncode, 0)
                state = json.loads((self.repo / "resources.json").read_text())
                removed = [call[-1] for call in calls if call[:2] == ["docker", "rm"]
                           or call[:3] == ["docker", "volume", "rm"]]
                for name, item in state.items():
                    if name.endswith("-tools"): continue
                    mismatch = options.get("ownership_mismatch", "")
                    if mismatch and name.endswith(mismatch):
                        self.assertNotIn(item["Id"], removed)
                        self.assertNotIn(name, removed)
                        self.assertIn("ownership mismatch", result.stderr)
                    else:
                        self.assertIn(name if item.get("Kind") == "volume" else item["Id"], removed)

    def test_capture_overflow_in_both_lanes(self):
        from bounded import REPORT_BYTES
        for command in (False, True):
            with self.subTest(command=command):
                self.calls.unlink(missing_ok=True)
                options = {"command_mode": "overflow"} if command else {"xml_mode": "overflow"}
                result, _ = self.run_lane(**options)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("report overflow", result.stderr)
                prefix = "command-" if command else ""
                directory = next(line.removeprefix("worker UID reports: ") for line in result.stdout.splitlines()
                                 if line.startswith("worker UID reports: "))
                report = Path(directory) / (prefix + "results.xml")
                self.assertEqual(report.stat().st_size, REPORT_BYTES)
                self.assertEqual(Path(str(report) + ".status").read_text(), "EXIT=125\n")
                self.assertTrue(list(self.repo.glob("*.terminated")))
                self.assertIn(("command " if command else "") + "node/container exit: 125", result.stderr)

    def test_no_implicit_volumes_survive(self):
        result, _ = self.run_lane()
        self.assertEqual(result.returncode, 0, result.stderr)
        state = json.loads((self.repo / "resources.json").read_text())
        self.assertTrue(all(not item.get("AnonymousVolumes") for item in state.values()), state)
        calls = [json.loads(line) for line in self.calls.read_text().splitlines()]
        for command in [c for c in calls if c[:2] == ["docker", "run"]]:
            mounts = {command[i + 1].split(":")[0] for i, arg in enumerate(command) if arg == "--tmpfs"}
            self.assertIn("/data", mounts)
            self.assertIn("/nix", mounts)

    def test_readonly_tools_use_named_owned_volumes(self):
        result, calls = self.run_lane()
        self.assertEqual(result.returncode, 0, result.stderr)
        creates = [call for call in calls if call[:3] == ["docker", "volume", "create"]]
        self.assertEqual(len(creates), 2)
        for command in creates:
            self.assertIn("--label", command)
            self.assertRegex(command[-1], r"^wuid-2134-[0-9a-f]{32}-(go|task)$")

    def test_only_static_tool_directories_use_copyup_volumes(self):
        result, calls = self.run_lane()
        self.assertEqual(result.returncode, 0, result.stderr)
        creates = [c for c in calls if c[:3] == ["docker", "volume", "create"]]
        self.assertEqual(len(creates), 2)
        self.assertTrue(all(c[-1].endswith(("-go", "-task")) for c in creates))

    def test_success_and_build(self):
        result, calls = self.run_lane(skip_build="")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("1 required leaves passed", result.stdout)
        self.assertIn("phase end: image-layout elapsed=", result.stderr)
        self.assertEqual([call for call in calls if call[:2] == ["docker", "build"] and "--builder" not in call],
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
                    if mode == "timeout":
                        self.assertIn("deadline: phase=command-proof command=capture", result.stderr)
                else:
                    self.assertIn("worker UID lane FAIL", result.stderr)

    def test_command_status_and_checker_failure(self):
        result, _ = self.run_lane(command_xml="<testsuites/>", command_rc=77)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("expected exactly one result, found 0", result.stderr)
        self.assertIn("worker UID command node/container exit: 77", result.stderr)

    def test_cleanup_failure_still_attempts_volume(self):
        result, calls = self.run_lane(cleanup_rc=124)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Docker rm -f exit: 124", result.stderr)
        self.assertEqual(len([c for c in calls if c[:3] == ["docker", "volume", "rm"]]), 2,
                         "a failed container removal must not suppress owned volume cleanup attempts")

    def test_worker_skip(self):
        result, _ = self.run_lane(xml=PASS.replace("/>", '><skipped/></testcase>'))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("skipped from testcase", result.stderr)

    def test_setup_failures(self):
        for key in ("metadata_rc", "provision_rc", "build_rc", "volume_rc"):
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
        self.assertFalse(any(call[:2] == ["docker", "build"] and "--builder" not in call for call in calls))

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
