"""Fixture-only tests for qualify.py. Every fixture below is synthetic: no Docker, Go,
subprocess or live observation; a PROVEN status here shows the code path, not an environment."""
import contextlib
import copy
import hashlib
import io
import json
import os
import socket
import tempfile
import threading
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import preflight as p
import qualify as q

MIB = 1024**2
GIB = 1024**3
NOW = 1_000_000.0
EXTRA = ("toolchain_download", "cache_init", "fuzz", "exported_archives", "linked_executables",
         "stdout_stderr", "harness_reports", "setup_cleanup", "container_writable_layer",
         "container_network_binds", "shm", "daemon_image_store", "daemon_build_cache",
         "daemon_metadata_snapshots", "volumes", "container_logs", "temp_extraction")
OUTER = ("daemon_image_store", "daemon_build_cache", "daemon_metadata_snapshots")
SCHEMA = "uzi-go-cache-env/1"


def sha(char):
    return "sha256:" + char*64


def mnt(mid, point, fstype, ro=False, sb=None, **extra):
    m = dict(mount_id=mid, superblock=sb or "sb-%d" % mid, fstype=fstype, mountpoint=point,
             ro=ro, options={})
    m.update(extra)
    return m


def tmpfs(mid, point, sb=None, size=4*MIB, inodes=100, ro=False):
    options = {}
    if size is not None:
        options["size"] = size
    if inodes is not None:
        options["nr_inodes"] = inodes
    return mnt(mid, point, "tmpfs", ro, sb, options=options)


def sink(name, path, mid, sb, b=MIB, i=10, dom="outer-A"):
    return dict(identity=name, paths=[path], mount_id=mid, superblock=sb, bytes=b, inodes=i,
                backing_domain=dom)


def cl(kind, resource, sinks=(), **extra):
    value = dict(kind=kind, resource=resource, sinks=list(sinks))
    value.update(extra)
    return value


def facts(**over):
    value = dict(
        tier=dict(mountinfo="collector", runtime="collector", daemon="collector"),
        new_mount=dict(cap_bnd=0, no_new_privs=1, seccomp=2, seccomp_profile="default",
                       max_user_namespaces=None),
        container=dict(privileged=False, caps=[], cgroupfs_writable=False, cgroupns=True),
        applied=dict(pids_max=100, memory_max=GIB, memory_swap_max=GIB, log_driver="none"),
        daemon=dict(MemoryLimit=True, SwapLimit=True, PidsLimit=True, CgroupDriver="systemd",
                    CgroupVersion="2", rootless=False),
        realpaths={}, backend_support=False)
    value.update(over)
    return value


def with_domains(sinks):
    out = dict(sinks)
    out["process_tree"] = dict(pseudo="process_tree")
    out["memory"] = dict(pseudo="memory")
    return out


def tmpfs_env():
    mounts = [mnt(1, "/", "overlay", True, sb="sb-root"),
              tmpfs(10, "/t/work", sb="sb-work"), tmpfs(11, "/t/output", sb="sb-output")]
    sinks = {"work": sink("work", "/t/work", 10, "sb-work"),
             "output": sink("output", "/t/output", 11, "sb-output")}
    claims = []
    for name in ("work", "output"):
        claims += [cl("tmpfs-size", "bytes", [name], superblock="sb-"+name),
                   cl("tmpfs-nr_inodes", "inodes", [name], superblock="sb-"+name)]
    return claims, mounts, sinks


def ro_env():
    mounts = [mnt(1, "/", "overlay", True, sb="sb-root"), tmpfs(12, "/dev/shm", sb="sb-shm"),
              mnt(13, "/vol", "ext4", False, sb="sb-vol", volume=True),
              mnt(14, "/etc/hosts", "ext4", False, sb="sb-hosts", bind=True),
              mnt(15, "/bindx", "ext4", False, sb="sb-bx", bind=True),
              tmpfs(16, "/t", sb="sb-t")]
    sinks = {"work": sink("work", "/work", 1, "sb-root"),
             "shm": sink("shm", "/dev/shm/a", 12, "sb-shm"),
             "volumes": sink("volumes", "/vol/a", 13, "sb-vol"),
             "container_network_binds": sink("container_network_binds", "/etc/hosts", 14, "sb-hosts"),
             "gotmp": sink("gotmp", "/bindx/a", 15, "sb-bx"),
             "tmp": sink("tmp", "/t/x", 16, "sb-t"),
             "home": sink("home", "/home/s", 1, "sb-root"),
             "cache": sink("cache", "/c", 1, "sb-root"),
             "logs": sink("logs", "/l", 1, "sb-root")}
    real = {s["paths"][0]: s["paths"][0] for s in sinks.values()}
    real.update({"/home/s": "/t/x", "/c": "/vol/c", "/l": "/bindx/l"})
    return mounts, sinks, facts(realpaths=real)


def ro_claim(names):
    return cl("readonly-rootfs", "write-deny", names)


def run(claims, mounts, sinks, fact=None, **kw):
    return q.evaluate_claims(claims, mounts, sinks, fact or facts(), **kw)


def good_binding():
    return dict(repo_revision="a"*40, scaffold_revision="b"*40, index_digest=sha("1"),
                platform_digest=sha("2"), image_id=sha("3"), running_container_image_id=sha("3"),
                go_declared=True,
                go=dict(sha256="4"*64, version="go1.25.0", source_sha256="5"*64,
                        observed_in="image-fs", provenance=dict(kind="signed-output-metadata")),
                toolchain_mounts=[dict(path="/usr/local/go", ro=True)])


def good_threshold():
    return dict(api_value=0.9, api_observed_at=NOW-10, margin_state="set", margin_raw="0.1",
                incarnation="inc-1",
                effective=dict(kind="soft-threshold-log", incarnation="inc-1",
                               observed_at=NOW-5, value=0.8))


def full_artifact():
    mounts = [mnt(1, "/", "overlay", True, sb="sb-root"), mnt(2, "/proc", "proc", False),
              mnt(3, "/dev/pts", "devpts", False), mnt(4, "/sys", "sysfs", True),
              mnt(5, "/sys/fs/cgroup", "cgroup2", True)]
    sinks, claims, realpaths = {}, [], {}
    mid = 10
    for name in p.SINKS + EXTRA:
        if name in OUTER or name == "container_logs":
            sinks[name] = sink(name, "", None, None)
            sinks[name].update(paths=[], outer_domain=True)
            del sinks[name]["superblock"]
        elif name == "temp_extraction":
            sinks[name] = dict(identity=name, parent="tmp")
        else:
            path = "/sinks/"+name
            sb = "sb-"+name
            mounts.append(tmpfs(mid, path, sb=sb))
            sinks[name] = sink(name, path, mid, sb)
            realpaths[path] = path
            claims += [cl("tmpfs-size", "bytes", [name], superblock=sb),
                       cl("tmpfs-nr_inodes", "inodes", [name], superblock=sb)]
            mid += 1
    claims += [cl("pids-max", "pids", ["process_tree"]), cl("log-driver-none", "logs", ["container_logs"]),
               cl("memcg-max-eq-swap", "memory-swap", ["memory"])]
    return dict(
        schema=SCHEMA, binding=good_binding(), threshold=good_threshold(),
        charge_map=dict(sinks=sinks),
        mountinfo=dict(source="collector", mounts=mounts, fds=[dict(mount_id=10, writable=True)],
                       realpaths=realpaths),
        runtime=dict(source="collector", new_mount=facts()["new_mount"],
                     container=facts()["container"], applied=facts()["applied"]),
        daemon=dict(source="collector", **facts()["daemon"]),
        claims=claims,
        envelope=dict(limits=dict(p.LIMITS), inode_ceiling=dict(value=1000, owner_approved=True)),
        backing=dict(source="collector", domains=dict({"outer-A": dict(observed=dict(
            capacity_bytes=10**12, capacity_inodes=10**9, used_bytes=0, used_inodes=0,
            concurrent_bytes=0, concurrent_inodes=0, sampled_at=NOW-1))})))


COLLECTOR_SECTIONS = ("binding", "threshold", "mountinfo", "runtime", "daemon", "backing")
ALL_SECTIONS = COLLECTOR_SECTIONS + ("charge_map", "claims")


