// Namespace teardown proof (issue #3644, PRD #3831 section 8.1, plan m6).
//
// A delayed child process of the predecessor worker holds a pending git-config write against a bare
// repository on a volume that persists across restart. The container's PID-namespace init is killed, so the
// kernel kills every process in that namespace. The same container is started again on the same volume (the
// compose restart: same slot, new execution). The successor writes its boundary record through the real
// agent/src/restore-boundary.ts, and the proof shows that the child's write never lands before the
// child's deadline has passed, through to the successor's restore admission (evaluateComposeRestore), and
// that the execution tuple the R5 comparison reads changed: initStartTicks is strictly newer on the same
// boot (bootId equal); nsIno may repeat because the kernel can reuse the freed namespace inode number.
//
// This test is host-driven: it runs the Docker CLI against the worker image and removes only the
// container and volume it created, by exact name. It is never skipped: a missing Docker or image fails it,
// like the other worker-uid proofs. Set WORKER_UID_IMAGE to reuse an image (default wuid-2134-base:local,
// the tag e2e/worker-uid/run.sh builds).
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const image = process.env.WORKER_UID_IMAGE ?? "wuid-2134-base:local";
const name = `cdr-3644-nsproof-${process.pid}-${randomBytes(3).toString("hex")}`;
const volume = `${name}-data`;
const proof = "/app/uzi-worker-uid-proof";
const probe = `${proof}/e2e/worker-uid/namespace-teardown-probe.mts`;
const tsx = "/app/node_modules/tsx/dist/loader.mjs";
const P = "nsproof-predecessor-incarnation";
const S = "nsproof-successor-incarnation";
const TOKEN = "nsproof-worker-token-fixture";
// Seconds from the child's start to its scheduled write. The restart, the successor's record and the
// admission all have to fit in this window for the proof to say anything about the pending write.
const DELAY_SECONDS = 40;

function docker(args: string[], seconds = 120): { status: number | null; stdout: string; stderr: string } {
  const r = spawnSync("docker", args, { encoding: "utf8", timeout: seconds * 1000, killSignal: "SIGKILL" });
  assert.equal(r.error, undefined, `docker ${args[0]}: ${String(r.error)}`);
  return { status: r.status, stdout: r.stdout, stderr: r.stderr };
}
function dockerOk(args: string[], seconds?: number): string {
  const r = docker(args, seconds);
  assert.equal(r.status, 0, `docker ${args.join(" ").slice(0, 120)} failed: ${r.stderr}`);
  return r.stdout.trim();
}
function sh(script: string): string { return dockerOk(["exec", name, "sh", "-c", script]); }
function probeJson(...args: string[]): any {
  const out = dockerOk(["exec", "--env", `UZI_NSPROOF_TOKEN=${TOKEN}`, name, "node", "--import", tsx, probe, ...args], 90);
  return JSON.parse(out.split("\n").filter(Boolean).at(-1)!);
}
/** A delayed git config writer that keeps running inside the container until its deadline. */
function startDelayedWriter(key: string, deadlineEpochSeconds: number): void {
  const script = `NSPROOF_CHILD=${key}; until [ "$(date +%s)" -ge ${deadlineEpochSeconds} ]; do sleep 0.2; done; ` +
    `git --git-dir=/data/bare.git config uzi.${key} written`;
  dockerOk(["exec", "-d", name, "sh", "-c", script]);
}
function liveChildren(): string[] {
  return sh("ps -o pid,args").split("\n").filter(line => line.includes("NSPROOF_CHILD="));
}
function state(): string { return dockerOk(["inspect", "--format", "{{.State.Status}}", name]); }
function waitFor(label: string, ok: () => boolean, seconds: number): void {
  const end = Date.now() + seconds * 1000;
  while (!ok()) {
    assert.ok(Date.now() < end, `timed out waiting for ${label}`);
    spawnSync("sleep", ["0.25"]);
  }
}
const sleepMs = (ms: number) => spawnSync("sleep", [String(ms / 1000)]);

