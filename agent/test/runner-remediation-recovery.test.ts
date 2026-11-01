import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { formatAttemptId } from "../src/attempt-path.js";
import type { CheckpointRange, RunnerClone } from "../src/git.js";
import type { SecretFinding } from "../src/secret-scan-guard.js";
import type { FrozenHeldPolicy } from "../src/frozen-held.js";
import { prepareFrozenHeld } from "../src/frozen-held.js";
import { noProofReseed, nullLogger } from "./helpers.js";
import { fakeGitlab, fx, git, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const BRANCH = "agent/issue-3747";
const KEY = "issue-3747";
const RUN = "run-3747";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: "pipe" }).trim();
}
type Flight = Record<string, unknown> & {
  remediationRestrictions?: {
    roots: string[];
    pending?: {
      runId: string; clonePath?: string; attemptId?: string; barePath?: string; branch?: string;
      requiredOIDs: string[]; requireDenial: boolean; bindingUncertain: boolean;
    };
  };
};
type Subject = {
  persistRemediationScan(flight: Flight, bare: string, branch: string, tip: string,
    findings: Array<{ commit?: string }>, origin?: "pre-exit"): Promise<void>;
  requireLaunchAuthority(flight: Flight): Promise<void>;
  checkpointRemediationHold(flight: Flight, bare: string, candidate: string, reap: boolean): Promise<boolean>;
  frozenHeldPolicy(flight: Flight): FrozenHeldPolicy;
  persistRemediationRestrictions(flight: Flight, bare: string, branch: string, tip: string,
    findings: SecretFinding[]): Promise<void>;
  runSecretRemediationGate(flight: Flight, bare: string, clone: RunnerClone): Promise<{ action: string }>;
  fetchBackBestEffort(...args: unknown[]): Promise<{ kind: "updated" }>;
  scanRemediationRange(...args: unknown[]): Promise<{
    trusted: true; range: CheckpointRange; findings: SecretFinding[]; floorCandidates: string[];
  }>;
  withholdPathsFor(findings: SecretFinding[]): Promise<boolean>;
  checkpointTestHooks: { flaggedCommitsCap?: number };
  recordFlaggedCommits(state: unknown, findings: SecretFinding[], log: unknown): void;
  scanCheckpointForPublish(flight: Flight, bare: string, branch: string): Promise<{ kind: string }>;
};
function freshRunner(): Subject {
  return runnerWith(() => ({ homeDir, executor: { run: async () => { throw new Error("unused executor"); } } }),
    fakeGitlab().gitlab) as unknown as Subject;
}
async function fixture() {
  const bare = await git.ensureClone(fx.originPath);
  const base = command(bare, "rev-parse", "refs/remotes/origin/main");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, BRANCH, RUN, ATTEMPT);
  const source = { runId: RUN, clonePath: clone.path, attemptId: ATTEMPT };
  const fetch = () => git.fetchAgentBranch(bare, clone.path, BRANCH, RUN, {
    context: { runId: RUN, generation: 1, kind: "issue", branch: BRANCH, barePath: bare,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } }, producingSource: source,
  });
  const commit = (label: string, parent = base) => {
    const tip = command(clone.path, "-c", "user.name=t", "-c", "user.email=t@example.com",
      "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
    command(clone.path, "update-ref", "refs/heads/" + BRANCH, tip);
    return tip;
  };
  const freshFlight = (): Flight => ({ runId: RUN, branch: BRANCH, barePath: bare, worktreePath: clone.path,
    runnerClone: clone, runLog: nullLogger(), batcher: { emit: () => {} }, claimGenerations: new Map(),
    reportedPublishOutcomes: new Set(),
    cancel: new AbortController(), steering: { lifecycleSignal: () => new AbortController().signal } });
  await fetch();
  const enroll = async () => {
    const flagged = commit("flagged");
    assert.equal((await fetch()).kind, "updated");
    await git.recordRemediationScan(bare, BRANCH, source,
      { trusted: true, tip: flagged, flaggedOIDs: [flagged], origin: "pre-exit" });
    return flagged;
  };
  return { bare, base, clone, source, fetch, commit, freshFlight, enroll };
}