class StubObserver:
    """Owned collector stub (fixture only): returns canned sections, never touches a host."""
    def __init__(self, artifact, names=COLLECTOR_SECTIONS):
        self.sections_ = {k: copy.deepcopy(artifact[k]) for k in names}

    def observe(self):
        return copy.deepcopy(self.sections_)

    def daemon_id(self):
        return "fixture-daemon-id"


def reasons(leg):
    return leg["reasons"]


class Base(unittest.TestCase):
    def setUp(self):
        p.SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="qualify-", dir=p.SCRATCH)
        self.root = Path(self.temp.name)
        self.addCleanup(self.temp.cleanup)


class BindingTests(Base):
    def test_good_binding_has_no_reasons(self):
        self.assertEqual(q.check_binding(good_binding(), "a"*40), [])

    def test_stale_or_unbound_source(self):
        self.assertIn("binding-source-unbound", q.check_binding(good_binding(), "c"*40))
        for key in ("repo_revision", "scaffold_revision"):
            value = good_binding()
            del value[key]
            self.assertIn("binding-source-unbound", q.check_binding(value, "a"*40), key)

    def test_running_image_must_equal_installed_image(self):
        value = good_binding()
        value["running_container_image_id"] = sha("9")
        self.assertIn("binding-image-mismatch", q.check_binding(value))

    def test_host_only_go(self):
        value = good_binding()
        value["go"]["observed_in"] = "host"
        got = q.check_binding(value)
        self.assertIn("go-host-only", got)
        self.assertIn("no-go-bearing-image-verified", got)

    def test_coordinate_provenance(self):
        for kind in ("tag", "label", "profile-path", "devbox-lock", "narhash"):
            value = good_binding()
            value["go"]["provenance"] = dict(kind=kind)
            with self.subTest(kind=kind):
                self.assertIn("provenance-coordinate-only", q.check_binding(value))

    def test_go_not_declared_is_not_absence(self):
        value = good_binding()
        value["go_declared"] = False
        value["go"] = {}
        got = q.check_binding(value)
        self.assertIn("go-not-declared", got)
        self.assertNotIn("go-absent-verified", got)
        value["go"] = dict(observed_in="image-fs", absent_verified=True)
        got = q.check_binding(value)
        self.assertIn("go-absent-verified", got)
        self.assertNotIn("go-not-declared", got)
        self.assertIn("no-go-bearing-image-verified", got)

    def test_writable_toolchain_mount_is_negative(self):
        value = good_binding()
        value["toolchain_mounts"] = [dict(path="/usr/local/go", ro=False)]
        self.assertIn("toolchain-mount-writable", q.check_binding(value))
        art = full_artifact()
        art["binding"] = value
        leg = q.evaluate(art, now=NOW)["legs"]["binding"]
        self.assertEqual(leg["status"], "NEGATIVE")
        self.assertIn("toolchain-mount-writable", reasons(leg))


class ThresholdTests(Base):
    def test_good_threshold(self):
        got = q.check_threshold(good_threshold(), NOW)
        self.assertEqual(got["reasons"], [])
        self.assertAlmostEqual(got["soft"], 0.8)

    def test_not_supplied(self):
        for value in (None, {}):
            self.assertIn("owner-input-not-supplied", q.check_threshold(value, NOW)["reasons"])

    def test_fresh_api_alone_is_not_the_effective_value(self):
        value = good_threshold()
        del value["effective"]
        self.assertIn("effective-value-unestablished", q.check_threshold(value, NOW)["reasons"])
        value["effective"] = dict(kind="api-response", incarnation="inc-1", observed_at=NOW-1, value=0.8)
        self.assertIn("effective-value-unestablished", q.check_threshold(value, NOW)["reasons"])

    def test_missing_effective_yields_reason_without_exception(self):
        for effective in (None, "x", [], {}):
            value = dict(good_threshold(), effective=effective)
            with self.subTest(effective=effective):
                got = q.check_threshold(value, NOW)
                self.assertIn("effective-value-unestablished", got["reasons"])
                self.assertIsNone(got["soft"])
        art = full_artifact()
        art["threshold"]["effective"] = None
        leg = q.evaluate(art, now=NOW)["legs"]["threshold"]
        self.assertIn("effective-value-unestablished", reasons(leg))
        self.assertNotIn("malformed-section", reasons(leg))

    def test_stale_and_future(self):
        for patch_ in (dict(api_observed_at=NOW-301), dict(api_observed_at=NOW+5)):
            value = dict(good_threshold(), **patch_)
            self.assertIn("threshold-stale", q.check_threshold(value, NOW)["reasons"])
        for stamp in (NOW-301, NOW+5):
            value = good_threshold()
            value["effective"]["observed_at"] = stamp
            self.assertIn("threshold-stale", q.check_threshold(value, NOW)["reasons"])

    def test_contradictory(self):
        value = good_threshold()
        value["effective"]["value"] = 0.5
        self.assertIn("threshold-contradictory", q.check_threshold(value, NOW)["reasons"])
        value = good_threshold()
        value["effective"]["incarnation"] = "inc-2"
        self.assertIn("threshold-contradictory", q.check_threshold(value, NOW)["reasons"])

    def test_soft_falls_back_to_threshold_when_margin_not_smaller(self):
        value = good_threshold()
        value.update(api_value=0.05, margin_state="unset", margin_raw=None)
        value["effective"]["value"] = 0.05
        got = q.check_threshold(value, NOW)
        self.assertEqual(got["reasons"], [])
        self.assertAlmostEqual(got["soft"], 0.05)

    def test_unsupported_margin_input(self):
        for raw in (5, "x"*257, "1\x00"):
            with self.subTest(raw=raw), self.assertRaises(q.JsParseUnsupported):
                q.config_margin(raw)
        value = good_threshold()
        value["margin_raw"] = 5
        self.assertIn("margin-js-parse-unsupported", q.check_threshold(value, NOW)["reasons"])

    def test_margin_golden_parity(self):
        golden = json.loads(Path(q.GOLDEN_PATH).read_text())
        self.assertGreaterEqual(len(golden["rows"]), 22)
        for row in golden["rows"]:
            with self.subTest(row=row["input"]):
                self.assertEqual(q.config_margin(row["input"])[0], row["expected"])

    def test_margin_fell_back_flag(self):
        self.assertEqual(q.config_margin("abc"), (0.1, True))
        self.assertEqual(q.config_margin(None), (0.1, True))
        self.assertEqual(q.config_margin("0.25"), (0.25, False))
        self.assertEqual(q.config_margin("　 0.2 "), (0.2, False))

    def test_config_ts_hash_matches_and_drift_is_red(self):
        self.assertTrue(q.golden_matches())
        drifted = self.root/"config.ts"
        drifted.write_bytes((p.ROOT/"agent/src/config.ts").read_bytes() + b"\n// drift\n")
        self.assertFalse(q.golden_matches(config_ts=drifted))
        value = good_threshold()
        self.assertIn("margin-js-parse-unsupported",
                      q.check_threshold(value, NOW, config_ts=drifted)["reasons"])
        recorded = json.loads(Path(q.GOLDEN_PATH).read_text())["config_ts_sha256"]
        self.assertEqual(recorded, hashlib.sha256((p.ROOT/"agent/src/config.ts").read_bytes()).hexdigest())


