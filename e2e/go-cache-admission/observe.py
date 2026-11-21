"""Issue 4575 M2: bounded, sanitized, read-only Docker environment collector.

    python3 -B -I e2e/go-cache-admission/observe.py --endpoint <tcp://127.0.0.1:PORT | /abs/unix.sock>
        [--image sha256:<id>] [--go-root <abs>] [--go-binary <abs>] [--toolchain-profile <abs file>]
        [--repo-revision <40hex>] [--scaffold-revision <40hex>] [--head-revision <40hex>]
        [--out-dir <path under .uzi/scratch>] >/dev/null 2>&1

It issues only GET /version, /info, /images/json and /images/sha256:<id>/json through
preflight.docker_get, reads no environment variable, and creates no container. What it observes
(daemon capability report, image metadata) goes through qualify.collect(); everything else in the
artifact (charge map, claims, envelope) is a declared proposal that stays file-tier. It collects no
mount, fd or runtime facts, so the verdict is NO-QUALIFIED-ENVIRONMENT.

Output lifecycle: the out-dir (exactly `go-cache-env`, or `go-cache-env-<[a-z0-9-]{1,32}>`, directly under
.uzi/scratch) is deleted and recreated first, and only when it holds nothing but run.log, artifact.json and
verdict.json (regular files); any other content is a refusal that deletes nothing; then run.log is opened and dup2'd over
fds 1 and 2, so the original stdio carries nothing (the caller should point it at /dev/null).
stdout is capped at 256 bytes (one summary line), stderr at 4 KiB; excess is dropped and counted.
Exit codes: 2 not qualified (the normal result), 0 never in this issue, 3 refusal or failure,
4 run.log is no longer the file on fd 1/2.
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:  # python -I leaves the script directory off sys.path
    sys.path.insert(0, HERE)

import argparse
import copy
import hashlib
import http.client
import json
import re
import stat
import time
import traceback
from pathlib import Path

import preflight as p
import qualify as q

STDOUT_CAP = 256
STDERR_CAP = 4096
EXIT_NO_QUALIFIED = 2
EXIT_REFUSED = 3
EXIT_RUN_LOG = 4
LOG_NAME = "run.log"
DOC_NAMES = ("artifact.json", "verdict.json")
ENTRY_CAP = 3
MAX_IMAGES = 64
MAX_BINARY = 512*p.MIB
COLLECT_SECONDS = 90
OUT_NAME = re.compile(r"go-cache-env(?:-[a-z0-9-]{1,32})?")
DEFAULT_OUT = p.SCRATCH/"go-cache-env"
OFFICIAL = frozenset(("postgres", "hello-world"))
OFFICIAL_PREFIX = "docker.io/library/"
NOTE = "no Go-bearing image verified (only image names and metadata listed; no image filesystem inspected)"

_COMP = r"[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*"
_DOM = r"(?:[a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])"
HOST_RE = re.compile(_DOM + r"(?:\." + _DOM + r")*(?::[0-9]+)?")
PATH_RE = re.compile(_COMP + "(?:/" + _COMP + ")*")
TAG_RE = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}")
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}")
IMAGE_ID_RE = DIGEST_RE
NIX_RE = re.compile(r"/nix/store/[a-z0-9]{32}-[A-Za-z0-9+._?=-]+")
TOKEN_RE = re.compile(r"[a-z0-9][a-z0-9._-]{0,31}")
SEMVER_RE = re.compile(r"\d+\.\d+\.\d+")
KERNEL_RE = re.compile(r"(\d+\.\d+\.\d+(?:-\d+)?)((?:\.el\d+(?:_\d+)?)?(?:\.x86_64|\.aarch64)?)")
KERNEL_PREFIX_RE = re.compile(r"\d+\.\d+\.\d+(?:-\d+)?")
HOST_MARKS = ("uzi-", ".internal")
TYPE_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._/+-]{0,63}")
GO_PATH_RE = re.compile(r"(?<![\w.-])/usr/local/go(?![\w.-])|(?<![\w.-])/go/bin(?![\w.-])")

HOST_ERRORS = (ValueError, OSError, KeyError, TypeError, OverflowError, RecursionError)
FETCH_ERRORS = (ValueError, OSError, http.client.HTTPException, RecursionError)


class Refusal(Exception):
    """A fixed reason code; the message is the code and carries no input."""
    def __init__(self, code):
        super().__init__(code)
        self.code = code


# ---- bounded output -------------------------------------------------------------------------

class BoundedSink:
    """Writes to a file descriptor up to `cap` bytes in total; the rest is dropped and counted."""
    def __init__(self, fd, cap):
        self.fd, self.cap, self.used, self.dropped = fd, cap, 0, 0

    def write(self, data):
        keep = data[:max(self.cap-self.used, 0)]
        self.dropped += len(data)-len(keep)
        view = memoryview(keep)
        while view:
            try:
                n = os.write(self.fd, view)
            except OSError:
                self.dropped += len(view)
                break
            self.used += n
            view = view[n:]


def validate_out_dir(raw):
    """The out-dir is pinned: its basename matches OUT_NAME and its parent is exactly .uzi/scratch."""
    try:
        path = Path(raw)
        if not path.is_absolute():
            path = p.ROOT/path
        p.require(path.parent == p.SCRATCH and OUT_NAME.fullmatch(path.name))
        return p.scratch_path(str(path))
    except (ValueError, OSError, TypeError):
        raise Refusal("out-dir-invalid") from None


def _open_scratch():
    """A descriptor of .uzi/scratch walked component by component from the filesystem root with
    O_NOFOLLOW, so a symlink swapped in anywhere below the root is refused rather than followed."""
    fd = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in p.SCRATCH.parts[1:]:
            nxt = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
            os.close(fd)
            fd = nxt
    except BaseException:
        os.close(fd)
        raise
    return fd


class OutDir:
    """The report directory: run.log plus at most the two JSON documents, nothing else."""
    def __init__(self, path, dfd):
        self.path, self.dfd, self.log_fd = path, dfd, None

    @classmethod
    def reset(cls, raw):
        """Validate, delete and recreate the directory, all relative to a no-follow descriptor of
        .uzi/scratch. An existing directory is removed only when every entry is a regular file the
        tool writes (run.log, artifact.json, verdict.json); anything else is a refusal that deletes
        nothing."""
        path = validate_out_dir(raw)
        name = path.name
        try:
            sfd = _open_scratch()
        except OSError:
            raise Refusal("out-dir-invalid") from None
        try:
            try:
                try:
                    st = os.stat(name, dir_fd=sfd, follow_symlinks=False)
                except FileNotFoundError:
                    st = None
                if st is not None:
                    if not stat.S_ISDIR(st.st_mode):
                        raise Refusal("out-dir-invalid")
                    old = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=sfd)
                    try:
                        entries = os.listdir(old)
                        for entry in entries:
                            kind = os.stat(entry, dir_fd=old, follow_symlinks=False).st_mode
                            if entry not in (LOG_NAME,)+DOC_NAMES or not stat.S_ISREG(kind):
                                raise Refusal("out-dir-invalid")
                        for entry in entries:
                            os.unlink(entry, dir_fd=old)
                    finally:
                        os.close(old)
                    os.rmdir(name, dir_fd=sfd)
                os.mkdir(name, 0o700, dir_fd=sfd)
                dfd = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=sfd)
            except OSError:
                raise Refusal("out-dir-invalid") from None
        finally:
            os.close(sfd)
        return cls(path, dfd)

    def _create(self, name):
        if len(os.listdir(self.dfd)) >= ENTRY_CAP:
            raise Refusal("report-limit")
        try:
            return os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=self.dfd)
        except OSError:
            raise Refusal("report-limit") from None

    def open_log(self):
        if self.log_fd is not None:
            raise Refusal("report-limit")
        self.log_fd = self._create(LOG_NAME)
        return self.log_fd

    def redirect(self):
        """Replace fds 1 and 2 with run.log; the original stdio is no longer reachable."""
        os.dup2(self.log_fd, 1)
        os.dup2(self.log_fd, 2)
        if self.log_fd not in (1, 2):
            os.close(self.log_fd)
        self.log_fd = None

    def write_doc(self, name, value):
        if name not in DOC_NAMES:
            raise Refusal("report-limit")
        try:
            raw = p.encode(value)
        except (ValueError, TypeError):
            raise Refusal("report-limit") from None
        data = raw.encode()+b"\n"
        if len(data) > p.MAX_JSON or name in os.listdir(self.dfd):
            raise Refusal("report-limit")
        fd = self._create(name)
        try:
            view = memoryview(data)
            while view:
                view = view[os.write(fd, view):]
        finally:
            os.close(fd)

    def verify_log(self, fds=(1, 2)):
        """Whether every descriptor in `fds` is still the (linked) run.log of this directory."""
        try:
            want = os.stat(LOG_NAME, dir_fd=self.dfd, follow_symlinks=False)
            if not stat.S_ISREG(want.st_mode) or want.st_nlink < 1:
                return False
            for fd in fds:
                got = os.fstat(fd)
                if (got.st_dev, got.st_ino) != (want.st_dev, want.st_ino) or got.st_nlink < 1:
                    return False
        except OSError:
            return False
        return True

    def measure(self):
        vfs = os.statvfs(self.dfd)
        files = {}
        for name in (LOG_NAME,)+DOC_NAMES:
            try:
                st = os.stat(name, dir_fd=self.dfd, follow_symlinks=False)
                files[name] = dict(apparent=st.st_size, allocated=st.st_blocks*512)
            except OSError:
                files[name] = dict(apparent=0, allocated=0)
        return dict(f_frsize=vfs.f_frsize, f_bsize=vfs.f_bsize, files=files)


# ---- sanitization ---------------------------------------------------------------------------

def safe(value, pattern):
    return value if type(value) is str and pattern.fullmatch(value) else None


def valid_name(name):
    """OCI repository name: an optional registry host (contains a dot or a port, or is localhost),
    then lowercase path components."""
    head, sep, rest = name.partition("/")
    if sep and ("." in head or ":" in head or head == "localhost"):
        return bool(HOST_RE.fullmatch(head) and PATH_RE.fullmatch(rest))
    return bool(PATH_RE.fullmatch(name))


def parse_ref(ref, digest_form):
    """(name, digest-or-None), "skip" for Docker's <none> placeholder, None for a malformed ref."""
    if type(ref) is str and ref in ("<none>@<none>", "<none>:<none>"):
        return "skip"
    if (type(ref) is not str or len(ref) > 256 or not ref.isascii()
            or any(ord(c) < 32 or ord(c) == 127 for c in ref)):
        return None
    digest = None
    name = ref
    if digest_form:
        name, sep, digest = ref.partition("@")
        if not sep or not DIGEST_RE.fullmatch(digest):
            return None
    head, sep, tag = name.rpartition(":")
    if sep and "/" not in tag:
        if not TAG_RE.fullmatch(tag):
            return None
        name = head
    return (name, digest) if valid_name(name) else None


