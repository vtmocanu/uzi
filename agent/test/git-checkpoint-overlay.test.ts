import { fixtureFetchTracking, fixtureUpdateTracking } from "./runner-tracking-fixture.js";
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import {
  GitCache,
  ScratchPublicationError,
  OVERLAY_COMMIT_PREFIX,
  WIP_PARK_COMMIT_PREFIX,
  type CheckpointOverlayContext,
} from "../src/git.js";

// PRD #1062 M2 (#1036) — Contract B: the worker-side `.github/workflows` overlay.
//
// checkpointPack builds a genuine fast-forward wrapper commit `O_ov` (subject prefixed
// `ckpt(overlay):`) whose `.github/workflows` tree equals the default's, so a branch behind
// `main` on those files checkpoints durably instead of being rejected `workflow_scope`. The
// UNCHANGED broker pushes it and adoption (runnerCloneForBranch) PEELS it — discards the swapped
// tree and re-points the base to the overlay's LAST parent (= realTip). These are REAL-git
// end-to-end cases over the cross-worker harness (bare repos, commit-tree fixtures), each
// exercising a distinct branch of the gate/synthesis/peel.

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const WF = ".github/workflows/ci.yml";

const cleanups: Array<() => void> = [];
afterEach(() => {
  // Run every cleanup even when one throws (issue #2020), then rethrow the first error.
  let first: unknown;
  let failed = false;
  while (cleanups.length) {
    try {
      cleanups.pop()!();
    } catch (err) {
      if (!failed) first = err;
      failed = true;
    }
  }
  if (failed) throw first;
});

/** A fixture whose origin `main` carries the given files (a `.github/workflows/ci.yml` by
 *  default), auto-cleaned after the test. */
function mk(files: Record<string, string> = { [WF]: "name: ci\non: push\njobs: {}\n# v1\n" }): Fixture {
  const fx = makeFixture(files);
  cleanups.push(() => fx.cleanup());
  return fx;
}

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

/** A distinct worker machine: its OWN dataDir → its own bare. */
function worker(fx: Fixture, name: string): GitCache {
  const dataDir = path.join(fx.dataDir, name);
  fs.mkdirSync(dataDir, { recursive: true });
  return new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
}

/** Commit `file` (default a non-workflow file) in the runner clone; return the new HEAD. */
function commit(dir: string, file: string, content = `${file}\n`): string {
  const target = path.join(dir, file);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, content);
  gitIn(dir, ["add", file]);
  gitIn(dir, [...IDENT, "commit", "-m", `work ${file}`]);
  return gitIn(dir, ["rev-parse", "HEAD"]);
}

/** Advance origin `main` by rewriting its workflow file — main moving its `.github/workflows`
 *  forward AFTER a branch's base, the exact "behind on workflows" shape. Returns the new tip. */
function advanceOriginWorkflow(fx: Fixture, content: string): string {
  const target = path.join(fx.originPath, WF);
  fs.mkdirSync(path.dirname(target), { recursive: true });
  fs.writeFileSync(target, content);
  gitIn(fx.originPath, ["add", WF]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", "main advances its workflow"]);
  return gitIn(fx.originPath, ["rev-parse", "HEAD"]);
}

/** Advance origin `main` by DELETING its whole `.github/workflows/` — the #627 deleted-workflows
 *  edge (default has no workflow tree). Returns the new tip. */
function deleteOriginWorkflow(fx: Fixture): string {
  gitIn(fx.originPath, ["rm", "-r", WF.split("/").slice(0, 2).join("/")]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", "main deletes its workflows"]);
  return gitIn(fx.originPath, ["rev-parse", "HEAD"]);
}

function subjectOf(bare: string, sha: string): string {
  return gitIn(bare, ["log", "-1", "--format=%s", sha]);
}

/** The tree OID of `<ref>:.github/workflows` (or "" when absent). */
function wfTreeOf(bare: string, ref: string): string {
  try {
    return gitIn(bare, ["rev-parse", `${ref}:.github/workflows`]);
  } catch {
    return "";
  }
}

/** Tokens after the commit itself in `rev-list --parents -n 1` — the parent list. */
function parentsOf(bare: string, sha: string): string[] {
  return gitIn(bare, ["rev-list", "--parents", "-n", "1", sha]).split(/\s+/).slice(1);
}

async function drain(r: Readable): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const c of r) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

/** Land a checkpoint pack into origin and point the brokered ref at its tip — the server-side
 *  effect of client.publishCheckpoint -> pushbroker, with local git. */
function publishPackToOrigin(fx: Fixture, pack: Buffer, tip: string, ref: string): void {
  execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"], {
    env: GIT_ENV,
    input: pack,
    stdio: ["pipe", "pipe", "pipe"],
  });
  gitIn(fx.originPath, ["update-ref", ref, tip]);
}

const ctx = (prev?: string): CheckpointOverlayContext => ({ defaultBranch: "main", prevCheckpointTip: prev });

