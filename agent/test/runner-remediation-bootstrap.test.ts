import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import childProcess, { execFileSync } from "node:child_process";
import { AsyncLocalStorage } from "node:async_hooks";
import { syncBuiltinESMExports } from "node:module";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { test } from "node:test";
import { GitCache, type CheckpointRange } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { SteeringChannel } from "../src/steering.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";

installHarness();
const BRANCH = "agent/issue-3747", KEY = "issue-3747";
const ACTIVE = "uzi-recovery." + BRANCH + ".clone";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}

async function fixture(enrolled = true) {
  const claim = gitlabClaim(3747, { claim_generation: 2, session_id: "predecessor-session" });
  api.setOwnershipStatus(claim.run_id, "running", 2);
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const base = command(bare, "rev-parse", "refs/remotes/origin/main");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, claim.run_id, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} });
  const source = { runId: claim.run_id, clonePath: clone.path, attemptId: ATTEMPT };
  await git.markRecoveryCapture(bare, clone.path, BRANCH, claim.run_id, ATTEMPT);
  const seedJournal = JSON.parse(command(bare, "config", ACTIVE));
  const fetch = () => git.fetchAgentBranch(bare, clone.path, BRANCH, claim.run_id, {
    context: { runId: claim.run_id, generation: 2, kind: "issue", branch: BRANCH, barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } }, producingSource: source,
  });
  const commit = (label: string, parent = base) => {
    const sha = command(clone.path, "-c", "user.name=t", "-c", "user.email=t@example.com",
      "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
    command(clone.path, "reset", "--hard", sha);
    return sha;
  };
  await fetch();
  const q = commit("flagged"); await fetch();
  if (enrolled) await git.recordRemediationScan(bare, BRANCH, source,
    { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  const d = commit("excluded inventory", q); await fetch();
  const s = commit("replacement"); await fetch();
  const journal = () => JSON.parse(command(bare, "config", ACTIVE));
  const write = (value: unknown) => command(bare, "config", ACTIVE, JSON.stringify(value));
  const detach = async () => {
    const reservation = await git.reserveRecoveryIteration(bare, BRANCH, KEY, source);
    await git.recordRecoveryCapture(bare, BRANCH, KEY, source, reservation.attempts, s, s);
    await git.detachTerminalRetained(bare, BRANCH, KEY, journal());
  };
  const fresh = () => {
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ gitleaksBin: (git as any).gitleaksBin }));
    const runner = new RunRunner(client, cache, () => ({ homeDir, executor: {
      run: async () => { throw new Error("unused executor"); },
    } }), nullLogger(), 20, undefined, {
      gitlab: fakeGitlab().gitlab, teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1,
    });
    const cancel = new AbortController();
    const steering = new SteeringChannel(client, claim.run_id, 5, nullLogger(), cancel, { claimGeneration: 2 });
    // This is a fresh flight: no prior process-local remediation knowledge.
    const flight: any = {
      runId: claim.run_id, branch: BRANCH, barePath: bare, worktreePath: clone.path, runnerClone: clone,
      runKind: "issue", claimGeneration: 2, claimGenerations: new Map(),
      runLog: nullLogger(), batcher: { emit: () => {} }, reportedPublishOutcomes: new Set(),
      cancel, steering,
      owedAnnounced: { heads: new Set(), archived: new Set() }, inventoryArchiveNotices: new Set(),
      publishedTip: d, checkpointFloor: d,
    };
    return { cache, runner, subject: runner as any, flight };
  };
  const snapshot = () => ({
    config: fs.readFileSync(path.join(bare, "config"), "utf8"),
    refs: command(bare, "for-each-ref", "--format=%(refname) %(objectname)"),
    head: command(clone.path, "rev-parse", "HEAD"),
  });
  return { claim, bare, base, clone, source, seedJournal, q, d, s, fetch, commit, journal, write, detach, fresh, snapshot };
}

for (const mode of ["active", "protected", "malformed", "readback", "overflow", "large"] as const) {
  test("fresh sinks restore durable negative restrictions: " + mode, async t => {
    const f = await fixture();
    if (mode === "large") {
      const j = f.journal();
      j.remediationRecovery.excludedRoots = Array.from({ length: 900 }, (_, i) =>
        (i + 1).toString(16).padStart(40, "0"));
      f.write(j); // Schema-only negative roots, no object or grant claim.
    }
    if (mode === "protected" || mode === "overflow") await f.detach();
    if (mode === "overflow") {
      const j = JSON.parse(command(f.bare, "config", "uzi-retained." + f.claim.run_id + ".journal")).journal;
      // Each canonical body fits its bound; their independent union does not.
      j.remediationRecovery.flaggedOIDs = Array.from({ length: 64 }, (_, i) =>
        (i + 1).toString(16).padStart(40, "0"));
      j.remediationRecovery.edges = [];
      delete j.remediationRecovery.active;
      f.write(j);
    }
    if (mode === "malformed") command(f.bare, "config", ACTIVE, "{");
    const before = f.snapshot();
    const { cache, runner, subject, flight } = f.fresh();
    const unavailable = mode === "malformed" || mode === "readback" || mode === "overflow";
    let queries = 0;
    const read = cache.readRemediationRestrictionFacts.bind(cache);
    t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof read>) => {
      queries++;
      assert.equal(args.length, 3, "custody only: no selected source or lease");
      return mode === "readback" ? read(args[0], args[1], args[2], undefined, { readback: async () => "different" }) : read(...args);
    });
    const build = t.mock.method(cache, "bridgeToFloors");
    const update = t.mock.method(cache, "updateTrackingRef");
    const floor = flight.checkpointFloor;
    try {
      assert.equal(flight.remediationRestrictions, undefined);
      assert.deepEqual(await subject.bridgeBareTrackingRefIfDivergent(f.bare, BRANCH, flight, nullLogger()),
        { kind: "unknown" });
      assert.ok(flight.remediationRestrictions);
      if (!unavailable) {
        assert.ok(flight.remediationRestrictions.roots.includes(f.q));
        if (mode !== "large") assert.ok(flight.remediationRestrictions.roots.includes(f.d));
        assert.deepEqual(flight.remediationRestrictions.flaggedOIDs, [f.q]);
      }
      if (mode === "large") assert.equal(flight.remediationRestrictions.roots.length, 902);
      assert.equal(build.mock.callCount(), 0);
      assert.equal(update.mock.callCount(), 0);
      assert.equal(flight.checkpointFloor, floor);
      assert.deepEqual(f.snapshot(), before);

      // An old permissive frozen policy and C with excluded ancestors cannot escape.
      const record: any = { runId: f.claim.run_id, generation: 2, captureId: "fixture",
        branch: BRANCH, sourceSha: f.d, defaultBranch: "main",
        heldPolicy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: [] } };
      const heldFlight = f.fresh();
      try {
        assert.deepEqual(await heldFlight.subject.heldPublicationStep(f.claim, heldFlight.flight, record),
          { reason: "secret_remediation_pending" });
      } finally { heldFlight.runner.shutdown(); }

      // The actual frozen-inventory entry still invokes private archive capture.
      // The coordinator transport is observed here; this does not assert FINAL/ACK authority.
      let captures = 0;
      t.mock.method(subject.recovery, "captureAndUpload", async () => {
        captures++;
        assert.ok(flight.remediationRestrictions);
        return { state: "needs_action", reason: "fixture transport" };
      });
      await subject.captureFrozenInventory(f.claim, flight, record);
      assert.equal(captures, 1);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.ok(queries >= 2, "each entry consumes a fresh real query");
      assert.deepEqual(f.snapshot(), before);
      await assert.rejects(subject.requireLaunchAuthority(flight));
      assert.equal(build.mock.callCount(), 0);
    } finally { runner.shutdown(); }
  });
}

