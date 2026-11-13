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
import { acquireWorkerLifetime, activeAuthorityTransaction } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { testLockOptions } from "./authority-test-support.js";
import { test } from "node:test";
import { GitCache, type CheckpointRange } from "../src/git.js";
import { FlightRetention, RunRunner, StaleClaimError } from "../src/runner.js";
import { SteeringChannel } from "../src/steering.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { legacyProtectedDetach } from "./legacy-protected-detach.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runner as runnerFor } from "./runner-harness.js";
import { StubExecutor } from "../src/executor.js";
import { CredentialSwitchSignal, PauseNowSignal } from "../src/steering.js";

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
    await legacyProtectedDetach(git, bare, BRANCH, KEY, journal());
  };
  const fresh = (authority?: AuthorityRuntime) => {
    const cache = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ gitleaksBin: (git as any).gitleaksBin,
        ...(authority ? { authority: authority.hooks, authorityRequired: true } : {}),
      }));
    const runner = new RunRunner(client, cache, () => ({ homeDir, executor: {
      run: async () => { throw new Error("unused executor"); },
    } }), nullLogger(), 20, undefined, {
      gitlab: fakeGitlab().gitlab, teardownTestDeps: portableTeardownTestDeps, recoveryRetryMs: 1, authority,
    });
    const cancel = new AbortController();
    const steering = new SteeringChannel(client, claim.run_id, 5, nullLogger(), cancel, { claimGeneration: 2 });
    // This is a fresh flight: no prior process-local remediation knowledge.
    const retention = new FlightRetention();
    const flight: any = {
      retention,
      get keepGuardedInventoryOpen(): boolean { return retention.guardedInventoryOpen.value; },
      set keepGuardedInventoryOpen(on: boolean) { retention.guardedInventoryOpen.setFlight(on); },
      get preserveSession(): boolean { return retention.session.value; },
      set preserveSession(on: boolean) { retention.session.setFlight(on); },
      get preserveRecoveryClone(): boolean { return retention.recoveryClone.value; },
      set preserveRecoveryClone(on: boolean) { retention.recoveryClone.setFlight(on); },
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

for (const mode of ["finding", "denial"] as const) {
  test("#4191 clearable revalidation refuses publication after " + mode, async t => {
    const f = await fixture(false);
    const { cache, runner, subject, flight } = f.fresh();
    try {
      await timeout4191(f, cache, subject, flight);
      const revision = flight.remediationDenialRevision;
      const read = cache.readRemediationRestrictionFacts.bind(cache);
      let injected = false;
      t.mock.method(cache, "recordRemediationScan", async () => { throw new Error("write failure"); });
      t.mock.method(cache, "readRemediationRestrictionFacts", async (...args: Parameters<typeof read>) => {
        const facts = await read(...args);
        if (!injected) {
          injected = true;
          await assert.rejects(subject.persistRemediationScan(flight, f.bare, BRANCH, f.s,
            mode === "finding" ? [{ commit: f.s }] : [{ commit: f.s }, {}], "pre-exit"));
        }
        return facts;
      });
      flight.publishedTip = f.base; flight.checkpointFloor = f.base;
      await assert.rejects(subject.pushRemediationCandidate(flight, f.bare, BRANCH, "fixture", fx.originPath, undefined,
        { candidate: f.s, originalHead: f.s, floor: { kind: "absent" } }));
      assert.equal(injected, true);
      assert.ok(flight.remediationRestrictions.pending.requiredOIDs.includes(f.s));
      assert.equal(subject.frozenHeldPolicy(flight).blocked, true);
      assert.ok(flight.remediationDenialRevision >= revision);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(command(f.clone.path, "rev-parse", "HEAD"), f.s);
      assert.throws(() => command(fx.originPath, "rev-parse", "refs/heads/" + BRANCH));
    } finally { runner.shutdown(); }
  });
}

for (const mode of ["finding", "denial", "binding"] as const) {
  test("#4191 failed persistence then clean read retains pending: " + mode, async t => {
    const f = await fixture(false);
    const { cache, runner, subject, flight } = f.fresh();
    const failure = t.mock.method(cache, "recordRemediationScan", async () => {
      assert.deepEqual(flight.remediationRestrictions.pending.requiredOIDs, [f.s]);
      throw new Error("write failure");
    });
    try {
      await assert.rejects(subject.persistRemediationScan(flight, f.bare, BRANCH, f.s, [{ commit: f.s }], "pre-exit"));
      assert.equal(failure.mock.callCount(), 1, "the real finding reached the failed persistence write");
      if (mode === "denial")
        await assert.rejects(subject.persistRemediationScan(flight, f.bare, BRANCH, f.s, [{ commit: f.s }, {}], "pre-exit"));
      if (mode === "binding") {
        flight.runnerClone = { ...f.clone, attemptId: "another-attempt" };
        await assert.rejects(subject.persistRemediationScan(flight, f.bare, BRANCH, f.s, [{ commit: f.s }], "pre-exit"));
        flight.runnerClone = f.clone;
      }
      failure.mock.restore();
      const pending = flight.remediationRestrictions.pending;
      assert.deepEqual(pending.requiredOIDs, [f.s]);
      if (mode === "denial") assert.equal(pending.requireDenial, true);
      if (mode === "binding") assert.equal(pending.bindingUncertain, true);
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      assert.equal(flight.remediationRestrictions.pending, pending);
      await assert.rejects(subject.requireLaunchAuthority(flight), /enrollment disappeared|pending persistence/);
    } finally { runner.shutdown(); }
  });
}

for (const poison of [false, true]) {
  test("#4191 captured admission rejects after real timeout clears poison=" + poison, async () => {
    const f = await fixture(false);
    const { cache, runner, subject, flight } = f.fresh();
    try {
      const admission = await subject.requireLaunchAuthority(flight);
      if (poison) flight.remediationDenialRevision = Number.MAX_SAFE_INTEGER;
      await timeout4191(f, cache, subject, flight);
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      assert.equal(flight.remediationRestrictions, undefined);
      assert.throws(admission, /confirmation changed/);
      if (poison) {
        assert.equal(flight.remediationDenialRevision, -1);
        await assert.rejects(subject.requireLaunchAuthority(flight), /confirmation changed/);
        assert.equal(flight.remediationDenialRevision, -1);
      } else await subject.requireLaunchAuthority(flight);
    } finally { runner.shutdown(); }
  });
}

for (const protectedOnly of [false, true]) {
  test("#4191 enrollment observed before a failed read stays sticky protected=" + protectedOnly, async t => {
    const f = await fixture();
    if (protectedOnly) await f.detach();
    const { cache, runner, subject, flight } = f.fresh();
    const messages: unknown[][] = [];
    t.mock.method(subject.log, "warn", (...args: unknown[]) => { messages.push(args); });
    const failure = t.mock.method(cache as any, "syncJournalAndReadBack", async () => {
      throw new Error("private-post-enrollment-sentinel");
    });
    try {
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
      assert.ok(flight.remediationRestrictions.flaggedOIDs.includes(f.q),
        "partial-read enrollment contributes negative knowledge before the later failure");
      assert.deepEqual(messages.at(-1), ["remediation restriction read unavailable", { class: "enrolled_observed" }]);
      failure.mock.restore();
      f.write(f.seedJournal);
      if (protectedOnly) command(f.bare, "config", "--unset", "uzi-retained." + f.claim.run_id + ".journal");
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      await assert.rejects(subject.restoreRemediationRestrictions(flight), /enrollment disappeared/);
      const diagnostics = JSON.stringify(messages);
      for (const secret of [f.bare, f.q, "private-post-enrollment-sentinel"])
        assert.equal(diagnostics.includes(secret), false);
    } finally { runner.shutdown(); }
  });
}

async function timeout4191(f: Awaited<ReturnType<typeof fixture>>, cache: GitCache, subject: any, flight: any) {
  const entered = latch(), release = latch();
  const hold = AsyncLocalStorage.bind(() => cache.withBareLock(f.bare, async () => {
    entered.release(); await release.promise;
  }));
  const holder = hold(); await entered.promise;
  subject.codexBoundaryDeadlineMs = 30;
  try {
    assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
    assert.equal(flight.remediationRestrictions.clearable, true);
  } finally { release.release(); await holder; subject.codexBoundaryDeadlineMs = 5000; }
}

for (const mode of ["zero blocked", "ambiguous", "malformed", "noncanonical", "identity", "overflow", "restore identity", "unknown"] as const) {
  test("#4191 actual cause remains sticky after clean read: " + mode, async t => {
    const f = await fixture(mode === "zero blocked" || mode === "noncanonical" || mode === "identity" || mode === "overflow");
    if (mode === "overflow") await f.detach();
    const { cache, runner, subject, flight } = f.fresh();
    const messages: unknown[][] = [];
    t.mock.method(subject.log, "warn", (...args: unknown[]) => { messages.push(args); });
    try {
      await timeout4191(f, cache, subject, flight);
      if (mode === "zero blocked") {
        const j = f.journal();
        j.remediationRecovery.flaggedOIDs = []; j.remediationRecovery.edges = [];
        j.remediationRecovery.blocked = true; delete j.remediationRecovery.active; f.write(j);
        assert.equal(f.journal().remediationRecovery.flaggedOIDs.length, 0);
      }
      if (mode === "ambiguous") command(f.bare, "config", "--add", ACTIVE, JSON.stringify(f.seedJournal));
      if (mode === "malformed") command(f.bare, "config", ACTIVE, "{private-journal-sentinel");
      if (mode === "noncanonical") command(f.bare, "config", ACTIVE, JSON.stringify(f.journal(), null, 2));
      if (mode === "identity") { const j = f.journal(); j.remediationRecovery.branch = "agent/other"; f.write(j); }
      if (mode === "overflow") {
        const j = JSON.parse(command(f.bare, "config", "uzi-retained." + f.claim.run_id + ".journal")).journal;
        j.remediationRecovery.flaggedOIDs = Array.from({ length: 64 }, (_, i) => (i + 1).toString(16).padStart(40, "0"));
        j.remediationRecovery.edges = []; delete j.remediationRecovery.active; f.write(j);
      }
      if (mode === "restore identity") {
        flight.barePath = undefined; flight.branch = undefined; flight.worktreePath = undefined;
        await assert.rejects(subject.restoreRemediationRestrictions(flight), /source identity unavailable/);
        flight.barePath = f.bare; flight.branch = BRANCH; flight.worktreePath = f.clone.path;
      } else {
        const injected = mode === "unknown" ? t.mock.method(cache as any, "readRecoverySnapshot", async () => {
          throw new Error("private-error-sentinel");
        }) : undefined;
        assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
        injected?.mock.restore();
        const expected = mode === "zero blocked" ? "blocked" : mode === "identity" ? "identity" : mode === "unknown" ? "unknown" : "integrity";
        assert.deepEqual(messages.at(-1), ["remediation restriction read unavailable", { class: expected }]);
      }
      command(f.bare, "config", "--replace-all", ACTIVE, JSON.stringify(f.seedJournal));
      if (mode === "overflow") command(f.bare, "config", "--unset", "uzi-retained." + f.claim.run_id + ".journal");
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      assert.equal(subject.frozenHeldPolicy(flight).blocked, true);
      assert.equal(flight.remediationRestrictions.clearable, false);
      const allowed = new Set(["pre_read_unavailable", "enrolled_observed", "blocked", "integrity", "identity", "changed_facts", "post_read_expiry", "unknown"]);
      for (const message of messages) {
        assert.equal(message.length, 2);
        assert.equal(message[0], "remediation restriction read unavailable");
        assert.ok(allowed.has((message[1] as { class: string }).class));
        assert.deepEqual(Object.keys(message[1] as object), ["class"]);
      }
      const diagnostics = JSON.stringify(messages);
      for (const secret of [f.bare, f.clone.path, f.q, "private-error-sentinel", "private-journal-sentinel", JSON.stringify(f.seedJournal)])
        assert.equal(diagnostics.includes(secret), false);
    } finally { runner.shutdown(); }
  });
}

for (const mode of ["branch", "bare", "run"] as const) {
  test("#4191 wrong confirmed identity cannot clear: " + mode, async t => {
    const f = await fixture(false);
    const { cache, runner, subject, flight } = f.fresh();
    try {
      await timeout4191(f, cache, subject, flight);
      if (mode === "run") {
        const read = (cache as any).readRecoverySnapshot.bind(cache);
        const entered = latch(), release = latch();
        const injected = t.mock.method(cache as any, "readRecoverySnapshot", async (...args: unknown[]) => {
          entered.release(); await release.promise; return read(...args);
        });
        const reading = subject.bootstrapRemediationRestrictions(flight);
        try { await entered.promise; flight.runId = "another-valid-run"; }
        finally { release.release(); }
        assert.deepEqual(await reading, { kind: "confirmed", enrolled: false });
        injected.mock.restore(); flight.runId = f.claim.run_id;
      } else {
        const otherBare = mode === "bare" ? path.join(path.dirname(f.bare), "other-confirmed.git") : f.bare;
        if (mode === "bare") command(f.bare, "init", "--bare", otherBare);
        assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight, otherBare,
          mode === "branch" ? "agent/other" : BRANCH), { kind: "confirmed", enrolled: false });
      }
      assert.ok(flight.remediationRestrictions);
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      assert.equal(flight.remediationRestrictions, undefined);
    } finally { runner.shutdown(); }
  });
}