test("fresh runner restores flagged and displaced roots, admits only confirmed clean source, and freezes a publication hold", async t => {
  const f = await fixture();
  const flagged = await f.enroll();
  const displaced = f.commit("displaced", flagged);
  assert.equal((await f.fetch()).kind, "updated");
  const clean = f.commit("clean");
  assert.equal((await f.fetch()).kind, "updated");
  await git.recordRemediationScan(f.bare, BRANCH, f.source,
    { trusted: true, tip: clean, flaggedOIDs: [] });
  let scans = 0;
  t.mock.method(git, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
    scans++;
    assert.equal(range.tipSha, clean, "scan the actual selected source");
    return { trusted: true, findings: [] };
  });
  const runner = freshRunner();
  const flight = f.freshFlight();
  await runner.requireLaunchAuthority(flight);
  assert.equal(scans, 1, "workers without authority still scan enrolled source");
  assert.deepEqual(new Set(flight.remediationRestrictions?.roots), new Set([flagged, displaced]));
  assert.equal(await runner.checkpointRemediationHold(flight, f.bare, clean, true), true);
  const policy = runner.frozenHeldPolicy(flight);
  assert.equal(policy.blocked, true);
  assert.deepEqual(await prepareFrozenHeld({ git, barePath: f.bare, branch: BRANCH, sourceSha: clean,
    policy, deadlineMs: 10_000 }), { reason: "secret_remediation_pending" });
  // A floor that would reintroduce excluded history must remain a requirement.
  flight.publishedRealTips = [displaced];
  await assert.rejects(runner.requireLaunchAuthority(flight), /published floor conflict/);
  const safeFloor = command(f.bare, "-c", "user.name=t", "-c", "user.email=t@example.com",
    "commit-tree", f.base + "^{tree}", "-p", f.base, "-m", "safe unrelated published floor");
  flight.publishedRealTips = [safeFloor];
  await runner.requireLaunchAuthority(flight);
  flight.publishedRealTips = [];
  const ancestry = git.ancestry.bind(git);
  t.mock.method(git, "ancestry", async (bare: string, ancestor: string, descendant: string) =>
    ancestor === displaced && descendant === clean ? "unknown" : ancestry(bare, ancestor, descendant));
  await assert.rejects(runner.requireLaunchAuthority(flight), /history restricted/);
  // A later query failure cannot borrow the earlier in-memory confirmation.
  t.mock.method(git, "readRuntimeRemediationRecovery", async () => { throw new Error("remediation source evidence unconfirmed"); });
  await assert.rejects(runner.requireLaunchAuthority(flight), /unconfirmed/);
  t.mock.method(git, "readRuntimeRemediationRecovery", async () => undefined);
  await assert.rejects(runner.requireLaunchAuthority(flight), /enrollment disappeared/);
});

