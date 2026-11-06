// issue #2969 (owner decision 2026-11-06, fallback): the retained-predecessor pass is disabled in production, because no
// in-process reader can rule out a COMMAND_UID process writing into a predecessor source. Without the test-only
// `predecessorFinalForTests` option, a retained re-claim of a Claude flight and of a Codex flight, each with guarded
// lower-generation candidates, skips with `command_uid_unproven` BEFORE any predecessor operation: no Codex identity
// inventory read, no free capture, no boundary registration, no coordinator operation. The successor proceeds. The
// positive twins (flag on) enter the pass with the same fixture and record non-zero counts for the same counters, so
// the zero counts cannot be vacuous. Real git, journal and retained re-claim, as in
// runner-retained-predecessor-final.test.ts.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { ExecutorFactory } from "../src/runner.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import { mintAttemptId, quiesceRunAttempt } from "../src/run-quiescence.js";
import type { Executor } from "../src/executor.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { FakeCodexExecutor, codexRig } from "./runner-codex-sinks-fixture.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith, fx } from "./runner-harness.js";

installHarness();

const JOURNAL_KEY = "fixture-journal-key";
const REPO_ID = "00000000-0000-4000-8000-000000002969";

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

type Internals = { registerPredecessorBoundary: (...args: unknown[]) => unknown };

interface Opts {
  codex?: boolean;
  /** Overrides the runner options; `predecessorFinalForTests: true` re-enables the pass (the positive twin). */
  runnerOpts?: Record<string, unknown>;
}

