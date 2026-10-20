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
PROOF = "/app/uzi-worker-uid-proof"
LAYOUT_PROBE = 'const fs = require("node:fs"); try { fs.lstatSync("/app/uzi-worker-uid-proof"); throw new Error("proof directory already exists in image"); } catch (e) { if (e.code !== "ENOENT") throw e; } if (!require.resolve("tsx").startsWith("/app/node_modules/")) throw new Error("image-native tsx unavailable");'
LABEL = bounded.LABEL
# Node assert diagnostic calibration (measurement only; no thresholds). Markers differ from the report markers.
DIAG_BEGIN = "UZI_NODE_DIAGNOSTIC_BEGIN"
DIAG_END = "UZI_NODE_DIAGNOSTIC_END"
DIAG_WORKLOADS = ("many-mismatch", "equivalent", "bounded-diagnostic")
# The same many-mismatch workload run as a guarded Claude command through the real lane, launcher,
# reader and monitor (calibration-claude-guard.mts); one container per size.
DIAG_GUARDED_WORKLOADS = ("guarded-many-mismatch",)
DIAG_SIZES = (1024 ** 3, 2 * 1024 ** 3)
# Eight containers (three plain workloads plus the guarded leg, per size), each bounded in turn:
# create <=10s, inspect <=10s, start/capture <=bound+5s, owned cleanup <=29s. A plain workload's bound
# is DIAG_SECONDS (30s), so ~84s worst case per container. The guarded leg's in-container worst case
# is the sum of its own bounds (calibration-claude-guard.mts, calibration-diagnostic.mts): ~0.75s
# baseline, <=10s for the launcher's started report, the 15s command deadline, on the deadline path
# <=7s of dispose (5s cleanup plus 2s join slack; the 1s grace applies only after an exit), then the
# ended and pressure waits (5s each), the monitor settle (8s) and the lane quiesce (8s): ~59s, before
# container start and the tsx compile. DIAG_GUARDED_SECONDS (90s) leaves a real margin over that, so
# a slow but legitimate "deadline" outcome is recorded rather than cut by the host. Its container is
# ~144s worst case, so the step adds at most ~13.2 minutes. About 14s is typical (the workload until the
# guard acts or the deadline, then bounded quiesce and settle); a hung leg is cut at
# DIAG_GUARDED_SECONDS by the host and fails like any other host error.
DIAG_SECONDS = 30
DIAG_GUARDED_SECONDS = 90
DIAG_OUTPUT_BYTES = 64 * 1024
# Host exit codes that mean the container's main process was killed by the kernel before it could
# report (docker start --attach returns the container's exit code): a recorded guarded outcome, not
# an error. 124 (the host deadline) is deliberately not in this set.
GUARDED_KILLED_EXIT = (137,)
# Measured outcomes only. Lane refusals, launch failures, supervision/enrollment failures, an admission
# closed without the guard's result, a workload that never started and the catch-all "other" are harness
# faults (calibration-claude-guard.mts records them under their own names), never accepted here.
GUARDED_OUTCOMES = ("guard_acted", "oom_before_guard", "deadline", "exited_before_guard")


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


def parse_diagnostic(raw, workload):
    """Exact, bounded parse of one diagnostic JSON line between the dedicated markers."""
    check.require(len(raw) <= DIAG_OUTPUT_BYTES, "oversized node diagnostic output")
    lines = raw.decode("utf8", errors="strict").splitlines()
    check.require(lines.count(DIAG_BEGIN) == 1 and lines.count(DIAG_END) == 1, "node diagnostic markers")
    begin, end = lines.index(DIAG_BEGIN), lines.index(DIAG_END)
    check.require(end == begin + 2, "node diagnostic payload must be exactly one line")
    value = check.exact_json(lines[begin + 1])
    check.require(isinstance(value, dict) and value.get("schemaVersion") == 1 and value.get("workload") == workload,
                  "node diagnostic identity")
    child, cgroup = value.get("child"), value.get("cgroup")
    check.require(isinstance(child, dict) and isinstance(cgroup, dict) and isinstance(value.get("nodeVersion"), str),
                  "node diagnostic shape")
    check.require(isinstance(child.get("timedOut"), bool) and type(child.get("wallMs")) is int, "node diagnostic child")
    for key in ("memoryPeakBytes", "oomKill"):
        check.require(cgroup.get(key) is None or type(cgroup[key]) is int, "node diagnostic cgroup")
    return value