test("launch pending admission: old readable recovery cannot cover an unpersisted divergent finding", async t => {
  const f = await fixture();
  const q1 = await f.enroll();
  const clean = f.commit("clean selected source");
  assert.equal((await f.fetch()).kind, "updated");
  await git.recordRemediationScan(f.bare, BRANCH, f.source,
    { trusted: true, tip: clean, flaggedOIDs: [] });
  const old = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
  assert.equal(old?.ownedTip, clean);
  assert.equal(old?.active, undefined, "clean scan closed the rewrite grant");
  assert.deepEqual(old?.flaggedOIDs, [q1]);

  const runner = freshRunner();
  const flight = f.freshFlight();
  const ranges: CheckpointRange[] = [];
  t.mock.method(git, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
    ranges.push(range);
    return { trusted: true, findings: [] };
  });
  // Real recovery-source authentication and range resolution must succeed before the new hold.
  await runner.requireLaunchAuthority(flight);
  assert.equal(ranges.length, 1);
  assert.equal(ranges[0]?.tipSha, clean);
  assert.equal(ranges[0]?.excludeSha, f.base);

  const q2 = f.commit("finding in separate candidate");
  const candidate = f.commit("candidate containing second finding", q2);
  command(f.bare, "fetch", "--no-tags", f.clone.path, candidate);
  command(f.clone.path, "update-ref", "refs/heads/" + BRANCH, clean);
  assert.equal(await git.ancestry(f.bare, q2, candidate), "ancestor");
  assert.equal(await git.ancestry(f.bare, q2, clean), "divergent");
  const finding: SecretFinding = { commit: q2, file: "withheld", startLine: 1, ruleId: "unit" };
  const writer = t.mock.method(git, "recordRemediationRestrictions", async () => {
    throw new Error("config write failed");
  });
  await assert.rejects(runner.persistRemediationRestrictions(flight, f.bare, BRANCH, candidate, [finding]),
    /config write failed/);
  assert.equal(writer.mock.callCount(), 1);
  writer.mock.restore();
  const pending = flight.remediationRestrictions!.pending!;
  assert.deepEqual(pending.requiredOIDs, [q2]);
  assert.deepEqual(await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source), old,
    "OLD RR remains independently readable and does not contain Q2");
  await assert.rejects(runner.requireLaunchAuthority(flight), /remediation pending/);
  assert.equal(ranges.length, 1, "incomplete pending facts hold before a clean scan can excuse them");

  await runner.persistRemediationRestrictions(flight, f.bare, BRANCH, candidate, [finding]);
  const complete = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
  assert.deepEqual(complete?.flaggedOIDs, [q1, q2]);
  assert.equal(complete?.ownedTip, clean);
  assert.equal(complete?.active, undefined, "negative persistence grants no authority to the candidate");
  await runner.requireLaunchAuthority(flight);
  assert.equal(ranges.length, 2, "complete durable pending facts allow the legitimate clean scan");
  assert.equal(ranges[1]?.tipSha, clean);
  assert.equal(flight.remediationRestrictions!.pending, pending, "admission never clears the latch");

  for (const key of ["runId", "clonePath", "attemptId", "barePath", "branch"] as const) {
    const original = pending[key];
    pending[key] = "different selected binding";
    await assert.rejects(runner.requireLaunchAuthority(flight), /remediation pending/, key);
    pending[key] = original!;
  }
  for (const key of ["requireDenial", "bindingUncertain"] as const) {
    pending[key] = true;
    await assert.rejects(runner.requireLaunchAuthority(flight), /remediation pending/, key);
    pending[key] = false;
  }
  assert.equal(ranges.length, 2, "binding and denial controls hold before scanning");
});

