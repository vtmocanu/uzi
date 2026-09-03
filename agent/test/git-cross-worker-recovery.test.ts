import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { GitCache, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";

// PRD #628 M3 — the cross-worker checkpoint recovery regression test (SC#2).
//
// This drives the FULL cross-worker path with REAL git, not mocks: worker A commits
// work ahead of the floor and publishes a checkpoint pack to a simulated origin (exactly
// as client.publishCheckpoint -> pushbroker does); worker B is a DIFFERENT, FRESH bare
// (its own dataDir, so no local refs/uzi-runner/<branch> -> NOT ownedHere) that fetches
// and reseeds. It proves the three links the ADR (D3b) leaves for M3 to verify end to
// end: (1) fetch()'s `+refs/uzi-checkpoints/*` refspec materialises the brokered ref in a
// fresh worker's bare — the one link that, if broken, makes a correct publish invisible;
// (2) the reseed then PREFERS that mirrored checkpoint (owner-anchored: since issue #1059 M1
// adoption requires the mirrored checkpoint to be THIS run's own, so the positive reseed models
// production by passing resume=true + this run's own matching checkpoint tip); (3) the
// strict-descendant guard is load-bearing — a checkpoint that merely equals the floor is NOT
// seeded as a recovery, so the positive assertion is non-vacuous.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

let fx: Fixture;

beforeEach(() => {
  fx = makeFixture();
});

afterEach(() => fx.cleanup());

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

function refInBare(bare: string, ref: string): boolean {
  try {
    gitIn(bare, ["rev-parse", "--verify", "--quiet", ref]);
    return true;
  } catch {
    return false;
  }
}

async function drain(r: Readable): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const c of r) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

/** Commit `file` in the runner clone and return the new HEAD sha. */
function commit(dir: string, file: string): string {
  fs.writeFileSync(path.join(dir, file), `${file}\n`);
  gitIn(dir, ["add", file]);
  gitIn(dir, [...IDENT, "commit", "-m", `work ${file}`]);
  return gitIn(dir, ["rev-parse", "HEAD"]);
}

/** A distinct worker machine: its OWN dataDir → its own bare, so worker B never shares
 *  worker A's refs/uzi-runner/* (the whole point of the cross-worker case). */
function worker(name: string): GitCache {
  const dataDir = path.join(fx.dataDir, name);
  fs.mkdirSync(dataDir, { recursive: true });
  return new GitCache(dataDir, nullLogger());
}

/** A worker whose GitCache captures every emitted log record into `lines`, so an
 *  issue-#1059 true-negative can assert the LOUD owner-anchor set-aside warn fired. */
function loggingWorker(name: string): { git: GitCache; lines: unknown[] } {
  const dataDir = path.join(fx.dataDir, name);
  fs.mkdirSync(dataDir, { recursive: true });
  const { logger, lines } = recordingLogger();
  return { git: new GitCache(dataDir, logger), lines };
}

/** True when `lines` holds a level:"warn" record whose msg names the owner-anchor guard and
 *  which carries the expected_checkpoint_tip + checkpoint_sha diagnostic fields. */
function sawOwnerAnchorWarn(lines: unknown[]): boolean {
  return lines.some((l) => {
    const r = l as { level?: string; msg?: string } & Record<string, unknown>;
    return (
      r.level === "warn" &&
      typeof r.msg === "string" &&
      r.msg.includes("owner-anchor guard") &&
      "expected_checkpoint_tip" in r &&
      "checkpoint_sha" in r
    );
  });
}

/** Create refs/heads/<branch> in the fixture origin at a NEW commit that descends `parent`
 *  and adds a single `file` — a line that DIVERGES from any sibling built off the same
 *  parent (e.g. a wip(park) marker). Uses a throwaway index + plumbing so the origin's
 *  working tree and checked-out branch are untouched. Worker B's ensureClone then mirrors
 *  it as refs/remotes/origin/<branch>, so originExists=true on the reseed and the floor is
 *  this published branch — the shape leg #4 needs under the issue-#1059 owner-anchor predicate. */
function makeOriginBranch(branch: string, parent: string, file: string, content: string): string {
  const idx = path.join(fx.originPath, ".git", `tmp-index-${branch.replace(/[^a-zA-Z0-9]/g, "_")}`);
  const env = { ...GIT_ENV, GIT_INDEX_FILE: idx };
  execFileSync("git", ["-C", fx.originPath, "read-tree", parent], { env });
  const blob = execFileSync("git", ["-C", fx.originPath, "hash-object", "-w", "--stdin"], {
    env: GIT_ENV,
    input: content,
    encoding: "utf8",
  }).trim();
  execFileSync("git", ["-C", fx.originPath, "update-index", "--add", "--cacheinfo", `100644,${blob},${file}`], { env });
  const tree = execFileSync("git", ["-C", fx.originPath, "write-tree"], { env, encoding: "utf8" }).trim();
  const sha = execFileSync(
    "git",
    ["-C", fx.originPath, ...IDENT, "commit-tree", tree, "-p", parent, "-m", `origin work ${file}`],
    { env: GIT_ENV, encoding: "utf8" },
  ).trim();
  gitIn(fx.originPath, ["update-ref", `refs/heads/${branch}`, sha]);
  fs.rmSync(idx, { force: true });
  return sha;
}

/** Build a commit object directly in the bare (no worktree) and return its sha. `parents`
 *  empty makes a root commit. Used to construct precise checkpoint tip shapes (a wip(park)
 *  marker with/without a committed parent) deterministically for the hasCommittedCheckpoint
 *  unit tests — no timing, no working tree. */
function mkCommit(bare: string, tree: string, parents: string[], subject: string): string {
  const pargs = parents.flatMap((p) => ["-p", p]);
  return gitIn(bare, [...IDENT, "commit-tree", tree, ...pargs, "-m", subject]);
}

/** Advance the fixture origin's `main` by one commit and return its new tip. Simulates
 *  `main` moving forward DURING a park, which is what diverges a set-aside checkpoint. */
