import { it, afterEach, type TestContext } from "node:test";
import { RecoveryClosureLimitError } from "../src/recovery-closure.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { randomBytes, createHmac } from "node:crypto";
import { PendingRecoveryCaptureError } from "../src/git.js";
import { CredentialSwitchSignal, PauseNowSignal } from "../src/steering.js";
import { MessageBatcher } from "../src/batcher.js";
import { canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import { deflateSync } from "node:zlib";
import { Readable } from "node:stream";
import type { ExecutorFactory } from "../src/runner.js";
import { Outbox } from "../src/outbox.js";
import { noProofReseed, nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

/** Preserve the persisted clock and drive only the already armed deadline timer. */
function episodeDeadline(t: TestContext, deadline: number, setupDelay = 0): () => void {
  const realNow = Date.now;
  const realSetTimeout = globalThis.setTimeout;
  let now: number | undefined = setupDelay ? realNow() + setupDelay : undefined;
  t.mock.method(Date, "now", () => now ?? realNow());
  const callbacks: (() => void)[] = [];
  t.mock.method(globalThis, "setTimeout", (callback: (...args: any[]) => void, delay?: number, ...args: any[]) => {
    if (delay !== undefined && delay > 200_000 && delay <= 300_000) callbacks.push(() => callback(...args));
    return realSetTimeout(callback, delay, ...args);
  });
  return () => {
    assert.ok(callbacks.length > 0, "persisted recovery deadline timer was armed");
    now = deadline;
    for (const fire of callbacks.splice(0)) fire();
  };
}

function waitForEpisodeAbort(signal: AbortSignal, observed: { reason?: Error }): Promise<void> {
  return new Promise<void>((_resolve, reject) => {
    const aborted = () => {
      observed.reason = signal.reason;
      reject(signal.reason);
    };
    if (signal.aborted) aborted();
    else signal.addEventListener("abort", aborted, { once: true });
  });
}

for (const alreadyAborted of [false, true]) {
  it(`wrong episode abort reason fails its assertion after cleanup (already aborted: ${alreadyAborted})`, async () => {
    const controller = new AbortController();
    const wrongReason = new Error("wrong episode reason");
    const observed: { reason?: Error } = {};
    if (alreadyAborted) controller.abort(wrongReason);
    let joined = false;
    const execution = waitForEpisodeAbort(controller.signal, observed).finally(() => { joined = true; });
    const rejection = assert.rejects(execution, error => error === wrongReason);
    if (!alreadyAborted) controller.abort(wrongReason);
    try {
      await rejection;
    } finally {
      await Promise.allSettled([execution]);
    }
    assert.equal(joined, true);
    assert.throws(() => assert.equal(observed.reason?.message, "recovery deadline exhausted"), {
      code: "ERR_ASSERTION",
    });
  });
}

async function requireEpisodeEntry(entry: Promise<void>, execution: Promise<unknown>): Promise<void> {
  const entered = await Promise.race([entry.then(() => true), execution.then(() => false, () => false)]);
  assert.equal(entered, true, "runner ended before the recovery seam");
}

for (const authority of ["covering FINAL", "uploaded-without-FINAL", "wrong-generation FINAL", "tampered physical journal", "unreadable physical journal", "incomplete coverage", "absent journal", "legacy journal", "shutdown during FINAL read", "fence during FINAL read"] as const) {
  it(`successor terminal retirement requires ${authority}`, async () => {
    const { claim, bare, clone, key } = await seed();
    claim.inventory_guarded = true;
    const feature = client.hasFeature.bind(client);
    client.hasFeature = name => name === "recovery_inventory_v1" || name === "claim_generation_fence" || feature(name);
    client.getRunOwnership = async () => ({
      status: api.states.some(s => s.body.status === "failed") ? "failed" : "running",
      claim_generation: 2, inventory_guarded: true,
    });
    client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
      hold_id: "fixture-hold", generation: 2, inventory_guarded: true, has_available_capture: false,
    }] });
    client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" });
    client.uploadRecoveryBundle = async (_run, capture, _manifest, stream) => {
      for await (const _chunk of stream) { /* consume the verified production bundle */ }
      return { capture_id: capture, state: "available", manifest_bound: true };
    };
    client.publishCheckpoint = async () => { throw new Error("publication unknown"); };
    // This report-only oracle must never override the authenticated physical journal.
    client.hasRecoveryRetirementAuthority = async () => true;
    let models = 0, successor = "", retirements = 0, terminalPreparations = 0, generationSettlements = 0;
    let descriptor: unknown;
    let settlingGeneration = false, guardedGenerationSettlements = 0;
    let terminalSteering: any;
    const retirementLog = recordingLogger();
    const retainedHome = path.join(homeDir, claim.run_id);
    fs.mkdirSync(retainedHome, { recursive: true });
    fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
    const journalKey = "fixture-journal-key";
    const sign = (record: RecoveryRecord) => {
      const macKey = createHmac("sha256", journalKey).update("uzi-recovery-journal-v1").digest();
      const mac = createHmac("sha256", macKey).update(canonicalJson(record)).digest("hex");
      const file = path.join(git.recoveryRoot, claim.run_id, record.captureId + ".json");
      fs.writeFileSync(file, JSON.stringify({ ...record, mac }));
      return file;
    };
    const installTerminalProof = async () => {
      const records = await r.snapshotBootRecoveries();
      const inventory = records.filter(rec => rec.coverageDigest).sort((a, b) => b.createdAt - a.createdAt)[0]!;
      assert.ok(inventory, "terminal producer froze a physical inventory");
      if (authority !== "uploaded-without-FINAL") {
        let acknowledged: RecoveryRecord = { ...inventory, finalAcknowledged: true, finalRequest: {
          disposition: { kind: "archive", capture_id: inventory.serverCaptureId!, source_sha: inventory.sourceSha, coverage_digest: inventory.coverageDigest! },
        } };
        if (authority === "wrong-generation FINAL") acknowledged = { ...acknowledged, generation: 3,
          coverageContext: { ...acknowledged.coverageContext!, generation: 3 } };
        const file = sign(acknowledged);
        if (authority === "tampered physical journal") fs.writeFileSync(file, JSON.stringify({ ...acknowledged, mac: "0".repeat(64) }));
        if (authority === "unreadable physical journal") { fs.unlinkSync(file); fs.mkdirSync(file); }
        if (authority === "incomplete coverage") {
          const source = records.find(rec => !rec.coverageDigest)!;
          assert.ok(source);
          sign({ ...source, sourceSha: "f".repeat(40) });
        }
        if (authority === "absent journal") {
          for (const name of fs.readdirSync(path.dirname(file))) fs.unlinkSync(path.join(path.dirname(file), name));
        }
        if (authority === "legacy journal") {
          for (const name of fs.readdirSync(path.dirname(file))) fs.unlinkSync(path.join(path.dirname(file), name));
          const source = records.find(rec => !rec.coverageDigest)!;
          sign({ ...source, inventoryGuarded: false });
        }
      }
    };
    const r = runnerWith(factory(async ctx => {
      models++;
      successor = ctx.worktreePath;
      await ctx.onModelTurnSettled!(1);
      descriptor = readJournal(bare, clone.branch);
      assert.equal((descriptor as any).recovery, undefined);
      const records = await r.snapshotBootRecoveries();
      const inventory = records.find(rec => rec.coverageDigest)!;
      assert.ok(inventory, "real guarded producer froze coverage");
      assert.equal(inventory.state, "uploaded", "an upload alone has no FINAL authority");
      throw new Error("fixture ends successor");
    }), fakeGitlab().gitlab, journalKey, retirementLog.logger, { recoveryRetryMs: 1 });
    const quiesce = (r as any).quiesceRun.bind(r);
    (r as any).quiesceRun = async (...args: unknown[]) => {
      if ((args[2] as any)?.site === "terminal_retire") terminalPreparations++;
      return quiesce(...args);
    };
    const settle = (r as any).settleRecoveryGeneration.bind(r);
    (r as any).settleRecoveryGeneration = async (...args: any[]) => {
      generationSettlements++;
      terminalSteering = args[1].steering;
      assert.equal(args[1].retainedLocalCustody, false);
      assert.equal(args[1].keepGuardedInventoryOpen, false);
      assert.deepEqual(args[1].completedRetainedSuccessor, descriptor);
      settlingGeneration = true;
      try {
        const result = await settle(...args);
        await installTerminalProof();
        return result;
      } finally { settlingGeneration = false; }
    };
    const settleInventory = (r as any).settleGuardedInventory.bind(r);
    (r as any).settleGuardedInventory = async (...args: any[]) => {
      if (settlingGeneration) {
        guardedGenerationSettlements++;
        assert.equal(args[0].claim_generation, 2);
      }
      return settleInventory(...args);
    };
    const interruptedFinal = authority === "shutdown during FINAL read" || authority === "fence during FINAL read";
    let interruptionInjected = false;
    if (interruptedFinal) {
      const recovery = (r as any).recovery;
      const cleanupState = recovery.inventoryCleanupState.bind(recovery);
      recovery.inventoryCleanupState = async (...args: any[]) => {
        const state = await cleanupState(...args);
        const rows = command(bare, "config", "--get-all", `uzi-attempts.${clone.branch}.entry`).split("\n").map(raw => JSON.parse(raw));
        if (!interruptionInjected && rows.filter(row => row.clonePath === successor).at(-1)?.state === "retired") {
          assert.equal(state, "acknowledged");
          if (authority === "shutdown during FINAL read") (r as any).shuttingDownGlobal = true;
          else terminalSteering.claimFence = () => ({ kind: "superseded" });
          interruptionInjected = true;
        }
        return state;
      };
    }
    const retire = git.retireRunnerClone.bind(git);
    git.retireRunnerClone = async (...args) => {
      retirements++;
      assert.equal(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2), true);
      assert.deepEqual(args[4]?.verifiedSuccessor?.expected, descriptor);
      return retire(...args);
    };
    await r.execute(claim);
    const movedSuccessor = authority === "covering FINAL" || interruptedFinal;
    if (interruptedFinal) {
      assert.equal(interruptionInjected, true, "lifecycle changed inside the awaited physical FINAL read");
      const held = fs.readdirSync(path.join(fx.dataDir, "runner-quarantine"));
      assert.equal(held.length, 1, "interrupted FINAL read retains the moved successor");
      assert.ok(fs.existsSync(path.join(fx.dataDir, "runner-quarantine", held[0]!)));
    }
    assert.equal(models, 1);
    assert.ok(successor);
    assert.equal(fs.existsSync(successor), !movedSuccessor,
      `successor retention for ${authority}; retirement calls: ${retirements}`);
    assert.ok(terminalPreparations >= 1, "completed successor reaches terminal inventory preparation");
    assert.equal(generationSettlements, 1, "completed successor reaches its generation settlement");
    assert.equal(guardedGenerationSettlements, 1, "its exact terminal generation actually settles guarded inventory");
    assert.equal(retirements, movedSuccessor ? 1 : 0);
    if (authority === "covering FINAL") {
      assert.equal(await (r as any).recovery.inventoryCleanupState(claim.run_id, 2), "acknowledged",
        "terminal retirement retains physical FINAL evidence for this generation");
    }
    const journal = readJournal(bare, clone.branch);
    assert.equal(journal.clonePath, movedSuccessor ? clone.path : successor);
    const ledger = command(bare, "config", "--get-all", `uzi-attempts.${clone.branch}.entry`).split("\n").map(raw => JSON.parse(raw));
    assert.equal(ledger.filter(entry => entry.clonePath === successor).at(-1).state, movedSuccessor ? "retired" : "live");
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.equal(fs.readFileSync(path.join(retainedHome, "session"), "utf8"), "predecessor session");
    assert.ok(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id));
    if (movedSuccessor) {
      const fresh = new (git.constructor as typeof import("../src/git.js").GitCache)(fx.dataDir, nullLogger());
      assert.equal((await fresh.discoverRetainedRecovery(fx.originPath, clone.branch, key, claim.run_id))?.journal.clonePath, clone.path);
    }
  });
}


