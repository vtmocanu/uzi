"""Fixture-only tests for observe.py. Every Docker daemon here is an owned local stub (loopback TCP
or AF_UNIX) serving canned JSON; nothing touches a real Docker endpoint, container or image.
The collector redirects fds 1 and 2, so it only ever runs in a subprocess (`python3 -B -I`)."""
import hashlib
import json
import os
import re
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from pathlib import Path

import observe as o
import preflight as p
import qualify as q

HERE = Path(__file__).resolve().parent
OBSERVE = HERE/"observe.py"
EVIDENCE = HERE/"evidence"
MAX_DOC = 65536
NOTE = "no Go-bearing image verified (only image names and metadata listed; no image filesystem inspected)"
ARTIFACT_KEYS = {"schema", "observed_at", "binding", "daemon", "charge_map", "claims", "envelope", "proposal"}
VERDICT_KEYS = {"schema", "scope", "qualified", "verdict", "producer_launched", "production_authorized",
                "legs", "collector", "observation"}
DAEMON_KEYS = {"ServerVersion", "Os", "Arch", "KernelVersion", "Driver", "DriverType", "CgroupVersion",
               "CgroupDriver", "MemoryLimit", "SwapLimit", "PidsLimit", "LoggingDriver", "DefaultRuntime",
               "RuncVersion", "rootless", "seccomp"}
ALLOWED_ROUTES = {"/version", "/info", "/images/json"}
IMAGE_ROUTE = re.compile(r"/images/sha256:[0-9a-f]{64}/json")
SECRETS = ("DAEMON-ID-SECRET-0123", "uzi-hw-secret-host", "/srv/secret-root", "secret-label",
           "secret-proxy", "secret-registry-token")


def iid(char):
    return "sha256:" + char*64


def ok(body):
    raw = json.dumps(body).encode()
    return b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n" % len(raw) + raw


def info(**over):
    value = dict(
        ID="DAEMON-ID-SECRET-0123", Name="uzi-hw-secret-host", DockerRootDir="/srv/secret-root/docker",
        ServerVersion="27.3.1", Driver="overlay2",
        DriverStatus=[["driver-type", "io.containerd.snapshotter.v1"], ["Backing Filesystem", "extfs"]],
        CgroupVersion="2", CgroupDriver="systemd", MemoryLimit=True, SwapLimit=True, PidsLimit=True,
        LoggingDriver="json-file", DefaultRuntime="runc", KernelVersion="6.1.0-test",
        Runtimes={"runc": {"path": "runc", "status": {"org.opencontainers.runtime-spec.features": json.dumps(
            {"annotations": {"org.opencontainers.runc.version": "1.2.5"}})}}},
        SecurityOptions=["name=seccomp,profile=builtin", "name=cgroupns"],
        Labels=["owner=secret-label"], HttpProxy="http://secret-proxy:3128")
    value.update(over)
    return value


VERSION = dict(Version="27.3.1", Os="linux", Arch="amd64", KernelVersion="6.1.0-test", ApiVersion="1.47",
               Name="uzi-hw-secret-host")


def image(char, tags=(), digests=(), env=(), cmd=None, entry=None, arch="amd64"):
    return dict(Id=iid(char), Architecture=arch, Os="linux", RepoTags=list(tags), RepoDigests=list(digests),
                Config=dict(Env=list(env), Cmd=cmd, Entrypoint=entry))


def routes_for(images, daemon_info=None, version=None):
    routes = {"/version": ok(version or VERSION), "/info": ok(daemon_info or info()),
              "/images/json": ok([dict(Id=m["Id"], RepoTags=m["RepoTags"]) for m in images])}
    for m in images:
        routes["/images/%s/json" % m["Id"]] = ok(m)
    return routes