class ChargeMapTests(Base):
    def test_charge_sinks_superset_of_preflight_sinks_verbatim(self):
        self.assertEqual(tuple(q.CHARGE_SINKS[:len(p.SINKS)]), p.SINKS)
        self.assertEqual(set(q.CHARGE_SINKS), set(p.SINKS) | set(EXTRA))

    def test_missing_sink(self):
        sinks = full_artifact()["charge_map"]["sinks"]
        self.assertEqual(q.check_sinks(sinks), [])
        for name in ("tmp", "home", "daemon_build_cache"):
            partial = {k: v for k, v in sinks.items() if k != name}
            with self.subTest(name=name):
                self.assertIn("sink-missing", q.check_sinks(partial))

    def test_unaccounted_mqueue(self):
        got = q.check_mounts([mnt(7, "/dev/mqueue", "mqueue", False)], [])
        self.assertIn("unaccounted-writable-mount", got["reasons"])
        for mounts in ([], [mnt(7, "/dev/mqueue", "mqueue", True)]):
            self.assertEqual(q.check_mounts(mounts, [])["reasons"], [])

    def test_unknown_fstype_and_bind_volume(self):
        for m in (mnt(7, "/x", "ext4", False), mnt(8, "/y", "tmpfs", False, bind=True),
                  mnt(9, "/z", "tmpfs", False, volume=True)):
            with self.subTest(mount=m["mountpoint"]):
                self.assertIn("unaccounted-writable-mount", q.check_mounts([m], [])["reasons"])

    def test_proc_devpts_non_storage_allowlist(self):
        got = q.check_mounts([mnt(2, "/proc", "proc"), mnt(3, "/dev/pts", "devpts")], [])
        self.assertEqual(got["reasons"], [])
        self.assertEqual({a["mount_id"]: a["reason"] for a in got["accepted"]},
                         {2: "pseudo-filesystem, no persistent allocation",
                          3: "pty count bounded host-wide; not a storage sink"})

    def test_sysfs_cgroup(self):
        ok = [mnt(4, "/sys", "sysfs", True), mnt(5, "/sys/fs/cgroup", "cgroup2", True)]
        self.assertEqual(q.check_mounts(ok, [])["reasons"], [])
        for fstype in ("sysfs", "cgroup", "cgroup2"):
            with self.subTest(fstype=fstype):
                self.assertIn("unaccounted-writable-mount",
                              q.check_mounts([mnt(4, "/sys", fstype, False)], [])["reasons"])

    def test_writable_network_bind(self):
        for point in ("/etc/hosts", "/etc/hostname", "/etc/resolv.conf"):
            with self.subTest(point=point):
                self.assertIn("unaccounted-writable-mount",
                              q.check_mounts([mnt(6, point, "tmpfs", False)], [])["reasons"])

    def test_kernel_memory_retains_every_mount(self):
        mounts = [mnt(1, "/", "overlay", True), mnt(2, "/proc", "proc"), mnt(3, "/dev/pts", "devpts"),
                  mnt(4, "/sys", "sysfs", True), mnt(7, "/dev/mqueue", "mqueue", True),
                  tmpfs(10, "/t")]
        for listing in (q.kernel_memory_accounting(mounts), q.check_mounts(mounts, [])["kernel_memory"]):
            self.assertEqual(sorted(e["mount_id"] for e in listing), [1, 2, 3, 4, 7, 10])
            self.assertTrue(all(e["charged_to"] == "memory" for e in listing))
        art = full_artifact()
        leg = q.evaluate(art, now=NOW)["legs"]["charge_map"]
        self.assertEqual(len(leg["kernel_memory"]), len(art["mountinfo"]["mounts"]))

    def test_tmpfs_without_nr_inodes_or_size(self):
        mounts = [tmpfs(20, "/dev", sb="sb-dev", inodes=None), tmpfs(21, "/dev/shm", sb="sb-shm", inodes=None)]
        got = run([cl("tmpfs-size", "bytes", [], superblock="sb-dev"),
                   cl("tmpfs-size", "bytes", [], superblock="sb-shm")], mounts, {})
        self.assertIn("sink-inodes-uncovered", got["reasons"])
        got = run([], [tmpfs(22, "/x", sb="sb-x", size=None)], {})
        self.assertIn("sink-bytes-uncovered", got["reasons"])

    def test_writable_fd(self):
        mounts = [tmpfs(10, "/t"), mnt(1, "/", "overlay", True), mnt(13, "/vol", "ext4", False, volume=True)]
        self.assertEqual(q.check_mounts(mounts, [dict(mount_id=10, writable=True)])["reasons"].count(
            "unaccounted-writable-fd"), 0)
        for fd in (dict(mount_id=99, writable=True), dict(target_superblock="sb-none", writable=True),
                   dict(mount_id=13, writable=True)):
            with self.subTest(fd=fd):
                self.assertIn("unaccounted-writable-fd", q.check_mounts(mounts, [fd])["reasons"])
        by_sb = dict(target_superblock=mounts[0]["superblock"], writable=True)
        self.assertNotIn("unaccounted-writable-fd", q.check_mounts(mounts, [by_sb])["reasons"])

    def test_writable_fd_with_mount_id_matches_on_that_id_alone(self):
        mounts = [mnt(1, "/", "overlay", True, sb="sb-root")]
        fd = dict(writable=True, mount_id=999, target_superblock="sb-root")
        self.assertIn("unaccounted-writable-fd", q.check_mounts(mounts, [fd])["reasons"])
        # A writable fd whose own mount is read-only is unaccounted too.
        ro_fd = dict(writable=True, mount_id=1)
        self.assertIn("unaccounted-writable-fd", q.check_mounts(mounts, [ro_fd])["reasons"])
        # A good mount_id is not rescued or broken by a stray superblock hint.
        ok = [tmpfs(10, "/t", sb="sb-t"), mnt(1, "/", "overlay", True, sb="sb-root")]
        self.assertNotIn("unaccounted-writable-fd", q.check_mounts(
            ok, [dict(writable=True, mount_id=10)])["reasons"])
        self.assertNotIn("unaccounted-writable-fd", q.check_mounts(
            ok, [dict(writable=True, mount_id=10, target_superblock="sb-root")])["reasons"])

    def test_artifact_sink_cannot_be_pseudo(self):
        sinks = full_artifact()["charge_map"]["sinks"]
        sinks["work"]["pseudo"] = "work"
        self.assertIn("sink-pseudo-forbidden", q.check_sinks(sinks))
        art = full_artifact()
        art["charge_map"]["sinks"]["work"]["pseudo"] = "work"
        ce = q.collect(StubObserver(art))
        got = q.evaluate(art, collector=ce, now=NOW, head_revision="a"*40)["legs"]
        for name in ("charge_map", "enforcement", "aliasing"):
            with self.subTest(leg=name):
                self.assertEqual(got[name]["status"], "NEGATIVE")
                self.assertIn("sink-pseudo-forbidden", reasons(got[name]))
        for name in ("charge_map", "enforcement", "aliasing", "backing"):
            self.assertNotEqual(got[name]["status"], "PROVEN", name)

    def test_incremental_sink_needs_inodes_and_no_parent_cycle(self):
        sinks = full_artifact()["charge_map"]["sinks"]
        self.assertEqual(q.check_sinks(sinks), [])
        sinks["work"]["inodes"] = 0
        self.assertIn("sink-malformed", q.check_sinks(sinks))
        art = full_artifact()
        art["charge_map"]["sinks"]["work"]["inodes"] = 0
        self.assertIn("sink-malformed", reasons(q.evaluate(art, now=NOW)["legs"]["charge_map"]))
        sinks = full_artifact()["charge_map"]["sinks"]
        sinks["tmp"] = dict(identity="tmp", parent="temp_extraction")
        self.assertIn("sink-parent-cycle", q.check_sinks(sinks))
        sinks["tmp"] = dict(identity="tmp", parent="tmp2")
        sinks["tmp2"] = dict(identity="tmp2", parent="tmp")
        self.assertIn("sink-parent-cycle", q.check_sinks(sinks))

    def test_no_collector_mount_facts(self):
        art = full_artifact()
        del art["mountinfo"]
        leg = q.evaluate(art, now=NOW)["legs"]["charge_map"]
        self.assertEqual(leg["status"], "UNVERIFIED")
        self.assertIn("no-container-mount-facts", reasons(leg))
        art = full_artifact()
        del art["mountinfo"]["fds"]
        self.assertIn("no-container-mount-facts", reasons(q.evaluate(art, now=NOW)["legs"]["charge_map"]))


