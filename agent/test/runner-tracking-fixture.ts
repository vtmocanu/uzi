import assert from "node:assert/strict";
import type { FetchAgentBranchOptions, GitCache, TrackingUpdateResult } from "../src/git.js";

/** TEST ONLY: generation 1 identifies a deliberately fresh fixture claim. */
export async function fixtureTrackingOptions(
  cache: GitCache, bare: string, branch: string, runId: string,
  explicitFixtureGeneration = 1, remotelyConfirmedSha?: string,
): Promise<FetchAgentBranchOptions> {
  const defaultBranch = await cache.defaultBranchName(bare);
  assert.ok(defaultBranch, "fixture bare must have a known default branch");
  const sha = await cache.originBranchTip(bare, defaultBranch);
  assert.ok(sha, "fixture bare must have a known default tip");
  return { context: { barePath: bare, branch, runId, generation: explicitFixtureGeneration,
    kind: "issue", defaultIdentity: { ref: `refs/remotes/origin/${defaultBranch}`, sha } },
    ...(remotelyConfirmedSha === undefined ? {} : { remotelyConfirmedSha }) };
}

export async function fixtureFetchTracking(
  cache: GitCache, bare: string, clone: string, branch: string, runId: string,
  explicitFixtureGeneration = 1, remotelyConfirmedSha?: string,
): Promise<string> {
  const opts = await fixtureTrackingOptions(cache, bare, branch, runId, explicitFixtureGeneration, remotelyConfirmedSha);
  const result = await cache.fetchAgentBranch(bare, clone, branch, runId, opts);
  assert.equal(result.kind, "updated", JSON.stringify(result));
  assert.ok(result.kind === "updated");
  return result.trackingRef;
}

export async function fixtureUpdateTracking(
  cache: GitCache, bare: string, branch: string, sha: string, runId: string,
  explicitFixtureGeneration = 1, remotelyConfirmedSha?: string,
): Promise<TrackingUpdateResult> {
  const opts = await fixtureTrackingOptions(cache, bare, branch, runId, explicitFixtureGeneration, remotelyConfirmedSha);
  const result = await cache.updateTrackingRef(bare, branch, sha, opts);
  assert.equal(result.kind, "updated", JSON.stringify(result));
  return result;
}

/** Explicit committed custody for capture tests whose Git objects are mocked. */
export function mockCommittedTracking(git: GitCache, sha: string): void {
  let committed: FetchAgentBranchOptions["context"] | undefined;
  git.fetchAgentBranch = (async (bare, _clone, branch, runId, opts): Promise<TrackingUpdateResult> => {
    if (!opts || opts.context.barePath !== bare || opts.context.branch !== branch || opts.context.runId !== runId) {
      throw new Error("fixture requires trusted tracking context");
    }
    committed = opts.context;
    return { kind: "updated", trackingRef: `refs/uzi-runner/${branch}`, candidateSha: sha,
      retainedShas: [sha], divergence: "none" };
  }) as typeof git.fetchAgentBranch;
  git.committedTrackingOwnership = async (bare, branch, runId, expectedSha, generation) => {
    if (!committed || committed.barePath !== bare || committed.branch !== branch || committed.runId !== runId ||
        (expectedSha !== undefined && expectedSha !== sha) ||
        (generation !== undefined && generation !== committed.generation)) return { kind: "not_owned" };
    return { kind: "owned", sha, context: committed };
  };
}
