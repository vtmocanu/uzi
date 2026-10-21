import assert from "node:assert/strict";
import path from "node:path";
import { mkdir } from "node:fs/promises";
import { createHash } from "node:crypto";
import type { BoundedCoverageTestTransport } from "../src/plan-contract-review.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { SemanticPlanning } from "../src/semantic-plan.js";
import { CodexAdviceHarness } from "../src/codex/codex-advice-harness.js";
import { selectCodexBinding } from "../src/codex/select.js";
import type { CodexTransport, CodexNotification } from "../src/codex/transport.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { nullLogger } from "./helpers.js";

export const enrollment: SemanticPlanning = { profile: "semantic", policy: {
  version: 1, model: "sonnet", max_reviews: 3, max_invocations: 2, max_tokens: 120000, max_seconds: 900,
} };
export const hash = (s: string) => createHash("sha256").update(s).digest("hex");
export const criterionId = "00000000-0000-4000-8000-000000000001";
export function proposal(text = "Implement A") {
  return { criteria: [{ milestone_id: "m1", kind: "code" as const, text: "A", done_when: "Verified A",
    citations: [{ source_id: "issue:1", hash: hash(text), start: 0, end: Buffer.byteLength(text) }],
    evidence_requirements: [{ description: "Tests" }] }] };
}

/** Finite in-memory frames only; no network, tools or provider retries. */
export async function boundedTransport(harness: "claude" | "codex", report: () => unknown, events: string[]) {
  const homeRoot = path.resolve("../.uzi/scratch");
  await mkdir(homeRoot, { recursive: true });
  const transport: BoundedCoverageTestTransport = {
    kind: "bounded-test", harness,
    inputUpperBound: () => 1000,
    constrain: ({ tokens, deadlineMs }) => { assert.ok(tokens <= 40000); assert.ok(deadlineMs > Date.now()); },
    usageComplete: () => true, isSafetyRefusal: () => false,
    pass: { token: "fixture", homeRoot, log: nullLogger(), teardownTestDeps: portableTeardownTestDeps,
      queryFn: ((params) => (async function* () {
        events.push("invoke");
        assert.equal(params.options.model, "sonnet");
        assert.deepEqual(params.options.settingSources, []);
        assert.equal(params.options.tools, undefined);
        assert.equal(params.options.mcpServers, undefined);
        for await (const _frame of params.prompt) { /* one finite prompt */ }
        yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: JSON.stringify(report()) }] } };
        yield { type: "result", subtype: "success", is_error: false, modelUsage: { sonnet: {
          inputTokens: 100, outputTokens: 10, cacheReadInputTokens: 0, cacheCreationInputTokens: 0,
        } } };
      })()) as SdkQueryFn },
  };
  if (harness === "codex") {
    const selected = selectCodexBinding({ codex: { auth_mode: "api_key", access_token: "fixture", capability: "fixture" } });
    assert.equal(selected.kind, "codex");
    if (selected.kind !== "codex") throw new Error("fixture selection failed");
    transport.pass.codex = { binding: selected.binding, runId: "fixture", buildHarness: async () => new CodexAdviceHarness({
      log: nullLogger(),
      provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
      launchRoot: async () => {
        let text = "";
        const wire: CodexTransport = {
          request: async <T>(method: string, params?: unknown) => {
            if (method === "thread/start") {
              const p = params as { dynamicTools: unknown[]; environments: unknown[] };
              assert.deepEqual(p.dynamicTools, []); assert.deepEqual(p.environments, []);
              return { thread: { id: "th" } } as T;
            }
            if (method === "turn/start") { events.push("invoke"); text = JSON.stringify(report()); return { turn: { id: "tn" } } as T; }
            return {} as T;
          },
          notify() {}, respond() { throw new Error("No tools"); }, close: async () => {},
          async *notifications() {
            const usage = { inputTokens: 100, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 10, reasoningOutputTokens: 0, totalTokens: 110 };
            const notes: CodexNotification[] = [
              { kind: "thread_started", method: "thread/started", threadId: "th", params: { thread: { id: "th" } } },
              { kind: "token_usage_updated", method: "thread/tokenUsage/updated", threadId: "th", turnId: "tn",
                usage: { total: usage, last: usage, pricingEvidenceComplete: true }, params: { threadId: "th", turnId: "tn" } },
              { kind: "activity", method: "item/completed", params: { threadId: "th", item: { type: "agentMessage", text } } },
              { kind: "turn_completed", method: "turn/completed", threadId: "th", turnId: "tn", status: "completed",
                params: { threadId: "th", turn: { id: "tn", status: "completed" } } },
            ];
            yield* notes;
          },
        };
        return { transport: wire, cwd: "/isolated/coverage", dispose: async () => {} };
      },
    }) };
  }
  return transport;
}