for (const outcome of ["missing journal", "thrown discovery", "corrupt journal", "missing source", "aborted", "owner cancel", "shutdown", "fence"] as const) {
  it(`pending rediscovery retains custody on ${outcome}`, async () => {
    const { claim, bare, clone } = await seed();
    client.protocolFeatures = ["claim_generation_fence"];
    const discover = git.discoverRetainedRecovery.bind(git);
    let discoveries = 0, models = 0, reaps = 0, releases = 0, forgets = 0, deletes = 0;
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab);
    const retainedHome = path.join(homeDir, claim.run_id);
    fs.mkdirSync(retainedHome, { recursive: true });
    fs.writeFileSync(path.join(retainedHome, "session"), "retained session");
    const tip = command(clone.path, "rev-parse", "HEAD");
    const pin = `refs/uzi-recovery-episode/${claim.run_id}/${tip}`;
    command(bare, "update-ref", pin, tip);
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must not release"); };
    (r as any).recovery.forgetGeneration = async () => { forgets++; };
    git.deleteRecoveryPin = async () => { deletes++; };
    (r as any).quiesceRun = async () => { reaps++; throw new Error("must not reap"); };
    git.createOrAttachRunnerClone = async () => { throw new PendingRecoveryCaptureError(clone.path, clone.branch); };
    git.discoverRetainedRecovery = async (...args) => {
      if (++discoveries === 1) return undefined;
      if (outcome === "missing journal") return undefined;
      if (outcome === "thrown discovery") throw new Error("discovery storage read unavailable");
      if (outcome === "corrupt journal") command(bare, "config", `uzi-recovery.${clone.branch}.clone`, "{broken");
      if (outcome === "missing source") fs.renameSync(clone.path, clone.path + ".saved");
      if (["aborted", "owner cancel", "shutdown", "fence"].includes(outcome)) {
        const active = (r as any).activeRuns.get(claim.run_id);
        if (outcome === "owner cancel") active.steering.cancelled = true;
        if (outcome === "shutdown") { active.steering.cancelled = true; active.shuttingDown = true; }
        if (outcome === "fence") { active.steering.cancelled = true; active.steering.claimFence = () => ({ kind: "superseded" }); }
        active.cancel.abort(new Error("discovery interrupted"));
        throw active.cancel.signal.reason;
      }
      return discover(...args);
    };
    await r.execute(claim);
    assert.equal(discoveries, 2);
    assert.deepEqual({ models, reaps, releases, forgets, deletes }, { models: 0, reaps: 0, releases: 0, forgets: 0, deletes: 0 });
    const terminals = api.states.filter(s => s.body.status === "failed");
    if (["aborted", "shutdown", "fence"].includes(outcome)) assert.equal(terminals.length, 0);
    else {
      assert.equal(terminals.length, 1);
      assert.equal(terminals[0]!.body.claim_generation, 2);
      const reason = outcome === "owner cancel" ? "run cancelled" :
        outcome === "missing journal" ? "required journal is missing" :
        outcome === "missing source" ? "required source is missing" : "corrupt journal or unsafe";
      assert.ok(terminals[0]!.body.failure_reason?.includes(reason));
    }
    assert.equal(command(bare, "rev-parse", pin), tip);
    assert.equal(fs.readFileSync(path.join(outcome === "missing source" ? clone.path + ".saved" : clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.equal(fs.readFileSync(path.join(retainedHome, "session"), "utf8"), "retained session");
  });
}

for (const outcome of ["owner cancel", "shutdown", "fence"] as const) {
  it(`aborted retained ownership read propagates ${outcome} once`, async () => {
    const { claim, clone, bare } = await seed();
    client.protocolFeatures = ["claim_generation_fence"];
    let models = 0, reaps = 0, releases = 0, forgets = 0, deletes = 0, reads = 0;
    const before = readJournal(bare, clone.branch);
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab);
    (r as any).quiesceRun = async () => { reaps++; throw new Error("must not reap"); };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must not release"); };
    (r as any).recovery.forgetGeneration = async () => { forgets++; };
    git.deleteRecoveryPin = async () => { deletes++; };
    client.getRunOwnership = async (_run, signal) => {
      reads++;
      const active = (r as any).activeRuns.get(claim.run_id);
      active.steering.cancelled = true;
      if (outcome === "shutdown") active.shuttingDown = true;
      if (outcome === "fence") active.steering.claimFence = () => ({ kind: "superseded" });
      active.cancel.abort(new Error("ownership read cancelled"));
      assert.equal(signal?.aborted, true);
      throw signal!.reason;
    };
    await r.execute(claim);
    assert.equal(reads, 1);
    assert.deepEqual({ models, reaps, releases, forgets, deletes }, { models: 0, reaps: 0, releases: 0, forgets: 0, deletes: 0 });
    const terminals = api.states.filter(s => s.body.status === "failed");
    assert.equal(terminals.length, outcome === "owner cancel" ? 1 : 0);
    if (outcome === "owner cancel") {
      assert.equal(terminals[0]!.body.claim_generation, 2);
      assert.equal(terminals[0]!.body.failure_reason, "run cancelled");
    }
    const after = readJournal(bare, clone.branch);
    assert.deepEqual({ ...after, recovery: undefined }, { ...before, recovery: undefined });
    assert.equal(after.recovery.attempts, 1, "reservation is charged before ownership is read");
    assert.equal(after.recovery.stage, "capturing", "cancellation performs no blocker mutation");
    assert.ok(fs.existsSync(path.join(clone.path, "retained.txt")));
  });
}

it("failed episode completion retains suppression and settled owner cancellation propagates", async () => {
  const { claim, clone, bare } = await seed();
  let turns = 0, terminalPreparations = 0, settlements = 0;
  const complete = git.completeRecoveryEpisode.bind(git);
  const r = runnerWith(factory(async ctx => {
    turns++;
    git.completeRecoveryEpisode = async () => { throw new Error("completion write unavailable"); };
    await ctx.onModelTurnSettled!(1);
    assert.equal(await r.recoveryInventoryPending(claim.run_id, 2), true);
    assert.equal(readJournal(bare, clone.branch).recovery.stage, "ready-for-model");
    git.completeRecoveryEpisode = complete;
    const active = (r as any).activeRuns.get(claim.run_id);
    active.steering.cancelled = true;
    active.cancel.abort();
    await assert.rejects(ctx.onModelTurnSettled!(1), /retained recovery stopped/);
    throw new Error("run cancelled");
  }), fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
  const quiesce = (r as any).quiesceRun.bind(r);
  (r as any).quiesceRun = async (...args: any[]) => {
    if (args[2]?.site === "terminal_retire") terminalPreparations++;
    return quiesce(...args);
  };
  (r as any).settleRecoveryGeneration = async () => { settlements++; };
  await r.execute(claim);
  assert.equal(turns, 1);
  assert.equal(terminalPreparations, 0);
  assert.equal(settlements, 0);
  assert.ok(fs.existsSync(readJournal(bare, clone.branch).clonePath));
});

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed(iid = 2512, large = false) {
  const claim = gitlabClaim(iid, { claim_generation: 2, session_id: "predecessor-session" });
  if (large) {
    command(fx.originPath, "config", "user.name", "fixture");
    command(fx.originPath, "config", "user.email", "fixture@example.com");
    fs.writeFileSync(path.join(fx.originPath, "large-public"), randomBytes(64 * 1024));
    command(fx.originPath, "add", "large-public");
    command(fx.originPath, "commit", "-m", "public history");
  }
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone, key: `issue-${iid}` };
}
it("retained recovery credential switch acknowledges verified exact generation without releasing custody", async () => {
  await retainedSwitchRegression("capture", true);
});

it("retained recovery credential switch rejects unverified evidence and retains custody", async () => {
  await retainedSwitchRegression("ownership", false);
});

type RetainedSwitchOptions = {
  ack?: "lost" | "statusless" | "stale" | "idempotent";
  deadline?: boolean;
  drainDeadline?: boolean;
  proofTerminal?: "shutdown" | "fence" | "cancel";
  terminal?: "shutdown" | "fence" | "cancel";
  mismatch?: "pending" | "ownership";
  guarded?: boolean;
  unsafe?: "bundle" | "source" | "late bundle" | "late source";
  interruption?: "execution" | "lifecycle";
  cancelOnly?: boolean;
};

for (const ack of ["lost", "statusless", "stale", "idempotent"] as const) {
  for (const verified of [true, false]) {
    it(`retained recovery credential switch retains custody on ${ack} ${verified ? "release" : "clear"} ACK`, async () => {
      await retainedSwitchRegression(verified ? "capture" : "ownership", verified, { ack });
    });
  }
}
for (const terminal of ["shutdown", "fence", "cancel"] as const) {
  it(`retained recovery credential switch honors ${terminal} precedence`, async () => {
    await retainedSwitchRegression("capture", true, { terminal });
  });
}
for (const mismatch of ["pending", "ownership"] as const) {
  it(`retained recovery credential switch stops on ${mismatch} generation mismatch`, async () => {
    await retainedSwitchRegression("capture", true, { mismatch });
  });
}
it("retained recovery credential switch verifies existing guarded bundle", async () => {
  await retainedSwitchRegression("capture", true, { guarded: true });
});
for (const unsafe of ["bundle", "source"] as const) {
  it(`retained recovery credential switch rejects changed ${unsafe} evidence`, async () => {
    await retainedSwitchRegression("capture", false, { guarded: unsafe === "bundle", unsafe });
  });
}
it("retained recovery credential switch preserves cancel-controller typed reason", async () => {
  await retainedSwitchRegression("ownership", false, { cancelOnly: true });
});
for (const interruption of ["execution", "lifecycle"] as const) {
  it(`retained recovery credential switch rejects unrelated ${interruption} interruption`, async () => {
    await retainedSwitchRegression("capture", true, { interruption });
  });
}
it("retained recovery credential switch rejects changed retained sibling", async () => {
  await retainedSwitchRegression("adoption", false, { unsafe: "source" });
});
for (const unsafe of ["late bundle", "late source"] as const) {
  it(`retained recovery credential switch rejects ${unsafe} change during drain`, async () => {
    await retainedSwitchRegression("capture", false, { unsafe, guarded: unsafe === "late bundle" });
  });
}
it("retained recovery credential switch during successor adoption keeps both clones", async () => {
  await retainedSwitchRegression("adoption", true);
});

it("retained recovery credential switch during discovery keeps unknown custody", async () => {
  await retainedSwitchRegression("discovery", false);
});
it("retained recovery credential switch completes bounded ownership verification on deadline", async () => {
  await retainedSwitchRegression("ownership", false, { deadline: true });
});
it("retained recovery credential switch aborts drain at its operation deadline", async t => {
  let drains = 0;
  let drainSignal: AbortSignal | undefined;
  t.mock.method(MessageBatcher.prototype, "close", async (signal?: AbortSignal) => {
    drains++;
    drainSignal = signal;
    // Missing propagation must fail promptly instead of hanging the regression.
    if (!signal) return;
    await new Promise<void>(resolve => {
      if (signal.aborted) resolve();
      else signal.addEventListener("abort", () => resolve(), { once: true });
    });
  });
  await retainedSwitchRegression("ownership", false, { drainDeadline: true });
  assert.ok(drainSignal, "drain receives the independent operation signal");
  assert.equal(drainSignal.aborted, true, "drain settled only after deadline abort");
  assert.equal(drains, 1, "outer cleanup joins the bounded drain rather than draining again");
});
for (const proofTerminal of ["shutdown", "fence", "cancel"] as const) {
  it(`retained recovery credential switch honors ${proofTerminal} during verification`, async () => {
    await retainedSwitchRegression("ownership", false, { proofTerminal });
  });
}

async function retainedSwitchRegression(seam: "capture" | "ownership" | "adoption" | "discovery", verified: boolean,
  options: RetainedSwitchOptions = {}): Promise<void> {
  const { claim, bare, clone } = await seed();
  claim.inventory_guarded = options.guarded === true;
  client.protocolFeatures = ["claim_generation_fence", ...(options.guarded ? ["recovery_inventory_v1"] : [])];
  if (options.guarded) {
    client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
      hold_id: "fixture-hold", generation: 2, inventory_guarded: true, has_available_capture: false,
    }] });
    client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" });
    client.uploadRecoveryBundle = async (_run, capture, _manifest, stream) => {
      for await (const _chunk of stream) { /* real production bundle */ }
      return { capture_id: capture, state: "available", manifest_bound: true };
    };
  }
  const retainedHome = path.join(homeDir, claim.run_id);
  fs.mkdirSync(retainedHome, { recursive: true });
  fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
  let models = 0, captures = 0, releases = 0, injected = false, proofInterrupted = false;
  let flight: any;
  const log = recordingLogger();
  const r = runnerWith(factory(async () => { models++; throw new Error("must not resume model"); }),
    fakeGitlab().gitlab, "fixture-journal-key", log.logger, { recoveryRetryMs: 1 });
  const phaseClone = (r as any).phaseClone.bind(r);
  (r as any).phaseClone = async (...args: any[]) => {
    flight = args[1];
    return phaseClone(...args);
  };
  const owner = client.getRunOwnership.bind(client);
  client.getRunOwnership = async (...args) => {
    if (injected && (options.deadline || options.proofTerminal) && !proofInterrupted) {
      proofInterrupted = true;
      if (options.deadline) {
        assert.ok(args[1], "verification passes its independent bounded signal");
        return new Promise<Awaited<ReturnType<typeof owner>>>((_resolve, reject) => {
          const aborted = () => reject(args[1]!.reason);
          if (args[1]!.aborted) aborted();
          else args[1]!.addEventListener("abort", aborted, { once: true });
        });
      }
      if (options.proofTerminal === "shutdown") (r as any).shuttingDownGlobal = true;
      if (options.proofTerminal === "fence") flight.steering.claimFence = () => ({ kind: "superseded" });
      if (options.proofTerminal === "cancel") flight.steering.isCancelled = () => true;
    }
    return owner(...args);
  };
  const trip = async () => {
    injected = true;
    flight.steering.tripCredentialSwitch(2);
    if (options.interruption) {
      const interrupted = new AbortController();
      interrupted.abort(new Error("unrelated interruption"));
      if (options.interruption === "execution") flight.cancel = interrupted;
      else flight.steering.lifecycleSignal = () => interrupted.signal;
    }
    if (options.unsafe?.startsWith("late ")) {
      const close = flight.batcher.close.bind(flight.batcher);
      let changed = false;
      flight.batcher.close = async () => {
        await close();
        if (changed) return;
        changed = true;
        if (options.unsafe === "late source") fs.appendFileSync(path.join(clone.path, "retained.txt"), "changed during drain");
        else {
          const records = await r.snapshotBootRecoveries();
          const record = records.find(record => record.coverageDigest && record.bundlePath);
          assert.ok(record);
          fs.appendFileSync(record.bundlePath!, "changed during drain");
        }
      };
    }
    if (options.deadline) (r as any).codexBoundaryDeadlineMs = 50;
    if (options.drainDeadline) (r as any).codexBoundaryDeadlineMs = 1_000;
    if (options.cancelOnly) {
      const live = new AbortController();
      flight.steering.lifecycleSignal = () => live.signal;
    }
    if (options.terminal === "shutdown") (r as any).shuttingDownGlobal = true;
    if (options.terminal === "fence") flight.steering.claimFence = () => ({ kind: "superseded" });
    if (options.terminal === "cancel") flight.steering.isCancelled = () => true;
    if (options.mismatch === "pending") flight.steering.pendingCredentialSwitch = () => 3;
    if (options.mismatch === "ownership") api.setOwnershipStatus(claim.run_id, "running", 3);
    if (options.unsafe === "source") fs.appendFileSync(path.join(clone.path, "retained.txt"), "changed after capture");
    if (options.unsafe === "bundle") {
      const records = await r.snapshotBootRecoveries();
      const record = records.find(record => record.coverageDigest && record.bundlePath);
      assert.ok(record, "real guarded bundle already exists");
      fs.appendFileSync(record.bundlePath!, "tampered bytes");
    }
  };
  const report = client.reportState.bind(client);
  client.reportState = async (...args) => {
    const ack = await report(...args);
    if (!["credential_switch", "credential_switch_failed"].includes(args[1].status ?? "")) return ack;
    if (options.ack === "lost") throw new Error("ACK lost after server applied");
    if (options.ack === "statusless") return { applied: true };
    if (options.ack === "idempotent") return { applied: true, status: "running",
      ...(args[1].status === "credential_switch" ? { credentialSwitchReleased: true } : {}) };
    if (options.ack === "stale") return { applied: false, status: "running", staleClaim: true };
    return ack;
  };
  const discover = git.discoverRetainedRecovery.bind(git);
  git.discoverRetainedRecovery = async (...args) => {
    if (options.drainDeadline && injected) return undefined;
    const retained = await discover(...args);
    if (seam === "discovery" && !injected) await trip();
    return retained;
  };
  const ownership = (r as any).requireRetainedOwnership.bind(r);
  (r as any).requireRetainedOwnership = async (...args: any[]) => {
    flight = args[0];
    if (seam === "ownership" && !injected) {
      await trip();
    }
    return ownership(...args);
  };
  const capture = (r as any).captureRecoveryRestorePoint.bind(r);
  (r as any).captureRecoveryRestorePoint = async (...args: any[]) => {
    captures++;
    assert.equal(injected, false, "switch must never create another capture");
    return capture(...args);
  };
  const record = git.recordRecoveryCapture.bind(git);
  git.recordRecoveryCapture = async (...args) => {
    await record(...args);
    if (seam === "capture" && !injected) await trip();
  };
  const adopt = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => {
    const successor = await adopt(...args);
    if (seam === "adoption" && !injected) await trip();
    return successor;
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("custody must remain"); };
  await r.execute(claim);
  assert.equal(injected, true, "actual retained recovery await received the switch");
  if (options.interruption !== "execution") assert.ok(flight.cancel.signal.reason instanceof CredentialSwitchSignal);
  assert.equal(models, 0);
  assert.equal(captures, seam === "ownership" || seam === "discovery" ? 0 : 1);
  assert.equal(releases, 0);
  const reports = api.states.filter(s => s.body.status === "credential_switch" || s.body.status === "credential_switch_failed");
  assert.deepEqual(reports.map(s => [s.body.status, s.body.claim_generation]),
    options.terminal || options.mismatch || options.deadline || options.drainDeadline || options.proofTerminal || options.interruption ? [] :
      [[verified ? "credential_switch" : "credential_switch_failed", 2]]);
  assert.equal(api.states.some(s => s.body.status === "failed"), options.terminal === "cancel");
  if (options.deadline || options.proofTerminal) assert.equal(proofInterrupted, true, "verification await was interrupted");
  if (!options.terminal && !options.mismatch && !options.deadline && !options.drainDeadline && !options.proofTerminal && !options.interruption) {
    const disposition = log.lines.find(line => (line as { msg: string }).msg ===
      "retained credential switch stopped; all local work and custody retained") as { confirmed: boolean } | undefined;
    if (options.ack === "lost" || options.ack === "stale") assert.equal(disposition, undefined);
    else assert.equal(disposition?.confirmed, options.ack !== "statusless" || !verified);
  }
  for (const flag of ["preserveRecoveryClone", "preserveSession", "retainedEpisode",
    "retainedEpisodeCustody", "retainedLocalCustody", "keepGuardedInventoryOpen"]) {
    if (seam === "discovery" && flag === "retainedEpisode") assert.equal(flight[flag], undefined, flag);
    else assert.equal(flight[flag], true, flag);
  }
  assert.equal(fs.readFileSync(path.join(retainedHome, "session"), "utf8"), "predecessor session");
  assert.equal(flight.steering.pendingCredentialSwitch(), options.mismatch === "pending" ? 3 : 2, "never rearm retained recovery");
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"),
    "only local dirty work\n" + (options.unsafe === "source" ? "changed after capture" :
      options.unsafe === "late source" ? "changed during drain" : ""));
  const journal = readJournal(bare, clone.branch);
  if (seam === "adoption") {
    assert.notEqual(journal.clonePath, clone.path);
    assert.equal(fs.readFileSync(path.join(journal.clonePath, "retained.txt"), "utf8"), "only local dirty work\n");
  } else assert.equal(journal.clonePath, clone.path);
}