def public_name(name, salt):
    if name in OFFICIAL or (name.startswith(OFFICIAL_PREFIX) and name[len(OFFICIAL_PREFIX):] in OFFICIAL):
        return name
    return "repo-"+hashlib.sha256(salt+name.encode()).hexdigest()[:12]


def go_declared(config):
    """Metadata only: whether Env/PATH/Cmd/Entrypoint mention a Go toolchain location."""
    if not isinstance(config, dict):
        return False
    texts = []
    env = config.get("Env")
    for item in (env[:128] if type(env) is list else []):
        if type(item) is not str or len(item) > 4096:
            continue
        key, _, value = item.partition("=")
        if key in ("GOROOT", "GOPATH"):
            return True
        if key == "PATH":
            texts.append(value)
    for field in ("Cmd", "Entrypoint"):
        value = config.get(field)
        for item in (value[:64] if type(value) is list else [value]):
            if type(item) is str and len(item) <= 4096:
                texts.append(item)
    return any(GO_PATH_RE.search(t) for t in texts)


def host_free(value):
    """A daemon string that names an uzi host or an internal domain is refused outright."""
    return not (type(value) is str and any(mark in value.lower() for mark in HOST_MARKS))


def dsafe(value, pattern):
    return safe(value, pattern) if host_free(value) else None


