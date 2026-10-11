// Issue #2686: the SummaryRunner reports each pass's observed model usage through `onUsage`
// (at most once, only when usage evidence arrived), for the intent, plan and PR-description
// passes on Claude and the PR-description pass on Codex.

import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";

import { SummaryRunner, type DeliverySummaryClaimView, type DeliverySummaryInput } from "../src/summary-runner.js";
import { buildDeliveryContext, type DeliveryContext } from "../src/pr-description-context.js";
import { makeTextRedactor } from "../src/redact.js";
import type { AdviceUsageSnapshot } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { Logger } from "../src/log.js";
import { nullLogger } from "./helpers.js";

const HAIKU = "claude-haiku-4-5-20251001";
const CODEX_MODEL = "gpt-6-luna";

type Scenario = "success" | "error" | "timeout" | "unparseable" | "no_usage";
const SCENARIOS: Scenario[] = ["success", "error", "timeout", "unparseable", "no_usage"];

const homeRoots: string[] = [];
after(async () => {
  await Promise.all(homeRoots.map((d) => fs.rm(d, { recursive: true, force: true })));
});

function logs(): { log: Logger; warns: { message: string; fields?: Record<string, unknown> }[] } {
  const warns: { message: string; fields?: Record<string, unknown> }[] = [];
  const log = { ...nullLogger(), warn: (message: string, fields?: Record<string, unknown>) => void warns.push({ message, fields }) };
  return { log, warns };
}

/** Text each pass can parse as a success; an unparseable run replaces it with garbage. */
function textFor(pass: string, scenario: Scenario): string {
  if (scenario === "unparseable") return pass === "intent" ? "" : "{ not json";
  if (pass === "intent") return "This run adds usage reporting.";
  if (pass === "plan") return JSON.stringify({ summary: "Plan.", deltas: [] });
  return JSON.stringify({ summary: "Delivery.", changes: ["c"], scope_notes: [], review_pointers: [] });
}

function claudeQuery(pass: string, scenario: Scenario): SdkQueryFn {
  return async function* () {
    const withUsage = scenario !== "no_usage";
    yield {
      type: "assistant",
      message: {
        id: "m1",
        model: HAIKU,
        role: "assistant",
        content: [{ type: "text", text: textFor(pass, scenario) }],
        ...(withUsage ? { usage: { input_tokens: 11, output_tokens: 5 } } : {}),
      },
    };
    if (scenario === "timeout") await new Promise(() => {});
    if (scenario === "error") {
      yield { type: "result", subtype: "error_during_execution", is_error: true };
      return;
    }
    yield { type: "result", subtype: "success", is_error: false };
  } as unknown as SdkQueryFn;
}

function codexFactory(pass: string, scenario: Scenario) {
  return (async () => ({
    kind: "codex" as const,
    run: async (req: { usageObserver?: (u: AdviceUsageSnapshot) => void }) => {
      if (scenario !== "no_usage") {
        req.usageObserver?.({ [CODEX_MODEL]: { inputTokens: 3, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } });
      }
      if (scenario === "timeout") await new Promise(() => {});
      if (scenario === "error") throw new Error("codex failed");
      return { text: textFor(pass, scenario), end: { kind: "terminal", terminal: { outcome: "success" } } };
    },
  })) as never;
}

async function newRunner(opts: { queryFn?: SdkQueryFn; codex?: unknown; log: Logger }): Promise<SummaryRunner> {
  const homeRoot = await fs.mkdtemp(path.join(os.tmpdir(), "summary-usage-test-"));
  homeRoots.push(homeRoot);
  return new SummaryRunner(opts.log, {
    queryFn: opts.queryFn,
    homeRoot,
    modelTimeoutMs: 80,
    codexAdviceHarnessFactory: opts.codex as never,
  });
}

const claudeClaim: DeliverySummaryClaimView = { run_id: "run-1", summary_model: "haiku", secrets: { anthropic_oauth_token: "tok-abc" } };
const codexClaim: DeliverySummaryClaimView = {
  run_id: "run-1",
  secrets: { codex: { auth_mode: "api_key", access_token: "codex-tok", capability: "cap-1" } },
};

async function context(): Promise<DeliveryContext> {
  return buildDeliveryContext({
    git: {
      sizeMergeBase: async () => "b".repeat(40),
      diffNumstatZ: async () => "1\t0\tsrc/x.ts\0",
      checkAttrZ: async (_b, _h, paths) => new Map(paths.map((p) => [p, {}])),
      readBare: async (_b, args) => ({ text: args[0] === "log" ? "feat: add x\0" : "diff --git a/src/x.ts b/src/x.ts\n+x\n", truncated: false }),
    },
    barePath: "/bare",
    targetBranch: "main",
    headSha: "a".repeat(40),
    deadlineMs: Date.now() + 60_000,
    redact: makeTextRedactor([]),
    issueTitle: "Add x",
    issueBody: "Please add x.",
    prSummary: { what: "Adds x" },
  });
}

