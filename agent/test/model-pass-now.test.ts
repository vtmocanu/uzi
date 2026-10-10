// PRD #2603: the optional fields runReadOnlyModelPass and the Claude advice harness gained for
// the Now summary (output cap, thinking, external abort, usage callback), and proof that the
// existing callers are untouched by them.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import { readFileSync } from "node:fs";

import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";

import { runReadOnlyModelPass, type ReadOnlyModelPassOpts } from "../src/model-pass.js";
import { SummaryRunner } from "../src/summary-runner.js";
import { buildJudgePrompt } from "../src/judge-runner.js";
import type { AdviceUsageSnapshot } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";

function capturing(frames: unknown[]): { queryFn: SdkQueryFn; seen: { options?: SdkOptions } } {
  const seen: { options?: SdkOptions } = {};
  const queryFn = ((params: { options: SdkOptions }) => {
    seen.options = params.options;
    return (async function* () {
      for (const f of frames) yield f;
    })();
  }) as unknown as SdkQueryFn;
  return { queryFn, seen };
}

function baseOpts(over: Partial<ReadOnlyModelPassOpts> = {}): ReadOnlyModelPassOpts {
  return {
    token: "tok",
    systemPrompt: "sys",
    prompt: "hello",
    homeRoot: os.tmpdir(),
    homePrefix: "uzi-now-test-",
    label: "now",
    timeoutMs: 5000,
    graceMs: 20,
    queryFn: capturing([]).queryFn,
    denyReason: "read-only",
    log: nullLogger(),
    teardownTestDeps: portableTeardownTestDeps,
    ...over,
  };
}

const HAIKU = "claude-haiku-4-5-20251001";
const assistant = (id: string, usage: Record<string, unknown>, text = "ok", model = HAIKU) => ({
  type: "assistant",
  message: { id, model, role: "assistant", content: [{ type: "text", text }], usage },
});
const success = (modelUsage?: Record<string, unknown>) => ({ type: "result", subtype: "success", is_error: false, modelUsage });
const failure = (modelUsage?: Record<string, unknown>) => ({ type: "result", subtype: "error_during_execution", is_error: true, modelUsage });

function collect(): { calls: AdviceUsageSnapshot[]; onUsage(u: AdviceUsageSnapshot): void } {
  const calls: AdviceUsageSnapshot[] = [];
  return { calls, onUsage: (u) => void calls.push(u) };
}

describe("the output cap and thinking reach the Claude harness", () => {
  it("sets CLAUDE_CODE_MAX_OUTPUT_TOKENS and thinking:disabled when asked, leaving settingSources empty", async () => {
    const { queryFn, seen } = capturing([assistant("m1", { input_tokens: 1, output_tokens: 1 }), success()]);
    await runReadOnlyModelPass(baseOpts({ queryFn, maxOutputTokens: 256, thinking: "disabled" }));
    const env = seen.options!.env as Record<string, string | undefined>;
    assert.equal(env.CLAUDE_CODE_MAX_OUTPUT_TOKENS, "256");
    assert.deepEqual(seen.options!.thinking, { type: "disabled" });
    assert.deepEqual(seen.options!.settingSources, []);
    assert.equal(env.CLAUDE_CODE_OAUTH_TOKEN, "tok", "the allowlisted env is otherwise intact");
  });

  it("sets neither for a caller that does not ask", async () => {
    const { queryFn, seen } = capturing([success()]);
    await runReadOnlyModelPass(baseOpts({ queryFn, label: "summary" }));
    const env = seen.options!.env as Record<string, string | undefined>;
    assert.equal("CLAUDE_CODE_MAX_OUTPUT_TOKENS" in env, false);
    assert.equal("thinking" in seen.options!, false);
  });
});

