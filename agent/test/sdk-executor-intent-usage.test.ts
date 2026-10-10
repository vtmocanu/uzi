// Issue #2686: the unawaited intent-summary pass is aborted and settled (bounded) on every
// executor exit, so the usage it observed is emitted as one summary_usage message while the
// runner's batcher is still open. Planning stays nonblocking.

import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import { SummaryRunner, type IntentSummaryInput, type PlanSummaryInput } from "../src/summary-runner.js";
import type { WorkerClient } from "../src/client.js";
import type { IterationBudget, Milestone } from "../src/protocol.js";
import { CredentialSwitchSignal, PauseNowSignal, type PauseMode, type PlanVerdict } from "../src/steering.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

const HAIKU = "claude-haiku-4-5-20251001";
const SETTLE_BOUND_MS = 2_000;
const MILESTONES: Milestone[] = [
  { id: "m1", title: "milestone one" },
  { id: "m2", title: "milestone two" },
];

const asMsg = (m: unknown): SDKMessage => m as SDKMessage;
const submitPlan = (plan: string, milestones?: Milestone[]): SDKMessage =>
  asMsg({
    type: "assistant",
    session_id: "sess-1",
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan, ...(milestones ? { milestones } : {}) } }] },
  });
const signalDone = (): SDKMessage =>
  asMsg({ type: "assistant", session_id: "sess-1", message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] } });
const resultSuccess = (): SDKMessage => asMsg({ type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: "sess-1" });

function hangUntilAbort(signal: AbortSignal): Promise<void> {
  return new Promise<void>((resolve) => {
    if (signal.aborted) return resolve();
    const keepAlive = setInterval(() => {}, 1_000);
    signal.addEventListener("abort", () => (clearInterval(keepAlive), resolve()), { once: true });
  });
}

type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);
function spawningTurns(scripts: Script[], onTurn?: (index: number) => void): SdkQueryFn {
  let i = 0;
  return (params) => {
    const index = i++;
    onTurn?.(index);
    const script = scripts[Math.min(index, scripts.length - 1)]!;
    return (async function* () {
      params.options.spawnClaudeCodeProcess?.({ command: "x", args: [] } as never);
      for await (const _ of params.prompt) {
        /* drain */
      }
      if (typeof script === "function") yield* script(params.options.abortController!.signal) as AsyncIterable<SDKMessage>;
      else for (const m of script) yield m;
    })();
  };
}

/** The REAL SummaryRunner over a fake advice query: it observes usage on its first assistant
 *  message, then hangs until the pass is aborted (the generation timeout is huge). */
function realIntentRunner(homeRoot: string, hooks: { observed: () => void; sawAbort: () => void }): SummaryRunner {
  const queryFn = async function* (params: { options: { abortController?: AbortController } }) {
    yield { type: "assistant", message: { id: "m1", model: HAIKU, role: "assistant", content: [{ type: "text", text: "partial" }], usage: { input_tokens: 7, output_tokens: 2 } } };
    hooks.observed();
    const signal = params.options.abortController!.signal;
    await hangUntilAbort(signal);
    hooks.sawAbort();
  } as unknown as SdkQueryFn;
  return new SummaryRunner(nullLogger(), { queryFn, homeRoot, modelTimeoutMs: 600_000 });
}

const client = { postIntentSummary: async () => {}, postPlanSummary: async () => {} } as unknown as WorkerClient;
const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-intent-usage");

let homeDir: string;
let summaryHome: string;
let saved: Record<string, string | undefined>;
beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-intentusage-"));
  summaryHome = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-intentusage-sum-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = "dummy-join-token-do-not-scan-2222";
  process.env.UZI_FORGE_PAT = "dummy-forge-pat-do-not-scan-1111";
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  fs.rmSync(summaryHome, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

interface Probe {
  ctx: RunContext;
  emits: EmittedMessage[];
  mode: { value: PauseMode };
  usage(): EmittedMessage[];
}
function makeCtx(overrides: Partial<RunContext> = {}): Probe {
  const emits: EmittedMessage[] = [];
  const mode: { value: PauseMode } = { value: null };
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: (m) => void emits.push(m),
    oauthToken: "dummy-oauth-token-do-not-scan-0000",
    agents: [],
    config: null,
    sessionId: null,
    onSessionId: () => {},
    gatePlan: async () => approve,
    pullFollowUp: () => undefined,
    reportIteration: async () => undefined,
    checkpoint: async () => {},
    worktreeFingerprint: async () => "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
    pauseModeRequested: () => mode.value,
    parkForPause: async () => true,
    ...overrides,
  };
  return { ctx, emits, mode, usage: () => emits.filter((m) => m.kind === "summary_usage") };
}