const base = { token: "t", model: "haiku", issueTitle: "t", issueBody: "b", prdText: "p" };

interface Case {
  name: string;
  pass: "intent" | "plan" | "pr_description";
  model: string;
  codex: boolean;
  run(r: SummaryRunner, onUsage: (u: AdviceUsageSnapshot) => void): Promise<unknown>;
}

const CASES: Case[] = [
  { name: "intent (Claude)", pass: "intent", model: HAIKU, codex: false, run: (r, onUsage) => r.generateIntentSummary({ ...base, onUsage }) },
  { name: "plan (Claude)", pass: "plan", model: HAIKU, codex: false, run: (r, onUsage) => r.generatePlanSummary({ ...base, planMd: "# plan", onUsage }) },
  {
    name: "pr_description (Claude)",
    pass: "pr_description",
    model: HAIKU,
    codex: false,
    run: async (r, onUsage) => r.generateDeliverySummary(await deliveryInput(claudeClaim, onUsage)),
  },
  {
    name: "pr_description (Codex)",
    pass: "pr_description",
    model: CODEX_MODEL,
    codex: true,
    run: async (r, onUsage) => r.generateDeliverySummary(await deliveryInput(codexClaim, onUsage)),
  },
];

async function deliveryInput(claim: DeliverySummaryClaimView, onUsage: (u: AdviceUsageSnapshot) => void): Promise<DeliverySummaryInput> {
  return { claim, context: await context(), deadlineMs: Date.now() + 60_000, onUsage };
}

for (const c of CASES) {
  describe(`${c.name}: usage reaches onUsage`, () => {
    for (const scenario of SCENARIOS) {
      it(`${scenario}`, async () => {
        const calls: AdviceUsageSnapshot[] = [];
        const { log, warns } = logs();
        const r = await newRunner(c.codex ? { codex: codexFactory(c.pass, scenario), log } : { queryFn: claudeQuery(c.pass, scenario), log });
        await c.run(r, (u) => void calls.push(u));
        const unavailable = warns.filter((w) => w.message === "summary pass usage unavailable");
        if (scenario === "no_usage") {
          assert.equal(calls.length, 0, "no usage observed: no callback, no zero row");
          assert.equal(unavailable.length, 1);
          assert.equal(unavailable[0]!.fields?.pass, c.pass);
        } else {
          assert.equal(calls.length, 1, "exactly once");
          assert.ok(c.model in calls[0]!, `keyed by the provider model id ${c.model}`);
          assert.ok(calls[0]![c.model]!.inputTokens > 0);
          assert.equal(unavailable.length, 0);
        }
      });
    }
  });
}

for (const pass of ["intent", "plan"] as const) {
  describe(`${pass}: an external abort`, () => {
    it("rejects the pass at once and delivers the observed usage once", async () => {
      const calls: AdviceUsageSnapshot[] = [];
      let seen!: () => void;
      const first = new Promise<void>((resolve) => (seen = resolve));
      const queryFn = async function* () {
        yield { type: "assistant", message: { id: "m1", model: HAIKU, role: "assistant", content: [{ type: "text", text: "x" }], usage: { input_tokens: 7, output_tokens: 1 } } };
        seen();
        await new Promise(() => {});
      } as unknown as SdkQueryFn;
      const homeRoot = await fs.mkdtemp(path.join(os.tmpdir(), "summary-usage-test-"));
      homeRoots.push(homeRoot);
      const r = new SummaryRunner(nullLogger(), { queryFn, homeRoot, modelTimeoutMs: 600_000 });
      const ctrl = new AbortController();
      const onUsage = (u: AdviceUsageSnapshot) => void calls.push(u);
      const started = Date.now();
      const out = pass === "intent"
        ? r.generateIntentSummary({ ...base, onUsage, signal: ctrl.signal })
        : r.generatePlanSummary({ ...base, planMd: "# p", onUsage, signal: ctrl.signal });
      await first;
      ctrl.abort();
      assert.equal(await out, null);
      assert.ok(Date.now() - started < 5_000, "bounded by the abort grace, never the generation timeout");
      assert.equal(calls.length, 1);
      assert.equal(calls[0]![HAIKU]!.inputTokens, 7);
    });
  });
}

describe("a delivery call whose deadline is spent", () => {
  it("makes no model call and reports no usage", async () => {
    const calls: AdviceUsageSnapshot[] = [];
    let modelCalls = 0;
    const queryFn = async function* () {
      modelCalls++;
      yield { type: "result", subtype: "success", is_error: false };
    } as unknown as SdkQueryFn;
    const r = await newRunner({ queryFn, log: nullLogger() });
    const input = await deliveryInput(claudeClaim, (u) => void calls.push(u));
    assert.equal(await r.generateDeliverySummary({ ...input, deadlineMs: Date.now() - 1 }), null);
    assert.equal(modelCalls, 0);
    assert.equal(calls.length, 0);
  });
});
