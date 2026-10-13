import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { deflateSync } from "node:zlib";
import type { ExecutorFactory } from "../src/runner.js";
import { DataVolumeGuard } from "../src/disk-full.js";
import { noProofReseed, nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
async function seed(large = false) {
  const claim = gitlabClaim(2512, { claim_generation: 2, session_id: "predecessor-session" });
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
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", 2);
  return { claim, bare, clone };
}
function journal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}
const callbackAssertions: assert.AssertionError[] = [];
function assertExecutorCallbacks() {
  const failures = callbackAssertions.splice(0);
  if (failures.length) throw failures[0];
}
afterEach(assertExecutorCallbacks);
function factory(run: (ctx: import("../src/executor.js").RunContext) => Promise<never>): ExecutorFactory {
  return runId => ({ homeDir: path.join(homeDir, runId), executor: { run: async ctx => {
    try { return await run(ctx); }
    catch (error) {
      if (error instanceof assert.AssertionError) callbackAssertions.push(error);
      throw error;
    }
  } } });
}
it("M3 executor assertion witness survives runner model-error handling", async () => {
  const { claim } = await seed();
  const failure = new assert.AssertionError({ message: "deliberate callback assertion witness" });
  await runnerWith(factory(async () => { throw failure; }),
    fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
  assert.throws(assertExecutorCallbacks, error => error === failure);
});

for (const corruption of ["unreadable", "hash mismatch"] as const) {
  it(`M3 corrupted blob (${corruption}) rejects closure and capture proof`, async () => {
    const { claim, bare, clone } = await seed(true);
    const tip = command(clone.path, "rev-parse", "HEAD");
    const blob = command(bare, "rev-parse", `${tip}:large-public`);
    const object = path.join(bare, "objects", blob.slice(0, 2), blob.slice(2));
    const source = { runId: claim.run_id, clonePath: clone.path };
    const reservation = await git.reserveRecoveryIteration(bare, clone.branch, "issue-2512", source, 1);
    assert.equal(await git.verifyRecoveryClosure(bare, tip), true);
    const before = command(bare, "config", `uzi-recovery.${clone.branch}.clone`);
    fs.chmodSync(object, 0o600);
    fs.writeFileSync(object, corruption === "unreadable"
      ? Buffer.from("unreadable git object")
      : deflateSync(Buffer.concat([Buffer.from("blob 7"), Buffer.from([0]), Buffer.from("changed")])));
    command(bare, "rev-list", "--objects", "--missing=error", tip);
    assert.equal(await git.verifyRecoveryClosure(bare, tip), false, "object presence is not content integrity");
    await assert.rejects(git.recordRecoveryCapture(bare, clone.branch, "issue-2512", source, reservation.attempts, tip));
    assert.equal(command(bare, "config", `uzi-recovery.${clone.branch}.clone`), before);
    assert.equal(command(bare, "for-each-ref", "--format=%(refname)", `refs/uzi-recovery-episode/${claim.run_id}/${tip}`), "");
  });
}

function guarded(claim: ReturnType<typeof gitlabClaim>) {
  const remote = { uploads: 0, uploadedBytes: 0 };
  claim.inventory_guarded = true;
  client.protocolFeatures = ["claim_generation_fence"];
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  client.getRunOwnership = async () => ({ status: "running", claim_generation: 2, inventory_guarded: true });
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "fixture-hold", generation: 2, inventory_guarded: true, has_available_capture: false,
  }] });
  client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" });
  client.uploadRecoveryBundle = async (_run, _capture, manifest, stream) => {
    remote.uploads++;
    assert.ok(manifest.prerequisite_shas!.length > 0);
    for await (const chunk of stream) remote.uploadedBytes += chunk.length;
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { throw new Error("status unavailable"); };
  return remote;
}

