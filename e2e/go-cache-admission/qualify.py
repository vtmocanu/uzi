"""Issue 4575 M1: pure prerequisite verifier. It qualifies nothing and launches nothing.

Reads one bounded "uzi-go-cache-env/1" artifact (or an in-process collector result) and
reports, per leg, PROVEN / NEGATIVE / UNVERIFIED / FIXTURE-CONSISTENT with a named reason.
Values that came from the artifact file reach at most FIXTURE-CONSISTENT; PROVEN needs
evidence built in this invocation by the collector code path (collect()). The only collector,
observe.py, supplies the daemon and binding sections and no mount, fd or runtime facts, and the
canary leg has no producer, so `qualified` is always false.

Outer sinks (daemon_image_store, daemon_build_cache, daemon_metadata_snapshots, container_logs) live
outside every container superblock, so a collector must not record realpaths for their paths: a
recorded realpath resolves under "/" and makes the sink sink-malformed.
"""
import argparse
import hashlib
import json
import math
import os
import re
import time
from pathlib import Path
from types import SimpleNamespace

import preflight as p
from preflight import require

SCHEMA = "uzi-go-cache-env/1"
OUTPUT_SCHEMA = "uzi-go-cache-qualify/1"
GOLDEN_PATH = Path(__file__).with_name("margin-golden.json")
CONFIG_TS = p.ROOT / "agent/src/config.ts"
FALLBACK_MARGIN = 0.1

OUTER_SINKS = ("daemon_image_store", "daemon_build_cache", "daemon_metadata_snapshots")
LOG_SINKS = ("container_logs",)
OUTER_EXEMPT = OUTER_SINKS + LOG_SINKS
CHARGE_SINKS = p.SINKS + (
    "toolchain_download", "cache_init", "fuzz", "exported_archives", "linked_executables",
    "stdout_stderr", "harness_reports", "setup_cleanup", "container_writable_layer",
    "container_network_binds", "shm", "daemon_image_store", "daemon_build_cache",
    "daemon_metadata_snapshots", "volumes", "container_logs", "temp_extraction")

RESOURCES = ("bytes", "inodes", "write-deny", "memory-swap", "pids", "logs")
KIND_RESOURCE = {
    "tmpfs-size": ("bytes",), "tmpfs-nr_inodes": ("inodes",),
    "exclusive-finite-fs": ("bytes", "inodes"), "readonly-rootfs": ("write-deny",),
    "pids-max": ("pids",), "log-driver-none": ("logs",), "memcg-max-eq-swap": ("memory-swap",)}
MOUNT_KINDS = ("tmpfs-size", "tmpfs-nr_inodes", "exclusive-finite-fs", "readonly-rootfs")
CGROUP_KINDS = ("pids-max", "memcg-max-eq-swap")
REFUSED_KINDS = (
    "emptydir-sizelimit", "ephemeral-storage-limit", "project-quota-monitoring", "pvc-request",
    "fs-capacity", "watchdog", "sampled-du-statfs", "broker-counter", "eviction",
    "cooperative-writer-cap", "json-file-max-size", "rlimit-fsize", "cacheprog-object-count",
    "storage-opt-size")
DAC_KINDS = ("dac-deny-nonroot", "mode-bits", "ownership", "file-permissions")
DAC_FIELDS = ("non_root_user", "owner", "mode", "acl", "permission", "uid", "gid", "chown")
DAC_TEXT = ("ownership checks cannot establish zero allocation: CHOWN/SETUID-class capabilities "
            "may remain and pre-existing writable fds are unaccounted")
ACCEPTED_QUOTA_KINDS = frozenset()
TRUSTED_PROVENANCE = ("signed-output-metadata", "reproducible-build-attestation")
NETWORK_BINDS = ("/etc/hosts", "/etc/hostname", "/etc/resolv.conf")
NON_STORAGE = {"proc": "pseudo-filesystem, no persistent allocation",
               "devpts": "pty count bounded host-wide; not a storage sink"}
UNACCOUNTED_FSTYPES = ("mqueue", "sysfs", "cgroup", "cgroup2")
THRESHOLD_MAX_AGE = 300
SAMPLE_MAX_AGE = 60
COLLECTOR_SECTIONS = frozenset(("binding", "threshold", "mountinfo", "runtime", "daemon", "backing",
                                "charge_map", "claims"))
LEGS = ("binding", "threshold", "charge_map", "enforcement", "aliasing", "envelope", "backing", "canary")
# Reasons that contradict a claim; every other reason means evidence is missing or unproven.
NEGATIVE = frozenset((
    "binding-image-mismatch", "toolchain-mount-writable", "go-absent-verified",
    "threshold-contradictory", "daemon-capability-unsupported", "domain-overcommitted",
    "domain-alias-split", "sink-mapping-mismatch", "envelope-drift", "boundary-removable",
    "boundary-not-allocation-enforcement", "boundary-resource-mismatch", "boundary-resource-unknown",
    "boundary-sink-out-of-scope", "unaccounted-writable-mount", "unaccounted-writable-fd",
    "write-deny-not-erofs", "canary-kill-only", "canary-errno-unexpected",
    "backing-aggregate-exceeds-soft", "sink-pseudo-forbidden"))


class JsParseUnsupported(ValueError):
    """Input outside the emulated JS Number grammar; the caller must not guess."""


# ---- margin: emulation of agent/src/config.ts marginFraction (String.trim + Number) ----------

