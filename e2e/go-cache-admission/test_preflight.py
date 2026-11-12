"""Pure small fixtures: no Docker, Go, subprocesses or native caches."""
import argparse
import contextlib
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import preflight as p


class PreflightTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cache-m1-", dir=p.SCRATCH)
        self.root = Path(self.temp.name)
        self.addCleanup(self.temp.cleanup)

    def manifest(self, **changes):
        value = dict(schema=1, provenance="live-worker-config", observed_at=1000,
                     threshold=0.8, margin="0.1")
        value.update(changes)
        return value

    def test_threshold(self):
        self.assertAlmostEqual(p.threshold(self.manifest(), 1000), 0.7)
        for margin in ("0.8", "0.9"):
            self.assertEqual(p.threshold(self.manifest(margin=margin), 1000), 0.8)
        for margin in (None, "", "bad", "-1", "1", "nan", "inf"):
            self.assertAlmostEqual(p.threshold(self.manifest(margin=margin), 1000), 0.7)
        for value in (None, True, 0, -1, 1.1, float("nan"), float("inf"), "0.8"):
            with self.subTest(value=value), self.assertRaises(ValueError):
                p.threshold(self.manifest(threshold=value), 1000)
        for changes in (dict(observed_at=699), dict(observed_at=1001),
                        dict(provenance="unknown"), dict(schema=2)):
            with self.assertRaises(ValueError):
                p.threshold(self.manifest(**changes), 1000)
        with self.assertRaises(ValueError):
            p.threshold({}, 1000)

    def test_environment_and_no_effects(self):
        before = list(self.root.iterdir())
        with patch.dict(os.environ, {"SECRET": "fixture", "HTTP_PROXY": "fixture", "GOCACHEPROG": "fixture"}):
            env = p.child_env(str(self.root), "DiskCache")
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
        self.assertEqual(p.child_env(str(self.root), "CacheProg", cacheprog=str(self.root/"prog"))["GOCACHEPROG"], str(self.root/"prog"))
        with self.assertRaises(ValueError):
            p.child_env(str(self.root), "DiskCache", cacheprog="anything")

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

    def test_statfs_every_sink_bytes_and_inodes(self):
        charges = {name: dict(path=str(self.root), bytes=1, inodes=1) for name in p.SINKS}
        sample = SimpleNamespace(f_blocks=100, f_bavail=90, f_frsize=1, f_files=100, f_favail=90)
        with patch("preflight.time.time", return_value=1000):
            calls = []
            def statfs(path):
                calls.append(path)
                return sample
            self.assertTrue(p.headroom(charges, 0.8, statfs))
            self.assertTrue(p.headroom(charges, 0.8, statfs))
            self.assertEqual(len(calls), 2*len(p.SINKS))
            charges["logs"]["bytes"] = 70
            self.assertFalse(p.headroom(charges, 0.8, statfs))
            charges["logs"]["bytes"] = 1
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
