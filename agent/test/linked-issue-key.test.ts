import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { linkedIssueKey } from "../src/linked-issue.js";
import { nonClosingTitle, renderCompletionBlock } from "../src/pr-description.js";
import { buildIssueContext } from "../src/prompt.js";
import { mrCompletionBlock, mrTitle } from "../src/runner.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

describe("linkedIssueKey", () => {
  it("is unlinked only when issue_key is absent", () => {
    assert.deepEqual(linkedIssueKey({}), { linked: false, label: null });
  });
  it("labels a well-formed key", () => {
    assert.deepEqual(linkedIssueKey({ issue_key: "ABC-12" }), { linked: true, label: "ABC-12" });
    assert.deepEqual(linkedIssueKey({ issue_key: "a_b1-7" }), { linked: true, label: "a_b1-7" });
  });
  it("is linked with no label for a malformed key, whatever its type", () => {
    const bad: unknown[] = ["", "ABC", "ABC-0", "ABC-012", "1ABC-1", "ABC-1 Closes #3", "ABC-1\nx", `A-${"1".repeat(70)}`, null, 12, {}, true];
    for (const issue_key of bad) assert.deepEqual(linkedIssueKey({ issue_key }), { linked: true, label: null }, String(issue_key));
  });
});

const linkedBlock = (over: Partial<Parameters<typeof renderCompletionBlock>[0]> = {}) =>
  renderCompletionBlock({ issueIid: 7, branch: "agent/issue-7", closes: true, linkedIssue: { label: "ABC-12" }, ...over });

