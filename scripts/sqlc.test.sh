#!/bin/sh
# Select cases with --case NAME; --mutations proves each assertion independently.
# Set UZI_SQLC_KEEP_TEST_ARTIFACTS=1 to preserve this run's artifacts for debugging.
set -eu
ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
TMPDIR="$ROOT/.uzi/scratch"
mkdir -p "$TMPDIR"
export TMPDIR
exec python3 - "$ROOT" "$@" <<'PY'
import argparse
import atexit
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time

root = Path(sys.argv[1])
parser = argparse.ArgumentParser()
parser.add_argument("--case")
parser.add_argument("--mutations", action="store_true")
parser.add_argument("--script", type=Path, default=root / "scripts/sqlc.sh")
args = parser.parse_args(sys.argv[2:])
source = args.script.read_text()
artifacts = Path(tempfile.mkdtemp(prefix="sqlc-tests.", dir=root / ".uzi/scratch"))
atexit.register(lambda: shutil.rmtree(artifacts) if os.environ.get("UZI_SQLC_KEEP_TEST_ARTIFACTS") != "1" else None)
signal.signal(signal.SIGTERM, lambda signum, frame: sys.exit(128 + signum))
print("ARTIFACTS=" + str(artifacts), flush=True)
pins = re.findall(r'SQLC_\w+_SHA256="([a-f0-9]+)"', source)
payload = b"""#!/bin/sh
printf '%s\\n' "$PWD" "$@" > "$CHILD_LOG"
printf '%s\\n' marker-A >> "$CHILD_LOG"
exit "${CHILD_RC:-0}"
"""

def archive(path, data=payload, name="sqlc", kind=None, mode=0o755):
    with tarfile.open(path, "w:gz", format=tarfile.USTAR_FORMAT) as tar:
        item = tarfile.TarInfo(name)
        item.mode = mode
        if kind:
            item.type = kind
            item.linkname = "target"
        else:
            item.size = len(data)
        tar.addfile(item, io.BytesIO(data) if not kind else None)

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

def write(path, text):
    path.write_text(text)
    path.chmod(0o755)

