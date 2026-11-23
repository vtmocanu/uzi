import { it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { SummaryRunner } from "../src/summary-runner.js";
import { ReviewRunner } from "../src/review-runner.js";
import type { ClaimProviderBinding, ClaimResponse, StateRequest } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import type { GitCache } from "../src/git.js";
import type { DeliveryContext } from "../src/pr-description-context.js";
import { nullLogger } from "./helpers.js";
import type { CodexAdviceHarnessFactory } from "../src/codex/codex-executor.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
async function makeHome(prefix: string): Promise<string> {
  await fs.mkdir(scratch, { recursive: true });
  return fs.mkdtemp(path.join(scratch, prefix));
}

// Keep prompt construction valid so removing the delivery guard reaches dispatch.
const deliveryContext: DeliveryContext = {
  issue: "Title\nBody", prd: null, plan: null, previous: null, claims: null,
  commits: "A change", paths: "src/example.ts", diff: "+change", bytes: 39,
  truncated: { issue: false, prd: false, plan: false, previous: false,
    claims: false, commits: false, paths: false, diff: false },
};

const claudeProvider: ClaimProviderBinding = {
  provider_id: "provider-test", revision: 1, engine: "claude", protocol: "anthropic_messages",
  base_url: "https://gateway.example.test", auth_style: "bearer", credential: "test-provider-key",
  model_map: { haiku: { model: "gateway-haiku" }, sonnet: { model: "gateway-sonnet" } },
};
function reply(text: string): SdkQueryFn {
  return async function* () {
    yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text }] } };
    yield { type: "result", subtype: "success", is_error: false };
  } as unknown as SdkQueryFn;
}
for (const pass of ["intent", "plan", "review"] as const) {
  it(`${pass} preserves supported Claude provider without vendor credentials`, async () => {
    const root = await makeHome("m2-supported-");
    const text = JSON.stringify({ summary: "Supported.", deltas: [], findings: [] });
    try {
      if (pass === "review") {
        let posted = false;
        const client = { reportState: async () => {}, postTaskReview: async (_id: string, review: { status: string }) => {
          assert.equal(review.status, "complete"); posted = true;
        } } as unknown as WorkerClient;
        const git = { ensureClone: async () => "/bare", reviewDiff: async () => "+change" } as unknown as GitCache;
        await new ReviewRunner(client, git, nullLogger(), { homeRoot: root, queryFn: reply(text) }).execute({
          run_id: "supported-review", review_target_run_id: "target", branch: "branch", base_branch: "main",
          repo: { clone_url: "/origin" }, task_review_model: "haiku",
          secrets: { forge_pat: "test-pat", provider: claudeProvider },
        } as unknown as ClaimResponse);
        assert.ok(posted);
      } else {
        const runner = new SummaryRunner(nullLogger(), { homeRoot: root, queryFn: reply(text) });
        const input = { token: "", provider: claudeProvider, model: "haiku", issueTitle: "Title", issueBody: "Body" };
        assert.ok(pass === "intent" ? await runner.generateIntentSummary(input)
          : await runner.generatePlanSummary({ ...input, planMd: "Plan" }));
      }
    } finally { await fs.rm(root, { recursive: true, force: true }); }
  });
}