def kernel_version(raw):
    """Numeric kernel release; an optional .elN and architecture suffix survive, anything else after
    the numeric prefix is dropped."""
    if type(raw) is not str or len(raw) > 128 or not host_free(raw):
        return None
    whole = KERNEL_RE.fullmatch(raw)
    if whole:
        return raw
    prefix = KERNEL_PREFIX_RE.match(raw)
    return prefix.group(0) if prefix else None


def runc_version(info):
    runtimes = info.get("Runtimes")
    runc = runtimes.get("runc") if isinstance(runtimes, dict) else None
    if not isinstance(runc, dict):
        return None
    annotations = runc.get("annotations")
    if not isinstance(annotations, dict):
        status = runc.get("status")
        features = status.get("org.opencontainers.runtime-spec.features") if isinstance(status, dict) else None
        if type(features) is not str or len(features) > 8192:
            return None
        try:
            doc = json.loads(features)
        except ValueError:
            return None
        annotations = doc.get("annotations") if isinstance(doc, dict) else None
    if not isinstance(annotations, dict):
        return None
    return annotations.get("org.opencontainers.runc.version")


def tri(value):
    """True or False as reported; None when the daemon did not report a boolean at all."""
    return value if type(value) is bool else None


def daemon_section(version, info, counts=None):
    """Allowlisted daemon fields only; Name, ID, DockerRootDir, labels and paths are never copied.
    A version string that is reduced or dropped is counted in counts["daemon"] when given."""
    v = version if isinstance(version, dict) else {}
    i = info if isinstance(info, dict) else {}
    options = i.get("SecurityOptions")
    names = {o.split(",")[0] for o in (options[:32] if type(options) is list else [])
             if type(o) is str and len(o) <= 256}
    driver_type = None
    status = i.get("DriverStatus")
    for pair in (status[:32] if type(status) is list else []):
        if type(pair) is list and len(pair) == 2 and pair[0] == "driver-type":
            driver_type = dsafe(pair[1], TYPE_RE)
            break
    server_raw = i.get("ServerVersion") or v.get("Version")
    kernel_raw = i.get("KernelVersion") or v.get("KernelVersion")
    runc_raw = runc_version(i)
    server = dsafe(server_raw, SEMVER_RE)
    kernel = kernel_version(kernel_raw)
    runc = dsafe(runc_raw, SEMVER_RE)
    if counts is not None:
        counts["daemon"] += sum(1 for raw, got in ((server_raw, server), (kernel_raw, kernel), (runc_raw, runc))
                                if raw is not None and raw != got)
    return dict(
        ServerVersion=server,
        Os=dsafe(v.get("Os"), TOKEN_RE), Arch=dsafe(v.get("Arch"), TOKEN_RE),
        KernelVersion=kernel,
        Driver=dsafe(i.get("Driver"), TOKEN_RE), DriverType=driver_type,
        CgroupVersion=dsafe(i.get("CgroupVersion"), re.compile(r"[12]")),
        CgroupDriver=dsafe(i.get("CgroupDriver"), TOKEN_RE),
        MemoryLimit=tri(i.get("MemoryLimit")), SwapLimit=tri(i.get("SwapLimit")),
        PidsLimit=tri(i.get("PidsLimit")),
        LoggingDriver=dsafe(i.get("LoggingDriver"), TOKEN_RE),
        DefaultRuntime=dsafe(i.get("DefaultRuntime"), TOKEN_RE), RuncVersion=runc,
        rootless="name=rootless" in names, seccomp="name=seccomp" in names)