function newExec(queryFn: SdkQueryFn, summaryRunner: SummaryRunner, boundMs = SETTLE_BOUND_MS): SdkExecutor {
  return new SdkExecutor(nullLogger(), homeDir, {
    queryFn,
    client,
    summaryRunner,
    intentSettleBoundMs: boundMs,
    spawn: () => ({ pid: 7800 }),
    kill: () => true,
    cliGroupPresent: () => false,
  });
}

function observer() {
  let seen = false;
  let abort = false;
  let release!: () => void;
  const observed = new Promise<void>((r) => (release = r));
  return {
    observed,
    hooks: { observed: () => ((seen = true), release()), sawAbort: () => void (abort = true) },
    sawUsage: () => seen,
    sawAbort: () => abort,
  };
}

function assertOneIntentUsage(probe: Probe): void {
  const usage = probe.usage();
  assert.equal(usage.length, 1, "exactly one summary_usage");
  assert.equal(usage[0]!.agent, "worker");
  assert.equal(usage[0]!.payload["pass"], "intent");
  const mu = usage[0]!.payload["model_usage"] as Record<string, { inputTokens: number }>;
  assert.equal(mu[HAIKU]!.inputTokens, 7);
  assert.deepEqual(Object.keys(usage[0]!.payload).sort(), ["model_usage", "pass"], "never an event or usage key");
}

