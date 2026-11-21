"""Regression fixtures only: no real Docker endpoint or producing workload."""
import contextlib
import copy
import io
import json
import os
import select
import subprocess
import sys
import unittest
from unittest.mock import patch

import observe as o
import preflight as p
import qualify as q
import test_observe as to
import test_preflight as tp
import test_qualify as tq


class VerifierRegressions(tq.Base):
    def sized(self):
        art = tq.full_artifact()
        for m in art["mountinfo"]["mounts"]:
            if m["fstype"] == "tmpfs":
                m["options"].update(size=tq.MIB, nr_inodes=10)
        return art

    def leg(self, art, name, collector=False):
        ce = q.collect(tq.StubObserver(art, tuple(n for n in tq.ALL_SECTIONS if n in art))) if collector else None
        return q.evaluate(art, collector=ce, now=tq.NOW)["legs"][name]

    def test_empty_mount_inventory_never_proves_charge_map_or_headroom(self):
        art = self.sized()
        art["mountinfo"].update(mounts=[], fds=[])
        for name in ("charge_map", "enforcement", "aliasing", "backing"):
            with self.subTest(leg=name):
                got = self.leg(art, name, True)
                self.assertIn("no-container-mount-facts", got["reasons"])
                self.assertNotEqual(got["status"], "PROVEN")
        art = self.sized()
        art["mountinfo"]["fds"] = []
        self.assertEqual(self.leg(art, "charge_map", True)["status"], "PROVEN")

    def test_invalid_threshold_never_proves_backing(self):
        for defect in ("stale", "contradictory", "missing"):
            art = self.sized()
            if defect == "stale":
                art["threshold"]["api_observed_at"] = tq.NOW-301
            elif defect == "contradictory":
                art["threshold"]["effective"]["value"] = 0.7
            else:
                del art["threshold"]
            with self.subTest(defect=defect):
                got = self.leg(art, "backing", True)
                self.assertNotIn(got["status"], ("PROVEN", "FIXTURE-CONSISTENT"))
                self.assertTrue(got["reasons"])

    def test_grandchild_coverage_does_not_depend_on_map_order(self):
        claims, mounts, sinks = tq.tmpfs_env()
        for names in (("grandchild", "child", "work", "output"),
                      ("output", "work", "child", "grandchild")):
            all_sinks = dict(sinks, child=dict(identity="child", parent="work"),
                             grandchild=dict(identity="grandchild", parent="child"))
            got = tq.run(claims, mounts, {n: all_sinks[n] for n in names})
            self.assertEqual(got["coverage"]["grandchild"], dict(bytes=True, inodes=True))
            self.assertEqual(got["reasons"], [])

    def test_byte_only_capacity_delta_is_projected(self):
        art = self.sized()
        work = next(m for m in art["mountinfo"]["mounts"] if m["superblock"] == "sb-work")
        work["options"]["size"] = 4*tq.MIB
        self.assertEqual(self.leg(art, "backing")["reasons"], [])
        declared = sum(s["bytes"] for s in art["charge_map"]["sinks"].values() if "parent" not in s)
        art["backing"]["domains"]["outer-A"]["observed"]["capacity_bytes"] = int((declared+tq.MIB)/0.8)
        self.assertIn("backing-aggregate-exceeds-soft", self.leg(art, "backing")["reasons"])
        art["charge_map"]["sinks"]["work"]["inodes"] = 0
        self.assertIn("sink-malformed", self.leg(art, "charge_map")["reasons"])

    def test_enforced_delta_cannot_overwrite_a_named_sink(self):
        art = self.sized()
        work = next(m for m in art["mountinfo"]["mounts"] if m["superblock"] == "sb-work")
        work["options"].update(size=2*tq.MIB, nr_inodes=20)
        name = "enforced:sb-work"
        art["mountinfo"]["mounts"].append(tq.tmpfs(999, "/extra", "sb-extra", size=10*tq.MIB, inodes=10))
        art["charge_map"]["sinks"][name] = tq.sink(name, "/extra", 999, "sb-extra", b=10*tq.MIB, dom="outer-B")
        art["mountinfo"]["realpaths"]["/extra"] = "/extra"
        art["backing"]["domains"]["outer-B"] = copy.deepcopy(art["backing"]["domains"]["outer-A"])
        art["backing"]["domains"]["outer-B"]["observed"]["capacity_bytes"] = 10*tq.MIB
        self.assertIn("backing-aggregate-exceeds-soft", self.leg(art, "backing")["reasons"])

    def quota_artifact(self, quota):
        art = self.sized()
        name = "container_writable_layer"
        art["mountinfo"]["mounts"] = [m for m in art["mountinfo"]["mounts"] if m["superblock"] != "sb-"+name]
        root = art["mountinfo"]["mounts"][0]
        root["ro"] = False
        art["charge_map"]["sinks"][name] = tq.sink(name, "/layer", 1, root["superblock"])
        art["mountinfo"]["realpaths"]["/layer"] = "/layer"
        art["claims"] = [c for c in art["claims"] if name not in c["sinks"]]
        art["claims"].append(tq.cl("storage-opt-size", "bytes", [name]))
        art["runtime"].update(backend_support=True)
        art["runtime"]["applied"]["writable_layer_quota_bytes"] = quota
        return art

    def test_accepted_writable_layer_quota_is_projected_and_not_double_counted(self):
        art = self.quota_artifact(3*tq.GIB)
        art["backing"]["domains"]["outer-A"]["observed"]["capacity_bytes"] = tq.GIB
        self.assertIn("enforced-capacity-exceeds-envelope", self.leg(art, "envelope")["reasons"])
        self.assertIn("backing-aggregate-exceeds-soft", self.leg(art, "backing")["reasons"])

    def test_writable_layer_quota_uses_real_headroom_and_one_superblock(self):
        quota = 3*tq.GIB//2
        art = self.quota_artifact(quota)
        art["mountinfo"]["mounts"][0].update(exclusive=True, capacity_bytes=quota, capacity_inodes=100)
        self.assertNotIn("enforced-capacity-exceeds-envelope", self.leg(art, "envelope")["reasons"])
        art["backing"]["domains"]["outer-A"]["observed"]["capacity_bytes"] = tq.GIB
        self.assertIn("backing-aggregate-exceeds-soft", self.leg(art, "backing")["reasons"])
        art["backing"]["domains"]["outer-A"]["observed"]["capacity_bytes"] = 10*tq.GIB
        self.assertEqual(self.leg(art, "backing")["reasons"], [])

    def test_accepted_quota_reaches_backing_without_superblock_capacity(self):
        art = self.quota_artifact(3*tq.GIB)
        art["backing"]["domains"]["outer-A"]["observed"]["capacity_bytes"] = tq.GIB
        self.assertIn("backing-aggregate-exceeds-soft", self.leg(art, "backing")["reasons"])

    def test_expanded_cli_report_returns_bounded_refusal(self):
        art = tq.full_artifact()
        art["mountinfo"]["mounts"] += [dict(mount_id=n, superblock="p", fstype="proc", mountpoint="/p")
                                             for n in range(1000, 1450)]
        raw = json.dumps(art, separators=(",", ":"))
        self.assertLess(len(raw), p.MAX_JSON)
        path = self.root/"large.json"
        path.write_text(raw)
        out = io.StringIO()
        try:
            with contextlib.redirect_stdout(out):
                code = q.main(["--artifact", str(path)])
        except ValueError:
            self.fail("expanded mount report must return bounded JSON, not raise")
        self.assertEqual(code, 2)
        got = json.loads(out.getvalue())
        self.assertFalse(got["qualified"])
        self.assertEqual({v["reason"] for v in got["legs"].values()}, {"report-limit"})
        self.assertLessEqual(len(out.getvalue().encode()), p.MAX_JSON)