class Run:
    def __init__(self, name, mutation=None):
        self.dir = artifacts / name
        self.dir.mkdir()
        self.bin = self.dir / "bin"
        self.bin.mkdir()
        self.tmp = self.dir / "tmp"
        self.tmp.mkdir()
        self.fixture = self.dir / "a.tar.gz"
        archive(self.fixture)
        self.other = self.dir / "b.tar.gz"
        archive(self.other, payload.replace(b"marker-A", b"marker-B"))
        self.cache = self.dir / "cache"
        self.entry = self.cache / "1.31.1/linux-amd64/sqlc_1.31.1_linux_amd64.tar.gz"
        self.env = dict(os.environ, TMPDIR=str(self.tmp), UZI_SQLC_DIR=str(self.cache),
                        PATH=str(self.bin), FIXTURE=str(self.fixture),
                        CHILD_LOG=str(self.dir / "child"), TRANSPORT_LOG=str(self.dir / "transport"),
                        GO_LOG=str(self.dir / "go"), SLEEP_LOG=str(self.dir / "sleep"),
                        FAKE_OS="Linux", FAKE_ARCH="x86_64")
        # Isolated PATH lets the fallback cases exercise real shasum and wget.
        for name in ("sh", "mkdir", "mktemp", "rm", "cp", "mv", "rmdir", "wc",
                     "tr", "gzip", "tar", "sort", "cmp", "awk", "sha256sum", "shasum"):
            exe = shutil.which(name)
            if exe:
                (self.bin / name).symlink_to(exe)
        write(self.bin / "uname", '#!/bin/sh\ncase "$1" in -s) echo "$FAKE_OS";; -m) echo "$FAKE_ARCH";; esac\n')
        write(self.bin / "go", '#!/bin/sh\necho forbidden >> "$GO_LOG"\nexit 97\n')
        write(self.bin / "sleep", '#!/bin/sh\necho "$*" >> "$SLEEP_LOG"\n')
        transport = """#!/bin/sh
printf '%s\\n' "$*" >> "$TRANSPORT_LOG"
if [ "$1" = -fsSL ]; then
  [ "$2 $3 $4 $5 $6 $7 $8" = "--retry 3 --retry-delay 2 --retry-all-errors --retry-connrefused -o" ] || exit 91
  out="$9"
else
  [ "$1" = -qO ] || exit 92
  out="$2"
  [ ! -e "$out" ] || { echo partial-survived >> "$TRANSPORT_LOG"; exit 93; }
fi
[ "${TRANSPORT_FAIL:-0}" = 0 ] || { printf partial > "$out"; exit 7; }
if [ -n "${FAIL_FIRST:-}" ] && [ ! -e "$FAIL_FIRST" ]; then
  : > "$FAIL_FIRST"
  printf partial > "$out"
  exit 7
fi
cp "$FIXTURE" "$out"
"""
        write(self.bin / "curl", transport)
        write(self.bin / "wget", transport)
        self.text = source
        self.mutation = mutation
        self.script = self.dir / "sqlc.sh"

    def prepare(self):
        text = self.text
        for pin in pins:
            text = text.replace(pin, digest(self.fixture))
        if self.mutation:
            old, new = self.mutation
            assert old in text, "mutation target absent: " + old
            text = text.replace(old, new, 1)
        write(self.script, text)

    def run(self, argv=None, expected=0, diagnostic=None):
        self.prepare()
        result = subprocess.run([str(self.script)] + (["v1.31.1", "generate"] if argv is None else argv),
                                env=self.env, cwd=self.dir, capture_output=True, timeout=12)
        with (self.dir / "runs.log").open("ab") as log:
            log.write(result.stdout + result.stderr + ("\nEXIT=%s\n" % result.returncode).encode())
        assert result.returncode == expected, (result.returncode, expected, result.stderr)
        if diagnostic:
            assert diagnostic.encode() in result.stderr, result.stderr
        assert not (self.dir / "go").exists(), "Go invoked"
        assert not list(self.tmp.iterdir()), "private snapshot leaked"
        assert not list(self.cache.glob("**/.stage.*")), "publication stage leaked"
        return result

    def reject(self, diagnostic):
        self.run(expected=2, diagnostic=diagnostic)
        assert not (self.dir / "child").exists(), "unverified child executed"

def skip_failure(fragment):
    # Locate the exit belonging to one specific diagnostic.
    start = source.index(fragment)
    pos = source.index("exit 2", start)
    return (source[start:pos + 6], source[start:pos] + "exit 0")

class SkipCase(Exception):
    pass

cases = {}
def case(name, mutation):
    def add(fn):
        cases[name] = (fn, mutation)
        return fn
    return add

@case("missing-version", ('[ -z "$VERSION" ] || [ "$#" -lt 2 ]', 'false'))
def missing(r):
    r.run([], expected=2, diagnostic="usage:")

@case("missing-command", ('[ "$#" -lt 2 ]', 'false'))
def missing_command(r):
    r.run(["v1.31.1"], expected=2, diagnostic="usage:")

@case("version-mismatch", skip_failure("pinned archives are for"))
def mismatch(r):
    r.run(["v9", "generate"], expected=2, diagnostic="pinned archives")

@case("versions-disagree", skip_failure("per-arch versions disagree"))
def disagree(r):
    r.text = r.text.replace('SQLC_DARWIN_ARM64_VERSION="v1.31.1"', 'SQLC_DARWIN_ARM64_VERSION="v9"')
    r.reject("per-arch versions disagree")

@case("unsupported-host", skip_failure("unsupported host"))
def unsupported(r):
    r.env["FAKE_ARCH"] = "riscv64"
    r.reject("unsupported host")

@case("argv-cwd", ('"$BIN" "$@" || rc=$?', '"$BIN" "$*" || rc=$?'))
def argv(r):
    r.run(["v1.31.1", "generate", "two words", "", "*"])
    assert (r.dir / "child").read_text().splitlines() == [str(r.dir), "generate", "two words", "", "*", "marker-A"]

