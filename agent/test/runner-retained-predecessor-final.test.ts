// issue #2969 D3/D7: a retained re-claim settles the lower generations' guarded holds from the still-intact predecessor
// source (the predecessor FINAL) before it prepares its successor. Real git, the real RecoveryCoordinator journal and the
// real runFreeCapture -> registerPredecessorBoundary -> proveInventorySources chain; only the coordinator's per-record
// operation (operateFrozenInventory with a predecessor scope) is replaced by a stub that enters the runner's boundary
// exactly as the coordinator does, so each test chooses the outcome the coordinator would report.
import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";
import type { ExecutorFactory } from "../src/runner.js";
import {
  PredecessorAuthorityLost, type CaptureInput, type FrozenOperationOptions, type InventorySourceDiagnostic,
  type RecoveryOutcome, type RecoveryRecord, type RecoveryCoordinator,
} from "../src/recovery.js";
import { quiesceRunAttempt, mintAttemptId, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { DiskGovernor, type CacheCapConfig } from "../src/cache-cap.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import type { Executor } from "../src/executor.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { FakeCodexExecutor, codexRig } from "./runner-codex-sinks-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const JOURNAL_KEY = "fixture-journal-key";
const REPO_ID = "00000000-0000-4000-8000-000000002969";
const CODEX_BOUNDARY_MS = 1_000;

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
      GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
function journal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}

const callbackFailures: unknown[] = [];
afterEach(() => {
  const failures = callbackFailures.splice(0);
  assert.deepEqual(failures, [], "an executor callback assertion was swallowed by the runner");
});

