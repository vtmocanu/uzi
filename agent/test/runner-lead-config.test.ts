import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { RunRunner } from "../src/runner.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimConfig } from "../src/protocol.js";
import type { LeadConfigReporter, LeadRequest } from "../src/lead-config-report.js";

// PRD #3244: the runner constructs exactly one lead-config reporter per claim, carries it as
// RunContext.leadConfig, and every executor-side call of that claim reaches the same instance, so
// rows from a re-driven turn or a later phase keep one seq line on the server.
describe("runner lead-config reporter (PRD #3244)", () => {
  // Assembled at runtime: a literal token-shaped string trips the repo secret scan (gitleaks generic-api-key).
  const TOKEN = ["tkn", "lead", "config", "3244"].join("-");
  let api: FakeApi;
  let fx: Fixture;
  let client: WorkerClient;
  let git: GitCache;

  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    const baseUrl = await api.listen();
    fx = makeFixture();
    git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  });
  afterEach(async () => {
    // try/finally (issue #2020): a throwing close must not skip the fixture cleanup.
    try {
      await api.close();
    } finally {
      fx.cleanup();
    }
  });

  const REQ: LeadRequest = { model: "opus", modelSource: "user_default", effort: "medium", effortMode: "explicit", effortSource: "builtin" };
  const ACTIVE: ClaimConfig = {
    default_model: "opus", default_model_source: "user_default", default_effort: "medium", default_effort_source: "builtin",
  };

  const claimWith = (iid: number, config: ClaimConfig | null, generation = 7) => makeClaim({
    issue_iid: iid,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    claim_generation: generation,
    config,
  });

  const execute = (claim: ReturnType<typeof claimWith>, script: (ctx: RunContext) => Promise<void>) => {
    const executor: Executor = {
      async run(ctx) {
        await script(ctx);
        throw new Error("the executor ends here");
      },
    };
    return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, { pollMs: 5 }).execute(claim);
  };

  it("hands every phase and re-driven turn of a claim the same reporter, with one continuing seq line", async () => {
    const claim = claimWith(3244, ACTIVE);
    const seen: Array<LeadConfigReporter | undefined> = [];
    await execute(claim, async (ctx) => {
      seen.push(ctx.leadConfig);
      await ctx.leadConfig!.enterPhase("plan", REQ);
      seen.push(ctx.leadConfig);
      await ctx.leadConfig!.enterPhase("plan", REQ); // a re-driven turn
      await ctx.leadConfig!.enterPhase("implement", REQ); // approval: a new phase
      await ctx.leadConfig!.enterPhase("implement", REQ); // a recreated epoch drives the same phase again
      await ctx.leadConfig!.enterPhase("plan", REQ); // a post-approval re-plan: a new plan entry
    });
    assert.ok(seen[0], "the runner constructed a reporter");
    assert.equal(seen[0], seen[1], "one instance for the whole claim");
    assert.deepEqual(
      api.leadConfigPosts.map((p) => `${p.runId === claim.run_id ? "this-run" : p.runId}:${String(p.body.phase)}#${String(p.body.phase_entry)}/${String(p.body.seq)}`),
      ["this-run:plan#1/1", "this-run:implement#1/1", "this-run:plan#2/2"],
    );
    assert.ok(api.leadConfigPosts.every((p) => p.body.claim_generation === 7));
    assert.equal(api.leadConfigPosts[0]!.body.requested_model, "opus");
    assert.equal(api.leadConfigPosts[0]!.body.observed_model, null);
  });

  it("a different claim gets its own reporter and its own numbering", async () => {
    const first = claimWith(3245, ACTIVE, 1);
    const second = claimWith(3246, ACTIVE, 2);
    let r1: LeadConfigReporter | undefined;
    let r2: LeadConfigReporter | undefined;
    await execute(first, async (ctx) => { r1 = ctx.leadConfig; await ctx.leadConfig!.enterPhase("implement", REQ); });
    await execute(second, async (ctx) => { r2 = ctx.leadConfig; await ctx.leadConfig!.enterPhase("implement", REQ); });
    assert.notEqual(r1, r2);
    assert.deepEqual(api.leadConfigPosts.map((p) => [p.body.claim_generation, p.body.seq]), [[1, 1], [2, 1]]);
  });

  it("is inactive when the claim carries no default_effort_source", async () => {
    const claim = claimWith(3247, { default_model: "opus", default_effort: "medium" });
    await execute(claim, async (ctx) => { await ctx.leadConfig!.enterPhase("plan", REQ); });
    assert.equal(api.leadConfigPosts.length, 0);
  });

  it("stops reporting for the rest of the claim after the server says the claim is not current", async () => {
    const claim = claimWith(3248, ACTIVE);
    api.leadConfig.queue.push({ status: 409, reason: "claim_not_current" });
    await execute(claim, async (ctx) => {
      await ctx.leadConfig!.enterPhase("plan", REQ);
      await new Promise((r) => setTimeout(r, 20));
      await ctx.leadConfig!.enterPhase("implement", REQ);
      await new Promise((r) => setTimeout(r, 20));
    });
    assert.equal(api.leadConfigPosts.length, 1);
  });
});