for (const unavailable of [false, true]) {
  test("private restore-point capture continues with negative bootstrap unavailable=" + unavailable, async t => {
    const f = await fixture();
    const { cache, runner, subject, flight } = f.fresh();
    const read = cache.readRemediationRestrictionFacts.bind(cache);
    t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof read>) =>
      unavailable ? read(args[0], args[1], args[2], undefined, { readback: async () => "different" }) : read(...args));
    let captureChecks = 0;
    // Isolate negative restoration from process ownership proof. This fixture is
    // not a planted-process or hosted quiescence acceptance test.
    t.mock.method(subject, "quiesceRun", async () => {
      captureChecks++;
      assert.ok(flight.remediationRestrictions, "denial is frozen before private capture");
      if (!unavailable) {
        assert.ok(flight.remediationRestrictions.roots.includes(f.q));
        assert.ok(flight.remediationRestrictions.roots.includes(f.d));
      }
      return { blocked: false };
    });
    t.mock.method(subject, "reproveDetailAfterRunnerGit", async () => undefined);
    const bridge = t.mock.method(cache, "bridgeToFloors");
    const update = t.mock.method(cache, "updateTrackingRef");
    const before = f.snapshot();
    try {
      const result = await subject.captureRecoveryRestorePointBody(f.claim, flight, nullLogger(),
        "recovery_capture", { credentialFree: true, publish: false });
      assert.equal(captureChecks, 1);
      assert.equal(result.verified, true, "private bytes remain recoverable");
      assert.equal(result.published, false);
      assert.equal(bridge.mock.callCount(), 0);
      assert.equal(update.mock.callCount(), 0);
      assert.equal(flight.checkpointFloor, f.d);
      assert.deepEqual(f.snapshot(), before, "no floor, pin, owed or hold changes from the query");
    } finally { runner.shutdown(); }
  });
}

for (const unavailable of [false, true]) {
  test("retained bootstrap restores current custody after real reservation before effects unavailable=" + unavailable, async t => {
    const f = await fixture();
    const { cache, runner, subject, flight } = f.fresh();
    const stop = new Error("fixture stops at source admission");
    const reserve = t.mock.method(cache, "reserveRecoveryIteration");
    const read = cache.readRemediationRestrictionFacts.bind(cache);
    const query = t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof read>) => {
      assert.equal(reserve.mock.callCount(), 1);
      assert.deepEqual(f.journal().remediationRecovery, original,
        "actual reservation preserves CURRENT remediation, absent from the stale parsed source");
      return unavailable ? read(args[0], args[1], args[2], undefined, { readback: async () => "different" }) : read(...args);
    });
    const original = f.journal().remediationRecovery;
    const admission = t.mock.method(subject, "admitPredecessorSource", async () => {
      assert.equal(query.mock.callCount(), 1);
      if (unavailable) {
        assert.ok(flight.remediationRestrictions.denialRevision > 0);
        assert.equal(flight.preserveRecoveryClone, true);
      } else {
        assert.ok(flight.remediationRestrictions.roots.includes(f.q));
        assert.ok(flight.remediationRestrictions.roots.includes(f.d));
      }
      flight.cancel.abort(stop);
      throw stop;
    });
    const capture = t.mock.method(subject, "runFreeCapture", async () => { throw new Error("must not capture"); });
    try {
      assert.equal(f.seedJournal.remediationRecovery, undefined);
      await assert.rejects(subject.recoverRetainedClaim(f.claim, flight, KEY, f.seedJournal));
      assert.equal(admission.mock.callCount(), 1);
      assert.equal(reserve.mock.callCount(), 1);
      assert.equal(query.mock.callCount(), 1);
      assert.equal(flight.cancel.signal.aborted, true, "all before-effect assertions reached the controlled stop");
      assert.equal(capture.mock.callCount(), 0);
      assert.deepEqual(f.journal().remediationRecovery, original);
    } finally { runner.shutdown(); }
  });
}