type Internals = {
  withInventorySourceBoundary(context: {
    runId: string; generation: number; barePath: string; predecessor?: object;
    reportDiagnostic?: (d: InventorySourceDiagnostic) => void;
  }, action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained">;
  registerPredecessorBoundary: (...args: unknown[]) => unknown;
  settlePredecessorHolds: (...args: unknown[]) => Promise<"proceed" | "stop">;
};

interface Proof { generation: number; result: "passed" | "retained"; proves: boolean[]; diagnostics: InventorySourceDiagnostic[] }
interface OperateCall {
  record: RecoveryRecord; input: CaptureInput; options: FrozenOperationOptions;
  /** Enter the runner's predecessor boundary the way the coordinator does; resolves to what `prove()` answered. */
  prove(): Promise<Proof>;
}
interface World {
  claim: ReturnType<typeof gitlabClaim>; bare: string; clone: { path: string; branch: string };
  events: string[]; recovery: RecoveryCoordinator; internals: Internals;
}

interface Opts {
  generations?: number[];
  claimGeneration?: number;
  codex?: boolean;
  /** Replaces the default per-record operation (a boundary proof that acks the hold). */
  operate?: (call: OperateCall, world: World) => Promise<RecoveryOutcome>;
  budget?: { successorPrepAllowanceMs?: number; actionCapMs?: number; settlementMarginMs?: number };
  /** Wraps the real readInventoryCloneHeads. */
  inventory?: (real: typeof git.readInventoryCloneHeads) => typeof git.readInventoryCloneHeads;
  /** Called right before execute, with the live event log (for tests that observe a run in progress). */
  observe?: (world: World) => void;
  /** Runs inside the successor's model call. */
  duringModel?: (world: World) => Promise<void>;
  /** False withdraws the negotiated recovery_inventory_v1 feature. */
  inventoryFeature?: boolean;
  inventoryGuarded?: boolean;
  protocolFeatures?: string[];
  runnerOpts?: Record<string, unknown>;
}

async function drive(o: Opts = {}) {
  const claimGeneration = o.claimGeneration ?? 3;
  const claim = gitlabClaim(2512, { claim_generation: claimGeneration, session_id: "predecessor-session" });
  claim.repo.id = REPO_ID;
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attemptId = mintAttemptId(Math.max(1, claimGeneration - 1));
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id, attemptId);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", claimGeneration);
  claim.inventory_guarded = o.inventoryGuarded ?? true;
  client.protocolFeatures = o.protocolFeatures ?? ["claim_generation_fence"];
  const hasFeature = client.hasFeature.bind(client);
  // Journaling a guarded record needs the feature, so a withdrawal applies only once the fixture is pinned.
  let inventoryFeature = true;
  client.hasFeature = name => name === "recovery_inventory_v1" ? inventoryFeature : hasFeature(name);
  const generations = o.generations ?? [1, 2];
  client.getRunOwnership = async () => ({ status: "running", claim_generation: claimGeneration, inventory_guarded: true });
  // One open guarded hold per generation, as the api keeps them until each is settled.
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [...generations, claimGeneration].map(generation => ({
    hold_id: `fixture-hold-${generation}`, generation, inventory_guarded: true, has_available_capture: false,
  })) });
  client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _capture, _manifest, stream) => {
    for await (const _chunk of stream) { /* consume */ }
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { throw new Error("status unavailable"); };
  client.releaseRecoveryCustody = async () => { throw new Error("must keep custody"); };

  const events: string[] = [];
  const prepare = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { events.push("prepare"); return prepare(...args); };
  const quiesceRequests: QuiesceRunRequest[] = [];
  const quiesceRun = async (req: QuiesceRunRequest) => { quiesceRequests.push(req); return quiesceRunAttempt(req); };
  if (o.inventory) git.readInventoryCloneHeads = o.inventory(git.readInventoryCloneHeads.bind(git));
  let models = 0;
  const rig = o.codex ? codexRig() : undefined;
  let makeCalls = 0;
  let world!: World;
  const make: ExecutorFactory = runId => {
    const run: Executor["run"] = async () => {
      models++;
      events.push("model");
      try { await o.duringModel?.(world); } catch (error) { callbackFailures.push(error); }
      throw new Error("model interrupted before settled completion");
    };
    // The first executor is the predecessor's; a Codex predecessor is replaced by an ordinary one for the successor.
    return { homeDir: path.join(homeDir, runId), executor: rig && makeCalls++ === 0
      ? new FakeCodexExecutor(rig.safety, run, rig.settle, rig.capture)
      : { run } };
  };
  const logs = recordingLogger();
  const runner = runnerWith(make, fakeGitlab().gitlab, JOURNAL_KEY, logs.logger, {
    recoveryRetryMs: 1, quiesceRun, codexBoundaryDeadlineMs: CODEX_BOUNDARY_MS,
    ...(o.budget ? { predecessorFinalBudgetMs: o.budget } : {}),
    ...o.runnerOpts,
  } as never);
  const recovery = Reflect.get(runner, "recovery") as RecoveryCoordinator;
  const internals = runner as unknown as Internals;
  const counts = { registrations: 0, settles: 0 };
  const register = internals.registerPredecessorBoundary.bind(runner);
  internals.registerPredecessorBoundary = (...args) => { counts.registrations++; return register(...args); };
  const settle = internals.settlePredecessorHolds.bind(runner);
  internals.settlePredecessorHolds = (...args) => { counts.settles++; return settle(...args); };
  for (const generation of generations) {
    const pinned = await recovery.pin({ runId: claim.run_id, generation, kind: "issue", branch: clone.branch,
      sourceSha: command(clone.path, "rev-parse", "HEAD"), inventoryGuarded: true });
    assert.ok(pinned, `fixture: guarded generation ${generation} pinned`);
  }
  inventoryFeature = o.inventoryFeature !== false;
  const operated: Array<{ generation: number; options: FrozenOperationOptions; input: CaptureInput }> = [];
  const proofs: Proof[] = [];
  const original = recovery.operateFrozenInventory.bind(recovery);
  recovery.operateFrozenInventory = async (input, options = {}) => {
    if (!options.predecessor) return original(input, options);
    const record = input.record;
    events.push(`operate:${record.generation}`);
    operated.push({ generation: record.generation!, options, input });
    const call: OperateCall = {
      record, input, options,
      prove: async () => {
        const diagnostics: InventorySourceDiagnostic[] = [];
        const proves: boolean[] = [];
        const result = await internals.withInventorySourceBoundary({
          runId: record.runId, generation: record.generation!, barePath: bare, predecessor: options.predecessor!.boundary,
          reportDiagnostic: d => diagnostics.push(d),
        }, async prove => { proves.push(await prove()); });
        const proof = { generation: record.generation!, result, proves, diagnostics };
        proofs.push(proof);
        return proof;
      },
    };
    if (o.operate) return o.operate(call, world);
    const proof = await call.prove();
    return { state: record.state, captureId: record.captureId, held: proof.result === "passed" ? "acked" : "kept" };
  };
  world = { claim, bare, clone, events, recovery, internals };
  o.observe?.(world);
  await runner.execute(claim);
  return { ...world, runner, logs, operated, proofs, quiesceRequests, counts, models: () => models, rig };
}
type Result = Awaited<ReturnType<typeof drive>>;