@case("child-status", ('exit "$rc"', 'exit 0'))
def child_rc(r):
    r.env["CHILD_RC"] = "37"
    r.run(expected=37)

@case("cold-warm", ('if [ -f "$ARCHIVE" ]; then', 'if false; then'))
def warm(r):
    r.run()
    r.env["TRANSPORT_FAIL"] = "1"
    r.run()
    assert len((r.dir / "transport").read_text().splitlines()) == 1

@case("darwin-shasum", ('shasum -a 256 -c - >/dev/null', 'true'))
def darwin(r):
    (r.bin / "sha256sum").unlink(missing_ok=True)
    r.env.update(FAKE_OS="Darwin", FAKE_ARCH="arm64")
    r.run()
    assert "sqlc_1.31.1_darwin_arm64.tar.gz" in (r.dir / "transport").read_text()
    shutil.rmtree(r.cache)
    (r.dir / "child").unlink()
    r.env["FIXTURE"] = str(r.other)
    r.reject("tarball checksum mismatch")

@case("linux-arm64-cache", ('asset="linux-arm64"', 'asset="linux-amd64"'))
def linux_arm64(r):
    for arch in ("aarch64", "arm64"):
        r.env.update(FAKE_OS="Linux", FAKE_ARCH=arch)
        r.run()
        assert "sqlc_1.31.1_linux_arm64.tar.gz" in (r.dir / "transport").read_text()
        assert (r.cache / "1.31.1/linux-arm64/sqlc_1.31.1_linux_arm64.tar.gz").is_file()
        r.env["TRANSPORT_FAIL"] = "1"
    assert len((r.dir / "transport").read_text().splitlines()) == 1

@case("linux-arm64-version", skip_failure("per-arch versions disagree"))
def arm64_version(r):
    r.text = r.text.replace('SQLC_LINUX_ARM64_VERSION="v1.31.1"', 'SQLC_LINUX_ARM64_VERSION="v0.0.0"')
    r.reject("per-arch versions disagree")

@case("download-checksum", ('verify_sha256 "$tar_sha256" "$CANDIDATE" \\',
                            'true \\'))
def download_hash(r):
    r.env["FIXTURE"] = str(r.other)
    r.reject("tarball checksum mismatch")

@case("cache-reverification", ('&& verify_sha256 "$tar_sha256" "$CANDIDATE"; then',
                              '&& true; then'))
def cache_hash(r):
    r.run()
    shutil.copyfile(r.other, r.entry)
    r.run()
    assert (r.dir / "child").read_text().endswith("marker-A\n")
    assert len((r.dir / "transport").read_text().splitlines()) == 2

@case("cache-corrupt-failure", skip_failure("bounded download failed"))
def corrupt_fail(r):
    r.entry.parent.mkdir(parents=True)
    shutil.copyfile(r.other, r.entry)
    r.env["TRANSPORT_FAIL"] = "1"
    r.reject("bounded download failed")

@case("curl-retries", ('--retry 3 --retry-delay 2', '--retry 2 --retry-delay 2'))
def curl_retry(r):
    r.run()
    log = (r.dir / "transport").read_text()
    assert "--retry 3 --retry-delay 2 --retry-all-errors --retry-connrefused" in log

@case("wget-recover", ('rm -f "$CANDIDATE"\n      if', ':\n      if'))
def wget_recover(r):
    (r.bin / "curl").unlink()
    r.env["FAIL_FIRST"] = str(r.dir / "first")
    r.run()
    assert len((r.dir / "transport").read_text().splitlines()) == 2
    assert (r.dir / "sleep").read_text() == "2\n"

@case("wget-exhaust", ('[ "$attempt" -ge 4 ]', '[ "$attempt" -ge 3 ]'))
def wget_exhaust(r):
    (r.bin / "curl").unlink()
    r.env["TRANSPORT_FAIL"] = "1"
    r.reject("bounded download failed")
    assert len((r.dir / "transport").read_text().splitlines()) == 4
    assert (r.dir / "sleep").read_text() == "2\n2\n2\n"