// This deliberately models worker requests/outcomes, not API cap or SQL atomicity.
// At most two claim requests: one failure never blocks a sibling test.
it("M3 failing retained capture ends one successor hold instead of parking and reclaiming", async () => {
  const { claim, bare, clone } = await seed();
  client.protocolFeatures = ["claim_generation_fence"];
  let eligible = true, generation = 1, models = 0, captures = 0, releases = 0;
  const requests: number[] = [], holds: number[] = [];
  client.claimRun = async () => {
    requests.push(generation + 1);
    if (!eligible) return null;
    eligible = false;
    holds.push(++generation);
    api.setOwnershipStatus(claim.run_id, "running", generation);
    return { ...claim, claim_generation: generation };
  };
  const report = client.reportState.bind(client);
  client.reportState = async (...args) => {
    const ack = await report(...args);
    if (ack.applied) {
      api.setOwnershipStatus(claim.run_id, args[1].status, generation);
      if (args[1].status === "recovery_wait") eligible = true;
    }
    return ack;
  };
  const status = git.worktreeStatus.bind(git);
  // A finite outage exposes the old park/reclaim loop without an unbounded base retry.
  // The fixed policy must exhaust before this fourth, healthy capture is reachable.
  git.worktreeStatus = async (...args) => {
    captures++;
    return captures <= 3 ? null : status(...args);
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must keep custody"); };
  const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
  for (let request = 0; request < 2; request++) {
    const next = await client.claimRun();
    if (next) await r.execute(next);
  }
  assert.deepEqual(holds, [2], "M3 capture exhaustion must not open a second successor generation/hold");
  assert.deepEqual(requests, [2, 3]);
  assert.equal(captures, 3, "M3 exactly three capture attempts in the same successor generation");
  assert.equal(models, 0);
  assert.equal(releases, 0);
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2), JSON.stringify({ states: api.states, features: client.protocolFeatures }));
  assert.equal(journal(bare, clone.branch).recovery.attempts, 3);
  assert.equal(journal(bare, clone.branch).recovery.stage, "blocked");
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
});