test("launch pending admission: finding latched during clean scan must refuse incomplete persistence", async t => {
  const f = await fixture();
  const q1 = await f.enroll();
  const clean = f.commit("clean selected source");
  assert.equal((await f.fetch()).kind, "updated");
  await git.recordRemediationScan(f.bare, BRANCH, f.source,
    { trusted: true, tip: clean, flaggedOIDs: [] });
  const old = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
  assert.equal(old?.ownedTip, clean);
  assert.equal(old?.active, undefined, "clean scan closed the rewrite grant");
  assert.deepEqual(old?.flaggedOIDs, [q1]);

  const q2 = f.commit("finding in separate candidate");
  const candidate = f.commit("candidate containing second finding", q2);
  command(f.bare, "fetch", "--no-tags", f.clone.path, candidate);
  command(f.clone.path, "update-ref", "refs/heads/" + BRANCH, clean);
  assert.equal(await git.ancestry(f.bare, q2, candidate), "ancestor");
  assert.equal(await git.ancestry(f.bare, q2, clean), "divergent");

  const runner = freshRunner();
  const flight = f.freshFlight();
  assert.equal((runner as unknown as { authority?: unknown }).authority, undefined,
    "exercise the supported launch path without an authority runtime");
  const events: string[] = [];
  let markEntered!: () => void;
  let releaseScan!: () => void;
  const entered = new Promise<void>(resolve => { markEntered = resolve; });
  const released = new Promise<void>(resolve => { releaseScan = resolve; });
  t.mock.method(git, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
    assert.equal(range.tipSha, clean);
    assert.equal(range.excludeSha, f.base);
    events.push("scan entered");
    markEntered();
    await released;
    events.push("scan returned clean");
    return { trusted: true, findings: [] };
  });
  const writer = t.mock.method(git, "recordRemediationRestrictions", async () => {
    events.push("negative write failed");
    throw new Error("config write failed");
  });
  const launch = runner.requireLaunchAuthority(flight);
  // Attach the expected rejection immediately, so a rejection cannot go unhandled.
  const refused = assert.rejects(launch, /remediation pending/,
    "launch must refuse facts latched during the scan until independently confirmed");
  try {
    await entered;
    const finding: SecretFinding = { commit: q2, file: "withheld", startLine: 1, ruleId: "unit" };
    // This callback runs outside launch's async context; the scanner only waits on release.
    await assert.rejects(runner.persistRemediationRestrictions(flight, f.bare, BRANCH, candidate, [finding]),
      /config write failed/);
    assert.equal(writer.mock.callCount(), 1);
    assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [q2]);
    assert.equal(flight.remediationRestrictions?.pending?.requireDenial, false);
    assert.equal(flight.remediationRestrictions?.pending?.bindingUncertain, false);
    assert.deepEqual(await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source), old,
      "OLD RR remains independently readable and does not contain Q2");
    assert.deepEqual(events, ["scan entered", "negative write failed"]);
    events.push("pending latched and OLD RR independently confirmed");
    events.push("scan released");
  } finally {
    releaseScan();
  }
  await refused;
  assert.deepEqual(events, ["scan entered", "negative write failed",
    "pending latched and OLD RR independently confirmed", "scan released", "scan returned clean"]);
  assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [q2]);
  assert.deepEqual(await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source), old,
    "launch refusal leaves OLD RR independently readable and the new finding pending");
});

for (const enrolled of [false, true]) {
  test(`launch final admission: ${enrolled ? "selected source drift" : "first pending enrollment"} during authority wait refuses`, async t => {
    const f = await fixture();
    if (enrolled) {
      await f.enroll();
      const clean = f.commit("clean selected source");
      assert.equal((await f.fetch()).kind, "updated");
      await git.recordRemediationScan(f.bare, BRANCH, f.source,
        { trusted: true, tip: clean, flaggedOIDs: [] });
    } else {
      assert.equal(await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source), undefined);
    }
    const scanner = t.mock.method(git, "secretScanCheckpointRange",
      async () => ({ trusted: true, findings: [] }));
    const runner = freshRunner();
    const flight = f.freshFlight();
    flight.executor = {};
    let markEntered!: () => void;
    let releaseWait!: () => void;
    const entered = new Promise<void>(resolve => { markEntered = resolve; });
    const released = new Promise<void>(resolve => { releaseWait = resolve; });
    const events: string[] = [];
    // Exercise the existing authority wait seam without adding a production test API.
    (runner as unknown as { authority: unknown }).authority = {
      withTransaction: (fn: () => Promise<unknown>) => fn(),
      recordAdmissions: (_paths: Set<string>, fn: () => Promise<unknown>) => fn(),
      assertLaunchable: async () => {
        events.push("authority entered");
        markEntered();
        await released;
        events.push("authority returned");
      },
    };
    const launch = runner.requireLaunchAuthority(flight);
    const refused = assert.rejects(launch,
      enrolled ? /remediation source identity unavailable/ : /remediation pending/);
    try {
      await entered;
      assert.equal(scanner.mock.callCount(), enrolled ? 1 : 0,
        "truly unenrolled admission does not scan");
      if (enrolled) {
        flight.runnerClone = { ...f.clone, attemptId: "different selected attempt" };
      } else {
        const q = f.commit("first finding during launch wait");
        t.mock.method(git, "recordRemediationRestrictions", async () => {
          throw new Error("config write failed");
        });
        await assert.rejects(runner.persistRemediationRestrictions(flight, f.bare, BRANCH, q,
          [{ commit: q, file: "withheld", startLine: 1, ruleId: "unit" }]), /config write failed/);
        assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [q]);
        assert.equal(await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source), undefined);
      }
      events.push("late change confirmed");
    } finally {
      releaseWait();
    }
    await refused;
    assert.deepEqual(events, ["authority entered", "late change confirmed", "authority returned"]);
  });
}

