import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { JudgeRunner } from "../src/judge-runner.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import { createLogger, type Logger } from "../src/log.js";
import { providerKeyForms } from "../src/redact.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type {
  ClaimProviderBinding,
  ClaimResponse,
  JudgeTraceResponse,
  OutgoingMessage,
  ReviewRequest,
  StateRequest,
} from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { testOwnedPassHomeRoot } from "./teardown-fixtures.js";

// PRD #4322 M1a (D-H): the Claude provider-bound JUDGE on the worker. Every case drives the real
// JudgeRunner.execute with a scripted queryFn (the SDK seam) and a fake api client, and asserts only
// what the api would observe: the posted state/review/usage and the captured log output.
// Gateway keys are assembled at runtime: no provider-token-shaped literal in source.

const PASS_HOME_ROOT = testOwnedPassHomeRoot();
const KEY = ["gw", "key/do-not-scan", "7777<abcdefgh&z"].join("-");
const REDACTED = "***REDACTED***";

const trace: JudgeTraceResponse = {
  target: {
    id: "target-1", kind: "issue", status: "completed", issue_title: "Do X", issue_description: "d", branch: null,
    mr_iid: 7, failure_reason: null, fix_verdict: null, plan_md: "# plan", iteration_count: 2, repo_agents: null, stop_kind: null,
  },
  messages: [],
  inputs: [],
};

function binding(credential = KEY): ClaimProviderBinding {
  return {
    provider_id: "p-1", revision: 3, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/anthropic",
    auth_style: "bearer", credential,
    model_map: { haiku: { model: "corp-haiku" }, sonnet: { model: "corp-sonnet" } },
  };
}

function providerClaim(over: Partial<ClaimResponse> = {}, credential = KEY): ClaimResponse {
  return {
    run_id: "judge-1", kind: "judge", target_run_id: "target-1", claim_generation: 5, issue_iid: null, issue_title: "Judge: Do X",
    issue_description: "", repo: {} as never, last_seq: 0, agents: [], judge_model: "sonnet",
    judge_signal: { missing_tools: [{ command: "jq", evidence: "jq: command not found" }] },
    secrets: { forge_pat: "", anthropic_oauth_token: "", provider: binding(credential) } as never,
    ...over,
  } as ClaimResponse;
}

interface Calls {
  review?: ReviewRequest;
  states: StateRequest[];
  messages: OutgoingMessage[][];
  order: string[];
}

function fakeClient(opts: { getTrace?: () => Promise<JudgeTraceResponse>; staleFirst?: boolean } = {}) {
  const calls: Calls = { states: [], messages: [], order: [] };
  let running = 0;
  const client = {
    getTrace: async () => { calls.order.push("getTrace"); return opts.getTrace ? opts.getTrace() : trace; },
    postReview: async (_id: string, review: ReviewRequest) => { calls.review = review; },
    reportState: async (_id: string, body: StateRequest) => {
      calls.order.push(`state:${body.status}`);
      calls.states.push(body);
      if (body.status === "running" && ++running === 1 && opts.staleFirst) return { applied: false, staleClaim: true } as never;
      return { applied: true, status: body.status } as never;
    },
    postMessages: async (_id: string, messages: OutgoingMessage[]) => { calls.messages.push(messages); },
  } as unknown as WorkerClient;
  return { client, calls };
}

const assistant = (text: string) => ({ type: "assistant", message: { role: "assistant", content: [{ type: "text", text }] } });
const verdict = (summary = "fine") => JSON.stringify({ verdict: "ok", summary, recommendations: [] });
/** A pass that streams one assistant frame, then throws. */
const failing = (message: string) => async function* () { yield assistant("partial"); throw new Error(message); };
const success = (extra: Record<string, unknown> = {}) => ({ type: "result", subtype: "success", is_error: false, ...extra });

/** A scripted queryFn that counts its calls; `frames` may also throw. */
function scripted(frames: unknown[] | (() => AsyncGenerator<unknown>)) {
  const seen = { calls: 0, model: undefined as string | undefined, prompt: undefined as unknown };
  const queryFn = ((params: { prompt?: unknown; options: { model?: string } }) => {
    seen.calls++;
    seen.model = params.options.model;
    seen.prompt = params.prompt;
    return typeof frames === "function" ? frames() : (async function* () { for (const f of frames) yield f; })();
  }) as unknown as SdkQueryFn;
  return { queryFn, seen };
}

