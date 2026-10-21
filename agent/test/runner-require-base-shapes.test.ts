import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { type RunContext, type ExecutorResult } from "../src/executor.js";
import { type ExecutorFactory, type RunRunner } from "../src/runner.js";
import { GitCache } from "../src/git.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { fixtureFetchTracking, fixtureUpdateTracking } from "./runner-tracking-fixture.js";

installHarness();

// ── issue #3194 acceptance: the adopted shapes through the REAL seed and --require-base guard ────
//
// Each test prepares the worker and origin state an earlier flight leaves behind (with the real
// GitCache producers: checkpointPack, bridgeToFloors, commitWipMarker, fetchAgentBranch, or a real
// runner flight interrupted before its guard), then runs a RunRunner flight on that state. The seed
// (runnerCloneForBranch), the guard (phasePreflightHandoff) and the hold/refusal paths are the
// production ones; only the forge, the api and the executor are fakes. An accepted shape reaches the
// executor and completes; a refused or held shape never invokes it.

const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test", "-c", "commit.gpgsign=false"];
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const WF = ".github/workflows/ci.yml";
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
const gitInput = (dir: string, args: string[], input: Buffer): string =>
  execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", input }).trim();

/** Write `file` in `dir`, commit it, return the new HEAD. */
function commitFile(dir: string, file: string, content = `${file}\n`): string {
  const target = path.join(dir, file);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, content);
  gitIn(dir, ["add", file]);
  gitIn(dir, [...IDENT, "commit", "-m", `work ${file}`]);
  return gitIn(dir, ["rev-parse", "HEAD"]);
}

/** Origin `main` moves its workflow file (the "behind on workflows" overlay trigger). */
function advanceOriginWorkflow(content: string): string {
  return commitFile(fx.originPath, WF, content);
}

/** A branch at origin carrying one commit over `main` (an earlier push), `main` checked out again. */
function originBranch(branch: string, file: string): string {
  gitIn(fx.originPath, ["checkout", "-q", "-b", branch]);
  const tip = commitFile(fx.originPath, file);
  gitIn(fx.originPath, ["checkout", "-q", "main"]);
  return tip;
}

/** Land a checkpoint pack in origin under the brokered ref, as the api does. */
async function landPack(pack: Readable, tip: string, branch: string): Promise<void> {
  const chunks: Buffer[] = [];
  for await (const c of pack) chunks.push(Buffer.from(c as Buffer));
  gitInput(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], Buffer.concat(chunks));
  gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tip]);
}

/** Every publish the runner makes, with the attestation it sends; each one lands in origin. */
function brokerStub(branch: string): { calls: { tip: string; attested?: string }[]; restore: () => void } {
  const calls: { tip: string; attested?: string }[] = [];
  const orig = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
    _runId: string, tipOid: string, pack: Readable, _signal?: AbortSignal, _gen?: number, attested?: string,
  ) => {
    await landPack(pack, tipOid, branch);
    calls.push({ tip: tipOid, ...(attested ? { attested } : {}) });
    return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
  };
  return { calls, restore: () => { (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig; } };
}

/** Answer the owner-hold request positively and record it (the hold path itself is pinned in
 *  runner-base-attestation-hold.test.ts; here it only proves the guard chose the hold). */
function holdStub(): { heads: string[]; restore: () => void } {
  const heads: string[] = [];
  const c = client as unknown as { requestBaseAttestationHold?: unknown };
  const orig = c.requestBaseAttestationHold;
  c.requestBaseAttestationHold = async (_runId: string, args: { head: string }) => {
    heads.push(args.head);
    return { outcome: "held", status: "paused" };
  };
  return { heads, restore: () => { c.requestBaseAttestationHold = orig; } };
}

/** An executor that records each invocation, commits one file and completes. */
function recordingFactory(onRun?: (ctx: RunContext) => void): { factory: ExecutorFactory; legs: RunContext[] } {
  const legs: RunContext[] = [];
  const factory: ExecutorFactory = () => ({
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        legs.push(ctx);
        onRun?.(ctx);
        commitFile(ctx.worktreePath, `leg${legs.length}-${path.basename(ctx.worktreePath)}.txt`);
        return { branch: ctx.branch };
      },
    },
  });
  return { factory, legs };
}