# ---- host-only Go coordinates ---------------------------------------------------------------

def _open_regular(raw, limit):
    path = os.path.realpath(raw)
    p.require(os.path.isabs(path))
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        st = os.fstat(fd)
        p.require(stat.S_ISREG(st.st_mode) and st.st_size <= limit)
    except BaseException:
        os.close(fd)
        raise
    return fd


def hash_binary(raw):
    """sha256 and size of a regular file of at most 512 MiB; the path is not recorded."""
    fd = _open_regular(raw, MAX_BINARY)
    try:
        digest, total, started = hashlib.sha256(), 0, time.monotonic()
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            total += len(chunk)
            p.require(total <= MAX_BINARY and time.monotonic()-started < 60)
            digest.update(chunk)
        return digest.hexdigest(), total
    finally:
        os.close(fd)


def first_line(raw):
    fd = _open_regular(raw, 1 << 20)
    try:
        data = os.read(fd, 4097)
    finally:
        os.close(fd)
    try:
        return data.split(b"\n", 1)[0].decode("ascii")
    except UnicodeDecodeError:
        return ""


def host_coordinates(go_root, go_binary, profile):
    """Host-side Go facts, labelled host_only; None when no input was given. An input that was
    given but cannot be read is a refusal, never a silent omission."""
    if not (go_root or go_binary or profile):
        return None
    go = dict(host_only=True, observed_in="host")
    try:
        if go_root:
            inv = p.inventory(go_root)
            go.update(version=inv["version"], version_sha256=inv["version_sha256"],
                      source_sha256=inv["sha256"], inventory_files=inv["files"], inventory_bytes=inv["bytes"])
        if go_binary:
            go["sha256"], go["size"] = hash_binary(go_binary)
        if profile:
            line = first_line(profile)
            go["toolchain_profile"] = line if NIX_RE.fullmatch(line) else None
            go["toolchain_profile_dropped"] = not NIX_RE.fullmatch(line)
    except HOST_ERRORS:
        raise Refusal("host-input-invalid") from None
    return go


