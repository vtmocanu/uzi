"""Pure small fixtures: no Docker, Go, subprocesses or native caches."""
import argparse
import contextlib
import json
import os
import socket
import threading
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import preflight as p


class PreflightTests(unittest.TestCase):
    def setUp(self):
        p.SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="cache-m1-", dir=p.SCRATCH)
        self.root = Path(self.temp.name)
        self.addCleanup(self.temp.cleanup)

    def manifest(self, **changes):
        value = dict(schema=2, margin_state="set", provenance="live-worker-config", observed_at=1000,
                     threshold=0.8, margin="0.1")
        value.update(changes)
        return value

    def tools(self):
        tools = self.root/"tools"
        tools.mkdir(mode=0o700)
        tools.chmod(0o700)
        for name in ("go", "cc"):
            path = tools/name
            path.write_text("fixture only; never execute")
            path.chmod(0o700)
        return str(tools)

    def charges(self):
        charges = {name: dict(path=str(self.root), bytes=0, inodes=1) for name in p.SINKS}
        charges["retained"]["bytes"] = p.LIMITS["retained"]
        charges["staging"]["bytes"] = p.LIMITS["staging"]
        charges["work"]["bytes"] = p.LIMITS["work_modules_output_logs"]
        return charges

    def test_fresh_scratch_setup(self):
        fresh = self.root/"fresh"/"scratch"
        with patch.object(p, "SCRATCH", fresh):
            case = PreflightTests()
            try:
                case.setUp()
                self.assertTrue(fresh.is_dir())
            finally:
                case.doCleanups()

    def test_explicit_tools(self):
        with self.assertRaises(ValueError):
            p.child_env(str(self.root), "DiskCache")
        tools = self.tools()
        self.assertEqual(p.child_env(str(self.root), "DiskCache", tools)["PATH"], tools)
        for path in ("", ".", tools+":", tools+"::"+tools):
            with self.assertRaises(ValueError):
                p.child_env(str(self.root), "DiskCache", path)
        for prog in ("white space", "'quoted'", "back\\slash"):
            with self.assertRaises(ValueError):
                p.child_env(str(self.root), "CacheProg", tools, cacheprog=str(self.root/prog))
        (Path(tools)/"cc").unlink()
        with self.assertRaises(ValueError):
            p.child_env(str(self.root), "DiskCache", tools)

    def test_frozen_charges(self):
        for sink, delta in (("retained", -1), ("staging", -1), ("work", -1), ("logs", 1)):
            charges = self.charges()
            charges[sink]["bytes"] += delta
            with self.subTest(sink=sink), self.assertRaises(ValueError):
                p.headroom(charges, 0.8)
        charges = self.charges()
        charges["logs"]["inodes"] = 0
        with self.assertRaises(ValueError):
            p.headroom(charges, 0.8)
        charges = self.charges()
        charges["work"]["bytes"] -= 42
        charges["logs"]["bytes"] = 42
        sample = SimpleNamespace(f_blocks=10**12, f_bavail=10**12, f_frsize=1,
                                 f_files=100, f_favail=100)
        self.assertTrue(p.headroom(charges, 0.8, lambda _: sample))

    def test_missing_expectations_before_probes(self):
        args = argparse.Namespace(manifest=None, endpoint="/fixture", docker_version=None,
            image=None, repo_digest=None, go_root="/fixture", source_sha256=None, experimental=True)
        for digest in (None, "", "z"*64):
            args.source_sha256 = digest
            with patch.object(p, "docker_get") as get, patch.object(p, "inventory") as inv:
                result = p.report(args)
            get.assert_not_called()
            inv.assert_not_called()
            for key in ("docker", "source", "environment"):
                self.assertEqual(result["legs"][key]["status"], "UNVERIFIED")
        args.image = "sha256:"+"c"*64
        for digest in (None, "", "invalid"):
            args.repo_digest = digest
            with patch.object(p, "docker_get") as get:
                result = p.report(args)
            get.assert_not_called()
            self.assertEqual(result["legs"]["image"]["status"], "UNVERIFIED")
        args.image = None
        args.docker_version = "expected"
        args.source_sha256 = "a"*64
        with patch.object(p, "docker_get", return_value={"Version": "other"}), patch.object(
                p, "inventory", return_value={"sha256": "b"*64}):
            result = p.report(args)
        for key in ("docker", "source"):
            self.assertEqual(result["legs"][key]["status"], "NEGATIVE")

    def test_shape_and_constructor_not_live_proof(self):
        fixture = self.root/"manifest.json"
        fixture.write_text(json.dumps(self.manifest()))
        args = argparse.Namespace(manifest=str(fixture), toolchain_path=self.tools(),
            endpoint=None, docker_version=None, image=None, repo_digest=None,
            go_root=None, source_sha256=None, experimental=True)
        with patch.object(p.time, "time", return_value=1000):
            result = p.report(args)
        self.assertEqual(result["legs"]["threshold"]["status"], "UNVERIFIED")
        self.assertTrue(result["legs"]["threshold"]["shape_valid"])
        self.assertAlmostEqual(result["legs"]["threshold"]["calculated_soft"], 0.7)
        self.assertEqual(result["legs"]["environment"]["status"], "FIXTURE-PROVEN")

    def test_unix_http_fixed_host_and_routes(self):
        # A live directory fd keeps the absolute Unix path short without mocking validation.
        directory_fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
        self.addCleanup(os.close, directory_fd)
        endpoint = f"/proc/self/fd/{directory_fd}/s"
        server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        server.settimeout(2)
        server.bind(endpoint)
        server.listen(2)
        requests, errors = [], []
        def serve():
            try:
                for _ in range(2):
                    with server.accept()[0] as conn:
                        conn.settimeout(2)
                        data = b""
                        while b"\r\n\r\n" not in data and len(data) < 8192:
                            chunk = conn.recv(1024)
                            if not chunk:
                                break
                            data += chunk
                        requests.append(data)
                        conn.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}")
            except Exception as error:
                errors.append(error)
        thread = threading.Thread(target=serve)
        thread.start()
        try:
            for route in ("/version", "/images/sha256:"+"a"*64+"/json"):
                self.assertEqual(p.docker_get(endpoint, route), {})
        finally:
            thread.join(5)
            server.close()
        self.assertFalse(thread.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(len(requests), 2)
        for request, route in zip(requests, ("/version", "/images/sha256:"+"a"*64+"/json")):
            self.assertTrue(request.startswith(("GET "+route+" HTTP/1.1").encode()))
            self.assertIn(b"Host: localhost\r\n", request)

    def test_threshold(self):
        self.assertAlmostEqual(p.threshold(self.manifest(), 1000), 0.7)
        for margin in ("0.8", "0.9"):
            self.assertEqual(p.threshold(self.manifest(margin=margin), 1000), 0.8)
        for margin in (None, "", " ", "bad", "-1", "1", "nan", "inf", "0_0", "1_0", "\x850\x85", "\ufeff0"):
            with self.subTest(margin=margin), self.assertRaises(ValueError):
                p.threshold(self.manifest(margin=margin), 1000)
        for margin in ("0x0", "0b0", "0o0", "0e0", " .0 "):
            self.assertEqual(p.threshold(self.manifest(margin=margin), 1000), 0.8)
        self.assertAlmostEqual(p.threshold(self.manifest(margin=None, margin_state="unset"), 1000), 0.7)
        for changes in (dict(margin=" ", margin_state="unset"), dict(margin_state="bad"),
                        dict(margin_state=None)):
            with self.assertRaises(ValueError):
                p.threshold(self.manifest(**changes), 1000)
        for value in (None, True, 0, -1, 1.1, float("nan"), float("inf"), "0.8"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                p.threshold(self.manifest(threshold=value), 1000)
        for changes in (dict(observed_at=699), dict(observed_at=1001),
                        dict(provenance="unknown"), dict(schema=1)):
            with self.assertRaises(ValueError):
                p.threshold(self.manifest(**changes), 1000)
        with self.assertRaises(ValueError):
            p.threshold({}, 1000)

    def test_environment_and_no_effects(self):
        tools = self.tools()
        before = list(self.root.iterdir())
        with patch.dict(os.environ, {"SECRET": "fixture", "HTTP_PROXY": "fixture", "GOCACHEPROG": "fixture"}):
            env = p.child_env(str(self.root), "DiskCache", tools)
        self.assertEqual(set(env), {"PATH", "LANG", "TZ", "HOME", "GOPATH", "GOMODCACHE",
            "GOCACHE", "GOTMPDIR", "TMPDIR", "GOTOOLCHAIN", "GOMAXPROCS", "GOENV",
            "GOFLAGS", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"})
        self.assertEqual(env["GOFLAGS"], "")
        self.assertEqual(env["GOENV"], "off")
        self.assertEqual(env["GOMAXPROCS"], "2")
        self.assertEqual(env["GOTOOLCHAIN"], "local")
        self.assertEqual(env["LANG"], "C.UTF-8")
        self.assertEqual(env["TZ"], "UTC")
        self.assertEqual(env["GIT_CONFIG_GLOBAL"], "/dev/null")
        self.assertEqual(env["GIT_CONFIG_SYSTEM"], "/dev/null")
        for key in ("HOME", "GOPATH", "GOMODCACHE", "GOCACHE", "GOTMPDIR", "TMPDIR"):
            self.assertTrue(Path(env[key]).is_relative_to(self.root))
        self.assertEqual(list(self.root.iterdir()), before)
        self.assertEqual(p.child_env(str(self.root), "CacheProg", tools, cacheprog=str(self.root/"prog"))["GOCACHEPROG"], str(self.root/"prog"))
        with self.assertRaises(ValueError):
            p.child_env(str(self.root), "DiskCache", tools, cacheprog="anything")

    def test_paths_and_bounded_manifest(self):
        for path in ("/tmp/fixture", str(self.root/".."/"escape")):
            with self.assertRaises(ValueError):
                p.scratch_path(path)
        (self.root/"link").symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(ValueError):
            p.scratch_path(str(self.root/"link"/"x"))
        fifo = self.root/"fifo"
        os.mkfifo(fifo)
        with self.assertRaises(ValueError):
            p.read_json(str(fifo))
        fixture = self.root/"manifest.json"
        fixture.write_text('{"a":1,"a":2}')
        with self.assertRaises(ValueError):
            p.read_json(str(fixture))
        fixture.write_bytes(b" "*(p.MAX_JSON+1))
        with self.assertRaises(ValueError):
            p.read_json(str(fixture))

    def test_multiple_devices_and_exact_boundaries(self):
        charges = self.charges()
        for name in p.SINKS:
            charges[name]["path"] = str(self.root/name)
        good = SimpleNamespace(f_blocks=10**12, f_bavail=10**12, f_frsize=1,
                               f_files=100, f_favail=100)
        bad = SimpleNamespace(f_blocks=10**12, f_bavail=10**12, f_frsize=1,
                              f_files=10, f_favail=3)
        with patch.object(p.Path, "stat", lambda path: SimpleNamespace(st_dev=str(path))):
            self.assertTrue(p.headroom(charges, 0.8, lambda _: good))
            self.assertFalse(p.headroom(charges, 0.8,
                lambda path: bad if path.name == "logs" else good))
        sample = SimpleNamespace(f_blocks=5*p.LIMITS["aggregate"],
            f_bavail=2*p.LIMITS["aggregate"], f_frsize=1, f_files=100, f_favail=100)
        self.assertFalse(p.headroom(self.charges(), 0.8, lambda _: sample))
        sample.f_bavail += 1
        self.assertTrue(p.headroom(self.charges(), 0.8, lambda _: sample))

    def test_statfs_every_sink_bytes_and_inodes(self):
        charges = self.charges()
        sample = SimpleNamespace(f_blocks=100*p.LIMITS["aggregate"], f_bavail=90*p.LIMITS["aggregate"], f_frsize=1, f_files=100, f_favail=90)
        with patch("preflight.time.time", return_value=1000):
            calls = []
            def statfs(path):
                calls.append(path)
                return sample
            self.assertTrue(p.headroom(charges, 0.8, statfs))
            self.assertTrue(p.headroom(charges, 0.8, statfs))
            self.assertEqual(len(calls), 2*len(p.SINKS))
            sample.f_blocks = p.LIMITS["aggregate"]
            sample.f_bavail = sample.f_blocks
            self.assertFalse(p.headroom(charges, 0.8, statfs))
            sample.f_blocks = 100*p.LIMITS["aggregate"]
            sample.f_bavail = sample.f_blocks
            charges["logs"]["inodes"] = 70
            self.assertFalse(p.headroom(charges, 0.8, statfs))
            del charges["logs"]
            with self.assertRaises(ValueError):
                p.headroom(charges, 0.8, statfs)

    def test_inventory(self):
        (self.root/"VERSION").write_text("go1.25.0\n")
        (self.root/"src").mkdir()
        (self.root/"src"/"fixture.go").write_text("package fixture\n")
        first = p.inventory(str(self.root))
        self.assertEqual(first, p.inventory(str(self.root)))
        self.assertEqual(first["files"], 1)
        self.assertEqual(first["version"], "go1.25.0")
        (self.root/"src"/"fixture.go").write_text("package changed\n")
        self.assertNotEqual(first["sha256"], p.inventory(str(self.root))["sha256"])
        (self.root/"src"/"link").symlink_to(self.root/"VERSION")
        with self.assertRaises(ValueError):
            p.inventory(str(self.root))

    def test_available_docker_never_proves_allocation(self):
        args = argparse.Namespace(manifest=None, endpoint="/fixture.sock", docker_version="fixture",
            image=None, repo_digest=None, go_root=None, source_sha256=None, experimental=True)
        with patch.object(p, "docker_get", return_value={"Version": "fixture"}) as get:
            result = p.report(args)
        self.assertEqual(get.call_count, 1)
        self.assertEqual(result["legs"]["docker"]["status"], "PROVEN")
        for key in ("allocation", "headroom", "tiny", "handler", "workersvc", "protocol"):
            self.assertEqual(result["legs"][key]["status"], "UNVERIFIED")
        for key in ("producer_launched", "production_authorized", "ab_trigger"):
            self.assertFalse(result[key])
        self.assertEqual(result["limits"]["aggregate"], 2147483648)
        self.assertEqual(result["frozen"]["T"], 268435456)

    def test_malformed_numeric_metadata_reports_unverified(self):
        fixture = self.root/"manifest.json"
        args = argparse.Namespace(manifest=str(fixture), endpoint="/fixture.sock", docker_version="fixture",
            image=None, repo_digest=None, go_root=None, source_sha256=None, experimental=True)
        for field in ("observed_at", "threshold", "margin"):
            fixture.write_text(json.dumps(self.manifest(**{field: 10**400})))
            with patch.object(p, "docker_get", side_effect=OSError):
                self.assertEqual(p.report(args)["legs"]["threshold"]["status"], "UNVERIFIED")
        fixture.write_text("["*1500 + "0" + "]"*1500)
        with patch.object(p, "docker_get", side_effect=OSError):
            self.assertEqual(p.report(args)["legs"]["threshold"]["status"], "UNVERIFIED")

    def test_inventory_stops_during_stream_enumeration(self):
        (self.root/"VERSION").write_text("go1.27.3\n")
        (self.root/"src").mkdir()
        consumed = []
        class Entry:
            def __init__(self, n):
                self.name = "entry" + str(n)
            def is_dir(self, **_):
                return False
            def is_symlink(self):
                return False
        @contextlib.contextmanager
        def scan(_):
            def entries():
                for n in range(20005):
                    consumed.append(n)
                    yield Entry(n)
            yield entries()
        with patch.object(p.os, "scandir", scan):
            with self.assertRaises(ValueError):
                p.inventory(str(self.root))
        self.assertLessEqual(len(consumed), 20001)

    def test_json_sanitization(self):
        raw = p.encode({"value": "fixture\x1b[31m\n\u202e"})
        self.assertTrue(all(32 <= ord(c) < 127 for c in raw))
        self.assertIn("\\u001b", raw)
        self.assertIn("\\u202e", raw)
        self.assertEqual(json.loads(raw)["value"], "fixture\x1b[31m\n\u202e")
        with self.assertRaises(ValueError):
            p.encode({"value": "x"*p.MAX_JSON})


if __name__ == "__main__":
    unittest.main()
