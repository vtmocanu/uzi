import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import type { HookInput } from "@anthropic-ai/claude-agent-sdk";
import { RunRunner } from "../src/runner.js";
import { buildAgentGuardHook, NESTED_AGENT_TOOL } from "../src/guardrails.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import type { Executor, RunContext } from "../src/executor.js";

// Issue #1660 acceptance across claims: a follow-up consumed during claim 1 must reach a
// subagent dispatched during claim 2. The steering channel is per claim, so without the
// rehydrate read (GET /runs/{id}/follow-ups) claim 2 starts with no constraints.
describe("operator constraints survive a re-claim (issue #1660)", () => {
  // Assembled at runtime: a literal token-shaped string trips the repo secret scan (gitleaks generic-api-key).
  const TOKEN = ["tkn", "constraints", "1660"].join("-");
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
    await api.close();
    fx.cleanup();
  });

  // What the SDK executor's Agent guard would hand a reviewer dispatched right now.
  const dispatchPrompt = async (ctx: RunContext): Promise<string> => {
    const hook = buildAgentGuardHook(["reviewer"], nullLogger(), () => ctx.operatorConstraints?.() ?? []);
    const out = (await hook({
      session_id: "s",
      transcript_path: "/t",
      cwd: "/w",
      hook_event_name: "PreToolUse",
      tool_name: NESTED_AGENT_TOOL,
      tool_input: { subagent_type: "reviewer", prompt: "review HEAD" },
      tool_use_id: "tu",
    } as HookInput)) as { hookSpecificOutput?: { updatedInput?: { prompt?: string } } };
    return out.hookSpecificOutput?.updatedInput?.prompt ?? "";
  };

  it("a follow-up consumed in claim 1 is attached to a dispatch in claim 2", async () => {
    const rule = "never execute a string containing kill; screen strings only";
    const claim = makeClaim({
      issue_iid: 71,
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
    });
    api.setInputs(claim.run_id, [{ id: 7, kind: "follow_up", body: rule }]);

    // Claim 1: wait until the live drain consumed the follow-up, then end the flight.
    let claim1Saw: unknown = [];
    const claim1: Executor = {
      async run(ctx) {
        const deadline = Date.now() + 5_000;
        const seen = () => ctx.operatorConstraints?.() ?? [];
        while (!(Array.isArray(seen()) && seen().length) && Date.now() < deadline) {
          await new Promise((r) => setTimeout(r, 5));
        }
        claim1Saw = seen();
        throw new Error("claim 1 ends here");
      },
    };
    await new RunRunner(client, git, () => ({ executor: claim1 }), nullLogger(), 20, undefined, { pollMs: 5 }).execute(claim);
    assert.deepStrictEqual(claim1Saw, [rule], "claim 1 consumed the follow-up live");

    // Claim 2: a fresh runner (a re-claim, possibly on another worker). Nothing is pending on
    // /inputs any more, so the only source is the rehydrate read.
    let claim2Prompt = "";
    const claim2: Executor = {
      async run(ctx) {
        claim2Prompt = await dispatchPrompt(ctx);
        throw new Error("claim 2 ends here");
      },
    };
    await new RunRunner(client, git, () => ({ executor: claim2 }), nullLogger(), 20, undefined, { pollMs: 5 }).execute({
      ...claim,
      last_seq: 0,
    });
    assert.ok(claim2Prompt.startsWith("review HEAD\n\n"), "the lead's prompt is kept first");
    assert.ok(claim2Prompt.includes(rule), "claim 2's dispatch carries claim 1's follow-up");
    assert.strictEqual(claim2Prompt.split(rule).length - 1, 1, "attached once, not duplicated");
    assert.ok((api.followUpReads.get(claim.run_id) ?? 0) >= 2, "every claim rehydrates");
  });

  it("a claim whose constraint reload fails denies every subagent dispatch and says so on the feed", async () => {
    const claim = makeClaim({
      issue_iid: 72,
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
    });
    api.overrideFollowUps(claim.run_id, 503, { error: "unavailable" });
    let decision: { permissionDecision?: string; permissionDecisionReason?: string } | undefined;
    const executor: Executor = {
      async run(ctx) {
        // As sdk-executor.ts wires it: null (unavailable) must reach the guard, not become [].
        const hook = buildAgentGuardHook(["reviewer"], nullLogger(), () => (ctx.operatorConstraints ? ctx.operatorConstraints() : []));
        const out = (await hook({
          session_id: "s",
          transcript_path: "/t",
          cwd: "/w",
          hook_event_name: "PreToolUse",
          tool_name: NESTED_AGENT_TOOL,
          tool_input: { subagent_type: "reviewer", prompt: "review HEAD" },
          tool_use_id: "tu",
        } as HookInput)) as { hookSpecificOutput?: { permissionDecision?: string; permissionDecisionReason?: string } };
        decision = out.hookSpecificOutput;
        throw new Error("claim ends here");
      },
    };
    await new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, { pollMs: 5 }).execute(claim);
    assert.ok((api.followUpReads.get(claim.run_id) ?? 0) > 1, "the reload was retried");
    assert.strictEqual(decision?.permissionDecision, "deny");
    assert.match(decision?.permissionDecisionReason ?? "", /operator constraints could not be loaded; retry the run/);
    const feed = api.messages(claim.run_id).map((m) => JSON.stringify(m.payload));
    assert.ok(feed.some((t) => t.includes("could not reload this run's earlier follow-ups")), "the feed says so");
  });

  it("a malformed row in a reload never loses an earlier valid row: the retry seeds both", async () => {
    const safety = "never execute a string containing kill; screen strings only";
    const claim = makeClaim({
      issue_iid: 73,
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
    });
    const valid = { id: 4, kind: "follow_up", body: safety, created_at: "2026-09-25T07:01:07Z" };
    api.overrideFollowUpsSequence(claim.run_id, [
      { status: 200, body: { inputs: [valid, { id: 5, kind: "follow_up", body: 42 }] } },
      { status: 200, body: { inputs: [valid, { id: 5, kind: "follow_up", body: "use port 5433" }] } },
    ]);
    let prompt = "";
    const executor: Executor = {
      async run(ctx) {
        prompt = await dispatchPrompt(ctx);
        throw new Error("claim ends here");
      },
    };
    await new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, { pollMs: 5 }).execute(claim);
    assert.strictEqual(api.followUpReads.get(claim.run_id), 2, "the malformed response was retried once");
    assert.ok(prompt.includes(safety), "the earlier valid row reaches the dispatch");
    assert.ok(prompt.includes("use port 5433"), "and so does the row the retry fixed");
    assert.strictEqual(prompt.split(safety).length - 1, 1, "once");
  });
});