/** A seeded --require-base claim against the planned commit. */
function seededClaim(iid: number, planned: string, over: Record<string, unknown> = {}) {
  return gitlabClaim(iid, {
    plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it", session_id: null,
    planned_base_commit: planned, require_base_match: true, claim_generation: 1, ...over,
  });
}

/** What one flight reported for the run. */
function outcome(runId: string) {
  const states = api.states.filter((s) => s.runId === runId);
  return {
    completed: states.some((s) => s.body.status === "completed"),
    refusals: states.filter((s) => s.body.status === "failed").map((s) => s.body.failure_reason ?? "")
      .filter((r) => r.includes("--require-base")),
  };
}

/** A distinct worker machine (its own data dir and bare), removed by the caller. */
function otherWorker(): { cache: GitCache; dir: string } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "cdr-3194-shape-"));
  return { cache: new GitCache(dir, nullLogger(), undefined, testGitCacheOptions()), dir };
}

/** Run one flight of `claim` on the harness worker and return what it did. */
async function flight(claim: ReturnType<typeof seededClaim>, factory: ExecutorFactory): Promise<void> {
  api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation as number);
  await runnerWith(factory, fakeGitlab().gitlab).execute(claim);
}

function assertAccepted(runId: string, legs: RunContext[]): void {
  const o = outcome(runId);
  assert.deepEqual(o.refusals, [], "the continuation is not refused");
  assert.equal(legs.length, 1, "the executor ran on the adopted work");
  assert.ok(o.completed, "the flight completed");
}

function assertNotExecuted(runId: string, legs: RunContext[]): void {
  assert.equal(legs.length, 0, "the executor was never invoked");
  assert.equal(outcome(runId).completed, false, "the flight did not complete");
}

// ── accepted shapes ───────────────────────────────────────────────────────────────────────────