test("fresh enrolled flight refuses flagged history even when a trusted scan hides it behind a floor", async t => {
  const f = await fixture();
  const flagged = await f.enroll();
  t.mock.method(git, "secretScanCheckpointRange", async () => ({ trusted: true, findings: [] }));
  const runner = freshRunner();
  const flight = f.freshFlight();
  await assert.rejects(runner.requireLaunchAuthority(flight), /history restricted/);
  assert.deepEqual(flight.remediationRestrictions?.roots, [flagged]);
  assert.equal(await runner.checkpointRemediationHold(flight, f.bare, flagged, true), true);
});

test("enrolled but unconfirmed and malformed records hold rather than look absent", async t => {
  const f = await fixture();
  await f.enroll();
  t.mock.method(git, "readRuntimeRemediationRecovery", async () => { throw new Error("remediation source evidence unconfirmed"); });
  const runner = freshRunner();
  const flight = f.freshFlight();
  await assert.rejects(runner.requireLaunchAuthority(flight), /unconfirmed/);
  assert.equal(await runner.checkpointRemediationHold(flight, f.bare, f.base, true), true);
  command(f.bare, "config", "uzi-recovery." + BRANCH + ".clone", "{");
  const restarted = f.freshFlight();
  await assert.rejects(freshRunner().requireLaunchAuthority(restarted));
  assert.equal(await runner.checkpointRemediationHold(restarted, f.bare, f.base, true), true);
});

test("enrolled launch cannot borrow a missing selected attempt identity", async () => {
  const f = await fixture();
  await f.enroll();
  const flight = f.freshFlight();
  flight.runnerClone = { ...f.clone, attemptId: undefined };
  await assert.rejects(freshRunner().requireLaunchAuthority(flight), /source identity unavailable/);
  assert.equal(await freshRunner().checkpointRemediationHold(flight, f.bare, f.base, true), true);
});

test("ordinary unenrolled launch and checkpoint remain unscanned", async t => {
  const f = await fixture();
  t.mock.method(git, "secretScanCheckpointRange", async () => { throw new Error("unenrolled scan"); });
  const runner = freshRunner();
  const flight = f.freshFlight();
  await runner.requireLaunchAuthority(flight);
  assert.equal(flight.remediationRestrictions, undefined);
  assert.equal(await runner.checkpointRemediationHold(flight, f.bare, f.base, true), false);
});


test("pending persistence foundation: invalid findings latch denial before validation without retaining content", async t => {
  const f = await fixture();
  const producer = t.mock.method(git, "recordRemediationScan", async () => {});
  for (const malformed of [{}, { commit: 0 }, null]) {
    const flight = f.freshFlight();
    await assert.rejects(freshRunner().persistRemediationScan(flight, f.bare, BRANCH, f.base,
      [{ commit: f.base }, malformed] as Array<{ commit?: string }>, "pre-exit"));
    assert.deepEqual(flight.remediationRestrictions?.pending, {
      runId: RUN, clonePath: f.clone.path, attemptId: ATTEMPT, barePath: f.bare, branch: BRANCH,
      requiredOIDs: [f.base], requireDenial: true, bindingUncertain: false,
    });
  }
  assert.equal(producer.mock.callCount(), 0);
});

test("pending persistence foundation: config failure retains facts and refuses source rebinding", async t => {
  const f = await fixture();
  const flight = f.freshFlight();
  const runner = freshRunner();
  let writes = 0;
  t.mock.method(git, "recordRemediationScan", async () => {
    writes++;
    assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [f.base], "already latched at first effect");
    throw new Error("config write failed");
  });
  await assert.rejects(runner.persistRemediationScan(flight, f.bare, BRANCH, f.base,
    [{ commit: f.base }], "pre-exit"), /config write failed/);
  flight.runnerClone = { ...f.clone, attemptId: "another-attempt" };
  await assert.rejects(runner.persistRemediationScan(flight, f.bare, BRANCH, f.base,
    [{ commit: f.base }], "pre-exit"), /source identity unavailable/);
  assert.equal(writes, 1);
  assert.equal(flight.remediationRestrictions?.pending?.attemptId, ATTEMPT, "never rebound");
  assert.equal(flight.remediationRestrictions?.pending?.bindingUncertain, true);
});