function failedReports(r: Result) {
  return api.states.filter(s => s.runId === r.claim.run_id && s.body.status === "failed");
}
function logged(r: Result, msg: string): Array<Record<string, any>> {
  return (r.logs.lines as Array<Record<string, any>>).filter(l => l.msg === msg);
}
/** The retained recovery stopped before any successor: custody and the predecessor stay exactly as they were. */
function assertStopped(r: Result): void {
  assert.equal(r.events.includes("prepare"), false, "no successor was prepared");
  assert.equal(r.models(), 0, "no model call");
  assert.ok(failedReports(r).length > 0, "the claim ended with the custody-retaining failed report");
  const retained = journal(r.bare, r.clone.branch);
  assert.equal(retained.clonePath, r.clone.path, "no successor replaces the predecessor");
  assert.equal(retained.recovery.stage, "blocked");
  assert.equal(retained.recovery.blocker, "quiescence_failed");
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.equal(logged(r, "retained predecessor FINAL pass").at(-1)?.verdict, "stop");
}
/** The pass finished (or skipped) cleanly and the successor was prepared and ran. */
function assertProceeded(r: Result): void {
  assert.ok(r.events.includes("prepare"), "the successor was prepared");
  assert.equal(r.models(), 1, "the successor reached its model");
  assert.notEqual(journal(r.bare, r.clone.branch).recovery?.blocker, "quiescence_failed");
}
function skipReasons(r: Result): string[] {
  return logged(r, "retained predecessor FINAL skipped").map(l => l.reason as string);
}
/** The pass's own clock must not leak into the flight's other continuations (steering polls, message flushes). */
function assertNoLeakedBudget(r: Result): void {
  const leaked = (r.logs.lines as Array<Record<string, any>>).filter(l => /maintenance deadline exhausted/.test(String(l.error ?? "")));
  assert.deepEqual(leaked.map(l => l.msg), [], "an expired pass budget reached an unrelated continuation");
}
const operations = (r: { events: string[] }) => r.events.filter(e => e.startsWith("operate:"));
const ordered = (r: { events: string[] }) => r.events.filter(e => e.startsWith("operate:") || e === "prepare");
const settledBy = (call: OperateCall, proof: Proof): RecoveryOutcome =>
  ({ state: call.record.state, captureId: call.record.captureId, held: proof.result === "passed" ? "acked" : "kept" });
const driftCommit = (clonePath: string): void => {
  command(clonePath, "-c", "user.name=drift", "-c", "user.email=drift@example.com", "commit", "--allow-empty", "-m", "drift");
};

// ── ordering ────────────────────────────────────────────────────────────────────────────────────────────────────────

it("both guarded generations are settled oldest first, before the successor is prepared (Claude)", async () => {
  const r = await drive();
  assert.deepEqual(ordered(r), ["operate:1", "operate:2", "prepare"]);
  assertProceeded(r);
  assert.deepEqual(r.proofs.map(p => [p.generation, p.result, p.proves, p.diagnostics]),
    [[1, "passed", [true], []], [2, "passed", [true], []]], "the real runner boundary proved the predecessor source for each");
  assert.equal(r.counts.registrations, 1, "one held scope for the whole pass");
  const pass = logged(r, "retained predecessor FINAL pass").at(-1)!;
  assert.deepEqual([pass.verdict, pass.candidates, pass.operated, pass.outcomes], ["proceed", 2, 2, ["1:acked", "2:acked"]]);
  for (const key of ["uploaded_bytes", "charged_bytes", "upload_count", "retained_unuploaded_bytes", "reused_bytes",
    "added_ms", "reserve_ms", "scope_ms", "cap_ms"]) assert.equal(typeof pass[key], "number", key);
  assert.equal(pass.reserve_ms, 2 * CODEX_BOUNDARY_MS + 60_000, "R = 2 * codexBoundaryDeadlineMs + the prep allowance");
});

