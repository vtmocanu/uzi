import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { GitCache, type CheckpointRange } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { SteeringChannel } from "../src/steering.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";

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

for (const protectedOnly of [false, true]) {
  test("retained entry restores custody before settlement/reservation protected=" + protectedOnly, async t => {
    const f = await fixture();
    if (protectedOnly) await f.detach();
    const { cache, runner, subject, flight } = f.fresh();
    const stop = new Error("fixture stops before reservation");
    const reserve = t.mock.method(cache, "reserveRecoveryIteration");
    const read = t.mock.method(cache, "readRemediationRestrictionFacts");
    t.mock.method(subject, "logPredecessorMemorySettlement", async () => {
      assert.equal(read.mock.callCount(), 1);
      assert.ok(flight.remediationRestrictions.roots.includes(f.q));
      assert.ok(flight.remediationRestrictions.roots.includes(f.d));
      throw stop;
    });
    const before = f.snapshot();
    try {
      // A genuinely earlier parsed journal contains no remediation state. Current
      // active/protected artifacts, rather than this old process view, supply it.
      assert.equal(f.seedJournal.remediationRecovery, undefined);
      await assert.rejects(subject.recoverRetainedClaim(f.claim, flight, KEY, f.seedJournal),
        error => error === stop);
      assert.equal(reserve.mock.callCount(), 0);
      assert.deepEqual(f.snapshot(), before);
    } finally { runner.shutdown(); }
  });
}

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
