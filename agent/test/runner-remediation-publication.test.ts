import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { test } from "node:test";
import { formatAttemptId } from "../src/attempt-path.js";
import type { CheckpointRange, CheckpointOverlayContext, GitCache } from "../src/git.js";
import { noProofReseed, nullLogger } from "./helpers.js";
import { client, fakeGitlab, fx, git, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
const BRANCH = "agent/issue-3747", KEY = "issue-3747", RUN = "run-3747";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
type Flight = Record<string, unknown> & {
  remediationRestrictions?: { roots: string[]; denialRevision?: number;
    pending?: { requiredOIDs: string[]; requireDenial: boolean } };
};
type Subject = {
  publishCheckpointOutcome(f: Flight, bare: string, branch: string, overlay?: CheckpointOverlayContext,
    signal?: AbortSignal, pinned?: CheckpointRange): Promise<{ published: boolean; reason?: string }>;
  pushRemediationCandidate(f: Flight, bare: string, branch: string, pat: string, url: string,
    username: undefined, publication: Parameters<GitCache["pushBranch"]>[5], signal?: AbortSignal): Promise<void>;
  bridgeBareTrackingRefIfDivergent(bare: string, branch: string, flight: Flight, log: unknown): Promise<{ kind: string; bridge?: string }>;
  checkpointTestHooks: { retryBackoffMs: [number, number] };
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
    reportedPublishOutcomes: new Set(), claimGeneration: 1, runKind: "issue",
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

async function safeFixture() {
  const f = await fixture();
  const flagged = await f.enroll();
  const displaced = f.commit("displaced", flagged);
  assert.equal((await f.fetch()).kind, "updated");
  const clean = f.commit("safe source");
  assert.equal((await f.fetch()).kind, "updated");
  await git.recordRemediationScan(f.bare, BRANCH, f.source, { trusted: true, tip: clean, flaggedOIDs: [] });
  return { ...f, flagged, displaced, clean };
}
function refs(bare: string): string {
  return command(bare, "for-each-ref", "--format=%(refname) %(objectname)");
}
function push(runner: Subject, flight: Flight, f: Awaited<ReturnType<typeof safeFixture>>, candidate = f.clean) {
  return runner.pushRemediationCandidate(flight, f.bare, BRANCH, "", fx.originPath, undefined,
    { floor: { kind: "absent" }, originalHead: f.clean, candidate });
}
function uploads() {
  const tips: string[] = [];
  client.publishCheckpoint = async (_id, tip, pack) => {
    tips.push(tip);
    for await (const _chunk of pack) { /* drain the actual worker pack */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/fixture" } };
  };
  return tips;
}

for (const mode of ["checkpoint", "push"] as const) {
  test(`${mode}: fresh runner independently binds safe selected source and publishes real Git bytes`, async t => {
    const f = await safeFixture(), runner = freshRunner(), flight = f.freshFlight();
    const scans: string[] = [];
    t.mock.method(git, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
      scans.push(range.tipSha); return { trusted: true, findings: [] };
    });
    const tips = uploads();
    if (mode === "checkpoint") {
      assert.deepEqual(await runner.publishCheckpointOutcome(flight, f.bare, BRANCH), { published: true });
      assert.deepEqual(tips, [f.clean]);
    } else {
      await push(runner, flight, f);
      assert.equal(command(fx.originPath, "rev-parse", "refs/heads/" + BRANCH), f.clean);
    }
    assert.ok(scans.includes(f.clean));
    assert.deepEqual(new Set(flight.remediationRestrictions?.roots), new Set([f.flagged, f.displaced]));
    for (const root of [f.flagged, f.displaced]) assert.equal(await git.ancestry(f.bare, root, f.clean), "divergent");
    assert.equal(await git.trackingTip(f.bare, BRANCH), f.clean);
    for (const root of [f.flagged, f.displaced])
      assert.equal(command(f.bare, "rev-parse", "refs/uzi-owed/" + RUN + "/" + root), root,
        "excluded custody remains retained after successful publication");
  });

  for (const failure of ["floor", "default", "unknown scan", "untrusted binding", "late pending", "late denial"] as const) {
    test(`${mode}: ${failure} refuses before publication and leaves refs unchanged`, async t => {
      const f = await safeFixture(), runner = freshRunner(), flight = f.freshFlight();
      t.mock.method(git, "secretScanCheckpointRange", async () => {
        if (failure === "late pending") flight.remediationRestrictions!.pending = { requiredOIDs: [f.clean], requireDenial: false };
        if (failure === "late denial") flight.remediationRestrictions!.denialRevision = 1;
        return { trusted: failure !== "unknown scan", findings: [] };
      });
      if (failure === "floor") flight.checkpointFloor = f.displaced;
      if (failure === "default") command(f.bare, "update-ref", "refs/remotes/origin/main", f.flagged);
      if (failure === "untrusted binding") flight.runnerClone = { ...f.clone, attemptId: ATTEMPT + "wrong" };
      const before = refs(f.bare), remote = refs(fx.originPath), tips = uploads();
      if (mode === "checkpoint") assert.equal((await runner.publishCheckpointOutcome(flight, f.bare, BRANCH)).published, false);
      else await assert.rejects(push(runner, flight, f));
      assert.deepEqual(tips, []);
      assert.equal(refs(f.bare), before);
      assert.equal(refs(fx.originPath), remote);
    });
  }
}

for (const transform of ["overlay", "chain"] as const) {
  for (const unsafe of [false, true]) {
    test(`checkpoint: scans actual ${unsafe ? "unsafe" : "safe"} ${transform} candidate under B`, async t => {
      const f = await safeFixture(), runner = freshRunner(), flight = f.freshFlight();
      const parent = unsafe ? f.displaced : f.base;
      const wrapper = command(f.bare, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit-tree",
        f.clean + "^{tree}", "-p", f.clean, "-p", parent, "-m", "worker wrapper");
      const builder = git as unknown as { buildWorkflowOverlay(): Promise<string>; buildCheckpointChain(): Promise<string> };
      t.mock.method(builder, transform === "overlay" ? "buildWorkflowOverlay" : "buildCheckpointChain", async () => wrapper);
      const scanned: string[] = [];
      t.mock.method(git, "secretScanCheckpointRange", async (_bare: string, range: CheckpointRange) => {
        scanned.push(range.tipSha); return { trusted: true, findings: [] };
      });
      flight.checkpointChainForge = true;
      const before = refs(f.bare), tips = uploads();
      const outcome = await runner.publishCheckpointOutcome(flight, f.bare, BRANCH,
        transform === "overlay" ? {} as CheckpointOverlayContext : undefined);
      assert.equal(outcome.published, !unsafe);
      assert.ok(scanned.includes(wrapper), "the actual transformed commit must be scanned");
      assert.deepEqual(tips, unsafe ? [] : [wrapper]);
      if (unsafe) assert.equal(refs(f.bare), before);
      else assert.equal(await git.trackingTip(f.bare, BRANCH), f.clean);
    });
  }
}

test("checkpoint replay rechecks actual candidate, durable facts and pending findings after the first upload", async t => {
  const f = await safeFixture(), runner = freshRunner(), flight = f.freshFlight();
  runner.checkpointTestHooks = { retryBackoffMs: [0, 0] };
  let uploaded = 0, lateScans = 0;
  t.mock.method(git, "secretScanCheckpointRange", async () => {
    if (uploaded) lateScans++;
    return { trusted: true, findings: uploaded ? [{ commit: f.clean }] : [] };
  });
  client.publishCheckpoint = async (_id, _tip, pack) => {
    for await (const _chunk of pack) { /* real producer exits before deterministic retry */ }
    uploaded++;
    return { ok: false, httpStatus: 503 };
  };
  const before = refs(f.bare);
  assert.equal((await runner.publishCheckpointOutcome(flight, f.bare, BRANCH)).published, false);
  assert.equal(uploaded, 1);
  assert.equal(lateScans, 1);
  assert.ok(flight.remediationRestrictions?.pending?.requiredOIDs.includes(f.clean));
  const record = await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source);
  assert.ok(record?.flaggedOIDs.includes(f.clean), "persist after B releases without deadlocking");
  assert.equal(refs(f.bare), before);
});

for (const unsafe of [false, true]) {
  test(`bridge: ${unsafe ? "excluded" : "safe"} floor ${unsafe ? "holds before mutation" : "uses enrolled transformation preserving S"}`, async t => {
    const f = await safeFixture(), runner = freshRunner(), flight = f.freshFlight();
    const floor = unsafe ? f.displaced : command(f.bare, "-c", "user.name=t", "-c", "user.email=t@example.com",
      "commit-tree", f.base + "^{tree}", "-p", f.base, "-m", "published floor");
    flight.publishedTip = floor;
    t.mock.method(git, "secretScanCheckpointRange", async () => ({ trusted: true, findings: [] }));
    const before = refs(f.bare);
    const outcome = await runner.bridgeBareTrackingRefIfDivergent(f.bare, BRANCH, flight, nullLogger());
    assert.equal(outcome.kind, unsafe ? "unknown" : "bridged");
    if (unsafe) assert.equal(refs(f.bare), before);
    else {
      assert.ok(outcome.bridge);
      assert.equal(await git.trackingTip(f.bare, BRANCH), outcome.bridge);
      assert.equal((await git.readRuntimeRemediationRecovery(f.bare, BRANCH, f.source))?.ownedTip, f.clean);
      assert.equal(await git.ancestry(f.bare, f.displaced, outcome.bridge), "divergent");
      assert.equal(await git.ancestry(f.bare, floor, outcome.bridge), "ancestor");
      await push(runner, flight, f, outcome.bridge);
    }
  });
}
