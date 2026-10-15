#!/usr/bin/env python3
"""Host-owned serial calibration runner; provided local image only, never builds."""
import argparse
import json
import os
from pathlib import Path
import secrets
import subprocess
import sys
import time
import uuid
import unicodedata

import bounded
import calibration_check as check

ROOT = Path(__file__).resolve().parents[2]
SCRATCH = ROOT / ".uzi/scratch"
LABEL = bounded.LABEL


def diagnostic(message):
    """Render one host diagnostic, capped at 2048 UTF-8 bytes including suffix."""
    suffix = " [truncated]"
    budget = 2048 - len(suffix.encode())
    parts = []
    size = 0
    for char in str(message):
        category = unicodedata.category(char)
        piece = ascii(char)[1:-1] if category.startswith("C") or category in ("Zl", "Zp") else char
        length = len(piece.encode("utf8"))
        if size + length > budget:
            parts.append(suffix)
            break
        parts.append(piece)
        size += length
    print("".join(parts), file=sys.stderr)


class DiagnosticParser(argparse.ArgumentParser):
    def _print_message(self, message, file=None):
        if file is sys.stderr:
            if message:
                diagnostic(message)
        else:
            super()._print_message(message, file)


def uuid4():
    return str(uuid.uuid4())


def create_manifest(mode, run_id, attempt, sha):
    if mode == "ci":
        check.require(run_id == os.environ.get("GITHUB_RUN_ID") and
                      attempt == os.environ.get("GITHUB_RUN_ATTEMPT"), "CI host identity required")
    check.require(run_id and attempt, "explicit host run identity and attempt required")
    manifest = {"schemaVersion": 1, "synthetic": False, "invocation": secrets.token_hex(16),
                "identity": {"sourceSha": sha, "mode": mode, "runId": run_id, "attempt": attempt},
                "context": {"run_id": uuid4(), "worker_id": uuid4(), "register_nonce": uuid4(),
                            "claim_generation": 1, "memory_episode": 0},
                "fixtures": [{"fixtureId": uuid4(), "legId": uuid4(), "reportId": uuid4(),
                              "treeIds": [uuid4(), uuid4()], "limitBytes": size}
                             for size in (1024 ** 3, 2 * 1024 ** 3)]}
    check.manifest_check(manifest)
    return manifest