# ---- the observer ---------------------------------------------------------------------------

class LiveObserver:
    """The observer seam of qualify.collect(): observe() returns the daemon and binding sections,
    daemon_id() the daemon's own ID (stored by collect() only as a salted hash)."""
    def __init__(self, endpoint, image=None, host=None, revisions=None, salt=None, monotonic=time.monotonic):
        self.endpoint, self.image, self.host = endpoint, image, host
        self.revisions = revisions or {}
        self.salt = os.urandom(16) if salt is None else salt
        self.monotonic = monotonic
        self.sections, self._id = None, None

    def _get(self, route):
        try:
            return p.docker_get(self.endpoint, route)
        except FETCH_ERRORS:
            raise Refusal("docker-unavailable") from None

    def _record(self, image_id, meta, counts, id_kind):
        refs = [(ref, True) for ref in (meta.get("RepoDigests") if type(meta.get("RepoDigests")) is list else [])[:32]]
        refs += [(ref, False) for ref in (meta.get("RepoTags") if type(meta.get("RepoTags")) is list else [])[:32]]
        repos, digests = set(), set()
        for ref, digest_form in refs:
            got = parse_ref(ref, digest_form)
            if got == "skip":
                continue
            if got is None:
                counts["refs"] += 1
                continue
            repos.add(public_name(got[0], self.salt))
            if got[1]:
                digests.add(got[1])
        config = meta.get("Config")
        return dict(id=image_id, id_kind=id_kind, architecture=safe(meta.get("Architecture"), TOKEN_RE),
                    os=safe(meta.get("Os"), TOKEN_RE), digests=sorted(digests)[:8], repos=sorted(repos)[:8],
                    go_declared=go_declared(config))

    def observe(self):
        deadline = self.monotonic()+COLLECT_SECONDS
        counts = dict(refs=0, ids=0, daemon=0)
        version, info = self._get("/version"), self._get("/info")
        ident = info.get("ID") if isinstance(info, dict) else None
        if type(ident) is not str or not 0 < len(ident) <= 256:
            raise Refusal("daemon-id-missing")
        self._id = ident
        daemon = daemon_section(version, info, counts)
        # Under the containerd snapshotter the image Id is the index digest, else the config digest.
        id_kind = "index" if daemon["DriverType"] == "io.containerd.snapshotter.v1" else "config"
        listing = self._get("/images/json")
        if type(listing) is not list:
            raise Refusal("docker-unavailable")
        if len(listing) > MAX_IMAGES:
            raise Refusal("too-many-images")
        ids = []
        for item in listing:
            image_id = item.get("Id") if isinstance(item, dict) else None
            if type(image_id) is str and IMAGE_ID_RE.fullmatch(image_id):
                if image_id not in ids:
                    ids.append(image_id)
            else:
                counts["ids"] += 1
        if self.image is not None:
            if self.image not in ids:
                raise Refusal("image-not-in-listing")
            ids = [self.image]
        records = []
        for image_id in ids:
            if self.monotonic() > deadline:
                raise Refusal("docker-unavailable")
            meta = self._get("/images/"+image_id+"/json")
            if not isinstance(meta, dict) or meta.get("Id") != image_id:
                counts["ids"] += 1
                continue
            records.append(self._record(image_id, meta, counts, id_kind))
        binding = dict(self.revisions)
        binding.update(go_declared=any(r["go_declared"] for r in records), images=records, note=NOTE,
                       dropped_repo_refs=counts["refs"], dropped_image_ids=counts["ids"], dropped_daemon_strings=counts["daemon"])
        if self.host is not None:
            binding["go"] = dict(self.host)
        sections = dict(daemon=daemon, binding=binding)
        self.sections = copy.deepcopy(sections)
        return sections

    def daemon_id(self):
        return self._id