describe("--require-base accepts exactly the attested source (issue #3194, positive shapes)", () => {
  it("shape 1, checkpoint overlay: a fresh worker adopts the ckpt(overlay) wrapper, peels it to the attested realTip, implements", async () => {
    const iid = 31961;
    const branch = `agent/issue-${iid}`;
    const planned = advanceOriginWorkflow("name: ci\non: push\njobs: {}\n# v1\n");
    const claim = seededClaim(iid, planned);
    const a = otherWorker();
    try {
      // Worker A's accepted flight: work on P, main moves its workflow, the publish ships an overlay
      // whose packed (and attested) source is the tracking tip.
      const bareA = await a.cache.ensureClone(fx.originPath);
      const seed = await a.cache.createOrAttachRunnerClone(bareA, iid, noProofReseed, claim.run_id);
      const realTip = commitFile(seed.path, "leg0.txt");
      advanceOriginWorkflow("name: ci\non: push\njobs: {}\n# v2\n");
      await fixtureFetchTracking(a.cache, bareA, seed.path, branch, claim.run_id);
      const packed = await a.cache.checkpointPack(bareA, branch, { defaultBranch: "main" });
      assert.ok(packed, "sanity: a pack was built");
      assert.notEqual(packed.tipOid, realTip, "sanity: the publish shipped an overlay wrapper, not realTip");
      assert.match(gitIn(bareA, ["log", "-1", "--format=%s", packed.tipOid]), /^ckpt\(overlay\):/);
      await landPack(packed.pack, packed.tipOid, branch);

      const seen: { seededFrom?: string; baseCommit?: string }[] = [];
      const real = git.createOrAttachRunnerClone.bind(git);
      git.createOrAttachRunnerClone = (async (...args: Parameters<typeof real>) => {
        const out = await real(...args);
        seen.push(out);
        return out;
      }) as typeof real;
      const { factory, legs } = recordingFactory((ctx) =>
        assert.ok(fs.existsSync(path.join(ctx.worktreePath, "leg0.txt")), "the attested work is present"));
      await flight({ ...claim, claim_generation: 2, resume_phase: "implementing",
        checkpoint_tip: packed.tipOid, checkpoint_attested_source: realTip }, factory);
      assertAccepted(claim.run_id, legs);
      assert.equal(seen[0]?.seededFrom, "checkpoint", "the flight adopted the brokered checkpoint");
      assert.equal(seen[0]?.baseCommit, realTip, "the overlay was peeled to realTip");
    } finally {
      fs.rmSync(a.dir, { recursive: true, force: true });
    }
  });

  it("shape 2, overlay chain: a tree-equal chain wrapper over a prior overlay peels to the attested real tip", async () => {
    const iid = 31962;
    const branch = `agent/issue-${iid}`;
    const planned = advanceOriginWorkflow("name: ci\non: push\njobs: {}\n# v1\n");
    const claim = seededClaim(iid, planned);
    const a = otherWorker();
    try {
      const bareA = await a.cache.ensureClone(fx.originPath);
      const seed = await a.cache.createOrAttachRunnerClone(bareA, iid, noProofReseed, claim.run_id);
      commitFile(seed.path, "leg0.txt");
      advanceOriginWorkflow("name: ci\non: push\njobs: {}\n# v2\n");
      await fixtureFetchTracking(a.cache, bareA, seed.path, branch, claim.run_id);
      const first = await a.cache.checkpointPack(bareA, branch, { defaultBranch: "main" });
      assert.ok(first, "sanity: the first publish built an overlay");
      await landPack(first.pack, first.tipOid, branch);
      // The branch catches up with main's workflows, then commits N: the next publish chains a
      // tree-equal wrapper [overlay, N] whose packed source is N.
      gitIn(seed.path, ["fetch", "-q", fx.originPath, "main"]);
      gitIn(seed.path, [...IDENT, "merge", "--no-edit", "-m", "merge main", "FETCH_HEAD"]);
      const n = commitFile(seed.path, "leg0b.txt");
      await fixtureFetchTracking(a.cache, bareA, seed.path, branch, claim.run_id);
      const chained = await a.cache.checkpointPack(bareA, branch, undefined, undefined, undefined, undefined, undefined,
        [first.tipOid]);
      assert.ok(chained, "sanity: the chain publish built a pack");
      assert.notEqual(chained.tipOid, n, "sanity: a chain wrapper was shipped");
      assert.equal(gitIn(bareA, ["rev-parse", `${chained.tipOid}^{tree}`]), gitIn(bareA, ["rev-parse", `${n}^{tree}`]),
        "sanity: the chain is tree-equal to N");
      await landPack(chained.pack, chained.tipOid, branch);

      const { factory, legs } = recordingFactory((ctx) =>
        assert.ok(fs.existsSync(path.join(ctx.worktreePath, "leg0b.txt")), "the attested work is present"));
      await flight({ ...claim, claim_generation: 2, resume_phase: "implementing",
        checkpoint_tip: chained.tipOid, checkpoint_attested_source: n }, factory);
      assertAccepted(claim.run_id, legs);
      assert.equal(legs[0]?.baseCommit, n, "the chain was peeled to N");
    } finally {
      fs.rmSync(a.dir, { recursive: true, force: true });
    }
  });

  it("shape 3, park bridge: tracking moved to the tree-preserving bridge that was the attested source", async () => {
    const iid = 31963;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const claim = seededClaim(iid, planned);
    // The accepted flight published X, then rewrote it (H, divergent); its park bridged the tracking
    // ref to B (tree(H), X restored as an ancestor) and published B with the attestation.
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
    const x = commitFile(rc.path, "leg0.txt");
    gitIn(rc.path, ["push", "-q", fx.originPath, `HEAD:refs/heads/${branch}`]);
    await git.ensureClone(fx.originPath);
    fs.writeFileSync(path.join(rc.path, "rewrite.txt"), "rewritten\n");
    gitIn(rc.path, ["add", "rewrite.txt"]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const h = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await fixtureFetchTracking(git, bare, rc.path, branch, claim.run_id);
    const bridged = await git.bridgeToFloors(bare, h, [x]);
    assert.equal(bridged.kind, "built", "sanity: a bridge was built");
    const b = (bridged as { sha: string }).sha;
    await fixtureUpdateTracking(git, bare, branch, b, claim.run_id);

    const { factory, legs } = recordingFactory((ctx) =>
      assert.ok(fs.existsSync(path.join(ctx.worktreePath, "rewrite.txt")), "the bridged work is present"));
    await flight({ ...claim, claim_generation: 2, checkpoint_tip: b, checkpoint_attested_source: b }, factory);
    assertAccepted(claim.run_id, legs);
    assert.equal(legs[0]?.baseCommit, b, "the flight adopted the bridge");
  });

  it("shape 4, checkpoint equal to origin: Path-B falls to the origin floor (seededFrom origin) and the exact attested match implements", async () => {
    const iid = 31964;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const claim = seededClaim(iid, planned);
    // The accepted flight's attested publish, later pushed: origin's branch and the brokered
    // checkpoint both name S. This fresh worker has no tracking ref.
    const s = originBranch(branch, "leg0.txt");
    gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, s]);
    const seen: { seededFrom?: string }[] = [];
    const real = git.createOrAttachRunnerClone.bind(git);
    git.createOrAttachRunnerClone = (async (...args: Parameters<typeof real>) => {
      const out = await real(...args);
      seen.push(out);
      return out;
    }) as typeof real;
    const { factory, legs } = recordingFactory();
    await flight({ ...claim, claim_generation: 2, checkpoint_tip: s, checkpoint_attested_source: s }, factory);
    assertAccepted(claim.run_id, legs);
    assert.equal(seen[0]?.seededFrom, "origin", "a checkpoint equal to the floor seeds from origin");
  });
});