class AliasingTests(Base):
    def test_two_sink_tmpfs_overflow(self):
        mounts = [tmpfs(10, "/t", sb="sb-shared", size=4*MIB)]
        sinks = {"work": sink("work", "/t/w", 10, "sb-shared", b=3*MIB),
                 "output": sink("output", "/t/o", 10, "sb-shared", b=3*MIB)}
        self.assertIn("domain-overcommitted", q.check_aliasing(sinks, mounts))
        sinks["output"]["bytes"] = MIB
        self.assertEqual(q.check_aliasing(sinks, mounts), [])
        sinks["output"]["inodes"] = 95
        self.assertIn("domain-overcommitted", q.check_aliasing(sinks, mounts))

    def test_alias_split(self):
        mounts = [tmpfs(10, "/t", sb="sb-shared")]
        sinks = {"work": sink("work", "/t/w", 10, "sb-shared", dom="dom-A"),
                 "output": sink("output", "/t/o", 10, "sb-shared", dom="dom-B")}
        self.assertIn("domain-alias-split", q.check_aliasing(sinks, mounts))

    def test_mount_id_only_maps_paths_domain_is_superblock(self):
        mounts = [tmpfs(10, "/a", sb="sb-same", size=4*MIB), tmpfs(11, "/b", sb="sb-same", size=4*MIB)]
        sinks = {"work": sink("work", "/a/w", 10, "sb-same", b=3*MIB),
                 "output": sink("output", "/b/o", 11, "sb-same", b=3*MIB)}
        self.assertIn("domain-overcommitted", q.check_aliasing(sinks, mounts))

    def test_parent_and_child_charged_once(self):
        mounts = [tmpfs(10, "/t", sb="sb-shared", size=4*MIB)]
        sinks = {"tmp": sink("tmp", "/t", 10, "sb-shared", b=3*MIB),
                 "temp_extraction": dict(identity="temp_extraction", parent="tmp",
                                         bytes=3*MIB, inodes=10)}
        self.assertEqual(q.check_aliasing(sinks, mounts), [])

    def test_declared_superblock_is_checked_against_mountinfo(self):
        mounts = [tmpfs(10, "/t", sb="sb-shared", size=4*MIB)]
        real = {"/t/w": "/t/w", "/t/o": "/t/o"}
        def pair(**over):
            out = sink("output", "/t/o", None, "sb-bogus", b=3*MIB)
            out.update(over)
            return {"work": sink("work", "/t/w", 10, "sb-shared", b=3*MIB), "output": out}
        for realpaths in (real, None):
            with self.subTest(realpaths=realpaths):
                got = q.check_aliasing(pair(), mounts, realpaths)
                self.assertTrue({"sink-mapping-mismatch", "domain-overcommitted"} & set(got), got)
        # Declared superblock absent from mountinfo, mount_id naming a real mount, or unknown id.
        for over in (dict(mount_id=10), dict(mount_id=999)):
            with self.subTest(over=over):
                self.assertIn("sink-mapping-mismatch", q.check_aliasing(pair(**over), mounts, real))
        # Declared equal to what the path resolves to: no mismatch, and the sum still overcommits.
        got = q.check_aliasing(pair(superblock="sb-shared"), mounts, real)
        self.assertNotIn("sink-mapping-mismatch", got)
        self.assertIn("domain-overcommitted", got)
        # Nothing to resolve a container sink with: no mount_id and no resolvable path.
        self.assertIn("sink-mapping-mismatch",
                      q.check_aliasing(pair(superblock="sb-shared"), mounts, None))

    def test_outer_domain_sinks_are_exempt_only_when_flagged(self):
        mounts = [tmpfs(10, "/t", sb="sb-shared", size=4*MIB)]
        outer = dict(identity="daemon_image_store", paths=[], mount_id=None, bytes=3*MIB, inodes=10,
                     backing_domain="outer-A", outer_domain=True)
        sinks = {"work": sink("work", "/t/w", 10, "sb-shared", b=MIB), "daemon_image_store": outer}
        self.assertEqual(q.check_aliasing(sinks, mounts, {}), [])
        # The flag does not exempt an ordinary container sink.
        flagged = sink("output", "/t/o", None, "sb-bogus")
        flagged["outer_domain"] = True
        self.assertIn("sink-mapping-mismatch", q.check_aliasing(
            {"work": sinks["work"], "output": flagged}, mounts, {"/t/o": "/t/o"}))


class EnvelopeTests(Base):
    def test_envelope(self):
        good = full_artifact()["envelope"]
        self.assertEqual(q.check_envelope(good), [])
        drift = dict(good, limits=dict(p.LIMITS, aggregate=p.LIMITS["aggregate"]+1))
        self.assertIn("envelope-drift", q.check_envelope(drift))
        extra = dict(good, limits=dict(p.LIMITS, extra=1))
        self.assertIn("envelope-drift", q.check_envelope(extra))
        for ceiling in (None, dict(value=1000, owner_approved=False), dict(value=1000),
                        dict(value=1000, owner_approved="true")):
            value = dict(good, inode_ceiling=ceiling)
            with self.subTest(ceiling=ceiling):
                self.assertIn("inode-ceiling-unapproved", q.check_envelope(value))