test("#4191 deadline after B before facts stays sticky", async t => {
  const f = await fixture(false);
  const { cache, runner, subject, flight } = f.fresh();
  let wall = Date.now(); t.mock.method(Date, "now", () => wall);
  let readStarted: boolean | undefined, expired = false;
  const read = (cache as any).readRecoverySnapshot.bind(cache);
  const injected = t.mock.method(cache as any, "readRecoverySnapshot", async (...args: unknown[]) => {
    readStarted = (cache as any).restrictionProgress.getStore()?.started;
    wall += subject.codexBoundaryDeadlineMs;
    expired = true;
    return read(...args);
  });
  try {
    assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
    assert.equal(readStarted, true, "the actual reader began under B before expiry");
    assert.equal(expired, true, "the test actually advances the deadline before facts exist");
    injected.mock.restore();
    assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
    assert.equal(subject.frozenHeldPolicy(flight).blocked, true);
  } finally { runner.shutdown(); }
});

test("#4191 unenrolled lock timeout clears before ordinary push and ends real source slots", async () => {
  const f = await sourceImportFixture(false);
  const entered = latch(), release = latch();
  const hold = AsyncLocalStorage.bind(() => f.cache.withBareLock(f.bare, async () => {
    entered.release(); await release.promise;
  }));
  const holder = hold();
  await entered.promise;
  f.subject.codexBoundaryDeadlineMs = 30;
  try {
    assert.deepEqual(await f.subject.bootstrapRemediationRestrictions(f.flight), { kind: "unavailable" });
    assert.ok(f.flight.remediationRestrictions, "timeout latches denial while B remains held");
  } finally { release.release(); await holder; }
  f.subject.codexBoundaryDeadlineMs = 5000;
  try {
    const frozen = f.subject.frozenHeldPolicy(f.flight);
    assert.equal(frozen.blocked, true);
    await f.subject.persistRemediationScan(f.flight, f.bare, BRANCH, f.s, [], "pre-exit");
    assert.deepEqual(await f.subject.bootstrapRemediationRestrictions(f.flight), { kind: "confirmed", enrolled: false });
    assert.equal(f.flight.remediationRestrictions, undefined, "verified absence clears the pre-acquisition denial and empty pending");
    assert.equal(f.subject.frozenHeldPolicy(f.flight).blocked, false);
    assert.equal(frozen.blocked, true, "a previously frozen policy stays blocked");
    assert.equal(f.journal().remediationRecovery, undefined, "clean scan never enrolls a blocked journal");
    f.flight.publishedTip = f.base; f.flight.checkpointFloor = f.base;
    await f.subject.pushRemediationCandidate(f.flight, f.bare, BRANCH, "fixture", fx.originPath, undefined,
      { candidate: f.s, originalHead: f.s, floor: { kind: "absent" } });
    assert.equal(command(fx.originPath, "rev-parse", "refs/heads/" + BRANCH), f.s);
    f.flight.sourceTailsJoined = true;
    const end = await f.subject.endFlightSourceUse(f.flight, nullLogger());
    assert.ok(end.endedSlots.length > 0, "real authority source-use slots end");
    assert.equal(f.journal().remediationRecovery, undefined, "teardown writes no denial after the clear");
  } finally { f.runner.shutdown(); }
});

for (const failure of [false, true]) {
  test("#4191 teardown revalidates empty pending with real source slots failure=" + failure, async t => {
    const f = await sourceImportFixture(false);
    const entered = latch(), release = latch();
    const hold = AsyncLocalStorage.bind(() => f.cache.withBareLock(f.bare, async () => {
      entered.release(); await release.promise;
    }));
    const holder = hold(); await entered.promise;
    f.subject.codexBoundaryDeadlineMs = 30;
    try { await f.subject.bootstrapRemediationRestrictions(f.flight); }
    finally { release.release(); await holder; }
    f.subject.codexBoundaryDeadlineMs = 5000;
    try {
      await f.subject.persistRemediationScan(f.flight, f.bare, BRANCH, f.s, [], "pre-exit");
      assert.ok(f.flight.remediationRestrictions.pending);
      if (failure) t.mock.method(f.cache, "readRemediationRestrictionFacts", async () => {
        throw new Error("private sentinel must stay out of diagnostics");
      });
      f.flight.sourceTailsJoined = true;
      const end = await f.subject.endFlightSourceUse(f.flight, nullLogger());
      if (failure) {
        assert.ok(f.flight.remediationRestrictions);
        assert.equal(f.journal().remediationRecovery.blocked, true, "failed revalidation persists denial");
      } else {
        assert.equal(f.flight.remediationRestrictions, undefined);
        assert.equal(f.journal().remediationRecovery, undefined);
      }
      assert.ok(end.endedSlots.length > 0, "real source-use slots leave active after absence or durable denial");
    } finally { f.runner.shutdown(); }
  });
}

for (const mode of ["lock entry budget expiry", "run entry budget expiry", "lock entry other error", "run entry other error"] as const) {
  test("#4191 pre-read maintenance-budget expiry is classified only when positively identified: " + mode, async t => {
    const f = await fixture(false);
    const { runner, cache, subject, flight } = f.fresh();
    let wall = 0;
    const budget = new MaintenanceBudget(1000, () => wall);
    const expire = (): unknown => {
      wall = 5000;
      try { budget.check(); } catch (error) { return error; }
      throw new Error("budget did not expire");
    };
    try {
      await MaintenanceBudget.run(budget, async () => {
        if (mode === "lock entry budget expiry") {
          t.mock.method(cache as any, "recoveryOperation", () => { throw expire(); });
        } else if (mode === "lock entry other error") {
          t.mock.method(cache as any, "recoveryOperation", () => { throw new Error("resource check failed"); });
        } else if (mode === "run entry budget expiry") {
          expire();
        } else {
          t.mock.method(MaintenanceBudget, "run", () => { throw new Error("unrelated entry failure"); });
        }
        assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
      });
      assert.ok(flight.remediationRestrictions, "bootstrap latched");
      assert.equal(flight.remediationRestrictions.clearable, mode.includes("budget expiry"),
        "only a positively identified budget expiry leaves the latch clearable");
    } finally { runner.shutdown(); }
  });
}

for (const mode of ["generic error", "budget-owned expiry"] as const) {
  test("#4191 a failure after the bare lock is acquired keeps the latch sticky: " + mode, async t => {
    const f = await fixture(false);
    const { runner, cache, subject, flight } = f.fresh();
    let wall = 0;
    const budget = new MaintenanceBudget(1000, () => wall);
    const expire = (): unknown => {
      wall = 5000;
      try { budget.check(); } catch (error) { return error; }
      throw new Error("budget did not expire");
    };
    try {
      // withLock checks recovery interruption once before scheduling (pre-acquisition) and once inside
      // the locked section (post-acquisition). Abort only the second check, before the facts callback.
      const original = (cache as any).checkRecoveryInterruption.bind(cache);
      let calls = 0;
      t.mock.method(cache as any, "checkRecoveryInterruption", () => {
        calls++;
        if (calls === 2) throw mode === "budget-owned expiry" ? expire() : new Error("deadline expired after lock acquisition");
        return original();
      });
      await MaintenanceBudget.run(budget, async () => {
        assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "unavailable" });
      });
      assert.ok(calls >= 2, "the post-acquisition check ran");
      assert.ok(flight.remediationRestrictions, "bootstrap latched");
      assert.equal(flight.remediationRestrictions.clearable, false, "post-acquisition failure is sticky");
      t.mock.restoreAll();
      // A later verified unenrolled read must not clear a sticky latch.
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      assert.ok(flight.remediationRestrictions, "sticky latch survives a clean read");
    } finally { runner.shutdown(); }
  });
}

for (const mode of ["clearable reread fails", "clearable reread confirms", "sticky"] as const) {
  test("#4191 teardown of a no-pending latch with a bound completion source: " + mode, async t => {
    const f = await sourceImportFixture(false);
    const entered = latch(), release = latch();
    const hold = AsyncLocalStorage.bind(() => f.cache.withBareLock(f.bare, async () => {
      entered.release(); await release.promise;
    }));
    const holder = hold(); await entered.promise;
    f.subject.codexBoundaryDeadlineMs = 30;
    try { await f.subject.bootstrapRemediationRestrictions(f.flight); }
    finally { release.release(); await holder; }
    f.subject.codexBoundaryDeadlineMs = 5000;
    try {
      assert.ok(f.flight.remediationRestrictions, "bootstrap latched");
      assert.equal(f.flight.remediationRestrictions.pending, undefined, "no pending entry");
      if (mode === "sticky") f.flight.remediationRestrictions.clearable = false;
      if (mode === "clearable reread fails") t.mock.method(f.cache, "readRemediationRestrictionFacts", async () => {
        throw new Error("private sentinel must stay out of diagnostics");
      });
      f.flight.completionSourceBound = { clonePath: f.clone.path };
      f.flight.sourceTailsJoined = true;
      let published = 0;
      t.mock.method(f.subject, "publishFlightSource", async () => { published++; return "published"; });
      const end = await f.subject.endFlightSourceUse(f.flight, nullLogger());
      if (mode === "clearable reread confirms") {
        assert.equal(f.flight.remediationRestrictions, undefined, "verified absence cleared the latch");
        assert.equal(published, 1, "ordinary publication proceeds");
      } else {
        assert.ok(f.flight.remediationRestrictions, "latch kept");
        assert.equal(published, 0, "no publication under a latch");
        assert.deepEqual(end.endedSlots, [], "slots stay active");
        assert.equal(end.published, false);
      }
    } finally { f.runner.shutdown(); }
  });
}