class Host:
    def __init__(self, manifest, directory, image):
        self.manifest, self.directory, self.image = manifest, directory, image
        self.requests = []
        self.cleanup_proofs = {}
        self.cleanup_attempted = set()
        self.results = {}
        self.deadline = None

    def docker(self, args, seconds=10):
        if self.deadline is not None:
            seconds = min(seconds, self.deadline - time.monotonic())
            check.require(seconds > 5, "provisioning deadline exhausted")
        return bounded.docker(args, self.directory, seconds)

    def journal(self, kind, name):
        entry = {"kind": kind, "name": name}
        self.requests.append(entry)
        # Journal flushed BEFORE create. A lost ACK still requires ownership inspect.
        with open(self.directory / "requests.jsonl", "a", encoding="utf8") as stream:
            stream.write(json.dumps(entry) + "\n")
            stream.flush()
            os.fsync(stream.fileno())

    def inspect(self, name, seconds=10):
        metadata_format = ('[{"Id":{{json .Id}},"Image":{{json .Image}},'
                           '"Config":{"User":{{json .Config.User}},"Labels":{{json .Config.Labels}}},'
                           '"State":{"Running":{{json .State.Running}}},'
                           '"HostConfig":{{json .HostConfig}},"Mounts":{{json .Mounts}}}]')
        items = check.exact_json(self.docker(["inspect", "--format", metadata_format, name], seconds))
        check.require(isinstance(items, list) and len(items) == 1, "ambiguous Docker metadata")
        item = items[0]
        check.require(item["Config"]["Labels"].get(LABEL) == self.manifest["invocation"], "ownership mismatch")
        check.require(len(item["Id"]) == 64 and all(c in "0123456789abcdef" for c in item["Id"]),
                      "immutable Docker CID")
        return item

    def cleanup_container(self, name, fixture=None, deadline=None, expected_cid=None):
        # One shared <=30s budget, no retries. Failed metadata never authorizes removal.
        check.require(name not in self.cleanup_attempted, "cleanup already attempted; no retry")
        self.cleanup_attempted.add(name)
        started = time.monotonic()
        deadline = min(deadline or float("inf"), started + 29)
        def left():
            remaining = deadline - time.monotonic()
            check.require(remaining > 5, "owned cleanup budget exhausted")
            return min(10, remaining)
        item = self.inspect(name, left())
        cid = item["Id"]
        if expected_cid is not None:
            check.require(cid == expected_cid, "create ACK does not match immutable CID")
        if fixture:
            check.require(item["Config"]["Labels"].get("io.uzi.memory.fixture") == fixture["fixtureId"],
                          "cleanup fixture label mismatch")
            if "containerId" in fixture:
                check.require(cid == fixture["containerId"], "cleanup CID changed")
            # Lost create ACK permits only cleanup by the inspected owned CID.
            # It never supplies containerId authority for a later start.
        if item["State"]["Running"]:
            self.docker(["stop", "--time", "1", cid], left())
        removed = self.docker(["rm", cid], left()).decode().strip()
        check.require(removed == cid, "ambiguous remove ACK")
        proof = {"confirmed": True, "containerId": cid, "removedId": removed,
                 "label": self.manifest["invocation"], "hostMonotonicEnd": time.monotonic(),
                 "hostMonotonicStart": started}
        if fixture:
            self.cleanup_proofs[fixture["fixtureId"]] = proof
        self.requests = [entry for entry in self.requests if entry["name"] != name]

    def provision(self):
        started = time.monotonic()
        self.deadline = started + 60
        metadata = check.exact_json(self.docker(["image", "inspect", "--format", '[{"Id":{{json .Id}}}]', self.image]))
        check.require(isinstance(metadata, list) and len(metadata) == 1, "image metadata")
        image_id = metadata[0]["Id"]
        check.require(isinstance(image_id, str) and image_id.startswith("sha256:") and len(image_id) == 71,
                      "local immutable image ID")
        self.manifest["imageId"] = image_id
        token = self.manifest["invocation"]
        volume = "mem2396-" + token + "-deps"
        self.journal("volume", volume)
        ack = self.docker(["volume", "create", "--label", LABEL + "=" + token, volume]).decode().strip()
        check.require(ack == volume, "ambiguous volume create ACK")
        bounded.owned("volume", volume, token, self.directory)
        # Docker seeds the volume from the provided image at create, without executing it.
        seed = "mem2396-" + token + "-seed"
        self.journal("container", seed)
        receipt = self.docker(["create", "--pull", "never", "--network", "none", "--cgroupns", "private",
                     "--name", seed, "--label", LABEL + "=" + token,
                     "--user", "10003:10003", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                     "--memory", str(1024 ** 3), "--memory-swap", str(1024 ** 3),
                     "--mount", "type=volume,src=" + volume + ",dst=/app/node_modules",
                     "--entrypoint", "/usr/local/bin/node", image_id, "-e", "void 0"]).decode().strip()
        self.cleanup_container(seed, expected_cid=receipt)
        check.require(time.monotonic() - started < 60, "offline provisioning exceeds 60 seconds")
        self.deadline = None
        return volume

    def create(self, fixture, volume):
        token = self.manifest["invocation"]
        name = "mem2396-" + token + "-" + str(fixture["limitBytes"])
        fixture["containerName"] = name
        disk = self.directory / fixture["fixtureId"]
        disk.mkdir(mode=0o777)
        disk.chmod(0o777)
        self.journal("container", name)
        args = ["create", "--pull", "never", "--network", "none", "--cgroupns", "private",
                "--name", name, "--label", LABEL + "=" + token,
                "--label", "io.uzi.memory.fixture=" + fixture["fixtureId"],
                "--user", "10003:10003", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--memory", str(fixture["limitBytes"]), "--memory-swap", str(fixture["limitBytes"]),
                "--pids-limit", "128", "--workdir", "/proof",
                "--tmpfs", "/proof/.uzi/scratch:exec,uid=10003,gid=10003,mode=0700,size=256m",
                "--tmpfs", "/tmp:exec,uid=10003,gid=10003,mode=0700,size=32m",
                "--env", "HOME=/proof/.uzi/scratch", "--env", "TMPDIR=/proof/.uzi/scratch",
                "--mount", "type=bind,src=" + str(disk) + ",dst=/calibration-disk",
                "--mount", "type=bind,src=" + str(self.directory / "expected.json") + ",dst=/expected.json,readonly",
                "--mount", "type=volume,src=" + volume + ",dst=/proof/agent/node_modules,readonly"]
        for source in ("e2e/worker-uid", "agent/src", "agent/test/helpers", "agent/test/fixtures",
                       "agent/package.json", "agent/tsconfig.json"):
            args += ["--mount", "type=bind,src=" + str(ROOT / source) + ",dst=/proof/" + source + ",readonly"]
        args += ["--entrypoint", "/usr/local/bin/node", self.manifest["imageId"], "--import",
                 "/proof/agent/node_modules/tsx/dist/loader.mjs",
                 "/proof/e2e/worker-uid/calibration-fixture.mts", "/expected.json", fixture["fixtureId"]]
        receipt = self.docker(args).decode().strip()
        item = self.inspect(name)
        check.require(receipt == item["Id"], "create ACK does not match immutable CID")
        fixture["containerId"] = item["Id"]
        self.verify_config(item, fixture)

    def verify_config(self, item, fixture):
        host, config = item["HostConfig"], item["Config"]
        check.require(item["Id"] == fixture["containerId"] and item["Image"] == self.manifest["imageId"],
                      "Docker image/CID binding")
        check.require(host["NetworkMode"] == "none" and host["CgroupnsMode"] == "private" and
                      host["Memory"] == fixture["limitBytes"] and host["MemorySwap"] == fixture["limitBytes"],
                      "finite private isolated Docker config")
        check.require(config["User"] == "10003:10003" and host["CapDrop"] == ["ALL"] and
                      not host.get("CapAdd") and not host.get("Privileged") and
                      "no-new-privileges" in host["SecurityOpt"], "Docker capless NNP identity")
        check.require(config["Labels"]["io.uzi.memory.fixture"] == fixture["fixtureId"], "fixture label")
        mounts = item["Mounts"]
        manifest_mount = [m for m in mounts if m["Destination"] == "/expected.json"]
        check.require(len(manifest_mount) == 1 and manifest_mount[0]["RW"] is False and
                      manifest_mount[0]["Source"] == str(self.directory / "expected.json"), "readonly trusted manifest")

    def run(self):
        # All idle creates first; each start waits for both container and host cleanup.
        started = time.monotonic()
        expected_file = self.directory / "expected.json"
        expected_file.write_text(json.dumps(self.manifest))
        volume = self.provision()
        self.deadline = started + 120
        for fixture in self.manifest["fixtures"]:
            self.create(fixture, volume)
        self.deadline = None
        check.require(time.monotonic() - started < 120, "total provisioning exceeds 120 seconds")
        expected_file.write_text(json.dumps(self.manifest))
        failures = []
        for fixture in self.manifest["fixtures"]:
            fixture_deadline = time.monotonic() + 90
            name = fixture["containerName"]
            item = self.inspect(fixture["containerId"])
            self.verify_config(item, fixture)
            output = self.directory / (fixture["fixtureId"] + ".stdout")
            # <=60s attach + <=30s host cleanup: one <=90s fixture wall.
            rc = bounded.capture(["timeout", "--kill-after=5s", "54s", "docker", "start", "--attach",
                                  fixture["containerId"]], output, 60)
            raw = output.read_bytes()
            (self.directory / (fixture["fixtureId"] + ".status")).write_text(f"EXIT={rc}\n")
            try:
                report = check.reports(raw, count=1)[0]
                for key, value in {**self.manifest["identity"], **fixture}.items():
                    check.require(report[key] == value, "producer identity mismatch: " + key)
                check.require(type(report["schemaVersion"]) is int and report["schemaVersion"] == 1,
                              "producer version")
                check.require(isinstance(report["reasons"], list) and
                              all(isinstance(reason, str) for reason in report["reasons"]), "producer reasons")
                check.require(type(report["status"]) is str and
                              report["status"] in ("SUCCESS_COMPLETE", "FAILED"), "producer status")
                if report["status"] == "SUCCESS_COMPLETE":
                    check.verify_report(report, self.manifest, fixture)
                self.results[fixture["fixtureId"]] = report
            except (ValueError, KeyError, TypeError, UnicodeError) as exc:
                self.results[fixture["fixtureId"]] = self.aborted(fixture, "unparseable producer report: " + str(exc))
            self.cleanup_container(name, fixture, fixture_deadline)
            report = self.results[fixture["fixtureId"]]
            if rc or report.get("status") != "SUCCESS_COMPLETE":
                failures.append(f"fixture {fixture['fixtureId']} exit {rc}; status {report.get('status')}")
                # Only a demonstrated unavailable reader BEFORE launch allows the
                # other requested size. Arbitrary zero-handle failures do not.
                prereq = (report.get("status") == "FAILED" and
                          isinstance(report.get("finalCleanup"), dict) and report["finalCleanup"].get("handles") == 0 and
                          any("actual default reader unavailable:" in reason for reason in report.get("reasons", [])) and
                          any(record.get("sample", {}).get("available") is False
                              for record in report.get("rawSamples", [])))
                if not prereq:
                    break
        check.require(not failures, "; ".join(failures))

    def aborted(self, fixture, reason):
        # Missing evidence is explicit null, never a zero-duration measurement.
        return {"schemaVersion": 1, **self.manifest["identity"], **fixture,
                "provenance": "measured", "status": "FAILED", "reasons": [reason],
                **{key: None for key in ("observed", "guard", "plan", "clock", "bounds", "response",
                                        "derivation", "settlement", "cleanup", "finalCleanup")},
                "baseline": [], "samples": [], "rawSamples": [], "trees": [],
                "signalMeaning": "application supervisor disposal control write", "ackService": "LOCALFIXTURE",
                "outcome": {"status": "failed", "reasons": ["host aborted"], "successor": "aborted"}}

    def publish(self, error=None):
        values = []
        for fixture in self.manifest["fixtures"]:
            report = self.results.get(fixture["fixtureId"])
            if report is None:
                report = self.aborted(fixture, "host aborted before complete producer report: " + str(error))
            # This field is host-owned. A producer supplying it is invalid, never
            # sanitized into success. Original stdout is retained byte-for-byte.
            if "hostCleanup" in report:
                report["status"] = "FAILED"
                report["reasons"] = ["producer supplied host-only cleanup evidence"]
            report["hostCleanup"] = self.cleanup_proofs.get(fixture["fixtureId"])
            if error is not None:
                report["status"] = "FAILED"
                report["reasons"] = [*report.get("reasons", [])] if isinstance(report.get("reasons"), list) else []
                report["reasons"].append("host failure: " + str(error))
            values.append(report)
        data = "".join(check.BEGIN + "\n" + json.dumps(r, separators=(",", ":")) + "\n" + check.END + "\n"
                       for r in values).encode()
        (self.directory / "reports.stdout").write_bytes(data)
        (self.directory / "host-cleanup.json").write_text(json.dumps(self.cleanup_proofs))
        sys.stdout.buffer.write(data)
        sys.stdout.buffer.flush()
        if error is None:
            check.verify(self.manifest, data, self.cleanup_proofs)

    def finish(self):
        errors = []
        self.deadline = None
        deadline = time.monotonic() + 29
        # One attempt per journaled resource; independent failures do not suppress siblings.
        # All remaining resources share 30s including reaping and kill grace.
        for entry in list(reversed(self.requests)):
            try:
                if entry["kind"] == "container":
                    check.require(entry["name"] not in self.cleanup_attempted, "previous owned cleanup failed")
                    fixture = next((f for f in self.manifest["fixtures"] if f.get("containerName") == entry["name"]), None)
                    self.cleanup_container(entry["name"], fixture, deadline=deadline)
                else:
                    remaining = deadline - time.monotonic()
                    check.require(remaining > 5, "shared cleanup deadline")
                    handle = bounded.owned("volume", entry["name"], self.manifest["invocation"], self.directory, min(10, remaining))
                    remaining = deadline - time.monotonic()
                    check.require(remaining > 5, "shared cleanup deadline")
                    ack = self.docker(["volume", "rm", handle], min(10, remaining)).decode().strip()
                    check.require(ack == handle, "ambiguous volume removal ACK")
                    self.requests.remove(entry)
            except (OSError, ValueError, KeyError, TypeError) as exc:
                errors.append(str(exc))
        check.require(not errors, "; ".join(errors))