function readJournal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}
let callbackFailures: unknown[] = [];
afterEach(() => {
  const failures = callbackFailures;
  callbackFailures = [];
  assert.deepEqual(failures, [], "runner must not swallow model callback assertions");
});
function factory(run: (ctx: import("../src/executor.js").RunContext) => Promise<never>): ExecutorFactory {
  return runId => ({ homeDir: path.join(homeDir, runId), executor: { run: async ctx => {
    try { return await run(ctx); }
    catch (error) { if (error instanceof assert.AssertionError) callbackFailures.push(error); throw error; }
  } } });
}

for (const outageAt of [1, 2]) {
  it(`missing source finalization needs no ownership GET (former outage position ${outageAt})`, async () => {
    const { claim, bare, clone } = await seed();
    const before = readJournal(bare, clone.branch);
    client.protocolFeatures = ["claim_generation_fence"];
    const tip = command(clone.path, "rev-parse", "HEAD");
    const pin = `refs/uzi-recovery-episode/${claim.run_id}/${tip}`;
    command(bare, "update-ref", pin, tip);
    fs.renameSync(clone.path, clone.path + ".saved");
    let reads = 0, models = 0, fetches = 0, releases = 0;
    const ownership = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (...args) => {
      if (++reads === outageAt) throw new Error("ownership endpoint unavailable");
      return ownership(...args);
    };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    git.ensureClone = async () => { fetches++; throw new Error("must not refresh"); };
    await runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab).execute(claim);
    assert.equal(reads, 0, "discovery finalization uses the fenced terminal endpoint");
    assert.equal(models, 0);
    assert.equal(fetches, 0);
    assert.equal(releases, 0);
    const journal = readJournal(bare, clone.branch);
    assert.equal(journal.clonePath, before.clonePath);
    assert.equal(journal.runId, before.runId);
    assert.equal(journal.recovery.blocker, "source_missing");
    assert.equal(journal.recovery.attempts, 3);
    assert.equal(journal.recovery.stage, "blocked");
    assert.equal(command(bare, "rev-parse", pin), tip);
    assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assert.equal(fs.readFileSync(path.join(clone.path + ".saved", "retained.txt"), "utf8"), "only local dirty work\n");
  });
}

