import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { noProofReseed } from "./helpers.js";
import type { GitCache, OwedCandidateContext } from "../src/git.js";
import type { ClaimResponse, PublicationContextRequest, PublicationContextResponse } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { FakeApi } from "./fake-api.js";

/** Explicit historical source authority; only the incoming candidate identity is echoed. */
export function sourcePublicationContext(
  runId: string,
  request: PublicationContextRequest,
  source: {
    runId: string;
    publishedHead: string;
    mrIid: number;
    issueIid: number | null;
    intent: "closing" | "non_closing" | "not_applicable";
  },
): PublicationContextResponse {
  assert.notEqual(source.runId, runId);
  assert.notEqual(source.publishedHead, request.candidate_head);
  assert.equal(source.issueIid === null, source.intent === "not_applicable");
  return {
    version: 1,
    status: source.intent === "not_applicable" ? "not_applicable" : "resolved",
    binding: {
      run_id: runId,
      ...request,
      mr_iid: source.mrIid,
      source_run_id: source.runId,
      source_publication: source.intent === "not_applicable" ? null : {
        version: 1,
        claim_generation: 1,
        branch: request.branch,
        mr_iid: source.mrIid,
        published_head: source.publishedHead,
        intent: source.intent,
        reported_non_closing: source.intent === "non_closing",
      },
      issue_iid: source.issueIid,
      intent: source.intent,
    },
  };
}

/** Opt-in historical authority over HTTP, rebinding each alignment/retry candidate. */
export function installSourcePublicationContext(
  api: FakeApi, claim: ClaimResponse, publishedHead: string,
  issueIid: number | null, intent: "closing" | "non_closing" | "not_applicable",
): void {
  assert.ok(claim.claim_generation);
  api.publicationContextWire = ({ runId, body }) => {
    assert.equal(runId, claim.run_id);
    return { status: 200, body: sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
      runId: "00000000-0000-4000-8000-000000003120",
      publishedHead, mrIid: 42, issueIid, intent,
    }) };
  };
}

/** Preserve negotiated capabilities while opting this fixture into refresh publication. */
export async function enableRefreshPublication(
  api: FakeApi, client: WorkerClient, claim: ClaimResponse, originPath: string,
  issueIid: number | null, intent: "closing" | "non_closing" | "not_applicable",
): Promise<void> {
  const historicalHead = execFileSync("git", ["-C", originPath, "rev-parse", "main"], { encoding: "utf8" }).trim();
  installSourcePublicationContext(api, claim, historicalHead, issueIid, intent);
  api.setRegisterProtocolFeatures([...new Set([
    ...client.protocolFeatures, "claim_generation_fence", "refresh_publication_context_v1",
  ])]);
  await client.register("existing-refresh-fixture");
}

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
    assert.match(args[5]?.candidate ?? "", /^[0-9a-f]{40}$/, "finalize supplies its proved literal source");
    await push(...args);
    source = args[5]!.candidate;
    if (follow) pr.head = source;
  };
  return () => source;
}