describe("onUsage: exactly once with the latest non-empty snapshot", () => {
  it("success: the terminal modelUsage (with costUSD) replaces the message evidence", async () => {
    const u = collect();
    const { queryFn } = capturing([
      assistant("m1", { input_tokens: 10, output_tokens: 4, cache_read_input_tokens: 2 }),
      success({ [HAIKU]: { inputTokens: 11, outputTokens: 5, cacheReadInputTokens: 2, cacheCreationInputTokens: 0, costUSD: 0.0004 } }),
    ]);
    await runReadOnlyModelPass(baseOpts({ queryFn, onUsage: u.onUsage }));
    assert.equal(u.calls.length, 1);
    assert.deepEqual(u.calls[0], { [HAIKU]: { inputTokens: 11, outputTokens: 5, cacheReadInputTokens: 2, cacheCreationInputTokens: 0, costUSD: 0.0004 } });
  });

  it("error result: the spend of the failed call is still delivered, once, and the pass still throws", async () => {
    const u = collect();
    const { queryFn } = capturing([
      assistant("m1", { input_tokens: 9, output_tokens: 3 }),
      failure({ [HAIKU]: { inputTokens: 9, outputTokens: 3, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costUSD: 0.0002 } }),
    ]);
    await assert.rejects(runReadOnlyModelPass(baseOpts({ queryFn, onUsage: u.onUsage })), /returned an error result/);
    assert.equal(u.calls.length, 1);
    assert.equal(u.calls[0]![HAIKU]!.costUSD, 0.0002);
  });

  it("pre-terminal evidence: message.id dedup, per-column GREATEST, the 5m/1h split and the tier markers, no costUSD", async () => {
    const u = collect();
    const frames = [
      // The same message seen twice: a partial then the final usage. Counted once, per-column max.
      assistant("m1", { input_tokens: 10, output_tokens: 2, cache_creation_input_tokens: 100, cache_creation: { ephemeral_5m_input_tokens: 60, ephemeral_1h_input_tokens: 40 }, service_tier: "standard" }),
      assistant("m1", { input_tokens: 10, output_tokens: 7, cache_creation_input_tokens: 100, cache_creation: { ephemeral_5m_input_tokens: 60, ephemeral_1h_input_tokens: 40 }, speed: "standard", inference_geo: "us" }),
      assistant("m2", { input_tokens: 3, output_tokens: 1, cache_read_input_tokens: 50 }),
    ];
    const { queryFn } = capturing(frames);
    // No terminal frame: the stream just ends (clean EOF), as an aborted pass would.
    await runReadOnlyModelPass(baseOpts({ queryFn, onUsage: u.onUsage }));
    assert.equal(u.calls.length, 1);
    assert.deepEqual(u.calls[0], {
      [HAIKU]: {
        inputTokens: 13,
        outputTokens: 8,
        cacheReadInputTokens: 50,
        cacheCreationInputTokens: 100,
        cacheCreation5mInputTokens: 60,
        cacheCreation1hInputTokens: 40,
        service_tier: "standard",
        speed: "standard",
        inference_geo: "us",
      },
    });
    assert.equal("costUSD" in u.calls[0]![HAIKU]!, false);
  });

  it("an error result without modelUsage keeps the message evidence", async () => {
    const u = collect();
    const { queryFn } = capturing([assistant("m1", { input_tokens: 5, output_tokens: 1 }), failure()]);
    await assert.rejects(runReadOnlyModelPass(baseOpts({ queryFn, onUsage: u.onUsage })));
    assert.equal(u.calls.length, 1);
    assert.equal(u.calls[0]![HAIKU]!.inputTokens, 5);
  });

  it("not called when no usage evidence arrived", async () => {
    const u = collect();
    const { queryFn } = capturing([{ type: "assistant", message: { role: "assistant", content: [{ type: "text", text: "x" }] } }, success()]);
    await runReadOnlyModelPass(baseOpts({ queryFn, onUsage: u.onUsage }));
    assert.equal(u.calls.length, 0);
  });

  it("timeout: the evidence seen before the timeout is delivered once", async () => {
    const u = collect();
    const queryFn = (() =>
      (async function* () {
        yield assistant("m1", { input_tokens: 8, output_tokens: 2 });
        await new Promise(() => {});
      })()) as unknown as SdkQueryFn;
    await assert.rejects(runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 20, onUsage: u.onUsage })), /exceeded 20ms/);
    assert.equal(u.calls.length, 1);
    assert.equal(u.calls[0]![HAIKU]!.inputTokens, 8);
  });

  it("a throwing onUsage is swallowed", async () => {
    const { queryFn } = capturing([assistant("m1", { input_tokens: 1, output_tokens: 1 }), success()]);
    const text = await runReadOnlyModelPass(baseOpts({ queryFn, onUsage: () => { throw new Error("nope"); } }));
    assert.equal(text, "ok");
  });
});

