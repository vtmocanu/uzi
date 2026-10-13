// Issue #2833 M1: intent/plan select Codex by claim shape and never fall back.
import { before, describe, it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import path from "node:path";
import { SummaryRunner, type IntentSummaryInput } from "../src/summary-runner.js";
import type { CodexAdviceHarnessFactory } from "../src/codex/codex-executor.js";
import type { AdviceRequest, AdviceResultPolicy, HarnessTerminal } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

const base = { token: "usable-claude", model: "sonnet", issueTitle: "Add reports", issueBody: "Report results", prdText: "PRD" };
const bindings = [
  { auth_mode: "api_key", access_token: "test-key", capability: "test-cap" },
  { auth_mode: "subscription", access_token: "test-subscription", capability: "test-cap", generation: 7, chatgpt_account_id: "account-test", chatgpt_plan_type: null },
];
const homeRoot = path.resolve("../.uzi/scratch");
before(async () => {
  await fs.mkdir(homeRoot, { recursive: true });
});
const success: HarnessTerminal = { outcome: "success", subtype: "success", errors: [], metrics: { cost: { kind: "unreported" } } };

for (const pass of ["intent", "plan"] as const) {
  const run = (r: SummaryRunner, input: IntentSummaryInput) => pass === "intent"
    ? r.generateIntentSummary(input)
    : r.generatePlanSummary({ ...input, planMd: "# Proposed plan" });
  const text = pass === "intent" ? "Adds reports." : JSON.stringify({ summary: "Adds reports.", deltas: [{ kind: "added", text: "Reports" }] });

  describe(`${pass}: Codex selection`, () => {
    for (const binding of bindings) {
      it(`${binding.auth_mode}: pins model/effort, denies refresh, and preserves prompts/output`, async () => {
        let builds = 0;
        let calls = 0;
        let claudeCalls = 0;
        const factory = (async (params) => {
          builds++;
          assert.equal(params.runId, "run-summary");
          assert.equal(params.binding.authMode, binding.auth_mode);
          assert.equal(params.binding.accessToken, binding.access_token);
          assert.equal(params.binding.capability, binding.capability);
          if (params.binding.authMode === "subscription") assert.equal(params.binding.generation, 7);
          assert.equal(params.refresh, "deny");
          assert.ok(params.signal);
          assert.equal(params.signal.aborted, false);
          return { kind: "codex", run: async (req: AdviceRequest, policy: AdviceResultPolicy) => {
            calls++;
            assert.equal(req.model, "gpt-6-luna");
            assert.equal(req.effort, "low");
            assert.equal(req.output.kind, "text");
            assert.equal(req.timeoutMs, 1000);
            assert.match(req.systemPrompt, /UNTRUSTED DATA/);
            assert.match(req.prompt, /Add reports/);
            assert.match(req.prompt, /PRD/);
            if (pass === "plan") assert.match(req.prompt, /Proposed plan/);
            policy.onTerminal(success, { isError: false, latest: undefined });
            return { text, end: { kind: "terminal", terminal: success } };
          } };
        }) as CodexAdviceHarnessFactory;
        const queryFn = (() => { claudeCalls++; throw new Error("unexpected Claude"); }) as unknown as SdkQueryFn;
        const r = new SummaryRunner(nullLogger(), { homeRoot, modelTimeoutMs: 1000, queryFn, codexAdviceHarnessFactory: factory });
        const out = await run(r, { ...base, claim: { run_id: "run-summary", secrets: { codex: binding } } });
        assert.deepEqual(out, pass === "intent" ? "Adds reports." : { summary: "Adds reports.", deltas: [{ kind: "added", text: "Reports" }] });
        assert.equal(builds, 1);
        assert.equal(calls, 1);
        assert.equal(claudeCalls, 0);
      });
    }

    for (const [name, codex] of [
      ["null", null], ["primitive", "bad"], ["empty", {}],
      ["partial API key", { auth_mode: "api_key", access_token: "test-key" }],
      ["partial subscription", { auth_mode: "subscription", access_token: "test-subscription", capability: "test-cap" }],
    ] as const) {
      it(`${name} binding returns null/warn even with usable Claude`, async () => {
        let calls = 0;
        const warns: string[] = [];
        const queryFn = (() => { calls++; throw new Error("unexpected Claude"); }) as unknown as SdkQueryFn;
        const factory = (async () => { calls++; throw new Error("unexpected factory"); }) as CodexAdviceHarnessFactory;
        const r = new SummaryRunner({ ...nullLogger(), warn: (message) => void warns.push(message) }, { homeRoot, queryFn, codexAdviceHarnessFactory: factory });
        assert.equal(await run(r, { ...base, claim: { run_id: "run-summary", secrets: { codex } } }), null);
        assert.equal(calls, 0);
        assert.ok(warns.includes(`${pass} summary generation failed`));
      });
    }

    for (const failure of ["missing factory", "release failure", "refusal", "error terminal", "empty output"] as const) {
      it(`${failure} is advisory with no Claude or alternate model fallback`, async () => {
        let builds = 0;
        let calls = 0;
        let claudeCalls = 0;
        const queryFn = (() => { claudeCalls++; throw new Error("unexpected Claude"); }) as unknown as SdkQueryFn;
        const factory: CodexAdviceHarnessFactory = (async () => {
          builds++;
          if (failure === "release failure") throw new Error("credential release failed");
          return { kind: "codex", run: async (req: AdviceRequest, policy: AdviceResultPolicy) => {
            calls++;
            assert.equal(req.model, "gpt-6-luna");
            assert.equal(req.effort, "low");
            if (failure === "refusal" || failure === "error terminal") {
              const terminal: HarnessTerminal = {
                ...success, outcome: "failed", subtype: "error_during_execution",
                errors: [failure === "refusal" ? "provider refused" : "provider failed"],
                ...(failure === "refusal" ? { policyRefusal: {
                  event: "provider_policy_refusal", provider: "codex", category: "policy_refusal",
                  policy_tag: "cyberPolicy", phase: "planning", origin: "root",
                  correlation_id: "pr-" + "a".repeat(32) + "-1",
                } as const } : {}),
              };
              // Exercise the model pass's injected terminal policy before returning text.
              policy.onTerminal(terminal, { isError: true, latest: undefined });
              return { text, end: { kind: "terminal", terminal } };
            }
            return { text: " ", end: { kind: "terminal", terminal: success } };
          } };
        }) as unknown as CodexAdviceHarnessFactory;
        const r = new SummaryRunner(nullLogger(), { homeRoot, queryFn, codexAdviceHarnessFactory: failure === "missing factory" ? undefined : factory });
        assert.equal(await run(r, { ...base, claim: { run_id: "run-summary", secrets: { codex: bindings[0] } } }), null);
        assert.equal(claudeCalls, 0);
        assert.equal(builds, failure === "missing factory" ? 0 : 1);
        assert.equal(calls, failure === "missing factory" || failure === "release failure" ? 0 : 1);
      });
    }

    for (const claim of [undefined, { run_id: "run-summary", secrets: {} }, { run_id: "run-summary", secrets: { codex: undefined } }]) {
      it(`absent Codex uses the legacy token/model (claim ${claim === undefined ? "omitted" : "present"})`, async () => {
        let calls = 0;
        const queryFn = async function* (args: { options: { model?: string; env?: Record<string, string>; effort?: string } }) {
          calls++;
          assert.equal(args.options.model, "sonnet");
          assert.equal(args.options.effort, undefined);
          assert.ok(Object.values(args.options.env ?? {}).includes(base.token));
          yield { type: "assistant", message: { content: [{ type: "text", text }] } };
          yield { type: "result", subtype: "success", is_error: false };
        } as unknown as SdkQueryFn;
        const factory = (async () => { assert.fail("Codex must not be built"); }) as CodexAdviceHarnessFactory;
        const r = new SummaryRunner(nullLogger(), { homeRoot, queryFn, codexAdviceHarnessFactory: factory });
        assert.ok(await run(r, { ...base, claim }));
        assert.equal(calls, 1);
      });
    }
  });
}
