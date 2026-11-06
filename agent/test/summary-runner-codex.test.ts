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
import { testOwnedPassHomeRoot } from "./teardown-fixtures.js";
import { DEFAULT_CODEX_SUMMARY_MODEL } from "../src/codex/summary-model.js";
import { isCodexRoleModel } from "../src/codex/render.js";

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

// Issue #3248: the Codex lane model is the claim's `model` when it is a Codex role model, else the built-in fallback.
describe("Codex summary model selection (#3248)", () => {
  const root = testOwnedPassHomeRoot();
  for (const pass of ["intent", "plan"] as const) {
    for (const [label, model, want] of [
      ["absent", "", "gpt-6-luna"],
      ["configured member", "gpt-6-astra", "gpt-6-astra"],
      ["Claude lane value from an older server", "haiku", "gpt-6-luna"],
      ["unknown id", "gpt-7-unknown", "gpt-6-luna"],
    ] as const) {
      it(`${pass}: ${label} model resolves to ${want} at effort low, refresh deny, no Claude`, async () => {
        const seen: { model?: string; effort?: string; refresh?: string }[] = [];
        let claudeCalls = 0;
        const factory = (async (params) => ({ kind: "codex", run: async (req: AdviceRequest, policy: AdviceResultPolicy) => {
          seen.push({ model: req.model, effort: req.effort, refresh: params.refresh });
          policy.onTerminal(success, { isError: false, latest: undefined });
          return { text: pass === "intent" ? "Adds reports." : JSON.stringify({ summary: "Adds reports.", deltas: [] }), end: { kind: "terminal", terminal: success } };
        } })) as CodexAdviceHarnessFactory;
        const queryFn = (() => { claudeCalls++; throw new Error("unexpected Claude"); }) as unknown as SdkQueryFn;
        const r = new SummaryRunner(nullLogger(), { homeRoot: root, modelTimeoutMs: 1000, queryFn, codexAdviceHarnessFactory: factory });
        const input: IntentSummaryInput = { ...base, model, claim: { run_id: "run-summary", secrets: { codex: bindings[0] } } };
        assert.ok(pass === "intent" ? await r.generateIntentSummary(input) : await r.generatePlanSummary({ ...input, planMd: "# Plan" }));
        assert.deepEqual(seen, [{ model: want, effort: "low", refresh: "deny" }]);
        assert.equal(claudeCalls, 0);
      });
    }
  }
});

describe("summary fixture pin (#3248)", () => {
  it("the Codex fallback equals the fixture built-in and is a Codex role model", async () => {
    const fx = JSON.parse(await fs.readFile(new URL("../../fixtures/model-settings/summary.json", import.meta.url), "utf8")) as { builtins: { codex: string } };
    assert.equal(fx.builtins.codex, DEFAULT_CODEX_SUMMARY_MODEL);
    assert.equal(isCodexRoleModel(DEFAULT_CODEX_SUMMARY_MODEL), true);
  });
});

// Issue #3248 M3: stub-harness cases for the skip classification that the real advice harness
// cannot produce (an `authorization` terminal) or cannot order deterministically (deny then timeout).
describe("Codex summary skip reasons, stub harness (#3248 M3)", () => {
  const root = testOwnedPassHomeRoot();
  const claim = { run_id: "run-summary", secrets: { codex: bindings[0] } };
  const failedTerminal = (category: string): HarnessTerminal => ({
    ...success, outcome: "failed", subtype: "error_during_execution", errors: ["provider failed"],
    failure: { materialize: () => ({ failure: { category, message: "x" }, original: new Error("x") }) } as never,
  });
  type Harness = (params: { onRefreshDenied?: () => void }, req: AdviceRequest, policy: AdviceResultPolicy, ctl: AbortController) => Promise<{ text: string; end: unknown }>;
  async function drive(pass: "intent" | "plan", harness: Harness | "missing" | "malformed", timeoutMs = 1000) {
    const ctl = new AbortController();
    let claude = 0;
    let builds = 0;
    const factory = (async (params: { onRefreshDenied?: () => void }) => {
      builds++;
      return { kind: "codex", run: (req: AdviceRequest, policy: AdviceResultPolicy) => (harness as Harness)(params, req, policy, ctl) };
    }) as unknown as CodexAdviceHarnessFactory;
    const r = new SummaryRunner(nullLogger(), {
      homeRoot: root, modelTimeoutMs: timeoutMs,
      queryFn: (() => { claude++; throw new Error("unexpected Claude"); }) as unknown as SdkQueryFn,
      codexAdviceHarnessFactory: harness === "missing" ? undefined : factory,
    });
    const reasons: string[] = [];
    const input: IntentSummaryInput = {
      ...base, signal: ctl.signal, onSkip: (reason) => { reasons.push(reason); },
      claim: { run_id: "run-summary", secrets: { codex: harness === "malformed" ? { auth_mode: "subscription" } : claim.secrets.codex } },
    };
    const out = pass === "intent" ? await r.generateIntentSummary(input) : await r.generatePlanSummary({ ...input, planMd: "# Plan" });
    return { out, reasons, claude, builds };
  }

  for (const pass of ["intent", "plan"] as const) {
    describe(pass, () => {
      it("an authorization error terminal stays silent", async () => {
        const r = await drive(pass, async (_p, _req, policy) => {
          const t = failedTerminal("authorization");
          policy.onTerminal(t, { isError: true, latest: undefined });
          return { text: "", end: { kind: "terminal", terminal: t } };
        });
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude, builds: r.builds }, { out: null, reasons: [], claude: 0, builds: 1 });
      });
      it("an authentication error terminal reports credential_rejected through the pass policy", async () => {
        const r = await drive(pass, async (_p, _req, policy) => {
          const t = failedTerminal("authentication");
          policy.onTerminal(t, { isError: true, latest: undefined });
          return { text: "", end: { kind: "terminal", terminal: t } };
        });
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude }, { out: null, reasons: ["credential_rejected"], claude: 0 });
      });
      it("a denied refresh then a model-pass timeout stays silent", async () => {
        const r = await drive(pass, (p, req) => {
          p.onRefreshDenied?.();
          return new Promise((_, reject) => setTimeout(() => reject(new Error(`summary model call exceeded ${req.timeoutMs}ms`)), req.timeoutMs));
        }, 80);
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude }, { out: null, reasons: [], claude: 0 });
      });
      it("a denied refresh then an abort stays silent", async () => {
        const r = await drive(pass, async (p, _req, _policy, ctl) => {
          p.onRefreshDenied?.();
          ctl.abort();
          throw new Error("summary model call aborted");
        });
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude }, { out: null, reasons: [], claude: 0 });
      });
      it("a denied refresh then a thrown failure reports refresh_denied", async () => {
        const r = await drive(pass, async (p) => {
          p.onRefreshDenied?.();
          throw new Error("codex advice transport failed");
        });
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude }, { out: null, reasons: ["refresh_denied"], claude: 0 });
      });
      it("a missing factory stays silent", async () => {
        const r = await drive(pass, "missing");
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude, builds: r.builds }, { out: null, reasons: [], claude: 0, builds: 0 });
      });
      it("a malformed codex binding stays silent", async () => {
        const r = await drive(pass, "malformed");
        assert.deepEqual({ out: r.out, reasons: r.reasons, claude: r.claude, builds: r.builds }, { out: null, reasons: [], claude: 0, builds: 0 });
      });
    });
  }
});
