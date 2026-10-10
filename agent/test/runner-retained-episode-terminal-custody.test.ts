import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createHmac } from "node:crypto";
import { PendingRecoveryCaptureError } from "../src/git.js";
import { canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { installEpisodeFixture, command, factory, readJournal, seed, waitForEpisodeAbort } from "./runner-retained-episode-fixture.js";

installHarness();
installEpisodeFixture();

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
