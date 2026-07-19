import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Options as SdkOptions, HookInput } from "@anthropic-ai/claude-agent-sdk";

import { JudgeRunner, buildJudgePrompt, parseReview, fallbackReview } from "../src/judge-runner.js";
import { stubJudgeQueryFn } from "../src/judge-runner-stub.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { WorkerClient } from "../src/client.js";
import type { ClaimResponse, JudgeTraceResponse, ReviewRequest, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// A fake worker client recording the review + state the judge posts.
function fakeClient(trace: JudgeTraceResponse) {
  const calls: { review?: { id: string; review: ReviewRequest }; state?: { id: string; body: StateRequest } } = {};
  const client = {
    getTrace: async () => trace,
    postReview: async (id: string, review: ReviewRequest) => {
      calls.review = { id, review };
    },
    reportState: async (id: string, body: StateRequest) => {
      calls.state = { id, body };
    },
  } as unknown as WorkerClient;
  return { client, calls };
}

const emptyTrace: JudgeTraceResponse = {
  target: {
    id: "target-1",
    kind: "issue",
    status: "completed",
    issue_title: "Do X",
    issue_description: "d",
    branch: null,
    mr_iid: 7,
    failure_reason: null,
    fix_verdict: null,
    plan_md: "# plan",
    iteration_count: 2,
    repo_agents: null,
  },
  messages: [],
  inputs: [],
};

function judgeClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return {
    run_id: "judge-1",
    kind: "judge",
    target_run_id: "target-1",
    issue_iid: null,
    issue_title: "Judge: Do X",
    issue_description: "",
    repo: {} as never,
    secrets: { forge_pat: "", anthropic_oauth_token: "tok-abc" } as never,
    last_seq: 0,
    agents: [],
    judge_model: "haiku",
    judge_signal: { missing_tools: [{ command: "jq", evidence: "jq: command not found" }] },
    ...overrides,
  } as ClaimResponse;
}

// A queryFn that emits one assistant text block then a terminal result.
function replyingQueryFn(text: string, error = false): SdkQueryFn {
  return async function* () {
    yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text }] } };
    yield { type: "result", subtype: error ? "error_max_turns" : "success", is_error: error };
  } as unknown as SdkQueryFn;
}