# ---- the declared proposal (file tier; nothing below was observed) ---------------------------

W_SINKS = p.SINKS + ("toolchain_download", "cache_init", "fuzz", "exported_archives", "linked_executables",
                     "stdout_stderr", "harness_reports", "setup_cleanup")
OUTER = q.OUTER_EXEMPT


def declared_sinks():
    sinks = {}

    def add(name, paths, superblock, nbytes, inodes, domain):
        sinks[name] = dict(identity=name, paths=paths, mount_id=None, superblock=superblock, bytes=nbytes,
                           inodes=inodes, backing_domain=domain)
    for name in W_SINKS:
        add(name, ["/w/"+name], "declared:w-tmpfs", 128*1024, 8, "declared:memory")
    add("container_writable_layer", ["/"], "declared:rootfs-ro", 0, 1, "declared:rootfs")
    add("container_network_binds", ["/etc/hosts", "/etc/hostname", "/etc/resolv.conf"],
        "declared:network-binds", 4096, 3, "declared:rootfs")
    add("shm", ["/dev/shm"], "declared:dev-shm", 64*1024, 8, "declared:memory")
    add("volumes", [], "declared:no-volumes", 0, 1, "declared:none")
    for name in OUTER:
        sinks[name] = dict(identity=name, paths=[], mount_id=None, bytes=0, inodes=1,
                           backing_domain="declared:daemon-host-fs", outer_domain=True)
    sinks["temp_extraction"] = dict(identity="temp_extraction", parent="tmp")
    return sinks