it("corrupt journal finalizes through the terminal fence with all ownership reads down", async () => {
  const { claim, bare, clone } = await seed();
  const corrupt = '{"runId":"broken"';
  client.protocolFeatures = ["claim_generation_fence"];
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, corrupt);
  let reads = 0, models = 0;
  client.getRunOwnership = async () => { reads++; throw new Error("ownership endpoint unavailable"); };
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab).execute(claim);
  assert.equal(reads, 0);
  assert.equal(models, 0);
  assert.equal(command(bare, "config", `uzi-recovery.${clone.branch}.clone`), corrupt);
  assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.ok(fs.existsSync(clone.path));
});

for (const stage of ["reservation", "discovery"] as const) {
  it(`cancelled queued ${stage} settles execute before bare lock release without a charge`, async () => {
    const { claim, bare, clone } = await seed();
    const before = readJournal(bare, clone.branch);
    const configBytes = fs.readFileSync(path.join(bare, "config"));
    let entered!: () => void, acquired!: () => void, release!: () => void;
    const entry = new Promise<void>(resolve => { entered = resolve; });
    const held = new Promise<void>(resolve => { acquired = resolve; });
    // This holder is outside the recovery ALS context.
    const holder = git.withBareLock(bare, async () => {
      acquired();
      await new Promise<void>(resolve => { release = resolve; });
    });
    await held;
    git.recoveryAttemptMode = async () => false;
    if (stage === "reservation") {
      git.discoverRetainedRecovery = async () => ({ barePath: bare, journal: before });
      const reserve = git.reserveRecoveryIteration.bind(git);
      git.reserveRecoveryIteration = async (...args) => { entered(); return reserve(...args); };
    } else {
      const discover = git.discoverRetainedRecovery.bind(git);
      git.discoverRetainedRecovery = async (...args) => { entered(); return discover(...args); };
    }
    const retainedHome = path.join(homeDir, claim.run_id);
    fs.mkdirSync(retainedHome, { recursive: true });
    const sessionArtifact = path.join(retainedHome, "retained-session.jsonl");
    fs.writeFileSync(sessionArtifact, "predecessor session evidence\n");
    let models = 0, settled = false;
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab);
    let watchdog: ReturnType<typeof setTimeout> | undefined;
    const execution = r.execute(claim).finally(() => { settled = true; });
    try {
      await requireEpisodeEntry(entry, execution);
      (r as any).activeRuns.get(claim.run_id).cancel.abort();
      await Promise.race([execution, new Promise<void>(resolve => { watchdog = setTimeout(resolve, 250); })]);
      assert.equal(settled, true, "cancelled flight must settle while the lock is still held");
      assert.deepEqual(readJournal(bare, clone.branch), before);
    } finally {
      if (watchdog) clearTimeout(watchdog);
      (r as any).activeRuns.get(claim.run_id)?.cancel.abort(new Error("fixture cleanup"));
      release();
      await Promise.allSettled([holder, execution]);
    }
    await git.withBareLock(bare, async () => {});
    assert.deepEqual(readJournal(bare, clone.branch), before);
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), configBytes);
    assert.equal(fs.readFileSync(sessionArtifact, "utf8"), "predecessor session evidence\n",
      "a stop before discovery completes retains the predecessor HOME");
    assert.equal(models, 0);
    assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  });
}