describe("executor exit settles the unsettled intent pass and emits its usage once", () => {
  it("terminal return", async () => {
    const o = observer();
    const probe = makeCtx({ gatePlan: async () => (await o.observed, approve) });
    const exec = newExec(spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]), realIntentRunner(summaryHome, o.hooks));
    const started = Date.now();
    await exec.run(probe.ctx);
    assert.ok(Date.now() - started < SETTLE_BOUND_MS + 4_000, "bounded by the settle bound, not the generation timeout");
    assert.ok(o.sawAbort(), "the pass saw the abort");
    assertOneIntentUsage(probe);
  });

  it("cancel", async () => {
    const o = observer();
    const cancel = new AbortController();
    const probe = makeCtx({ signal: cancel.signal });
    const exec = newExec(
      spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        (signal) => ({
          // eslint-disable-next-line require-yield
          async *[Symbol.asyncIterator]() {
            await o.observed;
            cancel.abort(new Error("cancelled by owner"));
            await hangUntilAbort(signal);
          },
        }),
      ]),
      realIntentRunner(summaryHome, o.hooks),
    );
    const started = Date.now();
    await assert.rejects(exec.run(probe.ctx));
    assert.ok(Date.now() - started < SETTLE_BOUND_MS + 4_000);
    assert.ok(o.sawAbort());
    assertOneIntentUsage(probe);
  });

  it("disk park (pauseModeRequested disk)", async () => {
    const o = observer();
    const runSignal = new AbortController();
    const probe = makeCtx({ signal: runSignal.signal });
    const exec = newExec(
      spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        (signal) => ({
          // eslint-disable-next-line require-yield
          async *[Symbol.asyncIterator]() {
            await o.observed;
            probe.mode.value = "disk";
            runSignal.abort(new PauseNowSignal());
            await hangUntilAbort(signal);
          },
        }),
      ]),
      realIntentRunner(summaryHome, o.hooks),
    );
    const started = Date.now();
    await assert.rejects(exec.run(probe.ctx), (err: unknown) => err instanceof DiskParkSignal);
    assert.ok(Date.now() - started < SETTLE_BOUND_MS + 4_000);
    assert.ok(o.sawAbort());
    assertOneIntentUsage(probe);
  });

  it("milestone pause return", async () => {
    const o = observer();
    const probe = makeCtx({
      gatePlan: async () => (await o.observed, approve),
      reportIteration: async (): Promise<IterationBudget> => ({ pauseRequested: true, completedCount: 1 }),
    });
    const exec = newExec(spawningTurns([[submitPlan("plan", MILESTONES), resultSuccess()]]), realIntentRunner(summaryHome, o.hooks));
    const started = Date.now();
    const result = await exec.run(probe.ctx);
    assert.ok(result.pausedAt, "the run parked at the milestone pause");
    assert.ok(Date.now() - started < SETTLE_BOUND_MS + 4_000);
    assert.ok(o.sawAbort());
    assertOneIntentUsage(probe);
  });

  it("credential-switch release: the usage is emitted BEFORE the release closes the batcher", async () => {
    const o = observer();
    let gates = 0;
    let usageAtSwitch = -1;
    let sawAbortAtSwitch = false;
    const probe = makeCtx({
      gatePlan: async () => {
        gates++;
        await o.observed;
        throw new CredentialSwitchSignal();
      },
    });
    probe.ctx.attemptCredentialSwitch = async () => {
      // The runner's verified release closes the batcher here, before the executor returns.
      usageAtSwitch = probe.usage().length;
      sawAbortAtSwitch = o.sawAbort();
      return "released";
    };
    const exec = newExec(spawningTurns([[submitPlan("plan"), resultSuccess()]]), realIntentRunner(summaryHome, o.hooks));
    const result = await exec.run(probe.ctx);
    assert.equal(result.switchReleased, true);
    assert.equal(gates, 1);
    assert.equal(usageAtSwitch, 1, "the intent usage was emitted before attemptCredentialSwitch ran");
    assert.ok(sawAbortAtSwitch, "the pass was aborted and settled before the switch attempt");
    assertOneIntentUsage(probe);
  });

  it("credential-switch release from an implement turn", async () => {
    const o = observer();
    let usageAtSwitch = -1;
    const probe = makeCtx();
    probe.ctx.attemptCredentialSwitch = async () => {
      usageAtSwitch = probe.usage().length;
      return "released";
    };
    const exec = newExec(
      spawningTurns([
        [submitPlan("plan"), resultSuccess()],
        () => ({
          // eslint-disable-next-line require-yield
          async *[Symbol.asyncIterator]() {
            await o.observed;
            throw new CredentialSwitchSignal();
          },
        }),
      ]),
      realIntentRunner(summaryHome, o.hooks),
    );
    const result = await exec.run(probe.ctx);
    assert.equal(result.switchReleased, true);
    assert.equal(usageAtSwitch, 1);
    assertOneIntentUsage(probe);
  });
});

/** A fake SummaryRunner whose intent pass never settles and ignores abort. */
function stuckRunner() {
  const rec: { input?: IntentSummaryInput; settled: boolean } = { settled: false };
  const runner = {
    generateIntentSummary: (input: IntentSummaryInput) => {
      rec.input = input;
      return new Promise<string | null>(() => {});
    },
    generatePlanSummary: async () => null,
  } as unknown as SummaryRunner;
  return { runner, rec };
}

describe("the intent settlement edge cases", () => {
  it("a pass that ignores abort is abandoned at the bound and its late usage is dropped", async () => {
    const { runner, rec } = stuckRunner();
    const probe = makeCtx();
    const exec = newExec(spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]), runner, 100);
    const started = Date.now();
    await exec.run(probe.ctx);
    assert.ok(Date.now() - started < 3_000, "run returned at the bound");
    assert.equal(rec.input!.signal!.aborted, true);
    rec.input!.onUsage!({ [HAIKU]: { inputTokens: 1, outputTokens: 1, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } });
    assert.equal(probe.usage().length, 0, "a late usage report is not emitted");
  });

  it("a fenced claim emits no summary_usage", async () => {
    const o = observer();
    const probe = makeCtx({ gatePlan: async () => (await o.observed, approve), claimFenced: () => true });
    const exec = newExec(spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]), realIntentRunner(summaryHome, o.hooks));
    await exec.run(probe.ctx);
    assert.ok(o.sawAbort(), "the pass was still aborted");
    assert.equal(probe.usage().length, 0);
  });

  it("planning starts while the intent pass is still unsettled", async () => {
    const { runner } = stuckRunner();
    const probe = makeCtx();
    let planningStarted = false;
    const exec = newExec(
      spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]], (i) => {
        if (i === 0) planningStarted = true;
      }),
      runner,
      50,
    );
    await exec.run(probe.ctx);
    assert.ok(planningStarted, "the planning turn ran without waiting for the intent pass");
  });

  it("emits nothing when the pass observed no usage", async () => {
    const { runner } = stuckRunner();
    const probe = makeCtx();
    await newExec(spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]), runner, 50).run(probe.ctx);
    assert.equal(probe.usage().length, 0);
  });
});