// ── refused or held shapes ────────────────────────────────────────────────────────────────────

/**
 * Interrupt the next flight right at its guard: the first `resolveCommitUnique` read (the guard's
 * first proof read) runs `before`, starts a worker shutdown and throws, so the flight takes the real
 * shutdown durability sink (wip marker, fetch-back owner stamp, checkpoint publish) with its
 * acceptance bit still false.
 */
function interruptAtGuard(runner: () => RunRunner | undefined, before?: () => void): () => void {
  const real = git.resolveCommitUnique.bind(git);
  let fired = false;
  git.resolveCommitUnique = (async (...args: Parameters<typeof real>) => {
    if (fired) return real(...args);
    fired = true;
    before?.();
    runner()!.shutdown();
    throw new Error("worker interrupted before the --require-base guard");
  }) as typeof real;
  return () => { git.resolveCommitUnique = real; };
}

/** Flight 1 of `claim` is interrupted before its guard (optionally after `before` dirties the clone);
 *  flight 2 reclaims with the persisted tip and whatever the server holds as the attestation. */
async function interruptedThenReclaimed(
  claim: ReturnType<typeof seededClaim>, branch: string, before?: (clonePath: string) => void,
) {
  const broker = brokerStub(branch);
  const hold = holdStub();
  const { factory, legs } = recordingFactory();
  const clones: string[] = [];
  const realClone = git.createOrAttachRunnerClone.bind(git);
  git.createOrAttachRunnerClone = (async (...args: Parameters<typeof realClone>) => {
    const out = await realClone(...args);
    clones.push(out.path);
    return out;
  }) as typeof realClone;
  let first: RunRunner | undefined;
  const restoreGuard = interruptAtGuard(() => first, () => before?.(clones.at(-1)!));
  try {
    api.setOwnershipStatus(claim.run_id, "running", 1);
    first = runnerWith(factory, fakeGitlab().gitlab);
    await first.execute(claim);
    restoreGuard();
    assert.equal(legs.length, 0, "sanity: flight 1 never reached the executor");
    const tracking = gitIn(git.barePathFor(fx.originPath), ["rev-parse", `refs/uzi-runner/${branch}`]);
    // The reclaim carries what the server persisted: the last tip and its set-only attestation.
    const published = broker.calls.at(-1)?.tip ?? null;
    const attested = broker.calls.findLast((c) => c.attested)?.attested ?? null;
    await flight({ ...claim, claim_generation: 2, checkpoint_tip: published, checkpoint_attested_source: attested }, factory);
    return { legs, publishes: broker.calls, holds: hold.heads, tracking };
  } finally {
    restoreGuard();
    broker.restore();
    hold.restore();
  }
}