async function run(frames: Parameters<typeof scripted>[0], claim: ClaimResponse = providerClaim(), extra: { log?: Logger; client?: ReturnType<typeof fakeClient>; modelTimeoutMs?: number } = {}) {
  const { queryFn, seen } = scripted(frames);
  const api = extra.client ?? fakeClient();
  const runner = new JudgeRunner(api.client, extra.log ?? nullLogger(), { homeRoot: PASS_HOME_ROOT, queryFn, modelTimeoutMs: extra.modelTimeoutMs });
  await runner.execute(claim);
  return { calls: api.calls, seen };
}

function assertFailedNoReview(calls: Calls, reason?: RegExp | string): StateRequest {
  const last = calls.states.at(-1)!;
  assert.equal(last.status, "failed");
  assert.equal(calls.review, undefined, "no review is posted");
  assert.equal(calls.states.some(s => s.status === "completed"), false, "never completed");
  if (typeof reason === "string") assert.equal(last.failure_reason, reason);
  else if (reason) assert.match(last.failure_reason ?? "", reason);
  return last;
}

describe("provider judge: deterministic result-frame settlement", () => {
  for (const status of [429, 529]) {
    it(`an error result with api_error_status ${status} fails as provider_gateway_limit with no reset`, async () => {
      const { calls, seen } = await run([assistant("x"), { type: "result", subtype: "success", is_error: true, api_error_status: status }]);
      assert.equal(seen.calls, 1);
      const last = assertFailedNoReview(calls);
      assert.equal(last.rate_limit_type, "provider_gateway_limit");
      assert.equal(last.limit_resets_at, undefined);
      assert.equal(last.claim_generation, 5);
    });
  }

  it("api_error_status collision: a credential equal to that member name still settles the real 429", async () => {
    const { calls } = await run([{ type: "result", subtype: "success", is_error: true, api_error_status: 429 }], providerClaim({}, "api_error_status"));
    assert.equal(assertFailedNoReview(calls).rate_limit_type, "provider_gateway_limit");
  });

  it("raw-frame guard: a credential that rewrites the is_error member name cannot turn a limit into a success", async () => {
    const { calls } = await run([assistant(verdict()), { type: "result", subtype: "success", is_error: true, api_error_status: 429 }], providerClaim({}, "is_error"));
    assert.equal(assertFailedNoReview(calls).rate_limit_type, "provider_gateway_limit");
  });

  for (const [name, frame] of [
    ["api_error_status null", { type: "result", subtype: "success", is_error: true, api_error_status: null }],
    ["no api_error_status member", { type: "result", subtype: "success", is_error: true }],
    ["a non-success subtype with no is_error member and no status", { type: "result", subtype: "error_during_execution" }],
  ] as const) {
    it(`${name}: terminal non-limit failure, no review`, async () => {
      const { calls } = await run([assistant(verdict()), frame]);
      const last = assertFailedNoReview(calls, "provider judge model returned an error result without a status");
      assert.equal(last.rate_limit_type, undefined);
    });
  }

  it("a 401 error result keeps the deterministic fallback review and completes", async () => {
    const { calls } = await run([{ type: "result", subtype: "success", is_error: true, api_error_status: 401 }]);
    assert.equal(calls.review?.status, "failed");
    assert.equal(calls.review?.recommendations[0]?.target, "jq");
    assert.equal(calls.states.at(-1)?.status, "completed");
    assert.equal(calls.states.some(s => s.status === "failed"), false);
  });

  it("a prompt-build failure keeps the fallback review and completes with no model call", async () => {
    // A page fetchTrace accepts but buildJudgePrompt cannot render (a non-string plan_md makes the
    // prompt clip throw), so this reaches judgeProvider's prompt-build branch, not the outer
    // trace/prep catch in executeClaim.
    const malformed = { ...trace, target: { ...trace.target, plan_md: { length: 7000 } } } as unknown as JudgeTraceResponse;
    const warns: string[] = [];
    const base = nullLogger();
    const log: Logger = { ...base, warn: (msg: string, ...rest: unknown[]) => { warns.push(msg); return (base.warn as (...a: unknown[]) => void)(msg, ...rest); } };
    const { calls, seen } = await run([assistant(verdict()), success()], providerClaim(), { log, client: fakeClient({ getTrace: async () => malformed }) });
    assert.ok(warns.some(w => /judge prompt build failed/.test(w)), `the prompt-build branch ran: ${JSON.stringify(warns)}`);
    assert.ok(!warns.some(w => /judge trace\/prep failed/.test(w)), "not the outer trace/prep catch");
    assert.equal(seen.calls, 0, "no model call");
    assert.equal(calls.review?.status, "failed");
    assert.equal(calls.review?.recommendations[0]?.target, "jq");
    assert.equal(calls.states.at(-1)?.status, "completed");
    assert.equal(calls.states.some(s => s.status === "failed"), false);
  });

  it("EOF with no result frame is a terminal no-result failure", async () => {
    const { calls } = await run([assistant(verdict())]);
    assertFailedNoReview(calls, "provider judge model returned no result");
  });

  it("a thrown pass error is a terminal failure with the key redacted, never completed", async () => {
    const { calls, seen } = await run(failing(`gateway said no for ${KEY}`));
    assert.equal(seen.calls, 1);
    const last = assertFailedNoReview(calls, /gateway said no for/);
    assert.ok(last.failure_reason?.includes(REDACTED));
    assert.ok(!last.failure_reason?.includes(KEY));
  });

  it("a model timeout is a terminal failure, never completed", async () => {
    const hang: SdkQueryFn = ((params: { options: { abortController?: AbortController } }) => (async function* () {
      await new Promise<void>(resolve => params.options.abortController?.signal.addEventListener("abort", () => resolve()));
      yield assistant("never delivered");
    })()) as unknown as SdkQueryFn;
    const api = fakeClient();
    const runner = new JudgeRunner(api.client, nullLogger(), { homeRoot: PASS_HOME_ROOT, queryFn: hang, modelTimeoutMs: 40 });
    await runner.execute(providerClaim());
    assertFailedNoReview(api.calls, /judge model call exceeded 40ms/);
  });

  it("an unmapped model fails as provider_model_unmapped with no model call", async () => {
    const { calls, seen } = await run([success()], providerClaim({ judge_model: "opus" }));
    assert.equal(seen.calls, 0);
    assertFailedNoReview(calls, "provider_model_unmapped");
  });

  it("a mapped alias reaches the model call unchanged", async () => {
    const { calls, seen } = await run([assistant(verdict()), success()], providerClaim({ judge_model: "haiku" }));
    assert.equal(seen.model, "haiku");
    assert.equal(calls.review?.status, "complete");
    assert.equal(calls.states.at(-1)?.status, "completed");
  });
});