class EnforcementTests(Base):
    def test_positive_controls_each_permitted_pairing(self):
        claims, mounts, sinks = tmpfs_env()
        got = run(claims, mounts, sinks)
        self.assertEqual((got["covered"], got["reasons"]), (True, []))
        # exclusive-finite-fs pair on a dedicated finite filesystem.
        ex = mnt(30, "/fin", "ext4", False, sb="sb-fin", exclusive=True,
                 capacity_bytes=8*MIB, capacity_inodes=1000)
        s = {"work": sink("work", "/fin/w", 30, "sb-fin")}
        pair = [cl("exclusive-finite-fs", "bytes", ["work"], superblock="sb-fin"),
                cl("exclusive-finite-fs", "inodes", ["work"], superblock="sb-fin")]
        self.assertEqual(run(pair, [ex], s)["reasons"], [])
        self.assertTrue(run(pair, [ex], s)["covered"])
        # readonly-rootfs covers write-deny for a sink resolving to the ro root.
        mounts, sinks, fact = ro_env()
        got = run([ro_claim(["work"])], mounts[:1], {"work": sinks["work"]}, fact)
        self.assertEqual((got["covered"], got["reasons"]), (True, []))
        # process tree, container logs, outer memory domain.
        dom = with_domains({})
        got = run([cl("pids-max", "pids", ["process_tree"]), cl("memcg-max-eq-swap", "memory-swap", ["memory"])],
                  [], dom)
        self.assertEqual((got["covered"], got["reasons"]), (True, []))
        got = run([cl("log-driver-none", "logs", ["container_logs"])], [],
                  {"container_logs": sink("container_logs", "", None, "x")})
        self.assertEqual((got["covered"], got["reasons"]), (True, []))

    def test_tmpfs_resource_mismatch(self):
        claims, mounts, sinks = tmpfs_env()
        bad = [cl("tmpfs-size", "inodes", ["work"], superblock="sb-work")]
        self.assertIn("boundary-resource-mismatch", run(bad+claims[1:], mounts, sinks)["reasons"])
        bad = [cl("tmpfs-nr_inodes", "bytes", ["work"], superblock="sb-work")]
        self.assertIn("boundary-resource-mismatch", run(bad+[claims[0]], mounts, sinks)["reasons"])
        got = run([claims[0], cl("tmpfs-nr_inodes", "bytes", ["work"], superblock="sb-work")], mounts, sinks)
        self.assertIn("sink-inodes-uncovered", got["reasons"])

    def test_tmpfs_other_superblock(self):
        claims, mounts, sinks = tmpfs_env()
        wrong = [cl("tmpfs-size", "bytes", ["work"], superblock="sb-output"),
                 cl("tmpfs-nr_inodes", "inodes", ["work"], superblock="sb-output")]
        got = run(wrong, mounts, sinks)
        self.assertIn("boundary-sink-out-of-scope", got["reasons"])
        self.assertIn("sink-bytes-uncovered", got["reasons"])

    def test_tmpfs_claim_without_option_observed(self):
        claims, mounts, sinks = tmpfs_env()
        mounts[1]["options"].pop("size")
        got = run(claims, mounts, sinks)
        self.assertIn("sink-bytes-uncovered", got["reasons"])
        self.assertIn("work", got["uncovered"]["bytes"])
        mounts[2]["options"].pop("nr_inodes")
        self.assertIn("sink-inodes-uncovered", run(claims, mounts, sinks)["reasons"])

    def test_exclusive_finite_fs_requirements(self):
        def env(**over):
            ex = mnt(30, "/fin", "ext4", False, sb="sb-fin", exclusive=True,
                     capacity_bytes=8*MIB, capacity_inodes=1000)
            ex.update(over)
            return [ex], {"work": sink("work", "/fin/w", 30, "sb-fin")}
        pair = [cl("exclusive-finite-fs", "bytes", ["work"], superblock="sb-fin"),
                cl("exclusive-finite-fs", "inodes", ["work"], superblock="sb-fin")]
        for over in (dict(capacity_bytes=None), dict(capacity_inodes=None), dict(exclusive=False)):
            with self.subTest(over=over):
                mounts, sinks = env(**over)
                got = run(pair, mounts, sinks)
                self.assertFalse(got["covered"])
                self.assertTrue({"sink-bytes-uncovered", "sink-inodes-uncovered"} & set(got["reasons"]))
        mounts, sinks = env()
        other = [dict(c, superblock="sb-other") for c in pair]
        self.assertIn("boundary-sink-out-of-scope", run(other, mounts, sinks)["reasons"])
        file_facts = facts(tier=dict(mountinfo="file", runtime="collector", daemon="collector"))
        got = run(pair, mounts, sinks, file_facts)
        self.assertIn("claim-evidence-not-collector", got["reasons"])
        self.assertFalse(got["covered"])
        for single, missing in ((pair[:1], "sink-inodes-uncovered"), (pair[1:], "sink-bytes-uncovered")):
            with self.subTest(missing=missing):
                self.assertIn(missing, run(single, mounts, sinks)["reasons"])
        for resource in ("write-deny", "pids"):
            bad = [cl("exclusive-finite-fs", resource, ["work"], superblock="sb-fin")]
            self.assertIn("boundary-resource-mismatch", run(bad, mounts, sinks)["reasons"])

    def test_bytes_only_exclusive_claim_leaves_mount_uncovered_for_inodes(self):
        ex = mnt(30, "/fin", "ext4", False, sb="sb-fin", exclusive=True,
                 capacity_bytes=8*MIB, capacity_inodes=1000)
        got = run([cl("exclusive-finite-fs", "bytes", [], superblock="sb-fin")], [ex], {})
        self.assertIn("sink-inodes-uncovered", got["reasons"])
        self.assertEqual(got["uncovered"]["mounts"]["inodes"], [30])
        self.assertEqual(got["uncovered"]["mounts"]["bytes"], [30])
        got = run([cl("exclusive-finite-fs", "inodes", [], superblock="sb-fin")], [ex], {})
        self.assertEqual(got["uncovered"]["mounts"]["bytes"], [30])

    def test_readonly_rootfs_resource_mismatch(self):
        mounts, sinks, fact = ro_env()
        for resource in ("bytes", "inodes"):
            with self.subTest(resource=resource):
                got = run([cl("readonly-rootfs", resource, ["work"])], mounts, sinks, fact)
                self.assertIn("boundary-resource-mismatch", got["reasons"])
                self.assertFalse(got["covered"])

    def test_readonly_rootfs_scope(self):
        mounts, sinks, fact = ro_env()
        cases = {"tmpfs": "tmp", "shm": "shm", "volumes": "volumes",
                 "network bind": "container_network_binds", "other bind": "gotmp",
                 "symlink to tmpfs": "home", "symlink to volume": "cache", "symlink to bind": "logs"}
        for label, name in cases.items():
            with self.subTest(label=label):
                got = run([ro_claim([name])], mounts, sinks, fact)
                self.assertIn("boundary-sink-out-of-scope", got["reasons"])
                self.assertIn(name, got["uncovered"]["bytes"])
                self.assertIn(name, got["uncovered"]["inodes"])

    def test_readonly_rootfs_evidence_and_removability(self):
        mounts, sinks, fact = ro_env()
        file_facts = dict(fact, tier=dict(mountinfo="file", runtime="collector", daemon="collector"))
        got = run([ro_claim(["work"])], mounts, sinks, file_facts)
        self.assertIn("claim-evidence-not-collector", got["reasons"])
        self.assertIn("work", got["uncovered"]["bytes"])
        priv = dict(fact, container=dict(facts()["container"], privileged=True))
        self.assertIn("boundary-removable", run([ro_claim(["work"])], mounts, sinks, priv)["reasons"])
        sysadmin = dict(fact, container=dict(facts()["container"], caps=["CAP_SYS_ADMIN"]))
        self.assertIn("boundary-removable", run([ro_claim(["work"])], mounts, sinks, sysadmin)["reasons"])
        # A writable root is not read-only.
        mounts[0]["ro"] = False
        self.assertIn("work", run([ro_claim(["work"])], mounts, sinks, fact)["uncovered"]["bytes"])

    def test_new_mount_possible(self):
        mounts, sinks, fact = ro_env()
        def nm(**over):
            return dict(fact, new_mount=dict(fact["new_mount"], **over))
        broken = [nm(cap_bnd=1), nm(no_new_privs=0), nm(seccomp=0), nm(seccomp_profile="custom"),
                  dict(fact, new_mount=None), dict(fact, new_mount={})]
        for f in broken:
            with self.subTest(new_mount=f["new_mount"]):
                got = run([ro_claim(["work"])], mounts, sinks, f)
                self.assertIn("new-mount-possible", got["reasons"])
                self.assertFalse(got["covered"])
        self.assertNotIn("new-mount-possible", run([ro_claim(["work"])], mounts, sinks,
                         nm(seccomp=0, max_user_namespaces=0))["reasons"])
        claims, tm, ts = tmpfs_env()
        self.assertIn("new-mount-possible", run(claims, tm, ts, nm(cap_bnd=1))["reasons"])

    def test_pids_mismatch_and_scope(self):
        sinks = with_domains({"work": sink("work", "/t/w", 10, "sb-work")})
        for resource in ("bytes", "inodes"):
            self.assertIn("boundary-resource-mismatch",
                          run([cl("pids-max", resource, ["process_tree"])], [], sinks)["reasons"])
        for scope in (["work"], ["memory"], []):
            with self.subTest(scope=scope):
                self.assertIn("boundary-sink-out-of-scope",
                              run([cl("pids-max", "pids", scope)], [], sinks)["reasons"])

    def test_log_driver_none(self):
        sinks = with_domains({"container_logs": sink("container_logs", "", None, "x"),
                              "stdout_stderr": sink("stdout_stderr", "/o", 1, "sb-root"),
                              "harness_reports": sink("harness_reports", "/h", 1, "sb-root")})
        for resource in ("bytes", "inodes"):
            self.assertIn("boundary-resource-mismatch",
                          run([cl("log-driver-none", resource, ["container_logs"])], [], sinks)["reasons"])
        for scope in (["stdout_stderr"], ["harness_reports"]):
            self.assertIn("boundary-sink-out-of-scope",
                          run([cl("log-driver-none", "logs", scope)], [], sinks)["reasons"])

    def test_memcg(self):
        sinks = with_domains({"work": sink("work", "/t/w", 10, "sb-work")})
        for resource in ("bytes", "inodes"):
            self.assertIn("boundary-resource-mismatch",
                          run([cl("memcg-max-eq-swap", resource, ["memory"])], [], sinks)["reasons"])
        self.assertIn("boundary-sink-out-of-scope",
                      run([cl("memcg-max-eq-swap", "memory-swap", ["work"])], [], sinks)["reasons"])
        self.assertIn("boundary-sink-out-of-scope",
                      run([cl("memcg-max-eq-swap", "memory-swap", ["memory"], mount_id=7)], [], sinks)["reasons"])

    def test_cgroup_removable(self):
        sinks = with_domains({})
        for container in (dict(cgroupfs_writable=True), dict(cgroupns=False)):
            f = facts(container=dict(facts()["container"], **container))
            for claim in (cl("pids-max", "pids", ["process_tree"]),
                          cl("memcg-max-eq-swap", "memory-swap", ["memory"])):
                self.assertIn("boundary-removable", run([claim], [], sinks, f)["reasons"])

    def test_daemon_capability_and_applied_limits(self):
        sinks = with_domains({})
        claims = [cl("pids-max", "pids", ["process_tree"]), cl("memcg-max-eq-swap", "memory-swap", ["memory"])]
        base = facts()["daemon"]
        for change in (dict(MemoryLimit=False), dict(SwapLimit=False), dict(PidsLimit=False),
                       dict(rootless=True, CgroupDriver="cgroupfs")):
            with self.subTest(change=change):
                f = facts(daemon=dict(base, **change))
                self.assertIn("daemon-capability-unsupported", run(claims, [], sinks, f)["reasons"])
        # Rootless with the systemd driver is not refused on this ground alone.
        f = facts(daemon=dict(base, rootless=True))
        self.assertNotIn("daemon-capability-unsupported", run(claims, [], sinks, f)["reasons"])
        # Daemon support facts without observed applied limits cover nothing.
        for applied in ({}, dict(pids_max=100), dict(memory_max=GIB, memory_swap_max=2*GIB)):
            with self.subTest(applied=applied):
                got = run(claims, [], sinks, facts(applied=applied))
                self.assertIn("limits-not-applied-observed", got["reasons"])
                self.assertFalse(got["covered"])
        got = run([cl("log-driver-none", "logs", ["container_logs"])], [],
                  {"container_logs": sink("container_logs", "", None, "x")},
                  facts(applied=dict(log_driver="json-file")))
        self.assertIn("limits-not-applied-observed", got["reasons"])

    def test_refused_kinds(self):
        sinks = {"work": sink("work", "/t/w", 10, "sb-work")}
        kinds = ("emptydir-sizelimit", "ephemeral-storage-limit", "project-quota-monitoring", "pvc-request",
                 "fs-capacity", "watchdog", "sampled-du-statfs", "broker-counter", "eviction",
                 "cooperative-writer-cap", "json-file-max-size", "rlimit-fsize", "cacheprog-object-count",
                 "storage-opt-size")
        self.assertTrue(set(kinds) <= set(q.REFUSED_KINDS))
        for kind in kinds:
            for resource in ("bytes", "inodes"):
                with self.subTest(kind=kind, resource=resource):
                    got = run([cl(kind, resource, ["work"])], [], sinks)
                    self.assertIn("boundary-not-allocation-enforcement", got["reasons"])
                    self.assertIn("work", got["uncovered"]["bytes"])

    def test_storage_opt_size_needs_backend_support_evidence(self):
        sinks = {"container_writable_layer": sink("container_writable_layer", "/", 1, "sb-root")}
        claim = cl("storage-opt-size", "bytes", ["container_writable_layer"])
        self.assertIn("boundary-not-allocation-enforcement", run([claim], [], sinks)["reasons"])
        got = run([claim], [], sinks, facts(backend_support=True))
        self.assertNotIn("boundary-not-allocation-enforcement", got["reasons"])
        self.assertIn("sink-inodes-uncovered", got["reasons"])
        self.assertNotIn("container_writable_layer", got["uncovered"]["bytes"])

    def test_resource_missing_unknown_and_wrong_pairing(self):
        claims, mounts, sinks = tmpfs_env()
        missing = dict(kind="tmpfs-size", sinks=["work"], superblock="sb-work")
        self.assertIn("boundary-resource-missing", run([missing], mounts, sinks)["reasons"])
        unknown = cl("tmpfs-size", "iops", ["work"], superblock="sb-work")
        self.assertIn("boundary-resource-unknown", run([unknown], mounts, sinks)["reasons"])
        self.assertIn("boundary-resource-mismatch",
                      run([cl("tmpfs-size", "write-deny", ["work"], superblock="sb-work")], mounts, sinks)["reasons"])

    def test_dac_covers_nothing(self):
        claims, mounts, sinks = tmpfs_env()
        text = ("ownership checks cannot establish zero allocation: CHOWN/SETUID-class capabilities "
                "may remain and pre-existing writable fds are unaccounted")
        dac = [cl("dac-deny-nonroot", "write-deny", ["work"]), cl("dac-deny-nonroot", "bytes", ["work"]),
               cl("dac-deny-nonroot", "inodes", ["work"]),
               cl("mode-bits", "write-deny", ["work"], mode="0555"),
               cl("readonly-rootfs", "write-deny", ["work"], non_root_user=True),
               cl("tmpfs-size", "bytes", ["work"], superblock="sb-work", owner="root")]
        for claim in dac:
            with self.subTest(claim=claim["kind"], resource=claim["resource"]):
                got = run([claim], mounts, sinks)
                self.assertIn("dac-ownership-mode-unverified", got["reasons"])
                self.assertEqual(got["notes"]["dac-ownership-mode-unverified"], text)
                self.assertIn("work", got["uncovered"]["bytes"])
        self.assertTrue(all(c["kind"] != "dac-deny-nonroot" for c in claims))

    def test_dac_does_not_cover_a_sink_valid_claim_elsewhere(self):
        mounts, sinks, fact = ro_env()
        sinks = {k: sinks[k] for k in ("work", "tmp")}
        got = run([ro_claim(["work"]), cl("dac-deny-nonroot", "write-deny", ["tmp"])], mounts, sinks, fact)
        self.assertIn("sink-bytes-uncovered", got["reasons"])
        self.assertIn("sink-inodes-uncovered", got["reasons"])
        self.assertEqual(got["uncovered"]["bytes"], ["tmp"])
        self.assertEqual(got["coverage"]["work"], dict(bytes=True, inodes=True))

    def test_process_tree_logs_memory_need_their_claims(self):
        sinks = with_domains({"container_logs": sink("container_logs", "", None, "x")})
        got = run([], [], sinks)
        for code in ("sink-pids-uncovered", "sink-logs-uncovered", "sink-memory-swap-uncovered"):
            self.assertIn(code, got["reasons"])

    def test_daemon_info_missing(self):
        got = run([cl("pids-max", "pids", ["process_tree"])], [], with_domains({}), facts(daemon=None))
        self.assertIn("daemon-info-missing", got["reasons"])


