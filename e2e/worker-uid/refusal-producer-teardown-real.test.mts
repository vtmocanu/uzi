// Refusal producer teardown proof (issue #4481, plan m3).
//
// A worker process admits refusal generation G in a bare repository on a persistent volume, then starts two
// delayed trusted producers that would each create `refs/uzi-incoming/<run>/<G>/<id>` (one in the worker's
// process group, one in a new session). Only the worker's node process is SIGKILLed, not PID 1. Then:
//   (a) ORDERING: a successor process (an idle container on the same volume, started before the kill) runs the
//       real GitCache.refusalReleaseGate for the same run, generation and head immediately after the kill and
//       before the worker container's teardown is awaited, while the producers' deadline has not passed (they
//       were observed alive just before the kill), and gets `ineligible` (admission from another process), so
//       it would send nothing. The same state under the dead worker's nonce is `eligible`.
//       The same ordering is also observed live: before the kill, a second process inside the worker container
//       runs the gate while both producers are confirmed alive, and gets `ineligible`.
//   (b) EVENTUAL CLEANUP: the worker container stops (the worker's npm and tini exit), and after the
//       producers' deadline neither ref exists in a fresh container and the gate evidence is unchanged. Producer
//       processes are asserted alive before the kill and the container asserted stopped after it; no process
//       table is read afterwards.
//
// The worker is started through the shipped entrypoint chain: uzi-entrypoint (the image ENTRYPOINT) execs tini
// as PID 1, which runs the image CMD `npm run start`, which runs `tsx src/main.ts`, which runs node. The only
// substitution is `/app/src/main.ts`, bind-mounted read-only with a one-line import of the probe. The
// container starts as uid 10001 with no capabilities, so the entrypoint takes its non-root branch (the PRD #58
// k8s runAsUser shape; the root-started compose branch needs cap_add and is not exercised here).
//
// This test is host-driven: it runs the Docker CLI against the worker image and removes only the containers
// and volume it created, by exact name (`cdr-4481-*`). It is never skipped: a missing Docker or image fails
// it. Set WORKER_UID_IMAGE to reuse an image (default wuid-2134-base:local).
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";

const repo = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const image = process.env.WORKER_UID_IMAGE ?? "wuid-2134-base:local";
const base = `cdr-4481-refusal-${process.pid}-${randomBytes(3).toString("hex")}`;
const worker = `${base}-worker`;
const successor = `${base}-successor`;
const volume = `${base}-data`;
const label = `io.uzi.cdr-4481-refusal=${base}`;
const proof = "/app/uzi-worker-uid-proof";
const probe = `${proof}/e2e/worker-uid/refusal-producer-teardown-probe.mts`;
const tsx = "/app/node_modules/tsx/dist/loader.mjs";
// Seconds from the producers' start to their scheduled write. The kill, the successor's start and its gate
// all have to fit inside this window for the ordering assertion to say anything.
const DELAY_SECONDS = Number(process.env.RT_DELAY_SECONDS ?? "40");