it("every candidate runs under one predecessor operation: this claim's generation, the repo, one shared allowance and sink", async () => {
  const r = await drive();
  assert.equal(r.operated.length, 2);
  const [first, second] = r.operated.map(x => x.options.predecessor!);
  assert.equal(first!.claimGeneration, 3);
  assert.equal(first!.repoId, REPO_ID);
  assert.equal(typeof first!.boundary, "object");
  assert.equal(first!.boundary, second!.boundary, "one registration serves the whole pass");
  assert.equal(first!.uploadAllowance, second!.uploadAllowance, "the upload allowance is shared by identity");
  assert.equal(first!.measurements, second!.measurements, "the measurement sink is shared by identity");
  assert.ok(first!.uploadAllowance.remaining > 0 && first!.uploadAllowance.remaining <= 64 * 1024 * 1024);
  assert.equal(first!.live(), false, "the registration is disposed once the pass ends");
  for (const x of r.operated) {
    assert.equal(x.input.closureMint, "deny");
    assert.ok(x.input.signal, "each operation runs under the pass's cap signal");
  }
});

for (const variant of [{ inventoryGuarded: false }, { inventoryFeature: false }] as const) {
  it(`the predecessor pass is skipped, with no entry, when its preconditions do not hold (${Object.keys(variant)[0]})`, async () => {
    const r = await drive(variant);
    assert.equal(r.counts.registrations, 0);
    assert.deepEqual(operations(r), []);
    // Without the negotiated feature the retained guarded claim itself stops earlier; it must not be a stop of this pass.
    if ("inventoryGuarded" in variant) assertProceeded(r);
    else assert.deepEqual(logged(r, "retained predecessor FINAL pass"), []);
  });
}

it("claim generation 1 has no predecessor to settle: no entry", async () => {
  const r = await drive({ claimGeneration: 1, generations: [] });
  assert.equal(r.counts.registrations, 0);
  assert.deepEqual(operations(r), []);
  assertProceeded(r);
});

it("no guarded candidates: no entry, and the successor proceeds", async () => {
  const r = await drive({ generations: [] });
  assert.equal(r.counts.settles, 1, "the pass was consulted");
  assert.equal(r.counts.registrations, 0);
  assert.deepEqual(operations(r), []);
  assertProceeded(r);
});

it("a spent time budget skips before entry", async () => {
  // R = 2 * 1s + 400s exceeds the 300s retained window: the scope deadline is already in the past.
  const r = await drive({ budget: { successorPrepAllowanceMs: 400_000 } });
  assert.equal(r.counts.registrations, 0);
  assert.deepEqual(operations(r), []);
  assert.deepEqual(skipReasons(r), ["budget"]);
  assertProceeded(r);
});

it("a capacity refusal earlier in the iteration never reaches the predecessor pass", async () => {
  const BSIZE = 4096, BLOCKS = 10_000_000, FILES = 10_000_000;
  const used = 0.89;
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => {
      const free = Math.round(BLOCKS * (1 - used));
      return { bsize: BSIZE, blocks: BLOCKS, bfree: free, bavail: free, files: FILES, ffree: Math.round(FILES * 0.8) };
    },
    stat: async () => ({ dev: 7 }),
    reclaim: async () => {},
  });
  const governorConfig: CacheCapConfig = {
    capEnabled: true, capFraction: 0.5, lowWater: 0.6, maxConcurrentRuns: 2, hardStopEnabled: true, hardMargin: 0.03,
  };
  const diskGovernor = new DiskGovernor({
    config: governorConfig, log: nullLogger(), volumeTotalBytes: () => BLOCKS * BSIZE, thresholdOf: () => 0.9,
  });
  const r = await drive({
    protocolFeatures: ["claim_generation_fence", "recovery_cause_data_volume_full", "recovery_cause_retained_recovery_exhausted"],
    runnerOpts: { dataVolume: guard, diskGovernor },
  });
  assert.equal(r.counts.settles, 0, "settlePredecessorHolds is never called");
  assert.equal(r.counts.registrations, 0);
  assert.deepEqual(operations(r), []);
  assert.equal(r.events.includes("prepare"), false, "the seed was refused for capacity");
  assert.equal(r.models(), 0);
});