it("lifecycle cancellation settles the actual attempt-mode child without discovery or a terminal", async () => {
  const { claim, bare, clone } = await seed();
  const before = fs.readFileSync(path.join(bare, "config"));
  let started!: () => void, finish!: (value: { code: number }) => void;
  const entry = new Promise<void>(resolve => { started = resolve; });
  const completed = new Promise<{ code: number }>(resolve => { finish = resolve; });
  let cancelled = false, settledChild = false, models = 0, discoveries = 0, producerEntered = false;
  const stop = new AbortController();
  const discover = git.discoverRetainedRecovery.bind(git);
  git.discoverRetainedRecovery = async (...args) => { discoveries++; return discover(...args); };
  const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }), fakeGitlab().gitlab);
  const execution = git.withBoundaryProcessSpawner(async () => {
    producerEntered = true;
    started();
    return {
      stdout: Readable.from([]), stderr: Readable.from([]), stdin: null, completed,
      cancel: async () => { cancelled = true; settledChild = true; finish({ code: -1 }); },
    };
  }, stop.signal, () => r.execute(claim));
  try {
    await requireEpisodeEntry(entry, execution);
    assert.equal(producerEntered, true);
    (r as any).activeRuns.get(claim.run_id).cancel.abort();
    await execution;
  } finally {
    stop.abort(new Error("fixture cleanup"));
    (r as any).activeRuns.get(claim.run_id)?.cancel.abort(new Error("fixture cleanup"));
    await Promise.allSettled([execution, ...(producerEntered ? [completed] : [])]);
  }
  assert.equal(cancelled, true);
  assert.equal(settledChild, true);
  assert.equal(discoveries, 0);
  assert.equal(models, 0);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
});

for (const setupDelay of [0, 5_000]) {
  it(`queued reservation deadline admits no proof or later charge and bounds finalization lock waits (setup delay ${setupDelay}ms)`, async (t) => {
    const { claim, bare, clone, key } = await seed();
    await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
    const before = readJournal(bare, clone.branch);
    before.recovery.startedAt = Date.now() - 60_000;
    before.recovery.deadline = before.recovery.startedAt + 300_000;
    command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(before));
    client.protocolFeatures = ["claim_generation_fence"];
    git.recoveryAttemptMode = async () => false;
    git.discoverRetainedRecovery = async () => ({ barePath: bare, journal: before });
    let release!: () => void, acquired!: () => void, entered!: () => void;
    const held = new Promise<void>(resolve => { acquired = resolve; });
    const entry = new Promise<void>(resolve => { entered = resolve; });
    const holder = git.withBareLock(bare, async () => {
      acquired();
      await new Promise<void>(resolve => { release = resolve; });
    });
    await held;
    const expire = episodeDeadline(t, before.recovery.deadline, setupDelay);
    let deadlineAborted = false;
    let observedReason: Error | undefined;
    const reserve = git.reserveRecoveryIteration.bind(git);
    git.reserveRecoveryIteration = async (...args) => {
      const scope = (git as any).recoveryOperations.getStore();
      scope.signal.addEventListener("abort", () => {
        observedReason = scope.signal.reason;
        deadlineAborted = true;
      }, { once: true });
      entered();
      const reservation = reserve(...args);
      queueMicrotask(expire);
      return reservation;
    };
    let models = 0, proofs = 0, reads = 0, settled = false;
    client.getRunOwnership = async () => { reads++; throw new Error("must not read"); };
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
      fakeGitlab().gitlab, undefined, nullLogger(), { codexBoundaryDeadlineMs: 100 });
    (r as any).quiesceRun = async () => { proofs++; throw new Error("must not prove"); };
    let watchdog: ReturnType<typeof setTimeout> | undefined;
    const execution = r.execute(claim).finally(() => { settled = true; });
    try {
      await requireEpisodeEntry(entry, execution);
      await Promise.race([execution, new Promise<void>(resolve => { watchdog = setTimeout(resolve, 2_000); })]);
      assert.equal(settled, true, "deadline and bookkeeping must finish before unlock");
      assert.deepEqual(readJournal(bare, clone.branch), before);
    } finally {
      if (watchdog) clearTimeout(watchdog);
      (r as any).activeRuns.get(claim.run_id)?.cancel.abort(new Error("fixture cleanup"));
      release();
      await Promise.allSettled([holder, execution]);
    }
    await git.withBareLock(bare, async () => {});
    assert.deepEqual(readJournal(bare, clone.branch), before);
    assert.equal(models, 0);
    assert.equal(proofs, 0);
    assert.equal(reads, 0);
    assert.equal(deadlineAborted, true, "queued reservation entered and its deadline aborted the wait");
    assert.equal(observedReason?.message, "recovery deadline exhausted");
    assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  });

}

for (const disposition of ["stale_claim", "released"]) {
  it(`missing source terminal refusal (${disposition}) preserves custody and replacement state`, async () => {
    const { claim, bare, clone } = await seed();
    client.protocolFeatures = ["claim_generation_fence"];
    fs.renameSync(clone.path, clone.path + ".saved");
    api.failStateWhen(claim.run_id, body => body.status === "failed", { runStatus: "running", disposition });
    let reports = 0, reads = 0, releases = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => {
      if (args[1].status === "failed") {
        reports++;
        assert.equal(args[1].claim_generation, 2);
        api.setOwnershipStatus(claim.run_id, "running", 3);
      }
      return report(...args);
    };
    client.getRunOwnership = async () => { reads++; throw new Error("must not read"); };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    await runnerWith(factory(async () => { throw new Error("must not model"); }), fakeGitlab().gitlab).execute(claim);
    assert.equal(reports, 1);
    assert.equal(reads, 0);
    assert.equal(releases, 0);
    assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
    assert.equal(readJournal(bare, clone.branch).recovery.blocker, "source_missing");
    assert.equal(fs.readFileSync(path.join(clone.path + ".saved", "retained.txt"), "utf8"), "only local dirty work\n");
  });
}