class StubDaemon:
    """Bounded stub: one request per connection, a recorded (method, path) list, a 60 s lifetime."""
    def __init__(self, routes, unix_path=None):
        self.routes, self.requests, self.stop = routes, [], threading.Event()
        if unix_path:
            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
            self.sock.bind(unix_path)
            self.endpoint = unix_path
        else:
            self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            self.sock.bind(("127.0.0.1", 0))
            self.endpoint = "tcp://127.0.0.1:%d" % self.sock.getsockname()[1]
        self.sock.listen(8)
        self.sock.settimeout(0.2)
        self.thread = threading.Thread(target=self.serve)
        self.thread.start()

    def serve(self):
        deadline = time.monotonic()+60
        while not self.stop.is_set() and time.monotonic() < deadline:
            try:
                conn = self.sock.accept()[0]
            except socket.timeout:
                continue
            except OSError:
                break
            with conn:
                conn.settimeout(2)
                data = b""
                try:
                    while b"\r\n\r\n" not in data and len(data) < 8192:
                        chunk = conn.recv(1024)
                        if not chunk:
                            break
                        data += chunk
                    method, path = data.split(b" ")[:2]
                    self.requests.append((method.decode(), path.decode()))
                    conn.sendall(self.routes.get(path.decode(),
                                                 b"HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
                except (OSError, ValueError):
                    pass

    def close(self):
        self.stop.set()
        self.thread.join(5)
        self.sock.close()


def read_doc(path):
    """Bounded JSON read with duplicate-key rejection, independent of the code under test."""
    st = os.lstat(path)
    assert stat.S_ISREG(st.st_mode) and st.st_size <= MAX_DOC, path
    raw = Path(path).read_bytes()
    def pairs(items):
        out = {}
        for k, v in items:
            assert k not in out, k
            out[k] = v
        return out
    return json.loads(raw, object_pairs_hook=pairs)


class Base(unittest.TestCase):
    def setUp(self):
        p.SCRATCH.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="observe-", dir=p.SCRATCH)
        self.root = Path(self.temp.name)
        self.addCleanup(self.temp.cleanup)
        self.out = self.outdir()

    def outdir(self):
        """A fresh, valid out-dir name directly under SCRATCH; removed with the test."""
        path = p.SCRATCH/("go-cache-env-t"+os.urandom(6).hex())
        self.addCleanup(shutil.rmtree, path, True)
        self.addCleanup((path.parent/("."+path.name+".lock")).unlink, missing_ok=True)
        return path

    def stub(self, images, **kw):
        daemon = StubDaemon(routes_for(images, **kw))
        self.addCleanup(daemon.close)
        return daemon

    def observe(self, endpoint, *extra, env=None, out="default", **kw):
        argv = [sys.executable, "-B", "-I", str(OBSERVE)]
        if endpoint:
            argv += ["--endpoint", endpoint]
        if out == "default":
            argv += ["--out-dir", str(self.out)]
        elif out:
            argv += ["--out-dir", str(out)]
        return subprocess.run(argv + list(extra), stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                              stderr=subprocess.PIPE, env=env or {}, timeout=60, **kw)

    def texts(self):
        return {n: (self.out/n).read_text() for n in sorted(os.listdir(self.out))}

    def docs(self):
        return read_doc(self.out/"artifact.json"), read_doc(self.out/"verdict.json")

    def leg(self, name):
        return self.docs()[1]["legs"][name]

    def go(self, images, *extra, **kw):
        daemon = self.stub(images, **kw)
        result = self.observe(daemon.endpoint, *extra)
        return daemon, result

    def hook_run(self, code):
        """Run observe.main in a subprocess with a hook defined by `code` (a body using out, err, outdir)."""
        script = ("import sys\nsys.path.insert(0, %r)\nimport observe\n"
                  "def hook(out, err):\n    outdir = %r\n%s\n"
                  "sys.exit(observe.main(%r, hook=hook))\n")
        daemon = self.stub([image("a")])
        argv = ["--endpoint", daemon.endpoint, "--out-dir", str(self.out)]
        body = "\n".join("    " + line for line in code.splitlines())
        return subprocess.run([sys.executable, "-B", "-I", "-c", script % (str(HERE), str(self.out), body, argv)],
                              stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                              env={}, timeout=60)

    def log(self):
        return (self.out/"run.log").read_text()


def check_report_shape(test, artifact, verdict):
    """Shape and content invariants shared by a fresh run and the committed evidence."""
    test.assertEqual(set(artifact), ARTIFACT_KEYS)
    test.assertEqual(set(verdict), VERDICT_KEYS)
    test.assertEqual(artifact["schema"], q.SCHEMA)
    test.assertEqual(verdict["verdict"], "NO-QUALIFIED-ENVIRONMENT")
    test.assertIs(verdict["qualified"], False)
    test.assertIs(verdict["producer_launched"], False)
    test.assertIs(verdict["production_authorized"], False)
    test.assertEqual(set(artifact["daemon"]) - {"source"}, DAEMON_KEYS)


class SanitizationTests(Base):
    def test_private_registry_and_host_port_refs_become_stable_pseudonyms(self):
        d = "registry.internal.example:5000/team/img"
        images = [image("1", tags=[d+":latest"], digests=[d+"@"+iid("9")]),
                  image("2", tags=["localhost:5000/other:1", "localhost:5000/other:2"]),
                  image("3", tags=["10.1.2.3:443/x/y:v1"])]
        daemon, r = self.go(images)
        self.assertEqual((r.returncode, r.stdout, r.stderr), (2, b"", b""), r)
        artifact, _ = self.docs()
        recs = {rec["id"]: rec for rec in artifact["binding"]["images"]}
        for char in "123":
            names = recs[iid(char)]["repos"]
            self.assertEqual(len(names), 1, names)
            self.assertRegex(names[0], r"^repo-[0-9a-f]{12}$")
        self.assertEqual(recs[iid("1")]["digests"], [iid("9")])
        for text in self.texts().values():
            for leaked in ("registry.internal", "team/img", "localhost", "10.1.2.3", ":5000"):
                self.assertNotIn(leaked, text)

    def test_official_names_kept_and_digests_split(self):
        images = [image("1", tags=["postgres:16"], digests=["postgres@"+iid("7")]),
                  image("2", tags=["docker.io/library/hello-world:latest"],
                        digests=["docker.io/library/hello-world@"+iid("8")]),
                  image("3", tags=["docker.io/library/postgres-evil:1", "library/postgres:1"])]
        daemon, r = self.go(images)
        self.assertEqual(r.returncode, 2, r)
        recs = {rec["id"]: rec for rec in self.docs()[0]["binding"]["images"]}
        self.assertEqual(recs[iid("1")]["repos"], ["postgres"])
        self.assertEqual(recs[iid("1")]["digests"], [iid("7")])
        self.assertEqual(recs[iid("2")]["repos"], ["docker.io/library/hello-world"])
        self.assertEqual(recs[iid("2")]["digests"], [iid("8")])
        for name in recs[iid("3")]["repos"]:
            self.assertRegex(name, r"^repo-[0-9a-f]{12}$")
        self.assertEqual(recs[iid("1")]["architecture"], "amd64")
        self.assertEqual(recs[iid("1")]["os"], "linux")

    def test_image_architecture_and_os_use_the_exact_allowlists_and_are_counted(self):
        meta = dict(image("1"), Architecture="host9.example.com", Os="box.example.org")
        counts = dict(refs=0, ids=0, daemon=0)
        rec = o.LiveObserver("tcp://127.0.0.1:1", salt=b"s"*16)._record(iid("1"), meta, counts, "config")
        self.assertEqual((rec["architecture"], rec["os"], counts["daemon"]), (None, None, 2))
        counts = dict(refs=0, ids=0, daemon=0)
        rec = o.LiveObserver("tcp://127.0.0.1:1", salt=b"s"*16)._record(iid("1"), image("1"), counts, "config")
        self.assertEqual((rec["architecture"], rec["os"], counts["daemon"]), ("amd64", "linux", 0))
        daemon, r = self.go([dict(image("1"), Architecture="host9.example.com")])
        self.assertEqual(r.returncode, 2, r)
        binding = self.docs()[0]["binding"]
        self.assertIsNone(binding["images"][0]["architecture"])
        # One for the fixture's reduced KernelVersion ("6.1.0-test"), one for the image Architecture.
        self.assertEqual(binding["dropped_daemon_strings"], 2)
        for text in self.texts().values():
            self.assertNotIn("host9", text)

    def test_malformed_refs_are_dropped_and_counted(self):
        bad = [image("1", tags=["bad\x00name:1", "UPPER/case:1", "x"*300+":1", "ok.example/x:bad tag"],
                     digests=["good@sha256:short", "good@"+iid("5")+"x"])]
        daemon, r = self.go(bad)
        self.assertEqual(r.returncode, 2, r)
        binding = self.docs()[0]["binding"]
        self.assertEqual(binding["dropped_repo_refs"], 6)
        self.assertEqual(binding["images"][0]["repos"], [])
        self.assertEqual(binding["images"][0]["digests"], [])
        for text in self.texts().values():
            self.assertNotIn("UPPER", text)
            self.assertNotIn("bad\\u0000", text)

    def test_docker_none_placeholders_are_not_counted_as_dropped(self):
        daemon, r = self.go([image("1", tags=["<none>:<none>"], digests=["<none>@<none>"])])
        self.assertEqual(r.returncode, 2, r)
        self.assertEqual(self.docs()[0]["binding"]["dropped_repo_refs"], 0)

    def test_more_than_64_images_refused(self):
        listing = [dict(Id="sha256:%064x" % n, RepoTags=[]) for n in range(65)]
        routes = routes_for([])
        routes["/images/json"] = ok(listing)
        daemon = StubDaemon(routes)
        self.addCleanup(daemon.close)
        r = self.observe(daemon.endpoint)
        self.assertEqual(r.returncode, 3, r)
        self.assertEqual(sorted(os.listdir(self.out)), ["run.log"])
        self.assertIn("too-many-images", self.log())
        self.assertFalse([path for _, path in daemon.requests if path.startswith("/images/sha256:")])

    def test_exactly_64_images_are_inspected(self):
        listing = [dict(Id="sha256:%064x" % n, RepoTags=[]) for n in range(64)]
        routes = routes_for([])
        routes["/images/json"] = ok(listing)
        for item in listing:
            routes["/images/%s/json" % item["Id"]] = ok(dict(Id=item["Id"], Architecture="arm64", Os="linux",
                                                            RepoTags=[], RepoDigests=[], Config={}))
        daemon = StubDaemon(routes)
        self.addCleanup(daemon.close)
        r = self.observe(daemon.endpoint)
        self.assertEqual(r.returncode, 2, r)
        self.assertEqual(len(self.docs()[0]["binding"]["images"]), 64)

    def test_daemon_identity_strings_never_appear(self):
        images = [image("1", tags=["postgres:16"], env=["TOKEN=secret-registry-token", "PATH=/usr/bin"])]
        daemon, r = self.go(images)
        self.assertEqual(r.returncode, 2, r)
        blob = "".join(self.texts().values()) + r.stdout.decode() + r.stderr.decode()
        for secret in SECRETS:
            self.assertNotIn(secret, blob)
        artifact, verdict = self.docs()
        self.assertEqual(set(artifact["daemon"]) - {"source"}, DAEMON_KEYS)
        self.assertEqual(artifact["daemon"]["ServerVersion"], "27.3.1")
        self.assertEqual(artifact["daemon"]["DriverType"], "io.containerd.snapshotter.v1")
        self.assertEqual(artifact["daemon"]["RuncVersion"], "1.2.5")
        self.assertIs(artifact["daemon"]["rootless"], False)
        self.assertIs(artifact["daemon"]["seccomp"], True)
        # The identity is a salted hash: not the bare sha256 of the ID, and fresh per invocation.
        digest = verdict["collector"]["daemon_id_hash"]
        self.assertRegex(digest, r"^[0-9a-f]{64}$")
        self.assertNotEqual(digest, hashlib.sha256(b"DAEMON-ID-SECRET-0123").hexdigest())
        first = digest
        self.assertEqual(self.observe(daemon.endpoint).returncode, 2)
        self.assertNotEqual(self.docs()[1]["collector"]["daemon_id_hash"], first)

    def test_hostile_daemon_values_are_dropped_and_never_echoed(self):
        hostile = info(ServerVersion="27.3.1 uzi-hw-secret-host", KernelVersion="x"*200, Driver="a b",
                       CgroupDriver=["uzi-hw-secret-host"], DefaultRuntime="runc\nsecret-label")
        daemon, r = self.go([image("1")], daemon_info=hostile)
        self.assertEqual(r.returncode, 2, r)
        daemon_doc = self.docs()[0]["daemon"]
        for key in ("ServerVersion", "KernelVersion", "Driver", "CgroupDriver", "DefaultRuntime"):
            self.assertIsNone(daemon_doc[key], key)
        blob = "".join(self.texts().values())
        for secret in SECRETS:
            self.assertNotIn(secret, blob)

    def test_daemon_version_strings_reduce_to_numeric_forms(self):
        def runtimes(version):
            return {"runc": {"status": {"org.opencontainers.runtime-spec.features": json.dumps(
                {"annotations": {"org.opencontainers.runc.version": version}})}}}

        def ds(**over):
            return o.daemon_section(VERSION, info(**over))
        real = ds(ServerVersion="29.8.0", KernelVersion="6.12.0-243.el10.x86_64", Runtimes=runtimes("1.5.1"))
        self.assertEqual((real["ServerVersion"], real["KernelVersion"], real["RuncVersion"]),
                         ("29.8.0", "6.12.0-243.el10.x86_64", "1.5.1"))
        for kernel in ("6.12.0", "6.12.0-243", "5.14.0-503.el9_4.aarch64", "6.12.0-243.el10"):
            self.assertEqual(ds(KernelVersion=kernel)["KernelVersion"], kernel, kernel)
        for bad in ("29.8.0+uzi-hw-abc123", "29.8.0-rc.1", "29.8", "v29.8.0", "29.8.0 ", "29.8.0\n"):
            with self.subTest(server=bad):
                self.assertIsNone(ds(ServerVersion=bad)["ServerVersion"])
        for bad in ("1.5.1-uzi-hw-x", "1.5", "1.5.1+abc", "1.5.1.internal"):
            with self.subTest(runc=bad):
                self.assertIsNone(ds(Runtimes=runtimes(bad))["RuncVersion"])
        for raw, want in (("6.12.0-uzi-hw-worker7-prod.internal", None), ("6.12.0-243.uzi-host", None),
                          ("6.12.0-243.host.internal", None), ("6.1.0-test", "6.1.0"),
                          ("6.12.0-243.el10.x86_64.worker7", "6.12.0-243"), ("6.12.0-243.worker7", "6.12.0-243"),
                          ("x"*200, None), ("6.12", None)):
            with self.subTest(kernel=raw):
                self.assertEqual(ds(KernelVersion=raw)["KernelVersion"], want)
        for key in ("Driver", "DefaultRuntime", "LoggingDriver"):
            self.assertIsNone(ds(**{key: "runc-uzi-hw-x"})[key])
        # The /version fallback is held to the same rules.
        i = info()
        del i["ServerVersion"]
        self.assertIsNone(o.daemon_section(dict(VERSION, Version="29.8.0+uzi-hw-abc123"), i)["ServerVersion"])

    def test_non_version_daemon_fields_use_exact_allowlists_and_are_counted(self):
        probes = dict(DefaultRuntime="ci-runner-7.corp.example", LoggingDriver="fluentd-a.lan",
                      CgroupDriver="host9.example.com")
        for key, bad in probes.items():
            with self.subTest(key=key):
                self.assertIsNone(o.daemon_section(VERSION, info(**{key: bad}))[key])
        hostile = info(KernelVersion="6.1.0", DefaultRuntime=probes["DefaultRuntime"], LoggingDriver=probes["LoggingDriver"],
                       CgroupDriver=probes["CgroupDriver"],
                       DriverStatus=[["driver-type", "buildhost.example.org/snap"]])
        counts = dict(daemon=0)
        section = o.daemon_section(VERSION, hostile, counts)
        self.assertIsNone(section["DriverType"])
        self.assertEqual(counts["daemon"], 4)
        counts = dict(daemon=0)
        section = o.daemon_section(dict(VERSION, Os="box.example.org", Arch="cpu7.lan"), info(KernelVersion="6.1.0"), counts)
        self.assertEqual((section["Os"], section["Arch"], counts["daemon"]), (None, None, 2))
        daemon, r = self.go([image("1")], daemon_info=hostile)
        self.assertEqual(r.returncode, 2, r)
        artifact, verdict = self.docs()
        self.assertEqual(artifact["binding"]["dropped_daemon_strings"], 4)
        self.assertEqual(verdict["observation"]["dropped_daemon_strings"], 4)
        blob = "".join(self.texts().values())
        for leaked in ("example", "fluentd-a", "ci-runner", ".lan"):
            self.assertNotIn(leaked, blob)
        # Real values pass unchanged and are not counted.
        counts = dict(daemon=0)
        real = o.daemon_section(VERSION, info(KernelVersion="6.1.0"), counts)
        self.assertEqual(counts["daemon"], 0)
        self.assertEqual((real["CgroupDriver"], real["CgroupVersion"], real["LoggingDriver"], real["DefaultRuntime"],
                          real["Driver"], real["DriverType"], real["Os"], real["Arch"]),
                         ("systemd", "2", "json-file", "runc", "overlay2", "io.containerd.snapshotter.v1",
                          "linux", "amd64"))
        for key, good in (("CgroupDriver", "cgroupfs"), ("Driver", "overlayfs")):
            self.assertEqual(o.daemon_section(VERSION, info(**{key: good}))[key], good)

    def test_hyphenated_hostile_versions_never_reach_the_reports_and_are_counted(self):
        hostile = info(ServerVersion="29.8.0+uzi-hw-abc123", KernelVersion="6.12.0-uzi-hw-worker7-prod.internal",
                       Runtimes={"runc": {"status": {"org.opencontainers.runtime-spec.features": json.dumps(
                           {"annotations": {"org.opencontainers.runc.version": "1.5.1-uzi-hw-x"}})}}})
        daemon, r = self.go([image("1")], daemon_info=hostile)
        self.assertEqual(r.returncode, 2, r)
        artifact, verdict = self.docs()
        for key in ("ServerVersion", "KernelVersion", "RuncVersion"):
            self.assertIsNone(artifact["daemon"][key], key)
        blob = "".join(self.texts().values())
        for leaked in ("uzi-hw", "abc123", "worker7", ".internal"):
            self.assertNotIn(leaked, blob)
        self.assertEqual(artifact["binding"]["dropped_daemon_strings"], 3)
        self.assertEqual(verdict["observation"]["dropped_daemon_strings"], 3)
        daemon, r = self.go([image("1")], daemon_info=info(KernelVersion="6.12.0-243.el10.x86_64"))
        self.assertEqual(self.docs()[0]["binding"]["dropped_daemon_strings"], 0)

    def test_absent_limit_facts_are_null_not_false(self):
        i = info()
        for key in ("MemoryLimit", "SwapLimit", "PidsLimit"):
            del i[key]
        section = o.daemon_section(VERSION, i)
        for key in ("MemoryLimit", "SwapLimit", "PidsLimit"):
            self.assertIsNone(section[key], key)
        self.assertIs(o.daemon_section(VERSION, info(SwapLimit=False))["SwapLimit"], False)
        self.assertIs(o.daemon_section(VERSION, info(SwapLimit="yes"))["SwapLimit"], None)
        daemon, r = self.go([image("1")], daemon_info=i)
        self.assertEqual(r.returncode, 2, r)
        leg = self.leg("enforcement")
        self.assertEqual(leg["status"], "UNVERIFIED")
        self.assertIn("daemon-info-missing", leg["reasons"])
        self.assertNotIn("daemon-capability-unsupported", leg["reasons"])

    def test_missing_daemon_id_is_refused(self):
        bad = info()
        del bad["ID"]
        daemon, r = self.go([image("1")], daemon_info=bad)
        self.assertEqual(r.returncode, 3, r)
        self.assertIn("daemon-id-missing", self.log())


class BindingTests(Base):
    def test_go_declared_comes_from_metadata_and_go_absent_is_never_emitted(self):
        images = [image("1", env=["PATH=/usr/local/go/bin:/usr/bin"]),
                  image("2", env=["GOPATH=/go"]),
                  image("3", cmd=["/usr/local/go/bin/go", "version"]),
                  image("4", env=["PATH=/usr/bin", "NOTE=/usr/local/go/bin"], cmd=["sh"], entry=["/usr/local/golang"]),
                  image("5")]
        daemon, r = self.go(images)
        self.assertEqual(r.returncode, 2, r)
        artifact, verdict = self.docs()
        declared = {rec["id"]: rec["go_declared"] for rec in artifact["binding"]["images"]}
        self.assertEqual(declared, {iid("1"): True, iid("2"): True, iid("3"): True, iid("4"): False,
                                    iid("5"): False})
        self.assertIs(artifact["binding"]["go_declared"], True)
        binding = verdict["legs"]["binding"]
        self.assertNotEqual(binding["status"], "PROVEN")
        self.assertIn("no-go-bearing-image-verified", binding["reasons"])
        self.assertIn("go-host-only", binding["reasons"])
        blob = "".join(self.texts().values())
        self.assertNotIn("go_absent", blob)
        self.assertNotIn("go-absent-verified", blob)
        self.assertEqual(artifact["binding"]["note"], NOTE)
        self.assertEqual(verdict["observation"]["note"], NOTE)

    def test_image_id_kind_follows_the_snapshotter(self):
        daemon, r = self.go([image("1"), image("2")])
        self.assertEqual(r.returncode, 2, r)
        self.assertEqual({rec["id_kind"] for rec in self.docs()[0]["binding"]["images"]}, {"index"})
        overlay = info(DriverStatus=[["Backing Filesystem", "extfs"], ["driver-type", "io.containerd.overlayfs"]])
        daemon, r = self.go([image("1")], daemon_info=overlay)
        self.assertEqual(r.returncode, 2, r)
        self.assertEqual([rec["id_kind"] for rec in self.docs()[0]["binding"]["images"]], ["config"])
        legacy = info(DriverStatus=[["Backing Filesystem", "extfs"]])
        daemon, r = self.go([image("1")], daemon_info=legacy)
        self.assertEqual([rec["id_kind"] for rec in self.docs()[0]["binding"]["images"]], ["config"])
        self.assertIn("binding-source-unbound", self.leg("binding")["reasons"])

    def test_no_declared_go_is_go_not_declared_never_absent(self):
        daemon, r = self.go([image("1", tags=["postgres:16"])])
        self.assertEqual(r.returncode, 2, r)
        artifact, verdict = self.docs()
        self.assertIs(artifact["binding"]["go_declared"], False)
        reasons = verdict["legs"]["binding"]["reasons"]
        self.assertIn("go-not-declared", reasons)
        self.assertIn("no-go-bearing-image-verified", reasons)
        self.assertNotIn("go-absent-verified", reasons)

    def test_host_only_go_coordinates(self):
        goroot = self.root/"goroot"
        (goroot/"src/hello").mkdir(parents=True)
        (goroot/"VERSION").write_text("go1.25.0\ntime 2025-08-01\n")
        (goroot/"src/hello/hello.go").write_text("package hello\n")
        binary = self.root/"gobin"
        binary.write_bytes(b"fake-go-binary")
        profile = self.root/"profile"
        nix = "/nix/store/" + "a"*32 + "-go-1.25.0-env"
        profile.write_text(nix + "\nsecond-line-secret\n")
        revs = ("--repo-revision", "a"*40, "--scaffold-revision", "b"*40, "--head-revision", "a"*40)
        daemon, r = self.go([image("1", env=["PATH=/usr/local/go/bin"])],
                            "--go-root", str(goroot), "--go-binary", str(binary),
                            "--toolchain-profile", str(profile), *revs)
        self.assertEqual(r.returncode, 2, r)
        artifact, verdict = self.docs()
        go = artifact["binding"]["go"]
        self.assertIs(go["host_only"], True)
        self.assertEqual(go["observed_in"], "host")
        self.assertEqual(go["sha256"], hashlib.sha256(b"fake-go-binary").hexdigest())
        self.assertEqual(go["size"], len(b"fake-go-binary"))
        self.assertEqual(go["version"], "go1.25.0")
        self.assertEqual(go["source_sha256"], p.inventory(str(goroot))["sha256"])
        self.assertEqual((go["inventory_files"], go["toolchain_profile"]), (1, nix))
        self.assertEqual((artifact["binding"]["repo_revision"], artifact["binding"]["scaffold_revision"]),
                         ("a"*40, "b"*40))
        self.assertIn("go-host-only", verdict["legs"]["binding"]["reasons"])
        self.assertNotEqual(verdict["legs"]["binding"]["status"], "PROVEN")
        blob = "".join(self.texts().values())
        for hidden in (str(binary), str(goroot), "second-line-secret"):
            self.assertNotIn(hidden, blob)

    def test_non_nix_toolchain_profile_is_dropped(self):
        profile = self.root/"profile"
        profile.write_text("/home/someone/secret-profile\n")
        daemon, r = self.go([image("1")], "--toolchain-profile", str(profile))
        self.assertEqual(r.returncode, 2, r)
        go = self.docs()[0]["binding"]["go"]
        self.assertIsNone(go["toolchain_profile"])
        self.assertIs(go["toolchain_profile_dropped"], True)
        self.assertNotIn("secret-profile", "".join(self.texts().values()))

    def test_invalid_host_inputs_are_refused(self):
        binary = self.root/"notgo"
        binary.mkdir()
        for flag, value in (("--go-binary", "/dev/null"), ("--go-binary", str(binary)),
                            ("--go-binary", str(self.root/"missing")), ("--go-root", str(self.root/"nogoroot")),
                            ("--toolchain-profile", "/dev/null")):
            with self.subTest(flag=flag, value=value):
                daemon, r = self.go([image("1")], flag, value)
                self.assertEqual(r.returncode, 3, r)
                self.assertIn("host-input-invalid", self.log())
                self.assertEqual(sorted(os.listdir(self.out)), ["run.log"])

    def test_image_narrowing(self):
        images = [image("1", tags=["postgres:16"]), image("2", tags=["hello-world:latest"])]
        daemon = self.stub(images)
        r = self.observe(daemon.endpoint, "--image", iid("2"))
        self.assertEqual(r.returncode, 2, r)
        self.assertEqual([rec["id"] for rec in self.docs()[0]["binding"]["images"]], [iid("2")])
        inspected = [path for _, path in daemon.requests if path.startswith("/images/sha256:")]
        self.assertEqual(set(inspected), {"/images/%s/json" % iid("2")})
        before = len(daemon.requests)
        r = self.observe(daemon.endpoint, "--image", iid("9"))
        self.assertEqual(r.returncode, 3, r)
        self.assertIn("image-not-in-listing", self.log())
        self.assertFalse([path for _, path in daemon.requests[before:] if path.startswith("/images/sha256:")])
        self.assertEqual(self.observe(daemon.endpoint, "--image", "latest").returncode, 3)


class TransportTests(Base):
    def test_only_get_on_the_four_routes(self):
        daemon, r = self.go([image("1"), image("2")])
        self.assertEqual(r.returncode, 2, r)
        self.assertTrue(daemon.requests)
        for method, path in daemon.requests:
            self.assertEqual(method, "GET")
            self.assertTrue(path in ALLOWED_ROUTES or IMAGE_ROUTE.fullmatch(path), path)
            self.assertNotIn("/containers", path)
        self.assertEqual({path for _, path in daemon.requests} & ALLOWED_ROUTES, ALLOWED_ROUTES)

    def test_environment_is_never_read_for_the_endpoint(self):
        daemon = self.stub([image("1")])
        env = {"DOCKER_HOST": "tcp://127.0.0.1:9", "DOCKER_CONTEXT": "x"}
        r = self.observe(None, env=env)
        self.assertEqual(r.returncode, 3, r)
        self.assertIn(b"reason=usage", r.stdout)
        self.assertFalse(self.out.exists())
        self.assertEqual(daemon.requests, [])
        r = self.observe(daemon.endpoint, env=env)
        self.assertEqual(r.returncode, 2, r)
        self.assertTrue(daemon.requests)

    def test_usage_errors_refuse_with_exit_3(self):
        for extra in (["--endpoint", "tcp://localhost:2375"], ["--endpoint", "relative.sock"],
                      ["--endpoint", "tcp://127.0.0.1:1", "--bogus"], ["--endpoint", "tcp://127.0.0.1:1", "--end", "x"],
                      ["--endpoint", "tcp://127.0.0.1:1", "--head-revision", "abc"],
                      ["--endpoint", "tcp://127.0.0.1:1", "--go-root", "relative"]):
            with self.subTest(extra=extra):
                r = subprocess.run([sys.executable, "-B", "-I", str(OBSERVE), "--out-dir", str(self.out)] + extra,
                                   stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   env={}, timeout=60)
                self.assertEqual(r.returncode, 3, r)
                self.assertIn(b"reason=usage", r.stdout)
                self.assertFalse(self.out.exists())

    def test_unix_socket_endpoint(self):
        socket_dir = tempfile.mkdtemp(prefix="gco-", dir="/tmp")
        self.addCleanup(shutil.rmtree, socket_dir, True)
        os.chmod(socket_dir, 0o700)
        daemon = StubDaemon(routes_for([image("1")]), unix_path=os.path.join(socket_dir, "s"))
        self.addCleanup(daemon.close)
        r = self.observe(daemon.endpoint)
        self.assertEqual(r.returncode, 2, r)
        self.assertTrue(daemon.requests)

    def test_unreachable_endpoint_is_a_refusal(self):
        probe = socket.socket()
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
        probe.close()
        r = self.observe("tcp://127.0.0.1:%d" % port)
        self.assertEqual(r.returncode, 3, r)
        self.assertIn("docker-unavailable", self.log())

    def test_non_200_and_oversized_responses_are_refused(self):
        routes = routes_for([image("1")])
        routes["/info"] = b"HTTP/1.1 500 Err\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"
        daemon = StubDaemon(routes)
        self.addCleanup(daemon.close)
        self.assertEqual(self.observe(daemon.endpoint).returncode, 3)
        self.assertIn("docker-unavailable", self.log())
        routes["/info"] = ok(dict(info(), Pad="x"*70000))
        self.assertEqual(self.observe(daemon.endpoint).returncode, 3)
        self.assertIn("docker-unavailable", self.log())


class VerdictTests(Base):
    def test_verdict_and_artifact_shape(self):
        daemon, r = self.go([image("1", tags=["postgres:16"])], daemon_info=None)
        self.assertEqual((r.returncode, r.stdout, r.stderr), (2, b"", b""), r)
        artifact, verdict = self.docs()
        check_report_shape(self, artifact, verdict)
        legs = verdict["legs"]
        self.assertEqual(set(legs), set(q.LEGS))
        self.assertEqual((legs["threshold"]["status"], legs["threshold"]["reasons"]),
                         ("UNVERIFIED", ["owner-input-not-supplied"]))
        self.assertEqual((legs["charge_map"]["status"], legs["charge_map"]["reasons"]),
                         ("UNVERIFIED", ["no-container-mount-facts"]))
        self.assertEqual(legs["aliasing"]["reasons"], ["no-container-mount-facts"])
        self.assertEqual((legs["enforcement"]["status"], legs["enforcement"]["reasons"]),
                         ("UNVERIFIED", ["no-container-mount-facts"]))
        self.assertEqual(legs["envelope"]["reasons"], ["inode-ceiling-unapproved"])
        self.assertEqual(legs["canary"]["status"], "UNVERIFIED")
        self.assertEqual(legs["backing"]["reasons"], ["backing-soft-unknown"])
        self.assertNotIn("PROVEN", {leg["status"] for leg in legs.values()})
        self.assertNotIn("threshold", artifact)
        self.assertEqual(artifact["envelope"]["inode_ceiling"],
                         dict(value=65536, total=65536, retained=8192, staging=1024, work=56320,
                              owner_approved=False))
        self.assertEqual(artifact["envelope"]["limits"], p.LIMITS)
        self.assertEqual(set(artifact["charge_map"]["sinks"]), set(q.CHARGE_SINKS))
        kinds = {c["kind"] for c in artifact["claims"]}
        self.assertEqual(kinds, {"readonly-rootfs", "tmpfs-size", "tmpfs-nr_inodes", "pids-max",
                                 "log-driver-none", "memcg-max-eq-swap"})
        proposal = artifact["proposal"]
        self.assertEqual({m["point"] for m in proposal["mounts"]}, {"/w", "/dev", "/dev/shm", "/dev/mqueue"})
        self.assertIn("kernel memory", proposal["kernel_memory"])
        self.assertNotIn("mountinfo", artifact)
        self.assertNotIn("runtime", artifact)
        # Fed back through the file path the declared artifact reaches at most FIXTURE-CONSISTENT.
        again = q.evaluate(artifact, now=artifact["observed_at"])
        self.assertFalse(again["qualified"])
        self.assertNotIn("PROVEN", {leg["status"] for leg in again["legs"].values()})

    def test_daemon_capability_follows_the_info_report(self):
        daemon, r = self.go([image("1")], daemon_info=info(Rootless=True, CgroupDriver="cgroupfs",
                                                          SecurityOptions=["name=rootless", "name=seccomp,profile=builtin"]))
        self.assertEqual(r.returncode, 2, r)
        self.assertIs(self.docs()[0]["daemon"]["rootless"], True)
        leg = self.leg("enforcement")
        self.assertEqual((leg["status"], leg["reason"]), ("NEGATIVE", "daemon-capability-unsupported"))
        daemon, r = self.go([image("1")], daemon_info=info(SwapLimit=False))
        self.assertEqual(self.leg("enforcement")["reason"], "daemon-capability-unsupported")
        daemon, r = self.go([image("1")])
        self.assertEqual(self.leg("enforcement")["status"], "UNVERIFIED")
        self.assertEqual(self.leg("enforcement")["reasons"], ["no-container-mount-facts"])


class LifecycleTests(Base):
    def test_run_log_is_the_only_output_channel(self):
        daemon, r = self.go([image("1")])
        self.assertEqual((r.returncode, r.stdout, r.stderr), (2, b"", b""), r)
        self.assertEqual(sorted(os.listdir(self.out)), ["artifact.json", "run.log", "verdict.json"])
        text = self.log()
        self.assertLessEqual(len(text.encode()), o.STDOUT_CAP)
        self.assertEqual(text.count("\n"), 1)
        self.assertRegex(text, r"^verdict=NO-QUALIFIED-ENVIRONMENT exit=2 reason=not-qualified frsize=\d+ bsize=\d+ "
                               r"art=\d+/\d+ ver=\d+/\d+ log=\d+/\d+ err_dropped=0\n$")
        vfs = os.statvfs(self.out)
        observation = self.docs()[1]["observation"]
        self.assertEqual(observation["out_dir"], dict(f_frsize=vfs.f_frsize, f_bsize=vfs.f_bsize))
        size = (self.out/"artifact.json").stat()
        self.assertEqual(observation["artifact"]["apparent"], size.st_size)
        self.assertIsInstance(observation["artifact"]["allocated"], int)
        self.assertGreaterEqual(observation["artifact"]["allocated"], 0)
        for name in ("artifact.json", "verdict.json"):
            self.assertLessEqual((self.out/name).stat().st_size, MAX_DOC)

    def test_stale_tool_entries_are_removed(self):
        self.out.mkdir()
        for name in ("run.log", "artifact.json", "verdict.json"):
            (self.out/name).write_text("STALE")
        # Holding the stale file open keeps its inode allocated, so a reused inode number cannot mask it.
        held = os.open(self.out/"run.log", os.O_RDONLY)
        self.addCleanup(os.close, held)
        stale = os.fstat(held)
        daemon, r = self.go([image("1")])
        self.assertEqual(r.returncode, 2, r)
        self.assertEqual(sorted(os.listdir(self.out)), ["artifact.json", "run.log", "verdict.json"])
        fresh = (self.out/"run.log").stat()
        self.assertNotEqual((fresh.st_dev, fresh.st_ino), (stale.st_dev, stale.st_ino))
        self.assertEqual(os.fstat(held).st_nlink, 0)
        self.assertNotIn("STALE", self.log())

    def test_foreign_entries_cause_refusal_and_nothing_is_deleted(self):
        self.out.mkdir()
        (self.out/"run.log").write_text("keep")
        (self.out/"stale.txt").write_text("x")
        (self.out/"sub").mkdir()
        (self.out/"sub/deep").write_text("y")
        daemon = self.stub([image("1")])
        r = self.observe(daemon.endpoint)
        self.assertEqual(r.returncode, 3, r)
        self.assertIn(b"reason=out-dir-invalid", r.stdout)
        self.assertEqual(sorted(os.listdir(self.out)), ["run.log", "stale.txt", "sub"])
        self.assertEqual((self.out/"run.log").read_text(), "keep")
        self.assertEqual((self.out/"sub/deep").read_text(), "y")
        # A tool-named entry that is not a regular file is foreign too.
        (self.out/"stale.txt").unlink()
        shutil.rmtree(self.out/"sub")
        (self.out/"verdict.json").mkdir()
        self.assertEqual(self.observe(daemon.endpoint).returncode, 3)
        self.assertEqual(sorted(os.listdir(self.out)), ["run.log", "verdict.json"])
        self.assertEqual(daemon.requests, [])

    def test_out_dir_name_is_pinned_under_scratch(self):
        victims = [p.SCRATCH/("uzi-cmd-x"+os.urandom(4).hex()), p.SCRATCH/("snap-"+os.urandom(4).hex()),
                   p.SCRATCH/("go-cache-env-UPPER"+os.urandom(6).hex()),
                   p.SCRATCH/("go-cache-env-"+"a"*33+os.urandom(6).hex()),
                   p.SCRATCH/("go-cache-env"+os.urandom(6).hex()),
                   p.SCRATCH/("invalid-go-cache-env-"+os.urandom(6).hex())]
        daemon = self.stub([image("1")])
        for victim in victims:
            victim.mkdir()
            self.addCleanup(shutil.rmtree, victim, True)
            (victim/"precious").write_text("keep")
            with self.subTest(victim=victim.name):
                r = self.observe(daemon.endpoint, out=victim)
                self.assertEqual(r.returncode, 3, r)
                self.assertIn(b"reason=out-dir-invalid", r.stdout)
                self.assertEqual(sorted(os.listdir(victim)), ["precious"])
        nested = self.root/"go-cache-env-nested"
        self.assertEqual(self.observe(daemon.endpoint, out=nested).returncode, 3)
        self.assertFalse(nested.exists())
        a_file = p.SCRATCH/("go-cache-env-f"+os.urandom(4).hex())
        a_file.write_text("keep")
        self.addCleanup(a_file.unlink)
        self.assertEqual(self.observe(daemon.endpoint, out=a_file).returncode, 3)
        self.assertEqual(a_file.read_text(), "keep")
        self.assertEqual(daemon.requests, [])

    def test_out_dir_name_pin_alone_refuses_a_tool_only_victim(self):
        # Only run.log inside: the foreign-entry guard would accept it, so the name pin is the sole refusal.
        victim = p.SCRATCH/("snap-"+os.urandom(4).hex())
        victim.mkdir()
        self.addCleanup(shutil.rmtree, victim, True)
        (victim/"run.log").write_text("keep")
        daemon = self.stub([image("1")])
        r = self.observe(daemon.endpoint, out=victim)
        self.assertEqual(r.returncode, 3, r)
        self.assertIn(b"reason=out-dir-invalid", r.stdout)
        self.assertEqual(sorted(os.listdir(victim)), ["run.log"])
        self.assertEqual((victim/"run.log").read_text(), "keep")
        self.assertEqual(daemon.requests, [])

    def test_default_out_dir_is_the_pinned_name(self):
        self.assertEqual(o.DEFAULT_OUT, p.SCRATCH/"go-cache-env")
        self.assertEqual(o.validate_out_dir(str(o.DEFAULT_OUT)), o.DEFAULT_OUT)

    def test_symlinked_out_dir_and_symlink_components_are_refused(self):
        real = self.root/"real"
        real.mkdir()
        (real/"sentinel").write_text("keep")
        link = self.outdir()
        link.symlink_to(real)
        daemon = self.stub([image("1")])
        for target in (link, link/"go-cache-env-sub"):
            with self.subTest(target=str(target)):
                r = self.observe(daemon.endpoint, out=target)
                self.assertEqual(r.returncode, 3, r)
                self.assertIn(b"reason=out-dir-invalid", r.stdout)
                self.assertEqual(sorted(os.listdir(real)), ["sentinel"])
        self.assertEqual(daemon.requests, [])

    def test_out_dir_outside_scratch_or_unsafe_is_refused(self):
        outside = tempfile.mkdtemp(prefix="gco-outside-", dir="/tmp")
        self.addCleanup(shutil.rmtree, outside, True)
        Path(outside, "sentinel").write_text("keep")
        a_file = self.root/"afile"
        a_file.write_text("keep")
        daemon = self.stub([image("1")])
        for target in (outside, p.SCRATCH, str(self.root/".."/"x"), a_file, "/", "../escape"):
            with self.subTest(target=str(target)):
                r = self.observe(daemon.endpoint, out=target)
                self.assertEqual(r.returncode, 3, r)
                self.assertIn(b"reason=out-dir-invalid", r.stdout)
        self.assertEqual(sorted(os.listdir(outside)), ["sentinel"])
        self.assertEqual(a_file.read_text(), "keep")
        self.assertTrue(p.SCRATCH.is_dir())
        self.assertEqual(daemon.requests, [])

    def test_run_log_descriptor_check_runs_before_exit(self):
        r = self.hook_run("os = __import__('os')\nos.unlink(os.path.join(outdir, 'run.log'))")
        self.assertEqual(r.returncode, 4, r)
        self.assertEqual((r.stdout, r.stderr), (b"", b""))
        self.assertEqual(sorted(os.listdir(self.out)), ["artifact.json", "verdict.json"])

    def test_stdout_cap_is_enforced_and_counted(self):
        r = self.hook_run("out.write(b'#' * 100000)")
        self.assertEqual(r.returncode, 2, r)
        text = self.log()
        self.assertEqual(text.count("#"), o.STDOUT_CAP)
        self.assertIn("stdout_dropped=%d" % (100000-o.STDOUT_CAP), text)
        self.assertLessEqual(len(text.encode()), o.STDOUT_CAP+o.STDERR_CAP)

    def test_stderr_cap_is_enforced_and_counted_in_the_summary(self):
        r = self.hook_run("err.write(b'~' * 100000)")
        self.assertEqual(r.returncode, 2, r)
        text = self.log()
        self.assertEqual(text.count("~"), o.STDERR_CAP)
        self.assertIn("err_dropped=%d" % (100000-o.STDERR_CAP), text)
        self.assertLessEqual(len(text.encode()), o.STDOUT_CAP+o.STDERR_CAP)
        self.assertEqual((r.stdout, r.stderr), (b"", b""))

    def test_unexpected_error_reports_frames_and_type_only(self):
        r = self.hook_run("raise RuntimeError('hook-detail-secret')")
        self.assertEqual(r.returncode, 3, r)
        text = self.log()
        self.assertIn("reason=internal-error", text)
        self.assertIn("RuntimeError", text)
        self.assertNotIn("hook-detail-secret", text)
        self.assertLessEqual(len(text.encode()), o.STDOUT_CAP+o.STDERR_CAP)
        self.assertEqual(sorted(os.listdir(self.out)), ["run.log"])

    def test_out_dir_api_refuses_a_fourth_entry_and_oversize_documents(self):
        api = self.outdir()
        od = o.OutDir.reset(api)
        self.addCleanup(od.close)
        od.write_doc("artifact.json", {"a": 1})
        with self.assertRaises(o.Refusal):
            od.write_doc("artifact.json", {"a": 2})
        with self.assertRaises(o.Refusal):
            od.write_doc("extra.json", {})
        with self.assertRaises(o.Refusal):
            od.write_doc("verdict.json", {"big": "x"*MAX_DOC})
        self.assertEqual(sorted(os.listdir(api)), ["artifact.json"])
        log_fd = od.open_log()
        # A stray entry makes the next document the fourth: refused, and nothing is created.
        (api/"stray").write_text("x")
        with self.assertRaises(o.Refusal):
            od.write_doc("verdict.json", {"b": 1})
        self.assertEqual(sorted(os.listdir(api)), ["artifact.json", "run.log", "stray"])
        (api/"stray").unlink()
        od.write_doc("verdict.json", {"b": 1})
        self.assertEqual(sorted(os.listdir(api)), ["artifact.json", "run.log", "verdict.json"])
        self.assertEqual((api/"artifact.json").read_text(), '{"a": 1}\n')
        with self.assertRaises(o.Refusal):
            od.open_log()

    def test_out_dir_api_run_log_identity_check(self):
        od = o.OutDir.reset(self.outdir())
        self.addCleanup(od.close)
        log_fd = od.open_log()
        self.assertTrue(od.verify_log((log_fd,)))
        other = os.open(self.root/"other", os.O_WRONLY | os.O_CREAT, 0o600)
        self.addCleanup(os.close, other)
        self.assertFalse(od.verify_log((log_fd, other)))
        os.unlink("run.log", dir_fd=od.dfd)
        self.assertFalse(od.verify_log((log_fd,)))

    def test_bounded_sink_counts_what_it_drops(self):
        r, w = os.pipe()
        self.addCleanup(os.close, r)
        sink = o.BoundedSink(w, 10)
        sink.write(b"x"*6)
        sink.write(b"y"*6)
        sink.write(b"z")
        os.close(w)
        self.assertEqual(os.read(r, 100), b"xxxxxxyyyy")
        self.assertEqual(sink.dropped, 3)


class EvidenceTests(Base):
    def test_every_committed_daemon_string_is_reproduced_by_daemon_section(self):
        d = json.loads((EVIDENCE/"artifact.json").read_text())["daemon"]
        version = dict(Version=d["ServerVersion"], Os=d["Os"], Arch=d["Arch"], KernelVersion=d["KernelVersion"])
        features = json.dumps({"annotations": {"org.opencontainers.runc.version": d["RuncVersion"]}})
        options = ["name=seccomp,profile=builtin"] if d["seccomp"] else []
        if d["rootless"]:
            options.append("name=rootless")
        daemon_info = dict(
            ServerVersion=d["ServerVersion"], KernelVersion=d["KernelVersion"], Driver=d["Driver"],
            DriverStatus=[["driver-type", d["DriverType"]]], CgroupVersion=d["CgroupVersion"],
            CgroupDriver=d["CgroupDriver"], LoggingDriver=d["LoggingDriver"], DefaultRuntime=d["DefaultRuntime"],
            MemoryLimit=d["MemoryLimit"], SwapLimit=d["SwapLimit"], PidsLimit=d["PidsLimit"],
            Runtimes={"runc": {"status": {"org.opencontainers.runtime-spec.features": features}}},
            SecurityOptions=options)
        counts = dict(daemon=0)
        self.assertEqual(o.daemon_section(version, daemon_info, counts), d)
        self.assertEqual(counts["daemon"], 0)
        free = {"ServerVersion", "KernelVersion", "RuncVersion"}
        for key, value in d.items():
            if type(value) is str and key not in free:
                with self.subTest(key=key):
                    self.assertIn(value, o.ENUMS[key])

    def test_a_fresh_run_satisfies_the_evidence_invariants(self):
        daemon, r = self.go([image("1", tags=["postgres:16"])])
        self.assertEqual(r.returncode, 2, r)
        check_report_shape(self, *self.docs())

    def test_committed_evidence_directory_invariant(self):
        # The live run is done: the directory must exist and hold exactly the two sanitized documents.
        self.assertTrue(EVIDENCE.is_dir(), "evidence directory is missing")
        self.assertEqual(sorted(os.listdir(EVIDENCE)), ["artifact.json", "verdict.json"])
        artifact, verdict = read_doc(EVIDENCE/"artifact.json"), read_doc(EVIDENCE/"verdict.json")
        check_report_shape(self, artifact, verdict)
        for name in ("artifact.json", "verdict.json"):
            text = (EVIDENCE/name).read_text()
            for forbidden in ("uzi-hw-", "/data/runner", "/home/", "DockerRootDir", "RepoTags", "RepoDigests"):
                self.assertNotIn(forbidden, text, name)
        self.assertTrue(artifact["binding"]["images"])
        for rec in artifact["binding"]["images"]:
            self.assertTrue(rec["repos"])
            for name in rec["repos"]:
                self.assertTrue(re.fullmatch(r"repo-[0-9a-f]{12}|postgres|hello-world|docker\.io/library/(postgres|hello-world)",
                                             name), name)

    def test_committed_evidence_artifact_reevaluates_to_no_qualified_environment(self):
        artifact = read_doc(EVIDENCE/"artifact.json")
        again = q.evaluate(artifact, now=artifact["observed_at"])
        self.assertEqual(again["verdict"], "NO-QUALIFIED-ENVIRONMENT")
        self.assertIs(again["qualified"], False)
        self.assertEqual(set(again["legs"]), set(q.LEGS))
        self.assertNotIn("PROVEN", {leg["status"] for leg in again["legs"].values()})


if __name__ == "__main__":
    unittest.main()