JS_WHITESPACE = frozenset("\t\n\v\f\r           "
                          "       　﻿")
_DECIMAL = re.compile(r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?")
_RADIX = re.compile(r"0([xXbBoO])([0-9a-fA-F]+)")
_BASES = {"x": 16, "b": 2, "o": 8}


def js_trim(text):
    start, end = 0, len(text)
    while start < end and text[start] in JS_WHITESPACE:
        start += 1
    while end > start and text[end-1] in JS_WHITESPACE:
        end -= 1
    return text[start:end]


def js_number(token):
    """Number(token) for a trimmed, nonempty string; None stands for NaN."""
    if token in ("Infinity", "+Infinity"):
        return math.inf
    if token == "-Infinity":
        return -math.inf
    radix = _RADIX.fullmatch(token)
    if radix:
        try:
            n = int(radix.group(2), _BASES[radix.group(1).lower()])
        except ValueError:
            return None
        return float(n) if n < 2 else 2.0
    if _DECIMAL.fullmatch(token):
        return float(token)
    return None


def config_margin(raw):
    """Return (value, fell_back) as agent/src/config.ts marginFraction(env, key, 0.1) would.

    raw is None for an unset variable. Blank, NaN, non-finite and out-of-[0,1) inputs fall
    back to 0.1 (fell_back True). Inputs this emulation cannot classify raise
    JsParseUnsupported instead of guessing.
    """
    if raw is None:
        return FALLBACK_MARGIN, True
    if type(raw) is not str or len(raw) > 256:
        raise JsParseUnsupported("margin input outside the emulated domain")
    if any(ord(c) < 32 and c not in JS_WHITESPACE for c in raw):
        raise JsParseUnsupported("control character in margin input")
    token = js_trim(raw)
    if not token:
        return FALLBACK_MARGIN, True
    value = js_number(token)
    if value is not None and math.isfinite(value) and 0 <= value < 1:
        return value, False
    return FALLBACK_MARGIN, True


def golden_matches(config_ts=None, golden=None):
    """True when the config.ts the golden table was generated from is the one on disk."""
    try:
        recorded = json.loads(Path(golden or GOLDEN_PATH).read_bytes())["config_ts_sha256"]
        return recorded == hashlib.sha256(Path(config_ts or CONFIG_TS).read_bytes()).hexdigest()
    except (OSError, ValueError, KeyError, TypeError):
        return False


# ---- leg checks: each returns reasons only; tiers are graded separately ----------------------

def _add(reasons, code):
    if code not in reasons:
        reasons.append(code)


def _is_int(value, minimum=None):
    return type(value) is int and (minimum is None or value >= minimum)


def _is_num(value):
    return type(value) in (int, float) and math.isfinite(value)


def check_binding(binding, head_revision=None):
    reasons = []
    if not isinstance(binding, dict):
        return ["binding-source-unbound"]
    for key in ("repo_revision", "scaffold_revision"):
        value = binding.get(key)
        if type(value) is not str or not re.fullmatch(r"[0-9a-f]{40}", value):
            _add(reasons, "binding-source-unbound")
    if head_revision is not None and binding.get("repo_revision") != head_revision:
        _add(reasons, "binding-source-unbound")
    digests = [binding.get(k) for k in ("index_digest", "platform_digest", "image_id")]
    if any(type(d) is not str or not re.fullmatch(r"sha256:[0-9a-f]{64}", d) for d in digests) \
            or len(set(digests)) != 3:
        _add(reasons, "binding-source-unbound")
    if binding.get("running_container_image_id") != binding.get("image_id"):
        _add(reasons, "binding-image-mismatch")
    mounts = binding.get("toolchain_mounts", [])
    require(type(mounts) is list)
    if any(type(m) is not dict or m.get("ro") is not True for m in mounts):
        _add(reasons, "toolchain-mount-writable")
    go = binding.get("go")
    go = go if isinstance(go, dict) else {}
    failed = False
    if binding.get("go_declared") is False:
        failed = True
        if go.get("observed_in") == "image-fs" and go.get("absent_verified") is True:
            _add(reasons, "go-absent-verified")
        else:
            _add(reasons, "go-not-declared")
    elif go.get("observed_in") != "image-fs":
        failed = True
        _add(reasons, "go-host-only")
    else:
        version, provenance = go.get("version"), go.get("provenance")
        if not (type(go.get("sha256")) is str and re.fullmatch(r"[0-9a-f]{64}", go["sha256"])
                and type(go.get("source_sha256")) is str and re.fullmatch(r"[0-9a-f]{64}", go["source_sha256"])
                and type(version) is str and re.fullmatch(r"go[0-9]+\.[0-9]+(?:\.[0-9]+)?", version)):
            failed = True
        if not (isinstance(provenance, dict) and provenance.get("kind") in TRUSTED_PROVENANCE):
            failed = True
            _add(reasons, "provenance-coordinate-only")
    if failed:
        _add(reasons, "no-go-bearing-image-verified")
    return reasons


def check_threshold(value, now, config_ts=None):
    """Return {"reasons": [...], "soft": float|None}; soft mirrors disk-reclaim softThreshold()."""
    reasons = []
    if not isinstance(value, dict) or (value.get("api_value") is None and value.get("effective") is None):
        return dict(reasons=["owner-input-not-supplied"], soft=None)
    api = value.get("api_value")
    # A missing or non-mapping effective value is "unestablished", never an exception.
    effective = value.get("effective") if isinstance(value.get("effective"), dict) else {}
    if not (_is_num(api) and 0 < api <= 1) or not _is_num(value.get("api_observed_at")):
        return dict(reasons=["owner-input-not-supplied"], soft=None)
    stamps = [value["api_observed_at"]]
    if _is_num(effective.get("observed_at")):
        stamps.append(effective["observed_at"])
    if any(not 0 <= now-stamp <= THRESHOLD_MAX_AGE for stamp in stamps):
        _add(reasons, "threshold-stale")
    if not (effective.get("kind") in ("soft-threshold-log", "owner-authorized")
            and _is_num(effective.get("observed_at")) and _is_num(effective.get("value"))):
        _add(reasons, "effective-value-unestablished")
        return dict(reasons=reasons, soft=None)
    state, raw = value.get("margin_state"), value.get("margin_raw")
    soft = None
    try:
        require(state in ("set", "unset") and (raw is None) == (state == "unset"))
        margin, _ = config_margin(raw)
        if not golden_matches(config_ts):
            raise JsParseUnsupported("config.ts differs from the golden table")
        soft = api-margin if api-margin > 0 else api
    except ValueError:
        _add(reasons, "margin-js-parse-unsupported")
    if soft is not None:
        incarnation = value.get("incarnation")
        if (type(incarnation) is not str or not incarnation or effective.get("incarnation") != incarnation
                or abs(effective["value"]-soft) > 1e-9):
            _add(reasons, "threshold-contradictory")
    return dict(reasons=reasons, soft=soft)


def check_envelope(envelope):
    reasons = []
    limits = envelope.get("limits") if isinstance(envelope, dict) else None
    if not (isinstance(limits, dict) and set(limits) == set(p.LIMITS)
            and all(type(limits[k]) is type(v) and limits[k] == v for k, v in p.LIMITS.items())):
        _add(reasons, "envelope-drift")
    ceiling = envelope.get("inode_ceiling") if isinstance(envelope, dict) else None
    if not (isinstance(ceiling, dict) and ceiling.get("owner_approved") is True and _is_int(ceiling.get("value"), 1)):
        _add(reasons, "inode-ceiling-unapproved")
    return reasons


def outer_exempt(name, sink):
    """Daemon-side and container-log sinks live in an outer domain with no container superblock.
    The exemption is an explicit `outer_domain: true` on one of those named sinks; any other
    sink must resolve to a container mount."""
    return name in OUTER_EXEMPT and sink.get("outer_domain") is True


def _parent_cycle(sinks, name):
    seen, cur = set(), name
    while isinstance(sinks.get(cur), dict) and type(sinks[cur].get("parent")) is str:
        if cur in seen:
            return True
        seen.add(cur)
        cur = sinks[cur]["parent"]
    return False


def _outer_ancestor(sinks, name):
    """Whether a parent-chain ancestor of `name` is an outer sink. Callers have already rejected
    cycles, so the walk ends."""
    cur = sinks[name].get("parent")
    while isinstance(sinks.get(cur), dict):
        if outer_exempt(cur, sinks[cur]):
            return True
        cur = sinks[cur].get("parent")
    return False


def check_sinks(sinks):
    require(type(sinks) is dict)
    reasons = []
    if any(name not in sinks for name in CHARGE_SINKS):
        _add(reasons, "sink-missing")
    for name, s in sinks.items():
        require(type(s) is dict and s.get("identity") == name)
        if "pseudo" in s:
            # Only the evaluator injects pseudo sinks (process_tree, memory); an artifact cannot.
            _add(reasons, "sink-pseudo-forbidden")
            continue
        # outer_domain is meaningful on the exempt names only; a child sink may not carry it either.
        if "outer_domain" in s and name not in OUTER_EXEMPT:
            _add(reasons, "sink-malformed")
        if "parent" in s:
            require(type(s["parent"]) is str and s["parent"] != name)
            if s["parent"] not in sinks:
                _add(reasons, "sink-missing")
            elif _parent_cycle(sinks, name):
                _add(reasons, "sink-parent-cycle")
            elif name not in OUTER_EXEMPT and _outer_ancestor(sinks, name):
                _add(reasons, "sink-malformed")
            continue
        require(type(s.get("paths")) is list and all(type(x) is str for x in s["paths"]))
        require(type(s.get("backing_domain")) is str)
        require(s.get("mount_id") is None or _is_int(s["mount_id"]))
        require(_is_int(s.get("bytes"), 0) and _is_int(s.get("inodes"), 0))
        if s["inodes"] < 1:
            _add(reasons, "sink-malformed")
        if outer_exempt(name, s):
            if "superblock" in s or s.get("mount_id") is not None:
                _add(reasons, "sink-malformed")
        else:
            require(type(s.get("superblock")) is str)
    return reasons


def exclusive_finite(mount):
    return (mount.get("exclusive") is True and _is_int(mount.get("capacity_bytes"), 1)
            and _is_int(mount.get("capacity_inodes"), 1))


def classify_mount(mount):
    """Return (class, reason): readonly, non-storage, storage or unaccounted."""
    require(type(mount) is dict and type(mount.get("fstype")) is str and type(mount.get("mountpoint")) is str)
    fstype, point = mount["fstype"], mount["mountpoint"]
    if mount.get("ro") is True:
        return "readonly", None
    if point in NETWORK_BINDS:
        return "unaccounted", "writable network bind"
    if mount.get("bind") or mount.get("volume"):
        return "unaccounted", "writable bind or volume"
    if fstype in NON_STORAGE:
        return "non-storage", NON_STORAGE[fstype]
    if fstype in UNACCOUNTED_FSTYPES:
        return "unaccounted", "writable " + fstype
    if fstype == "tmpfs" or (fstype == "overlay" and point == "/") or exclusive_finite(mount):
        return "storage", fstype
    return "unaccounted", "writable " + fstype


def kernel_memory_accounting(mounts):
    """Every mount, read-only and non-storage included, stays charged to the memory domain
    (tmpfs pages, dentry and inode slab)."""
    require(type(mounts) is list)
    return [dict(mount_id=m["mount_id"], mountpoint=m["mountpoint"], fstype=m["fstype"],
                 ro=m.get("ro") is True, charged_to="memory") for m in mounts]


def check_mounts(mounts, fds):
    require(type(mounts) is list and type(fds) is list)
    reasons, accepted, classes = [], [], {}
    for m in mounts:
        cls, why = classify_mount(m)
        classes[m["mount_id"]] = cls
        if cls == "unaccounted":
            _add(reasons, "unaccounted-writable-mount")
        elif cls == "non-storage":
            accepted.append(dict(mount_id=m["mount_id"], reason=why))
    for fd in fds:
        require(type(fd) is dict)
        if fd.get("writable") is not True:
            continue
        # mount_id, when the fd carries one, is the only key; the superblock is the fallback.
        if fd.get("mount_id") is not None:
            found = [m for m in mounts if m["mount_id"] == fd["mount_id"]]
        else:
            found = [m for m in mounts if "target_superblock" in fd and m["superblock"] == fd["target_superblock"]]
        # An fd that stays writable on a read-only mount contradicts the mount's flag.
        if not found or any(classes[m["mount_id"]] in ("unaccounted", "readonly") for m in found):
            _add(reasons, "unaccounted-writable-fd")
    return dict(reasons=reasons, accepted=accepted, kernel_memory=kernel_memory_accounting(mounts))


def _sink_sb(sinks, name):
    s = sinks[name]
    return _sink_sb(sinks, s["parent"]) if "parent" in s else s.get("superblock")


def superblock_limits(mounts):
    limits = {}
    for m in mounts:
        opts = m.get("options") if isinstance(m.get("options"), dict) else {}
        size = opts.get("size") if m.get("fstype") == "tmpfs" else None
        nodes = opts.get("nr_inodes") if m.get("fstype") == "tmpfs" else None
        if exclusive_finite(m):
            size, nodes = m["capacity_bytes"], m["capacity_inodes"]
        entry = limits.setdefault(m["superblock"], [None, None])
        for n, v in enumerate((size, nodes)):
            if _is_int(v, 1) and (entry[n] is None or v < entry[n]):
                entry[n] = v
    return limits


def _path_superblocks(mounts, real, paths):
    """(superblocks, unresolved): the superblocks of every path that has a recorded realpath, by
    longest mountpoint prefix (None when no path resolved), and whether any listed path lacks one."""
    resolvable = [x for x in paths if type(real.get(x)) is str]
    hits = [_longest_prefix(mounts, real[x]) for x in resolvable]
    found = {m["superblock"] for m in hits if m is not None} | ({None} if None in hits else set())
    return (found or None), len(resolvable) != len(paths)


def check_aliasing(sinks, mounts, realpaths=None):
    """Domain key is the superblock. A sink's declared superblock is never trusted: it must be
    a superblock in mountinfo and agree with the mount named by its mount_id and with the mount
    its realpaths resolve to; a container sink needs a mount_id or resolvable paths. Sinks
    flagged outer_domain (see outer_exempt) are charged only through the backing leg. Child
    sinks charge their parent."""
    require(type(sinks) is dict and type(mounts) is list)
    reasons, totals, domains = [], {}, {}
    real = realpaths if isinstance(realpaths, dict) else {}
    limits = superblock_limits(mounts)
    by_id = {m["mount_id"]: m for m in mounts}
    known = {m["superblock"] for m in mounts}
    for name, s in sinks.items():
        if "parent" in s:
            continue
        if outer_exempt(name, s):
            # An outer sink resolving into a container mount is charged to the wrong domain.
            if any(type(real.get(x)) is str and _longest_prefix(mounts, real[x]) is not None
                   for x in s["paths"]):
                _add(reasons, "sink-malformed")
            continue
        sb = s["superblock"]
        resolved, unresolved = _path_superblocks(mounts, real, s["paths"])
        mount = by_id.get(s.get("mount_id"))
        if (sb not in known
                or unresolved
                or (s.get("mount_id") is None and resolved is None)
                or (s.get("mount_id") is not None and (mount is None or mount["superblock"] != sb))
                or (resolved is not None and resolved != {sb})):
            _add(reasons, "sink-mapping-mismatch")
            if resolved is not None and len(resolved) == 1 and None not in resolved:
                sb = next(iter(resolved))
        total = totals.setdefault(sb, [0, 0])
        total[0] += s["bytes"]
        total[1] += s["inodes"]
        domains.setdefault(sb, set()).add(s["backing_domain"])
    for sb, total in totals.items():
        limit = limits.get(sb, [None, None])
        if any(limit[n] is not None and total[n] > limit[n] for n in (0, 1)):
            _add(reasons, "domain-overcommitted")
    if any(len(v) > 1 for v in domains.values()):
        _add(reasons, "domain-alias-split")
    return reasons


def _longest_prefix(mounts, real):
    best = None
    for m in mounts:
        point = m["mountpoint"]
        if point == "/" or real == point or real.startswith(point.rstrip("/")+"/"):
            if best is None or len(point) > len(best["mountpoint"]):
                best = m
    return best


def daemon_capability(daemon):
    """Reasons from the daemon's own capability report; needs no container or mount facts."""
    limits = [daemon.get(k) for k in ("MemoryLimit", "SwapLimit", "PidsLimit")]
    # An explicit False contradicts the claim; an absent or non-boolean report is only missing.
    if (any(v is False for v in limits)
            or (daemon.get("rootless") is True and daemon.get("CgroupDriver") == "cgroupfs")
            or daemon.get("rootless") not in (True, False)):
        return ["daemon-capability-unsupported"]
    # Rootless with an unreported cgroup driver cannot be judged either way: missing, not supported.
    rootless_unknown = daemon.get("rootless") is True and type(daemon.get("CgroupDriver")) is not str
    return ["daemon-info-missing"] if rootless_unknown or any(v is not True for v in limits) else []


def evaluate_claims(claims, mounts, sinks, facts, require_collector=True):
    """Judge boundary claims {kind, resource, sinks[, superblock]} against observed mounts.

    facts: tier{mountinfo,runtime,daemon: "file"|"collector"}, new_mount, container, applied,
    daemon, realpaths, backend_support. With require_collector, mount- and cgroup-dependent
    claims whose evidence tier is not "collector" are not accepted (claim-evidence-not-collector).
    Returns {covered, reasons, notes, coverage, uncovered}.
    """
    require(type(claims) is list and type(mounts) is list and type(sinks) is dict and type(facts) is dict)
    reasons, notes = [], {}
    tier = facts.get("tier") or {}
    container, daemon = facts.get("container"), facts.get("daemon")
    applied = facts.get("applied") if isinstance(facts.get("applied"), dict) else {}
    real = facts.get("realpaths") if isinstance(facts.get("realpaths"), dict) else {}
    cov = {name: dict(bytes=False, inodes=False) for name in sinks}
    cov_sb, excl, domain_cov = {}, {}, set()

    def trusted(*sections):
        return not require_collector or all(tier.get(s) == "collector" for s in sections)

    def new_mount_ok():
        nm = facts.get("new_mount")
        if not isinstance(nm, dict):
            return False
        eq = lambda key, n: type(nm.get(key)) is int and nm[key] == n
        return eq("cap_bnd", 0) and eq("no_new_privs", 1) and (
            (eq("seccomp", 2) and nm.get("seccomp_profile") == "default") or eq("max_user_namespaces", 0))

    def in_sb(names, sb):
        ok = True
        for name in names:
            if name not in sinks or _sink_sb(sinks, name) != sb:
                _add(reasons, "boundary-sink-out-of-scope")
                ok = False
        return ok

    def sb_mount(sb, pred):
        return any(m["superblock"] == sb and pred(m) for m in mounts)

    for claim in claims:
        require(type(claim) is dict)
        kind, resource = claim.get("kind"), claim.get("resource")
        if kind in DAC_KINDS or any(key in claim for key in DAC_FIELDS):
            _add(reasons, "dac-ownership-mode-unverified")
            notes["dac-ownership-mode-unverified"] = DAC_TEXT
            continue
        backend = kind == "storage-opt-size" and facts.get("backend_support") is True
        if (kind in REFUSED_KINDS and not backend) or (kind not in REFUSED_KINDS and kind not in KIND_RESOURCE):
            _add(reasons, "boundary-not-allocation-enforcement")
            continue
        if resource is None:
            _add(reasons, "boundary-resource-missing")
            continue
        if resource not in RESOURCES:
            _add(reasons, "boundary-resource-unknown")
            continue
        if resource not in (("bytes",) if backend else KIND_RESOURCE[kind]):
            _add(reasons, "boundary-resource-mismatch")
            continue
        names = claim.get("sinks", [])
        require(type(names) is list and all(type(n) is str for n in names))
        if kind in MOUNT_KINDS or kind in CGROUP_KINDS:
            if not isinstance(container, dict):
                _add(reasons, "container-facts-missing")
                continue
            removable = container.get("privileged") is not False
            if kind in MOUNT_KINDS:
                caps = container.get("caps")
                removable = removable or type(caps) is not list or any(c in ("CAP_SYS_ADMIN", "SYS_ADMIN") for c in caps)
            else:
                removable = removable or container.get("cgroupfs_writable") is not False \
                    or container.get("cgroupns") is not True
            if removable:
                _add(reasons, "boundary-removable")
                continue
        if kind in MOUNT_KINDS:
            if not new_mount_ok():
                _add(reasons, "new-mount-possible")
                continue
            if not trusted("mountinfo", "runtime"):
                _add(reasons, "claim-evidence-not-collector")
                continue
        if backend:
            if not trusted("runtime"):
                _add(reasons, "claim-evidence-not-collector")
                continue
            for name in names:
                if name in sinks:
                    cov[name]["bytes"] = True
        elif kind in ("tmpfs-size", "tmpfs-nr_inodes", "exclusive-finite-fs"):
            sb = claim.get("superblock")
            if type(sb) is not str or not in_sb(names, sb):
                _add(reasons, "boundary-sink-out-of-scope")
                continue
            option = "size" if kind == "tmpfs-size" else "nr_inodes"
            if kind == "exclusive-finite-fs":
                if sb_mount(sb, exclusive_finite):
                    excl.setdefault(sb, {"bytes": [], "inodes": []})[resource].append(names)
            elif sb_mount(sb, lambda m: m["fstype"] == "tmpfs" and _is_int((m.get("options") or {}).get(option), 1)):
                cov_sb.setdefault(sb, dict(bytes=False, inodes=False))[resource] = True
                for name in names:
                    cov[name][resource] = True
        elif kind == "readonly-rootfs":
            ok_names = []
            for name in names:
                paths = sinks[name].get("paths", []) if name in sinks else []
                hit = [_longest_prefix(mounts, real[x]) for x in paths if type(real.get(x)) is str]
                if paths and len(hit) == len(paths) and all(
                        m is not None and m["mountpoint"] == "/" and m.get("ro") is True for m in hit):
                    ok_names.append(name)
                else:
                    _add(reasons, "boundary-sink-out-of-scope")
            if not names:
                _add(reasons, "boundary-sink-out-of-scope")
            for name in ok_names:
                cov[name] = dict(bytes=True, inodes=True)
        else:
            want, key = {"pids-max": (["process_tree"], "pids"), "log-driver-none": (["container_logs"], "logs"),
                         "memcg-max-eq-swap": (["memory"], "memory-swap")}[kind]
            if names != want or any(k in claim for k in ("mount_id", "superblock")):
                _add(reasons, "boundary-sink-out-of-scope")
                continue
            if not trusted("runtime", "daemon"):
                _add(reasons, "claim-evidence-not-collector")
                continue
            if kind == "pids-max":
                applied_ok = _is_int(applied.get("pids_max"), 1)
            elif kind == "log-driver-none":
                applied_ok = applied.get("log_driver") == "none"
            else:
                applied_ok = (_is_int(applied.get("memory_max"), 1)
                              and applied.get("memory_max") == applied.get("memory_swap_max"))
            if not applied_ok:
                _add(reasons, "limits-not-applied-observed")
                continue
            domain_cov.add(key)
    for sb, pair in excl.items():
        if pair["bytes"] and pair["inodes"]:
            cov_sb[sb] = dict(bytes=True, inodes=True)
            for resource in ("bytes", "inodes"):
                for names in pair[resource]:
                    for name in names:
                        cov[name][resource] = True
    if isinstance(daemon, dict):
        for code in daemon_capability(daemon):
            _add(reasons, code)
    elif any(c.get("kind") in CGROUP_KINDS for c in claims):
        _add(reasons, "daemon-info-missing")
    for name, s in sinks.items():
        if "parent" in s:
            cov[name] = dict(cov[s["parent"]])
    uncovered = dict(bytes=[], inodes=[], mounts=dict(bytes=[], inodes=[]))
    domain_need = {"process_tree": ("pids", "sink-pids-uncovered"), "memory": ("memory-swap", "sink-memory-swap-uncovered")}
    for name, s in sinks.items():
        if name in domain_need or name in LOG_SINKS:
            key, code = domain_need.get(name, ("logs", "sink-logs-uncovered"))
            if key not in domain_cov:
                _add(reasons, code)
        elif name not in OUTER_SINKS and s.get("pseudo") is None:
            for resource in ("bytes", "inodes"):
                if not cov[name][resource]:
                    uncovered[resource].append(name)
                    _add(reasons, "sink-%s-uncovered" % resource)
    for m in mounts:
        if classify_mount(m)[0] == "storage":
            got = cov_sb.get(m["superblock"], {})
            for resource in ("bytes", "inodes"):
                if not got.get(resource):
                    uncovered["mounts"][resource].append(m["mount_id"])
                    _add(reasons, "sink-%s-uncovered" % resource)
    return dict(covered=not reasons, reasons=reasons, notes=notes, coverage=cov, uncovered=uncovered)


def backing_check(charges, domains, soft, now):
    """Each domain link must be observed (not merely cited), fresh, with known capacity and
    concurrent use; charges are summed once per domain via preflight.project_domains and the
    projection must stay strictly below soft x capacity for bytes and inodes."""
    require(type(charges) is dict and type(domains) is dict and _is_num(soft) and 0 < soft <= 1)
    reasons, samples = [], {}
    for charge in charges.values():
        require("domain" in charge)
        if charge["domain"] not in domains:
            _add(reasons, "backing-mapping-unknown")
    for key in sorted({c["domain"] for c in charges.values() if c["domain"] in domains}):
        link = domains[key]
        obs = link.get("observed") if isinstance(link, dict) else None
        if not isinstance(obs, dict):
            _add(reasons, "backing-link-cited-only" if isinstance(link, dict) and "cited" in link
                 else "backing-mapping-unknown")
            continue
        if not (_is_int(obs.get("capacity_bytes"), 1) and _is_int(obs.get("capacity_inodes"), 1)
                and _is_int(obs.get("used_bytes"), 0) and _is_int(obs.get("used_inodes"), 0)):
            _add(reasons, "backing-capacity-unknown")
            continue
        if not (_is_int(obs.get("concurrent_bytes"), 0) and _is_int(obs.get("concurrent_inodes"), 0)):
            _add(reasons, "backing-concurrency-unknown")
            continue
        stamp = obs.get("sampled_at")
        if not _is_num(stamp) or not 0 <= now-stamp <= SAMPLE_MAX_AGE:
            _add(reasons, "backing-sample-stale")
            continue
        samples[key] = SimpleNamespace(
            f_blocks=obs["capacity_bytes"], f_frsize=1,
            f_bavail=max(0, obs["capacity_bytes"]-obs["used_bytes"]-obs["concurrent_bytes"]),
            f_files=obs["capacity_inodes"],
            f_favail=max(0, obs["capacity_inodes"]-obs["used_inodes"]-obs["concurrent_inodes"]))
    known = {n: c for n, c in charges.items() if c["domain"] in samples}
    for s, b, i in p.project_domains(known, samples).values():
        if not ((s.f_blocks-s.f_bavail)*s.f_frsize+b < soft*s.f_blocks*s.f_frsize
                and s.f_files-s.f_favail+i < soft*s.f_files):
            _add(reasons, "backing-aggregate-exceeds-soft")
    return reasons


def classify_canary(result):
    """Refusal classification of canary write attempts, one record per sink."""
    require(isinstance(result, dict) and type(result.get("sinks")) is list)
    reasons = []
    for item in result["sinks"]:
        require(type(item) is dict)
        kind, errno = item.get("kind"), item.get("errno")
        if errno is None and item.get("killed") is True:
            _add(reasons, "canary-kill-only")
        elif kind == "readonly-rootfs":
            if not (errno == "EROFS" and item.get("ro_flag") is True):
                _add(reasons, "write-deny-not-erofs")
        elif kind in ("tmpfs-size", "tmpfs-nr_inodes", "exclusive-finite-fs") and errno == "ENOSPC":
            continue
        elif errno == "EDQUOT" and kind in ACCEPTED_QUOTA_KINDS:
            continue
        else:
            _add(reasons, "canary-errno-unexpected")
    return dict(ok=not reasons, reasons=reasons)


# ---- evidence tiers: the only constructor of collector evidence is collect() -----------------

_COLLECTOR_TOKEN = object()


class CollectorEvidence:
    """Sections observed in this invocation. Constructed only by collect(); a JSON file, the
    CLI and any caller without the module-private token cannot build one. It has no canary
    attribute: there is no canary producer in M1, so evaluate() never reads one."""
    __slots__ = ("sections", "daemon_id_hash")

    def __init__(self, token, sections, salt, daemon_id):
        require(token is _COLLECTOR_TOKEN and type(sections) is dict and set(sections) <= COLLECTOR_SECTIONS
                and type(salt) is bytes and len(salt) >= 16 and type(daemon_id) is str)
        self.sections = sections
        self.daemon_id_hash = hashlib.sha256(salt + daemon_id.encode()).hexdigest()


def collect(observer):
    """The collector code path; observe.py is the collector. `observer.observe()` returns the sections
    (binding, threshold, mountinfo, runtime, daemon, backing, charge_map, claims) read from the
    live environment and `observer.daemon_id()` a daemon identity string, which is stored only
    as a hash salted per invocation. A leg reaches PROVEN only when the collector supplied every
    section it uses, so a charge_map or claims section left to the artifact file caps the legs
    that read it. `claims` is a list; the others are dicts."""
    sections = observer.observe()
    require(type(sections) is dict and all(type(v) in (dict, list) for v in sections.values()))
    sections = {name: dict(value, source="collector") if type(value) is dict else value
                for name, value in sections.items()}
    return CollectorEvidence(_COLLECTOR_TOKEN, sections, os.urandom(16), observer.daemon_id())


def grade(reasons, evidence, tiers, **extra):
    if reasons:
        negative = [r for r in reasons if r in NEGATIVE]
        status, reason = ("NEGATIVE" if negative else "UNVERIFIED"), (negative or reasons)[0]
    elif evidence and all(tiers.get(name) == "collector" for name in evidence):
        status, reason = "PROVEN", "collector-observed"
    else:
        status, reason = "FIXTURE-CONSISTENT", "file-evidence-only"
    return dict(status=status, reason=reason, reasons=list(reasons),
                tier=sorted({tiers.get(name, "file") for name in evidence}) if evidence else ["declared"], **extra)


def evaluate(artifact, collector=None, now=None, head_revision=None, config_ts=None):
    """Pure verdict. `collector` must be a CollectorEvidence from collect(); anything else,
    including a dict parsed from a file, is refused."""
    require(collector is None or isinstance(collector, CollectorEvidence))
    now = time.time() if now is None else now
    art = artifact if isinstance(artifact, dict) else {}
    sections, tiers = {}, {}
    for name in ("binding", "threshold", "mountinfo", "runtime", "daemon", "backing",
                 "charge_map", "claims", "envelope"):
        if collector is not None and name in collector.sections:
            sections[name], tiers[name] = collector.sections[name], "collector"
        elif name in art:
            sections[name], tiers[name] = art[name], "file"
    legs = {}
    if art.get("schema") != SCHEMA:
        legs = {name: grade(["schema-unsupported"], (), tiers) for name in LEGS}
    else:
        def run(name, fn):
            try:
                legs[name] = fn()
            except (ValueError, KeyError, TypeError, AttributeError, OverflowError, RecursionError):
                legs[name] = grade(["malformed-section"], (), tiers)
        info = sections.get("mountinfo")
        mount_facts = isinstance(info, dict) and type(info.get("mounts")) is list and type(info.get("fds")) is list
        raw_sinks = sections.get("charge_map")
        sinks = raw_sinks.get("sinks") if isinstance(raw_sinks, dict) else None
        threshold = {}
        pseudo_sink = isinstance(sinks, dict) and any(isinstance(s, dict) and "pseudo" in s for s in sinks.values())

        def binding():
            return grade(check_binding(sections.get("binding"), head_revision) if "binding" in sections
                         else ["binding-source-unbound"], ("binding",), tiers)

        def thresh():
            threshold.update(check_threshold(sections.get("threshold"), now, config_ts))
            return grade(threshold["reasons"], ("threshold",), tiers, soft=threshold["soft"])

        def charge_map():
            reasons = check_sinks(sinks)
            extra = dict(kernel_memory=[], accepted=[])
            if mount_facts:
                got = check_mounts(info["mounts"], info["fds"])
                reasons += [r for r in got["reasons"] if r not in reasons]
                extra = dict(kernel_memory=got["kernel_memory"], accepted=got["accepted"])
            else:
                _add(reasons, "no-container-mount-facts")
            return grade(reasons, ("mountinfo", "charge_map"), tiers, **extra)

        def enforcement():
            if not mount_facts:
                # The daemon's capability report stands on its own: a rootless cgroupfs daemon is
                # a contradiction whether or not any container mount was observed.
                got = ["no-container-mount-facts"]
                evidence = ("mountinfo",)
                if isinstance(sections.get("daemon"), dict):
                    got += daemon_capability(sections["daemon"])
                    evidence = ("mountinfo", "daemon")
                return grade(got, evidence, tiers)
            if pseudo_sink:
                return grade(["sink-pseudo-forbidden"], ("charge_map",), tiers)
            runtime = sections.get("runtime") if isinstance(sections.get("runtime"), dict) else {}
            daemon = sections.get("daemon")
            daemon = {k: v for k, v in daemon.items() if k != "source"} if isinstance(daemon, dict) else None
            facts = dict(tier={k: tiers.get(k, "file") for k in ("mountinfo", "runtime", "daemon")},
                         new_mount=runtime.get("new_mount"), container=runtime.get("container"),
                         applied=runtime.get("applied"), daemon=daemon,
                         realpaths=info.get("realpaths"), backend_support=runtime.get("backend_support"))
            domain_sinks = dict(sinks, process_tree=dict(pseudo="process_tree"), memory=dict(pseudo="memory"))
            # require_collector stays False here: evaluate() grades file evidence by tier
            # (FIXTURE-CONSISTENT at most) instead of refusing the claims outright.
            got = evaluate_claims(sections.get("claims"), info["mounts"], domain_sinks, facts, require_collector=False)
            return grade(got["reasons"], ("mountinfo", "runtime", "daemon", "charge_map", "claims"), tiers,
                         notes=got["notes"], uncovered=got["uncovered"])

        def aliasing():
            if not mount_facts:
                return grade(["no-container-mount-facts"], ("mountinfo",), tiers)
            if pseudo_sink:
                return grade(["sink-pseudo-forbidden"], ("charge_map",), tiers)
            return grade(check_aliasing(sinks, info["mounts"], info.get("realpaths")),
                         ("mountinfo", "charge_map"), tiers)

        def backing():
            if pseudo_sink:
                return grade(["sink-pseudo-forbidden"], ("charge_map",), tiers)
            if threshold.get("soft") is None:
                return grade(["backing-soft-unknown"], ("backing", "threshold", "charge_map"), tiers)
            charges = {n: dict(bytes=s["bytes"], inodes=s["inodes"], domain=s["backing_domain"])
                       for n, s in sinks.items() if "parent" not in s}
            return grade(backing_check(charges, sections["backing"]["domains"], threshold["soft"], now),
                         ("backing", "threshold", "charge_map"), tiers)

        run("binding", binding)
        run("threshold", thresh)
        run("charge_map", charge_map)
        run("enforcement", enforcement)
        run("aliasing", aliasing)
        run("envelope", lambda: grade(check_envelope(sections.get("envelope")), (), tiers))
        run("backing", backing)
        # M1 has no canary producer: the leg is unverified whatever any caller attaches.
        legs["canary"] = grade(["canary-collector-not-implemented"], (), tiers)
    qualified = all(legs[name]["status"] == "PROVEN" for name in LEGS)
    return dict(schema=OUTPUT_SCHEMA, scope="prerequisite-evidence", qualified=qualified,
                verdict="QUALIFIED-ENVIRONMENT" if qualified else "NO-QUALIFIED-ENVIRONMENT",
                producer_launched=False, production_authorized=False, legs=legs,
                collector=None if collector is None else dict(daemon_id_hash=collector.daemon_id_hash))


def _revision(value):
    if not re.fullmatch(r"[0-9a-f]{40}", value):
        raise argparse.ArgumentTypeError("must be 40 lowercase hex characters")
    return value


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--artifact", required=True, help="absolute path of an artifact under .uzi/scratch")
    parser.add_argument("--experimental", action="store_true",
                        help="exit 0 on an unqualified verdict; never sets qualified")
    parser.add_argument("--head-revision", type=_revision,
                        help="repo revision the artifact must be bound to; without it a stale repo_revision is not detected")
    args = parser.parse_args(argv)
    try:
        artifact, status = p.read_json(args.artifact), "read"
    except (ValueError, OSError, RecursionError):
        artifact, status = None, "unreadable"
    result = evaluate(artifact, now=time.time(), head_revision=args.head_revision)
    result["artifact"] = status
    print(p.encode(result))
    return 0 if args.experimental or result["qualified"] else 2


if __name__ == "__main__":
    raise SystemExit(main())