class CanaryTests(Base):
    def one(self, **over):
        item = dict(sink="work", kind="readonly-rootfs", errno="EROFS", ro_flag=True, killed=False)
        item.update(over)
        return q.classify_canary(dict(sinks=[item]))

    def test_readonly_rootfs_needs_erofs_and_flag(self):
        self.assertTrue(self.one()["ok"])
        self.assertIn("write-deny-not-erofs", self.one(errno="EACCES")["reasons"])
        self.assertIn("write-deny-not-erofs", self.one(errno="EPERM")["reasons"])
        self.assertIn("write-deny-not-erofs", self.one(ro_flag=False)["reasons"])

    def test_tmpfs_enospc_and_edquot(self):
        self.assertTrue(self.one(kind="tmpfs-size", errno="ENOSPC", ro_flag=None)["ok"])
        got = self.one(kind="tmpfs-size", errno="EDQUOT", ro_flag=None)
        self.assertFalse(got["ok"])
        self.assertIn("canary-errno-unexpected", got["reasons"])

    def test_kill_only(self):
        got = self.one(kind="tmpfs-size", errno=None, killed=True)
        self.assertIn("canary-kill-only", got["reasons"])
        self.assertFalse(got["ok"])

    def test_not_implemented_in_evaluate(self):
        art = full_artifact()
        ce = q.collect(StubObserver(art))
        got = q.evaluate(art, collector=ce, now=NOW, head_revision="a"*40)
        self.assertIn("canary-collector-not-implemented", reasons(got["legs"]["canary"]))
        self.assertFalse(got["qualified"])

    def test_canary_attribute_never_changes_the_leg(self):
        for names in (COLLECTOR_SECTIONS, ALL_SECTIONS):
            art = full_artifact()
            ce = q.collect(StubObserver(art, names))
            try:
                ce.canary = {"sinks": []}
            except AttributeError:
                pass
            got = q.evaluate(art, collector=ce, now=NOW, head_revision="a"*40)
            with self.subTest(names=len(names)):
                self.assertEqual(got["legs"]["canary"]["status"], "UNVERIFIED")
                self.assertEqual(got["legs"]["canary"]["reason"], "canary-collector-not-implemented")
                self.assertFalse(got["qualified"])