for (const mode of ["active", "protected", "readback", "unknown", "restore", "predecessor", "report proof"] as const) {
  test("#4191 sticky denial survives later verified absence: " + mode, async t => {
    const f = await fixture(mode === "active" || mode === "protected");
    if (mode === "protected") await f.detach();
    const { cache, runner, subject, flight } = f.fresh();
    const diagnostics: unknown[][] = [];
    if (mode === "readback") t.mock.method(subject.log, "warn", (...args: unknown[]) => { diagnostics.push(args); });
    try {
      if (mode === "predecessor") flight.predecessorCapture = true;
      const original = cache.readRemediationRestrictionFacts.bind(cache);
      let injection: ReturnType<typeof t.mock.method> | undefined;
      if (mode === "readback") injection = t.mock.method(cache, "readRemediationRestrictionFacts",
        async (...args: Parameters<typeof original>) => original(args[0], args[1], args[2], undefined,
          { readback: async () => "different" }));
      if (mode === "unknown") injection = t.mock.method(cache, "readRemediationRestrictionFacts", async () => {
        throw new Error("sensitive-fixture-sentinel");
      });
      if (mode === "report proof") injection = t.mock.method(cache as any, "checkReportProof", () => {
        throw new Error("sensitive-proof-sentinel");
      });
      if (mode === "predecessor" || mode === "restore") {
        const entered = latch(), release = latch();
        const hold = AsyncLocalStorage.bind(() => cache.withBareLock(f.bare, async () => {
          entered.release(); await release.promise;
        }));
        const holder = hold(); await entered.promise;
        subject.codexBoundaryDeadlineMs = 30;
        try { await subject.bootstrapRemediationRestrictions(flight); }
        finally { release.release(); await holder; }
        subject.codexBoundaryDeadlineMs = 5000;
      } else await subject.bootstrapRemediationRestrictions(flight);
      injection?.mock.restore();
      if (mode === "readback") assert.deepEqual(diagnostics, [
        ["remediation restriction read unavailable", { class: "changed_facts" }],
      ]);
      if (mode === "restore") {
        const broken = t.mock.method(cache, "readRuntimeRemediationRecovery", async () => { throw new Error("unreadable"); });
        await assert.rejects(subject.restoreRemediationRestrictions(flight), /unreadable/);
        broken.mock.restore();
      }
      if (mode === "active") {
        const journal = f.journal(); delete journal.remediationRecovery; f.write(journal);
      }
      if (mode === "protected") command(f.bare, "config", "--unset", "uzi-retained." + f.claim.run_id + ".journal");
      flight.predecessorCapture = false;
      assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
      assert.ok(flight.remediationRestrictions, "later absence cannot erase observed or unknown denial");
      assert.equal(subject.frozenHeldPolicy(flight).blocked, true);
    } finally { runner.shutdown(); }
  });
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
        assert.ok(flight.remediationDenialRevision > 0);
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
    if (late === "overflow") flight.remediationDenialRevision = Number.MAX_SAFE_INTEGER;
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
      assert.deepEqual(await subject.bridgeBareTrackingRefIfDivergent(f.bare, BRANCH, flight, nullLogger()),
        { kind: late === "confirmed" ? "clean" : "unknown" });
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
        assert.equal(flight.remediationDenialRevision, 1);
        await subject.requireLaunchAuthority(flight);
        assert.equal(flight.remediationDenialRevision, 1, "fresh confirmation never resets a denial");
      }
      if (late === "overflow") {
        assert.equal(flight.remediationDenialRevision, -1);
        await assert.rejects(subject.requireLaunchAuthority(flight), { message: "remediation restriction confirmation changed" });
        assert.equal(flight.remediationDenialRevision, -1, "overflow never wraps or clears");
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

for (const mode of ["healthy", "unenrolled", "revoked", "expired", "unsafe", "retry expiry", "default live"] as const) {
  test("shutdown checkpoint admission retains handoff and deadline: " + mode, async t => {
    const f = await fixture(mode !== "unenrolled");
    if (mode !== "unenrolled")
      await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: f.s, flaggedOIDs: [] });
    const { cache, runner, subject, flight } = f.fresh();
    flight.publishedTip = f.base; flight.checkpointFloor = f.base;
    assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight),
      { kind: "confirmed", enrolled: mode !== "unenrolled" });
    let wall = Date.now();
    t.mock.method(Date, "now", () => wall);
    const active: any = { steering: flight.steering };
    subject.activeRuns.set(flight.runId, active);
    runner.shutdown();
    const cause = active.gracefulShutdownCheckpoint;
    subject.checkpointTestHooks = { retryBackoffMs: [0, 0] };
    if (mode === "revoked") flight.steering.abortLifecycle(new Error("cleanup revoked"));
    if (mode === "expired") wall = cause.deadlineAt;
    if (mode === "unsafe") flight.checkpointFloor = f.q;
    const deadlines: number[] = [];
    const read = cache.readRemediationRestrictionFacts.bind(cache);
    t.mock.method(cache, "readRemediationRestrictionFacts", (...args: Parameters<typeof read>) => {
      const scope = (cache as any).recoveryOperations.getStore();
      assert.equal(scope.signal.aborted, false);
      assert.ok(scope.deadline <= cause.deadlineAt, "no admission renews the shutdown deadline");
      deadlines.push(scope.deadline);
      return read(...args);
    });
    let uploads = 0;
    t.mock.method(client, "publishCheckpoint", async (_run: string, _tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
      for await (const chunk of pack) assert.ok(chunk.length > 0);
      uploads++;
      if (uploads === 1) {
        wall = mode === "retry expiry" ? cause.deadlineAt : wall + 1;
        return { ok: false as const, httpStatus: 503 };
      }
      return { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/test" } };
    });
    try {
      const result = await subject.publishCheckpointOutcome(flight, f.bare, BRANCH, undefined,
        mode === "default live" ? undefined : cause.stopSignal, undefined, undefined, true,
        mode === "default live" ? undefined : cause.deadlineAt);
      const healthy = mode === "healthy" || mode === "unenrolled";
      assert.equal(result.published, healthy);
      assert.equal(uploads, healthy ? 2 : mode === "retry expiry" ? 1 : 0);
      if (healthy) {
        assert.ok(deadlines.length >= 2, "real admission runs before packing and replay");
        assert.equal(deadlines.at(-1), cause.deadlineAt, "retry retains the original absolute cap");
      }
      if (mode === "expired" || mode === "revoked" || mode === "default live")
        assert.equal(deadlines.length, 0, "no custody query after authority ends");
    } finally {
      subject.activeRuns.delete(flight.runId);
      runner.shutdown();
    }
  });
}

test("#4191 negative post-action expiry latches denial and independent private capture retains bytes", async t => {
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
    assert.equal(flight.remediationDenialRevision, 1);
    assert.equal(flight.preserveRecoveryClone, true);
    const result = await subject.captureRecoveryRestorePointBody(f.claim, flight, nullLogger(),
      "recovery_capture", { credentialFree: true, publish: false });
    assert.equal(result.verified, true);
    assert.equal(result.published, false);
    assert.deepEqual(f.snapshot(), before);
    f.write(f.seedJournal);
    assert.deepEqual(await subject.bootstrapRemediationRestrictions(flight), { kind: "confirmed", enrolled: false });
    await assert.rejects(subject.restoreRemediationRestrictions(flight), /enrollment disappeared/);
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
    assert.equal(flight.remediationDenialRevision, 1);
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
      assert.equal(flight.remediationDenialRevision, 1);
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
    assert.equal(flight.remediationDenialRevision, 1);
    assert.equal(siblingA, true); assert.equal(siblingB, true);
  });
}

test("publication findings persistence keeps a local bounded lifecycle", async t => {
  const f = await fixture();
  await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: f.s, flaggedOIDs: [] });
  const { cache, runner, subject, flight } = f.fresh();
  flight.publishedTip = f.base; flight.checkpointFloor = f.base;
  const wall = Date.now();
  t.mock.method(Date, "now", () => wall);
  const persist = cache.recordRemediationRestrictions.bind(cache);
  let writes = 0;
  t.mock.method(cache, "recordRemediationRestrictions", (...args: Parameters<typeof persist>) => {
    assert.equal((cache as any).recoveryOperations.getStore().deadline,
      wall + subject.codexBoundaryDeadlineMs);
    writes++;
    return persist(...args);
  });
  t.mock.method(cache, "secretScanCheckpointRange", async () => ({ trusted: true as const,
    findings: [{ file: "fixture.txt", startLine: 1, commit: f.s, ruleId: "fixture" }] }));
  try {
    assert.equal((await subject.publishCheckpointOutcome(flight, f.bare, BRANCH)).published, false);
    assert.equal(writes, 1);
    assert.ok(f.journal().remediationRecovery.flaggedOIDs.includes(f.s));
  } finally { runner.shutdown(); }
});

for (const transport of ["push", "pack/replay"] as const) {
  for (const clock of ["live", "caller", "maintenance"] as const) {
    test("Runner transport outlives admission clock: " + transport + "/" + clock, async t => {
      const f = await sourceImportFixture(true);
      await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: f.s, flaggedOIDs: [] });
      const { cache, runner, subject, flight } = f;
      flight.publishedTip = f.base; flight.checkpointFloor = f.base;
      const startedAt = Date.now();
      let wall = startedAt, transports = 0, guards = 0;
      t.mock.method(Date, "now", () => wall);
      const deadline = clock === "caller" || (clock === "maintenance" && transport === "pack/replay")
        ? wall + 300_000 : Infinity;
      const caller = new AbortController();
      const budget = clock === "maintenance" ? new MaintenanceBudget(300_000, () => wall) : undefined;
      const positive = cache.readRuntimeRemediationRecovery.bind(cache);
      let proofs = 0;
      t.mock.method(cache, "readRuntimeRemediationRecovery", (...args: Parameters<typeof positive>) => {
        assert.ok(activeAuthorityTransaction(f.lifetime), "enrolled source authentication holds A");
        proofs++;
        return positive(...args);
      });
      const scan = cache.secretScanCheckpointRange.bind(cache);
      t.mock.method(cache, "secretScanCheckpointRange", (...args: Parameters<typeof scan>) => {
        assert.ok(activeAuthorityTransaction(f.lifetime), "enrolled work retains A");
        const scope = (cache as any).recoveryOperations.getStore();
        assert.ok(scope.deadline <= wall + subject.codexBoundaryDeadlineMs, "each candidate scan is bounded");
        guards++;
        return scan(...args);
      });
      const advance = () => {
        assert.ok(activeAuthorityTransaction(f.lifetime), "enrolled work retains A");
        const scope = (cache as any).recoveryOperations.getStore();
        assert.equal(scope.deadline, deadline, "actual Git transport inherits only the caller clock");
        wall += 31_000;
        assert.equal(scope.signal.aborted, false);
        transports++;
      };
      const run = (cache as any).runGit.bind(cache);
      t.mock.method(cache as any, "runGit", (bare: string, args: string[], ...rest: unknown[]) => {
        const operation = run(bare, args, ...rest);
        if (args[0] === "push") advance();
        return operation;
      });
      const spawn = (cache as any).spawnGit.bind(cache);
      t.mock.method(cache as any, "spawnGit", (bare: string, args: string[], ...rest: unknown[]) => {
        const operation = spawn(bare, args, ...rest);
        if (args[0] === "pack-objects") advance();
        return operation;
      });
      subject.checkpointTestHooks = { retryBackoffMs: [0, 0] };
      let uploads = 0;
      t.mock.method(client, "publishCheckpoint", async (_run: string, _tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
        for await (const chunk of pack) assert.ok(chunk.length > 0);
        return ++uploads === 1 ? { ok: false as const, httpStatus: 503 }
          : { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/test" } };
      });
      try {
        const publish = async () => {
          if (transport === "push") {
            await subject.pushRemediationCandidate(flight, f.bare, BRANCH, "fixture", fx.originPath, undefined,
              { candidate: f.s, originalHead: f.s, floor: { kind: "absent" } });
            assert.equal(command(fx.originPath, "rev-parse", "refs/heads/" + BRANCH), f.s);
          } else {
            const result = await subject.publishCheckpointOutcome(flight, f.bare, BRANCH);
            assert.equal(result.published, true);
            assert.equal(uploads, 2, "the actual pack is replayed after an explicit 503");
          }
        };
        if (clock === "caller") await cache.withRecoveryOperation(caller.signal, deadline, publish);
        else if (budget) await MaintenanceBudget.run(budget, publish);
        else await publish();
        assert.equal(transports, transport === "push" ? 1 : 2);
        assert.ok(wall - startedAt > 30_000, "transport completes beyond the admission allowance");
        assert.ok(guards > 0, "real candidate scans precede transport");
        assert.equal(proofs, transport === "push" ? 1 : 2, "every producer rebuilds current source authentication");
      } finally { budget?.dispose(); runner.shutdown(); }
    });
  }
}