it("M3 real full-to-thin fallback adopts through executor.run with unknown remote ACKs and intact custody", async () => {
  const { claim, bare, clone } = await seed(true);
  const remote = guarded(claim);
  let models = 0, releases = 0, publications = 0;
  const bundles: Array<{ fullBytes: number; thinBytes: number; prerequisites: string[]; tip: string; bundle: string }> = [];
  const produce = git.produceRecoveryBundle.bind(git);
  git.produceRecoveryBundle = async (p, opts) => {
    const full = await produce(p, { ...opts, outPath: opts.outPath + ".full", maxBytes: 1024 * 1024 });
    assert.equal(full.selfContained, true);
    assert.ok(full.byteSize > 8 * 1024, "fixture full history exceeds the test cap");
    const thin = await produce(p, { ...opts, maxBytes: 8 * 1024 });
    const bytes = fs.readFileSync(thin.bundlePath);
    const header = bytes.subarray(0, bytes.indexOf("\n\n")).toString("utf8");
    const prerequisites = header.split("\n").filter(line => line.startsWith("-")).map(line => line.slice(1, 41)).sort();
    assert.ok(thin.byteSize < 8 * 1024);
    assert.equal(thin.selfContained, false);
    assert.ok(prerequisites.length > 0, "actual bundle header carries prerequisites");
    assert.deepEqual(prerequisites, thin.prerequisiteShas);
    command(p, "bundle", "verify", thin.bundlePath);
    for (const prerequisite of prerequisites) {
      command(p, "rev-list", "--objects", "--missing=error", prerequisite);
      command(p, "merge-base", "--is-ancestor", prerequisite, "refs/remotes/origin/main");
    }
    bundles.push({ fullBytes: full.byteSize, thinBytes: thin.byteSize, prerequisites, tip: thin.sourceSha, bundle: thin.bundlePath });
    fs.rmSync(full.bundlePath);
    return thin;
  };
  client.publishCheckpoint = async (_run, _tip, stream) => {
    publications++;
    for await (const _chunk of stream) { /* consume actual pack */ }
    throw new Error("checkpoint publication unknown");
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must keep custody"); };
  const r = runnerWith(factory(async ctx => {
    models++;
    assert.equal(bundles.length, 1);
    assert.notEqual(ctx.worktreePath, clone.path);
    assert.match(ctx.worktreePath, /\.attempt-.*-g2-/);
    assert.equal(ctx.sessionId, undefined, "retained predecessor session cannot execute");
    assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
    assert.equal(journal(bare, clone.branch).recovery.stage, "ready-for-model");
    assert.equal(journal(bare, clone.branch).recovery.attempts, 1);
    throw new Error("model interrupted before settled completion");
  }), fakeGitlab().gitlab, "fixture-journal-key", nullLogger(), { recoveryRetryMs: 1 });
  await r.execute(claim);
  assert.ok(bundles.length > 0, "the real full-to-thin producer completed bundle and prerequisite verification");
  assert.ok(remote.uploads > 0, "the real thin bundle reached the unknown-ACK upload");
  assert.ok(remote.uploadedBytes > 0, "the unknown-ACK upload consumed actual bundle bytes");
  assert.equal(models, 1, "M3 verified real thin fallback must enter actual executor.run");
  assert.ok(publications > 0);
  assert.equal(releases, 0);
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  const retained = journal(bare, clone.branch);
  assert.equal(retained.recovery.attempts, 1, "interrupted model cannot refund the budget");
  assert.equal(retained.recovery.stage, "ready-for-model");
  assert.ok(retained.retainedSources.some((source: any) => source.clonePath === clone.path));
  assert.ok(fs.existsSync(clone.path));
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.ok(fs.existsSync(retained.clonePath));
  assert.equal(command(bare, "rev-parse", `refs/uzi-recovery-episode/${claim.run_id}/${bundles[0]!.tip}`), bundles[0]!.tip);
  const records = await r.snapshotBootRecoveries();
  const descriptor = records.find(record => record.sourceSha === bundles[0]!.tip && record.selfContained === false);
  assert.ok(descriptor, "durable thin descriptor survives unknown upload ACK");
  assert.ok(fs.existsSync(descriptor.bundlePath!));
  assert.deepEqual(descriptor.prerequisiteShas, bundles[0]!.prerequisites);
});

function assertBlocked(bare: string, clone: { path: string; branch: string }, blocker: string, attempts: number) {
  const retained = journal(bare, clone.branch);
  assert.equal(retained.clonePath, clone.path, "no successor replaces the predecessor");
  assert.equal(retained.recovery.stage, "blocked");
  assert.equal(retained.recovery.blocker, blocker);
  assert.equal(retained.recovery.attempts, attempts);
  assert.ok(api.states.some(s => s.body.status === "failed" && s.body.claim_generation === 2));
  assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
}

for (const failure of ["oversize", "unverifiable prerequisite object", "missing prerequisite object"] as const) {
  it(`M3 guarded ${failure} fails before adoption with exact blocker and retained pins`, async () => {
    const { claim, bare, clone } = await seed(true);
    const remote = guarded(claim);
    let models = 0, releases = 0, productions = 0;
    let prerequisiteClosure: boolean | undefined;
    const produce = git.produceRecoveryBundle.bind(git);
    const produced: Array<Awaited<ReturnType<typeof produce>>> = [];
    const archivePaths: string[] = [];
    const dirty = randomBytes(32 * 1024);
    if (failure === "oversize") fs.writeFileSync(path.join(clone.path, "large-dirty"), dirty);
    git.produceRecoveryBundle = async (p, opts) => {
      productions++;
      // Real full-to-thin production: both attempts exceed this cap for large dirty work.
      const result = await produce(p, { ...opts, maxBytes: 8 * 1024 });
      assert.equal(result.selfContained, false);
      assert.ok(result.prerequisiteShas.length > 0);
      command(p, "bundle", "verify", result.bundlePath);
      produced.push(result);
      return result;
    };
    const upload = client.uploadRecoveryBundle.bind(client);
    client.uploadRecoveryBundle = async (...args) => {
      try { return await upload(...args); }
      finally {
        const result = produced[0]!;
        const record = (await r.snapshotBootRecoveries()).find(record => record.sourceSha === result.sourceSha && record.bundlePath);
        assert.ok(record?.bundlePath, "upload uses the installed, journaled archive");
        archivePaths.push(record.bundlePath);
        if (failure !== "oversize") {
          // Lose a real cached public blob AFTER the thin archive has been verified and read.
          const blob = command(bare, "rev-parse", `${result.prerequisiteShas[0]}:large-public`);
          const object = path.join(bare, "objects", blob.slice(0, 2), blob.slice(2));
          assert.ok(fs.existsSync(object), "fixture prerequisite blob is loose");
          if (failure === "missing prerequisite object") fs.unlinkSync(object);
          else {
            fs.chmodSync(object, 0o600);
            fs.writeFileSync(object, "unreadable git object");
          }
          prerequisiteClosure = await git.verifyRecoveryClosure(bare, result.prerequisiteShas[0]!);
        }
      }
    };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    const r = runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
      fakeGitlab().gitlab, "fixture-journal-key", nullLogger(), { recoveryRetryMs: 1 });
    await r.execute(claim);
    assert.equal(models, 0);
    assert.equal(releases, 0);
    assert.equal(productions, 1, "permanent guarded blocker consumes one charged attempt");
    assertBlocked(bare, clone, failure === "oversize" ? "oversize" : "prerequisites_unavailable", 1);
    if (failure !== "oversize") assert.equal(prerequisiteClosure, false, "actual prerequisite closure must be verifiable before adoption");
    const records = await r.snapshotBootRecoveries();
    assert.ok(records.length > 0, "durable recovery inventory remains");
    for (const record of records) {
      assert.equal(command(bare, "rev-parse", `refs/uzi-coverage/${claim.run_id}/${record.generation}/${record.coverageDigest}`), record.sourceSha);
    }
    if (failure === "oversize") {
      assert.equal(remote.uploads, 0);
      assert.deepEqual(fs.readFileSync(path.join(clone.path, "large-dirty")), dirty);
      assert.ok(records.some(record => record.reason === "oversized"));
    } else {
      assert.equal(remote.uploads, 1);
      assert.ok(remote.uploadedBytes > 0);
      assert.ok(fs.existsSync(archivePaths[0]!), "failed proof keeps actual archive bytes");
    }
  });
}

