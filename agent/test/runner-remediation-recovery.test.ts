import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { formatAttemptId } from "../src/attempt-path.js";
import type { CheckpointRange } from "../src/git.js";
import type { FrozenHeldPolicy } from "../src/frozen-held.js";
import { prepareFrozenHeld } from "../src/frozen-held.js";
import { noProofReseed } from "./helpers.js";
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
    runnerClone: clone, cancel: new AbortController(), steering: { lifecycleSignal: () => new AbortController().signal } });
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