for (const stop of ["deadline", "caller", "maintenance"] as const) {
  test("Runner push transport cancels and joins on " + stop, async t => {
    const f = await fixture(false);
    const { cache, runner, subject, flight } = f.fresh();
    let wall = Date.now(), expire: (() => void) | undefined, cancelled = false, settled = false;
    t.mock.method(Date, "now", () => wall);
    const deadline = wall + 90_000;
    const realTimer = globalThis.setTimeout;
    t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number) => {
      if (delay === 90_000 && !expire) expire = callback;
      return realTimer(callback, delay);
    });
    const budget = stop === "maintenance" ? new MaintenanceBudget(45_000, () => wall) : undefined;
    const caller = new AbortController();
    const entered = latch(), closed = latch();
    const run = (cache as any).runGit.bind(cache);
    t.mock.method(cache as any, "runGit", (bare: string, args: string[], ...rest: unknown[]) => {
      if (args[0] !== "push") return run(bare, args, ...rest);
      assert.equal((cache as any).recoveryOperations.getStore().deadline, deadline);
      return cache.withBoundaryProcessSpawner(async () => {
        entered.release();
        const stdin = new PassThrough(), stdout = new PassThrough(), stderr = new PassThrough();
        stdout.end(); stderr.end();
        return { stdin, stdout, stderr, completed: closed.promise.then(() => ({ code: 1 })),
          cancel: async () => { cancelled = true; } };
      }, new AbortController().signal, () => run(bare, args, ...rest));
    });
    const publish = () => cache.withRecoveryOperation(caller.signal, deadline, () =>
      subject.pushRemediationCandidate(flight, f.bare, BRANCH, "fixture", fx.originPath, undefined,
        { candidate: f.s, originalHead: f.s, floor: { kind: "absent" } }));
    const operation = budget ? MaintenanceBudget.run(budget, publish) : publish();
    const outcome = operation.then(() => "accepted", () => "rejected").finally(() => { settled = true; });
    try {
      assert.equal(await Promise.race([entered.promise.then(() => true), outcome.then(() => false)]), true);
      if (stop === "deadline") {
        assert.ok(expire);
        wall = deadline; expire();
      } else if (budget) {
        wall += 45_000;
        assert.throws(() => budget.check(), /maintenance deadline/);
      } else caller.abort(new Error("caller cancelled"));
      await tick();
      assert.equal(cancelled, true);
      assert.equal(settled, false, "cancellation retains custody until terminal close");
    } finally {
      closed.release();
      await outcome;
      budget?.dispose(); runner.shutdown();
    }
    assert.equal(await outcome, "rejected");
  });
}


async function sourceImportFixture(enrolled = false) {
  const f = await fixture(enrolled);
  const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: "fixture-worker", timeoutMs: 5000 });
  const fresh = f.fresh(authority);
  fresh.flight.executor = {};
  // Bind the selected genuine clone, then authenticate only this same flight-owned lease.
  await fresh.subject.bindFlightSource(fresh.flight, f.clone.path);
  const epoch = authority.ownedSourceEpoch(f.clone.path, fresh.flight.sourceUsePaths);
  assert.ok(epoch);
  return { ...f, ...fresh, lifetime, authority, epoch };
}

for (const change of ["none", "remediation", "journal", "attempt", "lease", "tracking", "unknown tracking"] as const) {
  test("source import releases A across actual transport and reauthenticates: " + change, async t => {
    const f = await sourceImportFixture(change === "remediation");
    const incoming = f.commit("import candidate", f.s);
    const enter = f.subject.inFlightAuthority.bind(f.subject);
    let proofs = 0;
    t.mock.method(f.subject, "inFlightAuthority", async (...args: any[]) => {
      if (++proofs === 2 && change === "tracking") {
        // A real same-cache B mutation in the unlocked gap, before promotion asks for A.
        await f.cache.updateTrackingRef(f.bare, BRANCH, f.d, {
          context: { runId: f.claim.run_id, generation: 2, kind: "issue", branch: BRANCH, barePath: f.bare,
            defaultIdentity: { ref: "refs/remotes/origin/main", sha: f.base } },
        });
      }
      return enter(...args);
    });
    const started = latch(), resume = latch();
    const caller = new AbortController();
    const deadline = Date.now() + 300_000;
    let wall = Date.now(), settled = false, transports = 0;
    t.mock.method(Date, "now", () => wall);
    const run = (f.cache as any).runGit.bind(f.cache);
    t.mock.method(f.cache as any, "runGit", async (bare: string, args: string[], ...rest: unknown[]) => {
      if (args.includes("fetch") && args.includes("file://" + f.clone.path)) {
        transports++;
        assert.equal(activeAuthorityTransaction(f.lifetime), undefined, "actual file transport has no A");
        assert.equal((f.cache as any).recoveryOperations.getStore().deadline, deadline);
        assert.deepEqual(args.slice(0, 7), ["-c", "protocol.file.allow=user", "fetch", "--refmap=",
          "--no-tags", "--no-write-fetch-head", "file://" + f.clone.path]);
        assert.match(args[7]!, /^\+refs\/heads\/agent\/issue-3747:refs\/uzi-incoming\/[0-9a-f-]+$/);
        started.release();
        await resume.promise;
        wall += 31_000; // Transport consumes its parent budget, not a fresh admission clock.
      }
      return run(bare, args, ...rest);
    });
    const action = f.subject.fetchTracking(f.flight, f.bare, f.clone.path, BRANCH,
      { expectedCandidateSha: incoming },
      (fn: () => Promise<unknown>) => f.cache.withRecoveryOperation(caller.signal, deadline, fn));
    const outcome = action.then((value: unknown) => { settled = true; return { value }; },
      (error: unknown) => { settled = true; return { error }; });
    let sameBare = false, otherBare = false;
    let sibling: Promise<void> | undefined;
    try {
      assert.equal(await Promise.race([started.promise.then(() => true), outcome.then(() => false)]), true);
      // This A -> different B must progress; an A waiter on the transport's B would obstruct it.
      const different = path.join(fx.dataDir, "sibling-import.git");
      command(fx.dataDir, "init", "--bare", different);
      await f.authority.withTransaction(() => f.cache.withBareLock(different, async () => { otherBare = true; }));
      sibling = f.cache.withBareLock(f.bare, async () => { sameBare = true; });
      await tick();
      assert.equal(otherBare, true); assert.equal(sameBare, false); assert.equal(settled, false);
      assert.equal(command(f.bare, "rev-parse", "refs/uzi-runner/" + BRANCH), f.s);
      assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
      if (change === "journal") f.write({ ...f.journal(), runId: "another-run" });
      if (change === "attempt") {
        const key = "uzi-attempts." + BRANCH + ".entry";
        const entries = command(f.bare, "config", "--get-all", key).split("\n").map(v => JSON.parse(v));
        const selected = entries.findLast(e => e.attemptId === ATTEMPT);
        assert.ok(selected);
        command(f.bare, "config", "--add", key, JSON.stringify({ ...selected, state: "abandoned" }));
      }
      if (change === "lease") {
        const ended = await f.authority.endSourceUse([f.clone.path], { sink: f.flight.sourceUsePaths });
        assert.equal(ended.endedSlots.length, 1);
      }
      if (change === "unknown tracking") command(f.bare, "update-ref", "refs/uzi-runner/" + BRANCH, f.d);
    } finally {
      resume.release();
      await Promise.all([outcome, ...(sibling ? [sibling] : [])]);
      f.runner.shutdown();
    }
    const result = await outcome;
    assert.equal(transports, 1); assert.equal(sameBare, true);
    const stages = command(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/");
    if (change === "none" || change === "remediation" || change === "tracking") {
      assert.ok("value" in result, String("error" in result ? result.error : ""));
      assert.equal(command(f.bare, "rev-parse", "refs/uzi-runner/" + BRANCH), incoming);
      if (change === "remediation") {
        assert.equal(f.journal().remediationRecovery.ownedTip, incoming);
        assert.equal(f.journal().remediationRecovery.edges.length, 1, "existing committed edge survives");
      } else assert.equal(f.journal().remediationRecovery, undefined, "import invents no remediation grant");
      assert.equal(stages, "", "independent tracking anchor permits stage cleanup");
      if (change === "tracking") {
        const owned = await f.cache.enumerateOwedCandidates(f.bare, f.claim.run_id);
        assert.ok(owned.some(c => c.sha === f.d), "fresh displacement preserves the intervening candidate");
        assert.equal((result.value as any).displacedSha, f.d);
      }
      assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
    } else {
      assert.ok("error" in result, "changed proof refuses import");
      assert.equal(command(f.bare, "rev-parse", "refs/uzi-runner/" + BRANCH),
        change === "unknown tracking" ? f.d : f.s);
      if (change !== "unknown tracking") assert.equal(stages, incoming, "uncertain source proof retains stage");
    }
  });
}

test("source import refuses a changed private stage before promotion", async t => {
  const f = await sourceImportFixture();
  const incoming = f.commit("staged candidate", f.s);
  const enter = f.subject.inFlightAuthority.bind(f.subject);
  let proofs = 0;
  t.mock.method(f.subject, "inFlightAuthority", async (...args: any[]) => {
    if (++proofs === 2) {
      const ref = command(f.bare, "for-each-ref", "--format=%(refname)", "refs/uzi-incoming/");
      assert.ok(ref.startsWith("refs/uzi-incoming/"));
      command(f.bare, "update-ref", ref, f.base, incoming);
    }
    return enter(...args);
  });
  try {
    await assert.rejects(f.subject.fetchTracking(f.flight, f.bare, f.clone.path, BRANCH,
      { expectedCandidateSha: incoming }), /private stage changed/);
    assert.equal(proofs, 2);
    assert.equal(command(f.bare, "rev-parse", "refs/uzi-runner/" + BRANCH), f.s);
    assert.equal(command(f.clone.path, "rev-parse", "HEAD"), incoming);
    assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
    assert.equal(f.journal().remediationRecovery, undefined);
  } finally { f.runner.shutdown(); }
});

test("source import requires an authority callback before transport", async t => {
  const f = await sourceImportFixture();
  let transports = 0;
  const run = (f.cache as any).runGit.bind(f.cache);
  t.mock.method(f.cache as any, "runGit", (bare: string, args: string[], ...rest: unknown[]) => {
    if (args.includes("fetch")) transports++;
    return run(bare, args, ...rest);
  });
  try {
    await assert.rejects(f.cache.fetchAgentBranch(f.bare, f.clone.path, BRANCH, f.claim.run_id, {
      context: { runId: f.claim.run_id, generation: 2, kind: "issue", branch: BRANCH, barePath: f.bare,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: f.base } }, producingSource: f.source,
    }), /requires trusted authority callback/);
    assert.equal(transports, 0);
    assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
  } finally { f.runner.shutdown(); }
});

test("source import cancellation joins terminal close before ending flight source use", async t => {
  const f = await sourceImportFixture();
  const spawned = latch();
  const child = { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() };
  let complete!: (result: { code: number }) => void;
  const completed = new Promise<{ code: number }>(resolve => { complete = resolve; });
  let cancelled = false, settled = false, sameBare = false;
  const caller = new AbortController();
  const run = (f.cache as any).runGit.bind(f.cache);
  t.mock.method(f.cache as any, "runGit", (bare: string, args: string[], ...rest: unknown[]) => {
    if (!args.includes("fetch")) return run(bare, args, ...rest);
    assert.equal(activeAuthorityTransaction(f.lifetime), undefined);
    return f.cache.withBoundaryProcessSpawner(async () => {
      spawned.release();
      return { ...child, completed, cancel: async () => { cancelled = true; } };
    }, caller.signal, () => run(bare, args, ...rest));
  });
  const operation = f.subject.fetchTracking(f.flight, f.bare, f.clone.path, BRANCH, {},
    (fn: () => Promise<unknown>) => f.cache.withRecoveryOperation(caller.signal, Infinity, fn));
  const outcome = operation.then((value: unknown) => { settled = true; return { value }; },
    (error: unknown) => { settled = true; return { error }; });
  let sibling: Promise<void> | undefined;
  try {
    assert.equal(await Promise.race([spawned.promise.then(() => true), outcome.then(() => false)]), true);
    sibling = f.cache.withBareLock(f.bare, async () => { sameBare = true; });
    child.stdout.end(); child.stderr.end(); caller.abort(new Error("caller cancelled"));
    await tick();
    assert.equal(cancelled, true); assert.equal(settled, false); assert.equal(sameBare, false);
    await f.authority.withTransaction(async () => {});
    assert.notEqual(f.flight.sourceTailsJoined, true);
    const end = await f.subject.endFlightSourceUse(f.flight, nullLogger());
    assert.deepEqual(end.endedSlots, [], "EOF/cancel is not a joined source tail");
    assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
  } finally {
    complete({ code: 1 });
    await Promise.all([outcome, ...(sibling ? [sibling] : [])]);
    f.runner.shutdown();
  }
  assert.ok("error" in await outcome);
  assert.equal(sameBare, true);
  f.flight.sourceTailsJoined = true; // Controller now has the actual terminal-close join.
  const end = await f.subject.endFlightSourceUse(f.flight, nullLogger());
  assert.equal(end.endedSlots.length, 1);
});