describe("JudgeRunner", () => {
  it("posts the parsed verdict + recommendations and completes the run", async () => {
    const { client, calls } = fakeClient(emptyTrace);
    const modelJson = JSON.stringify({
      verdict: "issues",
      summary: "needs a tool",
      recommendations: [{ category: "install_worker_tool", target: "jq", rationale: "missing", confidence: "high" }],
    });
    const runner = new JudgeRunner(client, nullLogger(), { queryFn: replyingQueryFn(modelJson) });
    await runner.execute(judgeClaim());

    assert.equal(calls.review?.id, "target-1");
    assert.equal(calls.review?.review.verdict, "issues");
    assert.equal(calls.review?.review.status, "complete");
    assert.equal(calls.review?.review.recommendations.length, 1);
    assert.equal(calls.review?.review.recommendations[0]?.target, "jq");
    assert.equal(calls.state?.id, "judge-1");
    assert.equal(calls.state?.body.status, "completed");
  });

  it("falls back to the deterministic command-not-found review when the model errors", async () => {
    const { client, calls } = fakeClient(emptyTrace);
    const runner = new JudgeRunner(client, nullLogger(), { queryFn: replyingQueryFn("garbage", true) });
    await runner.execute(judgeClaim());

    assert.equal(calls.review?.review.status, "failed");
    assert.equal(calls.review?.review.verdict, "issues");
    assert.equal(calls.review?.review.recommendations[0]?.category, "install_worker_tool");
    assert.equal(calls.review?.review.recommendations[0]?.target, "jq");
    // Even a fallback completes the judge run (it produced a review).
    assert.equal(calls.state?.body.status, "completed");
  });

  it("posts the deterministic fallback when the trace fetch fails (findings still land)", async () => {
    const { calls } = fakeClient(emptyTrace);
    // A client whose getTrace throws, but postReview/reportState still work.
    const client = {
      getTrace: async () => {
        throw new Error("trace endpoint 500");
      },
      postReview: async (id: string, review: ReviewRequest) => {
        calls.review = { id, review };
      },
      reportState: async (id: string, body: StateRequest) => {
        calls.state = { id, body };
      },
    } as unknown as WorkerClient;
    const runner = new JudgeRunner(client, nullLogger(), { queryFn: replyingQueryFn("{}") });
    await runner.execute(judgeClaim());

    assert.equal(calls.review?.review.status, "failed", "a trace-fetch failure still posts the deterministic fallback");
    assert.equal(calls.review?.review.recommendations[0]?.target, "jq", "the command-not-found signal from the claim lands");
    assert.equal(calls.state?.body.status, "completed", "the judge run completes with the fallback, not fails with no review");
  });

  it("falls back when the claim carries no Anthropic token (never calls the model)", async () => {
    const { client, calls } = fakeClient(emptyTrace);
    let queried = false;
    const queryFn: SdkQueryFn = (() => {
      queried = true;
      return (async function* () {})();
    }) as unknown as SdkQueryFn;
    const runner = new JudgeRunner(client, nullLogger(), { queryFn });
    await runner.execute(judgeClaim({ secrets: { forge_pat: "", anthropic_oauth_token: "" } as never }));

    assert.equal(queried, false, "no token ⇒ the model must not be called");
    assert.equal(calls.review?.review.status, "failed");
  });

  it("fails the run when the judge claim carries no target", async () => {
    const { client, calls } = fakeClient(emptyTrace);
    const runner = new JudgeRunner(client, nullLogger(), { queryFn: replyingQueryFn("{}") });
    await runner.execute(judgeClaim({ target_run_id: null }));
    assert.equal(calls.review, undefined, "no review without a target");
    assert.equal(calls.state?.body.status, "failed");
  });

  it("the UZI_E2E_EXECUTOR=stub judge queryFn drives the deterministic fallback (M8/B)", async () => {
    const { client, calls } = fakeClient(emptyTrace);
    const runner = new JudgeRunner(client, nullLogger(), { queryFn: stubJudgeQueryFn });
    await runner.execute(judgeClaim());
    // The stub yields an error result → JudgeRunner falls back to the claim's
    // command-not-found signal, so the e2e sees a real review naming jq, no spend.
    assert.equal(calls.review?.review.status, "failed");
    assert.equal(calls.review?.review.recommendations[0]?.category, "install_worker_tool");
    assert.equal(calls.review?.review.recommendations[0]?.target, "jq");
    assert.equal(calls.state?.body.status, "completed");
  });

  it("caps the model call by wall clock: a hung query still lands the fallback (M8/B)", async () => {
    const { client, calls } = fakeClient(emptyTrace);
    // A queryFn that never yields a terminal result — without the cap, runModel would
    // hang forever. modelTimeoutMs is injected tiny so the test is fast.
    const hung: SdkQueryFn = (() =>
      (async function* () {
        await new Promise((r) => setTimeout(r, 60_000).unref()); // longer than the injected cap; unref so the abandoned timer never holds the event loop open (else the file exceeds --test-timeout in CI)
      })()) as unknown as SdkQueryFn;
    const runner = new JudgeRunner(client, nullLogger(), { queryFn: hung, modelTimeoutMs: 20 });
    const started = Date.now();
    await runner.execute(judgeClaim());
    assert.ok(Date.now() - started < 5_000, "the wall-clock cap must abort the hung model call promptly");
    assert.equal(calls.review?.review.status, "failed", "a timed-out model call still posts the deterministic fallback");
    assert.equal(calls.review?.review.recommendations[0]?.target, "jq");
    assert.equal(calls.state?.body.status, "completed");
  });
});

describe("parseReview", () => {
  it("parses a fenced JSON block", () => {
    const r = parseReview('```json\n{"verdict":"ok","summary":"s","recommendations":[]}\n```', "haiku");
    assert.equal(r.verdict, "ok");
    assert.equal(r.model, "haiku");
  });

  it("parses JSON embedded in prose and drops unknown categories", () => {
    const text =
      'Here is my assessment: {"verdict":"issues","summary":"s","recommendations":[' +
      '{"category":"install_worker_tool","target":"jq","rationale":"x","confidence":"high"},' +
      '{"category":"nuke_everything","target":"y","rationale":"z"}]} done.';
    const r = parseReview(text, "haiku");
    assert.equal(r.recommendations.length, 1, "the unknown category is dropped client-side");
    assert.equal(r.recommendations[0]?.category, "install_worker_tool");
  });

  it("throws on an invalid verdict (→ caller falls back)", () => {
    assert.throws(() => parseReview('{"verdict":"amazing","summary":"s","recommendations":[]}', "haiku"));
  });

  it("throws when there is no JSON object", () => {
    assert.throws(() => parseReview("the model refused to answer", "haiku"));
  });
});

describe("fallbackReview", () => {
  it("maps missing tools to install_worker_tool recommendations", () => {
    const r = fallbackReview({ missing_tools: [{ command: "shellcheck", evidence: "not found" }] });
    assert.equal(r.status, "failed");
    assert.equal(r.verdict, "issues");
    assert.equal(r.recommendations[0]?.category, "install_worker_tool");
    assert.equal(r.recommendations[0]?.target, "shellcheck");
  });

  it("yields an empty-recommendation ok verdict when there is no signal", () => {
    const r = fallbackReview(null);
    assert.equal(r.verdict, "ok");
    assert.equal(r.recommendations.length, 0);
  });
});