test("pending persistence foundation: Q65 requires denial with a bounded Q snapshot", async t => {
  const f = await fixture();
  const flight = f.freshFlight();
  const findings = Array.from({ length: 65 }, (_, i) => ({ commit: (i + 1).toString(16).padStart(40, "0") }));
  t.mock.method(git, "recordRemediationScan", async () => { throw new Error("overflow denial sync failed"); });
  await assert.rejects(freshRunner().persistRemediationScan(flight, f.bare, BRANCH, f.base, findings, "pre-exit"),
    /overflow denial sync failed/);
  assert.equal(flight.remediationRestrictions?.pending?.requiredOIDs.length, 64);
  assert.equal(flight.remediationRestrictions?.pending?.requireDenial, true);
  assert.equal(flight.remediationRestrictions?.pending?.attemptId, ATTEMPT);
});

test("pending persistence foundation: selected missing identity holds while unenrolled legacy stays residual", async t => {
  const f = await fixture();
  const runner = freshRunner();
  const producer = t.mock.method(git, "recordRemediationScan", async () => {});
  const selected = f.freshFlight();
  selected.worktreePath = undefined;
  await assert.rejects(runner.persistRemediationScan(selected, f.bare, BRANCH, f.base, [{ commit: f.base }], "pre-exit"),
    /source identity unavailable/);
  assert.equal(selected.remediationRestrictions?.pending?.bindingUncertain, true);
  const enrolled = f.freshFlight();
  enrolled.runnerClone = undefined;
  enrolled.remediationRestrictions = { roots: [f.base] };
  await assert.rejects(runner.persistRemediationScan(enrolled, f.bare, BRANCH, f.base, [{ commit: f.base }], "pre-exit"),
    /source identity unavailable/);
  const legacy = f.freshFlight();
  legacy.runnerClone = undefined;
  await runner.persistRemediationScan(legacy, f.bare, BRANCH, f.base, [{ commit: f.base }], "pre-exit");
  assert.equal(legacy.remediationRestrictions, undefined);
  assert.equal(producer.mock.callCount(), 0);
});

