import assert from "node:assert/strict";
import { it } from "node:test";
import { Worker } from "../src/worker.js";
import { loadConfig } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { CrossCheckRunner } from "../src/cross-check-runner.js";
import { GitCache } from "../src/git.js";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { makeFixture } from "./fixture-repo.js";
import type { StatsCollector } from "../src/stats.js";
import type { DiskPressureController } from "../src/disk-reclaim.js";
import type { DindMaintenanceController } from "../src/dind-maintenance.js";
import type { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { DindPruneGate } from "../src/dind-prune.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { ActiveSnapshot, ClaimResponse } from "../src/protocol.js";
import { latchResidueQuarantine } from "../src/residue-quarantine.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";
import { recordingLogger, makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";

resetResidueQuarantineAfterEach();
const tick = (ms = 2) => new Promise<void>((resolve) => setTimeout(resolve, ms));
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => { resolve = r; });
  return { promise, resolve };
}
// Bounded observation of the public worker/client seams, never unbounded polling.
async function until(predicate: () => boolean) {
  for (let attempt = 0; attempt < 500 && !predicate(); attempt++) await tick();
  assert.ok(predicate(), "worker did not reach expected state");
}
function claim(id: string, kind = "cross_check"): ClaimResponse {
  return { run_id: id, kind, claim_generation: 2 } as ClaimResponse;
}