class BackingTests(Base):
    def link(self, **over):
        observed = dict(capacity_bytes=100*MIB, capacity_inodes=10000, used_bytes=0, used_inodes=0,
                        concurrent_bytes=0, concurrent_inodes=0, sampled_at=NOW-1)
        observed.update(over)
        return {"D": dict(observed=observed)}

    def charges(self, *sizes, dom="D"):
        return {"c%d" % n: dict(bytes=s, inodes=10, domain=dom) for n, s in enumerate(sizes)}

    def check(self, charges, domains, soft=0.8, now=NOW):
        return q.backing_check(charges, domains, soft, now)

    def test_safe_exhaustion_of_sublimit_within_outer_headroom_passes(self):
        self.assertEqual(self.check(self.charges(10*MIB), self.link(used_bytes=50*MIB)), [])
        self.assertEqual(self.check(self.charges(30*MIB, 20*MIB), self.link(used_bytes=20*MIB)), [])

    def test_shared_domain_exhaustion_refused(self):
        got = self.check(self.charges(30*MIB, 30*MIB), self.link(used_bytes=30*MIB))
        self.assertIn("backing-aggregate-exceeds-soft", got)

    def test_sublimit_fits_but_aggregate_exceeds(self):
        for size in (25*MIB, 25*MIB):
            self.assertEqual(self.check(self.charges(size), self.link(used_bytes=40*MIB)), [])
        got = self.check(self.charges(25*MIB, 25*MIB), self.link(used_bytes=40*MIB))
        self.assertIn("backing-aggregate-exceeds-soft", got)

    def test_exact_boundary_is_refused(self):
        self.assertIn("backing-aggregate-exceeds-soft",
                      self.check(self.charges(30*MIB), self.link(used_bytes=50*MIB)))
        self.assertEqual(self.check(self.charges(30*MIB-1), self.link(used_bytes=50*MIB)), [])

    def test_inode_aggregate(self):
        self.assertIn("backing-aggregate-exceeds-soft",
                      self.check(self.charges(MIB), self.link(used_inodes=7995)))

    def test_insufficient_headroom(self):
        self.assertIn("backing-aggregate-exceeds-soft",
                      self.check(self.charges(2*MIB), self.link(used_bytes=79*MIB)))

    def test_unknown_mapping(self):
        self.assertIn("backing-mapping-unknown", self.check(self.charges(MIB, dom="X"), self.link()))

    def test_unknown_concurrency(self):
        for key in ("concurrent_bytes", "concurrent_inodes"):
            with self.subTest(key=key):
                self.assertIn("backing-concurrency-unknown", self.check(self.charges(MIB), self.link(**{key: None})))
        domains = self.link()
        del domains["D"]["observed"]["concurrent_bytes"]
        self.assertIn("backing-concurrency-unknown", self.check(self.charges(MIB), domains))

    def test_concurrent_use_counts(self):
        self.assertIn("backing-aggregate-exceeds-soft",
                      self.check(self.charges(10*MIB), self.link(used_bytes=40*MIB, concurrent_bytes=40*MIB)))

    def test_cited_only(self):
        cited = {"D": dict(cited=dict(capacity_bytes=100*MIB))}
        self.assertIn("backing-link-cited-only", self.check(self.charges(MIB), cited))

    def test_capacity_unknown_and_stale(self):
        for key in ("capacity_bytes", "capacity_inodes"):
            self.assertIn("backing-capacity-unknown", self.check(self.charges(MIB), self.link(**{key: None})))
        self.assertIn("backing-capacity-unknown", self.check(self.charges(MIB), self.link(capacity_bytes=0)))
        self.assertIn("backing-sample-stale", self.check(self.charges(MIB), self.link(sampled_at=NOW-61)))
        self.assertIn("backing-sample-stale", self.check(self.charges(MIB), self.link(sampled_at=NOW+5)))

    def test_project_domains_is_shared_with_headroom(self):
        charges = {"a": dict(bytes=3, inodes=2, domain="D"), "b": dict(bytes=4, inodes=5, domain="D"),
                   "c": dict(bytes=1, inodes=1, domain="E")}
        def sample():
            return SimpleNamespace(f_blocks=1, f_bavail=1, f_frsize=1, f_files=1, f_favail=1)
        samples = {"D": sample(), "E": sample()}
        got = p.project_domains(charges, samples)
        self.assertEqual(got, {"D": (samples["D"], 7, 7), "E": (samples["E"], 1, 1)})
        with self.assertRaises(ValueError):
            p.project_domains({"a": dict(bytes=1, inodes=1, domain="Z")}, samples)


class EvaluateTests(Base):
    LEGS = ("binding", "threshold", "charge_map", "enforcement", "aliasing", "envelope", "backing", "canary")

    def test_file_artifact_is_fixture_consistent_never_proven_or_qualified(self):
        art = full_artifact()
        # Labels inside the file claim collector provenance; the file path ignores them.
        art["claims_collector"] = True
        got = q.evaluate(art, now=NOW, head_revision="a"*40)
        self.assertFalse(got["qualified"])
        self.assertEqual(got["verdict"], "NO-QUALIFIED-ENVIRONMENT")
        self.assertIs(got["producer_launched"], False)
        self.assertIs(got["production_authorized"], False)
        for name in self.LEGS[:7]:
            with self.subTest(leg=name):
                self.assertEqual(got["legs"][name]["status"], "FIXTURE-CONSISTENT", got["legs"][name])
        self.assertNotIn("PROVEN", [leg["status"] for leg in got["legs"].values()])
        self.assertIn("canary-collector-not-implemented", reasons(got["legs"]["canary"]))

    def test_legs_are_proven_only_when_the_collector_supplied_every_input(self):
        art = full_artifact()
        ce = q.collect(StubObserver(art))
        stripped = {k: v for k, v in art.items() if k not in COLLECTOR_SECTIONS}
        got = q.evaluate(stripped, collector=ce, now=NOW, head_revision="a"*40)
        for name in ("binding", "threshold"):
            self.assertEqual(got["legs"][name]["status"], "PROVEN", got["legs"][name])
        # The charge map and the claims came from the file, so the legs that use them stay capped.
        for name in ("charge_map", "enforcement", "aliasing", "backing"):
            with self.subTest(leg=name):
                self.assertEqual(got["legs"][name]["status"], "FIXTURE-CONSISTENT", got["legs"][name])
        self.assertIn("file", got["legs"]["enforcement"]["tier"])

    def test_collector_path_proves_covered_legs_but_never_qualifies(self):
        art = full_artifact()
        ce = q.collect(StubObserver(art, ALL_SECTIONS))
        stripped = {k: v for k, v in art.items() if k not in ALL_SECTIONS}
        got = q.evaluate(stripped, collector=ce, now=NOW, head_revision="a"*40)
        for name in ("binding", "threshold", "charge_map", "enforcement", "aliasing", "backing"):
            with self.subTest(leg=name):
                self.assertEqual(got["legs"][name]["status"], "PROVEN", got["legs"][name])
        self.assertEqual(got["legs"]["envelope"]["status"], "FIXTURE-CONSISTENT")
        self.assertFalse(got["qualified"])
        self.assertEqual(got["verdict"], "NO-QUALIFIED-ENVIRONMENT")
        self.assertIn("canary-collector-not-implemented", reasons(got["legs"]["canary"]))
        self.assertEqual(len(got["collector"]["daemon_id_hash"]), 64)

    def test_backing_needs_the_collector_to_supply_the_threshold(self):
        for names in (tuple(n for n in COLLECTOR_SECTIONS if n != "threshold"),
                      tuple(n for n in ALL_SECTIONS if n != "threshold")):
            art = full_artifact()
            ce = q.collect(StubObserver(art, names))
            got = q.evaluate(art, collector=ce, now=NOW, head_revision="a"*40)["legs"]
            with self.subTest(names=len(names)):
                self.assertEqual(got["threshold"]["status"], "FIXTURE-CONSISTENT")
                self.assertNotEqual(got["backing"]["status"], "PROVEN", got["backing"])
                self.assertIn("file", got["backing"]["tier"])

    def test_collector_evidence_cannot_be_forged(self):
        art = full_artifact()
        with self.assertRaises(ValueError):
            q.CollectorEvidence(object(), {}, b"salt", "id")
        with self.assertRaises(ValueError):
            q.evaluate(art, collector=dict(sections={}), now=NOW)
        art["collector"] = dict(sections=art["mountinfo"])
        got = q.evaluate(art, now=NOW, head_revision="a"*40)
        self.assertNotIn("PROVEN", [leg["status"] for leg in got["legs"].values()])

    def test_daemon_id_hash_is_salted(self):
        art = full_artifact()
        one, two = q.collect(StubObserver(art)), q.collect(StubObserver(art))
        self.assertNotEqual(one.daemon_id_hash, two.daemon_id_hash)
        self.assertNotEqual(one.daemon_id_hash, hashlib.sha256(b"fixture-daemon-id").hexdigest())

    def test_collector_sections_override_file_values(self):
        art = full_artifact()
        ce = q.collect(StubObserver(art, ALL_SECTIONS))
        art["mountinfo"]["mounts"].append(mnt(99, "/dev/mqueue", "mqueue", False))
        got = q.evaluate(art, collector=ce, now=NOW, head_revision="a"*40)
        self.assertEqual(got["legs"]["charge_map"]["status"], "PROVEN")

    def test_defects_are_reported_whatever_the_tier(self):
        art = full_artifact()
        art["mountinfo"]["mounts"].append(mnt(99, "/dev/mqueue", "mqueue", False))
        art["claims"] = [c for c in art["claims"] if c["kind"] != "pids-max"]
        art["envelope"]["limits"]["aggregate"] += 1
        got = q.evaluate(art, now=NOW, head_revision="a"*40)["legs"]
        self.assertEqual(got["charge_map"]["status"], "NEGATIVE")
        self.assertIn("unaccounted-writable-mount", reasons(got["charge_map"]))
        self.assertIn("sink-pids-uncovered", reasons(got["enforcement"]))
        self.assertIn("envelope-drift", reasons(got["envelope"]))

    def test_schema_mismatch_is_unverified(self):
        got = q.evaluate(dict(schema="other/2"), now=NOW)
        self.assertEqual({leg["status"] for leg in got["legs"].values()}, {"UNVERIFIED"})
        self.assertFalse(got["qualified"])

    def test_malformed_sections_do_not_crash(self):
        art = full_artifact()
        art["charge_map"] = []
        art["claims"] = "x"
        got = q.evaluate(art, now=NOW)
        self.assertFalse(got["qualified"])
        self.assertIn("malformed-section", reasons(got["legs"]["charge_map"]))
        self.assertIn("malformed-section", reasons(got["legs"]["enforcement"]))
        self.assertEqual(got["legs"]["charge_map"]["status"], "UNVERIFIED")