for (const mode of ["done", "milestone"] as const) for (const stop of ["soft", "hard"] as const) {
  test("source import preserves checkpoint stop scopes: " + mode + "/" + stop, async t => {
    const f = await sourceImportFixture();
    const incoming = f.commit("checkpoint candidate", f.s);
    const soft = new AbortController(), hard = new AbortController();
    let transported = false;
    const run = (f.cache as any).runGit.bind(f.cache);
    t.mock.method(f.cache as any, "runGit", async (bare: string, args: string[], ...rest: unknown[]) => {
      const result = await run(bare, args, ...rest);
      if (args.includes("fetch")) {
        assert.equal(activeAuthorityTransaction(f.lifetime), undefined);
        transported = true;
        (stop === "soft" ? soft : hard).abort();
      }
      return result;
    });
    const action = () => f.subject.fetchTracking(f.flight, f.bare, f.clone.path, BRANCH,
      { expectedCandidateSha: incoming, checkpointImportMode: mode },
      (fn: () => Promise<unknown>) => f.cache.withBoundaryProcessSpawner(async request => {
        const child = childProcess.spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env });
        const completed = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? -1 }));
        });
        return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
          cancel: async () => { child.kill("SIGTERM"); await completed; } };
      }, hard.signal, fn, { softSignal: soft.signal }));
    try {
      // Milestone import settles through a soft stop; the runner's subsequent
      // requireTrackingOwned guard still observes that stop and must refuse progress.
      await assert.rejects(action(), stop === "soft" ? /soft deadline/ : /aborted/);
      const settledImport = mode === "milestone" && stop === "soft";
      assert.equal(command(f.bare, "rev-parse", "refs/uzi-runner/" + BRANCH), settledImport ? incoming : f.s);
      assert.equal(command(f.bare, "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"),
        settledImport ? "" : incoming);
      if (settledImport) assert.equal((await f.cache.committedTrackingOwnership(f.bare, BRANCH,
        f.claim.run_id, incoming)).kind, "owned");
      assert.equal(transported, true);
      assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
    } finally { f.runner.shutdown(); }
  });
}

for (const transport of ["push", "pack/replay"] as const) {
  test("ordinary publication keeps A free and serializes B: " + transport, async t => {
    const f = await sourceImportFixture(false);
    f.flight.publishedTip = f.base; f.flight.checkpointFloor = f.base;
    const started = latch(), resume = latch();
    let transports = 0, reads = 0, uploads = 0, sameBare = false;
    const read = f.cache.readRemediationRestrictionFacts.bind(f.cache);
    t.mock.method(f.cache, "readRemediationRestrictionFacts", (...args: Parameters<typeof read>) => {
      reads++;
      assert.equal(args[3], undefined, "ordinary custody never selects a source");
      assert.equal(activeAuthorityTransaction(f.lifetime), undefined);
      return read(...args);
    });
    const observe = async () => {
      transports++;
      assert.equal(activeAuthorityTransaction(f.lifetime), undefined, "actual transport has no A");
      assert.equal((f.cache as any).recoveryOperations.getStore().deadline, Infinity);
      if (transports === 1) { started.release(); await resume.promise; }
    };
    const run = (f.cache as any).runGit.bind(f.cache);
    t.mock.method(f.cache as any, "runGit", async (bare: string, args: string[], ...rest: unknown[]) => {
      if (args[0] === "push") await observe();
      return run(bare, args, ...rest);
    });
    const spawn = (f.cache as any).spawnGit.bind(f.cache);
    t.mock.method(f.cache as any, "spawnGit", async (bare: string, args: string[], ...rest: unknown[]) => {
      if (args[0] === "pack-objects") await observe();
      return spawn(bare, args, ...rest);
    });
    f.subject.checkpointTestHooks = { retryBackoffMs: [0, 0] };
    t.mock.method(client, "publishCheckpoint", async (_run: string, _tip: string, pack: Parameters<typeof client.publishCheckpoint>[2]) => {
      for await (const chunk of pack) assert.ok(chunk.length > 0);
      return ++uploads === 1 ? { ok: false as const, httpStatus: 503 }
        : { ok: true as const, body: { published: true, ref: "refs/uzi-checkpoints/test" } };
    });
    const action = transport === "push"
      ? f.subject.pushRemediationCandidate(f.flight, f.bare, BRANCH, "fixture", fx.originPath, undefined,
        { candidate: f.s, originalHead: f.s, floor: { kind: "absent" } })
      : f.subject.publishCheckpointOutcome(f.flight, f.bare, BRANCH);
    const outcome = action.then((value: any) => ({ value }), (error: unknown) => ({ error }));
    let sibling: Promise<void> | undefined;
    try {
      assert.equal(await Promise.race([started.promise.then(() => true), outcome.then(() => false)]), true);
      const different = path.join(fx.dataDir, "sibling-publication.git");
      command(fx.dataDir, "init", "--bare", different);
      let otherBare = false;
      await f.authority.withTransaction(() => f.cache.withBareLock(different, async () => { otherBare = true; }));
      sibling = f.cache.withBareLock(f.bare, async () => { sameBare = true; });
      await tick();
      assert.equal(otherBare, true); assert.equal(sameBare, false);
    } finally {
      resume.release();
      await Promise.all([outcome, ...(sibling ? [sibling] : [])]);
      f.runner.shutdown();
    }
    const result = await outcome;
    assert.ok("value" in result, String("error" in result ? result.error : ""));
    if (transport === "push") assert.equal(command(fx.originPath, "rev-parse", "refs/heads/" + BRANCH), f.s);
    else { assert.equal(result.value.published, true); assert.equal(uploads, 2); }
    assert.equal(transports, transport === "push" ? 1 : 2);
    assert.equal(reads, transports, "one preliminary custody query per producer, no doubled ordinary query");
    assert.equal(sameBare, true);
    assert.equal(f.journal().remediationRecovery, undefined);
    assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
  });
}

for (const transport of ["push", "pack"] as const) {
  for (const denial of ["read failure", "retired", "late enrollment", "late latch", "replay enrollment"] as const) {
    if (transport === "push" && denial === "replay enrollment") continue;
    test("ordinary publication refuses " + denial + ": " + transport, async t => {
      const f = await sourceImportFixture(false);
      f.flight.publishedTip = f.base; f.flight.checkpointFloor = f.base;
      let transports = 0, entries = 0, uploads = 0;
      const finding = { file: "fixture.txt", startLine: 1, commit: f.s, ruleId: "fixture" };
      if (denial === "read failure") t.mock.method(f.cache, "readRemediationRestrictionFacts", async () => {
        throw new Error("known custody read failure");
      });
      const beforeB = async () => {
        entries++;
        assert.equal(activeAuthorityTransaction(f.lifetime), undefined);
        if (denial === "retired") f.flight.sourceAdmissionRetired = true;
        if (denial === "late latch") f.subject.latchRemediationFindings(f.flight, f.bare, BRANCH, [finding]);
        if (denial === "late enrollment" || (denial === "replay enrollment" && entries === 2)) {
          // Disk-only enrollment after preliminary absence, before the actual B guard.
          // Do not latch flight memory: this specifically tests fresh B-locked facts.
          await f.subject.inFlightAuthority(f.flight, () => f.cache.recordRemediationScan(
            f.bare, BRANCH, f.source,
            { trusted: true, tip: f.s, flaggedOIDs: [f.s], origin: "checkpoint-pending" }));
          assert.equal(f.flight.remediationRestrictions, undefined);
          assert.ok(f.journal().remediationRecovery.flaggedOIDs.includes(f.s));
        }
      };
      const push = f.cache.pushBranch.bind(f.cache);
      t.mock.method(f.cache, "pushBranch", async (...args: Parameters<typeof push>) => { await beforeB(); return push(...args); });
      const pack = f.cache.checkpointPack.bind(f.cache);
      t.mock.method(f.cache, "checkpointPack", async (...args: Parameters<typeof pack>) => {
        await beforeB();
        const packed = await pack(...args);
        if (!packed || denial !== "replay enrollment") return packed;
        return { ...packed, replay: async () => { await beforeB(); return packed.replay(); } };
      });
      const run = (f.cache as any).runGit.bind(f.cache);
      t.mock.method(f.cache as any, "runGit", (bare: string, args: string[], ...rest: unknown[]) => {
        if (args[0] === "push") transports++;
        return run(bare, args, ...rest);
      });
      const spawn = (f.cache as any).spawnGit.bind(f.cache);
      t.mock.method(f.cache as any, "spawnGit", (bare: string, args: string[], ...rest: unknown[]) => {
        if (args[0] === "pack-objects") transports++;
        return spawn(bare, args, ...rest);
      });
      f.subject.checkpointTestHooks = { retryBackoffMs: [0, 0] };
      t.mock.method(client, "publishCheckpoint", async (_run: string, _tip: string, bytes: Parameters<typeof client.publishCheckpoint>[2]) => {
        for await (const chunk of bytes) assert.ok(chunk.length > 0);
        uploads++;
        return { ok: false as const, httpStatus: 503 };
      });
      try {
        if (transport === "push") await assert.rejects(f.subject.pushRemediationCandidate(f.flight, f.bare, BRANCH,
          "fixture", fx.originPath, undefined, { candidate: f.s, originalHead: f.s, floor: { kind: "absent" } }));
        else assert.equal((await f.subject.publishCheckpointOutcome(f.flight, f.bare, BRANCH)).published, false);
        assert.equal(entries, denial === "read failure" ? 0 : denial === "replay enrollment" ? 2 : 1);
        assert.equal(transports, denial === "replay enrollment" ? 1 : 0, "fresh B facts refuse before push/producer");
        assert.equal(uploads, denial === "replay enrollment" ? 1 : 0);
        assert.equal(f.authority.ownedSourceEpoch(f.clone.path, f.flight.sourceUsePaths), f.epoch);
      } finally { f.runner.shutdown(); }
    });
  }
}

// ---------------------------------------------------------------------------------------------
// issue #4246: a launch round whose bare-lock wait aborts before acquisition is retried as a fresh
// round within a per-claim allowance. Real bare lock, real restore wrapper, no real 30s waits.
// ---------------------------------------------------------------------------------------------

type LockMethod = "readRemediationRestrictionFacts" | "readRuntimeRemediationRecovery";
const LAUNCH_MS = 4321; // distinctive, so the clock hook recognises the round's own operation timer
const ABORT_LINE = "launch round lock wait aborted";

/** Skews Date.now and captures the first matching operation timer so a test expires a round without waiting. */
function launchClock(t: any) {
  const realNow = Date.now.bind(Date);
  const realTimer = globalThis.setTimeout;
  let skew = 0, armed = false, expires: Array<() => void> = [];
  let frozen: number | undefined;
  const dateMock = t.mock.method(Date, "now", () => frozen ?? realNow() + skew);
  const timerMock = t.mock.method(globalThis, "setTimeout", (callback: () => void, delay?: number, ...rest: unknown[]) => {
    // Every deadline-window timer is captured in arm order: index 0 is the round's outer operation, index 1
    // the nested recovery operation the restriction-facts bootstrap opens for the same deadline.
    if (armed && typeof delay === "number" && delay > LAUNCH_MS - 1000 && delay <= LAUNCH_MS) expires.push(callback);
    return (realTimer as any)(callback, delay, ...rest);
  });
  return {
    /** node:test restores mocks FIFO at test end, which would leave a first wrapper installed under a second
     *  one; a caller that builds more than one clock in a test restores LIFO here instead. */
    restore: () => { timerMock.mock.restore(); dateMock.mock.restore(); },
    arm: () => { armed = true; expires = []; },
    /** Pins Date.now to an absolute value (until restore), so real elapsed time cannot move it. */
    freeze: (at: number) => { frozen = at; },
    /** elapsed=true advances the clock past the deadline first (a real expiry); false is an early notification.
     *  `nth` picks the captured timer: 0 the outer operation (default), 1 the nested one. */
    fire: (elapsed: boolean, nth = 0) => {
      const callback = expires[nth];
      assert.ok(callback, "the round's operation timer #" + nth + " was installed (captured " + expires.length + ")");
      expires = []; armed = false;
      if (elapsed) skew += LAUNCH_MS;
      callback();
    },
  };
}