function advanceOriginMain(file: string, content: string): string {
  fs.writeFileSync(path.join(fx.originPath, file), content);
  gitIn(fx.originPath, ["add", file]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", `main advances ${file}`]);
  return gitIn(fx.originPath, ["rev-parse", "HEAD"]);
}

/** Worker A: seed off the current floor, optionally commit `milestones` real commits, then
 *  leave a dirty `wipFile`, auto-commit it to a wip(park) marker (the production
 *  commitWipMarker path), and publish that marker as the branch's brokered checkpoint —
 *  WITHOUT pushing the agent branch to origin (originExists=false, the incident shape).
 *  Built + published while origin/main is still the OLD floor, so a later advanceOriginMain
 *  makes the checkpoint diverged. Returns the marker sha and its parent (the fork point). */
async function publishWipParkCheckpoint(
  gitA: GitCache,
  bareA: string,
  issue: number,
  branch: string,
  wipFile: string,
  wipContent: string,
  milestones: string[] = [],
): Promise<{ marker: string; parent: string }> {
  const seed = await gitA.createOrAttachRunnerClone(bareA, issue, "run-A");
  for (const m of milestones) commit(seed.path, m);
  const parent = gitIn(seed.path, ["rev-parse", "HEAD"]); // the marker's parent = the fork point
  fs.writeFileSync(path.join(seed.path, wipFile), wipContent);
  const committed = await gitA.commitWipMarker(seed.path);
  assert.strictEqual(committed, true, "commitWipMarker planted a marker for the dirty tree");
  const marker = gitIn(seed.path, ["rev-parse", "HEAD"]);
  assert.ok(
    gitIn(seed.path, ["log", "-1", "--format=%s"]).startsWith(WIP_PARK_COMMIT_PREFIX),
    "the checkpoint tip is a wip(park) marker",
  );
  await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
  const packed = await gitA.checkpointPack(bareA, branch);
  assert.ok(packed, "worker A builds a checkpoint pack from its tracking ref");
  assert.strictEqual(packed!.tipOid, marker, "the pack tip is the wip(park) marker");
  const pack = await drain(packed!.pack);
  assert.ok(pack.length > 0, "the checkpoint pack is non-empty");
  publishPackToOrigin(pack, marker, `refs/uzi-checkpoints/${branch}`);
  return { marker, parent };
}

/** Land a checkpoint pack into origin and point the brokered ref at its tip — the
 *  server-side effect of client.publishCheckpoint -> pushbroker, with local git. The
 *  pack from checkpointPack is non-thin (pack-objects has no --thin), but --fix-thin is
 *  harmless and keeps this robust if that ever changes; origin holds the floor objects. */
function publishPackToOrigin(pack: Buffer, tip: string, ref: string): void {
  execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"], {
    env: GIT_ENV,
    input: pack,
    stdio: ["pipe", "pipe", "pipe"],
  });
  gitIn(fx.originPath, ["update-ref", ref, tip]);
}