describe("checkpoint .github/workflows overlay (PRD #1062 M2, #1036)", () => {
  it("behind, not workflow-modified: builds O_ov aligned to default; adoption peels to realTip", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;

    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    const realTip = commit(seed.path, "M1.txt"); // non-workflow work off the v1-workflow base
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n"); // main moves its workflow
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed, "checkpointPack returns a pack");
    const ov = packed!.tipOid;
    assert.notStrictEqual(ov, realTip, "an overlay wrapper was packed, not the raw realTip");
    assert.ok(subjectOf(bareA, ov).startsWith(OVERLAY_COMMIT_PREFIX), "subject marks it an overlay");

    const defTip = gitIn(bareA, ["rev-parse", "refs/remotes/origin/main"]);
    assert.strictEqual(
      wfTreeOf(bareA, ov),
      wfTreeOf(bareA, defTip),
      "O_ov's .github/workflows tree equals the default's",
    );
    const parents = parentsOf(bareA, ov);
    assert.strictEqual(parents.length, 1, "single parent for a first overlay");
    assert.strictEqual(parents[parents.length - 1], realTip, "the LAST parent is realTip");

    // Publish O_ov and reseed a DIFFERENT worker: the adoption peel.
    const pack = await drain(packed!.pack);
    publishPackToOrigin(fx, pack, ov, checkpointRef);
    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    assert.strictEqual(gitIn(bareB, ["rev-parse", checkpointRef]), ov, "checkpoint ref mirrored");

    // resume + this run's own persisted tip (== O_ov) so the #1059 owner anchor admits the adopt.
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, noProofReseed, "run-B", true, ov);
    assert.strictEqual(rc.seededFrom, "checkpoint", "seeded from the checkpoint");
    assert.strictEqual(rc.baseCommit, realTip, "the peel landed the base on realTip (overlay discarded)");
    assert.strictEqual(gitIn(rc.path, ["rev-parse", "HEAD"]), realTip, "the clone is checked out at realTip");
    assert.strictEqual(
      gitIn(rc.path, ["diff", "--cached", "--name-only"]),
      "",
      "no staged .github diff — the swapped overlay tree was discarded, not kept",
    );
  });

  it("not-behind: workflows already equal the default ⇒ ships realTip (no overlay)", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    const realTip = commit(seed.path, "M1.txt"); // no workflow change; main not advanced
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "workflows already equal ⇒ raw realTip is shipped");
  });

  it("workflow-MODIFIED by the branch ⇒ ships realTip (the gate holds, #377 owns it at finalize)", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    commit(seed.path, "M1.txt");
    // The branch ITSELF edits a workflow file — the doomed-at-finalize shape.
    const realTip = commit(seed.path, WF, "name: ci\non: push\njobs: {}\n# branch-edit\n");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n"); // also behind, so gate-2 passes
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "a workflow-modifying branch ships realTip (no overlay)");
    assert.ok(
      !subjectOf(bareA, packed!.tipOid).startsWith(OVERLAY_COMMIT_PREFIX),
      "the shipped tip is the real commit, not an overlay wrapper",
    );
  });

  it("newline workflow edit ships realTip without a clobbering overlay", async () => {
    const workflow = ".github/workflows/line\nfeed.yml";
    const fx = mk({ [WF]: "name: ci\n# v1\n", [workflow]: "name: original\n" });
    const branch = "agent/issue-1037";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1037, noProofReseed, "run-A");
    const realTip = commit(seed.path, workflow, "name: branch\n");
    advanceOriginWorkflow(fx, "name: ci\n# v2\n");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "the branch workflow edit is never overlaid");
    assert.ok(!subjectOf(bareA, packed!.tipOid).startsWith(OVERLAY_COMMIT_PREFIX));
  });

  it("stacked: realTip is a wip(park) marker AND behind ⇒ overlay built; peel then soft-reset the marker", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    const realParent = commit(seed.path, "M1.txt"); // the last REAL commit (marker's parent)
    fs.writeFileSync(path.join(seed.path, "WIP.txt"), "in-progress\n"); // genuinely uncommitted
    assert.strictEqual(await gitA.commitWipMarker(seed.path), true, "a wip(park) marker was planted");
    const marker = gitIn(seed.path, ["rev-parse", "HEAD"]);
    assert.ok(subjectOf(seed.path, marker).startsWith(WIP_PARK_COMMIT_PREFIX));
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    const ov = packed!.tipOid;
    assert.ok(subjectOf(bareA, ov).startsWith(OVERLAY_COMMIT_PREFIX), "overlay wraps the marker tip");
    assert.strictEqual(parentsOf(bareA, ov)[0], marker, "the overlay's (last) parent is the marker");

    const pack = await drain(packed!.pack);
    publishPackToOrigin(fx, pack, ov, checkpointRef);
    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, noProofReseed, "run-B", true, ov);

    // Peel discards the overlay → base becomes the marker; the wip-park soft-reset then lands the
    // branch on realParent with the WIP uncommitted.
    assert.strictEqual(rc.baseCommit, realParent, "peel(overlay)→marker→soft-reset landed on realParent");
    assert.strictEqual(rc.wipRecovered, true, "the wip(park) marker was recovered to uncommitted");
    assert.strictEqual(gitIn(rc.path, ["rev-parse", "HEAD"]), realParent, "HEAD sits on realParent");
    assert.strictEqual(
      fs.existsSync(path.join(rc.path, "WIP.txt")),
      true,
      "the marker's WIP content is present (uncommitted)",
    );
  });

  it("pinned overlay gates inspect the pinned real tip after tracking advances", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bare = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bare, 1036, noProofReseed, "run-A");
    const floor = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    const pinnedTip = commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await fixtureFetchTracking(gitA, bare, seed.path, branch, "run-A");
    commit(seed.path, WF, "name: ci\non: push\njobs: {}\n# branch edit\n");
    await fixtureFetchTracking(gitA, bare, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bare, branch, ctx(), { tipSha: pinnedTip, excludeSha: floor });
    assert.ok(packed);
    assert.ok(subjectOf(bare, packed.tipOid).startsWith(OVERLAY_COMMIT_PREFIX));
    assert.equal(parentsOf(bare, packed.tipOid).at(-1), pinnedTip);
  });

  it("second sequential overlay: parent[0]=prev, last parent=realTip2, and prev is its ancestor", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");
    const p1 = await gitA.checkpointPack(bareA, branch, ctx());
    const ov1 = p1!.tipOid;
    assert.ok(subjectOf(bareA, ov1).startsWith(OVERLAY_COMMIT_PREFIX));

    const realTip2 = commit(seed.path, "M2.txt"); // more work → a second checkpoint
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");
    const p2 = await gitA.checkpointPack(bareA, branch, ctx(ov1));
    const ov2 = p2!.tipOid;
    const parents = parentsOf(bareA, ov2);
    assert.strictEqual(parents.length, 2, "a chained overlay carries two parents");
    assert.strictEqual(parents[0], ov1, "parent[0] is the prior overlay (base FIRST)");
    assert.strictEqual(parents[parents.length - 1], realTip2, "the LAST parent is realTip2");
    // The base-first shape is what the depth-1 broker accepts on; the true depth-1
    // broker-accept property is proven separately by a pushbroker test.
    assert.doesNotThrow(
      () => gitIn(bareA, ["merge-base", "--is-ancestor", ov1, ov2]),
      "O_ov1 is an ancestor of O_ov2 (a genuine fast-forward chain)",
    );
  });

  it("adopting a CHAINED (2-parent) overlay peels to the LAST parent (realTip2), not the prior overlay", async () => {
    // Guards the peel's parent-COUNT computation: a naive `^1` peel would land on the prior
    // overlay O_ov1 (parent[0]) instead of realTip2 (parent[nParents]), re-polluting the branch
    // with the swapped `.github` tree.
    const fx = mk();
    const branch = "agent/issue-1036";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");
    const ov1 = (await gitA.checkpointPack(bareA, branch, ctx()))!.tipOid;

    const realTip2 = commit(seed.path, "M2.txt");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");
    const p2 = await gitA.checkpointPack(bareA, branch, ctx(ov1));
    const ov2 = p2!.tipOid;
    assert.strictEqual(parentsOf(bareA, ov2).length, 2, "ov2 is a 2-parent chained overlay");

    publishPackToOrigin(fx, await drain(p2!.pack), ov2, checkpointRef);
    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, noProofReseed, "run-B", true, ov2);
    assert.strictEqual(rc.baseCommit, realTip2, "peeled to the LAST parent (realTip2), not O_ov1");
    assert.strictEqual(gitIn(rc.path, ["rev-parse", "HEAD"]), realTip2, "HEAD sits on realTip2");
    assert.strictEqual(
      gitIn(rc.path, ["diff", "--cached", "--name-only"]),
      "",
      "no staged .github diff — the chained overlay tree was discarded",
    );
  });

  it("edge — default DELETED its workflows: rm-only overlay, never throws", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    const realTip = commit(seed.path, "M1.txt"); // realTip still carries the v1 workflow
    deleteOriginWorkflow(fx); // default now has NO .github/workflows tree
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    const ov = packed!.tipOid;
    assert.ok(subjectOf(bareA, ov).startsWith(OVERLAY_COMMIT_PREFIX), "overlay built via rm-only");
    assert.strictEqual(wfTreeOf(bareA, ov), "", "the overlay has no .github/workflows (equalised to none)");
    assert.strictEqual(parentsOf(bareA, ov)[0], realTip, "last parent is realTip");
  });

  it("edge — no .github anywhere: not behind ⇒ ships realTip, never throws", async () => {
    const fx = mk({ "README.md": "# no workflows here\n" });
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    const realTip = commit(seed.path, "M1.txt");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "no workflows on either side ⇒ raw realTip");
  });

  it("determinism: two builds with identical inputs yield a byte-identical O_ov OID", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const a = await gitA.checkpointPack(bareA, branch, ctx());
    const b = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(a && b);
    assert.ok(subjectOf(bareA, a!.tipOid).startsWith(OVERLAY_COMMIT_PREFIX));
    assert.strictEqual(a!.tipOid, b!.tipOid, "identical inputs ⇒ identical overlay OID");
  });

  it("no overlay context ⇒ behaves exactly as today (ships the raw tracking tip)", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    const realTip = commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n"); // behind, but no overlay asked for
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch); // no 3rd arg
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "without an overlay context the raw realTip ships");
  });

  it("provenance gate: a FORGED ckpt(overlay): commit on a non-checkpoint base is NOT peeled (agent work preserved)", async () => {
    // Issue #1036 review (F1): the overlay peel must fire ONLY when seededFrom === "checkpoint"
    // — the worker-built, api-brokered ref the agent cannot push to. Here the agent commits REAL
    // work but forges the `ckpt(overlay):` subject on its OWN tracking tip. On reseed that tip is
    // ownedHere (seededFrom "tracking"), so the peel must be SKIPPED; peeling it would discard the
    // agent's commit (the peel throws the tip's tree away). A subject prefix is not provenance
    // across the agent trust boundary. On the UNFIXED (ungated) code this test fails: the forged
    // commit is peeled to its parent and M1.txt vanishes.
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    fs.writeFileSync(path.join(seed.path, "M1.txt"), "real agent work\n");
    gitIn(seed.path, ["add", "M1.txt"]);
    gitIn(seed.path, [...IDENT, "commit", "-m", `${OVERLAY_COMMIT_PREFIX} forged by the agent`]);
    const forged = gitIn(seed.path, ["rev-parse", "HEAD"]);
    // Stamp the worker-side tracking ref for run-A so the reseed is ownedHere.
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const rc = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    assert.strictEqual(rc.seededFrom, "tracking", "seeded from the owned tracking ref, not a checkpoint");
    assert.strictEqual(
      rc.baseCommit,
      forged,
      "the forged overlay-subject commit is NOT peeled (provenance gate: peel only on seededFrom 'checkpoint')",
    );
    assert.strictEqual(
      fs.existsSync(path.join(rc.path, "M1.txt")),
      true,
      "the agent's real work is preserved, not discarded by a forged-prefix peel",
    );
  });

  it("provenance gate: a FORGED ckpt(overlay): commit shipped as a PLAIN checkpoint (real-work diff) is NOT peeled", async () => {
    // Issue #1036 review (F1, part 2): seededFrom === "checkpoint" is necessary but NOT
    // sufficient. An agent can commit its OWN realTip with a forged `ckpt(overlay):` subject;
    // if it ships as a PLAIN checkpoint (not behind on workflows ⇒ no overlay built) it lands
    // on the worker-built checkpoint ref, so seededFrom is "checkpoint" too. The STRUCTURAL
    // check is the unforgeable discriminator: a genuine overlay differs from its last parent
    // ONLY under .github/workflows; this forged commit changes M1.txt, so the peel is refused
    // and the work is preserved. On the pre-structural-check code this fails (M1.txt is lost).
    const fx = mk();
    const branch = "agent/issue-1036";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, noProofReseed, "run-A");
    // Real work, forged subject. main is NOT advanced ⇒ not behind ⇒ checkpointPack ships the
    // raw tip as a plain checkpoint (no overlay).
    fs.writeFileSync(path.join(seed.path, "M1.txt"), "real agent work\n");
    gitIn(seed.path, ["add", "M1.txt"]);
    gitIn(seed.path, [...IDENT, "commit", "-m", `${OVERLAY_COMMIT_PREFIX} forged plain`]);
    const forged = gitIn(seed.path, ["rev-parse", "HEAD"]);
    await fixtureFetchTracking(gitA, bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, forged, "not behind ⇒ plain checkpoint ships the raw (forged-subject) tip");
    const pack = await drain(packed!.pack);
    publishPackToOrigin(fx, pack, forged, checkpointRef);

    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, noProofReseed, "run-B", true, forged);
    assert.strictEqual(rc.seededFrom, "checkpoint", "seeded from the checkpoint ref (the forge vector)");
    assert.strictEqual(
      rc.baseCommit,
      forged,
      "the forged plain commit is NOT peeled — its diff is real work, not a .github/workflows swap",
    );
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), true, "the agent's committed work is preserved");
  });
});