for name, member in [("extra-path", "extra"), ("absolute-path", "/sqlc"), ("traversal-path", "../sqlc")]:
    def paths(r, member=member):
        archive(r.fixture, name=member)
        r.reject("unexpected member paths")
    cases[name] = (paths, skip_failure("verified archive has unexpected member paths"))

@case("missing-member", skip_failure("verified archive has unexpected member paths"))
def missing_member(r):
    with tarfile.open(r.fixture, "w:gz"):
        pass
    r.reject("unexpected member paths")

for name, kind in [("symlink", tarfile.SYMTYPE), ("hardlink", tarfile.LNKTYPE), ("directory", tarfile.DIRTYPE),
                   ("fifo", tarfile.FIFOTYPE)]:
    def types(r, kind=kind):
        archive(r.fixture, kind=kind)
        # tar prints a trailing slash for directories, rejected by exact paths.
        r.reject("unexpected member paths" if kind == tarfile.DIRTYPE else "non-regular")
    diagnostic = "verified archive has unexpected member paths" if kind == tarfile.DIRTYPE else "verified archive contains a non-regular"
    cases[name] = (types, skip_failure(diagnostic))

@case("empty-binary", skip_failure("extracted binary has invalid size"))
def empty(r):
    archive(r.fixture, data=b"")
    r.reject("invalid size")

@case("not-executable", skip_failure("verified archive did not contain an executable"))
def not_exec(r):
    archive(r.fixture, mode=0o644)
    r.reject("executable")

@case("publication-cleanup", ('if [ -n "$STAGE" ]; then rm -rf "$STAGE"; fi', ':'))
def publication_cleanup(r):
    (r.bin / "mv").unlink()
    write(r.bin / "mv", '#!/bin/sh\nprintf \'%s\\n\' "$@" > "$MOVE_LOG"\nexit 1\n')
    r.env["MOVE_LOG"] = str(r.dir / "move")
    r.reject("cache publication failed")
    operands = (r.dir / "move").read_text().splitlines()
    assert operands[0] == "-f"
    assert Path(operands[1]).parent.parent == Path(operands[2]).parent
    assert Path(operands[1]).parent.name.startswith(".stage.")

@case("compressed-bound", ('[ "$archive_bytes" -le "$MAX_ARCHIVE_BYTES" ]', 'true'))
def compressed(r):
    r.text = r.text.replace("MAX_ARCHIVE_BYTES=33554432", "MAX_ARCHIVE_BYTES=32")
    r.reject("archive exceeds")

@case("download-ulimit", ('ulimit -f "$MAX_ARCHIVE_BLOCKS" && curl', ': && curl'))
def download_bound(r):
    r.text = r.text.replace("MAX_ARCHIVE_BLOCKS=32768", "MAX_ARCHIVE_BLOCKS=1")
    archive(r.fixture, data=payload + os.urandom(16384))
    r.reject("bounded download failed")

@case("cache-copy-bound", ('ulimit -f "$MAX_ARCHIVE_BLOCKS" && cp "$ARCHIVE"', ': && cp "$ARCHIVE"'))
def cache_bound(r):
    r.entry.parent.mkdir(parents=True)
    archive(r.fixture, data=payload + os.urandom(16384))
    shutil.copyfile(r.fixture, r.entry)
    r.text = r.text.replace("MAX_ARCHIVE_BLOCKS=32768", "MAX_ARCHIVE_BLOCKS=1")
    r.env["TRANSPORT_FAIL"] = "1"
    r.reject("bounded download failed")
    assert (r.dir / "transport").exists()

@case("decompressed-bound", ('[ "$tar_bytes" -le "$MAX_TAR_BYTES" ]', 'true'))
def decompressed(r):
    r.text = r.text.replace("MAX_TAR_BYTES=134217728", "MAX_TAR_BYTES=4096")
    r.reject("decompressed tar exceeds")

