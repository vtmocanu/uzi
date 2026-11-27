// Issue #4574: the provider-bound Codex JUDGE through the PRODUCTION advice factory
// (makeProductionCodexAdviceHarnessFactory -> makeProductionLaunchAdviceRoot -> CodexAdviceHarness
// -> launchCodexRoot -> app-server auth -> real transport) against the pinned app-server and an
// authenticated local HTTP Responses fake with an opt-in error-status mode.
// Only the OS-bound primitives are simulated, as in provider-advice-summary.test.ts: the supervisor
// spawn, runner-tree provisioning and the shared-directory owner. This is NOT a production UID split,
// kernel drainage, or session-staging proof.
import assert from "node:assert/strict";
import { test } from "node:test";
import { promises as fs, mkdirSync, writeFileSync, rmSync } from "node:fs";
import path from "node:path";
import { makeProductionCodexAdviceHarnessFactory } from "../../agent/src/codex/codex-executor.js";
import { launchCodexRoot } from "../../agent/src/codex/launcher.js";
import { selectCodexExecutionAuthority } from "../../agent/src/codex/select.js";
import { isCodexProviderGatewayLimit } from "../../agent/src/codex/terminal-normalize.js";
import { JudgeRunner } from "../../agent/src/judge-runner.js";
import { runReadOnlyModelPass, type ModelPassSettlement } from "../../agent/src/model-pass.js";
import type { WorkerClient } from "../../agent/src/client.js";
import type { ClaimProviderBinding, ClaimResponse, JudgeTraceResponse, ReviewRequest, StateRequest } from "../../agent/src/protocol.js";
import { recordingLogger } from "../../agent/test/helpers.js";
import { makeDirectCodexSpawn } from "./direct-codex-spawn.js";
import { FakeProvider } from "./fake-provider.js";
import { resolveCodexBin } from "./provision.js";
import { recordEvidence } from "./evidence.js";
import { PROVIDER_ADVICE_JUDGE_TITLE } from "./titles-provider.js";
import { P_LAYER_SKIP } from "./p-platform.js";