it("M3 journal discovery intercepts ordinary full-volume park before ensureClone", async () => {
  const { claim, bare, clone } = await seed();
  client.protocolFeatures = ["claim_generation_fence", "recovery_cause_data_volume_full"];
  let fetches = 0, models = 0, reclaims = 0, releases = 0;
  git.ensureClone = async () => { fetches++; throw new Error("must not refresh while full"); };
  const guard = new DataVolumeGuard({
    dataDir: fx.dataDir,
    statfs: () => ({ bsize: 4096, blocks: 1_000_000, bfree: 1, bavail: 1, files: 1_000_000, ffree: 900_000 }),
    stat: async () => ({ dev: 7 }),
    reclaim: async () => { reclaims++; },
  });
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
  await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
    fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1, dataVolume: guard }).execute(claim);
  assert.equal(fetches, 0);
  assert.equal(models, 0);
  assert.equal(releases, 0);
  assert.equal(reclaims, 3);
  assertBlocked(bare, clone, "budget_exhausted", 3);
});

for (const [nextGeneration, publication] of [[2, "500"], [2, "post-pack throw"], [3, "post-pack throw"]] as const) {
  it(`M3 ${publication}: ready g2 successor is recaptured onto fresh g${nextGeneration} without budget reset`, async () => {
    const { claim, bare, clone } = await seed();
    const paths: string[] = [], tips: string[] = [];
    let models = 0, captures = 0, releases = 0;
    const status = git.worktreeStatus.bind(git);
    git.worktreeStatus = async (...args) => { captures++; return status(...args); };
    client.publishCheckpoint = async (_run, _tip, stream) => {
      for await (const _chunk of stream) { /* actual pack is consumed before the lost ACK */ }
      if (publication === "500") return { ok: false, httpStatus: 500 };
      throw new Error("post-pack response unknown");
    };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain custody"); };
    const r = runnerWith(factory(async ctx => {
      models++;
      paths.push(ctx.worktreePath);
      const retained = journal(bare, clone.branch);
      assert.equal(retained.recovery.attempts, models);
      assert.equal(retained.recovery.stage, "ready-for-model");
      tips.push(retained.recovery.restoreTip);
      assert.match(ctx.worktreePath, new RegExp(`\\.attempt-.*-g${models === 1 ? 2 : nextGeneration}-`));
      assert.equal(ctx.sessionId, undefined);
      assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "retained.txt"), "utf8"), "only local dirty work\n");
      if (models === 1) fs.writeFileSync(path.join(ctx.worktreePath, "successor-dirty"), "new successor work\n");
      else {
        assert.notEqual(ctx.worktreePath, paths[0], "prior ready path cannot directly enter model");
        assert.equal(fs.readFileSync(path.join(ctx.worktreePath, "successor-dirty"), "utf8"), "new successor work\n");
        command(bare, "merge-base", "--is-ancestor", tips[0]!, tips[1]!);
      }
      throw new Error("model interrupted before settled evidence");
    }), fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 });
    await r.execute(claim);
    api.setOwnershipStatus(claim.run_id, "running", nextGeneration);
    await r.execute({ ...claim, claim_generation: nextGeneration });
    assert.equal(models, 2);
    assert.ok(captures >= 2, "both admissions capture through the runner");
    assert.equal(releases, 0);
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    const retained = journal(bare, clone.branch);
    assert.equal(retained.recovery.attempts, 2);
    for (const source of [clone.path, ...paths]) assert.ok(fs.existsSync(source));
    for (const tip of tips) assert.equal(command(bare, "rev-parse", `refs/uzi-recovery-episode/${claim.run_id}/${tip}`), tip);
    assert.equal(fs.readFileSync(path.join(paths[0]!, "successor-dirty"), "utf8"), "new successor work\n");
  });
}

for (const failure of ["status", "WIP", "tracking"] as const) {
  it(`M3 ${failure} capture failure preserves dirty source and consumes one three-attempt budget`, async () => {
    const { claim, bare, clone } = await seed();
    let models = 0, releases = 0, failures = 0;
    if (failure === "status") git.worktreeStatus = async () => { failures++; return null; };
    if (failure === "WIP") git.commitWipMarker = async () => { failures++; return false; };
    if (failure === "tracking") git.verifyRunnerTrackingCovers = async () => { failures++; return false; };
    client.releaseRecoveryCustody = async () => { releases++; throw new Error("must retain"); };
    await runnerWith(factory(async () => { models++; throw new Error("must not model"); }),
      fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    assert.equal(models, 0);
    assert.equal(failures, 3);
    assert.equal(releases, 0);
    assert.equal(journal(bare, clone.branch).recovery.attempts, 3);
    assert.equal(journal(bare, clone.branch).recovery.stage, "blocked");
    assert.ok(api.states.some(s => s.body.status === "failed"));
    assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  });
}