describe("--require-base never accepts unchecked content (issue #3194, negative shapes)", () => {
  it("shape 7: an unchecked wip(park) marker over P adopted on the first claim is held, not unwrapped to P", async () => {
    const iid = 31971;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const claim = seededClaim(iid, planned);
    // A pre-guard writer left staged content over P in a wip(park) marker owned by this run.
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
    fs.writeFileSync(path.join(rc.path, "unchecked.txt"), "never checked against P\n");
    assert.equal(await git.commitWipMarker(rc.path), true, "sanity: a marker was committed");
    const marker = gitIn(rc.path, ["rev-parse", "HEAD"]);
    assert.equal(gitIn(rc.path, ["rev-parse", "HEAD^"]), planned, "sanity: the marker's parent is P");
    await fixtureFetchTracking(git, bare, rc.path, branch, claim.run_id);
    const hold = holdStub();
    const { factory, legs } = recordingFactory();
    try {
      await flight(claim, factory);
    } finally {
      hold.restore();
    }
    assertNotExecuted(claim.run_id, legs);
    assert.deepEqual(hold.heads, [marker], "the guard held the run on the marker itself");
  });

  it("shape 8: a marker with the same parent but a different WIP tree than the attested marker is held", async () => {
    const iid = 31972;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const claim = seededClaim(iid, planned);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
    fs.writeFileSync(path.join(rc.path, "wip.txt"), "attested content\n");
    assert.equal(await git.commitWipMarker(rc.path), true);
    const attested = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await fixtureFetchTracking(git, bare, rc.path, branch, claim.run_id);
    // After the attested publish, unchecked content replaced the WIP: a new marker over P.
    gitIn(rc.path, ["reset", "-q", "--soft", "HEAD^"]);
    fs.writeFileSync(path.join(rc.path, "wip.txt"), "different, unchecked content\n");
    assert.equal(await git.commitWipMarker(rc.path), true);
    const marker = gitIn(rc.path, ["rev-parse", "HEAD"]);
    assert.equal(gitIn(rc.path, ["rev-parse", "HEAD^"]), gitIn(bare, ["rev-parse", `${attested}^`]), "sanity: same parent");
    assert.notEqual(gitIn(rc.path, ["rev-parse", "HEAD^{tree}"]), gitIn(bare, ["rev-parse", `${attested}^{tree}`]),
      "sanity: different WIP tree");
    await fixtureFetchTracking(git, bare, rc.path, branch, claim.run_id);
    const hold = holdStub();
    const { factory, legs } = recordingFactory();
    try {
      await flight({ ...claim, claim_generation: 2, checkpoint_attested_source: attested }, factory);
    } finally {
      hold.restore();
    }
    assertNotExecuted(claim.run_id, legs);
    assert.deepEqual(hold.heads, [marker], "held on the unattested marker");
  });

  it("shape 9: a prior run's origin branch P->B adopted by a flight interrupted before its guard is held on reclaim", async () => {
    const iid = 31973;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const b = originBranch(branch, "prior-run.txt");
    const claim = seededClaim(iid, planned);
    const r = await interruptedThenReclaimed(claim, branch);
    assert.equal(r.tracking, b, "sanity: the interrupted flight's fetch-back stamped this run's tracking at B");
    assert.ok(r.publishes.length > 0 && r.publishes.every((p) => p.attested === undefined),
      `the pre-guard shutdown publish attests nothing: ${JSON.stringify(r.publishes)}`);
    assertNotExecuted(claim.run_id, r.legs);
    const refused = outcome(claim.run_id).refusals.length > 0;
    assert.ok(refused || r.holds.length > 0, "the reclaim was refused or held");
  });

  it("shape 10, shutdown fetch-back: unchecked dirty content captured before the guard is never attested and the next flight is held", async () => {
    const iid = 31974;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const claim = seededClaim(iid, planned);
    const r = await interruptedThenReclaimed(claim, branch, (clonePath) =>
      fs.writeFileSync(path.join(clonePath, "unchecked.txt"), "written before the guard\n"));
    const bare = git.barePathFor(fx.originPath);
    assert.match(gitIn(bare, ["log", "-1", "--format=%s", r.tracking]), /^wip\(park\):/,
      "sanity: the shutdown sink captured the dirty content in a marker and fetched it back");
    assert.equal(gitIn(bare, ["rev-parse", `${r.tracking}^`]), planned, "sanity: the marker sits on P");
    assert.ok(r.publishes.length > 0 && r.publishes.every((p) => p.attested === undefined),
      `the pre-guard shutdown publish attests nothing: ${JSON.stringify(r.publishes)}`);
    assertNotExecuted(claim.run_id, r.legs);
    assert.deepEqual(r.holds, [r.tracking], "the next flight is held on the unattested marker");
  });

  it("shape 10, retained recovery capture: an unchecked dirty source captured before the guard is never attested and the flight is held", async () => {
    const iid = 31975;
    const branch = `agent/issue-${iid}`;
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const claim = seededClaim(iid, planned, { claim_generation: 2 });
    // A predecessor attempt of this run left a retained clone with dirty, never-checked work.
    const bare = await git.ensureClone(fx.originPath);
    fs.chmodSync(path.dirname(bare), 0o700);
    fs.chmodSync(bare, 0o700);
    const rc = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
    await git.markRecoveryCapture(bare, rc.path, branch, claim.run_id);
    fs.writeFileSync(path.join(rc.path, "retained.txt"), "only local dirty work\n");
    const broker = brokerStub(branch);
    const hold = holdStub();
    const { factory, legs } = recordingFactory();
    try {
      api.setOwnershipStatus(claim.run_id, "running", 2);
      await runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 }).execute(claim);
    } finally {
      broker.restore();
      hold.restore();
    }
    const restore = JSON.parse(gitIn(bare, ["config", `uzi-recovery.${branch}.clone`])).recovery;
    assert.ok(restore?.restoreTip, `sanity: the retained capture journaled a restore tip: ${JSON.stringify(restore)}`);
    assert.ok(broker.calls.every((p) => p.attested === undefined),
      `no pre-guard capture publish attests: ${JSON.stringify(broker.calls)}`);
    assertNotExecuted(claim.run_id, legs);
    assert.equal(hold.heads.length, 1, "the successor's guard held the run");
  });
});