describe("cross-worker checkpoint recovery (PRD #628 M3)", () => {
  it("recovers committed work from another worker's published checkpoint via the fetch mirror link", async () => {
    const branch = "agent/issue-628";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;

    // WORKER A: commit work ahead of the default branch WITHOUT pushing the branch to
    // origin — the incident case (originExists=false, floor=default) — then build a
    // checkpoint pack from the fetched-back tracking ref via the PRODUCTION pack builder.
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 628, "run-A");
    commit(seed.path, "M1.txt");
    const cpTip = commit(seed.path, "M2.txt"); // ≥1 commit strictly ahead of the floor
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
    const packed = await gitA.checkpointPack(bareA, branch);
    assert.ok(packed, "worker A builds a checkpoint pack from its tracking ref");
    assert.strictEqual(packed!.tipOid, cpTip, "the pack tip is the checkpoint tip");
    const pack = await drain(packed!.pack);
    assert.ok(pack.length > 0, "the checkpoint pack is non-empty");

    // WORKER B: a DIFFERENT, FRESH bare. Clone it BEFORE the checkpoint is published so
    // fetch()'s mirror refspec is isolated as the thing that materialises the ref.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    assert.strictEqual(
      refInBare(bareB, checkpointRef),
      false,
      "precondition: no checkpoint yet in worker B's fresh bare",
    );

    // Publish worker A's pack into origin and point the brokered checkpoint ref at its tip.
    publishPackToOrigin(pack, cpTip, checkpointRef);

    // THE CROSS-WORKER MIRROR LINK: a warm ensureClone runs fetch(), whose
    // `+refs/uzi-checkpoints/*` refspec materialises the ref (and its objects) in the
    // FRESH worker's bare. This is the residual verification the ADR (D3b) leaves M3.
    await gitB.ensureClone(fx.originPath);
    assert.strictEqual(
      refInBare(bareB, checkpointRef),
      true,
      "fetch() mirrored the checkpoint ref into worker B's fresh bare (the cross-worker link)",
    );
    assert.strictEqual(
      gitIn(bareB, ["rev-parse", checkpointRef]),
      cpTip,
      "…at worker A's checkpoint tip",
    );

    // THE RESEED: worker B seeds a cross-worker run (no local tracking ref → NOT ownedHere)
    // off the mirrored checkpoint. Models production: resume=true and THIS run's own persisted
    // checkpoint tip (== the mirrored checkpoint's SHA), so the issue-#1059 owner anchor admits
    // the adopt. With no origin/<branch> this is the Path-A resume-adopt leg.
    const rc = await gitB.createOrAttachRunnerClone(bareB, 628, "run-B", true /*resume*/, cpTip /*matching own tip*/);

    // SC#2 NON-VACUITY GUARD — all of these must hold, else the assertion would pass on a
    // checkpoint that merely equals the floor and prove nothing.
    assert.strictEqual(rc.seededFrom, "checkpoint", "seeded from the checkpoint, NOT the default branch");
    assert.ok(rc.priorCommits > 0, `priorCommits must be > 0 (cross-worker recovery), got ${rc.priorCommits}`);
    assert.strictEqual(rc.baseCommit, cpTip, "the base is the checkpoint tip");
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.notStrictEqual(
      rc.baseCommit,
      floorSha,
      "the checkpoint is STRICTLY ahead of the floor (checkpointSha !== floorSha, git.ts:479)",
    );
    assert.doesNotThrow(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, rc.baseCommit]),
      "the floor is an ancestor of the checkpoint — it genuinely descends the floor by ≥1 commit",
    );
    // The recovered work is actually checked out in worker B's runner clone.
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), true, "recovered work checked out");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M2.txt")), true, "recovered work checked out");
    assert.notStrictEqual(rc.checkpointSetAside, true, "a strictly-descending checkpoint is not set aside");
  });

  it("does NOT seed from a checkpoint that merely equals the floor (the strict-descendant guard bites cross-worker)", async () => {
    const branch = "agent/issue-629";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;

    // Publish a checkpoint ref that points at EXACTLY the default floor — a park with no
    // new work beyond the default tip. No pack is needed (origin already holds the
    // objects), which is itself the point: nothing was recovered.
    const floorAtOrigin = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["update-ref", checkpointRef, floorAtOrigin]);

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    assert.strictEqual(refInBare(bareB, checkpointRef), true, "the equal checkpoint mirrored into the fresh bare");
    assert.strictEqual(
      gitIn(bareB, ["rev-parse", checkpointRef]),
      gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]),
      "precondition: the mirrored checkpoint EQUALS the floor",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 629, "run-B");

    // The reseed must fall through to the default floor rather than seed "checkpoint": under
    // issue #1059 M1 this fresh run (no resume, no own-checkpoint tip) fails the owner anchor,
    // so it never adopts regardless of ancestry — and even had the tip matched, the equal
    // checkpoint recovers nothing (the strict-descendant guard). Either way, "default".
    assert.notStrictEqual(rc.seededFrom, "checkpoint", "an equal checkpoint must NOT be seeded as a recovery");
    assert.strictEqual(rc.seededFrom, "default", "falls through to the default floor");
    assert.strictEqual(rc.priorCommits, 0);
    assert.notStrictEqual(rc.checkpointSetAside, true, "equality is not divergence");
  });

  it("recovers a wip(park) marker to UNCOMMITTED at adopt time via reset --soft (same-worker, PRD #759 M2)", async () => {
    // The PRIMARY seam M2 adds: a same-worker resume whose adopted tracking-ref tip is a
    // `wip(park):` marker (M1's throwaway auto-commit of the pre-park uncommitted tree) is
    // reset --soft'd back so the WIP content returns as UNCOMMITTED changes and the marker
    // never enters the branch history. Drives it with REAL git through the production
    // commitWipMarker + fetchAgentBranch + reseed path.
    const branch = "agent/issue-759";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);

    // Seed off the default branch, then leave a genuinely UNCOMMITTED edit in the clone —
    // the single deviation from the committing park factories, mirroring the #685 incident
    // (mid-milestone work never committed).
    const seed = await gitA.createOrAttachRunnerClone(bareA, 759, "run-A");
    const forkPoint = gitIn(seed.path, ["rev-parse", "HEAD"]); // the last REAL commit
    fs.writeFileSync(path.join(seed.path, "WIP.txt"), "in-progress work\n");

    // M1: auto-commit the uncommitted work to a wip(park) marker, then fetch it back to the
    // worker-side tracking ref (stamped run-A → ownedHere on reseed).
    const committed = await gitA.commitWipMarker(seed.path);
    assert.strictEqual(committed, true, "commitWipMarker planted a marker for the dirty tree");
    const markerSubject = gitIn(seed.path, ["log", "-1", "--format=%s"]);
    assert.ok(markerSubject.startsWith(WIP_PARK_COMMIT_PREFIX), "the marker subject is prefixed wip(park):");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    // THE RESEED (same worker, same run): the tracking ref is owned here and its tip is the
    // marker, so M2's reset --soft fires.
    const rc = await gitA.createOrAttachRunnerClone(bareA, 759, "run-A");

    // (a) the WIP content is present in the resumed clone as an UNCOMMITTED change.
    assert.strictEqual(fs.existsSync(path.join(rc.path, "WIP.txt")), true, "recovered WIP file is in the working tree");
    assert.strictEqual(
      fs.readFileSync(path.join(rc.path, "WIP.txt"), "utf8"),
      "in-progress work\n",
      "…with its pre-park content",
    );
    const porcelain = gitIn(rc.path, ["status", "--porcelain"]);
    assert.ok(/WIP\.txt/.test(porcelain), `WIP.txt shows as an uncommitted change, got: ${JSON.stringify(porcelain)}`);

    // (b) the finalized branch history carries NO wip(park): subject — the marker was
    // stripped by reset --soft, so it never reaches finalize / the MR.
    const subjects = gitIn(rc.path, ["log", "--format=%s"]).split("\n");
    assert.ok(
      subjects.every((s) => !s.startsWith(WIP_PARK_COMMIT_PREFIX)),
      `no wip(park): commit in the resumed branch history, got: ${JSON.stringify(subjects)}`,
    );

    // (c) the recovery is surfaced on the RunnerClone, and the base is the real fork point
    // (the marker's parent), not the marker — so M5's recovered-commit count excludes it.
    assert.strictEqual(rc.wipRecovered, true, "wipRecovered signals the WIP-snapshot recovery");
    assert.strictEqual(rc.seededFrom, "tracking", "adopted the same-worker tracking ref");
    assert.strictEqual(rc.baseCommit, forkPoint, "baseCommit is the marker's parent (the last real commit)");
    assert.strictEqual(rc.priorCommits, 0, "the marker is not counted as recovered committed work");
  });

  it("commitWipMarker on a CLEAN tree makes no commit and returns false (PRD #759 M1)", async () => {
    // The other half of M1's contract (the dirty→marker half is asserted throughout this
    // file): a park whose clone has NOTHING uncommitted must NOT manufacture an empty
    // wip(park) marker — commitWipMarker returns false and HEAD is byte-identical, so the
    // reseed has no marker to reset --soft and the "recovered N commits" count is unperturbed.
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 766, "run-A");
    const headBefore = gitIn(seed.path, ["rev-parse", "HEAD"]);
    assert.strictEqual(gitIn(seed.path, ["status", "--porcelain"]), "", "precondition: the clone is clean");

    const committed = await gitA.commitWipMarker(seed.path);

    assert.strictEqual(committed, false, "a clean tree produces no marker commit");
    assert.strictEqual(gitIn(seed.path, ["rev-parse", "HEAD"]), headBefore, "HEAD is unchanged (no empty commit)");
    assert.ok(
      !gitIn(seed.path, ["log", "-1", "--format=%s"]).startsWith(WIP_PARK_COMMIT_PREFIX),
      "the tip is not a wip(park) marker",
    );
  });

  it("recovers a DIVERGED wip(park) checkpoint onto a divergent published origin as an uncommitted change (cross-worker, PRD #759 M4 leg #4)", async () => {
    // Leg #4, under the issue-#1059 M1 owner-anchor predicate: leg #4 (checkpointSetAside →
    // cherry-pick) is now reachable ONLY when the checkpoint is THIS run's OWN (matching
    // expectedCheckpointTip), origin/<branch> EXISTS as the floor, and the checkpoint DIVERGES
    // from it. So the fixture publishes origin/<branch> off the pre-park floor on a line
    // DISJOINT from the WIP: it becomes the floor, the checkpoint marker (over the same pre-park
    // floor) diverges from it → the strict-descendant guard sets it aside, and the marker's
    // parent (the pre-park floor) is an ancestor of the origin floor, so the WIP delta
    // cherry-picks CLEAN. (This is also the leg that once shipped DOA — M2 cherry-picked the ref
    // NAME, absent from the `--shared --no-checkout` clone — so the #759 guard is preserved here.)
    const branch = "agent/issue-760";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath); // origin/main = the pre-park floor (C0)
    const floor0 = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const { marker, parent } = await publishWipParkCheckpoint(
      gitA, bareA, 760, branch, "WIP.txt", "diverged in-progress work\n",
    );
    assert.strictEqual(parent, floor0, "the marker's parent is the pre-park floor");

    // A published origin/<branch> off the pre-park floor, on a file DISJOINT from the WIP → it
    // is the floor and the checkpoint marker diverges from it (so the WIP delta picks clean).
    const originTip = makeOriginBranch(branch, floor0, "ORIGIN_WORK.txt", "published divergent origin work\n");

    // Worker B: a fresh bare that fetches the origin branch AND the (diverged) checkpoint.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", `refs/remotes/origin/${branch}`]);
    assert.strictEqual(floorSha, originTip, "worker B's floor is the published origin branch");
    assert.strictEqual(
      refInBare(bareB, `refs/uzi-checkpoints/${branch}`) && gitIn(bareB, ["rev-parse", `refs/uzi-checkpoints/${branch}`]),
      marker,
      "the diverged checkpoint mirrored in at the wip(park) marker",
    );
    // The checkpoint is genuinely DIVERGED from the origin floor — else this is not leg #4.
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, marker]),
      "the checkpoint does NOT descend the origin floor (it is diverged, set aside)",
    );
    // …and the marker's parent (fork point) IS an ancestor of the floor — the recoverable shape.
    assert.doesNotThrow(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", parent, floorSha]),
      "the marker's parent is an ancestor of the origin floor (no committed work below the marker)",
    );

    // RESUME with THIS run's own matching tip (== the marker) so the owner anchor admits it;
    // origin/<branch> exists → Path B; diverged → set aside → leg #4 fires.
    const rc = await gitB.createOrAttachRunnerClone(bareB, 760, "run-B", true /*resume*/, marker /*matching own tip*/);

    // (a) the WIP content is recovered as an UNCOMMITTED change on the origin floor.
    //     PRE-FIX this FAILS: M2 cherry-picked `refs/uzi-checkpoints/<branch>` by NAME, but a
    //     `git clone --shared --no-checkout` copies no custom refs, so the pick hit `fatal:
    //     bad revision` and the leg always safe-failed → WIP.txt absent, wipRecovered false,
    //     checkpointSetAside true. So each assertion below would not hold before the fix.
    assert.strictEqual(fs.existsSync(path.join(rc.path, "WIP.txt")), true, "recovered WIP file is in the working tree");
    assert.strictEqual(
      fs.readFileSync(path.join(rc.path, "WIP.txt"), "utf8"),
      "diverged in-progress work\n",
      "…with its pre-park content",
    );
    const porcelain = gitIn(rc.path, ["status", "--porcelain"]);
    assert.ok(/WIP\.txt/.test(porcelain), `WIP.txt shows as an uncommitted change, got: ${JSON.stringify(porcelain)}`);
    // The published origin work is present too (we sit ON the origin floor, not the checkpoint).
    assert.strictEqual(fs.existsSync(path.join(rc.path, "ORIGIN_WORK.txt")), true, "the origin-floor file is present");

    // (b) the recovery is surfaced and set-aside is cleared; no wip(park) marker in history.
    assert.strictEqual(rc.wipRecovered, true, "wipRecovered signals the diverged-WIP recovery");
    assert.notStrictEqual(rc.checkpointSetAside, true, "a recovered checkpoint is no longer set aside");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the origin floor (no committed checkpoint work recovered)");
    assert.strictEqual(rc.priorCommits, 1, "only the origin floor's own published commit ahead of default — no checkpoint commit recovered");
    const subjects = gitIn(rc.path, ["log", "--format=%s"]).split("\n");
    assert.ok(
      subjects.every((s) => !s.startsWith(WIP_PARK_COMMIT_PREFIX)),
      `no wip(park): commit in the resumed branch history, got: ${JSON.stringify(subjects)}`,
    );
  });

  it("SAFE-FAILS a diverged wip(park) checkpoint whose WIP conflicts with the published origin, leaving a pristine floor (leg #4)", async () => {
    // The recoverable shape (marker parent is a floor ancestor) but the WIP touches the SAME
    // file the published origin/<branch> holds, so the cherry-pick CONFLICTS. Required safe
    // failure (SC#1(b)): abort the half-applied pick, hard-reset to the floor, report failure —
    // never a half-merged tree, never a silent drop.
    const branch = "agent/issue-761";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const floor0 = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const { marker, parent } = await publishWipParkCheckpoint(
      gitA, bareA, 761, branch, "CONTESTED.txt", "the parked worker's edit\n",
    );
    assert.strictEqual(parent, floor0, "the marker's parent is the pre-park floor");

    // origin/<branch> off the pre-park floor touches the VERY SAME file as the WIP, with
    // different content → add/add conflict when the WIP delta is picked onto it.
    const originTip = makeOriginBranch(branch, floor0, "CONTESTED.txt", "origin's published edit\n");
    assert.doesNotThrow(
      () => gitIn(fx.originPath, ["merge-base", "--is-ancestor", parent, originTip]),
      "the fork point is still an ancestor of the floor (so the guard admits the pick; the conflict is the WIP itself)",
    );

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", `refs/remotes/origin/${branch}`]);
    assert.strictEqual(floorSha, originTip, "worker B's floor is the published origin branch");
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, marker]),
      "the checkpoint is diverged (set aside)",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 761, "run-B", true /*resume*/, marker /*matching own tip*/);

    // Recovery FAILED — set aside, not recovered.
    assert.notStrictEqual(rc.wipRecovered, true, "a conflicting WIP is not recovered");
    assert.strictEqual(rc.checkpointSetAside, true, "the checkpoint stays SET ASIDE (loud notice preserved)");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the pristine origin floor");

    // The clone tree is a PRISTINE floor: origin's version of the file, no conflict markers, no
    // staged WIP.
    const contested = fs.readFileSync(path.join(rc.path, "CONTESTED.txt"), "utf8");
    assert.strictEqual(contested, "origin's published edit\n", "the floor's content is intact (WIP was not force-applied)");
    assert.ok(!/^<{7}|^={7}|^>{7}/m.test(contested), "no conflict markers left in the tree");
    assert.strictEqual(gitIn(rc.path, ["status", "--porcelain"]), "", "no staged/uncommitted residue — a clean floor");
  });

  it("LEAVES SET ASIDE a diverged checkpoint with a committed milestone BELOW the marker — no cherry-pick (Defect 2 guard, leg #4)", async () => {
    // `floor → m1(committed) → wip-marker`, with m1 NOT on the published origin floor.
    // Cherry-picking only the marker tip would apply the WIP delta cleanly (it is disjoint from
    // the origin work) and flip checkpointSetAside=false — silently dropping m1 AND suppressing
    // the set-aside notice. The Defect-2 guard (marker PARENT must be an ancestor of the floor)
    // bites here: m1 is not an ancestor of the origin floor, so leg #4 must NOT cherry-pick and
    // must keep the checkpoint set aside for a human.
    const branch = "agent/issue-762";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const floor0 = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const { marker, parent } = await publishWipParkCheckpoint(
      gitA, bareA, 762, branch, "WIP.txt", "in-progress over a committed milestone\n",
      ["M1.txt"], // one committed-but-unpushed milestone below the marker
    );
    assert.notStrictEqual(parent, floor0, "the marker's parent is the committed milestone m1, not the floor");

    // origin/<branch> off the pre-park floor on a DISJOINT file — the marker's parent (m1) is
    // NOT an ancestor of this floor, so the Defect-2 guard must leave the checkpoint set aside.
    const originTip = makeOriginBranch(branch, floor0, "ORIGIN_WORK.txt", "published divergent origin work\n");
    assert.throws(
      () => gitIn(fx.originPath, ["merge-base", "--is-ancestor", parent, originTip]),
      "the marker's parent (m1) is NOT an ancestor of the origin floor — committed divergence below the marker",
    );

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", `refs/remotes/origin/${branch}`]);
    assert.strictEqual(floorSha, originTip, "worker B's floor is the published origin branch");
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, marker]),
      "the checkpoint is diverged (set aside)",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 762, "run-B", true /*resume*/, marker /*matching own tip*/);

    // The guard left it set aside: NO cherry-pick, NO flip, NO silent milestone drop.
    assert.strictEqual(rc.checkpointSetAside, true, "committed divergence below the marker stays SET ASIDE");
    assert.notStrictEqual(rc.wipRecovered, true, "nothing was recovered — the loud notice is preserved");
    // Neither the committed milestone nor the WIP was applied — the tree is the pristine floor.
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "the committed milestone was NOT partially applied");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "WIP.txt")), false, "the WIP delta was NOT cherry-picked");
    assert.strictEqual(gitIn(rc.path, ["status", "--porcelain"]), "", "a clean, pristine floor");
  });

  it("without a published checkpoint, the reseed falls to the default branch (positive control: the test depends on the mirror link + publish)", async () => {
    const branch = "agent/issue-630";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;

    // No checkpoint is published for this branch, so worker B's fetch mirrors nothing.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    assert.strictEqual(refInBare(bareB, checkpointRef), false, "no checkpoint ref exists for this branch");

    const rc = await gitB.createOrAttachRunnerClone(bareB, 630, "run-B");

    // seededFrom="checkpoint" in the positive test therefore genuinely depends on the
    // published checkpoint + the fetch-mirror leg, not on incidental fixture state.
    assert.strictEqual(rc.seededFrom, "default", "no checkpoint ⇒ a fresh start from the default branch");
    assert.strictEqual(rc.priorCommits, 0);
    assert.notStrictEqual(rc.checkpointSetAside, true);
  });
});