def main():
    parser = DiagnosticParser(description=__doc__)
    parser.add_argument("--image", required=True, help="provided, already local image; no pull/build")
    parser.add_argument("--mode", required=True, choices=("ci", "local"))
    parser.add_argument("--run-id")
    parser.add_argument("--attempt")
    args = parser.parse_args()
    top = subprocess.run(["git", "-C", str(ROOT), "rev-parse", "--show-toplevel"], check=True,
                         capture_output=True, text=True, timeout=5).stdout.strip()
    check.require(Path(top).resolve() == ROOT, "script root must be the actual checkout root")
    sha = subprocess.run(["git", "-C", str(ROOT), "rev-parse", "HEAD"], check=True,
                         capture_output=True, text=True, timeout=5).stdout.strip()
    manifest = create_manifest(args.mode,
                               os.environ.get("GITHUB_RUN_ID") if args.mode == "ci" else args.run_id,
                               os.environ.get("GITHUB_RUN_ATTEMPT") if args.mode == "ci" else args.attempt, sha)
    SCRATCH.mkdir(exist_ok=True, parents=True)
    directory = SCRATCH / ("memory-calibration-" + manifest["invocation"])
    directory.mkdir()
    host = Host(manifest, directory, args.image)
    error = None
    try:
        host.run()
    except Exception as exc:
        # Ordinary implementation/shape errors still require owned cleanup and
        # two explicit failure reports. Interrupts retain their normal semantics.
        error = exc
    finally:
        try:
            host.finish()
        except (OSError, ValueError, KeyError, TypeError) as exc:
            error = ValueError(str(error) + "; cleanup: " + str(exc))
    host.publish(error)
    diagnostic("calibration host evidence: " + str(directory))
    if error is not None:
        raise ValueError(str(error))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as exc:
        diagnostic("calibration FAIL: " + str(exc))
        sys.exit(1)