def parse_guarded(raw, workload):
    """Exact, bounded parse of the guarded-leg JSON line; harness faults raise, measured outcomes do not."""
    check.require(len(raw) <= DIAG_OUTPUT_BYTES, "oversized node diagnostic output")
    lines = raw.decode("utf8", errors="strict").splitlines()
    check.require(lines.count(DIAG_BEGIN) == 1 and lines.count(DIAG_END) == 1, "node diagnostic markers")
    begin, end = lines.index(DIAG_BEGIN), lines.index(DIAG_END)
    check.require(end == begin + 2, "node diagnostic payload must be exactly one line")
    value = check.exact_json(lines[begin + 1])
    check.require(isinstance(value, dict) and value.get("schemaVersion") == 1 and value.get("workload") == workload
                  and value.get("leg") == "guarded", "node diagnostic identity")
    tool, cleanup, growth, timeline, budget = (value.get(key) for key in ("tool", "cleanup", "growth", "timeline", "budget"))
    check.require(all(isinstance(item, dict) for item in (tool, cleanup, growth, timeline, budget)) and
                  isinstance(value.get("nodeVersion"), str) and type(value.get("psiSynthesized")) is bool and
                  isinstance(growth.get("series"), list), "node diagnostic shape")
    check.require(tool.get("outcome") in GUARDED_OUTCOMES, "node diagnostic guarded outcome")
    if tool["outcome"] == "guard_acted":
        pressure = tool.get("pressure")
        check.require(isinstance(pressure, dict) and pressure.get("code") == "worker_memory_pressure" and
                      tool.get("pressureToolIdMatches") is True and pressure.get("tool_id") == tool.get("toolUseId"),
                      "node diagnostic pressure result attribution")
    # Cleanup is part of the harness, not the measurement: an unconfirmed lane, a leaked command
    # process or a still-registered command is an error even though the container is removed anyway.
    check.require(cleanup.get("quiesceConfirmed") is True and cleanup.get("liveCountAfter") == 0 and
                  cleanup.get("registryEligibleEntries") == 0 and cleanup.get("commandPidsGone") is True and
                  cleanup.get("otherProcessPidsRemaining") == [] and cleanup.get("poisoned") is False,
                  "node diagnostic guarded cleanup unconfirmed")
    return value


def killed_guarded_result(workload, rc):
    """The guarded container's main process was killed before it could report: a recorded outcome."""
    return {"schemaVersion": 1, "workload": workload, "leg": "guarded", "nodeVersion": "unknown",
            "psiSynthesized": None, "noReport": True, "tool": {"outcome": "container_killed_no_report"},
            "killedExit": rc}


def guarded_summary(result):
    if result.get("noReport"):
        return (f"node diagnostic {result['workload']} {result['limitBytes'] >> 20}MiB: container_killed_no_report "
                f"node={result['nodeVersion']} exit={result['killedExit']}")
    tool, timeline, budget, cleanup = result["tool"], result["timeline"], result["budget"], result["cleanup"]
    pressure = tool.get("pressure") or {}
    return (f"node diagnostic {result['workload']} {result['limitBytes'] >> 20}MiB: {tool['outcome']} "
            f"node={result['nodeVersion']} psi_synthesized={result['psiSynthesized']} baseline={result['baselineBytes']} "
            f"peak_series={result['growth']['peakSeriesBytes']} samples={result['growth']['sampleCount']} "
            f"crossing_ms={timeline['crossingSampleCompletedMs']} crossing_to_reserve_ms={budget['crossingToReserveRequestMs']} "
            f"cancel_to_drain_ms={budget['cancelToConfirmedDrainMs']} crossing_to_drain_ms={budget['crossingToConfirmedDrainMs']} "
            f"pressure_outcome={pressure.get('outcome')} oom_kill={cleanup['oomKillAfter']} peak={cleanup['memoryPeakBytes']} "
            f"host_wall_s={result['hostWallSeconds']} exit={result['hostExit']}")