describe("resume-relaxed checkpoint adoption (PRD #1030 M3)", () => {
  // M3: when `main` advances during a rate-limit park, a resumed run must NOT discard its
  // valid mirrored checkpoint. On a RESUME with no origin/<branch> (unpushed branch) the
  // strict-descendant ancestry test against the moved default is skipped — the checkpoint
  // is adopted using the disjoint-history guard only (sharesHistory, already encoded in
  // checkpointExists). A FRESH run keeps the strict/floor behaviour, and an EXISTING
  // origin/<branch> keeps the strict test even on resume (competing published work).

  /** Worker A commits `milestones` real (committed) commits on `branch` off the current
   *  floor, then publishes them as the branch's brokered checkpoint WITHOUT pushing
   *  origin/<branch> (originExists=false, the incident shape). Returns the checkpoint tip. */
  async function publishCommittedCheckpoint(
    gitA: GitCache,
    bareA: string,
    issue: number,
    branch: string,
    milestones: string[],
  ): Promise<string> {
    const seed = await gitA.createOrAttachRunnerClone(bareA, issue, "run-A");
    let tip = gitIn(seed.path, ["rev-parse", "HEAD"]);
    for (const m of milestones) tip = commit(seed.path, m);
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
    const packed = await gitA.checkpointPack(bareA, branch);
    assert.ok(packed, "worker A builds a checkpoint pack from its tracking ref");
    assert.strictEqual(packed!.tipOid, tip, "the pack tip is the committed checkpoint tip");
    const pack = await drain(packed!.pack);
    assert.ok(pack.length > 0, "the checkpoint pack is non-empty");
    publishPackToOrigin(pack, tip, `refs/uzi-checkpoints/${branch}`);
    return tip;
  }

  it("RESUME adopts a checkpoint that DIVERGED from a default that moved during the park (no origin/<branch>)", async () => {
    const branch = "agent/issue-10301";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10301, branch, ["M1.txt", "M2.txt"]);

    // `main` advances during the park on a file DISJOINT from the milestones → the
    // checkpoint diverges from the new floor (it is no longer a strict descendant).
    const advancedFloor = advanceOriginMain("MAIN_ADVANCE.txt", "landed while parked\n");

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.strictEqual(floorSha, advancedFloor, "worker B's floor is the advanced main");
    // Preconditions: the checkpoint is genuinely DIVERGED (not reachable from the floor) but
    // still shares history with the floor (so the disjoint guard admits it).
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, cpTip]),
      "the checkpoint does NOT descend the advanced floor (diverged — the strict test would set it aside)",
    );
    assert.doesNotThrow(
      () => gitIn(bareB, ["merge-base", floorSha, cpTip]),
      "the checkpoint shares history with the floor (the disjoint-history guard admits it)",
    );

    // THE RESUME (resume=true, no origin/<branch>): adopt the checkpoint despite divergence.
    // issue #1042 M4: adoption is now gated on the owner anchor — pass THIS run's own persisted
    // checkpoint tip (== the mirrored checkpoint's SHA) so the legitimate same-run resume adopts.
    const rc = await gitB.createOrAttachRunnerClone(bareB, 10301, "run-B", true, cpTip);

    assert.strictEqual(rc.seededFrom, "checkpoint", "resume adopts the diverged checkpoint, not the default floor");
    assert.strictEqual(rc.baseCommit, cpTip, "the base is the checkpoint tip (committed milestones preserved)");
    assert.notStrictEqual(rc.checkpointSetAside, true, "the checkpoint is NOT set aside on resume");
    assert.ok(rc.priorCommits > 0, `priorCommits must be > 0 (milestones recovered), got ${rc.priorCommits}`);
    // The committed milestone commits are present on the working branch.
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), true, "milestone M1 recovered onto the branch");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M2.txt")), true, "milestone M2 recovered onto the branch");
    const subjects = gitIn(rc.path, ["log", "--format=%s"]);
    assert.ok(
      subjects.includes("work M1.txt") && subjects.includes("work M2.txt"),
      `both milestone commits are in the resulting history, got: ${JSON.stringify(subjects)}`,
    );
  });

  it("FRESH run (resume=false, no own-checkpoint tip) does NOT inherit a diverged checkpoint and does NOT set it aside (issue #1059 true-negative)", async () => {
    const branch = "agent/issue-10302";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10302, branch, ["M1.txt", "M2.txt"]);

    const advancedFloor = advanceOriginMain("MAIN_ADVANCE.txt", "landed while parked\n");

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.strictEqual(floorSha, advancedFloor);
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, cpTip]),
      "precondition: the checkpoint is diverged",
    );

    // A genuine FRESH run: resume=false AND no own-checkpoint tip (session_id == null, never
    // published). issue #1059 M1 — the checkpoint is a FOREIGN/prior run's work (ownerMatch
    // false), so it is neither adopted NOR set aside (setting aside would drive the #759
    // cherry-pick, re-importing the very foreign work the owner anchor keeps out). The run
    // cold-starts from the default floor, quietly-loudly (a warn, no checkpointSetAside flag).
    const rc = await gitB.createOrAttachRunnerClone(bareB, 10302, "run-B", false /*fresh*/, undefined /*no own tip*/);

    assert.strictEqual(rc.seededFrom, "default", "a fresh run falls through to the default floor");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the advanced default floor");
    assert.notStrictEqual(rc.checkpointSetAside, true, "a foreign/fresh checkpoint is NOT set aside (no #759 cherry-pick)");
    assert.strictEqual(rc.priorCommits, 0, "nothing recovered on a fresh cold start");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "a fresh run does NOT inherit the prior checkpoint's work");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M2.txt")), false, "…nor its second milestone");
  });

  it("RESUME with an EXISTING origin/<branch> still applies the strict-descendant test (diverged published origin wins)", async () => {
    const branch = "agent/issue-10303";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10303, branch, ["M1.txt", "M2.txt"]);

    // A COMPETING PUBLISHED origin/<branch> off the same floor but on a DIFFERENT line than
    // the checkpoint — genuinely competing published work the relaxed rule must NOT discard.
    const floorAtOrigin = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const floorTree = gitIn(fx.originPath, ["rev-parse", "HEAD^{tree}"]);
    const originBranchTip = mkCommit(fx.originPath, floorTree, [floorAtOrigin], "published divergent origin work");
    gitIn(fx.originPath, ["update-ref", `refs/heads/${branch}`, originBranchTip]);

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    assert.strictEqual(
      gitIn(bareB, ["rev-parse", `refs/remotes/origin/${branch}`]),
      originBranchTip,
      "precondition: origin/<branch> exists at the published divergent tip",
    );
    // The checkpoint does NOT descend origin/<branch> (they diverge) — so the strict test bites.
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", originBranchTip, cpTip]),
      "precondition: the checkpoint does not descend the published origin branch (diverged)",
    );

    // RESUME, but origin/<branch> EXISTS → the strict-descendant test is KEPT: the published
    // origin branch is the floor and wins; the checkpoint is set aside, not blindly adopted.
    // A matching owner-anchor tip does NOT relax this (the resume-adopt leg requires !originExists).
    const rc = await gitB.createOrAttachRunnerClone(bareB, 10303, "run-B", true, cpTip);

    assert.strictEqual(rc.seededFrom, "origin", "the published origin branch is the floor even on resume");
    assert.strictEqual(rc.baseCommit, originBranchTip, "the base is the origin branch tip, NOT the checkpoint");
    assert.strictEqual(rc.checkpointSetAside, true, "the diverged checkpoint is set aside — the strict test still bites");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "the checkpoint's work was NOT blindly adopted over published origin");
  });

  // issue #1042 M4 — the OWNER-ANCHOR guard. A resume adopts the mirrored checkpoint ONLY
  // when THIS run's own persisted checkpoint tip (claim.checkpoint_tip, threaded as
  // expectedCheckpointTip) is non-empty AND equals the checkpoint's current SHA. NULL (never
  // published) or a mismatch (a prior/foreign run's checkpoint) → do NOT adopt, fall through
  // to the origin/default floor, even though the checkpoint exists and shares history.

  it("RESUME with a NULL own-checkpoint tip does NOT adopt — a never-published run seeds off the floor, not a prior run's checkpoint", async () => {
    const branch = "agent/issue-10304";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    // A PRIOR run's checkpoint sits at refs/uzi-checkpoints/<branch>, sharing history with the
    // floor and (main not advanced) even strictly descending it — so ONLY the owner-anchor
    // guard, not the disjoint/strict test, can keep it from being adopted.
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10304, branch, ["M1.txt", "M2.txt"]);

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.doesNotThrow(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, cpTip]),
      "precondition: the checkpoint strictly descends the floor (so nothing BUT the tip guard blocks adoption)",
    );

    // THE RESUME with NO own-checkpoint tip (a run that never published its own checkpoint).
    const rc = await gitB.createOrAttachRunnerClone(bareB, 10304, "run-B", true /* resume */, undefined /* no own tip */);

    assert.strictEqual(rc.seededFrom, "default", "a never-published resume seeds off the default floor, NOT the checkpoint");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the default floor");
    assert.strictEqual(rc.priorCommits, 0, "nothing adopted — no prior run's committed work re-treaded");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "the prior checkpoint's work was NOT adopted");
  });

  it("RESUME with an own-checkpoint tip that MISMATCHES the mirrored checkpoint does NOT adopt — a foreign checkpoint is left aside", async () => {
    const branch = "agent/issue-10305";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10305, branch, ["M1.txt", "M2.txt"]);

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    // A DIFFERENT tip than the mirrored checkpoint's SHA — the floor sha stands in for "this
    // run's own persisted tip pointing somewhere other than the foreign checkpoint".
    assert.notStrictEqual(floorSha, cpTip, "precondition: the run's own tip differs from the mirrored checkpoint tip");

    const rc = await gitB.createOrAttachRunnerClone(bareB, 10305, "run-B", true /* resume */, floorSha /* mismatching own tip */);

    assert.strictEqual(rc.seededFrom, "default", "a mismatching own tip means the foreign checkpoint is NOT adopted");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the default floor, not the foreign checkpoint");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "the foreign checkpoint's work was NOT adopted");
  });

  it("RESUME whose own-checkpoint tip EQUALS the mirrored checkpoint STILL adopts (trap #5 — same-run legitimate resume)", async () => {
    const branch = "agent/issue-10306";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    // The server persists checkpoint_tip on EVERY publish (M2), so a same-run resume's own tip
    // equals its own checkpoint's current SHA — adoption must still fire.
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10306, branch, ["M1.txt", "M2.txt"]);

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);

    const rc = await gitB.createOrAttachRunnerClone(bareB, 10306, "run-B", true /* resume */, cpTip /* own tip == checkpoint */);

    assert.strictEqual(rc.seededFrom, "checkpoint", "a matching own tip adopts the checkpoint (same-run legitimate resume)");
    assert.strictEqual(rc.baseCommit, cpTip, "the base is the checkpoint tip — the run's own committed milestones");
    assert.notStrictEqual(rc.checkpointSetAside, true, "a matched checkpoint is NOT set aside");
    assert.ok(rc.priorCommits > 0, `priorCommits must be > 0 (own milestones adopted), got ${rc.priorCommits}`);
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), true, "milestone M1 adopted onto the branch");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M2.txt")), true, "milestone M2 adopted onto the branch");
  });

  // issue #1059 M1 — the OWNER-ANCHOR true-negatives for a genuine FRESH run (resume=false AND
  // no own-checkpoint tip). Before #1059 the strict-descendant leg (Path B) adopted ANY
  // mirrored checkpoint that strictly descended the floor and set aside a diverged one — with
  // NO owner check — so a fresh cross-worker run would re-tread a prior/foreign run's work. The
  // single owner-gated predicate now refuses both shapes: no adoption, no set-aside (the latter
  // would drive the #759 cherry-pick and re-import the very foreign work), just a LOUD warn.

  it("FRESH run does NOT adopt a strictly-DESCENDING foreign checkpoint — the headline #1059 AC (old Path B WOULD have adopted it)", async () => {
    const branch = "agent/issue-10307";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    // A prior/foreign run's checkpoint that STRICTLY DESCENDS the floor (main NOT advanced), so
    // ONLY the owner anchor — not the disjoint/strict test — can keep it from being adopted.
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10307, branch, ["M1.txt", "M2.txt"]);

    const { git: gitB, lines } = loggingWorker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.doesNotThrow(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, cpTip]),
      "precondition: the checkpoint STRICTLY descends the floor (old Path B would have adopted it)",
    );

    // A genuine FRESH run: resume=false, no own-checkpoint tip.
    const rc = await gitB.createOrAttachRunnerClone(bareB, 10307, "run-B", false /*fresh*/, undefined /*no own tip*/);

    assert.notStrictEqual(rc.seededFrom, "checkpoint", "a strictly-descending FOREIGN checkpoint is refused (the #1059 fix)");
    assert.strictEqual(rc.seededFrom, "default", "the fresh run seeds off the default floor");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the default floor, not the foreign checkpoint");
    assert.notStrictEqual(rc.checkpointSetAside, true, "not set aside either — no #759 cherry-pick of foreign work");
    assert.strictEqual(rc.priorCommits, 0, "no prior/foreign committed work re-treaded");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "the foreign checkpoint's work was NOT adopted");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M2.txt")), false, "…nor its second milestone");
    assert.ok(
      sawOwnerAnchorWarn(lines),
      `the LOUD owner-anchor set-aside warn must be emitted, got: ${JSON.stringify(lines)}`,
    );
  });

  it("FRESH run does NOT adopt or set aside a DIVERGED foreign checkpoint — emits the loud owner-anchor warn (issue #1059 true-negative)", async () => {
    const branch = "agent/issue-10308";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const cpTip = await publishCommittedCheckpoint(gitA, bareA, 10308, branch, ["M1.txt", "M2.txt"]);

    // main advances during the park → the foreign checkpoint DIVERGES from the new floor.
    const advancedFloor = advanceOriginMain("MAIN_ADVANCE.txt", "landed while parked\n");

    const { git: gitB, lines } = loggingWorker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.strictEqual(floorSha, advancedFloor, "worker B's floor is the advanced main");
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, cpTip]),
      "precondition: the checkpoint is diverged (old Path B would have SET IT ASIDE)",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 10308, "run-B", false /*fresh*/, undefined /*no own tip*/);

    assert.strictEqual(rc.seededFrom, "default", "the fresh run seeds off the default floor");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the advanced default floor");
    assert.notStrictEqual(rc.checkpointSetAside, true, "a diverged FOREIGN checkpoint is NOT set aside — no #759 cherry-pick");
    assert.strictEqual(rc.priorCommits, 0, "nothing recovered");
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), false, "the foreign checkpoint's work was NOT adopted");
    assert.ok(
      sawOwnerAnchorWarn(lines),
      `the LOUD owner-anchor set-aside warn must be emitted, got: ${JSON.stringify(lines)}`,
    );
  });
});

