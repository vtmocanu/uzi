import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { noProofReseed } from "./helpers.js";
import type { GitCache, OwedCandidateContext } from "../src/git.js";

/** Fresh fixture admission through the same worker-owned tracking transport as production. */
export async function establishTrackingOwnership(
  git: GitCache, barePath: string, clone: string, branch: string,
  runId: string, generation: number, kind: OwedCandidateContext["kind"],
): Promise<OwedCandidateContext> {
  const defaultBranch = await git.defaultBranchName(barePath);
  assert.ok(defaultBranch);
  const sha = await git.originBranchTip(barePath, defaultBranch);
  assert.ok(sha);
  const context: OwedCandidateContext = {
    runId, generation, kind, branch, barePath,
    defaultIdentity: { ref: `refs/remotes/origin/${defaultBranch}`, sha },
  };
  const runnerClone = await git.runnerCloneForBranch(barePath, branch, runId, noProofReseed, runId);
  execFileSync("git", ["-C", runnerClone.path, "-c", "protocol.file.allow=user", "fetch",
    "--no-tags", `file://${clone}`, `refs/heads/${branch}`]);
  execFileSync("git", ["-C", runnerClone.path, "update-ref", `refs/heads/${branch}`, "FETCH_HEAD"]);
  const admitted = await git.fetchAgentBranch(barePath, runnerClone.path, branch, runId, { context });
  assert.equal(admitted.kind, "updated", JSON.stringify(admitted));
  return context;
}

/** Model the forge's branch update at the successful real push, before any MR write. */
export function followSuccessfulPush(git: GitCache, pr: { head: string }, follow = true): () => string {
  const push = git.pushBranch.bind(git);
  let source = "";
  git.pushBranch = async (...args) => {
    assert.match(args[5] ?? "", /^[0-9a-f]{40}$/, "finalize supplies its proved literal source");
    await push(...args);
    source = args[5]!;
    if (follow) pr.head = source;
  };
  return () => source;
}