for (const pass of ["intent", "plan", "delivery", "review"] as const) {
  it(`${pass} executes a frozen Codex provider authority without vendor work`, async () => {
    const root = await makeHome("m2-codex-positive-");
    const provider: ClaimProviderBinding = {
      provider_id: "codex-gateway", revision: 4, engine: "codex", protocol: "openai_responses",
      base_url: "https://gateway.example.test", auth_style: "bearer", credential: "provider/secret-key",
      model_map: { default: { model: "gateway-default" } },
    };
    let builds = 0;
    let turns = 0;
    let posted = false;
    const warnings: unknown[] = [];
    const log = { ...nullLogger(), warn: (message: string, fields?: Record<string, unknown>) => { warnings.push([message, fields]); } };
    const factory = (async (params: Parameters<CodexAdviceHarnessFactory>[0]) => {
      builds++;
      assert.equal(params.binding.authMode, "provider");
      assert.ok(params.binding.authMode === "provider");
      assert.ok(Object.isFrozen(params.binding.providerBinding));
      assert.ok(Object.isFrozen(params.binding.providerBinding.model_map.default));
      assert.equal(params.binding.providerBinding.revision, 4);
      return { kind: "codex", run: async (req: import("../src/harness.js").AdviceRequest) => {
        turns++;
        assert.equal(req.model, pass === "review" || pass === "delivery" ? "gpt-6-sol" : "gpt-6-luna");
        const summary = "Safe " + encodeURIComponent(provider.credential);
        const text = pass === "intent" ? summary : JSON.stringify({ summary, findings: [], deltas: [], changes: [], scope_notes: [], review_pointers: [] });
        return { text, end: { kind: "terminal", terminal: {
          outcome: "success", subtype: "success", errors: [], metrics: { cost: { kind: "unreported" } },
        } } };
      } };
    }) as unknown as CodexAdviceHarnessFactory;
    const forbidden = () => { throw new Error("unexpected vendor operation"); };
    const claim = { run_id: "provider-pass", review_target_run_id: "target", branch: "branch", base_branch: "main",
      repo: { clone_url: "/origin" }, secrets: { forge_pat: "test-pat", provider } };
    try {
      let result: unknown;
      if (pass === "review") {
        const client = { reportState: async () => {}, postTaskReview: async (_id: string, review: { status: string }) => {
          assert.equal(review.status, "complete"); posted = true; result = review;
        }, checkCodexOperationAdmission: forbidden, retainCodexOperationFlight: forbidden } as unknown as WorkerClient;
        const git = { ensureClone: async () => "/bare", reviewDiff: async () => "+change" } as unknown as GitCache;
        await new ReviewRunner(client, git, log, { homeRoot: root, queryFn: forbidden, codexAdviceHarnessFactory: factory })
          .execute(claim as unknown as ClaimResponse);
        assert.ok(posted);
      } else {
        const runner = new SummaryRunner(log, { homeRoot: root, queryFn: forbidden, codexAdviceHarnessFactory: factory });
        const input = { claim, token: "", model: "", issueTitle: "Title", issueBody: "Body" };
        result = pass === "intent" ? await runner.generateIntentSummary(input)
          : pass === "plan" ? await runner.generatePlanSummary({ ...input, planMd: "Plan" })
          : await runner.generateDeliverySummary({ claim, context: deliveryContext, deadlineMs: Date.now() + 10000 });
      }
      assert.ok(result);
      assert.equal(builds, 1);
      assert.equal(turns, 1);
      assert.ok(!JSON.stringify([result, warnings]).includes(encodeURIComponent(provider.credential)));
      assert.ok(!JSON.stringify([result, warnings]).includes(provider.credential));
    } finally { await fs.rm(root, { recursive: true, force: true }); }
  });
}