// ── Codex ───────────────────────────────────────────────────────────────────────────────────────────────────────────

it("Codex single source: the production chain proves the capture attempt's own clone path and passes", async () => {
  const r = await drive({ codex: true });
  assert.deepEqual(ordered(r), ["operate:1", "operate:2", "prepare"]);
  assertProceeded(r);
  assert.equal(r.counts.registrations, 1);
  assert.deepEqual(r.proofs.map(p => [p.generation, p.result, p.proves, p.diagnostics]),
    [[1, "passed", [true], []], [2, "passed", [true], []]], "no live_attempt_path or codex_attempt_mismatch refusal");
  const sourceRequests = r.quiesceRequests.filter(q => q.site === "inventory_source");
  assert.ok(sourceRequests.length >= 2, "the boundary quiesced the source for each proof");
  const resolved = path.resolve(r.clone.path);
  for (const req of sourceRequests) {
    assert.ok(req.attempt, "a Codex proof carries the bound capture attempt");
    assert.equal(req.attempt.clonePath, resolved, "the bound attempt's clone path is the resolved predecessor clone");
    assert.deepEqual(req.targetPaths, [resolved]);
    assert.equal(req.registry.isLivePath(resolved), false, "the capture attempt is not registered as live");
  }
});

type ReadHeads = typeof git.readInventoryCloneHeads;
const identityRead = (options: unknown): boolean => (options as { cloneIdentity?: boolean } | undefined)?.cloneIdentity === true;
for (const [name, skip, inventory] of [
  ["a sibling source", "codex_sibling_source", (real: ReadHeads): ReadHeads =>
    (async (bare: string, runId: string, options?: never) => {
      const inventory = await real(bare, runId, options);
      if (inventory.kind !== "verified" || !identityRead(options)) return inventory;
      return { ...inventory, clones: [...inventory.clones,
        { clonePath: "/fixture/sibling", branch: "task", runId, head: "b".repeat(40), identity: "1:2" }] };
    }) as ReadHeads],
  ["an unverified inventory", "codex_inventory_unknown", (real: ReadHeads): ReadHeads =>
    (async (bare: string, runId: string, options?: never) =>
      identityRead(options) ? { kind: "unknown", cause: "other" } : real(bare, runId, options)) as ReadHeads],
  ["a clone without a head identity", "codex_inventory_incomplete", (real: ReadHeads): ReadHeads =>
    (async (bare: string, runId: string, options?: never) => {
      const inventory = await real(bare, runId, options);
      if (inventory.kind !== "verified" || !identityRead(options)) return inventory;
      return { ...inventory, clones: inventory.clones.map(({ identity: _identity, ...clone }) => clone) };
    }) as ReadHeads],
] as const) {
  it(`Codex skips before entry on ${name}, and the successor proceeds`, async () => {
    const r = await drive({ codex: true, inventory });
    assert.deepEqual(skipReasons(r), [skip]);
    assert.equal(r.counts.registrations, 0, "no held scope was entered for the predecessor");
    assert.deepEqual(operations(r), [], "no coordinator operation");
    assertProceeded(r);
  });
}

it("Codex: a change to the source after entry is baseline drift and stops with custody retained", async () => {
  const r = await drive({
    codex: true,
    operate: async (call, world) => {
      // The pre-entry read is the baseline; a new commit in the clone makes the first proof's read differ.
      driftCommit(world.clone.path);
      return settledBy(call, await call.prove());
    },
  });
  assert.deepEqual(r.proofs.map(p => [p.generation, p.result]), [[1, "retained"]]);
  assert.deepEqual(operations(r), ["operate:1"], "no later candidate is started after a stop");
  assertStopped(r);
  assert.match(String(logged(r, "retained predecessor FINAL pass").at(-1)?.stop_reason), /baseline_drift/);
});