// ── issue #3194 M1': the guard compares the clone's INDEX tree, not the identity's tree ─────────
//
// One real retained-recovery adoption, run twice. A predecessor flight accepted on P, committed and
// attested S, then its capture journaled a restore tip that stands for S (restoreSource = S, built
// with the real capture API) and the flight ended before adopting a successor. The reclaim carries
// attested S, so the guard's identity equals the attestation in both runs; only the content differs.
//  (a) the restore is a wrapper over S whose tree is S's: the successor implements.
//  (b) the restore is the wip(park) marker over S: the successor's HEAD is S and the unattested WIP
//      is STAGED, so only the index tree differs from tree(S). It is held before the executor.

async function retainedMappedRestore(iid: number, extraWip: boolean) {
  const branch = `agent/issue-${iid}`;
  const key = `issue-${iid}`;
  const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  const claim = seededClaim(iid, planned, { claim_generation: 2 });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const rc = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
  const s = commitFile(rc.path, "accepted.txt");
  await fixtureFetchTracking(git, bare, rc.path, branch, claim.run_id);
  await git.markRecoveryCapture(bare, rc.path, branch, claim.run_id);
  const source = { runId: claim.run_id, clonePath: rc.path };
  const reservation = await git.reserveRecoveryIteration(bare, branch, key, source);
  let restoreTip: string;
  if (extraWip) {
    fs.writeFileSync(path.join(rc.path, "unattested.txt"), "dirty WIP left after the attested publish\n");
    assert.equal(await git.commitWipMarker(rc.path), true);
    restoreTip = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await fixtureFetchTracking(git, bare, rc.path, branch, claim.run_id);
  } else {
    restoreTip = gitIn(bare, [...IDENT, "commit-tree", `${s}^{tree}`, "-p", s, "-p", planned, "-m", "uzi recovery coverage test"]);
  }
  await git.recordRecoveryCapture(bare, branch, key, source, reservation.attempts, restoreTip, s);
  const journaled = JSON.parse(gitIn(bare, ["config", `uzi-recovery.${branch}.clone`])).recovery;
  assert.equal(journaled.restoreSource, s, "sanity: the restore stands for the attested source");
  assert.notEqual(restoreTip, s, "sanity: the source is a mapping, not the restore tip itself");

  const successors: { path: string; adoptedContentSource?: string; head: string; staged: string }[] = [];
  const realPrepare = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = (async (...args: Parameters<typeof realPrepare>) => {
    const out = await realPrepare(...args);
    // What the adoption materialized, read before the guard and any executor.
    successors.push({ ...out, head: gitIn(out.path, ["rev-parse", "HEAD"]),
      staged: gitIn(out.path, ["diff", "--cached", "--name-only"]) });
    return out;
  }) as typeof realPrepare;
  const broker = brokerStub(branch);
  const hold = holdStub();
  const { factory, legs } = recordingFactory((ctx) => {
    if (extraWip) return;
    assert.equal(gitIn(ctx.worktreePath, ["write-tree"]), gitIn(ctx.worktreePath, [`rev-parse`, `${s}^{tree}`]),
      "the executor receives exactly the attested tree");
  });
  try {
    api.setOwnershipStatus(claim.run_id, "running", 2);
    await runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(), { recoveryRetryMs: 1 })
      .execute({ ...claim, checkpoint_tip: s, checkpoint_attested_source: s });
  } finally {
    broker.restore();
    hold.restore();
  }
  assert.equal(successors.length, 1, "sanity: the flight adopted the restore on a fresh successor");
  assert.equal(successors[0]!.adoptedContentSource, s, "sanity: the guard's identity is the attested source");
  return { claim, rc, bare, restoreTip, s, successor: successors[0]!, legs, holds: hold.heads };
}

