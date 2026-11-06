// issue #2969 (owner decision 2026-11-06, fallback): the retained-predecessor pass is disabled in production, because no
// in-process reader can rule out a COMMAND_UID process writing into a predecessor source. Without the test-only
// `predecessorFinalForTests` option, a retained re-claim of a Claude flight and of a Codex flight, each with guarded
// lower-generation candidates, skips with `command_uid_unproven` BEFORE any predecessor operation: no free capture, no
// boundary registration, no coordinator operation, no predecessor RPC, and byte-identical journal records for the
// predecessor generations. The successor proceeds. Real git, journal and retained re-claim, as in
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

type Internals = {
  registerPredecessorBoundary: (...args: unknown[]) => unknown;
  proveInventorySources: (...args: unknown[]) => Promise<boolean>;
};

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
  const lowerGenerationRpcs: string[] = [];
  const lower = (name: string, generation: number | undefined): void => {
    if (typeof generation === "number" && generation < claimGeneration) lowerGenerationRpcs.push(`${name}:${generation}`);
  };
  client.reserveRecoveryCapture = async (_run, req) => { rpc("reserve"); lower("reserve", req.generation); return { capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" }; };
  client.uploadRecoveryBundle = async (_run, _capture, _manifest, stream) => {
    rpc("upload");
    for await (const _chunk of stream) { /* consume */ }
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { rpc("status"); throw new Error("status unavailable"); };
  client.releaseRecoveryCustody = async (_run, generation) => { rpc("release"); lower("release", generation); throw new Error("must keep custody"); };

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
  const calls = { registrations: 0, proofs: 0, operations: 0 };
  const register = internals.registerPredecessorBoundary.bind(runner);
  internals.registerPredecessorBoundary = (...args) => { calls.registrations++; return register(...args); };
  const prove = internals.proveInventorySources.bind(runner);
  internals.proveInventorySources = (...args) => { calls.proofs++; return prove(...args); };
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
  // The predecessor generations' journal records, serialised before the claim runs.
  const snapshot = async (): Promise<string> => JSON.stringify(await recovery.predecessorRecords(claim.run_id, claimGeneration));
  const before = await snapshot();
  assert.equal(JSON.parse(before).length, generations.length, "fixture: both lower generations are candidates");
  await runner.execute(claim);
  return { claim, bare, clone, events, rpcs, lowerGenerationRpcs, logs, calls, models: () => models, before, after: await snapshot() };
}
type Result = Awaited<ReturnType<typeof drive>>;

const logged = (r: Result, msg: string): Array<Record<string, any>> => (r.logs.lines as Array<Record<string, any>>).filter(l => l.msg === msg);
const skipReasons = (r: Result): string[] => logged(r, "retained predecessor FINAL skipped").map(l => l.reason as string);

function assertZeroPredecessorEffects(r: Result): void {
  assert.deepEqual(skipReasons(r), ["command_uid_unproven"]);
  assert.deepEqual(r.calls, { registrations: 0, proofs: 0, operations: 0 },
    "no predecessor free-capture entry (the boundary is registered inside it), source proof or coordinator operation");
  // The retained claim's own generation may still reserve/upload; a predecessor generation may not.
  assert.deepEqual(r.lowerGenerationRpcs, [], "no reserve or release for a predecessor generation (the claim's own capture carries no generation)");
  assert.equal(r.after, r.before, "the predecessor generations' journal records are byte-identical");
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
    assert.ok(r.calls.registrations >= 1 && r.calls.operations >= 1, JSON.stringify(r.calls));
  });
});