const failedReports = (runId: string) => api.states.filter(s => s.runId === runId && s.body.status === "failed").map(s => s.body as any);
const abortLines = (calls: unknown[][]) => calls.filter(c => c[0] === ABORT_LINE).map(c => c[1] as any);

type Hold = { method: LockMethod; mode: "new_release_on_abort" | "new_until_end" | "existing" };

/** Full runner.execute with an independent holder of the real bare lock injected at chosen launch rounds. */
async function driveExecute(t: any, iid: number, decide: (site: "clone" | "launch", round: number) => Hold | undefined, opts: {
  /** Extra RunnerOptions, e.g. a real authority runtime. */
  extra?: Record<string, unknown>;
  /** Runs inside every lock read made by a clone or launch round, before it proceeds. */
  onRead?: (site: "clone" | "launch", round: number) => void;
  /** Runs once, inside the first launch-round backoff wait, before the wait is delegated. */
  onBackoff?: (flight: any, runner: RunRunner) => void | Promise<void>;
  /** Expire the NESTED operation's timer (the second deadline-window timer) instead of the outer one, so the
   *  read's wait rejects while the round's outer scope is still unaborted. */
  nestedTimer?: boolean;
} = {}) {
  const claim = gitlabClaim(iid);
  const stub = new StubExecutor(nullLogger());
  const origRun = stub.run.bind(stub);
  let flight: any, runs = 0, atLaunch: unknown = "unset";
  t.mock.method(stub, "run", async (...args: Parameters<typeof origRun>) => {
    runs++; atLaunch = flight?.remediationRestrictions; return origRun(...args);
  });
  const runner = runnerFor(stub, fakeGitlab().gitlab, undefined, { recoveryRetryMs: 1, ...opts.extra });
  const subject = runner as any;
  const defaultMs = subject.codexBoundaryDeadlineMs;
  const clock = launchClock(t);
  const hold = AsyncLocalStorage.bind((bare: string, entered: { release: () => void }, until: Promise<void>) =>
    git.withBareLock(bare, async () => { entered.release(); await until; }));
  const holders: Promise<void>[] = [], releases: Array<() => void> = [], diagnostics: unknown[][] = [];
  const counts = { clone: 0, launch: 0 };
  let current: { hold?: Hold; injected: boolean; release?: () => void; site: "clone" | "launch"; n: number } | undefined;
  for (const [name, site] of [["cloneRemediationRound", "clone"], ["requireLaunchAuthority", "launch"]] as const) {
    const orig = subject[name].bind(subject);
    t.mock.method(subject, name, async (...args: unknown[]) => {
      const n = ++counts[site];
      const round = { hold: decide(site, n), injected: false, site, n } as NonNullable<typeof current>;
      current = round;
      if (round.hold) { subject.codexBoundaryDeadlineMs = LAUNCH_MS; clock.arm(); }
      try { return await orig(...args); }
      catch (error) { round.release?.(); throw error; }
      finally { subject.codexBoundaryDeadlineMs = defaultMs; current = undefined; }
    });
  }
  const gitMocks: Array<{ mock: { restore(): void } }> = [];
  for (const method of ["readRemediationRestrictionFacts", "readRuntimeRemediationRecovery"] as const) {
    const orig = (git[method] as (...args: any[]) => Promise<any>).bind(git);
    gitMocks.push(t.mock.method(git, method, async (...args: any[]) => {
      const round = current;
      if (round) opts.onRead?.(round.site, round.n);
      if (round?.hold && round.hold.method === method && !round.injected) {
        round.injected = true;
        if (round.hold.mode !== "existing") {
          const entered = latch(), until = latch();
          holders.push(hold(args[0], entered, until.promise));
          await entered.promise;
          if (round.hold.mode === "new_release_on_abort") round.release = until.release; else releases.push(until.release);
        }
        setImmediate(() => clock.fire(true, opts.nestedTimer ? 1 : 0));
      }
      return orig(...args);
    }));
  }
  // Pass-through spies on the provider reap and custody settle that reportGenericFailure skips under keepCustody.
  const custody = { reaps: 0, settles: 0 };
  for (const [name, key] of [["reapRecoveryProviderForSettle", "reaps"], ["reapThenSettleRecoveryGeneration", "settles"],
    ["settleRecoveryGeneration", "settles"]] as const) {
    const orig = subject[name].bind(subject);
    t.mock.method(subject, name, async (...args: unknown[]) => { custody[key]++; return orig(...args); });
  }
  const retry = subject.withLaunchLockRetry.bind(subject);
  t.mock.method(subject, "withLaunchLockRetry", async (...args: any[]) => {
    if (flight !== args[0]) {
      flight = args[0];
      const warn = flight.runLog.warn.bind(flight.runLog);
      t.mock.method(flight.runLog, "warn", (...a: unknown[]) => { diagnostics.push(a); return warn(...a); });
    }
    try { return await retry(...args); }
    catch (error) { for (const release of releases) release(); throw error; }
  });
  if (opts.onBackoff) {
    const wait = subject.waitRecoveryRetry.bind(subject);
    let backoffs = 0;
    t.mock.method(subject, "waitRecoveryRetry", async (...args: any[]) => {
      if (args[0] === flight && flight.launchLockRetries >= 1 && backoffs++ === 0) await opts.onBackoff!(flight, runner);
      return wait(...args);
    });
  }
  try { await runner.execute(claim); }
  finally {
    for (const release of releases) release();
    await Promise.allSettled(holders);
    runner.shutdown();
    // Restore the globals in reverse install order, so a later driveExecute starts from unwrapped globals.
    for (const mocked of gitMocks.reverse()) mocked.mock.restore();
    clock.restore();
  }
  return { claim, flight, runs, atLaunch, counts, custody, runner, diagnostics: abortLines(diagnostics), raw: diagnostics, failed: failedReports(claim.run_id) };
}

for (const [method, label] of [["readRemediationRestrictionFacts", "restriction_facts"],
  ["readRuntimeRemediationRecovery", "runtime_restore"]] as const) {
  test("#4246 launch round retries a lock wait that expired before acquisition: " + label, async t => {
    const r = await driveExecute(t, label === "restriction_facts" ? 4246 : 4247,
      (site, round) => site === "launch" && round === 1 ? { method, mode: "new_release_on_abort" } : undefined);
    assert.equal(r.counts.launch, 2, "one expired round, then one fresh round");
    assert.equal(r.runs, 1, "the executor ran once");
    assert.deepEqual(r.failed, [], "the run did not fail at startup");
    assert.equal(r.atLaunch, undefined, "the clearable latch was lifted by the fresh round before launch");
    assert.equal(r.diagnostics.length, 1);
    assert.equal(r.diagnostics[0].label, label);
    assert.equal(r.diagnostics[0].abort, "deadline");
    assert.equal(typeof r.diagnostics[0].elapsed_ms, "number");
    assert.deepEqual(Object.keys(r.diagnostics[0]).sort(), ["abort", "elapsed_ms", "label"]);
  });
}

test("#4246 phaseClone round retries a lock wait that expired before acquisition", async t => {
  const r = await driveExecute(t, 4248,
    (site, round) => site === "clone" && round === 1 ? { method: "readRuntimeRemediationRecovery", mode: "new_release_on_abort" } : undefined);
  assert.equal(r.counts.clone, 2);
  assert.equal(r.counts.launch, 1);
  assert.equal(r.runs, 1);
  assert.deepEqual(r.failed, []);
  assert.equal(r.atLaunch, undefined);
  assert.deepEqual(r.diagnostics.map(d => [d.label, d.abort]), [["runtime_restore", "deadline"]]);
});

test("#4246 phaseClone exhaustion fails visibly after exactly three rounds and keeps the clone and custody", async t => {
  const r = await driveExecute(t, 4249, (site, round) => site === "clone"
    ? { method: "readRemediationRestrictionFacts", mode: round === 1 ? "new_until_end" : "existing" } : undefined);
  assert.equal(r.counts.clone, 3, "the first round plus the two allowed retries");
  assert.equal(r.counts.launch, 0);
  assert.equal(r.runs, 0, "no model turn");
  assert.equal(r.failed.length, 1, "exactly one failure report");
  assert.match(r.failed[0].failure_reason, /local repository lock busy after bounded retries; local work and custody retained/);
  assert.equal(r.failed[0].fail_origin, undefined);
  assert.deepEqual(r.diagnostics.map(d => [d.label, d.abort]), Array(3).fill(["restriction_facts", "deadline"]));
  assert.equal(r.flight.preserveRecoveryClone, true);
  assert.equal(r.flight.preserveSession, true);
  assert.equal(fs.existsSync(r.flight.worktreePath), true, "the clone is kept");
  assert.notEqual(command(r.flight.barePath, "config", "--get", "uzi-recovery." + r.flight.branch + ".clone"), "",
    "the recovery journal (custody) is kept");
  assert.deepEqual(r.custody, { reaps: 0, settles: 0 }, "keepCustody: no provider reap and no custody settle");
});

test("#4246 a launch round retries when only the nested restriction-facts timer fired and the outer scope is unaborted", async t => {
  const r = await driveExecute(t, 4264,
    (site, round) => site === "launch" && round === 1 ? { method: "readRemediationRestrictionFacts", mode: "new_release_on_abort" } : undefined,
    { nestedTimer: true });
  assert.equal(r.counts.launch, 2, "one expired round, then one fresh round");
  assert.equal(r.runs, 1, "the executor ran once");
  assert.deepEqual(r.failed, [], "the run did not fail at startup");
  assert.equal(r.diagnostics.length, 1);
  assert.equal(r.diagnostics[0].label, "restriction_facts");
  assert.equal(r.diagnostics[0].abort, "deadline");
});

test("#4246 setFlight(false) clears the launch-retry writer, other writers are kept", () => {
  const cleared = new FlightRetention();
  cleared.recoveryClone.setLaunchRetry(true);
  assert.equal(cleared.recoveryClone.value, true, "the launch-retry writer holds the flag");
  cleared.recoveryClone.setFlight(false);
  assert.equal(cleared.recoveryClone.value, false, "an explicit flight-level release supersedes the launch-retry writer");

  const kept = new FlightRetention();
  kept.recoveryClone.setLaunchRetry(true);
  kept.recoveryClone.setFlight(true);
  kept.recoveryClone.setLaunchRetry(false);
  assert.equal(kept.recoveryClone.value, true, "a flight-level hold survives the launch-retry release");
});

const cloneJournal = (flight: any) => command(flight.barePath, "config", "--get", "uzi-recovery." + flight.branch + ".clone");

test("#4246 a launch that recovers via a retry completes and retires its clone like an uncontended run", async t => {
  const base = await driveExecute(t, 4251, () => undefined);
  const retried = await driveExecute(t, 4252,
    (site, round) => site === "launch" && round === 1 ? { method: "readRemediationRestrictionFacts", mode: "new_release_on_abort" } : undefined);
  assert.equal(retried.counts.launch, 2, "one expired round, then one fresh round");
  assert.equal(retried.runs, 1);
  assert.deepEqual(retried.failed, []);
  assert.equal(retried.flight.remediationRestrictions, undefined);
  assert.equal(retried.flight.preserveRecoveryClone, base.flight.preserveRecoveryClone);
  assert.equal(retried.flight.preserveRecoveryClone, false, "the failed round's retention was undone");
  assert.equal(fs.existsSync(retried.flight.worktreePath), fs.existsSync(base.flight.worktreePath));
  assert.equal(fs.existsSync(retried.flight.worktreePath), false, "the clone is retired");
  assert.equal(cloneJournal(retried.flight), cloneJournal(base.flight));
  assert.equal(cloneJournal(retried.flight), "", "the recovery journal is cleared");
});