def declared_claims():
    w = list(W_SINKS)
    return [dict(kind="tmpfs-size", resource="bytes", sinks=w, superblock="declared:w-tmpfs"),
            dict(kind="tmpfs-nr_inodes", resource="inodes", sinks=w, superblock="declared:w-tmpfs"),
            dict(kind="tmpfs-size", resource="bytes", sinks=["shm"], superblock="declared:dev-shm"),
            dict(kind="readonly-rootfs", resource="write-deny", sinks=["container_writable_layer"]),
            dict(kind="pids-max", resource="pids", sinks=["process_tree"]),
            dict(kind="log-driver-none", resource="logs", sinks=["container_logs"]),
            dict(kind="memcg-max-eq-swap", resource="memory-swap", sinks=["memory"])]


def declared_proposal():
    return dict(
        label="declared-not-observed",
        rootfs="read-only",
        mounts=[dict(point="/w", fstype="tmpfs", size="4m", nr_inodes=256),
                dict(point="/dev", fstype="tmpfs", size=None, nr_inodes=None),
                dict(point="/dev/shm", fstype="tmpfs", size=None, nr_inodes=None),
                dict(point="/dev/mqueue", fstype="mqueue", writable=True)],
        limits=["pids-max", "log-driver-none", "memcg-max-eq-swap"],
        kernel_memory=("every mount, read-only and non-storage ones included, keeps its kernel memory "
                       "(tmpfs pages, dentry and inode slab) charged to the memory domain"),
        notes=["declared ceilings only; no mountinfo, fd or runtime fact was collected",
               "/dev and /dev/shm carry no nr_inodes bound and /dev/mqueue is writable: expected to stay uncovered",
               "daemon-side sinks carry placeholder ceilings; the backing leg needs observed capacity"])


def declared_envelope():
    return dict(limits=dict(p.LIMITS),
                inode_ceiling=dict(value=65536, total=65536, retained=8192, staging=1024, work=56320,
                                   owner_approved=False))


# ---- the run --------------------------------------------------------------------------------

def build_reports(args, clock):
    now = clock()
    host = host_coordinates(args.go_root, args.go_binary, args.toolchain_profile)
    revisions = {k: v for k, v in (("repo_revision", args.repo_revision),
                                   ("scaffold_revision", args.scaffold_revision)) if v}
    observer = LiveObserver(args.endpoint, args.image, host, revisions)
    evidence = q.collect(observer)
    artifact = dict(schema=q.SCHEMA, observed_at=int(now), binding=observer.sections["binding"],
                    daemon=observer.sections["daemon"], charge_map=dict(sinks=declared_sinks()),
                    claims=declared_claims(), envelope=declared_envelope(), proposal=declared_proposal())
    verdict = q.evaluate(artifact, collector=evidence, now=now, head_revision=args.head_revision)
    binding = artifact["binding"]
    verdict["observation"] = dict(
        images=len(binding["images"]), dropped_repo_refs=binding["dropped_repo_refs"],
        dropped_image_ids=binding["dropped_image_ids"],
        dropped_daemon_strings=binding["dropped_daemon_strings"], host_only_go=host is not None, note=NOTE)
    return artifact, verdict


def write_reports(od, artifact, verdict):
    od.write_doc("artifact.json", artifact)
    m = od.measure()
    verdict["observation"]["out_dir"] = dict(f_frsize=m["f_frsize"], f_bsize=m["f_bsize"])
    verdict["observation"]["artifact"] = m["files"]["artifact.json"]
    od.write_doc("verdict.json", verdict)


def trace_text(exc):
    """Exception type and innermost frames only; the exception message is never printed."""
    frames = traceback.extract_tb(exc.__traceback__)[-6:]
    where = " < ".join("%s:%d:%s" % (os.path.basename(f.filename), f.lineno, f.name) for f in reversed(frames))
    return ("error type=%s frames=%s\n" % (type(exc).__name__, where)).encode("ascii", "replace")