test("protected-only stale source refuses actual reservation before effects", async t => {
  const f = await fixture();
  await f.detach();
  const { cache, runner, subject, flight } = f.fresh();
  const reserve = t.mock.method(cache, "reserveRecoveryIteration");
  const admission = t.mock.method(subject, "admitPredecessorSource", async () => { throw new Error("must not admit"); });
  const capture = t.mock.method(subject, "runFreeCapture", async () => { throw new Error("must not capture"); });
  const before = f.snapshot();
  try {
    await assert.rejects(subject.recoverRetainedClaim(f.claim, flight, KEY, f.seedJournal));
    assert.equal(reserve.mock.callCount(), 1);
    assert.equal(admission.mock.callCount(), 0);
    assert.equal(capture.mock.callCount(), 0);
    assert.equal(flight.preserveRecoveryClone, true);
    assert.equal(flight.preserveSession, true);
    assert.deepEqual(f.snapshot(), before);
  } finally { runner.shutdown(); }
});

test("adoption rehold bootstraps current custody inside its bounded operation before quiescence", async t => {
  const f = await fixture();
  const iteration = await git.reserveRecoveryIteration(f.bare, BRANCH, KEY, f.source, 1, f.claim.claim_generation);
  await git.blockRecoveryEpisode(f.bare, BRANCH, KEY, f.source, "adoption_failed", iteration);
  client.protocolFeatures = ["claim_generation_fence", "recovery_cause_retained_recovery_adoption_failed"];
  const { cache, runner, subject, flight } = f.fresh();
  const original = f.journal().remediationRecovery;
  const stale = { ...f.seedJournal, recovery: f.journal().recovery };
  const read = cache.readRemediationRestrictionFacts.bind(cache);
  let boundedQuery = false;
  const query = t.mock.method(cache, "readRemediationRestrictionFacts", (...args: Parameters<typeof read>) => {
    const scope = (cache as any).recoveryOperations.getStore();
    boundedQuery = !!scope && Number.isFinite(scope.deadline) && !scope.signal.aborted;
    return read(...args);
  });
  const reserve = t.mock.method(cache, "reserveRecoveryIteration");
  const capture = t.mock.method(subject, "runFreeCapture", async () => {
    assert.equal(query.mock.callCount(), 1);
    assert.equal(reserve.mock.callCount(), 0, "a blocked rehold never enters the normal iteration");
    const scope = (cache as any).recoveryOperations.getStore();
    assert.ok(scope && Number.isFinite(scope.deadline) && !scope.signal.aborted);
    assert.ok(flight.remediationRestrictions.roots.includes(f.q));
    assert.ok(flight.remediationRestrictions.roots.includes(f.d));
    flight.cancel.abort(new Error("fixture stops before quiescence"));
    throw flight.cancel.signal.reason;
  });
  const quiesce = t.mock.method(subject, "quiesceRun", async () => { throw new Error("must not prove"); });
  try {
    await assert.rejects(subject.recoverRetainedClaim(f.claim, flight, KEY, stale));
    assert.equal(capture.mock.callCount(), 1);
    assert.equal(boundedQuery, true, "the rehold query itself inherits the existing operation");
    assert.equal(flight.cancel.signal.aborted, true, "custody assertions reached the controlled stop");
    assert.equal(quiesce.mock.callCount(), 0);
    assert.deepEqual(f.journal().remediationRecovery, original);
  } finally { runner.shutdown(); }
});

test("confirmed absence preserves ordinary ancestor and divergent bridge behavior", async t => {
  const f = await fixture(false);
  const { cache, runner, subject, flight } = f.fresh();
  const build = t.mock.method(cache, "bridgeToFloors");
  try {
    flight.publishedTip = f.base; flight.checkpointFloor = f.base;
    assert.deepEqual(await subject.bridgeBareTrackingRefIfDivergent(f.bare, BRANCH, flight, nullLogger()),
      { kind: "clean" });
    assert.equal(build.mock.callCount(), 0);
    assert.equal(flight.remediationRestrictions, undefined);
    await subject.requireLaunchAuthority(flight);
    flight.publishedTip = f.d; flight.checkpointFloor = f.d;
    const result = await subject.bridgeBareTrackingRefIfDivergent(f.bare, BRANCH, flight, nullLogger());
    assert.equal(result.kind, "bridged");
    assert.equal(build.mock.callCount(), 1);
    assert.equal(command(f.bare, "rev-parse", "refs/uzi-runner/" + BRANCH), result.bridge);
    assert.equal(flight.checkpointFloor, result.bridge);
    assert.equal(flight.remediationRestrictions, undefined);
  } finally { runner.shutdown(); }
});

test("strict current-body failure never falls back to custody authentication", async t => {
  const f = await fixture();
  const { cache, runner, subject, flight } = f.fresh();
  const order: string[] = [];
  const read = cache.readRemediationRestrictionFacts.bind(cache);
  t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof read>) => {
    order.push("negative"); return read(...args);
  });
  t.mock.method(cache, "readRuntimeRemediationRecovery", async () => {
    order.push("positive"); throw new Error("remediation source evidence unconfirmed");
  });
  try {
    await assert.rejects(subject.requireLaunchAuthority(flight), /unconfirmed/);
    assert.deepEqual(order, ["negative", "positive"]);
    assert.ok(flight.remediationRestrictions.roots.includes(f.d));
  } finally { runner.shutdown(); }
});