type Run = { status: number | null; stdout: string; stderr: string };
function docker(args: string[], seconds = 120): Run {
  const r = spawnSync("docker", args, { encoding: "utf8", timeout: seconds * 1000, killSignal: "SIGKILL" });
  assert.equal(r.error, undefined, `docker ${args[0]}: ${String(r.error)}`);
  return { status: r.status, stdout: r.stdout, stderr: r.stderr };
}
function dockerOk(args: string[], seconds?: number): string {
  const r = docker(args, seconds);
  assert.equal(r.status, 0, `docker ${args.join(" ").slice(0, 120)} failed: ${r.stderr}`);
  return r.stdout.trim();
}
const state = (name: string): string => dockerOk(["inspect", "--format", "{{.State.Status}}", name]);
function waitFor(label: string, ok: () => boolean, seconds: number): void {
  const end = Date.now() + seconds * 1000;
  while (!ok()) {
    assert.ok(Date.now() < end, `timed out waiting for ${label}`);
    spawnSync("sleep", ["0.25"]);
  }
}
function probeJson(...args: string[]): any {
  const out = dockerOk(["exec", successor, "node", "--import", tsx, probe, ...args], 90);
  return JSON.parse(out.split("\n").filter(Boolean).at(-1)!);
}
/** Processes of the worker container as {pid, ppid, pgid, args}. */
function psTable(): { pid: number; ppid: number; pgid: number; args: string }[] {
  return dockerOk(["exec", worker, "ps", "-o", "pid,ppid,pgid,args"]).split("\n").slice(1).map((line) => {
    const m = /^\s*(\d+)\s+(\d+)\s+(\d+)\s+(.*)$/.exec(line);
    assert.ok(m, `unparseable ps line: ${line}`);
    return { pid: Number(m[1]), ppid: Number(m[2]), pgid: Number(m[3]), args: m[4]! };
  });
}
test("a SIGKILLed worker's delayed refusal producers cannot land before a successor's gate says ineligible, and never land after teardown", { timeout: 300_000 }, async () => {
  assert.equal(process.platform, "linux");
  dockerOk(["image", "inspect", "--format", "{{.Id}}", image], 20);
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "cdr-4481-main-"));
  try {
    fs.chmodSync(tmp, 0o755);
    const mainStub = path.join(tmp, "main.ts");
    fs.writeFileSync(mainStub, `import ${JSON.stringify(probe)};\n`, { mode: 0o644 });
    dockerOk(["volume", "create", "--label", label, volume], 20);
    const bind = (src: string, dst: string) => `type=bind,src=${src},dst=${dst},readonly`;
    const proofBind = (src: string, dst: string) => bind(path.join(repo, src), `${proof}/${dst}`);
    const common = ["--pull", "never", "--network", "none", "--user", "10001:10001", "--cap-drop", "ALL",
      "--security-opt", "no-new-privileges", "--label", label, "--tmpfs", "/nix",
      "--mount", `type=volume,src=${volume},dst=/data`,
      "--tmpfs", `${proof}:exec,uid=10001,gid=10001,mode=0755`,
      "--mount", proofBind("e2e/worker-uid", "e2e/worker-uid"), "--mount", proofBind("agent/src", "agent/src"),
      "--mount", proofBind("agent/tsconfig.json", "agent/tsconfig.json"), "--mount", proofBind("agent/package.json", "agent/package.json")];

    // The worker: no entrypoint or command override, so the image's ENTRYPOINT and CMD run.
    const cfg = JSON.parse(dockerOk(["image", "inspect", "--format", "{{json .Config}}", image], 20));
    assert.deepEqual(cfg.Entrypoint, ["/usr/local/sbin/uzi-entrypoint"]);
    assert.deepEqual(cfg.Cmd, ["npm", "run", "start"]);
    dockerOk(["create", "--name", worker, ...common, "--env", `RT_DELAY_SECONDS=${DELAY_SECONDS}`,
      "--mount", bind(mainStub, "/app/src/main.ts"), image], 60);
    dockerOk(["start", worker], 60);

    // Readiness: the worker printed its line after admission, the gate control and the producers' start.
    let ready: any;
    waitFor("worker READY line", () => {
      const logs = docker(["logs", worker]);
      const line = (logs.stdout + logs.stderr).split("\n").find((l) => l.startsWith("READY "));
      if (line) ready = JSON.parse(line.slice("READY ".length));
      assert.equal(state(worker), "running", `worker exited before READY: ${logs.stdout}${logs.stderr}`);
      return ready !== undefined;
    }, 120);
    assert.equal(ready.admitted, "written", "the real admitRefusalProvenance wrote generation G");
    assert.equal(ready.gateSame.kind, "eligible", `the admitting process's own gate is eligible: ${JSON.stringify(ready.gateSame)}`);
    assert.equal(ready.controlLanded, true, "positive control: the producer mechanism lands its ref in a live container");
    assert.equal(ready.stagedBefore.includes("refs/uzi-incoming/"), false, "no staged ref before the deadline");
    assert.ok(Date.now() < ready.deadlineMs - 15_000, "enough of the producers' window remains for kill and successor gate");

    // The achieved process chain: tini (PID 1) -> npm run start -> ... -> the worker node process.
    const table = psTable();
    const byPid = new Map(table.map((p) => [p.pid, p]));
    const chain: string[] = [];
    for (let p = byPid.get(ready.pid); p; p = byPid.get(p.ppid)) chain.unshift(`${p.pid}:${p.args.slice(0, 80)}`);
    console.log("process chain (pid:args), PID 1 first:\n  " + chain.join("\n  "));
    assert.match(byPid.get(1)!.args, /tini/, "PID 1 is tini");
    const npm = table.find((p) => /^npm run start$/.test(p.args));
    assert.ok(npm, "npm run start is running");
    assert.equal(npm.ppid, 1, "npm is tini's child");
    assert.ok(chain.some((c) => c.startsWith(`${npm.pid}:`)), "the worker node process descends from npm");
    assert.ok(chain.some((c) => /tsx/.test(c)), "the worker is run through tsx");
    assert.ok(byPid.get(ready.pid)!.args.includes("main.ts"), "the worker node process runs src/main.ts");
    assert.notEqual(ready.pid, 1);

    // Both producers are alive: one in the worker's process group, one in its own session.
    const producers = ready.producers.map((p: any) => table.find((t) => t.args.includes(`RT4481_PRODUCER=${p.ref}`)));
    assert.ok(producers.every(Boolean), `both producers are running: ${JSON.stringify(table.map((t) => t.args.slice(0, 60)))}`);
    const workerPgid = byPid.get(ready.pid)!.pgid;
    assert.equal(producers[0].pgid, workerPgid, "producer 1 is in the worker's process group");
    assert.notEqual(producers[1].pgid, workerPgid, "producer 2 is outside the worker's process group");

    // The successor container exists and is warm BEFORE the kill, on the same volume, idle.
    dockerOk(["create", "--name", successor, ...common, "--entrypoint", "/bin/sh", image, "-c", "exec sleep 86400"], 60);
    dockerOk(["start", successor], 60);
    waitFor("successor", () => state(successor) === "running", 20);
    assert.deepEqual(probeJson("refs").refs.split("\n").filter((r: string) => r.startsWith("refs/uzi-incoming/")), [], "nothing staged yet (warm-up)");
    assert.ok(Date.now() < ready.deadlineMs - 15_000, "enough of the producers' window remains after the warm-up");

    // Live-producer ordering: a SECOND process inside the worker container (same uid, fresh process nonce)
    // runs the real gate while the producers are pending.
    const live = JSON.parse(dockerOk(["exec", worker, "node", "--import", tsx, probe, "gate", "-", ready.head], 90).split("\n").filter(Boolean).at(-1)!);
    assert.equal(live.gate.kind, "ineligible", JSON.stringify(live));
    assert.match(live.gate.reason, /admission for generation 1 is from another process/);
    assert.notEqual(live.nonce, ready.nonce, "the second process has its own nonce");
    assert.ok(live.at < ready.deadlineMs, "the in-container gate ran before the producers' deadline");
    // Re-read the process table immediately before the kill: both producers are still pending.
    const justBefore = psTable();
    const stillPending = ready.producers.filter((p: any) => justBefore.some((t) => t.args.includes(`RT4481_PRODUCER=${p.ref}`)));
    assert.equal(stillPending.length, ready.producers.length, "both producers are alive immediately before the kill");
    assert.ok(justBefore.some((t) => t.pid === ready.pid), "the worker node process is alive immediately before the kill");
    console.log(`live ordering: in-container gate margin ${ready.deadlineMs - live.at}ms; producers alive just before kill: ${stillPending.length}`);

    // SIGKILL only the worker node process, never PID 1.
    const killedAt = Date.now();
    dockerOk(["exec", worker, "kill", "-9", String(ready.pid)], 30);

    // (a) ORDERING, before awaiting teardown: the successor's real gate, right after the kill, with the producers'
    // deadline still in the future.
    const ineligible = probeJson("gate", "-", ready.head);
    const margin = ready.deadlineMs - ineligible.at;
    console.log(`ordering: gate ran ${ineligible.at - killedAt}ms after the kill; margin deadlineMs - gateAt = ${margin}ms; worker state at that moment: ${state(worker)}`);
    assert.ok(margin > 0, `the gate ran before the producers' deadline (${ineligible.at} vs ${ready.deadlineMs})`);
    assert.ok(ineligible.at > killedAt);
    assert.equal(ineligible.gate.kind, "ineligible", JSON.stringify(ineligible));
    assert.match(ineligible.gate.reason, /admission for generation 1 is from another process/, "the successor is refused because the history was admitted by a dead process");
    assert.notEqual(ineligible.nonce, ready.nonce, "the successor has its own process nonce");
    // Control: the same state, head and code under the dead worker's nonce is eligible, so the nonce is the only difference.
    const control = probeJson("gate", ready.nonce, ready.head);
    assert.equal(control.gate.kind, "eligible", JSON.stringify(control));
    assert.ok(control.at < ready.deadlineMs, "the control also ran before the deadline");
    assert.deepEqual(probeJson("refs").refs.split("\n").filter((r: string) => r.startsWith("refs/uzi-incoming/")), [], "nothing was staged at gate time");

    // (b, first half) the worker's npm and tini exit, so the container stops.
    const exitCode = dockerOk(["wait", worker], 60);
    console.log(`worker container exit code after SIGKILL of node: ${exitCode}`);
    assert.equal(state(worker), "exited");
    assert.notEqual(docker(["top", worker]).status, 0, "no process table for a stopped container");

    // (b, second half) wait out the deadline in the fresh container: neither ref exists.
    const settle = ready.deadlineMs + 4000 - Date.now();
    if (settle > 0) spawnSync("sleep", [String(settle / 1000)]);
    const after = probeJson("refs");
    assert.ok(after.at > ready.deadlineMs + 3000, "refs were read well after the producers' deadline");
    for (const p of ready.producers) assert.ok(!after.refs.includes(p.ref), `${p.ref} was not created`);
    assert.deepEqual(after.refs.split("\n").filter((r: string) => r.startsWith("refs/uzi-incoming/")), []);
    assert.equal(probeJson("gate", "-", ready.head).gate.kind, "ineligible", "still ineligible after the deadline");
    assert.equal(probeJson("gate", ready.nonce, ready.head).gate.kind, "eligible", "the dead worker's evidence is unchanged after the deadline");
    assert.equal(state(worker), "exited", "the worker container is still stopped");
  } finally {
    // Exact names only; all were created by this invocation. Each cleanup is attempted regardless of the others
    // and never throws, so a cleanup failure cannot mask the test's own failure.
    for (const args of [["rm", "--force", "--volumes", worker], ["rm", "--force", "--volumes", successor], ["volume", "rm", volume]]) {
      const r = spawnSync("docker", args, { encoding: "utf8", timeout: 60_000, killSignal: "SIGKILL" });
      if (r.error !== undefined || r.status !== 0) console.error(`cleanup: docker ${args.join(" ")} failed: ${String(r.error ?? r.stderr)}`);
    }
    fs.rmSync(tmp, { recursive: true, force: true });
  }
});
