"""Issue 4203 M1: observations only; no producer implementation."""
import argparse
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import re
import socket
import stat
import time

ROOT = Path(__file__).resolve().parents[2]
SCRATCH = ROOT / ".uzi/scratch"
MIB = 1024**2
LIMITS = dict(aggregate=2048*MIB, retained=768*MIB, staging=128*MIB,
              work_modules_output_logs=1152*MIB, memory=1024*MIB,
              clients=2, writers=1, objects=4096, records=8192, frames=2,
              body=64*MIB, header=16*1024, chunk=64*1024,
              tiny_bytes=16*MIB, tiny_seconds=30, workload_seconds=180, series_seconds=1800)
FROZEN = dict(baseline="18da9c670f4403f4fcab3891f8bb8c59757243ef",
              T=256*MIB, twoT=512*MIB, overshoot=256*MIB,
              slowdown=1.25, warm_hit=0.90, near_threshold=0.05)
SINKS = ("home", "gopath", "modules", "cache", "gotmp", "tmp", "work", "output", "logs", "retained", "staging")
MAX_JSON = 65536


def require(ok):
    if not ok:
        raise ValueError("invalid prerequisite")


def scratch_path(raw):
    p = Path(raw)
    require(p.is_absolute() and p.is_relative_to(SCRATCH) and ".." not in p.parts)
    cur = ROOT
    for part in p.relative_to(ROOT).parts:
        cur /= part
        require(not cur.is_symlink())
    return p