test("blocked protected custody refuses launch even when the active runtime proof is valid", async t => {
  const f = await fixture();
  const configPath = path.join(f.bare, "config");
  const liveConfig = fs.readFileSync(configPath, "utf8");
  await f.detach();
  const key = "uzi-retained." + f.claim.run_id + ".journal";
  const protectedCopy = JSON.parse(command(f.bare, "config", key));
  // Negative-only coexistence fixture: retain the original real live tuple and
  // receipt, and add the worker-produced descriptor with a blocked custody body.
  // No identity is rebound and no source grant is obtained from this copy.
  fs.writeFileSync(configPath, liveConfig);
  protectedCopy.journal.remediationRecovery.blocked = true;
  command(f.bare, "config", key, JSON.stringify(protectedCopy));
  const { cache, runner, subject, flight } = f.fresh();
  flight.publishedTip = f.base; flight.checkpointFloor = f.base;
  try {
    assert.ok(await cache.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source),
      "the independent current source proof is otherwise valid");
    assert.equal((await cache.readRemediationRestrictionFacts(f.bare, BRANCH, f.claim.run_id)).blocked, true);
    const strict = t.mock.method(cache, "readRuntimeRemediationRecovery");
    const scan = t.mock.method(cache, "secretScanCheckpointRange", async () => ({ trusted: true as const, findings: [] }));
    await assert.rejects(subject.requireLaunchAuthority(flight), /restriction facts unavailable/);
    assert.equal(strict.mock.callCount(), 0, "a current body cannot override blocked custody");
    assert.equal(scan.mock.callCount(), 0);
    assert.ok(flight.remediationRestrictions.roots.includes(f.q));
    assert.ok(flight.remediationRestrictions.roots.includes(f.d));
    assert.equal(flight.preserveRecoveryClone, true);
  } finally { runner.shutdown(); }
});

test("protected global findings cannot supply missing current positive metadata or pending Q", async t => {
  const f = await fixture();
  await f.detach();
  const protectedJournal = JSON.parse(command(f.bare, "config", "uzi-retained." + f.claim.run_id + ".journal")).journal;
  const active = structuredClone(protectedJournal);
  active.remediationRecovery.flaggedOIDs = [f.s];
  active.remediationRecovery.excludedRoots = [];
  active.remediationRecovery.edges = [];
  delete active.remediationRecovery.active;
  f.write(active); // Negative-only coexistence fixture; no current execution grant.
  const { cache, runner, subject, flight } = f.fresh();
  const strict = t.mock.method(cache, "readRuntimeRemediationRecovery");
  try {
    await assert.rejects(subject.requireLaunchAuthority(flight));
    assert.ok(flight.remediationRestrictions.flaggedOIDs.includes(f.q));
    assert.equal(strict.mock.callCount(), 1, "protected custody must still require live proof");
    // Even a readable current body cannot borrow Q from the protected copy.
    t.mock.method(cache, "readRuntimeRemediationRecovery", async () => active.remediationRecovery);
    flight.remediationRestrictions.pending = { ...f.source, barePath: f.bare, branch: BRANCH,
      requiredOIDs: [f.q], requireDenial: false, bindingUncertain: false };
    active.remediationRecovery.blocked = false;
    await assert.rejects(subject.requireLaunchAuthority(flight), /pending persistence unconfirmed/);
    delete flight.remediationRestrictions.pending;
    await assert.rejects(subject.requireLaunchAuthority(flight), /current restriction metadata unavailable/);
  } finally { runner.shutdown(); }
});

test("launch still scans full applicable history below a local floor", async t => {
  const f = await fixture();
  const historical = f.commit("new history finding");
  const s = f.commit("new source", historical); await f.fetch();
  await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: s, flaggedOIDs: [] });
  const { cache, runner, subject, flight } = f.fresh();
  flight.publishedTip = f.base; flight.checkpointFloor = historical;
  const scan = t.mock.method(cache, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
    assert.equal(range.tipSha, s);
    assert.ok(!range.scanFloorShas?.includes(historical), "a local floor cannot hide history");
    return { trusted: true as const, findings: [{ commit: historical } as any] };
  });
  try {
    await assert.rejects(subject.requireLaunchAuthority(flight), /continuation history unavailable/);
    assert.equal(scan.mock.callCount(), 1);
    assert.ok(flight.remediationRestrictions.roots.includes(historical));
  } finally { runner.shutdown(); }
});

for (const late of ["confirmed", "unavailable", "blocked", "reconfirmed", "initial-wait", "overflow"] as const) {
  test("launch observes negative custody changes across authority waits: " + late, async t => {
    const f = await fixture();
    await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: f.s, flaggedOIDs: [] });
    const { cache, runner, subject, flight } = f.fresh();
    flight.publishedTip = f.base; flight.checkpointFloor = f.base;
    flight.executor = {};
    if (late === "overflow") flight.remediationRestrictions = { roots: [], denialRevision: Number.MAX_SAFE_INTEGER };
    if (late === "initial-wait")
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: true });
    t.mock.method(cache, "secretScanCheckpointRange", async () => ({ trusted: true as const, findings: [] }));
    let observed = false;
    const observe = async () => {
      if (observed) return;
      observed = true;
      let restoreRead: (() => void) | undefined;
      if (late === "blocked") {
        const live = fs.readFileSync(path.join(f.bare, "config"), "utf8");
        await f.detach();
        const key = "uzi-retained." + f.claim.run_id + ".journal";
        const protectedCopy = JSON.parse(command(f.bare, "config", key));
        fs.writeFileSync(path.join(f.bare, "config"), live);
        protectedCopy.journal.remediationRecovery.blocked = true;
        command(f.bare, "config", key, JSON.stringify(protectedCopy));
        assert.equal((await cache.readRemediationRestrictionFacts(f.bare, BRANCH, f.claim.run_id)).blocked, true);
      } else if (late !== "confirmed") {
        const read = cache.readRemediationRestrictionFacts.bind(cache);
        const faulty = t.mock.method(cache, "readRemediationRestrictionFacts", (...args: Parameters<typeof read>) =>
          read(args[0], args[1], args[2], undefined, { readback: async () => "different" }));
        restoreRead = () => faulty.mock.restore();
      }
      assert.deepEqual(await subject.bridgeBareTrackingRefIfDivergent(f.bare, BRANCH, flight, nullLogger()), { kind: "unknown" });
      if (late !== "confirmed") {
        assert.equal(flight.preserveRecoveryClone, true);
        assert.ok(flight.remediationRestrictions.roots.includes(f.q));
        assert.ok(flight.remediationRestrictions.roots.includes(f.d));
        assert.ok(flight.remediationRestrictions.flaggedOIDs.includes(f.q));
      }
      if (late === "reconfirmed" || late === "initial-wait" || late === "overflow") {
        restoreRead!();
        assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: true });
      }
    };
    // This authority seam controls interleaving only; it does not prove process quiescence.
    subject.authority = {
      withTransaction: async (fn: () => Promise<unknown>) => {
        if (late === "initial-wait") await observe();
        return fn();
      },
      recordAdmissions: (_paths: Set<string>, fn: () => Promise<unknown>) => fn(),
      assertLaunchable: async () => { if (late !== "initial-wait") await observe(); },
    };
    try {
      if (late === "confirmed") await subject.requireLaunchAuthority(flight);
      else await assert.rejects(subject.requireLaunchAuthority(flight),
        { message: "remediation restriction confirmation changed" },
        "an unavailable read invalidates the admission already waiting");
      if (late === "reconfirmed") {
        assert.equal(flight.remediationRestrictions.denialRevision, 1);
        await subject.requireLaunchAuthority(flight);
        assert.equal(flight.remediationRestrictions.denialRevision, 1, "fresh confirmation never resets a denial");
      }
      if (late === "overflow") {
        assert.equal(flight.remediationRestrictions.denialRevision, -1);
        await assert.rejects(subject.requireLaunchAuthority(flight), { message: "remediation restriction confirmation changed" });
        assert.equal(flight.remediationRestrictions.denialRevision, -1, "overflow never wraps or clears");
      }
      assert.equal(observed, true);
    } finally { runner.shutdown(); }
  });
}