const USAGE = { [HAIKU]: { inputTokens: 7, outputTokens: 2, cacheReadInputTokens: 0, cacheCreationInputTokens: 0 } };
const sleep = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));

describe("a credential-switch give-up keeps the intent summary", () => {
  it("lets the pass finish naturally, posts its summary and emits its usage once", async () => {
    const posted: string[] = [];
    const postingClient = { postIntentSummary: async (_run: string, s: string) => void posted.push(s), postPlanSummary: async () => {} } as unknown as WorkerClient;
    // The intent pass finishes after 300ms with usage, or resolves null when aborted.
    const runner = {
      generateIntentSummary: (input: IntentSummaryInput) =>
        new Promise<string | null>((resolve) => {
          const timer = setTimeout(() => {
            input.onUsage!(USAGE);
            resolve("INTENT");
          }, 300);
          input.signal!.addEventListener("abort", () => (clearTimeout(timer), resolve(null)), { once: true });
        }),
      generatePlanSummary: async () => null,
    } as unknown as SummaryRunner;
    let gates = 0;
    const probe = makeCtx({
      gatePlan: async () => {
        gates++;
        if (gates === 1) throw new CredentialSwitchSignal();
        await sleep(600);
        return approve;
      },
    });
    probe.ctx.attemptCredentialSwitch = async () => "gave_up";
    const exec = new SdkExecutor(nullLogger(), homeDir, {
      queryFn: spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]),
      client: postingClient,
      summaryRunner: runner,
      intentSettleBoundMs: SETTLE_BOUND_MS,
      spawn: () => ({ pid: 7800 }),
      kill: () => true,
      cliGroupPresent: () => false,
    });
    await exec.run(probe.ctx);
    assert.equal(gates, 2, "the gate was re-presented after the give-up");
    assert.deepEqual(posted, ["INTENT"]);
    const usage = probe.usage();
    assert.equal(usage.length, 1, "exactly one summary_usage");
    assert.equal(usage[0]!.payload["pass"], "intent");
  });
});

describe("the plan pass's summary_usage", () => {
  function planRunner() {
    return {
      generateIntentSummary: async () => null,
      generatePlanSummary: async (input: PlanSummaryInput) => {
        input.onUsage!(USAGE);
        return null;
      },
    } as unknown as SummaryRunner;
  }
  // The real gate invokes the plan-summary generator it is handed; this one does the same.
  const callingPlanSummary: RunContext["gatePlan"] = async (plan, _milestones, generate) => (await generate?.(plan), approve);
  const planUsage = (probe: Probe) => probe.usage().filter((m) => m.payload["pass"] === "plan");

  it("an approved plan emits exactly one summary_usage with the reported model_usage", async () => {
    const probe = makeCtx({ gatePlan: callingPlanSummary });
    await newExec(spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]), planRunner()).run(probe.ctx);
    const usage = planUsage(probe);
    assert.equal(usage.length, 1);
    assert.equal(usage[0]!.agent, "worker");
    assert.deepEqual(usage[0]!.payload["model_usage"], USAGE);
    assert.deepEqual(Object.keys(usage[0]!.payload).sort(), ["model_usage", "pass"]);
  });

  it("a fenced claim emits none", async () => {
    const probe = makeCtx({ gatePlan: callingPlanSummary, claimFenced: () => true });
    await newExec(spawningTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]), planRunner()).run(probe.ctx);
    assert.equal(planUsage(probe).length, 0);
  });
});