for (const pass of ["intent", "plan"] as const) {
  const codexProvider: ClaimProviderBinding = {
    provider_id: "frozen-a", revision: 4, engine: "codex", protocol: "openai_responses",
    base_url: "https://gateway.example.test", auth_style: "bearer", credential: "frozen-provider-key",
    model_map: { default: { model: "gateway-default", context_window: 8192 } },
  };
  const conflicts: Record<string, ClaimProviderBinding> = {
    id: { ...codexProvider, provider_id: "independent-b" },
    revision: { ...codexProvider, revision: 5 },
    key: { ...codexProvider, credential: "independent-provider-key" },
    url: { ...codexProvider, base_url: "https://other.example.test" },
    map: { ...codexProvider, model_map: { default: { model: "other-model" } } },
    context: { ...codexProvider, model_map: { default: { model: "gateway-default", context_window: 16384 } } },
    engine: claudeProvider,
    style: { ...claudeProvider, auth_style: "x_api_key" },
  };
  for (const [field, provider] of Object.entries(conflicts)) {
    it(`${pass} refuses a separate provider changing frozen ${field} before dispatch`, async () => {
      const root = await makeHome("authority-conflict-");
      let calls = 0;
      const forbidden = () => { calls++; throw new Error("unexpected dispatch"); };
      const warnings: unknown[] = [];
      const runner = new SummaryRunner({ ...nullLogger(), warn: (message, fields) => { warnings.push([message, fields]); } }, {
        homeRoot: root, queryFn: forbidden, codexAdviceHarnessFactory: forbidden,
      });
      const claim = { run_id: "authority", secrets: { provider: field === "style" ? claudeProvider : codexProvider } };
      const input = { claim, provider, token: "usable-vendor", model: "", issueTitle: "Title", issueBody: "Body", onUsage: forbidden };
      try {
        const result = pass === "intent" ? await runner.generateIntentSummary(input)
          : await runner.generatePlanSummary({ ...input, planMd: "Plan" });
        assert.equal(result, null);
        assert.equal(calls, 0);
        assert.deepEqual(await fs.readdir(root), []);
        const logged = JSON.stringify(warnings);
        assert.match(logged, /provider_lane_unsupported/);
        assert.ok(!logged.includes(provider.credential));
        assert.ok(!logged.includes(codexProvider.credential));
      } finally { await fs.rm(root, { recursive: true, force: true }); }
    });
  }
  for (const secrets of [{ anthropic_oauth_token: "usable-vendor" }, { codex: {
    auth_mode: "api_key", access_token: "vendor-key", capability: "vendor-cap",
  } }, {}]) {
    it(`${pass} refuses a separate provider added to vendor claim ${Object.keys(secrets).join() || "empty"}`, async () => {
      const root = await makeHome("authority-vendor-");
      let calls = 0;
      const forbidden = () => { calls++; throw new Error("unexpected dispatch"); };
      const runner = new SummaryRunner(nullLogger(), {
        homeRoot: root, queryFn: forbidden, codexAdviceHarnessFactory: forbidden,
      });
      const input = { claim: { run_id: "vendor", secrets }, provider: codexProvider,
        token: "usable-vendor", model: "", issueTitle: "Title", issueBody: "Body", onUsage: forbidden };
      try {
        assert.equal(pass === "intent" ? await runner.generateIntentSummary(input)
          : await runner.generatePlanSummary({ ...input, planMd: "Plan" }), null);
        assert.equal(calls, 0);
        assert.deepEqual(await fs.readdir(root), []);
      } finally { await fs.rm(root, { recursive: true, force: true }); }
    });
  }
  for (const provider of [codexProvider, claudeProvider]) {
    for (const copy of [false, true]) {
      it(`${pass} accepts ${provider.engine} claim with ${copy ? "matching copy" : "claim only"}`, async () => {
        const root = await makeHome("authority-positive-");
        let turns = 0;
        const text = JSON.stringify({ summary: "Bound summary.", deltas: [] });
        const runner = new SummaryRunner(nullLogger(), {
          homeRoot: root,
          queryFn: (async function* (...args: Parameters<SdkQueryFn>) {
            turns++;
            yield* reply(text)(...args);
          }) as SdkQueryFn,
          codexAdviceHarnessFactory: (async (params: Parameters<CodexAdviceHarnessFactory>[0]) => {
            assert.equal(params.binding.authMode, "provider");
            assert.ok(params.binding.authMode === "provider");
            assert.equal(params.binding.providerBinding.provider_id, provider.provider_id);
            return { kind: "codex", run: async () => {
              turns++;
              return { text, end: { kind: "terminal", terminal: {
                outcome: "success", subtype: "success", errors: [], metrics: { cost: { kind: "unreported" } },
              } } };
            } };
          }) as unknown as CodexAdviceHarnessFactory,
        });
        // Reverse field insertion order: comparison is of decoded fields, not raw JSON.
        const matching = Object.fromEntries(Object.entries(structuredClone(provider)).reverse()) as unknown as ClaimProviderBinding;
        const input = { claim: { run_id: "positive", secrets: { provider } },
          ...(copy ? { provider: matching } : {}), token: "", model: provider.engine === "claude" ? "haiku" : "",
          issueTitle: "Title", issueBody: "Body" };
        try {
          assert.ok(pass === "intent" ? await runner.generateIntentSummary(input)
            : await runner.generatePlanSummary({ ...input, planMd: "Plan" }));
          assert.equal(turns, 1);
        } finally { await fs.rm(root, { recursive: true, force: true }); }
      });
    }
  }
}