function latch() {
  let release!: () => void;
  const promise = new Promise<void>(resolve => { release = resolve; });
  return { promise, release };
}
const tick = () => new Promise<void>(resolve => setImmediate(resolve));

for (const stage of ["negative", "restore"] as const) {
  for (const stop of ["cancel", "expiry"] as const) {
    test("remediation lifecycle refuses queued launch proof: " + stage + "/" + stop, async t => {
      const f = await fixture();
      await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: f.s, flaggedOIDs: [] });
      const { cache, runner, subject, flight } = f.fresh();
      flight.executor = {};
      flight.publishedTip = f.base; flight.checkpointFloor = f.base;
      t.mock.method(cache, "secretScanCheckpointRange", async () => ({ trusted: true as const, findings: [] }));
      const entered = latch(), release = latch();
      let holder: Promise<void> | undefined, callbacks = 0, settled = false;
      let queryScope: any;
      let wall = Date.now(), expire: (() => void) | undefined;
      t.mock.method(Date, "now", () => wall);
      const realTimer = globalThis.setTimeout;
      t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number) => {
        if (delay === subject.codexBoundaryDeadlineMs && !expire) expire = callback;
        return realTimer(callback, delay);
      });
      // Bind before entering the operation so the holder and observer inherit no cancellation.
      const hold = AsyncLocalStorage.bind(() => cache.withBareLock(f.bare, async () => {
        entered.release(); await release.promise;
      }));
      const observe = AsyncLocalStorage.bind(() => cache.withBareLock(f.bare, async () => {}));
      const method = stage === "negative" ? "readRemediationRestrictionFacts" : "readRuntimeRemediationRecovery";
      const original = (cache[method] as (...args: any[]) => Promise<any>).bind(cache);
      let first = true;
      t.mock.method(cache, method, async (...args: any[]) => {
        if (first) {
          first = false;
          queryScope = (cache as any).recoveryOperations.getStore();
          holder = hold();
          await entered.promise;
        }
        return original(...args);
      });
      // Observe actual B callbacks, rather than counting a queued public method invocation.
      const lock = (cache as any).withLock.bind(cache);
      t.mock.method(cache as any, "withLock", (key: string, fn: () => Promise<unknown>, signal?: AbortSignal) =>
        lock(key, async () => { callbacks++; return fn(); }, signal));
      let admitted = false;
      const operation = subject.requireLaunchAuthority(flight)
        .then((check: () => void) => { check(); admitted = true; })
        .finally(() => { settled = true; });
      const outcome = operation.then(() => "accepted", () => "rejected");
      let before = 0;
      try {
        assert.equal(await Promise.race([entered.promise.then(() => true), outcome.then(() => false)]), true,
          "the proof reaches the real query before settling");
        await tick();
        assert.equal(Number.isFinite(queryScope?.deadline), true, "query has a finite operation allowance");
        assert.equal(queryScope?.signal instanceof AbortSignal, true, "query inherits its operation signal");
        assert.equal(settled, false, "the real query is queued behind the independent holder");
        before = callbacks;
        if (stop === "cancel") flight.cancel.abort(new Error("controlled cancellation"));
        else {
          assert.ok(expire, "ordinary private operation timer was installed");
          wall += subject.codexBoundaryDeadlineMs;
          expire();
        }
        assert.equal(queryScope.signal.aborted, true, "the live proof observes its lifecycle stop");
        assert.equal(await outcome, "rejected", "proof rejects while the independent holder remains held");
        assert.equal(admitted, false);
      } finally {
        release.release();
        await Promise.allSettled([operation, ...(holder ? [holder] : [])]);
        runner.shutdown();
      }
      await observe();
      assert.equal(callbacks, before + 1, "only the independent observer runs after B releases");
      assert.equal(admitted, false, "no late launch admission");
    });
  }
}

