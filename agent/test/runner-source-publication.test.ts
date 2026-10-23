import assert from "node:assert/strict";
import { it } from "node:test";
import { execFileSync } from "node:child_process";
import { StubExecutor, type ExecutorResult } from "../src/executor.js";
import { codexRig, FakeCodexExecutor, commitInTree } from "./runner-codex-sinks-fixture.js";
import { nullLogger } from "./helpers.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";

installHarness();
const FEATURE = "refresh_publication_context_v1";

for (const interlocked of [false, true]) {
  for (const disposition of ["closing", "declared", "capped", "owner-partial", "pending"] as const) {
    if (!interlocked && disposition === "pending") continue; // Only interlocked publications receive this projection.
    it(`${interlocked ? "interlocked" : "legacy"} source proof uses pushed SHA and final ${disposition} predicate without inventory`, async () => {
      api.setRegisterProtocolFeatures([FEATURE, "claim_generation_fence", "completion_non_closing_v1", "completion_semantic_publication_v1"]);
      await client.register("source");
      const scope = disposition === "owner-partial"
        ? { deferred: [{ milestone_id: "m2", title: "Integration", reason: "Owner defers verification" }] }
        : {};
      const claim = gitlabClaim(3120, { claim_generation: 7, config: {
        ...(interlocked ? { completion_contract_version: 1, contract_revision: 1 } : {}),
        completion_scope: scope,
      } });
      api.setPublicationState(claim.run_id, scope.deferred ?? [], 1);
      api.setCompletionPermitResponse(true);
      api.completionPermitScope = scope;
      const forge = fakeGitlab();
      const pushed = followSuccessfulPush(git, forge.pr);
      const executor = new StubExecutor(nullLogger());
      const run = executor.run.bind(executor);
      executor.run = async ctx => {
        const result = await run(ctx);
        if (disposition === "pending") {
          const pending = { version: 1, contract_revision: 1, criteria: [{
            criterion_id: "00000000-0000-4000-8000-0000000000a1", milestone_id: "m2",
            kind: "maintainer", text: "Live check", reason: "Pending", conditions: ["Ready"],
            verification_steps: ["Observe rollout"], authorization_revision: 1, status: "unverified",
          }] };
          api.stateAckWire = (body, run) => body.status === "running"
            ? { status: 200, body: { run: { ...run, completion_pending: pending } } } : undefined;
          api.completionPermitWire = json => ({ granted: true, permit: {
            id: "permit", contract_revision: json.contract_revision, branch: json.branch, head: json.head,
            issued_at: "2026-10-20T00:00:00Z", finding_ids: [], completion_scope: {}, completion_pending: pending,
          } });
        }
        return { ...result,
          ...(disposition === "declared" ? { nonClosing: { reason: "Human check pending" } } : {}),
          ...(disposition === "capped" ? { scopeCapped: { completedCount: 1, total: 2 } } : {}),
        };
      };
      await runner(executor, forge.gitlab).execute(claim);
      const terminal = api.states.find(s => s.runId === claim.run_id && s.body.status === "completed");
      assert.ok(terminal, JSON.stringify(api.states));
      const actual = execFileSync("git", ["-C", fx.originPath, "rev-parse", "refs/heads/agent/issue-3120"], { encoding: "utf8" }).trim();
      assert.equal(pushed(), actual);
      assert.deepEqual(terminal.body.source_publication, {
        version: 1, published_head: actual, non_closing: disposition !== "closing",
      });
      assert.equal(terminal.body.completion_final_head, undefined);
      assert.equal(terminal.body.head, interlocked ? actual : undefined);
      assert.equal(terminal.body.claim_generation, 7);
    });
  }
}

it("deferred Codex terminal preserves frozen proof after boundary release and worktree change", async () => {
  api.setRegisterProtocolFeatures([FEATURE, "completion_non_closing_v1"]);
  await client.register("source");
  const forge = fakeGitlab();
  const pushed = followSuccessfulPush(git, forge.pr);
  const rig = codexRig();
  const originalBoundary = rig.safety.withBoundary.bind(rig.safety);
  let laterHead = "";
  rig.safety.withBoundary = (request, action) => originalBoundary(request, async permit => {
    const result = await action(permit);
    if (request.boundary === "finalize") {
      assert.equal(api.states.filter(s => s.body.status === "completed").length, 0);
      assert.ok(tree);
      commitInTree(tree, "LATER.txt", "later work\n");
      laterHead = execFileSync("git", ["-C", tree, "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
    }
    return result;
  });
  let tree = "";
  const result: Partial<ExecutorResult> = { nonClosing: { reason: "Human check pending" } };
  const executor = new FakeCodexExecutor(rig.safety, async ctx => {
    tree = ctx.worktreePath;
    commitInTree(tree, "SOURCE.txt", "published work\n");
    return { branch: ctx.branch, ...result };
  });
  await runnerWith(() => ({ executor }), forge.gitlab).execute(gitlabClaim(3124, { claim_generation: 7 }));
  assert.notEqual(laterHead, pushed());
  const terminal = api.states.find(s => s.body.status === "completed");
  assert.ok(terminal);
  assert.deepEqual(terminal.body.source_publication, { version: 1, published_head: pushed(), non_closing: true });
});

it("old API publication omits proof", async () => {
  await client.register("old-api");
  const forge = fakeGitlab();
  followSuccessfulPush(git, forge.pr);
  await runner(new StubExecutor(nullLogger()), forge.gitlab).execute(gitlabClaim(3121, { claim_generation: 7 }));
  const terminal = api.states.find(s => s.body.status === "completed");
  assert.ok(terminal);
  assert.equal(terminal.body.source_publication, undefined);
});

for (const mode of ["report-only", "headless", "nonissue"] as const) {
  it(mode + " does not stamp source evidence", async () => {
    api.setRegisterProtocolFeatures([FEATURE]);
    await client.register("source");
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr);
    const claim = gitlabClaim(3122, { claim_generation: 7, ...(mode === "nonissue" ? { kind: "mr_rework" } : {}) });
    const executor = mode === "nonissue" ? new StubExecutor(nullLogger()) : {
      run: async (ctx: { branch: string }): Promise<ExecutorResult> => ({
        branch: ctx.branch, ...(mode === "report-only" ? { reportOnly: true, summary: "Evidence delivered" } : {}),
      }),
    };
    await runner(executor, forge.gitlab).execute(claim);
    const terminals = api.states.filter(s => ["completed", "failed"].includes(s.body.status));
    assert.ok(terminals.length);
    for (const terminal of terminals) assert.equal(terminal.body.source_publication, undefined);
  });
}

it("negotiated feature shrink during publication refuses a proofless completion", async () => {
  api.setRegisterProtocolFeatures([FEATURE]);
  await client.register("source");
  const forge = fakeGitlab();
  followSuccessfulPush(git, forge.pr);
  const push = git.pushBranch.bind(git);
  git.pushBranch = async (...args) => {
    await push(...args);
    api.setRegisterProtocolFeatures([]);
    await client.register("shrunk");
  };
  await runner(new StubExecutor(nullLogger()), forge.gitlab).execute(gitlabClaim(3123, { claim_generation: 7 }));
  assert.equal(api.states.filter(s => s.body.status === "completed").length, 0);
});