for (const scenario of ["refused now", "refused wall", "owner cancel", "shutdown", "fence",
  "switch", "unrearmed pause", "fresh pause", "disk before rearm", "disk after rearm",
  "generic pre-aborted", "generic later abort"] as const) {
  it(`retained liveness: ${scenario}`, async () => {
    const { claim, bare, clone, key } = await seed();
    client.protocolFeatures = ["claim_generation_fence"];
    client.publishCheckpoint = async () => { throw new Error("publication unknown"); };
    const retainedHome = path.join(homeDir, claim.run_id);
    fs.mkdirSync(retainedHome, { recursive: true });
    fs.writeFileSync(path.join(retainedHome, "session"), "predecessor session");
    const observations: {
      models: number; parks: number; spent?: boolean; live?: boolean; parked?: unknown;
      before?: any; after?: any; settled?: boolean; error?: unknown;
      signalBefore?: boolean; signalAfter?: boolean; signalReason?: unknown; pending?: boolean;
      episodeCustody?: boolean;
    } = { models: 0, parks: 0 };
    const generic = scenario.startsWith("generic");
    const reason = new Error("generic cancel-only interruption");
    let flight: any;
    const r = runnerWith(factory(async ctx => {
      observations.models++;
      const steering = flight.steering;
      const route = (kind: string, body?: string) => steering.route(kind, body, 1, false, undefined);
      observations.before = readJournal(bare, clone.branch);
      if (generic) {
        if (scenario === "generic pre-aborted") flight.cancel.abort(reason);
      } else {
        route("pause", scenario === "refused wall" ? "wall" : "now");
        observations.spent = flight.cancel.signal.aborted && flight.cancel.signal.reason instanceof PauseNowSignal;
        if (scenario === "disk before rearm") steering.requestDiskStop();
        if (scenario !== "unrearmed pause") {
          observations.parked = scenario === "refused wall"
            ? await ctx.parkForWall!({ completedCount: 0 }) : await ctx.parkForPause!({ completedCount: 0 });
          if (scenario === "refused wall") ctx.clearWallMode!();
        }
        observations.live = !steering.lifecycleSignal().aborted;
        if (scenario === "owner cancel") route("cancel");
        if (scenario === "shutdown") (r as any).shuttingDownGlobal = true;
        if (scenario === "fence") steering.claimFence = () => ({ kind: "superseded" });
        if (scenario === "switch") steering.tripCredentialSwitch(2);
        if (scenario === "fresh pause") route("pause", "now");
        if (scenario === "disk after rearm") steering.requestDiskStop();
      }
      // Observe the actual operation composition before and after a cancel-only abort.
      const signal = (r as any).retainedLifecycleSignal(flight) as AbortSignal;
      observations.signalBefore = signal.aborted;
      if (scenario === "generic later abort") flight.cancel.abort(reason);
      observations.signalAfter = signal.aborted;
      observations.signalReason = signal.reason;
      try {
        await ctx.onModelTurnSettled!(1);
        observations.settled = true;
      } catch (error) {
        observations.error = error;
      }
      observations.after = readJournal(bare, clone.branch);
      observations.pending = await r.recoveryInventoryPending(claim.run_id, 2);
      observations.episodeCustody = flight.retainedEpisodeCustody;
      // Route a typed switch through the runner's real outer handler.
      if (scenario === "switch") throw observations.error;
      throw new Error("fixture ends retained liveness scenario");
    }), fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
    const phaseClone = (r as any).phaseClone.bind(r);
    (r as any).phaseClone = async (...args: any[]) => {
      flight = args[1];
      return phaseClone(...args);
    };
    (r as any).handlePausePark = async () => { observations.parks++; return false; };
    (r as any).enterWallPark = async () => { observations.parks++; return "refused"; };
    let releases = 0, settlements = 0;
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("custody must remain"); };
    (r as any).settleRecoveryGeneration = async () => { settlements++; };
    await r.execute(claim);
    assert.equal(observations.models, 1, "actual retained successor reaches model");
    assert.equal(observations.before.recovery.attempts, 1);
    assert.equal(observations.before.recovery.stage, "ready-for-model");
    if (!generic) {
      assert.equal(observations.spent, true, "shared cancel retains its first PauseNowSignal");
      assert.equal(observations.live, !["unrearmed pause", "disk before rearm"].includes(scenario),
        "refusal replaces only a nonterminal current lifecycle");
      assert.equal(flight.cancel.signal.reason instanceof PauseNowSignal, true);
      assert.equal(observations.parks, scenario === "unrearmed pause" ? 0 : 1);
      if (scenario !== "unrearmed pause")
        assert.equal(observations.parked, scenario === "refused wall" ? "refused" : false);
    }
    if (scenario === "refused now" || scenario === "refused wall") {
      assert.equal(observations.error, undefined);
      assert.equal(observations.settled, true);
      assert.equal(observations.signalBefore, false, "spent pause cannot poison composed operations");
      assert.equal(observations.after.recovery, undefined, "trusted settlement clears persisted budget");
      assert.equal(observations.episodeCustody, false, "trusted settlement clears episode custody suppression");
      assert.equal(observations.pending, true, "settlement grants no independent inventory retirement authority");
    } else {
      assert.equal(observations.settled, undefined);
      if (scenario === "switch") assert.ok(observations.error instanceof CredentialSwitchSignal);
      else assert.match(String(observations.error), /retained recovery stopped/);
      assert.deepEqual(observations.after.recovery, observations.before.recovery);
      assert.equal(observations.pending, true);
      assert.equal(observations.episodeCustody, true);
    }
    if (["unrearmed pause", "fresh pause", "disk before rearm", "disk after rearm"].includes(scenario))
      assert.equal(observations.signalAfter, true, "current lifecycle interruption aborts retained operations");
    if (generic) {
      assert.equal(flight.steering.lifecycleSignal().aborted, false);
      assert.equal(observations.signalBefore, scenario === "generic pre-aborted");
      assert.equal(observations.signalAfter, true);
      assert.equal(observations.signalReason, reason);
    }
    const switches = api.states.filter(s => s.body.status === "credential_switch" || s.body.status === "credential_switch_failed");
    if (scenario === "switch") {
      assert.equal(switches.length, 1, "switch after refusal reaches retained exact-generation handler");
      assert.equal(switches[0]!.body.claim_generation, 2);
      assert.equal(flight.steering.pendingCredentialSwitch(), 2);
    } else assert.deepEqual(switches, []);
    assert.equal(releases, 0);
    assert.equal(settlements, 0);
    const journal = readJournal(bare, clone.branch);
    assert.notEqual(journal.clonePath, clone.path);
    assert.ok(journal.retainedSources.some((source: any) => source.clonePath === clone.path));
    for (const clonePath of [clone.path, journal.clonePath])
      assert.equal(fs.readFileSync(path.join(clonePath, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.equal(fs.readFileSync(path.join(retainedHome, "session"), "utf8"), "predecessor session");
    assert.equal(await git.recoveryAttemptMode(fx.originPath, key), true);
    assert.ok(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id));
  });
}

it("retained dirty work resumes locally on a fresh unwired path and resets only on trusted settled notification", async () => {
  const { claim, bare, clone, key } = await seed();
  let model = 0;
  const { gitlab } = fakeGitlab();
  client.publishCheckpoint = async () => { throw new Error("publication unknown"); };
  client.hasRecoveryRetirementAuthority = async () => true;
  const r = runnerWith(factory(async ctx => {
    model++;
    assert.notEqual(ctx.worktreePath, clone.path);
    assert.match(ctx.worktreePath, /\.attempt-.*-g2-/);
    assert.equal(ctx.sessionId, undefined);
    assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
    const journal = readJournal(bare, clone.branch);
    assert.equal(journal.recovery.attempts, 1);
    assert.equal(journal.recovery.stage, "ready-for-model");
    await ctx.onModelTurnSettled!(1);
    assert.equal(readJournal(bare, clone.branch).recovery, undefined);
    assert.equal(await r.recoveryInventoryPending(claim.run_id, 2), false,
      "trusted settlement clears only episode custody suppression");
    throw new Error("fixture stops after settled turn");
  }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
  await r.execute(claim);
  assert.equal(model, 1);
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.equal(await git.recoveryAttemptMode(fx.originPath, key), true);
  const journal = readJournal(bare, clone.branch);
  assert.ok(journal.retainedSources.some((s: any) => s.clonePath === clone.path));
  assert.equal(fs.existsSync(clone.path), true);
  assert.equal(fs.existsSync(journal.clonePath), true);
  assert.ok(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id));
});

for (const succeeds of [true, false]) {
  it(`early forge failures consume the durable three-iteration budget (success=${succeeds})`, async () => {
    const { claim, bare, clone } = await seed();
    const ensure = git.ensureClone.bind(git);
    let fetches = 0, models = 0;
    git.ensureClone = async (...args) => {
      fetches++;
      if (!succeeds || fetches < 3) throw new Error("temporary connection reset");
      return ensure(...args);
    };
    const { gitlab } = fakeGitlab();
    await runnerWith(factory(async () => {
      models++;
      assert.equal(readJournal(bare, clone.branch).recovery.attempts, 3);
      throw new Error("model interrupted without settled evidence");
    }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    assert.equal(fetches, 3);
    assert.equal(models, succeeds ? 1 : 0);
    const episode = readJournal(bare, clone.branch).recovery;
    assert.equal(episode.attempts, 3);
    assert.equal(episode.stage, succeeds ? "ready-for-model" : "blocked");
    if (!succeeds) {
      assert.equal(episode.blocker, "budget_exhausted");
      assert.ok(api.states.some(s => s.body.status === "failed"));
    }
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assert.equal(fs.existsSync(clone.path), true);
  });
}

it("known missing source blocks before credentialed refresh and terminal retains journal", async () => {
  const { claim, bare, clone } = await seed();
  fs.renameSync(clone.path, clone.path + ".saved");
  let fetches = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not refresh"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { throw new Error("must not model"); }), gitlab).execute(claim);
  assert.equal(fetches, 0);
  assert.equal(readJournal(bare, clone.branch).recovery.blocker, "source_missing");
  assert.ok(api.states.some(s => s.body.status === "failed"));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("lost exact ownership stops the retained retry without a terminal or a park", async () => {
  const { claim, bare, clone } = await seed();
  let fetches = 0, models = 0;
  git.ensureClone = async () => {
    fetches++;
    api.setOwnershipStatus(claim.run_id, "running", 3);
    throw new Error("connection reset");
  };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not run"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(fetches, 1);
  assert.equal(models, 0);
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 1);
  assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
  assert.ok(fs.existsSync(clone.path));
});

it("typed exact ownership 404 stops after one charge without touching the source", async () => {
  const { claim, bare, clone } = await seed();
  const before = fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8");
  api.setOwnershipNotOwned(claim.run_id);
  let fetches = 0, models = 0, captures = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not refresh"); };
  git.recordRecoveryCapture = async () => { captures++; throw new Error("must not capture"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(readJournal(bare, clone.branch).recovery.attempts, 1);
  assert.equal(fetches, 0);
  assert.equal(models, 0);
  assert.equal(captures, 0);
  assert.equal(api.states.some(s => ["failed", "recovery_wait"].includes(s.body.status)), false);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), before);
});

it("corrupt owned journal fails with custody while preserving the exact corrupt bytes", async () => {
  const { claim, bare, clone } = await seed();
  const corrupt = '{"runId":"broken"';
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, corrupt);
  let models = 0, fetches = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not fetch"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }), gitlab).execute(claim);
  assert.equal(models, 0);
  assert.equal(fetches, 0);
  assert.equal(command(bare, "config", `uzi-recovery.${clone.branch}.clone`), corrupt);
  assert.ok(api.states.some(s => s.body.status === "failed"));
  assert.ok(fs.existsSync(clone.path));
});

it("missing-source blocker persistence failure still reports retained terminal failure", async () => {
  const { claim, bare, clone } = await seed();
  const before = readJournal(bare, clone.branch);
  fs.renameSync(clone.path, clone.path + ".saved");
  git.blockRecoveryEpisode = async () => { throw new Error("disk write refused"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { throw new Error("must not model"); }), gitlab).execute(claim);
  assert.deepEqual(readJournal(bare, clone.branch), before);
  assert.ok(api.states.some(s => s.body.status === "failed"));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("ownership read outages consume all three durable attempts without source mutation or model admission", async () => {
  const { claim, bare, clone } = await seed();
  let models = 0, fetches = 0;
  client.getRunOwnership = async () => { throw new Error("ownership read connection reset"); };
  git.ensureClone = async () => { fetches++; throw new Error("must not fetch"); };
  const { gitlab } = fakeGitlab();
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(models, 0);
  assert.equal(fetches, 0);
  const exhausted = readJournal(bare, clone.branch).recovery;
  assert.equal(exhausted.attempts, 3);
  assert.equal(exhausted.stage, "blocked");
  assert.equal(exhausted.blocker, "budget_exhausted");
  assert.ok(fs.existsSync(clone.path));
  assert.equal(api.states.some(s => s.body.status === "failed"), true,
    "the fenced terminal endpoint ends exhaustion even while ownership reads are unavailable");
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

for (const setupDelay of [0, 5_000]) {
  it(`persisted deadline interrupts a blocked ownership read and keeps the charged budget (setup delay ${setupDelay}ms)`, async (t) => {
    const { claim, bare, clone, key } = await seed();
    await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
    const old = readJournal(bare, clone.branch);
    const startedAt = Date.now() - 60_000;
    old.recovery.startedAt = startedAt;
    old.recovery.deadline = startedAt + 300_000;
    command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(old));
    const expire = episodeDeadline(t, old.recovery.deadline, setupDelay);
    let entered!: () => void;
    const entry = new Promise<void>(resolve => { entered = resolve; });
    claim.inventory_guarded = true;
    const feature = client.hasFeature.bind(client);
    client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
    const outbox = new Outbox({
      root: path.join(fx.dataDir, "deadline-outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000,
    });
    await outbox.init();
    let reads = 0, models = 0, lateReads = 0, lateWork = 0, releases = 0;
    const observed: { reason?: Error } = {};
    const authority = client.hasRecoveryRetirementAuthority.bind(client);
    client.hasRecoveryRetirementAuthority = async (...args) => {
      if (Date.now() >= old.recovery.deadline) lateWork++;
      return authority(...args);
    };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
      fakeGitlab().gitlab, "fixture-journal-key", nullLogger(), { recoveryRetryMs: 1, outbox });
    for (const method of ["quiesceRun", "transferRestorePointToTrustedBare"] as const) {
      const original = (r as any)[method].bind(r);
      (r as any)[method] = async (...args: unknown[]) => {
        if (Date.now() >= old.recovery.deadline) lateWork++;
        return original(...args);
      };
    }
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => {
      if (args[1].status === "failed") {
        assert.equal(readJournal(bare, clone.branch).recovery.blocker, "budget_exhausted");
        assert.equal(readJournal(bare, clone.branch).recovery.stage, "blocked");
      }
      return report(...args);
    };
    client.getRunOwnership = async (_run, signal) => {
      if (Date.now() >= old.recovery.deadline) lateReads++;
      if (++reads !== 1) {
        await new Promise(resolve => setTimeout(resolve, 600));
        return { status: "running", claim_generation: 2 };
      }
      assert.ok(signal);
      assert.equal(readJournal(bare, clone.branch).recovery.attempts, 2, "ownership read entered the charged iteration");
      entered();
      const aborted = waitForEpisodeAbort(signal, observed);
      queueMicrotask(expire);
      await aborted;
      throw new Error("unreachable");
    };
    const timers = process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length;
    const execution = r.execute(claim);
    try {
      await requireEpisodeEntry(entry, execution);
      await execution;
    } finally {
      (r as any).activeRuns.get(claim.run_id)?.cancel.abort(new Error("fixture cleanup"));
      await Promise.allSettled([execution]);
    }
    assert.ok(process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length <= timers);
    assert.ok(await outbox.readTerminalJournal(claim.run_id, claim.claim_generation!));
    assert.equal(lateReads, 0);
    assert.equal(reads, 1);
    assert.equal(lateWork, 0);
    assert.equal(releases, 0);
    assert.equal(observed.reason?.message, "recovery deadline exhausted");
    assert.equal(models, 0);
    assert.equal(readJournal(bare, clone.branch).recovery.attempts, 2);
    assert.equal(readJournal(bare, clone.branch).recovery.blocker, "budget_exhausted");
    assert.equal(readJournal(bare, clone.branch).recovery.startedAt, startedAt);
    assert.equal(readJournal(bare, clone.branch).recovery.deadline, old.recovery.deadline);
    assert.ok(api.states.some(s => s.body.status === "failed"));
  });
}

it("late blocker finalization cannot change a replacement episode on the same source", async () => {
  const { claim, bare, clone, key } = await seed();
  const expected = { runId: claim.run_id, clonePath: clone.path };
  const old = await git.reserveRecoveryIteration(bare, clone.branch, key, expected, 1);
  const replacement = readJournal(bare, clone.branch);
  replacement.recovery.startedAt += 1;
  replacement.recovery.deadline += 1;
  command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(replacement));
  await assert.rejects(git.blockRecoveryEpisode(bare, clone.branch, key, expected, "budget_exhausted", old),
    /recovery episode changed/);
  assert.deepEqual(readJournal(bare, clone.branch), replacement);
});

it("capture-closing executor is replaced before actual resumed model execution", async () => {
  const { claim, bare, clone } = await seed();
  let created = 0, models = 0, closed = false;
  const { gitlab } = fakeGitlab();
  const make: ExecutorFactory = runId => {
    const ordinal = ++created;
    return { homeDir: path.join(homeDir, runId), executor: {
      // A boundary-aware fake: retained capture closes admission permanently.
      settleForCredentialFreeCapture: async () => { closed = true; return { kind: "observed_empty" }; },
      sandboxesCommands: true,
      run: async ctx => {
        assert.equal(ordinal, 2);
        assert.ok(closed);
        models++;
        assert.notEqual(ctx.worktreePath, clone.path);
        throw new Error("model interruption");
      },
    } };
  };
  // The factory recreation condition also covers capture-capable executors without a live safety epoch.
  await runnerWith(make, gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(created, 2);
  assert.equal(models, 1);
  assert.equal(readJournal(bare, clone.branch).recovery.stage, "ready-for-model");
});

for (const rejects of [false, true]) {
  it(`unclean predecessor disposal after adoption blocks both sources (rejects=${rejects})`, async () => {
    const { claim, bare, clone } = await seed();
    let created = 0, models = 0, disposed = 0;
    const make: ExecutorFactory = runId => {
      created++;
      return { homeDir: path.join(homeDir, runId), executor: {
        sandboxesCommands: true,
        settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
        safety: {
          kind: "codex",
          withBoundary: async (request, action) => {
            const controller = new AbortController();
            const timer = setTimeout(() => controller.abort(new Error("fixture boundary expired")), request.deadlineMs);
            try {
              return await action({ epoch: 1, boundary: request.boundary, signal: controller.signal } as any);
            } finally { clearTimeout(timer); }
          },
          spawnBoundaryProcess: async (_permit, request) => {
            const [executable, ...args] = request.argv;
            const child = spawn(executable!, args, { cwd: request.cwd, env: request.env, stdio: ["pipe", "pipe", "pipe"] });
            const completed = new Promise<{ code: number }>((resolve, reject) => {
              child.once("error", reject);
              child.once("exit", code => resolve({ code: code ?? 1 }));
            });
            return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr,
              cancel: async () => { if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL"); await completed; },
              completed };
          },
          dispose: async () => {
            disposed++;
            if (rejects) throw new Error("disposal rejected");
            return { kind: "incomplete", errors: [{ category: "protocol", message: "fixture undrained" }] };
          },
        },
        run: async () => { models++; throw new Error("must not model"); },
      } };
    };
    const timers = process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length;
    await runnerWith(make, fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(process.getActiveResourcesInfo().filter(resource => resource === "Timeout").length <= timers);
    assert.equal(created, 1);
    assert.equal(models, 0);
    assert.ok(disposed > 0);
    const journal = readJournal(bare, clone.branch);
    assert.notEqual(journal.clonePath, clone.path);
    assert.equal(journal.recovery.stage, "blocked");
    assert.equal(journal.recovery.blocker, "quiescence_failed");
    assert.equal(journal.recovery.attempts, 1);
    assert.ok(journal.retainedSources.some((s: any) => s.clonePath === clone.path));
    for (const source of [clone.path, journal.clonePath]) {
      assert.ok(fs.existsSync(source));
      assert.equal(fs.readFileSync(path.join(source, "retained.txt"), "utf8"), "only local dirty work\n");
    }
    assert.ok(api.states.some(s => s.body.status === "failed"));
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  });
}

it("verified guarded thin fallback permits local adoption despite unknown upload and keeps every source", async () => {
  const { claim, bare, clone } = await seed(2512, true);
  claim.inventory_guarded = true;
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  client.getRunOwnership = async () => ({ status: "running", claim_generation: 2, inventory_guarded: true });
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "fixture-hold", generation: 2, inventory_guarded: true, has_available_capture: false,
  }] });
  client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _capture, manifest, stream) => {
    for await (const chunk of stream) assert.ok(chunk.length);
    assert.ok(manifest.prerequisite_shas!.length > 0);
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { throw new Error("status unavailable"); };
  let releases = 0, models = 0, productions = 0, publications = 0;
  client.publishCheckpoint = async (_run, _tip, stream) => {
    publications++;
    for await (const chunk of stream) assert.ok(chunk.length);
    throw new Error("checkpoint publication response unknown");
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must keep custody"); };
  const produce = git.produceRecoveryBundle.bind(git);
  git.produceRecoveryBundle = async (p, opts) => {
    productions++;
    // Lower only the cap: the real guarded self-contained -> thin producer still runs.
    return produce(p, { ...opts, maxBytes: 8 * 1024 });
  };
  const { gitlab } = fakeGitlab();
  const log = recordingLogger();
  const r = runnerWith(factory(async ctx => {
    models++;
    assert.equal(productions, 1, JSON.stringify(log.lines));
    assert.equal(readJournal(bare, clone.branch).recovery.attempts, 1);
    assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.notEqual(ctx.worktreePath, clone.path);
    await ctx.onModelTurnSettled!(1);
    throw new Error("fixture model stops");
  }), gitlab, "fixture-journal-key", log.logger, { recoveryRetryMs: 1 });
  await r.execute(claim);
  assert.equal(models, 1, JSON.stringify({ logs: log.lines, states: api.states, journal: readJournal(bare, clone.branch), records: await r.snapshotBootRecoveries() }));
  // The model interruption can create another terminal archive after model admission.
  assert.ok(productions >= 1);
  assert.ok(publications > 0);
  assert.equal(releases, 0);
  const records = await r.snapshotBootRecoveries();
  const thin = records.find(record => record.selfContained === false && record.prerequisiteShas!.length > 0);
  assert.ok(thin);
  const localProof = (r as unknown as { recovery: { verifiedLocalCapture: (
    record: typeof thin, signal?: AbortSignal,
  ) => Promise<unknown> } }).recovery;
  assert.equal(await localProof.verifiedLocalCapture(thin, AbortSignal.abort()), undefined,
    "cancelled local integrity proof cannot grant adoption authority");
  assert.ok(fs.existsSync(thin.bundlePath!), "cancellation retains the archive bytes");
  const journal = readJournal(bare, clone.branch);
  assert.equal(journal.recovery, undefined);
  assert.ok(fs.existsSync(clone.path));
  assert.ok(fs.existsSync(journal.clonePath));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
});

it("real production declared overflow blocks runner with decoded_history_limit and retains source custody", async () => {
  const { claim, bare, clone } = await seed();
  claim.inventory_guarded = false;
  let models = 0, releases = 0, proofs = 0;
  const log = recordingLogger();
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
  const record = git.recordRecoveryCapture.bind(git);
  git.recordRecoveryCapture = async (...args) => {
    proofs++;
    const tip = args[5];
    const oid = command(bare, "rev-parse", tip + ":retained.txt");
    const objectPath = path.join(bare, "objects", oid.slice(0, 2), oid.slice(2));
    fs.mkdirSync(path.dirname(objectPath), { recursive: true });
    if (fs.existsSync(objectPath)) fs.chmodSync(objectPath, 0o600);
    // Capture is complete; tamper before the real metadata proof, without allocating the declared body.
    fs.writeFileSync(objectPath, deflateSync(Buffer.from("blob 1073741825\0")));
    assert.equal(command(bare, "cat-file", "-s", oid), "1073741825");
    return record(...args);
  };
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 }).execute(claim);
  assert.equal(proofs, 1);
  assert.equal(models, 0);
  assert.equal(releases, 0);
  const journal = readJournal(bare, clone.branch);
  assert.equal(journal.clonePath, clone.path);
  assert.equal(journal.recovery.stage, "blocked");
  assert.equal(journal.recovery.blocker, "decoded_history_limit");
  assert.equal(journal.recovery.attempts, 1);
  assert.equal(journal.recovery.restoreTip, undefined);
  assert.equal(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id), "");
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.ok(api.states.some(s => s.body.status === "failed" &&
    s.body.failure_reason?.includes("1 GiB decoded verification limit")));
  assert.ok(log.lines.some(line => JSON.stringify(line).includes("1 GiB decoded verification limit")));
});

for (const failure of ["cap", "timeout", "interruption"] as const) {
  for (const setupDelay of failure === "timeout" ? [0, 5_000] : [0]) {
    it(`closure verification ${failure} keeps all source custody and never admits a model (setup delay ${setupDelay}ms)`, async (t) => {
      const { claim, bare, clone, key } = await seed();
      let releases = 0, models = 0, verified = 0;
      const log = recordingLogger();
      client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain"); };
      const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
        fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 });
      let expire!: () => void, entered!: () => void;
      let persisted: any;
      const observed: { reason?: Error } = {};
      const entry = new Promise<void>(resolve => { entered = resolve; });
      git.verifyRecoveryClosure = async () => {
        verified++;
        entered();
        if (failure === "cap") throw new RecoveryClosureLimitError();
        if (failure === "interruption") {
          (r as any).activeRuns.get(claim.run_id).cancel.abort();
          const scope = (git as any).recoveryOperations.getStore();
          scope.signal.throwIfAborted();
          throw new Error("fixture interruption");
        }
        // A persisted original deadline bounds this blocked verifier, without refreshing it.
        const scope = (git as any).recoveryOperations.getStore();
        assert.equal(readJournal(bare, clone.branch).recovery.attempts, 2, "verifier entered the charged iteration");
        const aborted = waitForEpisodeAbort(scope.signal, observed);
        queueMicrotask(expire);
        await aborted;
        return false;
      };
      if (failure === "timeout") {
        await git.reserveRecoveryIteration(bare, clone.branch, key, { runId: claim.run_id, clonePath: clone.path }, 1);
        const before = readJournal(bare, clone.branch);
        before.recovery.startedAt = Date.now() - 60_000;
        before.recovery.deadline = before.recovery.startedAt + 300_000;
        command(bare, "config", `uzi-recovery.${clone.branch}.clone`, JSON.stringify(before));
        persisted = before.recovery;
        expire = episodeDeadline(t, persisted.deadline, setupDelay);
      }
      const execution = r.execute(claim);
      try {
        await requireEpisodeEntry(entry, execution);
        await execution;
      } finally {
        (r as any).activeRuns.get(claim.run_id)?.cancel.abort(new Error("fixture cleanup"));
        await Promise.allSettled([execution]);
      }
      assert.equal(verified, 1);
      assert.equal(models, 0);
      assert.equal(releases, 0);
      const journal = readJournal(bare, clone.branch);
      assert.equal(journal.clonePath, clone.path);
      assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
      assert.equal(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-recovery-episode/" + claim.run_id), "");
      assert.equal(journal.recovery.restoreTip, undefined);
      assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
      if (failure === "interruption") {
        assert.equal(journal.recovery.stage, "capturing");
      } else {
        assert.equal(journal.recovery.stage, "blocked");
        assert.equal(journal.recovery.blocker, failure === "cap" ? "decoded_history_limit" : "budget_exhausted");
        assert.ok(api.states.some(s => s.body.status === "failed"));
      }
      if (failure === "timeout") {
        assert.equal(observed.reason?.message, "recovery deadline exhausted");
        assert.equal(journal.recovery.attempts, 2);
        assert.equal(journal.recovery.startedAt, persisted.startedAt);
        assert.equal(journal.recovery.deadline, persisted.deadline);
      }
      if (failure === "cap") {
        assert.ok(api.states.some(s => s.body.failure_reason?.includes("1 GiB decoded verification limit")));
        assert.ok(log.lines.some(line => JSON.stringify(line).includes("1 GiB decoded verification limit")));
        assert.equal(journal.recovery.attempts, 1);
      }
    });
  }
}