for (const stop of ["cancel", "expiry"] as const) {
  test("remediation lifecycle joins acquired validation child: " + stop, async t => {
    const f = await fixture();
    await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: f.s, flaggedOIDs: [] });
    const { cache, runner, subject, flight } = f.fresh();
    flight.executor = {};
    flight.publishedTip = f.base; flight.checkpointFloor = f.base;
    let validationStarted = false, earlierAncestry = 0, controlled = false, cancelled = false, settled = false;
    let validationScope: any;
    t.mock.method(cache, "secretScanCheckpointRange", async () => {
      validationStarted = true;
      return { trusted: true as const, findings: [] };
    });
    let wall = Date.now(), expire: (() => void) | undefined;
    t.mock.method(Date, "now", () => wall);
    const realTimer = globalThis.setTimeout;
    t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number) => {
      if (delay === subject.codexBoundaryDeadlineMs && !expire) expire = callback;
      return realTimer(callback, delay);
    });
    const spawned = latch(), closed = latch();
    const ancestry = cache.ancestry.bind(cache);
    t.mock.method(cache, "ancestry", (...args: Parameters<typeof ancestry>) => {
      if (!validationStarted) { earlierAncestry++; return ancestry(...args); }
      if (controlled) return ancestry(...args);
      controlled = true;
      validationScope = (cache as any).recoveryOperations.getStore();
      const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
      return cache.withBoundaryProcessSpawner(async () => {
        spawned.release();
        // Pipe EOF and cancellation acknowledgement are not terminal process close.
        stdout.end(); stderr.end();
        return { stdin, stdout, stderr, completed: closed.promise.then(() => ({ code: 1 })),
          cancel: async () => { cancelled = true; } };
      }, new AbortController().signal, () => ancestry(...args));
    });
    const operation = subject.requireLaunchAuthority(flight).finally(() => { settled = true; });
    const outcome = operation.then(() => "accepted", () => "rejected");
    try {
      assert.equal(await Promise.race([spawned.promise.then(() => true), outcome.then(() => false)]), true,
        "validation reaches its acquired child before settling");
      assert.equal(Number.isFinite(validationScope?.deadline), true, "validation has a finite operation allowance");
      assert.equal(validationScope?.signal instanceof AbortSignal, true, "validation inherits the live operation signal");
      assert.ok(earlierAncestry > 0, "strict graph validation ran untouched before the scan");
      if (stop === "cancel") flight.cancel.abort(new Error("validation cancelled"));
      else {
        assert.ok(expire);
        wall += subject.codexBoundaryDeadlineMs;
        expire();
      }
      await tick();
      assert.equal(validationScope.signal.aborted, true);
      assert.equal(cancelled, true);
      assert.equal(settled, false, "validation retains its operation until the child completes");
    } finally {
      closed.release();
      await outcome;
      runner.shutdown();
    }
    assert.equal(await outcome, "rejected");
    assert.equal(controlled, true);
    assert.equal(settled, true);
  });
}

for (const stage of ["negative", "restore"] as const) {
  test("actual phaseClone rejects queued remediation proof before models: " + stage, async t => {
    const claim = gitlabClaim(stage === "negative" ? 3748 : 3749, { claim_generation: 2 });
    api.setOwnershipStatus(claim.run_id, "running", 2);
    let models = 0;
    const runner = new RunRunner(client, git, () => ({ homeDir, executor: {
      run: async () => { models++; throw new Error("unexpected model"); },
    } }), nullLogger(), 20, undefined, {
      gitlab: fakeGitlab().gitlab, teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1,
    });
    const subject = runner as any;
    const entered = latch(), release = latch(), groupRejected = latch();
    let flight: any, holder: Promise<void> | undefined, inPhase = false, outerStarted = false;
    let outerSignal: AbortSignal | undefined;
    const hold = AsyncLocalStorage.bind((bare: string) => git.withBareLock(bare, async () => {
      entered.release(); await release.promise;
    }));
    const phaseClone = subject.phaseClone.bind(subject);
    t.mock.method(subject, "phaseClone", async (input: unknown, current: any) => {
      flight = current; inPhase = true;
      try { return await phaseClone(input, current); }
      finally { inPhase = false; }
    });
    const lifecycle = subject.withRemediationLifecycle.bind(subject);
    t.mock.method(subject, "withRemediationLifecycle", async (signal: AbortSignal | undefined, action: () => Promise<unknown>) => {
      const outer = inPhase && !outerStarted;
      if (outer) { outerStarted = true; outerSignal = signal; }
      try { return await lifecycle(signal, action); }
      catch (error) { if (outer) groupRejected.release(); throw error; }
    });
    const method = stage === "negative" ? "readRemediationRestrictionFacts" : "readRuntimeRemediationRecovery";
    const query = (git[method] as (...args: any[]) => Promise<any>).bind(git);
    let queued = false;
    t.mock.method(git, method, async (...args: any[]) => {
      if (inPhase && outerStarted && !queued) {
        queued = true;
        assert.ok((git as any).recoveryOperations.getStore());
        holder = hold(args[0]);
        await entered.promise;
      }
      return query(...args);
    });
    const executing = runner.execute(claim);
    try {
      assert.equal(await Promise.race([entered.promise.then(() => true), executing.then(() => false)]), true,
        "the actual clone phase reaches its metadata proof before settling");
      assert.equal(flight.branch, stage === "negative" ? "agent/issue-3748" : "agent/issue-3749");
      assert.equal(outerSignal instanceof AbortSignal, true, "clone proof supplies its live lifecycle signal");
      flight.cancel.abort(new Error("phase clone cancelled"));
      assert.equal(outerSignal?.aborted, true, "clone proof stops with the flight before cleanup");
      await groupRejected.promise;
      assert.equal(models, 0, "real clone proof rejects while the independent B holder remains held");
    } finally {
      release.release();
      await holder;
      await executing;
      runner.shutdown();
    }
    assert.equal(queued, true);
    assert.equal(models, 0);
  });
}

test("remediation lifecycle clock starts after A acquisition", async t => {
  const f = await fixture(false);
  const { cache, runner, subject, flight } = f.fresh();
  flight.executor = {};
  let wall = 1000, waitMs = 0, acquiredAt = 0;
  t.mock.method(Date, "now", () => wall);
  subject.authority = {
    withTransaction: async (fn: () => Promise<unknown>, wait: { timeoutMs: number }) => {
      waitMs = wait.timeoutMs;
      wall += 60_000; // A waited longer than the existing 30s operation allowance.
      acquiredAt = wall;
      return fn();
    },
    recordAdmissions: (_paths: Set<string>, fn: () => Promise<unknown>) => fn(),
    assertLaunchable: async () => {},
  };
  const query = cache.readRemediationRestrictionFacts.bind(cache);
  t.mock.method(cache, "readRemediationRestrictionFacts", (...args: Parameters<typeof query>) => {
    const scope = (cache as any).recoveryOperations.getStore();
    assert.equal(scope.deadline, acquiredAt + subject.codexBoundaryDeadlineMs);
    return query(...args);
  });
  try {
    await subject.requireLaunchAuthority(flight);
    assert.equal(waitMs, 300_000, "A keeps its five-minute acquisition contract");
  } finally { runner.shutdown(); }
});