// issue #2733 — a RAW tick/iteration publish after an overlay publish must stay a strict descendant
// of the remote checkpoint ref P. `checkpointPack(..., chainFrom)` chains the raw tip N over P with
// a `ckpt(overlay):` commit whose parents are [P, N] (base first) and whose `.github/workflows`
// subtree is P's. Each case below is real git over bare repos and commit-tree fixtures.
describe("checkpoint chain over a prior overlay tip (issue #2733)", () => {
  const branch = "agent/issue-2733";
  const checkpointRef = `refs/uzi-checkpoints/${branch}`;
  const V2 = "name: ci\non: push\njobs: {}\n# v2\n";

  function workerWith(fx: Fixture, name: string, opts: ConstructorParameters<typeof GitCache>[3] = {}): GitCache {
    const dataDir = path.join(fx.dataDir, name);
    fs.mkdirSync(dataDir, { recursive: true });
    return new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions(opts));
  }

  interface Scene {
    fx: Fixture;
    git: GitCache;
    bare: string;
    clone: string;
    /** the first real tip R and the overlay P built over it (main is behind on workflows). */
    r: string;
    p: string;
  }

  /** Worker A commits R, main advances its workflow, an overlay P is built over R and "published"
   *  (the ref points at P in origin). */
  async function overlayScene(
    opts: ConstructorParameters<typeof GitCache>[3] = {},
    files?: Record<string, string>,
  ): Promise<Scene> {
    const fx = mk(files);
    const git = workerWith(fx, "A", opts);
    const bare = await git.ensureClone(fx.originPath);
    const seed = await git.createOrAttachRunnerClone(bare, 2733, noProofReseed, "run-A");
    const r = commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, V2);
    await fixtureFetchTracking(git, bare, seed.path, branch, "run-A");
    const packed = await git.checkpointPack(bare, branch, ctx());
    assert.ok(packed);
    const p = packed.tipOid;
    assert.ok(subjectOf(bare, p).startsWith(OVERLAY_COMMIT_PREFIX), "scene: an overlay P was built");
    publishPackToOrigin(fx, await drain(packed.pack), p, checkpointRef);
    return { fx, git, bare, clone: seed.path, r, p };
  }

  /** Merge origin `main` (now carrying the v2 workflow) into the runner clone's branch. */
  function mergeMainInto(sc: Scene): void {
    gitIn(sc.clone, ["fetch", sc.fx.originPath, "main"]);
    gitIn(sc.clone, [...IDENT, "merge", "--no-edit", "-m", "merge main", "FETCH_HEAD"]);
  }

  async function track(sc: Scene): Promise<void> {
    await fixtureFetchTracking(sc.git, sc.bare, sc.clone, branch, "run-A");
  }

  /** A plain (non-wrapper) commit built directly in the bare with `tree` and `parents`. */
  function rawCommit(bare: string, tree: string, parents: string[], subject: string): string {
    const args = [...IDENT, "commit-tree", tree];
    for (const p of parents) args.push("-p", p);
    return gitIn(bare, [...args, "-m", subject]);
  }

  const chainPack = (
    sc: Scene,
    chainFrom: string | readonly (string | undefined)[] | undefined,
    extra: { beforePack?: (sha: string) => Promise<void>; onStep?: (s: string) => void; git?: GitCache } = {},
  ) =>
    (extra.git ?? sc.git).checkpointPack(
      sc.bare, branch, undefined, undefined, extra.onStep as never, undefined, extra.beforePack,
      typeof chainFrom === "string" ? [chainFrom] : chainFrom,
    );

  it("chains a raw tip over the overlay: parents [P, N], P's workflow subtree, N's everything else", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    const packed = await chainPack(sc, sc.p);
    assert.ok(packed);
    const c = packed.tipOid;
    assert.notStrictEqual(c, n);
    assert.ok(subjectOf(sc.bare, c).startsWith(OVERLAY_COMMIT_PREFIX));
    assert.deepStrictEqual(parentsOf(sc.bare, c), [sc.p, n], "base first, real tip last");
    assert.strictEqual(wfTreeOf(sc.bare, c), wfTreeOf(sc.bare, sc.p), "P's workflow subtree");
    assert.strictEqual(gitIn(sc.bare, ["show", `${c}:M2.txt`]), "M2.txt", "N's content");
    assert.doesNotThrow(() => gitIn(sc.bare, ["merge-base", "--is-ancestor", sc.p, c]), "strict descendant of P");
    // The chain publishes and a fresh worker's adoption peels it to N.
    publishPackToOrigin(sc.fx, await drain(packed.pack), c, checkpointRef);
    const gitB = worker(sc.fx, "B");
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, c);
    assert.strictEqual(rc.seededFrom, "checkpoint");
    assert.strictEqual(rc.baseCommit, n, "adoption peels the chain to the newest real tip");
  });

  it("chain-of-chains [C1, N2] is built and adoption peels it to N2", async () => {
    const sc = await overlayScene();
    commit(sc.clone, "M2.txt");
    await track(sc);
    const p1 = await chainPack(sc, sc.p);
    assert.ok(p1);
    const c1 = p1.tipOid;
    publishPackToOrigin(sc.fx, await drain(p1.pack), c1, checkpointRef);
    const n2 = commit(sc.clone, "M3.txt");
    await track(sc);
    const p2 = await chainPack(sc, c1);
    assert.ok(p2);
    const c2 = p2.tipOid;
    assert.deepStrictEqual(parentsOf(sc.bare, c2), [c1, n2]);
    publishPackToOrigin(sc.fx, await drain(p2.pack), c2, checkpointRef);
    const gitB = worker(sc.fx, "B");
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, c2);
    assert.strictEqual(rc.baseCommit, n2);
  });

  it("tree-equal chain (branch caught up with main's workflows) is still emitted and peeled on adoption", async () => {
    const sc = await overlayScene();
    mergeMainInto(sc);
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    assert.strictEqual(wfTreeOf(sc.bare, n), wfTreeOf(sc.bare, sc.p), "scene: N's workflows equal P's");
    const packed = await chainPack(sc, sc.p);
    assert.ok(packed);
    const c = packed.tipOid;
    assert.notStrictEqual(c, n, "the chain is emitted even though its tree equals N's");
    assert.strictEqual(gitIn(sc.bare, ["rev-parse", `${c}^{tree}`]), gitIn(sc.bare, ["rev-parse", `${n}^{tree}`]));
    assert.deepStrictEqual(parentsOf(sc.bare, c), [sc.p, n]);
    publishPackToOrigin(sc.fx, await drain(packed.pack), c, checkpointRef);
    const gitB = worker(sc.fx, "B");
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, c);
    assert.strictEqual(rc.seededFrom, "checkpoint");
    assert.strictEqual(rc.baseCommit, n, "the tree-equal chain is peeled, not adopted as branch content");
  });

  it("re-declares P when nothing is new since P, and every pack guard still judges P", async () => {
    const sc = await overlayScene();
    await track(sc); // tracking tip is still R == lastParent(P)
    const judged: string[] = [];
    const steps: string[] = [];
    const packed = await chainPack(sc, sc.p, {
      beforePack: async (sha) => { judged.push(sha); },
      onStep: (s) => steps.push(s),
    });
    assert.ok(packed);
    assert.strictEqual(packed.tipOid, sc.p, "the existing wrapper is re-declared");
    assert.deepStrictEqual(judged, [sc.p], "the #1964 hold judges the actual candidate");
    assert.ok(steps.filter((s) => s === "scratch_preflight").length >= 2, "preflight re-ran on the chosen tip");
    await drain(packed.pack);
    // The same on the pinned path with a real floor (the seed commit below P): P is re-declared
    // and the preflight and the #1964 hold run on P itself, not on the pinned tip R.
    const floor = gitIn(sc.bare, ["merge-base", sc.r, sc.p]);
    const pinnedJudged: string[] = [];
    const pinnedSteps: string[] = [];
    const pinned = await sc.git.checkpointPack(
      sc.bare, branch, undefined, { tipSha: sc.r, excludeSha: floor }, (s) => pinnedSteps.push(s), undefined,
      async (sha) => { pinnedJudged.push(sha); }, [sc.p],
    );
    assert.ok(pinned);
    assert.strictEqual(pinned.tipOid, sc.p, "pinned: the existing wrapper is re-declared");
    assert.deepStrictEqual(pinnedJudged, [sc.p], "pinned: the #1964 hold judges P, not the pinned tip");
    assert.ok(pinnedSteps.filter((s) => s === "scratch_preflight").length >= 2, "pinned: preflight re-ran on P");
    await drain(pinned.pack);
    // A floor that is not an ancestor of P is still refused.
    const stray = rawCommit(sc.bare, gitIn(sc.bare, ["rev-parse", `${sc.r}^{tree}`]), [], "unrelated root");
    await assert.rejects(
      sc.git.checkpointPack(sc.bare, branch, undefined, { tipSha: sc.r, excludeSha: stray }, undefined, undefined, undefined, [sc.p]),
      ScratchPublicationError,
    );
  });

  it("a raw tip that shipped ambiguously (the attempted tip) does not shadow the confirmed overlay", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    const n2 = commit(sc.clone, "M3.txt");
    await track(sc);
    // attempted = N is an ancestor of the packed N2, so the chain must come from the confirmed P.
    const packed = await chainPack(sc, [n, sc.p]);
    assert.ok(packed);
    assert.deepStrictEqual(parentsOf(sc.bare, packed.tipOid), [sc.p, n2], "chained over the confirmed tip");
  });

  it("when two candidates both qualify, the first (attempted) wins over the confirmed overlay", async () => {
    const sc = await overlayScene();
    const n1 = commit(sc.clone, "M2.txt");
    await track(sc);
    // C1 = [P, N1] is packed but never published: the attempted tip.
    const c1 = (await chainPack(sc, sc.p))!.tipOid;
    assert.deepStrictEqual(parentsOf(sc.bare, c1), [sc.p, n1]);
    const n2 = commit(sc.clone, "M3.txt");
    await track(sc);
    // C1 is not an ancestor of N2 (only N1 is), and both C1 and P verify as wrapper-only over N2.
    const packed = await chainPack(sc, [c1, sc.p]);
    assert.ok(packed);
    assert.deepStrictEqual(parentsOf(sc.bare, packed.tipOid), [c1, n2], "the first qualifying candidate wins");
    const reversed = await chainPack(sc, [sc.p, c1]);
    assert.ok(reversed);
    assert.deepStrictEqual(parentsOf(sc.bare, reversed.tipOid), [sc.p, n2], "order decides");
  });

  it("an attempted tip the agent amended away falls through to the confirmed overlay", async () => {
    const sc = await overlayScene();
    const n1 = commit(sc.clone, "M2.txt");
    await track(sc);
    // C1 = [P, N1] is packed but never lands: attempted = C1.
    const c1 = (await chainPack(sc, sc.p))!.tipOid;
    assert.deepStrictEqual(parentsOf(sc.bare, c1), [sc.p, n1]);
    // The agent amends N1 to N1' (reset to R, recommit different content).
    gitIn(sc.clone, ["reset", "--hard", sc.r]);
    const n1b = commit(sc.clone, "M2b.txt");
    assert.notStrictEqual(n1b, n1);
    await track(sc);
    // C1 adds N1, which is real work not in N1', so it does not verify; P alone does.
    const packed = await chainPack(sc, [c1, sc.p]);
    assert.ok(packed);
    assert.deepStrictEqual(parentsOf(sc.bare, packed.tipOid), [sc.p, n1b], "chained over the confirmed P, not raw");
  });

  it("no chain when P is an ancestor of N, absent from the bare, or equals N", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    assert.strictEqual((await chainPack(sc, sc.r))!.tipOid, n, "P an ancestor of N");
    assert.strictEqual((await chainPack(sc, "1".repeat(40)))!.tipOid, n, "P absent from the bare");
    assert.strictEqual((await chainPack(sc, n))!.tipOid, n, "P equals N");
    assert.strictEqual((await chainPack(sc, undefined))!.tipOid, n, "no chainFrom");
    assert.strictEqual((await chainPack(sc, "nothex"))!.tipOid, n, "malformed chainFrom");
  });

  it("no chain when the branch itself modified a workflow file", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, WF, "name: ci\n# branch edit\n");
    await track(sc);
    assert.strictEqual((await chainPack(sc, sc.p))!.tipOid, n);
  });

  it("no chain over a non-wrapper divergent commit, or a wrapper over a non-ancestor real tip", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    const tree = gitIn(sc.bare, ["rev-parse", `${n}^{tree}`]);
    const divergent = rawCommit(sc.bare, tree, [sc.r], "plain divergent work");
    assert.strictEqual((await chainPack(sc, divergent))!.tipOid, n, "non-wrapper P");
    // P = overlay over R, but the real tip no longer descends R (history rewritten).
    const rewritten = rawCommit(sc.bare, tree, [gitIn(sc.bare, ["rev-parse", "refs/remotes/origin/main"])], "rewritten N");
    await fixtureUpdateTracking(sc.git, sc.bare, branch, rewritten, "run-A");
    assert.strictEqual((await chainPack(sc, sc.p))!.tipOid, rewritten, "wrapper over a non-ancestor real tip");
  });

  it("verifier negative: a wrapper that differs from its last parent outside .github/workflows is not chained", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    const n3 = commit(sc.clone, "M3.txt");
    await track(sc);
    // A forged multi-parent wrapper carrying real (non-workflow) content over R.
    const forged = rawCommit(
      sc.bare, gitIn(sc.bare, ["rev-parse", `${n}^{tree}`]), [sc.p, sc.r], `${OVERLAY_COMMIT_PREFIX} forged`,
    );
    assert.strictEqual((await chainPack(sc, forged))!.tipOid, n3);
  });

  it("verifier negative: an unreadable last parent fails closed to the raw tip", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    const missing = "a".repeat(40);
    const tree = gitIn(sc.bare, ["rev-parse", `${sc.p}^{tree}`]);
    const raw = `tree ${tree}\nparent ${sc.p}\nparent ${missing}\nauthor t <t@t> 1 +0000\ncommitter t <t@t> 1 +0000\n\n${OVERLAY_COMMIT_PREFIX} dangling\n`;
    const dangling = execFileSync("git", ["-C", sc.bare, "hash-object", "-t", "commit", "-w", "--literally", "--stdin"], {
      encoding: "utf8", env: GIT_ENV, input: raw,
    }).trim();
    assert.strictEqual((await chainPack(sc, dangling))!.tipOid, n);
  });

  it("verifier negative: a wrapper verified earlier is refused once its tree goes missing (no stale verdict)", async () => {
    const sc = await overlayScene();
    const seam = sc.git as unknown as {
      verifyWrapperOnlyHistory(bare: string, tip: string, excludeTip: string): Promise<boolean>;
    };
    assert.strictEqual(await seam.verifyWrapperOnlyHistory(sc.bare, sc.p, sc.r), true, "P verifies while intact");
    // P's root tree was written loose by the overlay synthesis; drop it from the same bare.
    const tree = gitIn(sc.bare, ["rev-parse", `${sc.p}^{tree}`]);
    const loose = path.join(sc.bare, "objects", tree.slice(0, 2), tree.slice(2));
    assert.ok(fs.existsSync(loose), "scene: P's tree is a loose object");
    fs.rmSync(loose);
    assert.strictEqual(
      await seam.verifyWrapperOnlyHistory(sc.bare, sc.p, sc.r), false,
      "the same GitCache re-reads the trees and fails closed",
    );
  });

  it("verifier bounds: too many wrappers or an oversized diff listing refuses the chain", async () => {
    // maxWrappers: P's history over N holds two wrappers (C1 and the overlay), over a bound of 1.
    const many = await overlayScene({ checkpointChainBounds: { maxWrappers: 1 } });
    const n1 = commit(many.clone, "M2.txt");
    await track(many);
    const first = await chainPack(many, many.p);
    assert.ok(first);
    const c1 = first.tipOid;
    assert.deepStrictEqual(parentsOf(many.bare, c1), [many.p, n1], "the first pack is a real chain [P, N]");
    publishPackToOrigin(many.fx, await drain(first.pack), c1, checkpointRef);
    const n2 = commit(many.clone, "M3.txt");
    await track(many);
    assert.strictEqual((await chainPack(many, c1))!.tipOid, n2, "2 wrappers > maxWrappers 1: raw tip");

    // maxDiffBytes: the overlay's one-path workflow diff does not fit in 4 bytes.
    const big = await overlayScene({ checkpointChainBounds: { maxDiffBytes: 4 } });
    const n = commit(big.clone, "M2.txt");
    await track(big);
    assert.strictEqual((await chainPack(big, big.p))!.tipOid, n, "diff over maxDiffBytes: raw tip");
  });

  it("a chain built over exactly maxWrappers wrappers is still peeled on adoption", async () => {
    // maxWrappers 1: P is one wrapper over N, so the chain is allowed; the adopter then sees the
    // chain plus P (two wrappers), which only verifies with the peel's one-wrapper headroom.
    const sc = await overlayScene({ checkpointChainBounds: { maxWrappers: 1 } });
    mergeMainInto(sc);
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    const packed = await chainPack(sc, sc.p);
    assert.ok(packed);
    const c = packed.tipOid;
    assert.deepStrictEqual(parentsOf(sc.bare, c), [sc.p, n]);
    assert.strictEqual(gitIn(sc.bare, ["rev-parse", `${c}^{tree}`]), gitIn(sc.bare, ["rev-parse", `${n}^{tree}`]));
    publishPackToOrigin(sc.fx, await drain(packed.pack), c, checkpointRef);
    const gitB = workerWith(sc.fx, "B", { checkpointChainBounds: { maxWrappers: 1 } });
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, c);
    assert.strictEqual(rc.baseCommit, n, "the MAX-wrapper chain peels back to N");
  });

  it("a branch with no .github at all still chains with P's workflow subtree and peels back to N", async () => {
    const sc = await overlayScene({}, { "README.md": "readme\n" });
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    assert.strictEqual(wfTreeOf(sc.bare, n), "", "scene: the branch has no workflows");
    assert.notStrictEqual(wfTreeOf(sc.bare, sc.p), "", "scene: P carries the default's workflows");
    const packed = await chainPack(sc, sc.p);
    assert.ok(packed);
    const c = packed.tipOid;
    assert.deepStrictEqual(parentsOf(sc.bare, c), [sc.p, n]);
    assert.strictEqual(wfTreeOf(sc.bare, c), wfTreeOf(sc.bare, sc.p), "P's workflow subtree");
    publishPackToOrigin(sc.fx, await drain(packed.pack), c, checkpointRef);
    const gitB = worker(sc.fx, "B");
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, c);
    assert.strictEqual(rc.baseCommit, n, "adoption peels the chain to N");
  });

  it("adoption negative: a forged tree-equal merge hiding non-workflow history is not peeled", async () => {
    const sc = await overlayScene();
    const n = commit(sc.clone, "M2.txt");
    await track(sc);
    // A side commit with real content, then a merge-shaped `ckpt(overlay):` commit that has N's own tree
    // (so its diff to the last parent is empty) but reaches the side commit through parent[0].
    const baseTip = gitIn(sc.bare, ["rev-parse", "refs/remotes/origin/main"]);
    const sideTree = gitIn(sc.bare, ["rev-parse", `${sc.r}^{tree}`]);
    const side = rawCommit(sc.bare, sideTree, [baseTip], "side work");
    const forged = rawCommit(
      sc.bare, gitIn(sc.bare, ["rev-parse", `${n}^{tree}`]), [side, n], `${OVERLAY_COMMIT_PREFIX} forged tree-equal`,
    );
    await fixtureUpdateTracking(sc.git, sc.bare, branch, forged, "run-A");
    const packed = await chainPack(sc, undefined);
    assert.ok(packed);
    assert.strictEqual(packed.tipOid, forged, "not behind: shipped raw");
    publishPackToOrigin(sc.fx, await drain(packed.pack), forged, checkpointRef);
    const gitB = worker(sc.fx, "B");
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, forged);
    assert.strictEqual(rc.seededFrom, "checkpoint");
    assert.strictEqual(rc.baseCommit, forged, "unverifiable wrapper history keeps the tip as branch content");
  });

  it("adoption negative: a tree-equal chain whose wrapper history exceeds the verifier bound is not peeled", async () => {
    const sc = await overlayScene();
    mergeMainInto(sc);
    commit(sc.clone, "M2.txt");
    await track(sc);
    const first = await chainPack(sc, sc.p);
    assert.ok(first);
    const c1 = first.tipOid;
    publishPackToOrigin(sc.fx, await drain(first.pack), c1, checkpointRef);
    const n2 = commit(sc.clone, "M3.txt");
    await track(sc);
    const second = await chainPack(sc, c1);
    assert.ok(second);
    const c2 = second.tipOid;
    publishPackToOrigin(sc.fx, await drain(second.pack), c2, checkpointRef);
    assert.strictEqual(gitIn(sc.bare, ["rev-parse", `${c2}^{tree}`]), gitIn(sc.bare, ["rev-parse", `${n2}^{tree}`]));

    // The adopter's verifier is bounded below this chain's two wrappers: the structure is
    // unverifiable, so the peel is refused (a pack-stored object cannot be made unreadable here;
    // the unreadable-parent shape is covered by the dangling-wrapper chain test above).
    const gitB = workerWith(sc.fx, "B", { checkpointChainBounds: { maxWrappers: 1 } });
    const bareB = await gitB.ensureClone(sc.fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 2733, noProofReseed, "run-B", true, c2);
    assert.strictEqual(rc.seededFrom, "checkpoint");
    const base = rc.baseCommit;
    assert.strictEqual(base, c2, "an unverifiable wrapper history is kept, never peeled");
    assert.notStrictEqual(base, n2);
  });
});
