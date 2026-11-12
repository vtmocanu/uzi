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


def margin_fraction(raw):
    # agent/src/config.ts marginFraction: invalid or blank => 0.1.
    try:
        if raw is None or str(raw).strip() == "":
            return 0.1
        value = float(raw)
        return value if math.isfinite(value) and 0 <= value < 1 else 0.1
    except (ValueError, TypeError, OverflowError):
        return 0.1


def threshold(manifest, now):
    require(set(manifest) == {"schema", "provenance", "observed_at", "threshold", "margin"})
    require(manifest["schema"] == 1 and manifest["provenance"] == "live-worker-config")
    stamp, value = manifest["observed_at"], manifest["threshold"]
    require(type(stamp) in (float, int) and math.isfinite(stamp) and 0 <= now-stamp <= 300)
    require(type(value) in (float, int) and math.isfinite(value) and 0 < value <= 1)
    margin = margin_fraction(manifest["margin"])
    return value-margin if value-margin > 0 else value


def child_env(private, mode, verified_path="/usr/bin:/bin", cacheprog=None):
    """Empty allowlist; creates nothing, inherits nothing."""
    require(mode in ("DiskCache", "CacheProg") and verified_path == "/usr/bin:/bin")
    for part in verified_path.split(":"):
        p = Path(part)
        require(p.is_dir() and not p.stat().st_mode & stat.S_IWOTH)
    private = scratch_path(private)
    env = dict(PATH=verified_path, LANG="C.UTF-8", TZ="UTC", GOTOOLCHAIN="local",
               GOMAXPROCS="2", GOENV="off", GOFLAGS="",
               GIT_CONFIG_GLOBAL="/dev/null", GIT_CONFIG_SYSTEM="/dev/null")
    for key, name in (("HOME", "home"), ("GOPATH", "gopath"), ("GOMODCACHE", "modules"),
                      ("GOCACHE", "cache"), ("GOTMPDIR", "gotmp"), ("TMPDIR", "tmp")):
        env[key] = str(scratch_path(str(private/name)))
    if mode == "CacheProg":
        require(cacheprog is not None)
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
    devices = {}
    for name in SINKS:
        entry = charges[name]
        p = scratch_path(entry["path"])
        b, i = entry["bytes"], entry["inodes"]
        require(type(b) is int and b >= 0 and type(i) is int and i >= 0)
        s = statfs(p)
        require(s.f_blocks > 0 and s.f_files > 0 and s.f_frsize > 0)
        dev = p.stat().st_dev
        old = devices.get(dev, (s, 0, 0))
        devices[dev] = (s, old[1]+b, old[2]+i)
    return all((s.f_blocks-s.f_bavail)*s.f_frsize+b < soft*s.f_blocks*s.f_frsize
               and s.f_files-s.f_favail+i < soft*s.f_files for s, b, i in devices.values())


class UnixHTTP(http.client.HTTPConnection):
    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(2)
        self.sock.connect(self.host)


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
    digest = hashlib.sha256(v)
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
            digest.update(str(p.relative_to(root)).encode()+b"\0")
            with p.open("rb") as f:
                while True:
                    chunk = f.read(65536)
                    if not chunk:
                        break
                    total += len(chunk)
                    require(total <= 256*MIB and time.monotonic()-started < 10)
                    digest.update(chunk)
            digest.update(b"\0")
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
    observe("threshold", lambda: leg("PROVEN", "current-manifest-shape-only",
                                    soft=threshold(read_json(args.manifest), time.time())))
    observe("environment", lambda: leg("PROVEN", "empty-allowlist-construction-only",
        keys=sorted(child_env(str(SCRATCH/"go-cache-admission-private"), "DiskCache"))))
    observe("docker", lambda: leg("PROVEN", "endpoint-version-match-only")
            if docker_get(args.endpoint, "/version")["Version"] == args.docker_version and args.docker_version
            else leg("NEGATIVE", "endpoint-version-mismatch"))
    def image():
        require(re.fullmatch(r"sha256:[0-9a-f]{64}", args.image or ""))
        item = docker_get(args.endpoint, "/images/"+args.image+"/json")
        require(item["Id"] == args.image and args.repo_digest in item["RepoDigests"])
        require(re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", args.repo_digest or ""))
        return leg("PROVEN", "local-image-id-and-repodigest-only")
    observe("image", image)
    def source():
        facts = inventory(args.go_root)
        return leg("PROVEN", "installed-source-pin-only", **facts) if facts["sha256"] == args.source_sha256 else leg("NEGATIVE", "source-pin-mismatch")
    observe("source", source)
    legs["allocation"] = leg("UNVERIFIED", "no-independent-enforcement-contract-for-all-sinks")
    legs["headroom"] = leg("UNVERIFIED", "all-sink-charge-map-and-enforcement-unavailable")
    legs["tiny"] = leg("UNVERIFIED", "example.com/cacheproof-leaf-middle-app-and-runkind-small-not-launched")
    for name, size in (("handler", 269), ("workersvc", 576)):
        legs[name] = leg("UNVERIFIED", "resource-incompatible", owner_observed_archive_bytes=size*MIB)
    legs["protocol"] = leg("UNVERIFIED", "later-gated-protocol-and-producer-not-implemented")
    return dict(schema=1, scope="fixture-only", milestone="M1-preflight",
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