// ── stop and skip classification ─────────────────────────────────────────────────────────────────────────────────────

it("Claude: the source changing between two candidates is baseline drift and stops", async () => {
  const r = await drive({
    operate: async (call, world) => {
      const proof = await call.prove();
      if (call.record.generation === 1) driftCommit(world.clone.path);
      return settledBy(call, proof);
    },
  });
  assert.deepEqual(r.proofs.map(p => [p.generation, p.result]), [[1, "passed"], [2, "retained"]]);
  assertStopped(r);
  assert.match(String(logged(r, "retained predecessor FINAL pass").at(-1)?.stop_reason), /baseline_drift/);
});

it("residue (a dirty worktree) is a clean skip: later candidates are not started and the successor proceeds", async () => {
  const r = await drive({
    operate: async (call, world) => {
      fs.writeFileSync(path.join(world.clone.path, "uncaptured.txt"), "bytes the archive does not hold\n");
      return settledBy(call, await call.prove());
    },
  });
  assert.deepEqual(r.proofs.map(p => [p.generation, p.result]), [[1, "retained"]]);
  assert.deepEqual(operations(r), ["operate:1"], "the dependent candidate is skipped");
  assertProceeded(r);
  const pass = logged(r, "retained predecessor FINAL pass").at(-1)!;
  assert.deepEqual([pass.verdict, pass.residue], ["proceed", true]);
});

it("residue (ignored files the successor would lose) is a clean skip", async () => {
  const r = await drive({
    operate: async (call, world) => {
      // An ignored build directory: invisible to status, but the successor seed would not carry it.
      fs.appendFileSync(path.join(world.clone.path, ".git", "info", "exclude"), "build-output/\n");
      fs.mkdirSync(path.join(world.clone.path, "build-output"));
      fs.writeFileSync(path.join(world.clone.path, "build-output", "artifact.bin"), "ignored residue\n");
      return settledBy(call, await call.prove());
    },
  });
  assert.deepEqual(r.proofs.map(p => [p.generation, p.result]), [[1, "retained"]]);
  assert.deepEqual(operations(r), ["operate:1"]);
  assertProceeded(r);
  assert.equal(logged(r, "retained predecessor FINAL pass").at(-1)?.residue, true);
});

it("an integrity failure latched by the coordinator stops with custody retained", async () => {
  const r = await drive({
    operate: async call => ({ state: call.record.state, captureId: call.record.captureId, held: "kept",
      predecessor: { integrityFailure: "bundle_header" } }),
  });
  assert.deepEqual(operations(r), ["operate:1"], "no later candidate after a stop");
  assertStopped(r);
  assert.match(String(logged(r, "retained predecessor FINAL pass").at(-1)?.stop_reason), /integrity:bundle_header/);
});

for (const reason of ["predecessor_observation_refused", "restrictions", "predecessor_upload_budget", "oversized"]) {
  it(`an ordinary refusal (${reason}) is a clean skip: every candidate is still attempted and the successor proceeds`, async () => {
    const r = await drive({
      operate: async call => ({ state: call.record.state, captureId: call.record.captureId, reason, held: "kept" }),
    });
    assert.deepEqual(operations(r), ["operate:1", "operate:2"]);
    assertProceeded(r);
    assert.deepEqual(logged(r, "retained predecessor FINAL pass").at(-1)?.outcomes, [`1:kept/${reason}`, `2:kept/${reason}`]);
  });
}

it("a lost predecessor authority stops with custody retained", async () => {
  const r = await drive({ operate: async () => { throw new PredecessorAuthorityLost(); } });
  assert.deepEqual(operations(r), ["operate:1"]);
  assertStopped(r);
  assert.match(String(logged(r, "retained predecessor FINAL pass").at(-1)?.stop_reason), /authority_lost/);
});

it("an unexpected failure of the coordinator operation fails closed", async () => {
  const r = await drive({ operate: async () => { throw new Error("unclassified coordinator failure"); } });
  assertStopped(r);
  assert.match(String(logged(r, "retained predecessor FINAL pass").at(-1)?.stop_reason), /operation_error/);
});