class CliTests(Base):
    def call(self, argv):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = q.main(argv)
        return code, json.loads(out.getvalue())

    def artifact(self):
        path = self.root/"artifact.json"
        path.write_text(json.dumps(full_artifact()))
        return str(path)

    def test_exit_codes_and_experimental(self):
        code, verdict = self.call(["--artifact", self.artifact()])
        self.assertEqual(code, 2)
        self.assertFalse(verdict["qualified"])
        code, verdict = self.call(["--artifact", self.artifact(), "--experimental"])
        self.assertEqual(code, 0)
        self.assertFalse(verdict["qualified"])
        self.assertEqual(verdict["verdict"], "NO-QUALIFIED-ENVIRONMENT")
        self.assertIs(verdict["producer_launched"], False)
        self.assertIs(verdict["production_authorized"], False)

    def test_head_revision_flag_detects_a_stale_repo_revision(self):
        code, verdict = self.call(["--artifact", self.artifact(), "--experimental"])
        self.assertEqual(verdict["legs"]["binding"]["status"], "FIXTURE-CONSISTENT")
        code, verdict = self.call(["--artifact", self.artifact(), "--experimental",
                                   "--head-revision", "a"*40])
        self.assertEqual(verdict["legs"]["binding"]["status"], "FIXTURE-CONSISTENT")
        code, verdict = self.call(["--artifact", self.artifact(), "--experimental",
                                   "--head-revision", "c"*40])
        self.assertIn("binding-source-unbound", reasons(verdict["legs"]["binding"]))
        for bad in ("c"*39, "C"*40, "zz"):
            with self.subTest(bad=bad), contextlib.redirect_stderr(io.StringIO()), \
                    self.assertRaises(SystemExit):
                q.main(["--artifact", self.artifact(), "--head-revision", bad])

    def test_unreadable_artifact(self):
        for argv in (["--artifact", "/tmp/outside-scratch.json"], ["--artifact", str(self.root/"missing.json")]):
            code, verdict = self.call(argv)
            self.assertEqual(code, 2)
            self.assertFalse(verdict["qualified"])
            self.assertEqual({leg["status"] for leg in verdict["legs"].values()}, {"UNVERIFIED"})

    def test_duplicate_keys_rejected(self):
        path = self.root/"dup.json"
        path.write_text('{"schema":"uzi-go-cache-env/1","schema":"x"}')
        code, verdict = self.call(["--artifact", str(path)])
        self.assertEqual(code, 2)
        self.assertEqual({leg["status"] for leg in verdict["legs"].values()}, {"UNVERIFIED"})


class LoopbackDockerTests(Base):
    def serve(self, responses, count):
        server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.addCleanup(server.close)
        server.settimeout(3)
        server.bind(("127.0.0.1", 0))
        server.listen(count)
        seen, errors = [], []
        def work():
            try:
                for _ in range(count):
                    with server.accept()[0] as conn:
                        conn.settimeout(2)
                        data = b""
                        while b"\r\n\r\n" not in data and len(data) < 8192:
                            chunk = conn.recv(1024)
                            if not chunk:
                                break
                            data += chunk
                        seen.append(data)
                        route = data.split(b" ")[1].decode()
                        conn.sendall(responses[route])
            except Exception as error:
                errors.append(error)
        thread = threading.Thread(target=work)
        thread.start()
        def finish():
            thread.join(5)
            self.assertFalse(thread.is_alive())
            self.assertEqual(errors, [])
        return "tcp://127.0.0.1:%d" % server.getsockname()[1], seen, finish

    @staticmethod
    def ok(body):
        raw = json.dumps(body).encode()
        return b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % len(raw) + raw

    def test_routes(self):
        image = "/images/sha256:" + "a"*64 + "/json"
        responses = {"/version": self.ok({"Version": "1"}), "/info": self.ok({"Rootless": False}),
                     "/images/json": self.ok([]), image: self.ok({"Id": "x"})}
        endpoint, seen, finish = self.serve(responses, 4)
        with patch.dict(os.environ, {"DOCKER_HOST": "tcp://10.0.0.1:1"}):
            for route, want in (("/version", {"Version": "1"}), ("/info", {"Rootless": False}),
                                ("/images/json", []), (image, {"Id": "x"})):
                self.assertEqual(p.docker_get(endpoint, route), want)
        finish()
        self.assertTrue(all(r.startswith(b"GET ") and b"Host: localhost\r\n" in r for r in seen))

    def test_redirect_and_non_200_refused(self):
        responses = {"/version": b"HTTP/1.1 302 Found\r\nLocation: http://10.0.0.1/\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
                     "/info": b"HTTP/1.1 500 Err\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"}
        endpoint, seen, finish = self.serve(responses, 2)
        for route in ("/version", "/info"):
            with self.assertRaises(ValueError):
                p.docker_get(endpoint, route)
        finish()
        self.assertEqual(len(seen), 2)

    def test_refused_endpoints_and_routes_never_connect(self):
        with patch.object(socket.socket, "connect", side_effect=AssertionError("connected")):
            for endpoint in ("tcp://localhost:2375", "tcp://10.0.0.1:2375", "tcp://127.0.0.2:2375",
                             "tcp://127.0.0.1:0", "tcp://127.0.0.1:65536", "tcp://127.0.0.1",
                             "tcp://[::2]:2375", "tcp://[::1]", "http://127.0.0.1:2375",
                             "tcp://127.0.0.1:2375/", "tcp://user@127.0.0.1:2375", "tcp://0x7f.1:2375"):
                with self.subTest(endpoint=endpoint), self.assertRaises(ValueError):
                    p.docker_get(endpoint, "/version")
            for route in ("/containers/json", "/images/json?all=1", "/version/", "/images/sha256:abc/json",
                          "/info "):
                with self.subTest(route=route), self.assertRaises(ValueError):
                    p.docker_get("tcp://127.0.0.1:2375", route)


if __name__ == "__main__":
    unittest.main()