describe("GitCache.hasCommittedCheckpoint (issue #771 / PRD #759)", () => {
  // The report-only orphan guard must fire on a checkpoint that holds COMMITTED work but
  // NOT on one whose tip is only an abandoned `wip(park):` marker (the usage-limit park
  // planted it and nothing deletes it on resume). Each case constructs a real
  // refs/uzi-checkpoints/<branch> in a fresh bare with plumbing (commit-tree + update-ref)
  // and asserts the helper directly — deterministic, no timing/sleep.

  it("returns FALSE for a marker-only checkpoint (tip is a wip(park) marker whose parent is the floor) — issue #771", async () => {
    // THE failing-first case: a park with no committed work below the marker. The parent IS
    // the recovery floor, so nothing would be orphaned. Under the old existence-only guard
    // this returned TRUE (the bug — a legitimate report_only wrongly FAILED);
    // hasCommittedCheckpoint now returns false.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const branch = "agent/issue-771-marker-only";
    const floor = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    const floorTree = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main^{tree}"]);
    const marker = mkCommit(bareB, floorTree, [floor], `${WIP_PARK_COMMIT_PREFIX} interrupted work auto-saved`);
    gitIn(bareB, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);
    assert.strictEqual(
      gitIn(bareB, ["log", "-1", "--format=%s", marker]).startsWith(WIP_PARK_COMMIT_PREFIX),
      true,
      "precondition: the checkpoint tip is a wip(park) marker",
    );
    assert.strictEqual(
      gitIn(bareB, ["rev-parse", `${marker}^`]),
      floor,
      "precondition: the marker's parent is the floor (no committed work below it)",
    );
    assert.strictEqual(
      await gitB.hasCommittedCheckpoint(bareB, branch),
      false,
      "a marker-only checkpoint holds no committed work to orphan",
    );
  });

  it("returns TRUE for a real committed milestone tip (no marker)", async () => {
    // Byte-identical to the pre-PRD-759 behaviour for every non-marker checkpoint tip.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const branch = "agent/issue-771-real-tip";
    const floor = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    const floorTree = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main^{tree}"]);
    const milestone = mkCommit(bareB, floorTree, [floor], "real committed milestone");
    gitIn(bareB, ["update-ref", `refs/uzi-checkpoints/${branch}`, milestone]);
    assert.strictEqual(
      await gitB.hasCommittedCheckpoint(bareB, branch),
      true,
      "a genuine committed checkpoint tip still blocks a report-only completion",
    );
  });

  it("returns TRUE for a wip(park) marker sitting ON TOP OF a committed milestone (parent strictly descends the floor)", async () => {
    // fork → m1(committed) → wip-marker: the marker's parent (m1) strictly descends the
    // floor, so there IS committed work below the marker to orphan.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const branch = "agent/issue-771-marker-over-milestone";
    const floor = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    const floorTree = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main^{tree}"]);
    const milestone = mkCommit(bareB, floorTree, [floor], "real committed milestone");
    const marker = mkCommit(bareB, floorTree, [milestone], `${WIP_PARK_COMMIT_PREFIX} interrupted work auto-saved`);
    gitIn(bareB, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);
    assert.notStrictEqual(milestone, floor, "precondition: the milestone strictly descends the floor");
    assert.strictEqual(
      await gitB.hasCommittedCheckpoint(bareB, branch),
      true,
      "a committed milestone below the marker still blocks",
    );
  });

  it("returns FALSE when no checkpoint ref exists", async () => {
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const branch = "agent/issue-771-no-ref";
    assert.strictEqual(
      refInBare(bareB, `refs/uzi-checkpoints/${branch}`),
      false,
      "precondition: no checkpoint ref for this branch",
    );
    assert.strictEqual(await gitB.hasCommittedCheckpoint(bareB, branch), false);
  });

  it("returns TRUE for a wip(park) marker over a DIVERGED committed milestone (parent diverges from the floor — main advanced during the park)", async () => {
    // Case 4, the direction the old `isAncestor(floor, parent) && parent !== floor` return
    // MISSED: the milestone M and the floor F share a common ancestor but neither contains
    // the other (the "main advanced during a park" shape). M is a genuine committed milestone
    // a report-only completion would orphan, so the helper must BLOCK (true). The old return
    // yielded FALSE here (isAncestor(floor, M) is false for a diverged M), the dangerous
    // under-block; the fix's `!isAncestor(parent, floor)` yields true, mirroring the reseed's
    // diverged-WIP leg (git.ts:713).
    const gitB = worker("workerB");
    // Build the floor A→B: advance origin/main once so its tip B has a parent A, THEN clone
    // worker B's bare so its refs/remotes/origin/main resolves to B.
    const floorB = advanceOriginMain("floor-advance.txt", "second floor commit\n");
    const bareB = await gitB.ensureClone(fx.originPath);
    const branch = "agent/issue-771-diverged-milestone";
    const floor = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.strictEqual(floor, floorB, "precondition: worker B's floor is the advanced tip B");
    const forkA = gitIn(bareB, ["rev-parse", `${floor}^`]); // A = parent of the floor tip B
    const forkTree = gitIn(bareB, ["rev-parse", `${forkA}^{tree}`]);
    // M: a committed milestone whose parent is A (NOT B), so M and B diverge — neither is an
    // ancestor of the other.
    const milestone = mkCommit(bareB, forkTree, [forkA], "diverged committed milestone");
    const marker = mkCommit(bareB, forkTree, [milestone], `${WIP_PARK_COMMIT_PREFIX} interrupted work auto-saved`);
    gitIn(bareB, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", milestone, floor]),
      "precondition: the milestone is NOT an ancestor of the floor (diverged)",
    );
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floor, milestone]),
      "precondition: the floor is NOT an ancestor of the milestone (diverged, not merely behind)",
    );
    assert.strictEqual(
      await gitB.hasCommittedCheckpoint(bareB, branch),
      true,
      "a diverged committed milestone below the marker still blocks a report-only completion",
    );
  });

  it("returns FALSE for a root-commit wip(park) marker (marker with no parent)", async () => {
    // A marker planted on a repo with no commit below it — nothing committed to orphan.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const branch = "agent/issue-771-root-marker";
    const floorTree = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main^{tree}"]);
    const rootMarker = mkCommit(bareB, floorTree, [], `${WIP_PARK_COMMIT_PREFIX} root-commit park`);
    gitIn(bareB, ["update-ref", `refs/uzi-checkpoints/${branch}`, rootMarker]);
    assert.throws(
      () => gitIn(bareB, ["rev-parse", "--verify", `${rootMarker}^`]),
      "precondition: the marker is a root commit (no parent)",
    );
    assert.strictEqual(
      await gitB.hasCommittedCheckpoint(bareB, branch),
      false,
      "a root-commit marker has nothing committed below to orphan",
    );
  });
});