// ── time budget ──────────────────────────────────────────────────────────────────────────────────────────────────────

/** The held scope's deadline for the running pass: the iteration deadline in the journal minus R. */
function scopeDeadlineOf(world: World, prepMs: number): number {
  return journal(world.bare, world.clone.branch).recovery.deadline - (2 * CODEX_BOUNDARY_MS + prepMs);
}
// The scope deadline sits 15s after the iteration window opened; the pass starts a few seconds into the window.
const LATE_PREP_MS = 300_000 - 15_000 - 2 * CODEX_BOUNDARY_MS;

it("an operation that settles after the scope deadline is a late settlement and stops", async () => {
  const r = await drive({
    budget: { successorPrepAllowanceMs: LATE_PREP_MS, settlementMarginMs: 500 },
    operate: async (call, world) => {
      await sleep(Math.max(0, scopeDeadlineOf(world, LATE_PREP_MS) - Date.now() + 150));
      return { state: call.record.state, captureId: call.record.captureId, held: "acked" };
    },
  });
  assert.deepEqual(operations(r), ["operate:1"]);
  assertStopped(r);
  assert.match(String(logged(r, "retained predecessor FINAL pass").at(-1)?.stop_reason), /settlement_late/);
  assertNoLeakedBudget(r);
});

it("an operation that never settles keeps the flight inside the held scope, and no successor is created meanwhile", async () => {
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  let events!: string[];
  const running = drive({
    observe: world => { events = world.events; },
    operate: async call => {
      await gate;
      return { state: call.record.state, captureId: call.record.captureId, held: "kept" };
    },
  });
  try {
    const startedAt = Date.now();
    while (!events?.includes("operate:1")) {
      if (Date.now() - startedAt > 60_000) assert.fail("the predecessor pass never started");
      await sleep(25);
    }
    await sleep(1_500);
    assert.equal(events.includes("prepare"), false, "the successor is not prepared while the operation is unsettled");
    assert.equal(events.includes("operate:2"), false, "the next candidate waits for the first");
    assert.equal(events.includes("model"), false);
  } finally { release(); }
  const r = await running;
  assert.deepEqual(operations(r), ["operate:1", "operate:2"], "the pass resumed once the operation settled");
  assertProceeded(r);
});

it("the action cap aborts the in-flight candidate, starts no further one, and is a clean skip", async () => {
  const r = await drive({
    budget: { actionCapMs: 400 },
    operate: async call => {
      const signal = call.input.signal!;
      await new Promise<void>((_resolve, reject) => {
        if (signal.aborted) reject(signal.reason);
        signal.addEventListener("abort", () => reject(signal.reason), { once: true });
      });
      return { state: call.record.state, captureId: call.record.captureId, held: "kept" };
    },
  });
  assert.deepEqual(operations(r), ["operate:1"]);
  assertProceeded(r);
  assert.equal(logged(r, "retained predecessor FINAL pass").at(-1)?.capped, true);
  assertNoLeakedBudget(r);
});

// ── dead authority ───────────────────────────────────────────────────────────────────────────────────────────────────

it("after the pass the boundary token is dead: a later proof, even with the successor attempt live, is refused", async () => {
  let afterAdoption: { result: string; diagnostics: InventorySourceDiagnostic[]; proofRan: boolean } | undefined;
  let token: object | undefined;
  const r = await drive({
    operate: async call => {
      token ??= call.options.predecessor!.boundary;
      return settledBy(call, await call.prove());
    },
    duringModel: async world => {
      // The successor's attempt is registered and live here; the predecessor token must grant nothing.
      const diagnostics: InventorySourceDiagnostic[] = [];
      let proofRan = false;
      const result = await world.internals.withInventorySourceBoundary({
        runId: world.claim.run_id, generation: 1, barePath: world.bare, predecessor: token!,
        reportDiagnostic: d => diagnostics.push(d),
      }, async () => { proofRan = true; });
      afterAdoption = { result, diagnostics, proofRan };
    },
  });
  assertProceeded(r);
  assert.deepEqual(afterAdoption, { result: "retained", diagnostics: [{ check: "execution_tail_present" }], proofRan: false });
});