describe("provider judge: output redaction", () => {
  it("scrubs the key from the review, the model name and the usage frame, and reports unknown cost", async () => {
    const mappedModel = `corp-${KEY}`;
    const claim = providerClaim({ judge_model: mappedModel });
    (claim.secrets.provider!.model_map as { sonnet?: { model: string } }).sonnet = { model: mappedModel };
    const echoed = JSON.stringify({ verdict: "issues", summary: `the key ${KEY} leaked`,
      recommendations: [{ category: "improve_uzi", target: `t-${KEY}`, rationale: `rationale ${KEY}`, confidence: "high" }] });
    const { calls, seen } = await run([
      assistant(echoed),
      success({ total_cost_usd: 0.42, modelUsage: { [`m-${KEY}`]: { inputTokens: 100, outputTokens: 50, costUSD: 0.42 } } }),
    ], claim);
    assert.equal(seen.calls, 1, "the model was called");
    const review = calls.review!;
    assert.equal(review.status, "complete");
    assert.ok(review.summary.includes(REDACTED));
    assert.ok(review.model.includes(REDACTED));
    assert.ok(review.recommendations[0]!.rationale.includes(REDACTED));
    assert.ok(review.recommendations[0]!.target.includes(REDACTED));
    assert.equal(calls.messages.length, 1, "a usage frame is posted");
    const usage = calls.messages[0]![0]!;
    const wire = JSON.stringify([review, usage]);
    for (const form of providerKeyForms(KEY)) assert.ok(!wire.includes(form), `a key form reached the wire: ${form.slice(0, 8)}`);
    const payload = usage.payload as { modelUsage: Record<string, { costUSD?: number }>; total_cost_usd?: number };
    assert.ok(Object.keys(payload.modelUsage).some(m => m.includes(REDACTED)));
    assert.equal(payload.total_cost_usd, undefined);
    for (const u of Object.values(payload.modelUsage)) assert.equal(u.costUSD, undefined);
    assert.equal(calls.states.at(-1)?.status, "completed");
  });

  it("a model error echoing the key: the model was called, the failed report is redacted", async () => {
    const { calls, seen } = await run(failing(`upstream rejected ${KEY}`));
    assert.equal(seen.calls, 1);
    const last = assertFailedNoReview(calls, /upstream rejected/);
    for (const form of providerKeyForms(KEY)) assert.ok(!(last.failure_reason ?? "").includes(form));
  });
});