describe("buildJudgePrompt", () => {
  it("fences the trace with an unforgeable per-prompt nonce and includes the signal", () => {
    const prompt = buildJudgePrompt(emptyTrace, { missing_tools: [{ command: "jq", evidence: "jq: not found" }] });
    assert.match(prompt, /UNTRUSTED DATA/);
    assert.match(prompt, /command-not-found/i);
    assert.match(prompt, /jq/);
    // The fence tag carries a random 16-hex nonce, and the same nonce closes it — so
    // an attacker in the trace can't forge the closing tag.
    const open = prompt.match(/<untrusted_trace_([0-9a-f]{16})>/);
    assert.ok(open, "expected a nonced open tag");
    assert.match(prompt, new RegExp(`</untrusted_trace_${open![1]}>`), "close tag must reuse the same nonce");
  });

  it("mints a different nonce on each build (CSPRNG, not a static sentinel)", () => {
    const a = buildJudgePrompt(emptyTrace, null).match(/<untrusted_trace_([0-9a-f]{16})>/)?.[1];
    const b = buildJudgePrompt(emptyTrace, null).match(/<untrusted_trace_([0-9a-f]{16})>/)?.[1];
    assert.ok(a && b && a !== b, "each prompt must mint a fresh nonce");
  });
});

// PRD #89 M-allow / auditor Medium. ClaimRun's `repo_id IS NULL` exemption lets a
// docker-enabled worker claim repo-less judge runs (and the chat lane is ungated).
// That is safe ONLY because these repo-less executors reach no daemon-capable tool —
// a property that lives here in agent/, not at the claim gate. This pins it so a
// future tool addition to the judge trips CI. The chat side is pinned by
// chat-executor.test.ts ("restricts the tool set … no Bash/Write/Edit/WebFetch/
// WebSearch/Agent"); this covers the judge, whose confinement is a deny-ALL
// PreToolUse hook (it grants no `tools` allowlist at all).
describe("judge tool confinement (PRD #89 M-allow / auditor Medium)", () => {
  // Every tool a hijacked judge could use to reach dockerd (or otherwise execute /
  // exfiltrate). The judge must deny all of them even under bypassPermissions.
  const DAEMON_REACHING = [
    "Bash",
    "Write",
    "Edit",
    "MultiEdit",
    "NotebookEdit",
    "WebFetch",
    "WebSearch",
    "Agent",
    "Task",
  ];

  // A queryFn that captures the SdkOptions the judge hands the model, then emits a
  // valid terminal result so execute() completes.
  function capturingQueryFn(text: string): { queryFn: SdkQueryFn; captured: { options?: SdkOptions } } {
    const captured: { options?: SdkOptions } = {};
    const queryFn: SdkQueryFn = (params) => {
      captured.options = params.options;
      return (async function* () {
        yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text }] } };
        yield { type: "result", subtype: "success", is_error: false };
      })() as never;
    };
    return { queryFn, captured };
  }

  // Run every PreToolUse hook group that applies to toolName and return the last
  // permissionDecision (a group with no matcher applies to all tools).
  async function preToolUseDecision(options: SdkOptions, toolName: string): Promise<string | undefined> {
    const groups = (options.hooks?.PreToolUse ?? []) as Array<{
      matcher?: string;
      hooks: Array<(input: HookInput) => Promise<unknown>>;
    }>;
    let decision: string | undefined;
    for (const g of groups) {
      if (g.matcher && !new RegExp(g.matcher).test(toolName)) continue;
      for (const h of g.hooks) {
        const out = (await h({
          hook_event_name: "PreToolUse",
          tool_name: toolName,
          tool_input: {},
          tool_use_id: "t",
        } as unknown as HookInput)) as { hookSpecificOutput?: { permissionDecision?: string } };
        if (out?.hookSpecificOutput?.permissionDecision) decision = out.hookSpecificOutput.permissionDecision;
      }
    }
    return decision;
  }

  it("wires a PreToolUse hook that DENIES every daemon-reaching tool, and grants no tools allowlist", async () => {
    const { client } = fakeClient(emptyTrace);
    const { queryFn, captured } = capturingQueryFn(
      JSON.stringify({ verdict: "ok", summary: "", recommendations: [] }),
    );
    const runner = new JudgeRunner(client, nullLogger(), { queryFn });
    await runner.execute(judgeClaim());

    const options = captured.options;
    assert.ok(options, "the judge must have called the model (so options were captured)");
    // Repo-borne .claude/ can't grant permissions, and there is NO `tools` allowlist
    // that could widen the surface — confinement is the deny-all hook below.
    assert.deepStrictEqual(options!.settingSources, []);
    assert.equal(
      options!.tools,
      undefined,
      "the judge must grant no tools allowlist (its confinement is the deny-all hook)",
    );
    // The load-bearing invariant: every daemon-reaching tool is denied, so even with
    // DOCKER_HOST set a hijacked judge cannot invoke docker.
    for (const t of DAEMON_REACHING) {
      assert.equal(await preToolUseDecision(options!, t), "deny", `${t} must be denied for the judge`);
    }
  });
});