def summary(verdict, code, reason, od, err_dropped):
    parts = ["verdict=%s" % verdict, "exit=%d" % code, "reason=%s" % reason]
    if od is not None:
        m = od.measure()
        f = m["files"]
        parts += ["frsize=%d" % m["f_frsize"], "bsize=%d" % m["f_bsize"]]
        parts += ["%s=%d/%d" % (tag, f[name]["apparent"], f[name]["allocated"])
                  for tag, name in (("art", "artifact.json"), ("ver", "verdict.json"), ("log", LOG_NAME))]
        parts.append("err_dropped=%d" % err_dropped)
    return (" ".join(parts)+"\n").encode("ascii", "replace")


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        raise Refusal("usage")


def _revision(value):
    if not re.fullmatch(r"[0-9a-f]{40}", value):
        raise argparse.ArgumentTypeError("revision")
    return value


def _image_id(value):
    if not IMAGE_ID_RE.fullmatch(value):
        raise argparse.ArgumentTypeError("image")
    return value


def _absolute(value):
    if not os.path.isabs(value):
        raise argparse.ArgumentTypeError("path")
    return value


def _endpoint(value):
    if not (p.TCP_ENDPOINT.fullmatch(value) or os.path.isabs(value)):
        raise argparse.ArgumentTypeError("endpoint")
    return value


def parse_args(argv):
    parser = _Parser(prog="observe.py", add_help=False, allow_abbrev=False)
    parser.add_argument("--endpoint", required=True, type=_endpoint)
    parser.add_argument("--image", type=_image_id)
    parser.add_argument("--go-root", type=_absolute)
    parser.add_argument("--go-binary", type=_absolute)
    parser.add_argument("--toolchain-profile", type=_absolute)
    parser.add_argument("--repo-revision", type=_revision)
    parser.add_argument("--scaffold-revision", type=_revision)
    parser.add_argument("--head-revision", type=_revision)
    parser.add_argument("--out-dir", default=str(DEFAULT_OUT))
    return parser.parse_args(argv)


def refuse(out, err, code):
    err.write(("observe: refused %s\n" % code).encode())
    out.write(summary("none", EXIT_REFUSED, code, None, 0))
    return EXIT_REFUSED


def main(argv=None, *, clock=time.time, hook=None):
    """Run one collection. Redirects fds 1 and 2 to run.log, so call it only from a process that
    owns its stdio. `hook(out, err)`, when given, runs after the redirect with the bounded writers."""
    out, err = BoundedSink(1, STDOUT_CAP), BoundedSink(2, STDERR_CAP)
    try:
        args = parse_args(sys.argv[1:] if argv is None else list(argv))
        validate_out_dir(args.out_dir)
    except Refusal as refusal:
        return refuse(out, err, refusal.code)
    try:
        od = OutDir.reset(args.out_dir)
        od.open_log()
        od.redirect()
    except (Refusal, OSError) as failure:
        return refuse(out, err, getattr(failure, "code", "out-dir-invalid"))
    previous = sys.excepthook
    sys.excepthook = lambda kind, exc, tb: err.write(trace_text(exc))
    code, reason, verdict_name = EXIT_REFUSED, "internal-error", "none"
    try:
        if hook is not None:
            hook(out, err)
        artifact, verdict = build_reports(args, clock)
        write_reports(od, artifact, verdict)
        verdict_name = verdict["verdict"]
        code, reason = (0, "qualified") if verdict["qualified"] else (EXIT_NO_QUALIFIED, "not-qualified")
    except Refusal as refusal:
        reason = refusal.code
    except Exception as exc:
        err.write(trace_text(exc))
    finally:
        sys.excepthook = previous
    if not od.verify_log((1, 2)):
        code, reason = EXIT_RUN_LOG, "run-log-mismatch"
    if code not in (0, EXIT_NO_QUALIFIED):
        err.write(("observe: refused %s\n" % reason).encode())
    if out.dropped:
        err.write(("stdout_dropped=%d\n" % out.dropped).encode())
    out.write(summary(verdict_name, code, reason, od, err.dropped))
    return code


if __name__ == "__main__":
    raise SystemExit(main())