const native = { auth_mode: "subscription", access_token: "test-subscription", capability: "test-cap", generation: 7, chatgpt_account_id: "test-account", chatgpt_plan_type: null };
for (const engine of ["codex", "unknown", "claude"]) {
  for (const codex of [undefined, native]) {
    if (engine === "claude" && codex === undefined) continue;
    const provider = { engine, credential: "test-provider-key", model_map: {} } as unknown as ClaimProviderBinding;
    for (const pass of ["delivery", "intent", "plan"] as const) {
      it(`${pass} refuses ${engine} provider / native=${!!codex} before model work`, async () => {
        const root = await makeHome("m2-hold-");
        let calls = 0;
        const forbidden = () => { calls++; throw new Error("unexpected work"); };
        const warnings: unknown[] = [];
        const runner = new SummaryRunner({ ...nullLogger(), warn: (message, fields) => { warnings.push([message, fields]); } }, {
          homeRoot: root, queryFn: forbidden, codexAdviceHarnessFactory: forbidden,
        });
        const claim = { run_id: "summary-hold", secrets: { provider, codex, anthropic_oauth_token: "usable-vendor" } };
        const input = { claim, provider, token: "usable-vendor", model: "haiku", issueTitle: "Title", issueBody: "Body", onUsage: forbidden };
        try {
          const result = pass === "delivery"
            ? await runner.generateDeliverySummary({ claim, context: deliveryContext, deadlineMs: Date.now() + 10000, onUsage: forbidden })
            : pass === "intent" ? await runner.generateIntentSummary(input)
            : await runner.generatePlanSummary({ ...input, planMd: "Plan" });
          assert.equal(result, null);
          assert.equal(calls, 0);
          assert.deepEqual(await fs.readdir(root), []);
          assert.match(JSON.stringify(warnings), /provider_lane_unsupported/);
        } finally { await fs.rm(root, { recursive: true, force: true }); }
      });
    }
    it(`review refuses ${engine} provider / native=${!!codex} before admission or clone`, async () => {
      let calls = 0;
      const forbidden = () => { calls++; throw new Error("unexpected work"); };
      const states: StateRequest[] = [];
      const client = {
        reportState: async (_id: string, body: StateRequest) => { states.push(body); },
        postTaskReview: forbidden, retainCodexOperationFlight: forbidden,
        checkCodexOperationAdmission: forbidden, hasFeature: forbidden,
      } as unknown as WorkerClient;
      const claim = { run_id: "review-hold", claim_generation: 7, review_target_run_id: "target",
        branch: "branch", secrets: { provider, codex, anthropic_oauth_token: "usable-vendor" },
      } as unknown as ClaimResponse;
      const runner = new ReviewRunner(client, { ensureClone: forbidden } as unknown as GitCache, nullLogger(), {
        queryFn: forbidden, codexAdviceHarnessFactory: forbidden, preFetchCheck: forbidden,
      });
      await assert.doesNotReject(runner.execute(claim));
      assert.equal(calls, 0);
      assert.equal(states.length, 1);
      assert.ok(states[0]);
      assert.equal(states[0].status, "failed");
      assert.equal(states[0].claim_generation, 7);
      assert.equal(states[0].failure_reason, "provider_lane_unsupported");
    });
  }
}