test("default live remediation query remains unavailable after global shutdown", async t => {
  const f = await fixture(false);
  const { cache, runner, subject, flight } = f.fresh();
  const query = t.mock.method(cache, "readRemediationRestrictionFacts");
  runner.shutdown();
  assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
  assert.equal(query.mock.callCount(), 0);
  assert.equal(flight.preserveRecoveryClone, true);
});

for (const mode of ["healthy", "revoked", "expired", "existing cap"] as const) {
  test("shutdown bridge uses the exact workerShutdown handoff: " + mode, async t => {
    const f = await fixture(false);
    const { cache, runner, subject, flight } = f.fresh();
    let wall = Date.now();
    t.mock.method(Date, "now", () => wall);
    const active: any = { steering: flight.steering };
    subject.activeRuns.set(flight.runId, active);
    // The production shutdown path creates the frozen handoff before stopping global live work.
    runner.shutdown();
    const cause = active.gracefulShutdownCheckpoint;
    assert.equal(cause, flight.steering.workerShutdownCheckpoint());
    assert.equal(Object.isFrozen(cause), true);
    assert.equal(subject.shutdownSignal.signal.aborted, true);
    assert.equal(flight.cancel.signal.aborted, true);
    assert.equal(cause.stopSignal.aborted, false);
    const deadlineAt = mode === "existing cap" ? wall + 2 * subject.codexBoundaryDeadlineMs : cause.deadlineAt;
    if (mode === "revoked") flight.steering.abortLifecycle(new Error("cleanup revoked"));
    if (mode === "expired") wall = cause.deadlineAt;
    const read = cache.readRemediationRestrictionFacts.bind(cache);
    const query = t.mock.method(cache, "readRemediationRestrictionFacts", (...args: Parameters<typeof read>) => {
      const scope = (cache as any).recoveryOperations.getStore();
      assert.equal(scope.signal.aborted, false);
      assert.equal(scope.deadline, Math.min(wall + subject.codexBoundaryDeadlineMs, deadlineAt));
      return read(...args);
    });
    // No published floor needs a bridge, but the real custody query and ownership proof still run.
    flight.publishedTip = undefined;
    try {
      const result = await subject.bridgeParkSinkBestEffort(f.bare, BRANCH, flight, nullLogger(),
        "shutdown", cause.stopSignal, deadlineAt);
      if (mode === "revoked" || mode === "expired") {
        assert.deepEqual(result, { kind: "unknown" });
        assert.equal(query.mock.callCount(), 0);
        assert.equal(flight.preserveRecoveryClone, true);
      } else {
        assert.deepEqual(result, { kind: "clean" });
        assert.equal(query.mock.callCount(), 1);
        assert.equal(flight.remediationRestrictions, undefined);
      }
    } finally {
      subject.activeRuns.delete(flight.runId);
      runner.shutdown();
    }
  });
}

test("negative post-action expiry latches denial and independent private capture retains bytes", async t => {
  const f = await fixture();
  const { cache, runner, subject, flight } = f.fresh();
  let wall = Date.now(), first = true;
  t.mock.method(Date, "now", () => wall);
  const before = f.snapshot();
  const query = cache.readRemediationRestrictionFacts.bind(cache);
  t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof query>) => {
    const result = await query(...args);
    if (first) { first = false; wall += subject.codexBoundaryDeadlineMs; }
    return result;
  });
  t.mock.method(subject, "quiesceRun", async () => ({ blocked: false }));
  t.mock.method(subject, "reproveDetailAfterRunnerGit", async () => undefined);
  try {
    assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
    assert.equal(flight.remediationRestrictions.denialRevision, 1);
    assert.equal(flight.preserveRecoveryClone, true);
    const result = await subject.captureRecoveryRestorePointBody(f.claim, flight, nullLogger(),
      "recovery_capture", { credentialFree: true, publish: false });
    assert.equal(result.verified, true);
    assert.equal(result.published, false);
    assert.deepEqual(f.snapshot(), before);
  } finally { runner.shutdown(); }
});

test("negative bootstrap inherits a shorter parent deadline without renewing it", async t => {
  const f = await fixture();
  const { cache, runner, subject, flight } = f.fresh();
  let wall = Date.now();
  t.mock.method(Date, "now", () => wall);
  const deadline = wall + 10;
  const query = cache.readRemediationRestrictionFacts.bind(cache);
  t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof query>) => {
    assert.equal((cache as any).recoveryOperations.getStore().deadline, deadline);
    const facts = await query(...args);
    wall = deadline;
    return facts;
  });
  try {
    await assert.rejects(cache.withRecoveryOperation(new AbortController().signal, deadline, async () => {
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
    }), /recovery deadline/);
    assert.equal(flight.remediationRestrictions.denialRevision, 1);
    assert.equal(flight.preserveRecoveryClone, true);
  } finally { runner.shutdown(); }
});