def read_json(path):
    # A FIFO/device must not turn this metadata read into a blocking operation.
    fd = os.open(scratch_path(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as f:
        require(stat.S_ISREG(os.fstat(f.fileno()).st_mode))
        raw = f.read(MAX_JSON+1)
    require(len(raw) <= MAX_JSON)
    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result)
            result[key] = value
        return result
    def bounded_int(raw_number):
        require(len(raw_number) <= 20)
        return int(raw_number)
    return json.loads(raw, object_pairs_hook=pairs, parse_int=bounded_int)


def margin_fraction(raw, state):
    # A fail-closed subset of JS Number syntax; runtime invalid fallback is not evidence.
    require(state in ("set", "unset"))
    if state == "unset":
        require(raw is None)
        return 0.1
    require(type(raw) is str and len(raw) <= 256)
    # Accept only ASCII whitespace here; Python strip also accepts non-JS whitespace.
    token = raw.strip(" \t\r\n\v\f")
    require(token and re.fullmatch(
        r"(?:0[xX][0-9a-fA-F]+|0[bB][01]+|0[oO][0-7]+|"
        r"[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)", token))
    if token.lower().startswith(("0x", "0b", "0o")):
        value = float(int(token, 0))
    else:
        value = float(token)
    require(math.isfinite(value) and 0 <= value < 1)
    return value


def threshold(manifest, now):
    require(set(manifest) == {"schema", "provenance", "observed_at", "threshold", "margin", "margin_state"})
    require(type(manifest["schema"]) is int and manifest["schema"] == 2 and manifest["provenance"] == "live-worker-config")
    stamp, value = manifest["observed_at"], manifest["threshold"]
    require(type(stamp) in (float, int) and math.isfinite(stamp) and 0 <= now-stamp <= 300)
    require(type(value) in (float, int) and math.isfinite(value) and 0 < value <= 1)
    margin = margin_fraction(manifest["margin"], manifest["margin_state"])
    return value-margin if value-margin > 0 else value


def child_env(private, mode, toolchain_path=None, cacheprog=None):
    """Empty allowlist; creates nothing, inherits nothing."""
    require(mode in ("DiskCache", "CacheProg"))
    require(type(toolchain_path) is str and 0 < len(toolchain_path) <= 4096)
    parts = toolchain_path.split(":")
    require(len(parts) <= 32)
    for part in parts:
        require(part and not any(c.isspace() or ord(c) < 32 for c in part))
        p = Path(part)
        require(p.is_absolute() and ".." not in p.parts)
        # Resolve /bin and similar links; constructor checks are not trust attestation.
        require(p.is_dir() and not p.resolve().stat().st_mode & (stat.S_IWGRP | stat.S_IWOTH))
    for name in ("go", "cc"):
        selected = next((Path(part)/name for part in parts
                         if (Path(part)/name).is_file() and os.access(Path(part)/name, os.X_OK)), None)
        require(selected is not None)
        require(not selected.resolve().stat().st_mode & (stat.S_IWGRP | stat.S_IWOTH))
    private = scratch_path(private)
    env = dict(PATH=toolchain_path, LANG="C.UTF-8", TZ="UTC", GOTOOLCHAIN="local",
               GOMAXPROCS="2", GOENV="off", GOFLAGS="",
               GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
    for key, name in (("HOME", "home"), ("GOPATH", "gopath"), ("GOMODCACHE", "modules"),
                      ("GOCACHE", "cache"), ("GOTMPDIR", "gotmp"), ("TMPDIR", "tmp")):
        env[key] = str(scratch_path(str(private/name)))
    if mode == "CacheProg":
        require(type(cacheprog) is str and re.fullmatch(r"/[A-Za-z0-9_./+-]+", cacheprog))
        env["GOCACHEPROG"] = str(scratch_path(cacheprog))
    else:
        require(cacheprog is None)
    return env


def headroom(charges, soft, statfs=os.statvfs):
    """Re-sample all sinks each call. Sum projections per device; fail closed.
    M1 has no potential producing launches. Later launchers must call immediately
    before EACH launch and independently prove enforcement covering every sink.
    """
    require(math.isfinite(soft) and 0 < soft <= 1 and set(charges) == set(SINKS))
    for name in SINKS:
        require(type(charges[name]["bytes"]) is int and charges[name]["bytes"] >= 0)
    require(charges["retained"]["bytes"] >= LIMITS["retained"])
    require(charges["staging"]["bytes"] >= LIMITS["staging"])
    require(sum(charges[name]["bytes"] for name in SINKS
                if name not in ("retained", "staging")) >= LIMITS["work_modules_output_logs"])
    require(sum(charges[name]["bytes"] for name in SINKS) == LIMITS["aggregate"])
    devices = {}
    for name in SINKS:
        entry = charges[name]
        p = scratch_path(entry["path"])
        b, i = entry["bytes"], entry["inodes"]
        require(type(b) is int and b >= 0 and type(i) is int and i > 0)
        s = statfs(p)
        require(s.f_blocks > 0 and s.f_files > 0 and s.f_frsize > 0)
        dev = p.stat().st_dev
        old = devices.get(dev, (s, 0, 0))
        devices[dev] = (s, old[1]+b, old[2]+i)
    return all((s.f_blocks-s.f_bavail)*s.f_frsize+b < soft*s.f_blocks*s.f_frsize
               and s.f_files-s.f_favail+i < soft*s.f_files for s, b, i in devices.values())


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, endpoint, **kwargs):
        self.endpoint = endpoint
        super().__init__("localhost", **kwargs)

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(2)
        self.sock.connect(self.endpoint)


def docker_get(endpoint, route):
    # Fixed GET routes; no CLI, context, config, inherited env or process inspection.
    require(route == "/version" or re.fullmatch("/images/sha256:[0-9a-f]{64}/json", route))
    require(Path(endpoint).is_absolute() and stat.S_ISSOCK(Path(endpoint).stat().st_mode))
    conn = UnixHTTP(endpoint, timeout=2)
    try:
        conn.request("GET", route)
        response = conn.getresponse()
        require(response.status == 200)
        deadline = time.monotonic()+5
        chunks, size = [], 0
        while True:
            remaining = deadline-time.monotonic()
            require(remaining > 0)
            if conn.sock is not None:
                conn.sock.settimeout(min(2, remaining))
            chunk = response.read1(min(65536, MAX_JSON+1-size))
            if not chunk:
                break
            size += len(chunk)
            require(size <= MAX_JSON)
            chunks.append(chunk)
        return json.loads(b"".join(chunks))
    finally:
        conn.close()


def inventory(raw_root):
    """Bounded read-only installed Go source inventory, without executing Go."""
    root = Path(raw_root)
    require(root.is_absolute() and root.is_dir())
    # Reject symlinks in all ancestors, not only the final component.
    cur = Path(root.anchor)
    for part in root.parts[1:]:
        cur /= part
        require(not cur.is_symlink())
    require((root/"src").is_dir() and not (root/"src").is_symlink())
    version = root/"VERSION"
    require(version.is_file() and not version.is_symlink())
    with version.open("rb") as f:
        v = f.read(1025)
    require(len(v) <= 1024 and re.match(rb"go[0-9]+\.[0-9]+", v))
    # Length-prefixed fields and a per-file content digest, so no byte can move
    # between VERSION, a path and a file body without changing the aggregate.
    digest = hashlib.sha256(b"uzi-go-source-inventory-v2\0")

    def field(data):
        digest.update(len(data).to_bytes(8, "big") + data)

    field(v)
    started = time.monotonic()
    count = total = entries = 0
    pending = [root/"src"]
    while pending:
        base = pending.pop()
        dirs, files = [], []
        with os.scandir(base) as stream:
            for entry in stream:
                entries += 1
                require(entries <= 20000 and time.monotonic()-started < 10)
                require(not entry.is_symlink())
                (dirs if entry.is_dir(follow_symlinks=False) else files).append(entry.name)
        # Retained names and pending directories together are bounded by entries.
        pending.extend(base/name for name in sorted(dirs, reverse=True))
        for name in sorted(files):
            p = Path(base)/name
            require(not p.is_symlink() and p.is_file())
            count += 1
            field(str(p.relative_to(root)).encode())
            body = hashlib.sha256()
            with p.open("rb") as f:
                while True:
                    chunk = f.read(65536)
                    if not chunk:
                        break
                    total += len(chunk)
                    require(total <= 256*MIB and time.monotonic()-started < 10)
                    body.update(chunk)
            field(body.digest())
    require(count > 0)
    version_text = v.splitlines()[0].decode("ascii")
    require(re.fullmatch(r"go[0-9]+\.[0-9]+(?:\.[0-9]+)?", version_text))
    return dict(files=count, bytes=total, sha256=digest.hexdigest(), version=version_text,
                version_sha256=hashlib.sha256(v).hexdigest())


def leg(status, reason, **facts):
    return dict(status=status, reason=reason, **facts)


def report(args):
    legs = {}
    def observe(name, fn):
        try:
            legs[name] = fn()
        except (ValueError, OSError, KeyError, TypeError, OverflowError, RecursionError,
                http.client.HTTPException):
            legs[name] = leg("UNVERIFIED", "missing-invalid-or-unavailable")
    observe("threshold", lambda: leg("UNVERIFIED", "unauthenticated-manifest-shape-only",
                                    shape_valid=True, calculated_soft=threshold(read_json(args.manifest), time.time())))
    observe("environment", lambda: leg("FIXTURE-PROVEN", "empty-allowlist-construction-only",
        keys=sorted(child_env(str(SCRATCH/"go-cache-admission-private"), "DiskCache",
                              getattr(args, "toolchain_path", None)))))
    def docker():
        require(type(args.docker_version) is str and bool(args.docker_version.strip()))
        return (leg("PROVEN", "endpoint-version-match-only")
                if docker_get(args.endpoint, "/version")["Version"] == args.docker_version
                else leg("NEGATIVE", "endpoint-version-mismatch"))
    observe("docker", docker)
    def image():
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", args.image or ""))
        require(re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", args.repo_digest or ""))
        item = docker_get(args.endpoint, "/images/"+args.image+"/json")
        require(item["Id"] == args.image and args.repo_digest in item["RepoDigests"])
        return leg("PROVEN", "local-image-id-and-repodigest-only")
    observe("image", image)
    def source():
        require(re.fullmatch(r"[0-9a-fA-F]{64}", args.source_sha256 or ""))
        facts = inventory(args.go_root)
        return leg("PROVEN", "installed-source-pin-only", **facts) if facts["sha256"] == args.source_sha256.lower() else leg("NEGATIVE", "source-pin-mismatch")
    observe("source", source)
    legs["allocation"] = leg("UNVERIFIED", "no-independent-enforcement-contract-for-all-sinks")
    legs["headroom"] = leg("UNVERIFIED", "all-sink-charge-map-and-enforcement-unavailable")
    legs["tiny"] = leg("UNVERIFIED", "example.com/cacheproof-leaf-middle-app-and-runkind-small-not-launched")
    for name, size in (("handler", 269), ("workersvc", 576)):
        legs[name] = leg("UNVERIFIED", "resource-incompatible", owner_observed_archive_bytes=size*MIB)
    legs["protocol"] = leg("UNVERIFIED", "later-gated-protocol-and-producer-not-implemented")
    return dict(schema=2, scope="fixture-only", milestone="M1-preflight",
                producer_launched=False, production_authorized=False, ab_trigger=False,
                experimental=args.experimental, limits=LIMITS, frozen=FROZEN, legs=legs)


def encode(value):
    raw = json.dumps(value, ensure_ascii=True, allow_nan=False, sort_keys=True)
    require(len(raw.encode()) <= MAX_JSON)
    return raw


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--experimental", action="store_true", help="allow incomplete preflight exit only; never launch")
    parser.add_argument("--manifest")
    parser.add_argument("--toolchain-path")
    parser.add_argument("--endpoint", default="/var/run/docker.sock")
    parser.add_argument("--docker-version")
    parser.add_argument("--image")
    parser.add_argument("--repo-digest")
    parser.add_argument("--go-root")
    parser.add_argument("--source-sha256")
    args = parser.parse_args()
    print(encode(report(args)))
    return 0 if args.experimental else 2


if __name__ == "__main__":
    raise SystemExit(main())