describe("provider judge: whole-claim log redaction (real logger)", () => {
  async function captured(fn: () => Promise<void>): Promise<string> {
    const chunks: string[] = [];
    const origOut = process.stdout.write;
    const origErr = process.stderr.write;
    // Only the logger's JSON lines are captured; every other write (the test runner reports results
    // over the same stream) passes straight through.
    const tee = (stream: NodeJS.WriteStream, orig: typeof process.stdout.write) =>
      ((chunk: unknown, ...rest: unknown[]): boolean => {
        if (typeof chunk === "string" && chunk.startsWith('{"ts"')) { chunks.push(chunk); return true; }
        return (orig as unknown as (...a: unknown[]) => boolean).call(stream, chunk, ...rest);
      }) as unknown as typeof process.stdout.write;
    process.stdout.write = tee(process.stdout, origOut);
    process.stderr.write = tee(process.stderr, origErr);
    try { await fn(); } finally {
      process.stdout.write = origOut;
      process.stderr.write = origErr;
    }
    return chunks.join("");
  }
  function assertNoKey(out: string): void {
    for (const form of providerKeyForms(KEY)) assert.ok(!out.includes(form), `a key form reached the log: ${form.slice(0, 8)}`);
  }

  it("a trace fetch failure whose body echoes the key is logged redacted (registered before fetchTrace)", async () => {
    const api = fakeClient({ getTrace: async () => { throw new RequestError("GET", "/trace", 502, `bad gateway for ${KEY}`); } });
    const out = await captured(async () => {
      await run([], providerClaim(), { log: createLogger("info"), client: api });
    });
    assert.match(out, /judge trace\/prep failed/);
    assert.ok(out.includes(REDACTED));
    assertNoKey(out);
    assert.equal(api.calls.review?.status, "failed", "a prep failure keeps the deterministic fallback review");
    assert.equal(api.calls.states.at(-1)?.status, "completed");
  });

  it("a model error echoing the key is logged redacted", async () => {
    const out = await captured(async () => {
      const { calls } = await run(failing(`model blew up with ${KEY}`), providerClaim(), { log: createLogger("info") });
      assertFailedNoReview(calls);
    });
    assert.match(out, /provider judge model call failed/);
    assert.ok(out.includes(REDACTED));
    assertNoKey(out);
  });

  for (const scenario of ["success", "stale ack", "limit"] as const) {
    it(`registers the key forms before the first report and removes every one on exit (${scenario})`, async () => {
      const api = fakeClient({ staleFirst: scenario === "stale ack" });
      const events = api.calls.order; // one timeline: secret registrations and api calls
      const base = nullLogger();
      const log: Logger = { ...base, addSecret: s => { events.push(`add:${s}`); }, removeSecret: s => { events.push(`remove:${s}`); } };
      const frames = scenario === "limit"
        ? [{ type: "result", subtype: "success", is_error: true, api_error_status: 529 }]
        : [assistant(verdict()), success()];
      await run(frames, providerClaim(), { log, client: api });
      const forms = providerKeyForms(KEY);
      const adds = events.filter(e => e.startsWith("add:"));
      assert.deepEqual(adds.map(e => e.slice(4)).sort(), [...forms].sort());
      assert.deepEqual(events.filter(e => e.startsWith("remove:")).map(e => e.slice(7)).sort(), [...forms].sort());
      assert.equal(events.slice(0, forms.length).every(e => e.startsWith("add:")), true, "registered before the first api call");
      assert.equal(events[forms.length], "state:running");
      assert.ok(events.slice(-forms.length).every(e => e.startsWith("remove:")), "removed last, on every exit");
    });
  }
});