describe("--require-base materialized-tree check on a real recovery adoption (issue #3194 M1')", () => {
  it("(a) a mapped restore whose tree is exactly the attested source implements", async () => {
    const r = await retainedMappedRestore(31977, false);
    assertAccepted(r.claim.run_id, r.legs);
    assert.deepEqual(r.holds, [], "no hold was requested");
  });

  it("(b) the same restore plus unattested STAGED WIP is held before the executor, and the work is retained", async () => {
    const r = await retainedMappedRestore(31976, true);
    assertNotExecuted(r.claim.run_id, r.legs);
    assert.equal(r.successor.head, r.s, "sanity: the adopted successor's HEAD was the attested source");
    assert.equal(r.successor.staged, "unattested.txt", "sanity: the extra WIP was staged in the successor's index");
    assert.equal(r.holds.length, 1, "the guard held the run");
    assert.deepEqual(outcome(r.claim.run_id).refusals, [], "held, not failed");
    // Retained: the predecessor clone, the successor's staged WIP and the restore pin.
    assert.equal(fs.readFileSync(path.join(r.rc.path, "unattested.txt"), "utf8"), "dirty WIP left after the attested publish\n");
    assert.equal(fs.readFileSync(path.join(r.successor.path, "unattested.txt"), "utf8"), "dirty WIP left after the attested publish\n");
    assert.equal(gitIn(r.bare, ["rev-parse", `refs/uzi-recovery-episode/${r.claim.run_id}/${r.restoreTip}`]), r.restoreTip);
  });
});