// Linux executes here; macOS must execute this P proof in the existing Linux runner.
// Missing or invalid pinned runtime is a provisioning failure, never a skip.
test(PROVIDER_ADVICE_JUDGE_TITLE, { skip: P_LAYER_SKIP }, async (t) => {
  recordEvidence(PROVIDER_ADVICE_JUDGE_TITLE, "fail");
  const resolved = resolveCodexBin();
  const scratch = path.resolve(__dirname, "../../.uzi/scratch");
  mkdirSync(scratch, { recursive: true });
  const parent = await fs.mkdtemp(path.join(scratch, "provider-advice-judge-"));
  t.after(() => fs.rm(parent, { recursive: true, force: true }));
  const priorLoopback = process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
  process.env.UZI_PROVIDER_ALLOW_LOOPBACK = "true";
  t.after(() => { if (priorLoopback === undefined) delete process.env.UZI_PROVIDER_ALLOW_LOOPBACK;
    else process.env.UZI_PROVIDER_ALLOW_LOOPBACK = priorLoopback; });

  // Assembled at runtime: no provider-token-shaped literal in source.
  const credential = ["advice-judge", "gw/fixture", "key-5d2a"].join("-");
  const verdictText = JSON.stringify({ verdict: "ok", summary: "judged on the frozen binding", recommendations: [] });
  let errorStatus: number | undefined;
  const provider = await FakeProvider.start({
    credential,
    respond: () => [{ type: "message", role: "assistant", content: [{ type: "output_text", text: verdictText }] }],
    errorStatus: () => errorStatus,
  });
  t.after(() => provider.close());

  const rawProvider = (modelMap: Record<string, { model: string; context_window?: number }>) => ({
    provider_id: "advice-judge", revision: 3, engine: "codex" as const, protocol: "openai_responses" as const,
    base_url: provider.baseUrl, auth_style: "bearer" as const, credential, model_map: modelMap,
  });
  const MAPS = {
    // Only the default slot: every judge model falls through to it.
    defaultOnly: { default: { model: "gateway/default-model", context_window: 8192 } },
    // A key whose target differs from the default.
    mappedKey: { default: { model: "gateway/default-model", context_window: 8192 }, "gpt-6-sol": { model: "gateway/sol-model", context_window: 32768 } },
  };

  // Any use of the worker client by the factory is a vendor release/refresh/admission call: there must be none.
  const clientCalls: string[] = [];
  const factoryClient = new Proxy({}, { get: (_target, prop) => { clientCalls.push(String(prop)); return undefined; } }) as unknown as WorkerClient;

  let launches = 0, disposals = 0;
  const launchCodexRootForTest: typeof launchCodexRoot = async (spec, launcherDeps) => {
    launches++;
    const envSink = {};
    const spawn = makeDirectCodexSpawn(resolved.codexBin, 10002, envSink);
    const handle = await launchCodexRoot(spec, { ...launcherDeps,
      env: { UZI_UID_SPLIT: "1", UZI_PROVIDER_ALLOW_LOOPBACK: "true" },
      resolveRunnerUid: () => 10002, rootStartTime: () => 1,
      makeRunnerTrees: request => {
        for (const dir of request.dirs) mkdirSync(dir, { recursive: true });
        for (const file of request.files) writeFileSync(file.path, file.content, { mode: file.mode });
      },
      removeRunnerTree: request => { rmSync(request.root, { recursive: true, force: true }); },
      spawnSupervisor: (command, args, options) => {
        assert.equal(JSON.stringify(options.env).includes(credential), false, "the key never rides the launcher env");
        return spawn(command, args, options);
      },
    });
    return { ...handle, dispose: async ms => {
      const result = await handle.dispose(ms);
      assert.equal(result.clean, true, JSON.stringify(result));
      disposals++;
      return result;
    } };
  };

  const home = path.join(parent, "home");
  await fs.mkdir(home);
  const { logger: log, lines } = recordingLogger();
  const factory = makeProductionCodexAdviceHarnessFactory(factoryClient, log, home, {
    adviceRootTestSeams: { launchCodexRootForTest, sharedDirOwner: { uid: process.getuid!(), gid: process.getgid!() } },
  });
  const refuseClaude = (): never => { throw new Error("the Claude query path must not run on a Codex provider judge"); };

  // The judge-side api client: only the four calls the judge runner may make.
  const trace: JudgeTraceResponse = {
    target: { id: "target-1", kind: "issue", status: "completed", issue_title: "Do X", issue_description: "d", branch: null,
      mr_iid: 7, failure_reason: null, fix_verdict: null, plan_md: "# plan", iteration_count: 2, repo_agents: null, stop_kind: null },
    messages: [],
    inputs: [],
  };
  const judgeCalls = { states: [] as StateRequest[], review: undefined as ReviewRequest | undefined, accessed: new Set<string>() };
  const apiTarget = {
    getTrace: async () => trace,
    postReview: async (_id: string, review: ReviewRequest) => { judgeCalls.review = review; },
    reportState: async (_id: string, body: StateRequest) => { judgeCalls.states.push(body); return { applied: true, status: body.status }; },
    postMessages: async () => undefined,
  };
  const api = new Proxy(apiTarget, { get: (target, prop, receiver) => {
    if (typeof prop === "string") judgeCalls.accessed.add(prop);
    return Reflect.get(target, prop, receiver) as unknown;
  } }) as unknown as WorkerClient;
  const runner = new JudgeRunner(api, log, { homeRoot: home, queryFn: refuseClaude as never, modelTimeoutMs: 60_000, codexAdviceHarnessFactory: factory });
  const claimFor = (modelMap: Record<string, { model: string; context_window?: number }>, judgeModel: string): ClaimResponse => ({
    run_id: "advice-judge-run", kind: "judge", target_run_id: "target-1", claim_generation: 1, issue_iid: null,
    issue_title: "Judge: Do X", issue_description: "", repo: {}, last_seq: 0, agents: [], judge_model: judgeModel,
    judge_signal: { missing_tools: [] },
    secrets: { forge_pat: "", anthropic_oauth_token: "", provider: rawProvider(modelMap) },
  } as unknown as ClaimResponse);
  const reset = (): void => { judgeCalls.states.length = 0; judgeCalls.review = undefined; };

  // (a) Model receipts: the parsed /v1/responses body carries the mapped gateway model for each map shape.
  const receiptsFor = async (modelMap: Record<string, { model: string; context_window?: number }>, judgeModel: string): Promise<unknown> => {
    reset();
    const before = provider.requests.length;
    await runner.execute(claimFor(modelMap, judgeModel));
    assert.equal(provider.requests.length, before + 1, "one provider request per judge");
    assert.equal(judgeCalls.review?.status, "complete", "the judge posted its parsed review");
    assert.equal(judgeCalls.review?.summary, "judged on the frozen binding");
    assert.equal(judgeCalls.states.at(-1)?.status, "completed");
    return provider.requests.at(-1)!.model;
  };
  assert.equal(await receiptsFor(MAPS.defaultOnly, "gpt-6-sol"), "gateway/default-model", "a default-only map serves every model from its default slot");
  assert.equal(await receiptsFor(MAPS.mappedKey, "gpt-6-sol"), "gateway/sol-model", "a mapped key serves its own target, which differs from the default");
  assert.equal(await receiptsFor(MAPS.mappedKey, "gpt-6-luna"), "gateway/default-model", "an absent key falls back to the default slot");
  for (const receipt of provider.receipts) {
    assert.equal(receipt.authorization, `Bearer ${credential}`, "the bound key is the only bearer");
    assert.equal(receipt.status, 200);
  }

  // (b) A gateway 429 / 529 through JudgeRunner.execute: a limit failure, no review, never completed.
  const authority = (() => {
    const selection = selectCodexExecutionAuthority({ provider: rawProvider(MAPS.mappedKey) });
    if (selection.kind !== "codex") throw new Error("expected a Codex selection");
    const bound = selection.binding;
    if (bound.authMode !== "provider") throw new Error("expected a provider authority");
    return bound;
  })();
  for (const status of [429, 529]) {
    errorStatus = status;
    reset();
    const before = provider.requests.length;
    const receiptsBefore = provider.receipts.length;
    await runner.execute(claimFor(MAPS.mappedKey, "gpt-6-sol"));
    assert.equal(provider.requests.length, before + 1, `HTTP ${status}: exactly one provider request, no retry`);
    assert.deepEqual(provider.receipts.slice(receiptsBefore), [{ authorization: `Bearer ${credential}`, status }],
      `HTTP ${status}: the fake recorded the bound bearer and the answered status`);
    const last = judgeCalls.states.at(-1)!;
    assert.equal(last.status, "failed", `HTTP ${status}: the judge run failed`);
    assert.equal(last.rate_limit_type, "provider_gateway_limit", `HTTP ${status}: reported as a provider gateway limit`);
    assert.equal(last.limit_resets_at, undefined, "no reset time: nothing parks on a wall clock");
    assert.equal(last.failure_reason, undefined, "enums only on a limit");
    assert.equal(judgeCalls.review, undefined, `HTTP ${status}: no review posted`);
    assert.equal(judgeCalls.states.some(s => s.status === "completed"), false, `HTTP ${status}: never completed`);
  }
  assert.deepEqual(clientCalls, [], "no vendor release/refresh/admission call, so no vendor cooldown");

  // (c) The same statuses through a direct pass with a settlement: the pass records the raw typed
  // classification, and it satisfies the shared gateway-limit predicate the run lane uses.
  for (const status of [429, 529]) {
    errorStatus = status;
    const settlement: ModelPassSettlement = { terminalSeen: false, policyErrorSet: false, rejectionSet: false, escapedSet: false };
    await assert.rejects(runReadOnlyModelPass({
      provider: authority.providerBinding as ClaimProviderBinding, model: "gpt-6-sol", systemPrompt: "Judge the run.", prompt: "judge it",
      homeRoot: home, homePrefix: "uzi-judge-", label: "judge", timeoutMs: 60_000, queryFn: refuseClaude as never,
      denyReason: "read-only", log,
      codex: { runId: "advice-judge-run", claimGeneration: 1, binding: authority, buildHarness: factory, settlement },
    }));
    assert.equal(settlement.stop, undefined, `HTTP ${status}: not a timeout or abort`);
    assert.equal(settlement.terminalSeen, true, `HTTP ${status}: a raw terminal was seen`);
    assert.equal(settlement.terminal?.isError, true);
    assert.equal(settlement.policyErrorSet, true);
    assert.equal(settlement.rejectionSet, true);
    assert.equal(settlement.rejection, settlement.policyError, "the rejection is the policy's own error");
    assert.equal(settlement.escapedSet, true);
    assert.equal(isCodexProviderGatewayLimit("failed", "provider", settlement.terminal?.classification), true,
      `HTTP ${status}: the recorded classification ${JSON.stringify(settlement.terminal?.classification)} satisfies the gateway-limit predicate`);
  }
  errorStatus = undefined;

  // Root lifecycle and secrecy: every pass disposed its own root cleanly, and no log line carries the key.
  assert.equal(launches, disposals, "every launched root was disposed cleanly");
  assert.ok(launches >= 7, "three receipts, two judge limits and two direct passes each launched a root");
  assert.deepEqual([...judgeCalls.accessed].filter(name => !["getTrace", "postReview", "reportState", "postMessages"].includes(name)), [],
    "the judge runner only called getTrace / postReview / reportState / postMessages");
  const logText = JSON.stringify(lines);
  for (const form of [credential, credential.replaceAll("/", "\\/"), encodeURIComponent(credential)]) {
    assert.equal(logText.includes(form), false, "no log line carries a key form");
  }
  recordEvidence(PROVIDER_ADVICE_JUDGE_TITLE, "pass");
});