describe("provider judge: reported reasons and fallbacks are scrubbed of the key", () => {
  const noKey = (s: string) => { for (const form of providerKeyForms(KEY)) assert.ok(!s.includes(form), `a key form leaked: ${form.slice(0, 8)}`); };

  it("a review POST failure whose body echoes the key reports a scrubbed failed reason", async () => {
    const api = fakeClient();
    (api.client as unknown as { postReview: () => Promise<void> }).postReview = async () => { throw new RequestError("POST", "/review", 500, `upstream said ${KEY}`); };
    const { calls } = await run([assistant(verdict()), success()], providerClaim(), { client: api });
    const last = calls.states.at(-1)!;
    assert.equal(last.status, "failed");
    assert.ok((last.failure_reason ?? "").includes(REDACTED));
    noKey(last.failure_reason ?? "");
  });

  it("a prep failure posts a fallback review built from a key-bearing signal with the key scrubbed", async () => {
    const api = fakeClient({ getTrace: async () => { throw new Error("trace down"); } });
    const claim = providerClaim({ judge_signal: { missing_tools: [{ command: `curl -H ${KEY}`, evidence: `${KEY}: command not found` }] } });
    const { calls } = await run([], claim, { client: api });
    assert.ok(calls.review, "a fallback review is posted");
    noKey(JSON.stringify(calls.review));
    assert.ok(JSON.stringify(calls.review).includes(REDACTED));
    assert.equal(calls.states.at(-1)?.status, "completed");
  });

  it("a key straddling the fallback's 200-char evidence clip is scrubbed before clipping, leaving no partial prefix", async () => {
    const api = fakeClient({ getTrace: async () => { throw new Error("trace down"); } });
    // The clip keeps the first 188 chars, so the key (starting at 160) would be cut mid-key.
    const evidence = `${"x".repeat(160)}${KEY}: command not found`;
    const claim = providerClaim({ judge_signal: { missing_tools: [{ command: "jq", evidence }] } });
    const { calls } = await run([], claim, { client: api });
    const rationale = calls.review?.recommendations?.[0]?.rationale ?? "";
    assert.ok(rationale.includes(REDACTED), "the key is replaced before the clip");
    assert.ok(!rationale.includes(KEY.slice(0, 12)), "no partial key prefix survives the clip");
  });

  // Each clip keeps cap - 12 characters before its "…[truncated]" marker (plan_md 6000 -> 5988,
  // failure_reason 800 -> 788, a message payload preview 600 -> 588, rendered as {"text":"<value>
  // so the value starts at offset 9). Each key starts ten characters before its retained boundary,
  // so clipping before scrubbing would leave a key prefix in the prompt. One field per case.
  // (A scrubbed field is shorter than its cap, so the scrubbed prompt holds the filler and no marker.)
  const cases: Array<[string, string, (t: JudgeTraceResponse) => JudgeTraceResponse]> = [
    ["plan_md", "p", (t) => ({ ...t, target: { ...t.target, plan_md: `${"p".repeat(5978)}${KEY}` } })],
    ["failure_reason", "f", (t) => ({ ...t, target: { ...t.target, failure_reason: `${"f".repeat(778)}${KEY}` } })],
    ["message evidence", "m", (t) => ({ ...t, messages: [{ seq: 1, kind: "assistant", agent: "lead", agent_instance: null,
      created_at: "2026-11-25T10:00:00Z", payload: { text: `${"m".repeat(569)}${KEY}` } } as never] })],
  ];
  for (const [field, filler, keyed] of cases) {
    it(`a key straddling the ${field} clip is scrubbed from the trace before the prompt clips it`, async () => {
      const api = fakeClient({ getTrace: async () => keyed(trace) });
      const { seen } = await run([assistant(verdict()), success()], providerClaim(), { client: api });
      assert.equal(seen.calls, 1, "the model pass ran");
      // The pass hands the SDK an async stream of user messages; the scripted queryFn never drains it.
      const parts: unknown[] = [];
      for await (const m of seen.prompt as AsyncIterable<unknown>) parts.push(m);
      const prompt = JSON.stringify(parts);
      assert.ok(prompt.includes(filler.repeat(500)), "the field reached the prompt");
      assert.ok(!prompt.includes(KEY.slice(0, 8)), "no key prefix survives the clip");
    });
  }
});