describe("the external abort signal", () => {
  it("abort after a usage frame rejects at once, delivers the evidence once, and settles within the grace even when the query never does", async () => {
    const u = collect();
    const ctrl = new AbortController();
    let sawFirst!: () => void;
    const first = new Promise<void>((r) => (sawFirst = r));
    const queryFn = (() =>
      (async function* () {
        yield assistant("m1", { input_tokens: 42, output_tokens: 9 });
        sawFirst();
        await new Promise(() => {}); // never settles, even after abort
      })()) as unknown as SdkQueryFn;
    const started = Date.now();
    const pass = runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 30_000, signal: ctrl.signal, onUsage: u.onUsage }));
    const assertion = assert.rejects(pass, /model call aborted/);
    await first;
    ctrl.abort();
    await assertion;
    assert.ok(Date.now() - started < 2_000, "bounded by graceMs, never the 30s timeout");
    assert.equal(u.calls.length, 1);
    assert.equal(u.calls[0]![HAIKU]!.inputTokens, 42);
    assert.equal("costUSD" in u.calls[0]![HAIKU]!, false);
  });

  it("abort without evidence calls onUsage zero times", async () => {
    const u = collect();
    const ctrl = new AbortController();
    const queryFn = (() => (async function* () { await new Promise(() => {}); yield undefined; })()) as unknown as SdkQueryFn;
    const pass = runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 30_000, signal: ctrl.signal, onUsage: u.onUsage }));
    const assertion = assert.rejects(pass, /aborted/);
    ctrl.abort();
    await assertion;
    assert.equal(u.calls.length, 0);
  });

  it("an already-aborted signal rejects without waiting for anything", async () => {
    const ctrl = new AbortController();
    ctrl.abort();
    const queryFn = (() => (async function* () { await new Promise(() => {}); yield undefined; })()) as unknown as SdkQueryFn;
    await assert.rejects(runReadOnlyModelPass(baseOpts({ queryFn, timeoutMs: 30_000, signal: ctrl.signal })), /aborted/);
  });
});

describe("the Codex advice path: bounded settle and usage", () => {
  const codex = (run: (signal: AbortSignal) => Promise<{ text: string }>) =>
    ({
      runId: "r",
      binding: {} as never,
      buildHarness: async ({ signal }: { signal: AbortSignal }) => ({
        kind: "codex",
        run: async (req: { usageObserver?: (u: AdviceUsageSnapshot) => void }) => {
          req.usageObserver?.({ "gpt-6-luna": { inputTokens: 3, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } });
          return run(signal);
        },
      }),
    }) as unknown as NonNullable<ReadOnlyModelPassOpts["codex"]>;

  it("abort rejects at once and returns within the grace for a harness that never settles; usage delivered once", async () => {
    const u = collect();
    const ctrl = new AbortController();
    const started = Date.now();
    const pass = runReadOnlyModelPass(
      baseOpts({ codex: codex(() => new Promise(() => {})), token: undefined, timeoutMs: 30_000, signal: ctrl.signal, onUsage: u.onUsage }),
    );
    const assertion = assert.rejects(pass, /aborted/);
    await new Promise((r) => setTimeout(r, 10));
    ctrl.abort();
    await assertion;
    assert.ok(Date.now() - started < 2_000);
    assert.equal(u.calls.length, 1);
    assert.equal(u.calls[0]!["gpt-6-luna"]!.inputTokens, 3);
  });
});

describe("existing callers are unchanged", () => {
  it("SummaryRunner sets no cap, no thinking and passes no signal or usage callback", async () => {
    const seen: { options?: SdkOptions } = {};
    const queryFn = ((params: { options: SdkOptions }) => {
      seen.options = params.options;
      return (async function* () {
        yield assistant("m1", { input_tokens: 1, output_tokens: 1 }, "This run adds summaries.");
        yield success();
      })();
    }) as unknown as SdkQueryFn;
    const runner = new SummaryRunner(nullLogger(), { queryFn, homeRoot: os.tmpdir(), modelTimeoutMs: 2000 });
    const out = await runner.generateIntentSummary({ token: "t", model: "haiku", issueTitle: "t", issueBody: "b", prdText: "p" });
    assert.equal(out, "This run adds summaries.");
    const env = seen.options!.env as Record<string, string | undefined>;
    assert.equal("CLAUDE_CODE_MAX_OUTPUT_TOKENS" in env, false);
    assert.equal("thinking" in seen.options!, false);
  });

  it("none of the four callers names a new option", () => {
    for (const f of ["judge-runner.ts", "review-runner.ts", "summary-runner.ts", "pr-description-eval-cli.ts"]) {
      const src = readFileSync(new URL(`../src/${f}`, import.meta.url), "utf8");
      for (const needle of ["maxOutputTokens", "onUsage", "usageObserver", "thinking:"]) {
        assert.ok(!src.includes(needle), `${f} must not pass ${needle}`);
      }
    }
  });

  it("the judge's trace sample skips progress_note messages", () => {
    const trace = {
      target: { id: "r1", kind: "issue", status: "completed" },
      inputs: [],
      messages: [
        { seq: 1, kind: "text", agent: "lead", payload: { text: "real work" } },
        { seq: 2, kind: "progress_note", agent: "worker", payload: { text: "SUMMARY-LINE-XYZ", milestone_id: "m1" } },
      ],
    } as unknown as Parameters<typeof buildJudgePrompt>[0];
    const prompt = buildJudgePrompt(trace, null);
    assert.ok(prompt.includes("real work"));
    assert.ok(!prompt.includes("SUMMARY-LINE-XYZ"));
  });
});