test("#4246 the retry allowance is shared between the clone and launch sites", async t => {
  const r = await driveExecute(t, 4250, (site, round) => {
    if (site === "clone") return round === 1 ? { method: "readRemediationRestrictionFacts", mode: "new_release_on_abort" } : undefined;
    return { method: "readRemediationRestrictionFacts", mode: round === 1 ? "new_until_end" : "existing" };
  });
  assert.equal(r.counts.clone, 2, "phaseClone spent one retry");
  assert.equal(r.counts.launch, 2, "the launch site had only two rounds left");
  assert.equal(r.runs, 0);
  assert.equal(r.failed.length, 1);
  assert.match(r.failed[0].failure_reason, /local repository lock busy/);
  assert.equal(r.failed[0].fail_origin, undefined);
  assert.equal(r.flight.launchLockRetries, 2);
  assert.equal(r.flight.retainedDiscoveryRetries, undefined, "separate from the discovery allowance");
  assert.deepEqual(r.custody, { reaps: 0, settles: 0 }, "keepCustody: no provider reap and no custody settle");
});

type DirectStop = "cancel" | "pause" | "credential_switch" | "shutdown" | "claim_fence";
function applyStop(runner: RunRunner, flight: any, stop: DirectStop): void {
  const subject = runner as any;
  if (stop === "cancel") flight.cancel.abort(new Error("controlled cancellation"));
  else if (stop === "pause") { flight.cancel.abort(new PauseNowSignal()); flight.steering.abortLifecycle(new PauseNowSignal(), false); }
  else if (stop === "credential_switch") flight.steering.abortLifecycle(new CredentialSwitchSignal(), false);
  else if (stop === "shutdown") { subject.shuttingDownGlobal = true; subject.shutdownSignal.abort(); }
  else flight.steering.fence = "released";
}

/**
 * The retry helper around one real launch round on a fixture flight. A holder of the real bare lock is
 * injected at the chosen read; `during` and `expiry` then race the queued wait.
 */
async function directLaunch(t: any, f: Awaited<ReturnType<typeof fixture>>, opts: {
  at: "facts" | "restore";
  /** "just-before" freezes Date.now at the live round's deadlineAt - 10 (inside the former 20 ms slack
   *  window, before the deadline) and then fires the round's own operation timer. */
  expiry?: "elapsed" | "early" | "just-before" | "none";
  during?: (flight: any, runner: RunRunner) => void;
  afterFirstFailure?: (flight: any, runner: RunRunner) => void;
  preset?: (flight: any) => void;
  /** Drop the holder as soon as the first round fails, so the fresh round reads an uncontended lock. */
  releaseAfterFirstFailure?: boolean;
  budget?: boolean;
  retryMs?: number;
  breakRead?: boolean;
  /** The real read acquires the lock, then its cause is forced to pre_read_unavailable and it throws. */
  lateUnavailable?: boolean;
}) {
  const { cache, runner, subject, flight } = f.fresh();
  flight.executor = {};
  opts.preset?.(flight);
  if (opts.retryMs !== undefined) subject.recoveryRetryMs = opts.retryMs;
  subject.codexBoundaryDeadlineMs = LAUNCH_MS;
  const clock = launchClock(t);
  const diagnostics: unknown[][] = [];
  t.mock.method(flight.runLog, "warn", (...a: unknown[]) => { diagnostics.push(a); });
  const entered = latch(), release = latch();
  const hold = AsyncLocalStorage.bind(() => cache.withBareLock(f.bare, async () => { entered.release(); await release.promise; }));
  let holder: Promise<void> | undefined, rounds = 0, wall = 0;
  const budget = new MaintenanceBudget(1000, () => wall);
  const method = opts.at === "facts" ? "readRemediationRestrictionFacts" : "readRuntimeRemediationRecovery";
  const orig = (cache[method] as (...args: any[]) => Promise<any>).bind(cache);
  let captured: any, liveRound: any, frozenAt: number | undefined;
  if (opts.lateUnavailable) {
    const withProgress = (cache as any).withRemediationRestrictionProgress.bind(cache);
    t.mock.method(cache as any, "withRemediationRestrictionProgress", (progress: any, action: any) => {
      captured = progress; return withProgress(progress, action);
    });
  }
  t.mock.method(cache, method, async (...args: any[]) => {
    if (opts.lateUnavailable) {
      const progress = opts.at === "facts" ? captured : args[4];
      await orig(...args);
      assert.equal(!!progress.started || !!progress.acquired, true, "the real read reached the lock");
      progress.cause = "pre_read_unavailable";
      throw new Error("late-unavailable-sentinel");
    }
    if (rounds === 1 && !holder && !opts.breakRead) {
      holder = hold(); await entered.promise;
      setImmediate(() => {
        opts.during?.(flight, runner);
        if (opts.budget) { wall = 5000; try { budget.check(); } catch { /* the abort is the point */ } }
        if (opts.expiry === "just-before") {
          assert.equal(typeof liveRound?.deadlineAt, "number", "the live round recorded its deadline");
          frozenAt = liveRound.deadlineAt - 10;
          clock.freeze(frozenAt!);
          clock.fire(false);
        } else if (opts.expiry !== "none" && !opts.budget) clock.fire((opts.expiry ?? "elapsed") === "elapsed");
      });
    }
    return orig(...args);
  });
  if (opts.breakRead) t.mock.method(cache as any, "readRecoveryCaptureWithRaw", async () => { throw new Error("private-read-sentinel"); });
  const round = subject.requireLaunchAuthority.bind(subject);
  let firstError: unknown;
  t.mock.method(subject, "requireLaunchAuthority", async (...args: unknown[]) => {
    rounds++; clock.arm();
    if (rounds === 1) liveRound = args[1];
    try { return await round(...args); }
    catch (error) {
      if (rounds === 1) {
        firstError = error;
        if (opts.releaseAfterFirstFailure) release.release();
        opts.afterFirstFailure?.(flight, runner);
      }
      throw error;
    }
  });
  let error: any;
  try {
    const run = () => subject.withLaunchLockRetry(flight, (r: unknown) => subject.requireLaunchAuthority(flight, r));
    await (opts.budget ? MaintenanceBudget.run(budget, run) : run());
  } catch (e) { error = e; }
  finally { release.release(); await holder; subject.codexBoundaryDeadlineMs = 5000; clock.restore(); }
  return { error, firstError, rounds, flight, runner, subject, cache, diagnostics: abortLines(diagnostics), all: diagnostics,
    deadlineAt: liveRound?.deadlineAt as number | undefined, frozenAt };
}

const ORIGINAL = { facts: /remediation restriction facts unavailable/, restore: /lock wait aborted/ };

for (const at of ["facts", "restore"] as const) {
  for (const stop of ["cancel", "pause", "credential_switch", "shutdown", "claim_fence"] as const) {
    for (const phase of ["during the wait", "during backoff"] as const) {
      test("#4246 no retry when " + stop + " lands " + phase + " (" + at + ")", async t => {
        const f = await fixture(false);
        const during = phase === "during the wait";
        const r = await directLaunch(t, f, {
          at, retryMs: during ? undefined : 300,
          expiry: "elapsed",
          during: during ? (flight, runner) => applyStop(runner, flight, stop) : undefined,
          afterFirstFailure: during ? undefined : (flight, runner) => { setTimeout(() => applyStop(runner, flight, stop), 20); },
        });
        try {
          assert.ok(r.error, "the stop's own control is thrown");
          if (stop === "pause") assert.ok(r.error instanceof PauseNowSignal, "pause surfaces PauseNowSignal");
          else if (stop === "credential_switch") assert.ok(r.error instanceof CredentialSwitchSignal, "switch surfaces CredentialSwitchSignal");
          else if (stop === "claim_fence") assert.ok(r.error instanceof StaleClaimError, "a fence surfaces StaleClaimError");
          else if (stop === "shutdown") assert.equal(r.error.message, "worker shutdown");
          else {
            assert.equal(r.error, r.flight.cancel.signal.reason, "cancel surfaces its own reason");
            assert.equal(r.error.message, "controlled cancellation");
          }
          assert.doesNotMatch(String(r.error.message), ORIGINAL[at], "not the lock-read failure");
          assert.equal(r.rounds, 1, "no fresh round");
          assert.equal(r.diagnostics.length, 1, "exactly one diagnostic");
          assert.equal(r.diagnostics[0].label, at === "facts" ? "restriction_facts" : "runtime_restore");
          assert.equal(r.diagnostics[0].abort, during ? stop : "deadline");
          if (!during) assert.equal(r.flight.launchLockRetries, 1, "the backoff was entered and then interrupted");
        } finally { r.runner.shutdown(); }
      });
    }
  }
}

// A committed cancel (steering's real commitCancel: sticky cancelled flag plus the cancel and terminal
// aborts) must surface "run cancelled", guarding the cancel-before-terminal precedence in launchControl.
for (const at of ["facts", "restore"] as const) {
  for (const phase of ["during the wait", "during backoff"] as const) {
    test("#4246 a committed cancel surfaces run cancelled " + phase + " (" + at + ")", async t => {
      const f = await fixture(false);
      const during = phase === "during the wait";
      const commit = (flight: any) => (flight.steering as any).commitCancel();
      const r = await directLaunch(t, f, {
        at, retryMs: during ? undefined : 300, expiry: "elapsed",
        during: during ? commit : undefined,
        afterFirstFailure: during ? undefined : flight => { setTimeout(() => commit(flight), 20); },
      });
      try {
        assert.ok(r.error, "the cancel's own control is thrown");
        assert.equal(r.error.message, "run cancelled");
        assert.equal(r.flight.steering.isCancelled(), true);
        assert.equal(r.rounds, 1, "no fresh round");
        assert.doesNotMatch(String(r.error.message), ORIGINAL[at], "not the lock-read failure");
      } finally { r.runner.shutdown(); }
    });
  }
}

// Retention: a successful retry releases only what the failed round itself latched.
for (const at of ["facts", "restore"] as const) {
  test("#4246 a plain recovered retry leaves no clone retention behind (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, releaseAfterFirstFailure: true });
    try {
      assert.equal(r.error, undefined, "the fresh round succeeded");
      assert.equal(r.rounds, 2);
      assert.equal(r.flight.remediationRestrictions, undefined, "the clearable latch was lifted");
      assert.equal(r.flight.preserveRecoveryClone, false);
    } finally { r.runner.shutdown(); }
  });

  test("#4246 a retention set before the retry survives a recovered retry (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, releaseAfterFirstFailure: true, preset: flight => { flight.preserveRecoveryClone = true; } });
    try {
      assert.equal(r.error, undefined);
      assert.equal(r.rounds, 2);
      assert.equal(r.flight.remediationRestrictions, undefined, "the latch cleared");
      assert.equal(r.flight.preserveRecoveryClone, true, "another path's retention is not undone");
    } finally { r.runner.shutdown(); }
  });

  test("#4246 a retention set during the backoff survives a recovered retry (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, {
      at, releaseAfterFirstFailure: true, retryMs: 300,
      afterFirstFailure: flight => { setTimeout(() => { flight.preserveRecoveryClone = true; }, 20); },
    });
    try {
      assert.equal(r.error, undefined);
      assert.equal(r.rounds, 2);
      assert.equal(r.flight.remediationRestrictions, undefined, "the latch cleared");
      assert.equal(r.flight.preserveRecoveryClone, true, "a retention taken while waiting is not undone");
    } finally { r.runner.shutdown(); }
  });
}