// These private gate tests stub fetching/scanning only; enrollment, ownership receipts,
// negative writes and grant reads use the real GitCache and local Git objects.
for (const mode of ["floor", "exhausted", "overflow", "path failure", "remediate", "checkpoint authority"] as const) {
  test(`source finding unit: pre-exit ${mode} persists restrictions before decision`, async t => {
    const f = await fixture();
    let checkpointQ: string | undefined;
    if (mode === "checkpoint authority") {
      checkpointQ = f.commit("checkpoint finding");
      assert.equal((await f.fetch()).kind, "updated");
      await git.recordRemediationScan(f.bare, BRANCH, f.source,
        { trusted: true, tip: checkpointQ, flaggedOIDs: [checkpointQ], origin: "checkpoint-pending" });
    }
    const tip = f.commit("new trusted finding", checkpointQ ?? f.base);
    assert.equal((await f.fetch()).kind, "updated");
    const finding: SecretFinding = { commit: tip, file: "credential-shaped-filename", startLine: 1, ruleId: "unit" };
    const runner = freshRunner();
    const flight = f.freshFlight();
    flight.secretRemediation = { attempts: mode === "exhausted" ? 2 : 0 };
    const range: CheckpointRange = { tipSha: tip, excludeSha: f.base };
    t.mock.method(runner, "fetchBackBestEffort", async () => ({ kind: "updated" }));
    t.mock.method(runner, "scanRemediationRange", async () => ({
      trusted: true, range, findings: [finding], floorCandidates: mode === "floor" ? [tip] : [],
    }));
    if (mode === "overflow") runner.checkpointTestHooks = { flaggedCommitsCap: 0 };
    if (mode === "checkpoint authority") flight.secretRemediation = { attempts: 2 };
    const expectedFlags = checkpointQ ? [checkpointQ, tip] : [tip];
    const before = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
    let pathCalls = 0;
    let pathValidated = false;
    t.mock.method(runner, "withholdPathsFor", async () => {
      pathCalls++;
      const stored = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
      assert.deepEqual(stored?.flaggedOIDs, expectedFlags, "negative facts durable before path scanner");
      assert.deepEqual(stored?.active, before?.active, "negative write opens no grant");
      assert.equal((flight.secretRemediation as { withholdPaths: boolean }).withholdPaths, true);
      pathValidated = true;
      if (mode === "path failure") throw new Error("path scanner failed");
      return mode !== "remediate";
    });
    const decision = await runner.runSecretRemediationGate(flight, f.bare, f.clone);
    const stored = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
    assert.deepEqual(stored?.flaggedOIDs, expectedFlags);
    assert.equal(stored?.blocked, false, "ordinary early decisions do not poison enrollment");
    assert.equal(flight.preserveRecoveryClone, true);
    assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [tip]);
    assert.equal(decision.action, mode === "floor" ? "proceed" : mode === "remediate" ? "remediate" : "fail");
    assert.equal(pathCalls, mode === "overflow" ? 0 : 1);
    assert.equal(pathValidated, mode !== "overflow", "path scanner observed the complete durable negative record");
    if (mode === "remediate") {
      assert.equal(stored?.active?.origin, "pre-exit");
      assert.equal((flight.secretRemediation as { withholdPaths: boolean }).withholdPaths, false,
        "duplicate positive writer keeps the path scanner's decision");
    } else {
      assert.deepEqual(stored?.active, before?.active);
      assert.equal(stored?.sequence, before?.sequence ?? 0);
    }
    if (checkpointQ) {
      await runner.persistRemediationRestrictions(flight, f.bare, BRANCH, tip, []);
      const empty = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
      assert.deepEqual(empty, stored, "empty negative call neither closes nor changes existing authority");
    }
  });
}

test("source finding unit: launch and checkpoint latch trusted findings before effects", async t => {
  const f = await fixture();
  const first = await f.enroll();
  const tip = f.commit("later trusted finding", first);
  assert.equal((await f.fetch()).kind, "updated");
  const finding: SecretFinding = { commit: tip, file: "withheld", startLine: 1, ruleId: "unit" };
  t.mock.method(git, "secretScanCheckpointRange", async () => ({ trusted: true, findings: [finding] }));
  const runner = freshRunner();
  const launch = f.freshFlight();
  await assert.rejects(runner.requireLaunchAuthority(launch), /history unavailable/);
  const stored = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
  assert.deepEqual(new Set(stored?.flaggedOIDs), new Set([first, tip]));
  assert.equal(stored?.active?.origin, "pre-exit", "launch does not change authority");
  assert.deepEqual(stored?.active?.flaggedOIDs, [first]);

  const checkpoint = f.freshFlight();
  let collectorObservedLatch = false;
  const collect = runner.recordFlaggedCommits.bind(runner);
  t.mock.method(runner, "recordFlaggedCommits", (state: unknown, findings: SecretFinding[], log: unknown) => {
    assert.deepEqual(checkpoint.remediationRestrictions?.pending?.requiredOIDs, [tip]);
    collectorObservedLatch = true;
    collect(state, findings, log);
  });
  let writerObservedLatch = false;
  t.mock.method(git, "recordRemediationScan", async () => {
    assert.deepEqual(checkpoint.remediationRestrictions?.pending?.requiredOIDs, [tip]);
    assert.equal(checkpoint.preserveRecoveryClone, true);
    writerObservedLatch = true;
    throw new Error("positive sync failure");
  });
  assert.equal((await runner.scanCheckpointForPublish(checkpoint, f.bare, BRANCH)).kind, "blocked");
  assert.equal(collectorObservedLatch, true);
  assert.equal(writerObservedLatch, true);
});