async function drive(o: Opts = {}) {
  const claimGeneration = 3;
  const generations = [1, 2];
  const claim = gitlabClaim(2512, { claim_generation: claimGeneration, session_id: "predecessor-session" });
  claim.repo.id = REPO_ID;
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attemptId = mintAttemptId(claimGeneration - 1);
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id, attemptId);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", claimGeneration);
  claim.inventory_guarded = true;
  client.protocolFeatures = ["claim_generation_fence"];
  const hasFeature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" ? true : hasFeature(name);
  client.getRunOwnership = async () => ({ status: "running", claim_generation: claimGeneration, inventory_guarded: true });
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [...generations, claimGeneration].map(generation => ({
    hold_id: `fixture-hold-${generation}`, generation, inventory_guarded: true, has_available_capture: false,
  })) });
  const events: string[] = [];
  const rpcs: string[] = [];
  const rpc = (name: string): void => { rpcs.push(name); events.push(`rpc:${name}`); };
  client.reserveRecoveryCapture = async () => { rpc("reserve"); return { capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" }; };
  client.uploadRecoveryBundle = async (_run, _capture, _manifest, stream) => {
    rpc("upload");
    for await (const _chunk of stream) { /* consume */ }
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { rpc("status"); throw new Error("status unavailable"); };
  client.releaseRecoveryCustody = async () => { rpc("release"); throw new Error("must keep custody"); };

  const prepare = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { events.push("prepare"); return prepare(...args); };

  const rig = o.codex ? codexRig() : undefined;
  let makeCalls = 0;
  let models = 0;
  const make: ExecutorFactory = runId => {
    const run: Executor["run"] = async () => {
      models++;
      events.push("model");
      throw new Error("model interrupted before settled completion");
    };
    return { homeDir: path.join(homeDir, runId), executor: rig && makeCalls++ === 0
      ? new FakeCodexExecutor(rig.safety, run, rig.settle, rig.capture)
      : { run } };
  };
  const logs = recordingLogger();
  const runner = runnerWith(make, fakeGitlab().gitlab, JOURNAL_KEY, logs.logger, {
    recoveryRetryMs: 1, quiesceRun: quiesceRunAttempt, codexBoundaryDeadlineMs: 1_000, ...o.runnerOpts,
  } as never);
  const recovery = Reflect.get(runner, "recovery") as RecoveryCoordinator;
  const internals = runner as unknown as Internals;
  const calls = { registrations: 0, operations: 0, identityReads: 0, freeCaptures: 0 };
  // Entries made while settlePredecessorHolds is on the stack (it is awaited serially by the retained re-claim), so
  // these pin that the production gate precedes the Codex inventory read and the predecessor free capture.
  let inSettle = false;
  const settle = (runner as unknown as { settlePredecessorHolds: (...args: unknown[]) => Promise<unknown> }).settlePredecessorHolds.bind(runner);
  (runner as unknown as Record<string, unknown>).settlePredecessorHolds = async (...args: unknown[]) => {
    inSettle = true;
    try { return await settle(...args); } finally { inSettle = false; }
  };
  const freeCapture = (runner as unknown as { runFreeCapture: (...args: unknown[]) => unknown }).runFreeCapture.bind(runner);
  (runner as unknown as Record<string, unknown>).runFreeCapture = (...args: unknown[]) => {
    if (inSettle) calls.freeCaptures++;
    return freeCapture(...args);
  };
  const readHeads = git.readInventoryCloneHeads.bind(git);
  git.readInventoryCloneHeads = ((...args: Parameters<typeof readHeads>) => {
    const opt = args[2] as unknown;
    if (inSettle && typeof opt === "object" && opt !== null && (opt as { cloneIdentity?: boolean }).cloneIdentity === true) calls.identityReads++;
    return readHeads(...args);
  }) as typeof git.readInventoryCloneHeads;
  const register = internals.registerPredecessorBoundary.bind(runner);
  internals.registerPredecessorBoundary = (...args) => { calls.registrations++; return register(...args); };
  for (const generation of generations) {
    const pinned = await recovery.pin({ runId: claim.run_id, generation, kind: "issue", branch: clone.branch,
      sourceSha: command(clone.path, "rev-parse", "HEAD"), inventoryGuarded: true });
    assert.ok(pinned, `fixture: guarded generation ${generation} pinned`);
  }
  const original = recovery.operateFrozenInventory.bind(recovery);
  recovery.operateFrozenInventory = async (input, options = {}) => {
    if (options.predecessor) calls.operations++;
    return original(input, options);
  };
  assert.equal((await recovery.predecessorRecords(claim.run_id, claimGeneration)).length, generations.length,
    "fixture: both lower generations are candidates");
  await runner.execute(claim);
  return { claim, bare, clone, events, rpcs, logs, calls, models: () => models };
}
type Result = Awaited<ReturnType<typeof drive>>;

const logged = (r: Result, msg: string): Array<Record<string, any>> => (r.logs.lines as Array<Record<string, any>>).filter(l => l.msg === msg);
const skipReasons = (r: Result): string[] => logged(r, "retained predecessor FINAL skipped").map(l => l.reason as string);

function assertZeroPredecessorEffects(r: Result): void {
  assert.deepEqual(skipReasons(r), ["command_uid_unproven"]);
  assert.deepEqual(r.calls, { registrations: 0, operations: 0, identityReads: 0, freeCaptures: 0 },
    "the gate precedes the Codex identity read, the predecessor free capture, the boundary registration and every coordinator operation");
  assert.equal(logged(r, "retained predecessor FINAL pass").length, 0, "the pass never ran");
  assert.ok(r.events.includes("prepare"), "the successor was prepared");
  assert.equal(r.models(), 1, "the successor reached its model");
  assert.notEqual(journal(r.bare, r.clone.branch).recovery?.blocker, "quiescence_failed");
}

describe("without the test-only flag the retained-predecessor pass is disabled (issue #2969 fallback)", () => {
  it("Claude flight with guarded lower-generation candidates: skip command_uid_unproven, zero predecessor effects", async () => {
    assertZeroPredecessorEffects(await drive());
  });

  it("Codex flight with guarded lower-generation candidates: skip command_uid_unproven, zero predecessor effects", async () => {
    assertZeroPredecessorEffects(await drive({ codex: true }));
  });

  it("positive twin: the same Claude fixture with predecessorFinalForTests enters the pass (the controls are not vacuous)", async () => {
    const r = await drive({ runnerOpts: { predecessorFinalForTests: true } });
    assert.deepEqual(skipReasons(r), []);
    assert.ok(r.calls.freeCaptures >= 1 && r.calls.registrations >= 1 && r.calls.operations >= 1, JSON.stringify(r.calls));
    assert.equal(r.calls.identityReads, 0, "a Claude flight takes no Codex identity read");
    assert.ok(logged(r, "retained predecessor FINAL pass").length >= 1, "the pass ran");
  });

  it("positive twin: the same Codex fixture with predecessorFinalForTests reads the clone identity and enters the pass", async () => {
    const r = await drive({ codex: true, runnerOpts: { predecessorFinalForTests: true } });
    assert.deepEqual(skipReasons(r), []);
    assert.ok(r.calls.identityReads >= 1, JSON.stringify(r.calls));
    assert.ok(r.calls.freeCaptures >= 1 && r.calls.registrations >= 1 && r.calls.operations >= 1, JSON.stringify(r.calls));
    assert.ok(logged(r, "retained predecessor FINAL pass").length >= 1, "the pass ran");
  });
});
