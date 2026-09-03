import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import {
  GitCache,
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
  while (cleanups.length) cleanups.pop()!();
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
  return new GitCache(dataDir, nullLogger());
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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    const realTip = commit(seed.path, "M1.txt"); // non-workflow work off the v1-workflow base
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n"); // main moves its workflow
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

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
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, "run-B", true, ov);
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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    const realTip = commit(seed.path, "M1.txt"); // no workflow change; main not advanced
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "workflows already equal ⇒ raw realTip is shipped");
  });

  it("workflow-MODIFIED by the branch ⇒ ships realTip (the gate holds, #377 owns it at finalize)", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    commit(seed.path, "M1.txt");
    // The branch ITSELF edits a workflow file — the doomed-at-finalize shape.
    const realTip = commit(seed.path, WF, "name: ci\non: push\njobs: {}\n# branch-edit\n");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n"); // also behind, so gate-2 passes
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "a workflow-modifying branch ships realTip (no overlay)");
    assert.ok(
      !subjectOf(bareA, packed!.tipOid).startsWith(OVERLAY_COMMIT_PREFIX),
      "the shipped tip is the real commit, not an overlay wrapper",
    );
  });

  it("stacked: realTip is a wip(park) marker AND behind ⇒ overlay built; peel then soft-reset the marker", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const checkpointRef = `refs/uzi-checkpoints/${branch}`;
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    const realParent = commit(seed.path, "M1.txt"); // the last REAL commit (marker's parent)
    fs.writeFileSync(path.join(seed.path, "WIP.txt"), "in-progress\n"); // genuinely uncommitted
    assert.strictEqual(await gitA.commitWipMarker(seed.path), true, "a wip(park) marker was planted");
    const marker = gitIn(seed.path, ["rev-parse", "HEAD"]);
    assert.ok(subjectOf(seed.path, marker).startsWith(WIP_PARK_COMMIT_PREFIX));
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    const ov = packed!.tipOid;
    assert.ok(subjectOf(bareA, ov).startsWith(OVERLAY_COMMIT_PREFIX), "overlay wraps the marker tip");
    assert.strictEqual(parentsOf(bareA, ov)[0], marker, "the overlay's (last) parent is the marker");

    const pack = await drain(packed!.pack);
    publishPackToOrigin(fx, pack, ov, checkpointRef);
    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, "run-B", true, ov);

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

  it("second sequential overlay: parent[0]=prev, last parent=realTip2, and prev is its ancestor", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
    const p1 = await gitA.checkpointPack(bareA, branch, ctx());
    const ov1 = p1!.tipOid;
    assert.ok(subjectOf(bareA, ov1).startsWith(OVERLAY_COMMIT_PREFIX));

    const realTip2 = commit(seed.path, "M2.txt"); // more work → a second checkpoint
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
    const ov1 = (await gitA.checkpointPack(bareA, branch, ctx()))!.tipOid;

    const realTip2 = commit(seed.path, "M2.txt");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");
    const p2 = await gitA.checkpointPack(bareA, branch, ctx(ov1));
    const ov2 = p2!.tipOid;
    assert.strictEqual(parentsOf(bareA, ov2).length, 2, "ov2 is a 2-parent chained overlay");

    publishPackToOrigin(fx, await drain(p2!.pack), ov2, checkpointRef);
    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, "run-B", true, ov2);
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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    const realTip = commit(seed.path, "M1.txt"); // realTip still carries the v1 workflow
    deleteOriginWorkflow(fx); // default now has NO .github/workflows tree
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    const realTip = commit(seed.path, "M1.txt");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, realTip, "no workflows on either side ⇒ raw realTip");
  });

  it("determinism: two builds with identical inputs yield a byte-identical O_ov OID", async () => {
    const fx = mk();
    const branch = "agent/issue-1036";
    const gitA = worker(fx, "A");
    const bareA = await gitA.ensureClone(fx.originPath);
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n");
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    const realTip = commit(seed.path, "M1.txt");
    advanceOriginWorkflow(fx, "name: ci\non: push\njobs: {}\n# v2\n"); // behind, but no overlay asked for
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    fs.writeFileSync(path.join(seed.path, "M1.txt"), "real agent work\n");
    gitIn(seed.path, ["add", "M1.txt"]);
    gitIn(seed.path, [...IDENT, "commit", "-m", `${OVERLAY_COMMIT_PREFIX} forged by the agent`]);
    const forged = gitIn(seed.path, ["rev-parse", "HEAD"]);
    // Stamp the worker-side tracking ref for run-A so the reseed is ownedHere.
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    const rc = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
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
    const seed = await gitA.createOrAttachRunnerClone(bareA, 1036, "run-A");
    // Real work, forged subject. main is NOT advanced ⇒ not behind ⇒ checkpointPack ships the
    // raw tip as a plain checkpoint (no overlay).
    fs.writeFileSync(path.join(seed.path, "M1.txt"), "real agent work\n");
    gitIn(seed.path, ["add", "M1.txt"]);
    gitIn(seed.path, [...IDENT, "commit", "-m", `${OVERLAY_COMMIT_PREFIX} forged plain`]);
    const forged = gitIn(seed.path, ["rev-parse", "HEAD"]);
    await gitA.fetchAgentBranch(bareA, seed.path, branch, "run-A");

    const packed = await gitA.checkpointPack(bareA, branch, ctx());
    assert.ok(packed);
    assert.strictEqual(packed!.tipOid, forged, "not behind ⇒ plain checkpoint ships the raw (forged-subject) tip");
    const pack = await drain(packed!.pack);
    publishPackToOrigin(fx, pack, forged, checkpointRef);

    const gitB = worker(fx, "B");
    const bareB = await gitB.ensureClone(fx.originPath);
    const rc = await gitB.createOrAttachRunnerClone(bareB, 1036, "run-B", true, forged);
    assert.strictEqual(rc.seededFrom, "checkpoint", "seeded from the checkpoint ref (the forge vector)");
    assert.strictEqual(
      rc.baseCommit,
      forged,
      "the forged plain commit is NOT peeled — its diff is real work, not a .github/workflows swap",
    );
    assert.strictEqual(fs.existsSync(path.join(rc.path, "M1.txt")), true, "the agent's committed work is preserved");
  });
});