test("a predecessor's pending git-config write dies with its PID namespace and cannot land before the successor's restore admission", { timeout: 300_000 }, async () => {
  assert.equal(process.platform, "linux");
  dockerOk(["image", "inspect", "--format", "{{.Id}}", image], 20);
  try {
    dockerOk(["volume", "create", "--label", `io.uzi.cdr-3644-nsproof=${name}`, volume], 20);
    // The same shape as the single-uid worker-proof containers: worker uid, no capabilities, no
    // network, tmpfs /nix (an unmasked image /nix is a multi-GB copy). /data is the persistent volume.
    const bind = (src: string, dst: string) => `type=bind,src=${path.join(repo, src)},dst=${proof}/${dst},readonly`;
    dockerOk(["create", "--name", name, "--pull", "never", "--network", "none", "--user", "10001:10001",
      "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--label", `io.uzi.cdr-3644-nsproof=${name}`,
      "--env", "HOME=/tmp", "--workdir", "/tmp", "--tmpfs", "/nix",
      "--mount", `type=volume,src=${volume},dst=/data`,
      "--tmpfs", `${proof}:exec,uid=10001,gid=10001,mode=0755`,
      "--mount", bind("e2e/worker-uid", "e2e/worker-uid"), "--mount", bind("agent/src", "agent/src"),
      "--mount", bind("agent/tsconfig.json", "agent/tsconfig.json"), "--mount", bind("agent/package.json", "agent/package.json"),
      "--entrypoint", "/bin/sh", image, "-c", "exec sleep 86400"], 60);
    dockerOk(["start", name], 60);
    waitFor("first start", () => state() === "running", 20);

    // The authority store creates `.uzi-authority` (0700) before any boundary record is written; the
    // record writer only adds `boundaries/` beneath it.
    sh("test \"$(id -u)\" = 10001 && mkdir -m 700 /data/.uzi-authority && git init --bare -q /data/bare.git && git --git-dir=/data/bare.git config uzi.seed 1");

    // Positive control: the writer mechanism lands its config write at its deadline in a live namespace.
    const controlDeadline = Math.floor(Date.now() / 1000) + 3;
    startDelayedWriter("control", controlDeadline);
    waitFor("control write", () => sh("git --git-dir=/data/bare.git config --get uzi.control || true") === "written", 20);

    // Predecessor P: its own boundary record, then the pending write.
    const p = probeJson("boundary", P);
    assert.equal(p.result.ok, true, JSON.stringify(p));
    const pExecution = p.result.record.execution;
    const deadline = Math.floor(Date.now() / 1000) + DELAY_SECONDS;
    startDelayedWriter("late", deadline);
    waitFor("child running", () => liveChildren().length === 1, 10);
    const beforeKill = probeJson("snapshot");
    assert.ok(!beforeKill.text.includes("late"), "the pending write has not landed yet");
    assert.ok(beforeKill.at < deadline * 1000, "the kill happens before the child's deadline");

    // Kill the namespace init. The kernel then kills every process in the PID namespace.
    dockerOk(["kill", "--signal", "KILL", name], 30);
    waitFor("container exit", () => state() === "exited", 30);

    // Restart: same container, same volume, new PID namespace.
    dockerOk(["start", name], 60);
    waitFor("restart", () => state() === "running", 20);
    assert.deepEqual(liveChildren(), [], "the predecessor's child is not alive in the successor's namespace");

    // The successor's boundary record, still before the child's deadline.
    const s = probeJson("boundary", S);
    assert.equal(s.result.ok, true, JSON.stringify(s));
    assert.ok(s.writtenAt < deadline * 1000, `the successor's record must be durable before the child's deadline (${s.writtenAt} vs ${deadline * 1000})`);
    const sExecution = s.result.record.execution;
    assert.equal(s.result.record.slot.containerId, p.result.record.slot.containerId, "same compose slot");

    // The execution identity the R5 comparison reads has changed.
    assert.equal(sExecution.bootId, pExecution.bootId, "same host boot");
    // The kernel may hand the freed namespace inode number to the new namespace, so nsIno alone is not
    // evidence of a new namespace (observed on this host); R5 refuses only when nsIno AND initStartTicks both
    // match, so the new init's start time is what distinguishes the executions.
    assert.ok(BigInt(sExecution.initStartTicks) > BigInt(pExecution.initStartTicks), "successor init started after the predecessor's");

    // Wait until the child's deadline is well past, then take the admission instant.
    const settle = deadline * 1000 + 3000 - Date.now();
    if (settle > 0) sleepMs(settle);
    assert.ok(Date.now() > deadline * 1000 + 2000, "admission is evaluated after the child's deadline");
    const admission = probeJson("admit", P, S);
    assert.ok(admission.before.at > deadline * 1000, "the admission snapshot is taken after the deadline");

    assert.deepEqual(admission.decision, { admit: true, verdict: { predecessorIncarnation: P, slot: s.result.record.slot, boot: "same_boot" } },
      "the R5 comparison admits the restart: a distinct, strictly older execution");
    assert.equal(admission.live.ok, true);
    assert.equal(admission.live.identity.execution.initStartTicks, sExecution.initStartTicks, "the live identity is the successor's");

    // The child's write never landed: not at the admission instant, and not after.
    for (const snap of [admission.before, admission.after, probeJson("snapshot")]) {
      assert.equal(snap.sha256, beforeKill.sha256, "bare config bytes are unchanged since before the kill");
      assert.equal(snap.ino, beforeKill.ino);
      assert.equal(snap.mtimeMs, beforeKill.mtimeMs);
      assert.ok(!snap.text.includes("late"));
      assert.equal(snap.lockPresent, false, "no config.lock left by a half-finished write");
    }
    assert.equal(sh("git --git-dir=/data/bare.git config --get uzi.late; echo rc=$?").endsWith("rc=1"), true, "uzi.late is unset");
    assert.deepEqual(liveChildren(), [], "no child process exists after the deadline");
  } finally {
    // Exact names only; both were created by this invocation. Each cleanup call is attempted regardless of
    // the other and never throws, so a cleanup failure cannot mask the test's own failure.
    for (const args of [["rm", "--force", "--volumes", name], ["volume", "rm", volume]]) {
      const r = spawnSync("docker", args, { encoding: "utf8", timeout: 60_000, killSignal: "SIGKILL" });
      if (r.error !== undefined || r.status !== 0) {
        console.error(`cleanup: docker ${args.join(" ")} failed: ${String(r.error ?? r.stderr)}`);
      }
    }
  }
});
