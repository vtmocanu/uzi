import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { deriveCloneKey } from "../src/run-kind.js";
import { selfImproveBranch } from "../src/self-improve.js";

// Pins deriveCloneKey (agent/src/run-kind.ts) per run kind. This is the SINGLE source of
// the canonical runner-clone { branch, slug } rule: RUN_KIND_PROFILES.*.cloneBranch
// delegates here, and the #1319 orphan validation feeds it the owner's persisted identity,
// so the two derivations can never drift. These characterization assertions are the guard
// that the delegation stayed behaviour-preserving.
describe("deriveCloneKey", () => {
  it("issue/chat/judge derive from issueIid — the same literal createOrAttachRunnerClone uses", () => {
    // createOrAttachRunnerClone(bare, iid) calls
    //   runnerCloneForBranch(bare, `agent/issue-${iid}`, `issue-${iid}`, ...)
    // in git.ts, so this branch/slug literal lives across the git/run-kind boundary. Pin it
    // here so a drift on either side trips this test.
    const expected = { branch: "agent/issue-7", slug: "issue-7" };
    assert.deepEqual(deriveCloneKey({ kind: "issue", runId: "r", issueIid: 7 }), expected);
    assert.deepEqual(deriveCloneKey({ kind: "chat", runId: "r", issueIid: 7 }), expected);
    assert.deepEqual(deriveCloneKey({ kind: "judge", runId: "r", issueIid: 7 }), expected);
  });

  it("issue with a null issueIid fails closed (undefined)", () => {
    assert.equal(deriveCloneKey({ kind: "issue", runId: "r", issueIid: null }), undefined);
    assert.equal(deriveCloneKey({ kind: "issue", runId: "r" }), undefined);
  });

  it("ci_fix on a NON-default branch keeps the pipeline ref, slugified", () => {
    assert.deepEqual(
      deriveCloneKey({ kind: "ci_fix", runId: "r", pipelineRef: "feature/x", defaultBranch: "main" }),
      { branch: "feature/x", slug: "feature-x" },
    );
  });

  it("ci_fix on the DEFAULT branch reconstructs ci-fix/pipeline-<id>", () => {
    assert.deepEqual(
      deriveCloneKey({ kind: "ci_fix", runId: "r", pipelineRef: "main", defaultBranch: "main", pipelineId: 42 }),
      { branch: "ci-fix/pipeline-42", slug: "ci-fix-pipeline-42" },
    );
  });

  it("ci_fix fails closed when it cannot reconstruct the key", () => {
    // Default branch but no pipeline id: cannot build ci-fix/pipeline-<id>.
    assert.equal(
      deriveCloneKey({ kind: "ci_fix", runId: "r", pipelineRef: "main", defaultBranch: "main", pipelineId: null }),
      undefined,
    );
    // No pipeline ref at all (mirrors the `!claim.pipeline` guard).
    assert.equal(deriveCloneKey({ kind: "ci_fix", runId: "r", pipelineRef: null }), undefined);
  });

  it("self_improve derives its fresh-per-cycle branch from selfImproveBranch", () => {
    const branch = selfImproveBranch("r");
    assert.deepEqual(deriveCloneKey({ kind: "self_improve", runId: "r" }), {
      branch,
      slug: branch.replace(/\//g, "-"),
    });
  });

  it("prompt derives uzi/prompt-<runId>", () => {
    assert.deepEqual(deriveCloneKey({ kind: "prompt", runId: "r" }), {
      branch: "uzi/prompt-r",
      slug: "uzi-prompt-r",
    });
  });

  it("task uses the carried branch and a task-<runId> slug", () => {
    assert.deepEqual(deriveCloneKey({ kind: "task", runId: "r", branch: "uzi/task/r" }), {
      branch: "uzi/task/r",
      slug: "task-r",
    });
  });

  it("task fails closed when its branch is missing/empty", () => {
    assert.equal(deriveCloneKey({ kind: "task", runId: "r", branch: null }), undefined);
    assert.equal(deriveCloneKey({ kind: "task", runId: "r", branch: "" }), undefined);
    assert.equal(deriveCloneKey({ kind: "task", runId: "r", branch: "   " }), undefined);
  });

  it("mr_rework slugifies the carried branch — the cross-kind slug divergence #1319 targets", () => {
    // Same underlying branch as an issue owner (agent/issue-7), but the mr_rework SLUG is the
    // slugified branch `agent-issue-7`, NOT the issue owner's `issue-7`. This is exactly the
    // cross-kind clone-key divergence a CapturePathMismatchError classifies.
    const rework = deriveCloneKey({ kind: "mr_rework", runId: "r", branch: "agent/issue-7" });
    assert.deepEqual(rework, { branch: "agent/issue-7", slug: "agent-issue-7" });
    const issue = deriveCloneKey({ kind: "issue", runId: "r", issueIid: 7 });
    assert.equal(issue?.slug, "issue-7");
    assert.notEqual(rework?.slug, issue?.slug);
  });

  it("mr_rework fails closed when its branch is missing/empty", () => {
    assert.equal(deriveCloneKey({ kind: "mr_rework", runId: "r", branch: null }), undefined);
    assert.equal(deriveCloneKey({ kind: "mr_rework", runId: "r", branch: "" }), undefined);
  });
});