@case("decompression-ulimit", ('ulimit -f "$MAX_TAR_BLOCKS" && gzip', ': && gzip'))
def decompress_limit(r):
    r.text = r.text.replace("MAX_TAR_BLOCKS=131072", "MAX_TAR_BLOCKS=1")
    r.reject("archive exceeds or cannot produce")

@case("sparse-logical-bound", ('ulimit -f "$MAX_TAR_BLOCKS" && tar', ': && tar'))
def sparse(r):
    # GNU sparse tar is physically small but expands beyond the extraction cap.
    version = subprocess.run(["tar", "--version"], capture_output=True, text=True)
    if version.returncode != 0 or "GNU tar" not in version.stdout:
        raise SkipCase("GNU tar --format=gnu --sparse fixture capabilities unavailable")
    sparse_file = r.dir / "sqlc"
    with sparse_file.open("wb") as f:
        f.write(payload)
        f.seek(4 * 1024 * 1024)
        f.write(b"\n")
    sparse_file.chmod(0o755)
    tar = subprocess.check_output(["tar", "--format=gnu", "--sparse", "-cf", "-", "-C", str(r.dir), "sqlc"])
    r.fixture.write_bytes(gzip.compress(tar))
    r.text = r.text.replace("MAX_TAR_BLOCKS=131072", "MAX_TAR_BLOCKS=128")
    r.reject("extraction failed")

@case("ulimit-fail-closed", ('ulimit -f "$MAX_ARCHIVE_BLOCKS" && curl',
                           'ulimit -f "$MAX_ARCHIVE_BLOCKS"; curl'))
def ulimit_failure(r):
    r.text = r.text.replace("MAX_ARCHIVE_BLOCKS=32768", "MAX_ARCHIVE_BLOCKS=not-a-number")
    r.reject("bounded download failed")
    assert not (r.dir / "transport").exists(), "transport ran after limit failure"

for name, sig, status in [("interrupt", signal.SIGINT, 130), ("terminate", signal.SIGTERM, 143)]:
    def signals(r, sig=sig, status=status):
        # The child announces readiness and waits for a release file. The test
        # bounds readiness and completion, and owns both process handles.
        body = b'#!/bin/sh\n: > "$CHILD_LOG"\nwhile [ ! -e "$RELEASE" ]; do /bin/sleep 0.02; done\n'
        archive(r.fixture, data=body)
        r.env["RELEASE"] = str(r.dir / "release")
        r.prepare()
        with (r.dir / "runs.log").open("wb") as log:
            proc = subprocess.Popen([str(r.script), "v1.31.1", "generate"], env=r.env, cwd=r.dir,
                                    stdout=log, stderr=log)
            try:
                deadline = time.monotonic() + 5
                while not (r.dir / "child").exists():
                    assert time.monotonic() < deadline and proc.poll() is None, "readiness timeout"
                    time.sleep(.01)
                proc.send_signal(sig)
                (r.dir / "release").touch()
                assert proc.wait(timeout=5) == status
                assert not list(r.tmp.iterdir()), "signal leaked snapshot"
            finally:
                (r.dir / "release").touch()
                if proc.poll() is None:
                    proc.kill()
                    proc.wait()
    cases[name] = (signals, ("trap cleanup 0", "trap : 0"))

selected = [args.case] if args.case else list(cases)
for name in selected:
    fn, mutation = cases[name]
    try:
        fn(Run(name))
    except SkipCase as error:
        print("SKIP " + name + ": " + str(error), flush=True)
        continue
    print("PASS " + name, flush=True)
    if args.mutations:
        try:
            fn(Run(name + "-mutation", mutation))
        except (AssertionError, subprocess.SubprocessError) as error:
            (artifacts / (name + ".mutation.txt")).write_text(str(error))
            print("REJECTED mutation " + name, flush=True)
        else:
            raise AssertionError("SURVIVED mutation " + name)
print("sqlc wrapper tests: PASS")
PY