test("source finding unit: failed first negative write and malformed findings retain custody and fail closed", async t => {
  const f = await fixture();
  const tip = f.commit("first finding");
  assert.equal((await f.fetch()).kind, "updated");
  const runner = freshRunner();
  t.mock.method(runner, "fetchBackBestEffort", async () => ({ kind: "updated" }));
  const finding: SecretFinding = { commit: tip, file: "never-rendered", startLine: 1, ruleId: "unit" };
  t.mock.method(runner, "scanRemediationRange", async () => ({
    trusted: true, range: { tipSha: tip, excludeSha: f.base }, findings: [finding], floorCandidates: [],
  }));
  const writer = t.mock.method(git, "recordRemediationRestrictions", async () => { throw new Error("config failure"); });
  const paths = t.mock.method(runner, "withholdPathsFor", async () => { throw new Error("must not scan paths"); });
  const flight = f.freshFlight();
  assert.equal((await runner.runSecretRemediationGate(flight, f.bare, f.clone)).action, "fail");
  assert.equal(await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source), undefined);
  assert.equal(flight.preserveRecoveryClone, true);
  assert.deepEqual(flight.remediationRestrictions?.pending?.requiredOIDs, [tip]);
  assert.equal((flight.secretRemediation as { withholdPaths: boolean }).withholdPaths, true);
  assert.equal(writer.mock.callCount(), 1);
  assert.equal(paths.mock.callCount(), 0);
  for (const malformed of [null, { commit: 0 }, {}]) {
    const invalid = f.freshFlight();
    t.mock.method(runner, "scanRemediationRange", async () => ({
      trusted: true, range: { tipSha: tip, excludeSha: f.base },
      findings: [finding, malformed] as SecretFinding[], floorCandidates: [],
    }));
    assert.equal((await runner.runSecretRemediationGate(invalid, f.bare, f.clone)).action, "fail");
    assert.equal(invalid.remediationRestrictions?.pending?.requireDenial, true);
    assert.equal(invalid.preserveRecoveryClone, true);
    assert.equal((invalid.secretRemediation as { known?: unknown }).known, undefined, "malformed objects never reach rendering state");
  }
  assert.equal(writer.mock.callCount(), 1);
});


test("fresh runner restores actual adopted roots independently of flags and edge sources", async t => {
  const f = await fixture();
  const x = f.commit("clean inventory ancestor X"); await f.fetch();
  const q = f.commit("finding descendant of X", x); await f.fetch();
  await git.recordRemediationScan(f.bare, BRANCH, f.source,
    { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  const d = f.commit("D descendant of Q", q); await f.fetch();
  const s = f.commit("clean source"); await f.fetch();
  await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: s, flaggedOIDs: [] });
  const context = { runId: RUN, generation: 1, kind: "issue" as const, branch: BRANCH, barePath: f.bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: f.base } };
  const coverage = await git.buildRecoveryCoverage(f.bare, context, [d, x, s], s);
  const reservation = await git.reserveRecoveryIteration(f.bare, BRANCH, KEY, f.source);
  await git.recordRecoveryCapture(f.bare, BRANCH, KEY, f.source, reservation.attempts, coverage.sha, s);
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 2, "fedcba9876543210");
  const successor = await git.prepareRecoverySuccessor(f.bare, BRANCH, KEY, f.source, reservation.attempts,
    attemptId, { admitRemediationSource: async () => true });
  t.mock.method(git, "secretScanCheckpointRange", async () => ({ trusted: true, findings: [] }));
  const runner = freshRunner();
  const flight = f.freshFlight();
  flight.worktreePath = successor.path;
  flight.runnerClone = successor;
  await runner.requireLaunchAuthority(flight);
  assert.deepEqual(new Set(flight.remediationRestrictions?.roots), new Set([q, d, x]));
  flight.publishedRealTips = [x];
  await assert.rejects(runner.requireLaunchAuthority(flight), /published floor conflict/);
});