// Control surfacing: an unrecognized or terminal stop around a lock-wait abort.
for (const at of ["facts", "restore"] as const) {
  for (const phase of ["during the wait", "during backoff"] as const) {
    const during = phase === "during the wait";
    const timing = (apply: (flight: any) => void) => ({
      at, retryMs: during ? undefined : 300, expiry: "elapsed" as const,
      during: during ? apply : undefined,
      afterFirstFailure: during ? undefined : (flight: any) => { setTimeout(() => apply(flight), 20); },
    });

    test("#4246 an unrecognized lifecycle abort rethrows the original lock-read error " + phase + " (" + at + ")", async t => {
      const f = await fixture(false);
      const r = await directLaunch(t, f, timing(flight => flight.steering.abortLifecycle(new Error("unrecognized"), false)));
      try {
        assert.ok(r.firstError, "the first round failed");
        assert.equal(r.error, r.firstError, "the original error object is rethrown unchanged");
        assert.equal(r.rounds, 1);
        assert.equal(r.diagnostics.length, 1);
        assert.equal(r.diagnostics[0].abort, during ? "unknown" : "deadline");
      } finally { r.runner.shutdown(); }
    });

    test("#4246 a terminal abort surfaces its own reason " + phase + " (" + at + ")", async t => {
      const f = await fixture(false);
      const sentinel = new Error("terminal-sentinel");
      const r = await directLaunch(t, f, timing(flight => flight.steering.abortLifecycle(sentinel, true)));
      try {
        assert.equal(r.error, sentinel);
        assert.equal(r.rounds, 1);
        assert.equal(r.diagnostics[0].abort, during ? "terminal" : "deadline");
      } finally { r.runner.shutdown(); }
    });
  }
}

// Execute level: a pause or credential switch landing in the backoff takes its normal control path.
for (const stop of ["pause", "credential_switch"] as const) {
  test("#4246 an owner " + stop + " arriving during the launch backoff ends in its control handling, not a failure", async t => {
    const r = await driveExecute(t, stop === "pause" ? 4261 : 4262,
      (site, round) => site === "launch" && round === 1 ? { method: "readRemediationRestrictionFacts", mode: "new_release_on_abort" } : undefined,
      { onBackoff: (flight, runner) => applyStop(runner, flight, stop) });
    assert.equal(r.counts.launch, 1, "no fresh round after the stop");
    assert.equal(r.runs, 0, "no model turn");
    assert.deepEqual(r.failed.map(s => s.failure_reason), [], "not a failed / agent_failure report");
    const seen = api.states.filter(s => s.runId === r.claim.run_id).map(s => String(s.body.status));
    // The PauseNowSignal arm runs handlePausePark: it reports "paused" on a park, or "pause_failed" and leaves
    // the run running when no checkpoint exists yet (this pre-launch case). Either way it is never "failed".
    if (stop === "pause") assert.ok(seen.includes("paused") || seen.includes("pause_failed"), "the pause was handled: " + seen.join(","));
    else assert.ok(seen.some(s => s.startsWith("credential_switch")), "the switch was handled: " + seen.join(","));
  });
}

// A real authority runtime around the retry: every round is its own transaction and none spans the backoff.
test("#4246 an authority-backed retry takes a fresh transaction per round and holds none across the backoff", async t => {
  const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
  const authority = new AuthorityRuntime({ lifetime, workerToken: "fixture-worker", timeoutMs: 5000 });
  const roundTransactions = new Map<number, unknown>();
  let backoffTransaction: unknown = "unset", independentTransaction = false;
  const r = await driveExecute(t, 4263,
    (site, round) => site === "launch" && round === 1 ? { method: "readRemediationRestrictionFacts", mode: "new_release_on_abort" } : undefined,
    {
      extra: { authority },
      onRead: (site, round) => { if (site === "launch" && !roundTransactions.has(round)) roundTransactions.set(round, activeAuthorityTransaction(lifetime)); },
      onBackoff: async () => {
        backoffTransaction = activeAuthorityTransaction(lifetime);
        await authority.withTransaction(async () => {});
        independentTransaction = true;
      },
    });
  assert.equal(r.counts.launch, 2);
  assert.deepEqual([...roundTransactions.keys()], [1, 2]);
  const [first, second] = [roundTransactions.get(1), roundTransactions.get(2)];
  assert.ok(first, "round 1 read inside an authority transaction");
  assert.ok(second, "round 2 read inside an authority transaction");
  assert.notEqual(first, second, "a different transaction per round");
  assert.equal(backoffTransaction, undefined, "no transaction is held across the backoff");
  assert.equal(independentTransaction, true, "an independent transaction completed during the backoff");
  assert.equal(r.runs, 1, "the executor ran once");
  assert.deepEqual(r.failed, []);
});

for (const at of ["facts", "restore"] as const) {
  test("#4246 an unidentified abort is classified unknown and not retried (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, budget: true });
    try {
      assert.ok(r.error);
      assert.equal(r.rounds, 1);
      assert.equal(r.flight.launchLockRetries, undefined);
      assert.deepEqual(r.diagnostics.map(d => d.abort), ["unknown"]);
    } finally { r.runner.shutdown(); }
  });

  test("#4246 an early timer notification before the deadline elapsed is unknown and not retried (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, expiry: "early" });
    try {
      assert.ok(r.error);
      assert.match(String(r.error.message), ORIGINAL[at]);
      assert.equal(r.rounds, 1);
      assert.equal(r.flight.launchLockRetries, undefined);
      assert.deepEqual(r.diagnostics.map(d => [d.label, d.abort]), [[at === "facts" ? "restriction_facts" : "runtime_restore", "unknown"]]);
    } finally { r.runner.shutdown(); }
  });

  // Pins the exact deadline predicate: an own-timer abort observed 10 ms before the round's deadline (inside
  // the window a former 20 ms tolerance accepted) is unknown and fails closed, never retried.
  test("#4246 an abort 10 ms before the round's deadline is unknown and not retried (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, expiry: "just-before" });
    try {
      assert.equal(typeof r.deadlineAt, "number");
      assert.equal(r.frozenAt, r.deadlineAt! - 10, "Date.now frozen at the live round's deadline - 10");
      assert.ok(r.frozenAt! < r.deadlineAt! && r.frozenAt! >= r.deadlineAt! - 20, "inside the former slack window");
      assert.ok(r.error);
      assert.match(String(r.error.message), ORIGINAL[at]);
      assert.equal(r.error, r.firstError, "the original round error is rethrown unchanged");
      assert.equal(r.rounds, 1, "no fresh round");
      assert.equal(r.flight.launchLockRetries, undefined);
      assert.deepEqual(r.diagnostics.map(d => [d.label, d.abort]), [[at === "facts" ? "restriction_facts" : "runtime_restore", "unknown"]]);
    } finally { r.runner.shutdown(); }
  });

  test("#4246 a pre-existing sticky latch is never retried or lifted (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, preset: flight => { flight.remediationRestrictions = { roots: [], clearable: false }; } });
    try {
      assert.ok(r.error);
      assert.equal(r.rounds, 1);
      assert.equal(r.flight.remediationRestrictions.clearable, false);
      assert.deepEqual(r.diagnostics.map(d => d.abort), ["deadline"], "the abort is still diagnosed once");
    } finally { r.runner.shutdown(); }
  });

  test("#4246 a successor capture is never retried (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, preset: flight => { flight.predecessorCapture = true; } });
    try {
      assert.ok(r.error);
      assert.equal(r.rounds, 1);
      assert.equal(r.flight.remediationRestrictions.clearable, false);
      assert.equal(r.flight.launchLockRetries, undefined);
      assert.deepEqual(r.diagnostics.map(d => [d.label, d.abort]), [[at === "facts" ? "restriction_facts" : "runtime_restore", "deadline"]]);
    } finally { r.runner.shutdown(); }
  });
}

test("#4246 a denial landing during the real restore lock wait stays sticky and is not retried", async t => {
  const f = await fixture(false);
  const r = await directLaunch(t, f, { at: "restore", during: flight => {
    (flight.remediationRestrictions ??= { roots: [] }).clearable = false;
    flight.remediationDenialRevision = (flight.remediationDenialRevision ?? 0) + 1;
  } });
  try {
    assert.ok(r.error);
    assert.equal(r.rounds, 1, "no fresh round and no launch");
    assert.equal(r.flight.remediationRestrictions.clearable, false, "the concurrent denial is not made clearable");
    assert.deepEqual(r.diagnostics.map(d => [d.label, d.abort]), [["runtime_restore", "deadline"]]);
  } finally { r.runner.shutdown(); }
});

for (const at of ["facts", "restore"] as const) {
  test("#4246 a read that acquired the lock stays sticky even if its cause is forced to pre_read_unavailable (" + at + ")", async t => {
    const f = await fixture(false);
    const r = await directLaunch(t, f, { at, lateUnavailable: true });
    try {
      assert.ok(r.error);
      assert.match(String(r.error.message), /late-unavailable-sentinel|unavailable/);
      assert.equal(r.rounds, 1, "no retry");
      assert.ok(r.flight.remediationRestrictions, "a latch is held");
      assert.equal(r.flight.remediationRestrictions.clearable, false, "the latch is sticky");
      assert.deepEqual(r.diagnostics, [], "no lock-wait diagnostic");
      assert.equal(r.flight.launchLockRetries, undefined);
    } finally { r.runner.shutdown(); }
  });
}

test("#4246 a restore failure after the lock was acquired is sticky and carries no lock-wait diagnostic", async t => {
  const f = await fixture(false);
  const r = await directLaunch(t, f, { at: "restore", breakRead: true });
  try {
    assert.ok(r.error);
    assert.equal(r.rounds, 1);
    assert.equal(r.flight.remediationRestrictions.clearable, false);
    assert.deepEqual(r.diagnostics, []);
    assert.equal(r.flight.launchLockRetries, undefined);
    assert.equal(JSON.stringify(r.all).includes("private-read-sentinel"), false);
  } finally { r.runner.shutdown(); }
});

test("#4246 the lock-wait diagnostic carries no path, ref or message", async t => {
  const f = await fixture(false);
  const r = await directLaunch(t, f, { at: "facts", expiry: "elapsed" });
  try {
    const text = JSON.stringify(r.all);
    for (const secret of [f.bare, BRANCH, f.base, "boundary deadline"]) assert.equal(text.includes(secret), false, secret);
  } finally { r.runner.shutdown(); }
});

for (const enrolled of [false, true]) {
  test("#4246 teardown revalidation of a restore-site latch: " + (enrolled ? "enrolled journal stays held" : "clearable latch lifts only on a fresh unenrolled read"), async t => {
    const f = await sourceImportFixture(enrolled);
    f.subject.codexBoundaryDeadlineMs = LAUNCH_MS;
    const clock = launchClock(t);
    const entered = latch(), release = latch();
    const hold = AsyncLocalStorage.bind(() => f.cache.withBareLock(f.bare, async () => { entered.release(); await release.promise; }));
    let holder: Promise<void> | undefined;
    const orig = f.cache.readRuntimeRemediationRecovery.bind(f.cache);
    t.mock.method(f.cache, "readRuntimeRemediationRecovery", async (...args: Parameters<typeof orig>) => {
      if (!holder) {
        holder = hold(); await entered.promise;
        setImmediate(() => clock.fire(true));
      }
      return orig(...args);
    });
    const round: any = { startedAt: Date.now() };
    clock.arm();
    try {
      await assert.rejects(f.subject.requireLaunchAuthority(f.flight, round));
    } finally { release.release(); await holder; f.subject.codexBoundaryDeadlineMs = 5000; }
    try {
      assert.equal(round.lockWait, "runtime_restore");
      assert.ok(f.flight.remediationRestrictions, "a latch is left at the end of the flight");
      assert.equal(f.flight.remediationRestrictions.clearable, !enrolled);
      let published = 0;
      t.mock.method(f.subject, "publishFlightSource", async () => { published++; return "published"; });
      const facts = t.mock.method(f.cache, "readRemediationRestrictionFacts");
      f.flight.completionSourceBound = { clonePath: f.clone.path };
      f.flight.sourceTailsJoined = true;
      const end = await f.subject.endFlightSourceUse(f.flight, nullLogger());
      if (enrolled) {
        assert.ok(f.flight.remediationRestrictions, "latch kept");
        assert.equal(published, 0, "no publication under an enrolled journal");
        assert.deepEqual(end.endedSlots, []);
      } else {
        assert.ok(facts.mock.callCount() >= 1, "a fresh facts read was made");
        assert.equal(f.flight.remediationRestrictions, undefined, "verified absence cleared the latch");
        assert.equal(published, 1);
      }
    } finally { f.runner.shutdown(); }
  });
}