function build(slots = 1, registry = new ActiveRunRegistry()) {
  const controller = new AbortController();
  const { logger, lines: recorded } = recordingLogger();
  const lines = recorded as Array<{ level?: string; msg?: string; error?: string }>;
  const registrations: Parameters<WorkerClient["register"]>[] = [];
  const heartbeats: ActiveSnapshot[] = [];
  const snapshots: ActiveSnapshot[] = [];
  const gate = new DindPruneGate();
  const state = { diskBlocked: false, ended: 0, protected: 0, admissions: 0 };
  const client = {
    register: async (...args: Parameters<WorkerClient["register"]>) => {
      registrations.push(args);
      return { worker_id: "w" };
    },
    hasFeature: (feature: string) => feature === "active_run_snapshot",
    heartbeat: async (_stats: unknown, _outbox: unknown, snapshot?: ActiveSnapshot) => {
      if (snapshot) heartbeats.push(snapshot);
    },
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async () => null,
    claimCrossCheck: async (snapshot?: ActiveSnapshot): Promise<ClaimResponse | null> => {
      if (snapshot) snapshots.push(snapshot);
      return null;
    },
    reportState: async () => ({ applied: true, status: "failed" }),
  };
  const runner = {
    resumePendingRecoveries: async () => {},
    settlePendingPredecessors: async () => {},
    execute: async (_claim: ClaimResponse) => {},
  };
  const checker: Pick<CrossCheckRunner, "execute"> = { execute: async () => {} };
  const maintenance = {
    gate, register: () => {}, loop: async () => {}, observe: () => {},
    acknowledgement: () => undefined, noteActivityEnded: () => { state.ended++; },
  } as unknown as DindMaintenanceController;
  const disk = {
    claimsBlocked: () => state.diskBlocked, observe: () => {}, loop: async () => {},
  } as unknown as DiskPressureController;
  const admission = {
    start: async () => {}, loop: async () => {},
    acquireAdmission: async (_signal: AbortSignal) => {
      state.admissions++;
      return () => { state.admissions--; };
    },
    protectExecution: (_runId: string) => {
      state.protected++;
      return () => { state.protected--; };
    },
  };
  const config = loadConfig({ UZI_API_URL: "http://example.com", UZI_WORKER_TOKEN: "worker-token",
    WORKER_CROSS_CHECK_SLOTS: String(slots) });
  config.pollIntervalMs = config.chatPollMs = config.heartbeatIntervalMs = 2;
  const worker = new Worker(config, client as unknown as WorkerClient, runner as unknown as RunRunner,
    {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, logger,
    () => ({ ok: true, missing: [] }), undefined, undefined, registry, undefined, undefined,
    disk, undefined, () => ({ collect: () => ({}) }) as unknown as StatsCollector,
    undefined, undefined, maintenance, admission as unknown as TerminalRejectionCoordinator, checker);
  return { worker, client, runner, checker, registry, controller, state, gate,
    registrations, heartbeats, snapshots, lines, admission };
}

for (const slots of [0, 1, 16]) {
  it(`worker registers cap ${slots}, with lane capability only for positive capacity`, async () => {
    const h = build(slots);
    let polls = 0;
    h.client.claimCrossCheck = async () => { polls++; return null; };
    const done = h.worker.run(h.controller.signal);
    try {
      await until(() => h.registrations.length === 1 && h.heartbeats.length >= 3);
      const args = h.registrations[0]!;
      assert.equal(args[6], slots);
      assert.ok(args[4]?.includes("cross_check_v1"));
      assert.equal(args[4]?.includes("cross_check_lane_v1"), slots > 0);
      assert.equal(polls > 0, slots > 0);
    } finally { h.controller.abort(); await done; }
  });
}

it("cross-check cap is independent of a full run slot, snapshots see both, and shutdown drains every checker", async () => {
  const h = build(2);
  const release = deferred();
  let runClaims = 0;
  let checkClaims = 0;
  const executed: string[] = [];
  const ended: string[] = [];
  h.client.claimRun = async () => claim("lead-" + ++runClaims, "issue");
  h.client.claimCrossCheck = async (snapshot) => {
    if (snapshot) h.snapshots.push(snapshot);
    return claim("check-" + ++checkClaims);
  };
  h.runner.execute = async (c) => {
    h.registry.add(c.run_id, c.claim_generation!);
    await release.promise;
    h.registry.remove(c.run_id);
  };
  h.checker.execute = async (c, signal) => {
    executed.push(c.run_id);
    h.registry.add(c.run_id, c.claim_generation!);
    await new Promise<void>((resolve) => signal!.addEventListener("abort", () => resolve(), { once: true }));
    await release.promise;
    h.registry.remove(c.run_id);
    ended.push(c.run_id);
  };
  const done = h.worker.run(h.controller.signal);
  let drained = false;
  void done.then(() => { drained = true; });
  try {
    await until(() => executed.length === 2 && h.heartbeats.some((s) => s.active.length === 3));
    await tick(20);
    assert.equal(runClaims, 1);
    assert.equal(checkClaims, 2, "never polls beyond the dedicated cap");
    assert.equal(h.worker.isIdle(), false);
    assert.ok(h.snapshots.some((s) => s.active.some((e) => e.run_id === "lead-1")));
    assert.ok(h.snapshots.some((s) => s.active.some((e) => e.run_id === "check-1")));
    const epochs = [...h.snapshots, ...h.heartbeats].map((s) => s.snapshot_epoch);
    assert.equal(new Set(epochs).size, epochs.length, "claims and heartbeat share one epoch counter");
    h.controller.abort();
    await tick(10);
    assert.equal(drained, false, "shutdown waits for execution cleanup after abort");
  } finally { h.controller.abort(); release.resolve(); await done; }
  assert.deepEqual(ended.sort(), ["check-1", "check-2"]);
  assert.equal(h.worker.isIdle(), true);
  assert.equal(h.state.protected, 0);
  assert.equal(h.state.admissions, 0);
  assert.equal(h.gate.inFlightClaims(), 0);
});

it("code child dispatch uses the dedicated pool through settlement, reader cleanup and cancellation", async (t) => {
  const h = build(1);
  const fx = makeFixture({ "source.txt": "committed code\n" });
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fx.originPath, "");
  const head = execFileSync("git", ["-C", bare, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  const leadIds: string[] = [randomUUID(), randomUUID()];
  const childIds = [randomUUID(), randomUUID()];
  for (const lead of leadIds) await git.pinCodeSnapshot(bare, lead, head, head);
  const releaseLead = deferred();
  const firstTurn = deferred();
  const cleanupStarted = deferred();
  const cleanupRelease = deferred();
  let runClaims = 0;
  let codeClaims = 0;
  let modelCalls = 0;
  const states = new Map<string, string>();
  const verdicts: string[] = [];
  const actualClient = {
    reportState: async (id: string, body: { status: string }) => {
      states.set(id, body.status);
      return { applied: true };
    },
    getInputs: async () => ({ inputs: [] }),
    postMessages: async () => {},
    getCodeSnapshotCleanup: async (lead: string) => {
      const childId = childIds[leadIds.indexOf(lead)]!;
      const status = states.get(childId);
      return { protocol: "code_snapshot_cleanup_v1", lead_run_id: lead, head_commit: head,
        outcome: status === "completed" ? "completed" : "failed", lead_status: "running",
        owned_by_worker: true, checker_run_id: childId, checker_claim_generation: 2,
        checker_status: status };
    },
    reportCodeCrossCheckVerdict: async (id: string) => { verdicts.push(id); },
  } as unknown as WorkerClient;
  const child = (n: number) => makeClaim({
    run_id: childIds[n - 1]!, kind: "cross_check", claim_generation: 2,
    repo: { id: "repo", url: "https://example.com/repo", clone_url: fx.originPath },
    secrets: { forge_pat: "", codex: { auth_mode: "api_key", access_token: "fixture-access", capability: "fixture-cap" } },
    cross_check: { stage: "code", lead_run_id: leadIds[n - 1]!, round: 1, head_commit: head, base_commit: head,
      candidate_digest: "a".repeat(64), deadline_at: new Date(Date.now() + 30000).toISOString(),
      plan_md: "", milestones: [], required_capabilities: [], required_tools: [], size_class: "",
      code_context: {}, guidance_snapshot: "" },
  });
  t.mock.method(git, "ensureClone", async () => { assert.fail("code dispatch cannot fetch origin"); });
  const remove = git.removeRunnerClone.bind(git);
  t.mock.method(git, "removeRunnerClone", async (checkout: string) => {
    if (states.get(childIds[0]!) === "completed" && codeClaims === 1) {
      assert.equal(await git.codeSnapshotTip(bare, leadIds[0]!), head);
      cleanupStarted.resolve();
      await cleanupRelease.promise;
    }
    await remove(checkout);
  });
  const checker = new CrossCheckRunner(actualClient, git, nullLogger(), {
    homeRoot: fx.dataDir, pollMs: 2, activeRuns: h.registry,
    model: { run: async (_c, checkout, _home, signal, _usage, cleanupConfirmed) => {
      try {
        assert.equal(execFileSync("git", ["-C", checkout, "rev-parse", "HEAD"], { encoding: "utf8" }).trim(), head);
        if (++modelCalls === 1) { await firstTurn.promise; return '{"findings":[]}'; }
        await new Promise<void>((resolve) => {
          if (signal.aborted) resolve();
          else signal.addEventListener("abort", () => resolve(), { once: true });
        });
        signal.throwIfAborted();
        return '{"findings":[]}';
      } finally { cleanupConfirmed?.(); }
    } },
  });
  h.runner.execute = async () => { await releaseLead.promise; };
  h.client.claimRun = async () => claim("ordinary-" + ++runClaims, "issue");
  h.client.claimCrossCheck = async () => ++codeClaims <= 2 ? child(codeClaims) : null;
  h.checker.execute = (c, signal) => checker.execute(c, signal);
  const done = h.worker.run(h.controller.signal);
  try {
    await until(() => modelCalls === 1 && runClaims === 1);
    await tick(10);
    assert.equal(codeClaims, 1, "no overclaim while model reads");
    firstTurn.resolve();
    await cleanupStarted.promise;
    await tick(10);
    assert.equal(codeClaims, 1, "slot includes reader cleanup after terminal settlement");
    cleanupRelease.resolve();
    await until(() => modelCalls === 2);
    await assert.rejects(git.codeSnapshotTip(bare, leadIds[0]!));
    assert.equal(runClaims, 1, "code lane does not occupy or free ordinary slots");
    assert.equal(await git.codeSnapshotTip(bare, leadIds[1]!), head, "live cancelled reader still owns pin");
  } finally {
    firstTurn.resolve(); cleanupRelease.resolve(); releaseLead.resolve();
    h.controller.abort();
    await done;
    try {
      await assert.rejects(git.codeSnapshotTip(bare, leadIds[1]!), "cancelled child releases pin after cleanup");
    } finally { fx.cleanup(); }
  }
  assert.deepEqual(verdicts, childIds);
  assert.equal(states.get(childIds[1]!), "failed");
  assert.equal(h.state.protected, 0);
  assert.equal(h.state.admissions, 0);
  assert.equal(h.worker.isIdle(), true);
});

it("a checker alone makes the worker busy, and freeing its slot admits the queued checker", async () => {
  const h = build();
  const first = deferred();
  const second = deferred();
  let claims = 0;
  const started: string[] = [];
  h.client.claimCrossCheck = async () => ++claims <= 2 ? claim("check-" + claims) : null;
  // Deliberately no registry entries: isIdle must count the promise pool itself.
  h.checker.execute = async (c) => {
    started.push(c.run_id);
    await (c.run_id === "check-1" ? first.promise : second.promise);
  };
  const done = h.worker.run(h.controller.signal);
  try {
    await until(() => started.length === 1);
    await tick(10);
    assert.equal(h.registry.size, 0);
    assert.equal(h.worker.isIdle(), false);
    assert.equal(claims, 1);
    first.resolve();
    await until(() => started.length === 2);
    assert.equal(h.worker.isIdle(), false);
    second.resolve();
    await until(() => h.worker.isIdle());
    assert.deepEqual(started, ["check-1", "check-2"]);
    assert.equal(h.state.ended, 2);
  } finally { h.controller.abort(); first.resolve(); second.resolve(); await done; }
});

it("refuses wrong-kind and duplicate claims before any runner dispatch", async () => {
  const h = build();
  h.registry.add("already-live", 2);
  const queue = [claim("wrong", "task"), claim("already-live"), claim("good")];
  h.client.claimCrossCheck = async () => queue.shift() ?? null;
  const executed: string[] = [];
  h.runner.execute = async () => { assert.fail("ordinary runner dispatched"); };
  h.checker.execute = async (c) => { executed.push(c.run_id); };
  const done = h.worker.run(h.controller.signal);
  try {
    await until(() => executed.length === 1);
    assert.deepEqual(executed, ["good"]);
    assert.ok(h.lines.some((line) => line.level === "error" && String(line.msg).includes("non-cross-check claim")));
    assert.ok(h.lines.some((line) => line.level === "error" && String(line.msg).includes("already executing")));
  } finally { h.controller.abort(); h.registry.remove("already-live"); await done; }
});

it("claim failures, synchronous runner throws and rejected executions release protection and allow the next checker", async () => {
  const h = build();
  let claims = 0;
  let executions = 0;
  h.client.claimCrossCheck = async () => {
    if (++claims === 1) throw new Error("transport failed");
    return claims <= 4 ? claim("check-" + claims) : null;
  };
  h.checker.execute = () => {
    executions++;
    if (executions === 1) throw new Error("synchronous dispatch failed");
    if (executions === 2) return Promise.reject(new Error("execution rejected"));
    return Promise.resolve();
  };
  const done = h.worker.run(h.controller.signal);
  try {
    await until(() => executions === 3 && h.state.protected === 0);
    assert.equal(h.worker.isIdle(), true);
    assert.equal(h.state.admissions, 0);
    assert.equal(h.gate.inFlightClaims(), 0);
    assert.equal(h.state.ended, 2, "settled executions notify maintenance, including rejected executions");
    for (const reason of ["transport failed", "synchronous dispatch failed", "execution rejected"]) {
      assert.ok(h.lines.some((line) => line.error === reason), reason);
    }
  } finally { h.controller.abort(); await done; }
});

for (const blockedBy of ["activity", "overflow", "disk", "maintenance", "quarantine"] as const) {
  it(`cross-check polling respects the ${blockedBy} gate while heartbeat remains live`, async (t) => {
    const h = build();
    let paused = true;
    if (blockedBy === "activity") t.mock.method(h.registry, "isClaimPausedForE2E", () => paused);
    if (blockedBy === "overflow") t.mock.method(h.registry, "claimsPausedByPendingOverflow", () => paused);
    if (blockedBy === "disk") h.state.diskBlocked = true;
    if (blockedBy === "maintenance") h.gate.close("maintenance");
    if (blockedBy === "quarantine") latchResidueQuarantine({ cause: "test residue", site: "test" });
    let polls = 0;
    h.client.claimCrossCheck = async () => { polls++; return null; };
    const done = h.worker.run(h.controller.signal);
    try {
      await until(() => h.heartbeats.length >= 4);
      assert.equal(polls, 0);
      if (blockedBy !== "quarantine") {
        paused = false;
        h.state.diskBlocked = false;
        h.gate.open("maintenance");
        await until(() => polls > 0);
      }
    } finally { h.controller.abort(); await done; }
    assert.equal(h.gate.inFlightClaims(), 0);
  });
}

it("quarantine latched during admission refuses a dedicated claim and releases admission", async () => {
  const h = build();
  let polls = 0;
  h.client.claimCrossCheck = async () => { polls++; return null; };
  h.admission.acquireAdmission = async () => {
    h.state.admissions++;
    latchResidueQuarantine({ cause: "test residue", site: "admission" });
    await tick();
    return () => { h.state.admissions--; };
  };
  const done = h.worker.run(h.controller.signal);
  try {
    await until(() => h.heartbeats.length >= 4);
    assert.equal(polls, 0);
    assert.equal(h.state.admissions, 0);
    assert.equal(h.gate.inFlightClaims(), 0);
  } finally { h.controller.abort(); await done; }
});