class CollectorRegressions(to.Base):
    def test_stub_request_reader_stops_at_eof_and_keeps_complete_requests(self):
        class Connection:
            def __init__(self, chunks):
                self.chunks = iter(chunks)
            def recv(self, size):
                return next(self.chunks)
        self.assertIsNone(tp.read_request_headers(Connection([b"GET / HTTP/1.1\r\n", b""])))
        self.assertEqual(tp.read_request_headers(Connection([b"GET / HTTP/1.1\r\n", b"\r\n"])),
                         b"GET / HTTP/1.1\r\n\r\n")

    def test_active_output_directory_cannot_be_reset_and_sequential_reuse_works(self):
        daemon = self.stub([to.image("1")])
        ready_r, ready_w = os.pipe()
        release_r, release_w = os.pipe()
        script = ("import sys,os\nsys.path.insert(0,%r)\nimport observe\n"
                  "def hook(out,err):\n out.write(b'active\\n')\n os.write(%d,b'1')\n os.read(%d,1)\n"
                  "sys.exit(observe.main(%r,hook=hook))\n") % (
                      str(to.HERE), ready_w, release_r, ["--endpoint", daemon.endpoint, "--out-dir", str(self.out)])
        child = subprocess.Popen([sys.executable, "-B", "-I", "-c", script],
                                 pass_fds=(ready_w, release_r), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        os.close(ready_w)
        os.close(release_r)
        try:
            self.assertTrue(select.select([ready_r], [], [], 5)[0], "active collector reached its barrier")
            self.assertEqual(os.read(ready_r, 1), b"1")
            inode = (self.out/"run.log").stat().st_ino
            second = self.observe(daemon.endpoint)
            self.assertEqual(second.returncode, 3, "a contender must refuse without resetting the active report")
            self.assertIn(b"out-dir-busy", second.stdout+second.stderr)
            self.assertEqual((self.out/"run.log").stat().st_ino, inode)
        finally:
            os.write(release_w, b"1")
            os.close(release_w)
            os.close(ready_r)
            try:
                child.communicate(timeout=10)
            except subprocess.TimeoutExpired:
                child.kill()
                child.communicate()
        self.assertEqual(child.returncode, 2)
        self.assertEqual(self.observe(daemon.endpoint).returncode, 2)

    def test_collection_deadline_covers_initial_routes_and_last_response(self):
        # Owned fixture returns decoded response objects, never contacts a daemon.
        docs = {"/version": {"Version": "29.8.0", "Os": "linux", "Arch": "amd64"},
                "/info": to.info(), "/images/json": [{"Id": to.iid("1")}],
                "/images/"+to.iid("1")+"/json": to.image("1")}
        for expire_route in ("/info", "/images/"+to.iid("1")+"/json"):
            clock = [0.0]
            seen = []
            def get(endpoint, route, **kw):
                seen.append((route, kw))
                clock[0] = 91.0 if route == expire_route else clock[0]+1
                return docs[route]
            with self.subTest(route=expire_route), patch.object(o.p, "docker_get", side_effect=get):
                observer = o.LiveObserver("tcp://127.0.0.1:1", monotonic=lambda: clock[0])
                with self.assertRaises(o.Refusal) as caught:
                    observer.observe()
                self.assertEqual(caught.exception.code, "docker-unavailable")
                self.assertEqual(seen[-1][0], expire_route)
                self.assertTrue(all(0 < kw["timeout"] <= 90 for _, kw in seen))

    def test_overlong_profile_line_is_not_recorded_as_a_truncated_coordinate(self):
        prefix = "/nix/store/"+"a"*32+"-"
        profile = self.root/"profile"
        profile.write_text(prefix+"x"*4100+"!\n")
        got = o.host_coordinates(None, None, str(profile))
        self.assertIsNone(got["toolchain_profile"])
        self.assertTrue(got["toolchain_profile_dropped"])
        profile.write_text(prefix+"go")
        self.assertEqual(o.host_coordinates(None, None, str(profile))["toolchain_profile"], prefix+"go")

    def test_absent_security_options_are_unknown_not_rootful(self):
        for options in (None, ["name=other"]*33, [123], ["x"*257]):
            info = to.info()
            info["SecurityOptions"] = options
            daemon = o.daemon_section({}, info)
            with self.subTest(options=options):
                self.assertIsNone(daemon["rootless"])
                self.assertEqual(q.daemon_capability(daemon), ["daemon-info-missing"])

    def test_interim_response_cannot_bypass_header_limit(self):
        def send(conn, stop, end):
            conn.sendall(b"HTTP/1.1 100 Continue\r\n\r\n"+
                         b"HTTP/1.1 200 OK\r\nX-Pad: "+b"x"*p.LIMITS["header"]+
                         b"\r\nContent-Length: 2\r\n\r\n{}")
        endpoint = tp.PreflightTests.hostile_header_server(self, send)
        with self.assertRaises(ValueError):
            p.docker_get(endpoint, "/version")