describe("renderCompletionBlock linked arm", () => {
  it("names the key and never closes, even with closes=true", () => {
    const body = linkedBlock();
    assert.match(body, /^Related to ABC-12\.$/m);
    assert.doesNotMatch(body, /Closes/);
    assert.doesNotMatch(body, /#7/);
  });
  it("names the linked issue for an invalid key", () => {
    const body = linkedBlock({ linkedIssue: { label: null } });
    assert.match(body, /^Related to the linked issue\.$/m);
    assert.doesNotMatch(body, /Closes|#7/);
  });
  it("the partial arms name the key, not #N", () => {
    const owner = linkedBlock({ completionScope: { deferred: [{ milestone_id: "m2", title: "T", reason: "R" }] } });
    assert.match(owner, /Implements part of ABC-12 \(partial delivery/);
    assert.doesNotMatch(owner, /Closes|#7\b/);
    const capped = linkedBlock({ scopeCapped: { completedCount: 1, total: 2 } });
    assert.match(capped, /Implements part of ABC-12 \(partial delivery/);
    assert.doesNotMatch(capped, /Closes|#7\b/);
  });
  it("the non-closing declaration arm keeps no Closes", () => {
    const decl = linkedBlock({ nonClosing: { reason: "Acceptance remains" } });
    assert.match(decl, /Related to ABC-12\./);
    assert.match(decl, /Issue remains open/);
    assert.doesNotMatch(decl, /Closes|#7\b/);
  });
  it("the pending arm keeps no Closes and names the key", () => {
    const body = linkedBlock({ completionPending: { criteria: [{ criterion_id: "00000000-0000-4000-8000-0000000000a1", milestone_id: "m2",
      kind: "maintainer", text: "Live check", reason: "Pending", conditions: ["Ready"], verification_steps: ["Observe rollout"],
      authorization_revision: 1, status: "unverified" }] } as never });
    assert.match(body, /Related to ABC-12\./);
    assert.doesNotMatch(body, /Closes|#7\b/);
  });
  it("an unlinked claim is unchanged", () => {
    assert.match(renderCompletionBlock({ issueIid: 7, branch: "b", closes: true }), /Related to #7\.\n\nCloses #7/);
  });
});

describe("linked title and prompt wording", () => {
  it("nonClosingTitle breaks a closing title and leaves a clean one alone", () => {
    const title = nonClosingTitle("Close #7", 7, undefined, "ABC-12");
    assert.doesNotMatch(title, /^Close #7$/);
    assert.equal(nonClosingTitle("Plain title", 7, undefined, "ABC-12"), "Plain title");
  });
  it("mrTitle's empty-title fallback uses the key, or the linked-issue words", () => {
    assert.equal(mrTitle(makeClaim({ issue_iid: 7, issue_title: "", issue_key: "ABC-12" })), "Work on issue ABC-12");
    assert.equal(mrTitle(makeClaim({ issue_iid: 7, issue_title: "", issue_key: "nope" })), "Work on issue the linked issue");
    assert.equal(mrTitle(makeClaim({ issue_iid: 7, issue_title: "" })), "Work on issue #7");
  });
  it("mrCompletionBlock renders the linked arm from the claim", () => {
    const body = mrCompletionBlock(makeClaim({ issue_iid: 7, issue_key: "ABC-12" }), "agent/issue-7", undefined, undefined, undefined,
      undefined, undefined, undefined, true);
    assert.match(body, /Related to ABC-12\./);
    assert.doesNotMatch(body, /Closes/);
  });
  it("buildIssueContext headers by key for a linked claim only", () => {
    assert.match(buildIssueContext("t", "d", 7, "ABC-12"), /^Issue ABC-12\n/);
    assert.match(buildIssueContext("t", "d", 7, "bad key"), /^Issue context\n/);
    assert.match(buildIssueContext("t", "d", 7), /^Issue #7\n/);
  });
});

describe("linked run publication", () => {
  const IID = 3130;
  function commitMessage(message: string) {
    const executor = new StubExecutor(nullLogger());
    const run = executor.run.bind(executor);
    executor.run = async (ctx: RunContext): Promise<ExecutorResult> => {
      const result = await run(ctx);
      fs.writeFileSync(path.join(ctx.worktreePath, "linked.txt"), "work\n");
      const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
      execFileSync("git", ["-C", ctx.worktreePath, "add", "linked.txt"], { env });
      execFileSync("git", ["-C", ctx.worktreePath, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "commit", "-m", message], { env });
      return result;
    };
    return executor;
  }

  for (const issue_key of ["ABC-12", "not a key"]) {
    it(`key ${JSON.stringify(issue_key)}: non-closing proof, no Closes, key in the body`, async () => {
      api.setRegisterProtocolFeatures(["refresh_publication_context_v1", "claim_generation_fence", "completion_non_closing_v1"]);
      await client.register("linked");
      const claim = gitlabClaim(IID, { claim_generation: 7, issue_key });
      const forge = fakeGitlab();
      const pushed = followSuccessfulPush(git, forge.pr);
      await runner(new StubExecutor(nullLogger()), forge.gitlab).execute(claim);
      const terminal = api.states.find(s => s.runId === claim.run_id && s.body.status === "completed");
      assert.ok(terminal, JSON.stringify(api.states.map(s => s.body.status)));
      assert.deepEqual(terminal.body.source_publication, { version: 1, published_head: pushed(), non_closing: true });
      assert.doesNotMatch(forge.pr.description, /Closes/);
      assert.match(forge.pr.description, issue_key === "ABC-12" ? /Related to ABC-12\./ : /Related to the linked issue\./);
      assert.equal(execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", `refs/heads/agent/issue-${IID}`], { encoding: "utf8" }).trim().length, 40);
    });
  }

  it("a commit message closing #<iid> is still refused before any push", async t => {
    api.setRegisterProtocolFeatures(["refresh_publication_context_v1", "claim_generation_fence", "completion_non_closing_v1"]);
    await client.register("linked-gate");
    const claim = gitlabClaim(IID, { claim_generation: 7, issue_key: "ABC-12" });
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr);
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(commitMessage(`Closes #${IID}`), forge.gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    const failed = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failed, JSON.stringify(api.states.map(s => s.body.status)));
    assert.match(String(failed.body.failure_reason), /non-closing delivery refused: commit message\(s\) close issue #3130/);
  });
});