for (const seam of ["capture", "frozen", "prepare"] as const) {
  test("negative bootstrap consumes explicit caller cancellation at " + seam, async t => {
    const f = await fixture();
    const { cache, runner, subject, flight } = f.fresh();
    const caller = new AbortController();
    caller.abort(new Error("caller stopped"));
    const query = t.mock.method(cache, "readRemediationRestrictionFacts");
    const record: any = { branch: BRANCH, defaultBranch: "main" };
    t.mock.method(subject, "quiesceRun", async () => ({ blocked: true, outcome: { process: { detail: "fixture blocked" } } }));
    t.mock.method(subject.recovery, "captureAndUpload", async () => ({ state: "needs_action" }));
    try {
      if (seam === "capture")
        await subject.captureRecoveryRestorePointBody(f.claim, flight, nullLogger(), "recovery_capture",
          { credentialFree: true, publish: false, signal: caller.signal });
      else if (seam === "frozen") await subject.captureFrozenInventory(f.claim, flight, record, caller.signal);
      else assert.deepEqual(await subject.heldPublicationStep(f.claim, flight, record, caller.signal),
        { reason: "secret_remediation_pending" });
      assert.equal(query.mock.callCount(), 0, "scope entry refuses the earlier query");
      assert.equal(flight.remediationRestrictions.denialRevision, 1);
      assert.equal(flight.preserveRecoveryClone, true);
    } finally { runner.shutdown(); }
  });
}

// Native lifecycle fake: production spawn/abort/escalation/join code runs, but no OS
// process is planted. Terminal close is controlled independently of cancellation.
for (const native of [false, true]) for (const stop of ["caller", "late attempt"] as const) {
  test("actual Runner negative query joins " + (native ? "metered native" : "supervised") + " child and retains A/B: " + stop, async t => {
    const f = await fixture();
    const { cache, runner, subject, flight } = f.fresh();
    const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
    const authority = new AuthorityRuntime({ lifetime, workerToken: "fixture-worker", timeoutMs: 5000 });
    subject.authority = authority;
    const spawned = latch();
    const kills: string[] = [];
    let escalate!: () => void, expire!: () => void, now = 0, settled = false;
    let budget: MaintenanceBudget | undefined;
    const realTimer = globalThis.setTimeout;
    t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number) => {
      if (delay === 1000) escalate = callback;
      return realTimer(callback, delay);
    });
    const child = Object.assign(new EventEmitter(), {
      stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
      exitCode: null, signalCode: null,
      kill: (signal?: string) => { kills.push(signal ?? "SIGTERM"); return true; },
    });
    let complete!: (result: { code: number }) => void;
    const completed = new Promise<{ code: number }>(resolve => { complete = resolve; });
    let runNative = <T>(action: () => T): T => action();
    if (native) {
      const scratch = path.resolve("../.uzi/scratch");
      fs.mkdirSync(scratch, { recursive: true });
      const dir = fs.mkdtempSync(path.join(scratch, "runner-lifecycle-"));
      let finish = async () => {};
      t.after(async () => {
        try { await finish(); }
        finally { fs.rmSync(dir, { recursive: true, force: true }); }
      });
      const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
      const admitted = await ledger.admit({
        identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
          bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
        config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
      });
      assert.ok(admitted.ok, JSON.stringify(admitted));
      runNative = action => MaintenanceResources.run(admitted.value, action);
      finish = async () => { await ledger.finish(admitted.value, "transient"); };
      const spawn = childProcess.spawn;
      t.mock.method(childProcess, "spawn", (...args: Parameters<typeof spawn>) => {
        if (args[0] === "git" || args[0] === "/usr/bin/git") { spawned.release(); return child; }
        return spawn(...args);
      });
      syncBuiltinESMExports();
      t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
    }
    const caller = new AbortController();
    const operation = MaintenanceBudget.runAttempt("runner-query", () => budget = new MaintenanceBudget(100, () => now), () => {
      expire = AsyncLocalStorage.bind(() => {
        MaintenanceBudget.observeCancellation(); now = 100; assert.throws(() => budget!.check());
      });
      const action = () => subject.inFlightAuthority(flight, () => subject.bootstrapRemediationRestrictions(
        flight, undefined, undefined, stop === "caller" ? caller.signal : undefined));
      return native ? runNative(action) : cache.withBoundaryProcessSpawner(async () => {
        spawned.release();
        return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
          cancel: async () => { kills.push("cancel"); } };
      }, new AbortController().signal, action);
    });
    const outcome = operation.then(
      (value: unknown) => { settled = true; return { kind: "value" as const, value }; },
      (error: unknown) => { settled = true; return { kind: "error" as const, error }; });
    let siblingA = false, siblingB = false;
    let a: Promise<unknown> | undefined, b: Promise<unknown> | undefined;
    try {
      // Controller-only observation; finally always joins the owned operation.
      assert.equal(await Promise.race([spawned.promise.then(() => true), outcome.then(() => false)]), true,
        "the actual query acquires its child before settling");
      const currentBudget = () => budget;
      assert.equal(currentBudget(), undefined, "attempt relay starts without an active timed budget");
      // Created outside the aborting attempt's async context.
      a = authority.withTransaction(async () => { siblingA = true; });
      b = cache.withBareLock(f.bare, async () => { siblingB = true; });
      // Drain pipes first: only terminal-close joining can retain the locks.
      child.stdout.end(); child.stderr.end();
      await tick();
      assert.equal(settled, false, "pipe EOF is not terminal process close");
      assert.equal(siblingA, false); assert.equal(siblingB, false);
      if (stop === "caller") caller.abort(new Error("caller cancelled"));
      else expire();
      await tick();
      assert.deepEqual(kills, native ? ["SIGTERM"] : ["cancel"]);
      assert.equal(settled, false);
      assert.equal(siblingA, false); assert.equal(siblingB, false);
      if (native) {
        escalate();
        await tick();
        assert.deepEqual(kills, ["SIGTERM", "SIGKILL"]);
      }
      assert.equal(settled, false, "a kill request is not terminal close");
      assert.equal(siblingA, false); assert.equal(siblingB, false);
    } finally {
      child.stdout.end(); child.stderr.end(); child.emit("close", null); complete({ code: 1 });
      await Promise.all([outcome, ...(a ? [a] : []), ...(b ? [b] : [])]);
      budget?.dispose(); runner.shutdown();
    }
    const result = await outcome;
    assert.equal(result.kind, "value");
    assert.deepEqual(result.kind === "value" ? result.value : undefined, { kind: "unavailable" });
    assert.equal(flight.remediationRestrictions.denialRevision, 1);
    assert.equal(siblingA, true); assert.equal(siblingB, true);
  });
}