def diagnostic_summary(result):
    if result.get("leg") == "guarded":
        return guarded_summary(result)
    child, cgroup = result["child"], result["cgroup"]
    reported = child.get("reported") or {}
    outcome = ("timed_out" if child["timedOut"] else "oom_killed" if cgroup.get("oomKill") else
               reported.get("outcome", "no_report"))
    return (f"node diagnostic {result['workload']} {result['limitBytes'] >> 20}MiB: {outcome} "
            f"node={result['nodeVersion']} peak={cgroup.get('memoryPeakBytes')} oom_kill={cgroup.get('oomKill')} "
            f"wall_ms={child['wallMs']} exit={child.get('exitCode')} signal={child.get('signal')} "
            f"message_bytes={reported.get('messageBytes')}")


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
        removed = self.docker(["rm", "--volumes", cid], left()).decode().strip()
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
        # Check the unused source mount namespace and image-native dependencies before mounting.
        seed = "mem2396-" + token + "-seed"
        self.journal("container", seed)
        receipt = self.docker(["create", "--pull", "never", "--network", "none", "--cgroupns", "private",
                     "--name", seed, "--label", LABEL + "=" + token,
                     "--tmpfs", "/data", "--tmpfs", "/nix",
                     "--user", "10003:10003", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                     "--memory", str(1024 ** 3), "--memory-swap", str(1024 ** 3),
                     "--entrypoint", "/usr/local/bin/node", image_id, "-e", LAYOUT_PROBE]).decode().strip()
        item = self.inspect(seed)
        check.require(receipt == item["Id"] and item["Image"] == image_id,
                      "layout preflight create ACK/image binding")
        self.docker(["start", "--attach", item["Id"]])
        self.cleanup_container(seed, expected_cid=item["Id"])
        check.require(time.monotonic() - started < 60, "offline provisioning exceeds 60 seconds")
        self.deadline = None

    def create(self, fixture):
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
                "--tmpfs", "/data", "--tmpfs", "/nix",
                "--tmpfs", "/proof/.uzi/scratch:exec,uid=10003,gid=10003,mode=0700,size=256m",
                "--tmpfs", "/tmp:exec,uid=10003,gid=10003,mode=0700,size=32m",
                "--env", "HOME=/proof/.uzi/scratch", "--env", "TMPDIR=/proof/.uzi/scratch",
                "--mount", "type=bind,src=" + str(disk) + ",dst=/calibration-disk",
                "--mount", "type=bind,src=" + str(self.directory / "expected.json") + ",dst=/expected.json,readonly"]
        for source in ("e2e/worker-uid", "agent/src", "agent/test/helpers", "agent/test/fixtures",
                       "agent/package.json", "agent/tsconfig.json"):
            args += ["--mount", "type=bind,src=" + str(ROOT / source) + ",dst=" + PROOF + "/" + source + ",readonly"]
        args += ["--entrypoint", "/usr/local/bin/node", self.manifest["imageId"], "--import",
                 "/app/node_modules/tsx/dist/loader.mjs",
                 PROOF + "/e2e/worker-uid/calibration-fixture.mts", "/expected.json", fixture["fixtureId"]]
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
        self.provision()
        self.deadline = started + 120
        for fixture in self.manifest["fixtures"]:
            self.create(fixture)
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

    def diagnostic_create(self, name, size, workload):
        token = self.manifest["invocation"]
        self.journal("container", name)
        # The guarded leg imports the real agent lane, launcher, reader and monitor (read-only).
        guarded_mounts = []
        if workload in DIAG_GUARDED_WORKLOADS:
            for source in ("agent/src", "agent/package.json", "agent/tsconfig.json"):
                guarded_mounts += ["--mount", "type=bind,src=" + str(ROOT / source) + ",dst=" + PROOF + "/" + source + ",readonly"]
        args = ["create", "--pull", "never", "--network", "none", "--cgroupns", "private",
                "--name", name, "--label", LABEL + "=" + token,
                "--user", "10003:10003", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--memory", str(size), "--memory-swap", str(size),
                "--pids-limit", "128", "--workdir", "/proof",
                "--tmpfs", "/data", "--tmpfs", "/nix",
                "--tmpfs", "/proof/.uzi/scratch:exec,uid=10003,gid=10003,mode=0700,size=256m",
                "--tmpfs", "/tmp:exec,uid=10003,gid=10003,mode=0700,size=32m",
                "--env", "HOME=/proof/.uzi/scratch", "--env", "TMPDIR=/proof/.uzi/scratch",
                "--mount", "type=bind,src=" + str(ROOT / "e2e/worker-uid") + ",dst=" + PROOF + "/e2e/worker-uid,readonly",
                *guarded_mounts,
                "--entrypoint", "/usr/local/bin/node", self.manifest["imageId"], "--import",
                "/app/node_modules/tsx/dist/loader.mjs",
                PROOF + "/e2e/worker-uid/calibration-diagnostic.mts", workload]
        receipt = self.docker(args).decode().strip()
        item = self.inspect(name)
        check.require(receipt == item["Id"] and item["Image"] == self.manifest["imageId"],
                      "diagnostic create ACK/image binding")
        host = item["HostConfig"]
        check.require(host["NetworkMode"] == "none" and host["CgroupnsMode"] == "private" and
                      host["Memory"] == size and host["MemorySwap"] == size and
                      item["Config"]["User"] == "10003:10003" and host["CapDrop"] == ["ALL"] and
                      not host.get("CapAdd") and "no-new-privileges" in host["SecurityOpt"],
                      "diagnostic Docker isolation config")
        return item["Id"]

    def measure_diagnostic(self, size, workload):
        name = "mem2396-" + self.manifest["invocation"] + "-diag-" + workload + "-" + str(size)
        cid = self.diagnostic_create(name, size, workload)
        started = time.monotonic()
        output = self.directory / ("diagnostic-" + workload + "-" + str(size) + ".stdout")
        seconds = DIAG_GUARDED_SECONDS if workload in DIAG_GUARDED_WORKLOADS else DIAG_SECONDS
        rc = bounded.capture(["timeout", "--kill-after=5s", str(seconds) + "s", "docker", "start", "--attach", cid],
                             output, seconds + 5, DIAG_OUTPUT_BYTES)
        wall = time.monotonic() - started
        raw = output.read_bytes()
        (self.directory / (output.stem + ".status")).write_text(f"EXIT={rc}\n")
        # The owned container is always removed before its output is judged.
        self.cleanup_container(name, deadline=time.monotonic() + 29, expected_cid=cid)
        if workload in DIAG_GUARDED_WORKLOADS:
            # A kernel kill of the container's main process leaves no report: recorded, not an error.
            result = (killed_guarded_result(workload, rc) if rc in GUARDED_KILLED_EXIT and DIAG_BEGIN not in raw.decode("utf8", errors="replace")
                      else parse_guarded(raw, workload))
        else:
            result = parse_diagnostic(raw, workload)
        # Host-side facts; a workload OOM kill, timeout or assertion is a recorded outcome, not a failure.
        return {"limitBytes": size, "hostExit": rc, "hostWallSeconds": round(wall, 3), **result}

    def measure_diagnostics(self):
        """Serial measurement-only step; workload and guarded-leg outcomes never fail calibration."""
        check.require("imageId" in self.manifest, "diagnostics require provisioned image")
        results = []
        try:
            for size in DIAG_SIZES:
                for workload in DIAG_WORKLOADS + DIAG_GUARDED_WORKLOADS:
                    results.append(self.measure_diagnostic(size, workload))
        finally:
            (self.directory / "node-diagnostic.json").write_text(json.dumps(results, indent=1) + "\n")
        for result in results:
            diagnostic(diagnostic_summary(result))
        return results

    def aborted(self, fixture, reason):
        # Missing evidence is explicit null, never a zero-duration measurement.
        return {"schemaVersion": 1, **self.manifest["identity"], **fixture,
                "provenance": "measured", "status": "FAILED", "reasons": [reason],
                **{key: None for key in ("observed", "guard", "plan", "clock", "bounds", "response",
                                        "derivation", "samplingSchedule", "settlement", "cleanup", "finalCleanup",
                                        "units", "plannedChargeClasses", "oomEvents")},
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
    error = diagnostic_error = None
    try:
        try:
            host.run()
        except Exception as exc:
            # Retain fixture failures, including an unavailable production PSI reader.
            # Interrupts bypass diagnostics and publishing while still running owned cleanup.
            error = exc
        if "imageId" in manifest:
            try:
                host.measure_diagnostics()
            except Exception as exc:
                # Independent measurement still runs after fixture failure. Its own failure is
                # separate; unconfirmed owned cleanup below fails both reports closed.
                diagnostic_error = exc
    finally:
        try:
            host.finish()
        except (OSError, ValueError, KeyError, TypeError) as exc:
            error = ValueError(str(error) + "; cleanup: " + str(exc))
    host.publish(error)
    diagnostic("calibration host evidence: " + str(directory))
    if error is not None:
        raise ValueError(str(error))
    if diagnostic_error is not None:
        raise ValueError("node diagnostic: " + str(diagnostic_error))


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as exc:
        diagnostic("calibration FAIL: " + str(exc))
        sys.exit(1)
