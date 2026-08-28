import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
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
// (2) the reseed then PREFERS that mirrored checkpoint; (3) the strict-descendant guard
// (git.ts:479) is load-bearing — a checkpoint that merely equals the floor is NOT seeded
// as a recovery, so the positive assertion is non-vacuous.

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

    // THE RESEED: worker B seeds a fresh cross-worker run (no local tracking ref → NOT
    // ownedHere) off the mirrored checkpoint.
    const rc = await gitB.createOrAttachRunnerClone(bareB, 628, "run-B");

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

    // git.ts:479's strict-descendant condition is LOAD-BEARING: an equal checkpoint
    // recovers nothing, so the reseed must fall through to the default floor rather than
    // seed "checkpoint". This is the guard that makes the positive test above non-vacuous.
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

  it("recovers a DIVERGED wip(park) checkpoint onto the advanced floor as an uncommitted change (cross-worker, PRD #759 M4 leg #4)", async () => {
    // Leg #4: `main` advanced DURING the park, so the mirrored checkpoint (a wip(park)
    // marker over the OLD floor) is not a strict descendant of the new floor and the
    // strict-descendant guard set it aside. The marker's parent is the pre-park floor, an
    // ancestor of the advanced floor, and the WIP touches a file disjoint from main's
    // advance — so the WIP delta cherry-picks CLEAN onto the new floor and is recovered as
    // an uncommitted change. This is the leg that shipped DOA (M2 cherry-picked the ref
    // NAME, absent from the `--shared --no-checkout` clone) — see the pre-fix note below.
    const branch = "agent/issue-760";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath); // origin/main = the OLD floor (C0)
    const { marker, parent } = await publishWipParkCheckpoint(
      gitA, bareA, 760, branch, "WIP.txt", "diverged in-progress work\n",
    );

    // main ADVANCES during the park, on a file DISJOINT from the WIP (so the pick is clean).
    const advancedFloor = advanceOriginMain("MAIN_ADVANCE.txt", "landed while parked\n");
    assert.notStrictEqual(advancedFloor, parent, "the floor genuinely advanced past the fork point");

    // Worker B: a fresh bare that fetches the advanced floor AND the (now diverged) checkpoint.
    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.strictEqual(floorSha, advancedFloor, "worker B's floor is the advanced main");
    assert.strictEqual(
      refInBare(bareB, `refs/uzi-checkpoints/${branch}`) && gitIn(bareB, ["rev-parse", `refs/uzi-checkpoints/${branch}`]),
      marker,
      "the diverged checkpoint mirrored in at the wip(park) marker",
    );
    // The checkpoint is genuinely DIVERGED — not reachable from the floor — else this is not leg #4.
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, marker]),
      "the checkpoint does NOT descend the advanced floor (it is diverged, set aside)",
    );
    // …and the marker's parent (fork point) IS an ancestor of the floor — the recoverable shape.
    assert.doesNotThrow(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", parent, floorSha]),
      "the marker's parent is an ancestor of the advanced floor (no committed work below the marker)",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 760, "run-B");

    // (a) the WIP content is recovered as an UNCOMMITTED change on the advanced floor.
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
    // The advanced floor is present too (we sit ON the new floor, not the old one).
    assert.strictEqual(fs.existsSync(path.join(rc.path, "MAIN_ADVANCE.txt")), true, "the advanced-main file is on the floor");

    // (b) the recovery is surfaced and set-aside is cleared; no wip(park) marker in history.
    assert.strictEqual(rc.wipRecovered, true, "wipRecovered signals the diverged-WIP recovery");
    assert.notStrictEqual(rc.checkpointSetAside, true, "a recovered checkpoint is no longer set aside");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the advanced floor (priorCommits stays 0, no committed work recovered)");
    assert.strictEqual(rc.priorCommits, 0, "no committed work was recovered — only the uncommitted WIP");
    const subjects = gitIn(rc.path, ["log", "--format=%s"]).split("\n");
    assert.ok(
      subjects.every((s) => !s.startsWith(WIP_PARK_COMMIT_PREFIX)),
      `no wip(park): commit in the resumed branch history, got: ${JSON.stringify(subjects)}`,
    );
  });

  it("SAFE-FAILS a diverged wip(park) checkpoint whose WIP conflicts with main's advance, leaving a pristine floor (leg #4)", async () => {
    // The recoverable shape (marker parent is a floor ancestor) but the WIP touches the SAME
    // file main moved after the park, so the cherry-pick CONFLICTS. Required safe failure
    // (SC#1(b)): abort the half-applied pick, hard-reset to the floor, report failure —
    // never a half-merged tree, never a silent drop.
    const branch = "agent/issue-761";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const { marker, parent } = await publishWipParkCheckpoint(
      gitA, bareA, 761, branch, "CONTESTED.txt", "the parked worker's edit\n",
    );

    // main advances by touching the VERY SAME file, with different content → add/add conflict.
    const advancedFloor = advanceOriginMain("CONTESTED.txt", "main's landed edit\n");
    assert.doesNotThrow(
      () => gitIn(fx.originPath, ["merge-base", "--is-ancestor", parent, advancedFloor]),
      "the fork point is still an ancestor of the floor (so the guard admits the pick; the conflict is the WIP itself)",
    );

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, marker]),
      "the checkpoint is diverged (set aside)",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 761, "run-B");

    // Recovery FAILED — set aside, not recovered.
    assert.notStrictEqual(rc.wipRecovered, true, "a conflicting WIP is not recovered");
    assert.strictEqual(rc.checkpointSetAside, true, "the checkpoint stays SET ASIDE (loud notice preserved)");
    assert.strictEqual(rc.baseCommit, floorSha, "the base is the pristine advanced floor");

    // The clone tree is a PRISTINE floor: main's version of the file, no conflict markers, no
    // staged WIP.
    const contested = fs.readFileSync(path.join(rc.path, "CONTESTED.txt"), "utf8");
    assert.strictEqual(contested, "main's landed edit\n", "the floor's content is intact (WIP was not force-applied)");
    assert.ok(!/^<{7}|^={7}|^>{7}/m.test(contested), "no conflict markers left in the tree");
    assert.strictEqual(gitIn(rc.path, ["status", "--porcelain"]), "", "no staged/uncommitted residue — a clean floor");
  });

  it("LEAVES SET ASIDE a diverged checkpoint with a committed milestone BELOW the marker — no cherry-pick (Defect 2 guard, leg #4)", async () => {
    // `fork → m1(committed) → wip-marker`, with m1 NOT on the advanced floor. Cherry-picking
    // only the marker tip would apply the WIP delta cleanly (it is disjoint from main's
    // advance) and flip checkpointSetAside=false — silently dropping m1 AND suppressing the
    // set-aside notice. The Defect-2 guard (marker PARENT must be an ancestor of the floor)
    // bites here: m1 is not an ancestor of the floor, so leg #4 must NOT cherry-pick and must
    // keep the checkpoint set aside for a human.
    const branch = "agent/issue-762";
    const gitA = worker("workerA");
    const bareA = await gitA.ensureClone(fx.originPath);
    const { marker, parent } = await publishWipParkCheckpoint(
      gitA, bareA, 762, branch, "WIP.txt", "in-progress over a committed milestone\n",
      ["M1.txt"], // one committed-but-unpushed milestone below the marker
    );

    // main advances on a DISJOINT file — so absent the guard the WIP delta would pick clean
    // and the silent drop of M1 would occur. The guard is what prevents it.
    const advancedFloor = advanceOriginMain("MAIN_ADVANCE.txt", "landed while parked\n");
    // The marker's parent is m1, which is NOT an ancestor of the advanced floor.
    assert.throws(
      () => gitIn(fx.originPath, ["merge-base", "--is-ancestor", parent, advancedFloor]),
      "the marker's parent (m1) is NOT an ancestor of the floor — committed divergence below the marker",
    );

    const gitB = worker("workerB");
    const bareB = await gitB.ensureClone(fx.originPath);
    const floorSha = gitIn(bareB, ["rev-parse", "refs/remotes/origin/main"]);
    assert.throws(
      () => gitIn(bareB, ["merge-base", "--is-ancestor", floorSha, marker]),
      "the checkpoint is diverged (set aside)",
    );

    const rc = await gitB.createOrAttachRunnerClone(bareB, 762, "run-B");

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
