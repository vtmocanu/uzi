import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext } from "../src/executor.js";
import { CredentialSwitchSignal, PauseNowSignal, type PlanVerdict } from "../src/steering.js";
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

// PRD #1226 M3: the checkpoint-first same-lead completion attempt loop + post-attempt failure
// routing. The SDK boundary is faked (queryFn), and the completion seams
// (recordCompletionAttempt / enterCompletionHold / checkpoint / worktreeFingerprint) are injected
// mocks, so the loop's routing is provable with no network and no live model. Most tests script
// signal_done as a tool_use block; the integrated SDK admission tests also call the executor's
// actual in-process MCP server through JSON-RPC before streaming those frames.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

const nonexistentWorktree = nonexistentWorktreeFactory("uzi-completion");

// --- scripted SDK messages (subset of sdk-executor.test.ts's helpers) ---
function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function signalDone(input: Record<string, unknown> = {}, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);

interface Turn {
  promptText?: string;
}

function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    const turn: Turn = {};
    turns.push(turn);
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        turn.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turns };
}

// Exercise the executor-provided SDK server, without a transitive MCP dependency.
// Resolve replies from send() rather than polling or sleeping.
async function connectSignalRpc(server: unknown) {
  type Message = Record<string, unknown>;
  const pending = new Map<number, (message: Message) => void>();
  const transport: {
    start(): Promise<void>;
    send(message: Message): Promise<void>;
    close(): Promise<void>;
    onmessage?: (message: Message) => void;
    onclose?: () => void;
  } = {
    async start() {},
    async send(message) {
      const id = message.id;
      if (typeof id === "number") {
        pending.get(id)?.(message);
        pending.delete(id);
      }
    },
    async close() { transport.onclose?.(); },
  };
  const instance = (server as {
    instance: { connect(transport: unknown): Promise<void>; close(): Promise<void> };
  }).instance;
  let nextId = 0;
  const request = (method: string, params: Message): Promise<Message> => {
    const id = ++nextId;
    return new Promise(resolve => {
      pending.set(id, resolve);
      transport.onmessage!({ jsonrpc: "2.0", id, method, params });
    });
  };
  try {
    await instance.connect(transport);
    const initialized = await request("initialize", {
      protocolVersion: "2025-06-18", capabilities: {},
      clientInfo: { name: "runner-completion-attempt-test", version: "0" },
    });
    assert.equal(initialized.error, undefined);
    transport.onmessage!({ jsonrpc: "2.0", method: "notifications/initialized" });
    return { request, close: () => instance.close() };
  } catch (error) {
    await instance.close();
    throw error;
  }
}

function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    // eslint-disable-next-line require-yield
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener("abort", () => { clearInterval(keepAlive); resolve(); }, { once: true });
      });
    },
  };
}

interface AttemptCall {
  declared: string[];
  head: string | null;
  worktreeFingerprint: string | null;
}

interface CompletionProbe {
  ctx: RunContext;
  order: string[];
  attemptCalls: AttemptCall[];
  holdReasons: string[];
  iterations: number[];
  // PRD #1226 M5 (D6): the unmet id set passed to each askCompletionQuestion call, one entry per
  // call, in order. Empty when the live-window seam was never wired/consulted.
  askQuestionCalls: string[][];
}

type CompletionDecision =
  | { outcome: "continue"; guidance?: string }
  | { outcome: "expired" };

// makeCompletionCtx builds a minimal issue-run ctx wired for the interlock path. `unmetScript` is
// consumed one entry per recordCompletionAttempt call (the last entry sticks). `wireHold` false
// leaves enterCompletionHold unwired (the M3 legacy-throw fallback). `worktreeFingerprint` defaults
// to a constant so head derivation (its first line) and the completion fingerprint are deterministic.
function makeCompletionCtx(
  opts: {
    unmetScript?: string[][];
    wireHold?: boolean;
    interlock?: boolean;
    config?: RunContext["config"];
    worktreeFingerprint?: string | null;
    // PRD #1226 M4: what the wired enterCompletionHold returns. true = it entered the verified
    // hold (caller latches completionHeld + breaks); false = it could NOT hold (kept the run live)
    // so the caller falls through to the legacy throw. Defaults true.
    holdReturns?: boolean;
    // PRD #1226 M4 (D3): from this iteration number on, reportIteration serves budgetExhausted:true
    // (the server's steer). undefined ⇒ never served.
    budgetExhaustedFromIteration?: number;
    // PRD #1226 M5 (D6): scripted completion-question decisions, one consumed per
    // askCompletionQuestion call (the LAST entry sticks). When provided, wires
    // ctx.askCompletionQuestion (the live-window seam); absent leaves it UNWIRED (the M4 fallback —
    // the stall routes straight to the hold).
    askQuestionScript?: CompletionDecision[];
  } = {},
): CompletionProbe {
  const order: string[] = [];
  const attemptCalls: AttemptCall[] = [];
  const holdReasons: string[] = [];
  const iterations: number[] = [];
  const askQuestionCalls: string[][] = [];
  let askQuestionN = 0;
  const unmetScript = opts.unmetScript ?? [[]];
  let attemptN = 0;
  const wf = opts.worktreeFingerprint === undefined ? "TIP\n M src/x.ts" : opts.worktreeFingerprint;
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };

  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: () => {},
    oauthToken: OAUTH,
    agents: [],
    config: opts.config ?? { max_iterations: 10 },
    sessionId: null,
    onSessionId: () => {},
    gatePlan: async () => approve,
    pullFollowUps: () => [],
    reportIteration: async (n) => {
      iterations.push(n);
      if (
        opts.budgetExhaustedFromIteration !== undefined &&
        n >= opts.budgetExhaustedFromIteration
      ) {
        return { budgetExhausted: true };
      }
      return undefined;
    },
    completionInterlock: opts.interlock ?? true,
    checkpoint: async () => {
      order.push("checkpoint");
    },
    worktreeFingerprint: async () => wf,
    recordCompletionAttempt: async (args) => {
      order.push("attempt");
      attemptCalls.push(args);
      const unmet = unmetScript[Math.min(attemptN, unmetScript.length - 1)]!;
      attemptN++;
      return { unmet, attemptCount: attemptN };
    },
  };
  if (opts.wireHold !== false) {
    ctx.enterCompletionHold = async (reason) => {
      order.push("hold");
      holdReasons.push(reason);
      // The runner's real enterCompletionHold returns true only when it entered the verified hold;
      // false keeps the run live and the caller must fall through to the legacy throw.
      return opts.holdReturns ?? true;
    };
  }
  if (opts.askQuestionScript) {
    const script = opts.askQuestionScript;
    ctx.askCompletionQuestion = async (unmet) => {
      order.push("askQuestion");
      askQuestionCalls.push(unmet);
      const decision = script[Math.min(askQuestionN, script.length - 1)]!;
      askQuestionN++;
      return decision;
    };
  }
  return { ctx, order, attemptCalls, holdReasons, iterations, askQuestionCalls };
}

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-completionhome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = FAKE_JOIN_TOKEN;
  process.env.UZI_FORGE_PAT = FAKE_PAT;
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("approved m1 resume reconciliation", () => {
  for (const ack of ["present", "absent"] as const) {
    it(`m1 resume display combines local progress with ${ack} ACK`, async () => {
      const milestones = ["m1", "m2", "m3"].map((id) => ({ id, title: `Title ${id}` }));
      const report = {
        type: "assistant", session_id: "sess-1",
        message: { content: [{ type: "tool_use", id: "progress", name: "mcp__uzi__report_progress",
          input: { completed: ["m2"], in_progress: ["m1", "m3"] } }] },
      } as unknown as SDKMessage;
      const { queryFn, turns } = fakeTurns([[report, resultSuccess()], [signalDone(), resultSuccess()]]);
      const probe = makeCompletionCtx({ interlock: false });
      const pushed: unknown[] = [];
      Object.assign(probe.ctx, {
        planApproved: true, approvedPlan: "Approved", sessionId: "sess-1", frozenMilestones: milestones,
        reportIteration: async () => ack === "present" ? { completedIds: ["m1", "unknown-server"] } : undefined,
        reportProgress: async (progress: unknown) => { pushed.push(progress); },
      });
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      const second = turns[1]!.promptText!;
      assert.match(second, ack === "present" ? /\[m1\] Title m1 — done/ : /\[m1\] Title m1 — in progress/);
      assert.match(second, /\[m2\] Title m2 — done/);
      assert.match(second, /\[m3\] Title m3 — in progress/);
      assert.doesNotMatch(second, /unknown-server/);
      assert.equal(second.includes("Prior completion declarations may be unknown"), ack === "absent");
      assert.deepEqual(pushed, [{ completed: ["m2"], in_progress: ["m1", "m3"] }]);
    });
  }
  for (const list of [undefined, null, []]) {
    it(`m1 resume no breakdown: ${JSON.stringify(list)}`, async () => {
      const { queryFn, turns } = fakeTurns([[signalDone(), resultSuccess()]]);
      const probe = makeCompletionCtx({ interlock: false });
      Object.assign(probe.ctx, { planApproved: true, approvedPlan: "Approved", sessionId: "sess-1",
        frozenMilestones: list, reportIteration: async () => ({ completedIds: ["m1"] }) });
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.doesNotMatch(turns[0]!.promptText!, /Keep this tracker honest|Immediately before|\[m1\]/);
    });
  }
  for (const declaration of ["progress", "done", "unfinished", "seeded", "reviewed", "gated"] as const) {
    it(`m1 resume first prompt and completion: ${declaration}`, async () => {
      // f93ddb57: seq1969 resumed iter1, 2279 missing m4, 2280 iter2, 2284–88
      // redundant checks, 2290–93 declarations (~67s). ed894e5c: 5523 committed/reviewed
      // m1–8, 5526 missing m3–9, 5527 iter2, 5529–38 reports; maintainer-owned m9
      // remained incomplete. Scripts prove delivery/mechanics, not model compliance.
      const milestones = ["m1", "m2", "m3", "m4"].map((id) => ({ id, title: `Title ${id}` }));
      const progressMessage = {
        type: "assistant", session_id: "sess-1",
        message: { content: [{ type: "tool_use", id: "progress",
          name: "mcp__uzi__report_progress", input: { completed: ["m4"], in_progress: [] } }] },
      } as unknown as SDKMessage;
      const messages = ["progress", "seeded", "reviewed", "gated"].includes(declaration)
        ? [progressMessage, signalDone({ milestones_completed: ["m4"] }), resultSuccess()]
        : [signalDone(declaration === "done" ? { milestones_completed: ["m4"] } : {}), resultSuccess()];
      const planMessage = submitPlan("Approved m1–m4");
      (planMessage as unknown as { message: { content: { input: Record<string, unknown> }[] } })
        .message.content[0]!.input.milestones = milestones;
      const { queryFn, turns } = fakeTurns(declaration === "gated"
        ? [[planMessage, resultSuccess()], messages] : [messages]);
      const probe = makeCompletionCtx();
      Object.assign(probe.ctx, {
        planApproved: true, approvedPlan: "Approved m1–m4", sessionId: "sess-1",
        frozenMilestones: milestones,
      });
      if (declaration === "seeded") Object.assign(probe.ctx, { seeded: true, sessionId: null });
      if (declaration === "reviewed") Object.assign(probe.ctx, { reviewedPlanResume: true, sessionId: null });
      if (declaration === "gated") Object.assign(probe.ctx, {
        planApproved: false, sessionId: null, frozenMilestones: [{ id: "stale", title: "Stale claim" }],
      });
      const persisted = new Set(["m1", "m2", "m3"]);
      probe.ctx.reportIteration = async (n) => {
        probe.iterations.push(n);
        return { completedIds: [...persisted], completedCount: persisted.size };
      };
      const pushed: unknown[] = [];
      probe.ctx.reportProgress = async (progress) => {
        pushed.push(progress);
        for (const id of progress.completed ?? []) persisted.add(id);
      };
      probe.ctx.recordCompletionAttempt = async (args) => {
        probe.attemptCalls.push(args);
        for (const id of args.declared) persisted.add(id);
        return { unmet: milestones.filter((m) => !persisted.has(m.id)).map((m) => m.id),
          attemptCount: probe.attemptCalls.length };
      };
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.deepEqual(pushed, ["progress", "seeded", "reviewed", "gated"].includes(declaration)
        ? [{ completed: ["m4"], in_progress: [] }] : [], "only provider progress is pushed");
      const implementTurns = declaration === "gated" ? turns.slice(1) : turns;
      const first = implementTurns[0]!.promptText!;
      assert.doesNotMatch(first, /Stale claim/);
      for (const m of milestones) assert.match(first, new RegExp(`\\[${m.id}\\] ${m.title}`), "first prompt carries frozen context");
      for (const id of ["m1", "m2", "m3"]) assert.ok(first.includes(`[${id}] Title ${id} — done`),
        "first prompt reconciles every persisted ID");
      assert.match(first, /\[m4\] Title m4 — not started/, "unreported work requires reconciliation");
      assert.match(first, /Immediately before .*signal_done/, "before-done reconciliation reaches first prompt");
      if (declaration === "unfinished") {
        assert.ok(result.completionHeld);
        assert.equal(probe.attemptCalls.length, 3);
        assert.equal(probe.holdReasons.length, 1);
        assert.match(turns[1]!.promptText!, /Title m4/, "completion follow-up retains frozen title");
      } else {
        assert.equal(implementTurns.length, 1, "no reentry");
        assert.deepEqual(probe.iterations, [1]);
        assert.equal(probe.attemptCalls.length, 1);
        assert.deepEqual(probe.attemptCalls[0]!.declared, ["m4"]);
        assert.equal(result.completionHeld, undefined);
        assert.deepEqual(probe.holdReasons, []);
      }
    });
  }
});

describe("issue3089 m1 integrated SDK completion admission", () => {
  it("issue3089 m4 Claude non_closing alone preserves m2 through autonomous rework and three-attempt stall", { timeout: 10_000 }, async () => {
    const probe = makeCompletionCtx({
      unmetScript: [["m2"]], askQuestionScript: [{ outcome: "expired" }],
    });
    const milestones = [
      { id: "m1", title: "Delivered work" }, { id: "m2", title: "Required integration check" },
    ];
    Object.assign(probe.ctx, {
      kind: "issue", interactive: false, planApproved: true,
      approvedPlan: "Deliver m1 and m2", sessionId: "sess-1", frozenMilestones: milestones,
    });
    const ask = probe.ctx.askCompletionQuestion!;
    probe.ctx.askCompletionQuestion = async (unmet, trigger) => {
      assert.equal(probe.attemptCalls.length, 3, "non_closing cannot open an early owner window");
      assert.deepEqual(trigger, { kind: "no_progress" });
      return ask(unmet, trigger);
    };
    const prompts: string[] = [];
    const input = { non_closing: { reason: "Owner says defer m2" }, milestones_completed: ["m1"] };
    const queryFn: SdkQueryFn = params => (async function* () {
      assert.ok(prompts.length < 3, "unchanged completion is bounded to three attempts");
      for await (const message of params.prompt) {
        prompts.push(String((message as { message: { content: unknown } }).message.content));
      }
      assert.deepEqual(probe.askQuestionCalls, [], "rework before stall remains autonomous");
      const rpc = await connectSignalRpc(params.options.mcpServers!.uzi);
      try {
        const turnInput = prompts.length === 1 ? input : { milestones_completed: ["m1"] };
        const admitted = await rpc.request("tools/call", { name: "signal_done", arguments: turnInput });
        assert.equal(admitted.error, undefined);
        assert.notEqual((admitted.result as { isError?: boolean }).isError, true,
          "non_closing is valid metadata, even though it cannot revise scope");
        yield signalDone(turnInput);
        yield resultSuccess();
      } finally {
        await rpc.close();
      }
    })();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(prompts.length, 3);
    for (const prompt of prompts.slice(1)) {
      assert.match(prompt, /- m2: Required integration check/);
      assert.match(prompt, /Finish the remaining milestone/);
    }
    assert.deepEqual(probe.attemptCalls, Array.from({ length: 3 }, () => ({
      declared: ["m1"], head: "TIP", worktreeFingerprint: "TIP\n M src/x.ts", nonClosing: input.non_closing,
    })), "attempt payload contains only the declaration and real tree identity, never scope selection");
    assert.deepEqual(probe.order, [
      "checkpoint", "attempt", "checkpoint", "attempt", "checkpoint", "attempt", "askQuestion", "hold",
    ]);
    assert.deepEqual(probe.askQuestionCalls, [["m2"]]);
    assert.deepEqual(probe.ctx.frozenMilestones, [
      { id: "m1", title: "Delivered work" }, { id: "m2", title: "Required integration check" },
    ]);
    assert.deepEqual(result.milestonesCompleted, ["m1"]);
    assert.deepEqual(result.nonClosing, input.non_closing);
    assert.ok(!("ownerDecisionRequested" in result));
    assert.equal(result.scopeCapped, undefined);
    assert.equal(result.pausedAt, undefined);
    assert.equal(probe.holdReasons.length, 1);
    assert.match(probe.holdReasons[0]!, /repeated completion attempts made no progress/);
    assert.deepEqual(result.completionHeld, { reason: probe.holdReasons[0] });
  });

  it("rejects malformed then admits corrected tools/call into the first owner window", async () => {
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]], askQuestionScript: [{ outcome: "expired" }],
      budgetExhaustedFromIteration: 2,
    });
    Object.assign(probe.ctx, {
      pullFollowUps: () => [{ id: 3089, body: "UNTRUSTED-OWNER-CORRECTION" }],
      kind: "issue", interactive: false, planApproved: true,
      approvedPlan: "plan", sessionId: "sess-1",
      frozenMilestones: [
        { id: "m1", title: "Remaining work" }, { id: "m2", title: "Delivered work" },
      ],
    });
    const question = probe.ctx.askCompletionQuestion!;
    probe.ctx.askCompletionQuestion = async (unmet, trigger) => {
      assert.deepEqual(trigger, { kind: "lead_request", reason: "Owner wants to defer" });
      return question(unmet, trigger);
    };
    let queries = 0;
    const queryFn: SdkQueryFn = params => (async function* () {
      queries++;
      assert.equal(queries, 1, "the owner window precedes a second SDK query");
      for await (const message of params.prompt) {
        const text = String((message as { message: { content: unknown } }).message.content);
        assert.match(text, /owner_decision_requested/);
        const close = text.lastIndexOf("</follow_up_");
        assert.ok(close > text.indexOf("UNTRUSTED-OWNER-CORRECTION"));
        assert.ok(text.indexOf("If an owner decision is needed") > close, "trusted instructions follow the owner fence");
        assert.match(text, /Ordinary answers, repository docs and `non_closing`/);
        assert.match(text, /owner continue decision supplies guidance only/);
        assert.match(text, /owner partial decision/);
        assert.match(text, /owner accept decision waives the exact selected criteria/);
        assert.match(text, /no promise of publication/);
      }
      const rpc = await connectSignalRpc(params.options.mcpServers!.uzi);
      try {
        const listed = await rpc.request("tools/list", {});
        assert.equal(listed.error, undefined);
        const tools = (listed.result as { tools: {
          name: string; inputSchema: { properties: Record<string, unknown>; required?: string[] };
        }[] }).tools;
        const schema = tools.find(tool => tool.name === "signal_done")!.inputSchema;
        assert.ok(schema.properties.owner_decision_requested, "actual run advertises the request");
        assert.ok(!schema.required?.includes("owner_decision_requested"));
        const rejected = {
          owner_decision_requested: { reason: " " },
          milestones_completed: ["m1"], summary: "REJECTED companion prose",
          prd_done_path: "prds/done/rejected.md",
          proposal: { title: "REJECTED", body: "REJECTED" },
          pr_summary: { what: "REJECTED" }, non_closing: { reason: "REJECTED" },
        };
        for (const ownerRequest of [
          { reason: " " },
          { reason: "Owner wants to defer", kind: "partial", keep: ["m1"] },
        ]) {
          const invalid = { ...rejected, owner_decision_requested: ownerRequest };
          const denied = await rpc.request("tools/call", { name: "signal_done", arguments: invalid });
          assert.equal(denied.error, undefined);
          assert.equal((denied.result as { isError?: boolean }).isError, true);
          yield signalDone(invalid);
          assert.deepEqual(probe.order, [], "rejected frame cannot checkpoint or attempt completion");
          assert.deepEqual(probe.askQuestionCalls, []);
        }
        const accepted = {
          owner_decision_requested: { reason: "  Owner wants to defer  " },
          milestones_completed: ["m2"],
        };
        const admitted = await rpc.request("tools/call", { name: "signal_done", arguments: accepted });
        assert.equal(admitted.error, undefined);
        const ack = admitted.result as { isError?: boolean; content: unknown[] };
        assert.notEqual(ack.isError, true);
        assert.ok(ack.content.length > 0, "real SDK handler acknowledges corrected request");
        yield signalDone(accepted);
        yield resultSuccess();
      } finally {
        await rpc.close();
      }
    })();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(queries, 1);
    assert.deepEqual(probe.order, ["checkpoint", "attempt", "askQuestion", "hold"]);
    assert.deepEqual(probe.askQuestionCalls, [["m1"]]);
    assert.equal(probe.attemptCalls.length, 1);
    assert.deepEqual(probe.attemptCalls[0]!.declared, ["m2"], "rejected m1 declaration never accepted");
    assert.deepEqual(result.milestonesCompleted, ["m2"]);
    for (const key of ["summary", "prdDonePath", "proposal", "prSummary", "nonClosing"] as const) {
      assert.equal(result[key], undefined, `rejected companion ${key} never accepted`);
    }
    assert.ok(result.completionHeld);
    assert.doesNotMatch(JSON.stringify(result), /REJECTED/);
  });

  for (const mode of ["legacy", "interactive", "task", "prompt", "ci_fix", "self_improve"] as const) {
    it(`omits request in actual per-run tools/list for ${mode}`, async () => {
      const probe = makeCompletionCtx();
      Object.assign(probe.ctx, {
        kind: mode === "legacy" || mode === "interactive" ? "issue" : mode,
        completionInterlock: mode !== "legacy", interactive: mode === "interactive",
        planApproved: true, approvedPlan: "plan", sessionId: "sess-1",
      });
      let listings = 0;
      const queryFn: SdkQueryFn = params => (async function* () {
        for await (const message of params.prompt) {
          assert.doesNotMatch(String((message as { message: { content: unknown } }).message.content), /owner_decision_requested|frozen completion contract is authoritative/);
        }
        const rpc = await connectSignalRpc(params.options.mcpServers!.uzi);
        try {
          const listed = await rpc.request("tools/list", {});
          assert.equal(listed.error, undefined);
          const tools = (listed.result as { tools: {
            name: string; inputSchema: { properties: Record<string, unknown> };
          }[] }).tools;
          const schema = tools.find(tool => tool.name === "signal_done")!.inputSchema;
          assert.equal(schema.properties.owner_decision_requested, undefined);
          listings++;
          const called = await rpc.request("tools/call", { name: "signal_done", arguments: {} });
          assert.equal(called.error, undefined);
          assert.notEqual((called.result as { isError?: boolean }).isError, true);
          yield signalDone();
          yield resultSuccess();
        } finally {
          await rpc.close();
        }
      })();
      await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.equal(listings, 1, "inspected the server supplied to the actual query");
      assert.deepEqual(probe.askQuestionCalls, []);
    });
  }
});

describe("SdkExecutor completion interlock (PRD #1226 M3)", () => {
  for (const answered of [false, true]) {
    for (const restart of ["credential gave_up", "pause refused", "wall refused"] as const) {
      it(`issue3089 landing Claude retains ${answered ? "answered" : "autonomous"} rework after ${restart}`, async () => {
        const { queryFn, turns } = fakeTurns([
          [signalDone(answered ? { owner_decision_requested: { reason: "Need owner input" } } : {}), resultSuccess()],
          async function* () {
            yield assistantText("Interrupted recheck");
            if (restart === "credential gave_up") throw new CredentialSwitchSignal();
            if (restart === "pause refused") throw new PauseNowSignal();
            throw new Error("run exceeded its wall-clock timeout");
          },
          [assistantText("Recheck completed"), resultSuccess()],
          [signalDone(), resultSuccess()],
        ]);
        const probe = makeCompletionCtx({ unmetScript: [["m2"], []], holdReturns: false,
          ...(answered ? { askQuestionScript: [{ outcome: "continue" as const }] } : {}) });
        Object.assign(probe.ctx, { kind: "issue", interactive: false, planApproved: true,
          approvedPlan: "OBSOLETE_CHECKED_PLAN", sessionId: "sess-1",
          frozenMilestones: [{ id: "m2", title: "OBSOLETE_MILESTONE" }],
          attemptCredentialSwitch: async () => "gave_up",
          parkForPause: async () => false,
          parkForWall: async () => "refused" });
        await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
        assert.equal(turns.length, 4);
        for (const turn of turns.slice(1, 3)) {
          if (answered) {
            assert.match(turn.promptText!, /Worker instruction: call `signal_done` again/);
            assert.doesNotMatch(turn.promptText!, /OBSOLETE_CHECKED_PLAN|OBSOLETE_MILESTONE/);
          } else {
            assert.match(turn.promptText!, /Completion check \(structural interlock\)/);
            assert.match(turn.promptText!, /m2: OBSOLETE_MILESTONE/);
          }
        }
        assert.doesNotMatch(turns[3]!.promptText!, /Worker instruction: call `signal_done` again|Completion check \(structural interlock\)/,
          "a successfully spent rework does not leak into the next ordinary turn");
        assert.equal(probe.attemptCalls.length, 2, "restart consumes no completion attempt");
      });
    }
  }

  for (const outcome of ["parked", "undeliverable", "refused"] as const) {
    it(`issue3089 landing Claude numeric wall exhaustion falls back to wall park ${outcome}`, async t => {
      let now = Date.now();
      t.mock.method(Date, "now", () => now);
      const { queryFn } = fakeTurns([
        async function* () {
          try {
            yield signalDone({ owner_decision_requested: { reason: "Owner input" } });
            yield resultSuccess();
          } finally {
            // Existing SDK cleanup-time fixture pattern: disarmWall debits this
            // without yielding to a timer or setting a wall pause mode.
            now += 20_000;
          }
        },
        [signalDone(), resultSuccess()],
      ]);
      const probe = makeCompletionCtx({ unmetScript: [["m2"], []], holdReturns: false,
        config: { max_iterations: 10, run_timeout_seconds: 10 },
        askQuestionScript: [{ outcome: "continue" }] });
      let parks = 0;
      Object.assign(probe.ctx, { kind: "issue", interactive: false, planApproved: true,
        approvedPlan: "plan", sessionId: "sess-1", pauseModeRequested: () => null,
        // A refused wall park represents an owner extension; the next ACK applies it.
        reportIteration: async () => parks ? { totalWallSeconds: 30 } : undefined,
        parkForWall: async () => { parks++; return outcome; } });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.equal(parks, 1);
      assert.equal(result.completionHeld, undefined);
      assert.equal(Boolean(result.walled), outcome !== "refused");
      assert.equal(probe.attemptCalls.length, outcome === "refused" ? 2 : 1);
    });
  }

  for (const outcome of ["parked", "undeliverable", "refused"] as const) {
    it(`issue3089 landing Claude refused completion hold falls back to wall park ${outcome}`, async () => {
      const { queryFn } = fakeTurns([
        [signalDone({ owner_decision_requested: { reason: "Owner input" } }), resultSuccess()],
        [signalDone(), resultSuccess()],
      ]);
      const probe = makeCompletionCtx({ unmetScript: [["m2"], []], holdReturns: false,
        askQuestionScript: [{ outcome: "continue" }] });
      let wallPending = true;
      let parks = 0;
      Object.assign(probe.ctx, { kind: "issue", interactive: false, planApproved: true,
        approvedPlan: "plan", sessionId: "sess-1",
        pauseModeRequested: () => probe.attemptCalls.length && wallPending ? "wall" : null,
        clearWallMode: () => { wallPending = false; },
        parkForWall: async () => { parks++; return outcome; } });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.equal(parks, 1);
      assert.equal(result.completionHeld, undefined);
      assert.equal(Boolean(result.walled), outcome !== "refused");
      assert.equal(probe.attemptCalls.length, outcome === "refused" ? 2 : 1);
    });
  }

  it("issue3089 completion recheck retains queued owner input until an ordinary turn", async () => {
    const { queryFn, turns } = fakeTurns([
      [signalDone({ owner_decision_requested: { reason: "Need a decision" } }), resultSuccess()],
      [assistantText("Rechecking"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"], []],
      askQuestionScript: [{ outcome: "continue" }] });
    Object.assign(probe.ctx, { kind: "issue", interactive: false, planApproved: true,
      approvedPlan: "plan", sessionId: "sess-1" });
    let queued = false;
    const ask = probe.ctx.askCompletionQuestion!;
    probe.ctx.askCompletionQuestion = async (...args) => {
      const answer = await ask(...args);
      queued = true;
      return answer;
    };
    let pulls = 0;
    const included: number[] = [];
    probe.ctx.pullFollowUps = () => {
      if (!queued) return [];
      pulls++;
      queued = false;
      return [{ id: 3089, body: "QUEUED-OWNER-INPUT" }];
    };
    probe.ctx.followUpIncluded = id => included.push(id);
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(turns.length, 3);
    assert.match(turns[1]!.promptText!, /Worker instruction: call `signal_done` again/);
    assert.doesNotMatch(turns[1]!.promptText!, /QUEUED-OWNER-INPUT/);
    assert.match(turns[2]!.promptText!, /QUEUED-OWNER-INPUT/);
    assert.equal(pulls, 1);
    assert.deepEqual(included, [3089]);
  });

  it("issue3089 m1 Claude request clears across turns and reason stays out of rework", async () => {
    const { queryFn, turns } = fakeTurns([
      [signalDone({ owner_decision_requested: { reason: "PRIVATE reason" } }), resultSuccess()],
      [signalDone(), resultSuccess()], [signalDone(), resultSuccess()],
    ]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"], ["m1"], []],
      askQuestionScript: [{ outcome: "continue", guidance: "finish it" }] });
    Object.assign(probe.ctx, { kind: "issue", interactive: false, planApproved: true,
      approvedPlan: "plan", sessionId: "sess-1" });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(result.completionHeld, undefined);
    assert.equal(probe.attemptCalls.length, 3);
    assert.deepEqual(probe.askQuestionCalls, [["m1"]], "later plain done does not reuse request");
    for (const turn of turns.slice(1)) assert.ok(!turn.promptText!.includes("PRIVATE reason"));
    const autonomous = turns[2]!.promptText!;
    assert.match(autonomous, /Ordinary answers, repository docs and `non_closing`/);
    assert.match(autonomous, /If an owner decision is needed, call `signal_done` with `owner_decision_requested`/);
    assert.doesNotMatch(autonomous, /Do not signal done again until every frozen milestone/);
  });
  for (const mode of ["legacy", "interactive", "task", "prompt", "ci_fix", "self_improve"] as const) {
    it(`issue3089 m1 Claude ignores unadvertised malformed request on ${mode}`, async () => {
      const { queryFn } = fakeTurns([[signalDone({ owner_decision_requested: null }), resultSuccess()]]);
      const probe = makeCompletionCtx();
      Object.assign(probe.ctx, { kind: mode === "legacy" || mode === "interactive" ? "issue" : mode,
        completionInterlock: mode !== "legacy", interactive: mode === "interactive",
        planApproved: true, approvedPlan: "plan", sessionId: "sess-1" });
      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      assert.equal(result.completionHeld, undefined);
      assert.deepEqual(probe.attemptCalls, []);
    });
  }
  for (const scenario of ["checkpoint cancel", "fingerprint cancel", "attempt cancel", "question cancel",
    "budget", "final iteration", "wall", "pause", "pause before budget", "question pause", "question budget", "pending milestone", "optional seams", "unwired"] as const) {
    it(`issue3089 m1 Claude boundary ${scenario}`, async () => {
      const { queryFn, turns } = fakeTurns([[signalDone({
        owner_decision_requested: { reason: "PRIVATE reason" },
      }), resultSuccess()]]);
      const probe = makeCompletionCtx({ unmetScript: [["m1"]],
        config: { max_iterations: scenario === "final iteration" ? 1 : 10 },
        askQuestionScript: [{ outcome: "expired" }] });
      Object.assign(probe.ctx, { kind: "issue", interactive: false, planApproved: true,
        approvedPlan: "plan", sessionId: "sess-1" });
      let cancelled = false;
      let asked = false;
      let paused = false;
      probe.ctx.cancelRequested = () => cancelled;
      const checkpoint = probe.ctx.checkpoint!;
      probe.ctx.checkpoint = async args => {
        await checkpoint(args);
        if (scenario === "checkpoint cancel") cancelled = true;
      };
      probe.ctx.worktreeFingerprint = async () => {
        if (scenario === "fingerprint cancel") cancelled = true;
        return "TIP";
      };
      const attempt = probe.ctx.recordCompletionAttempt!;
      probe.ctx.recordCompletionAttempt = async args => {
        const result = await attempt(args);
        if (scenario === "attempt cancel") cancelled = true;
        return result;
      };
      probe.ctx.reportIteration = async () => probe.attemptCalls.length ? {
        budgetExhausted: scenario === "budget" || scenario === "pause before budget" || (scenario === "question budget" && asked),
        pauseRequested: scenario === "pause" || scenario === "pause before budget" || (scenario === "question pause" && asked),
      } : undefined;
      probe.ctx.pauseModeRequested = () => scenario === "wall" && probe.attemptCalls.length ? "wall" : scenario === "question pause" && asked ? "now" : scenario === "pending milestone" && probe.attemptCalls.length ? "milestone" : null;
      probe.ctx.parkForPause = async () => {
        // SetRunPaused refuses awaiting_input; expiry must keep the completion hold.
        if (scenario === "question pause") return false;
        paused = true;
        return true;
      };
      const question = probe.ctx.askCompletionQuestion!;
      probe.ctx.askCompletionQuestion = async (unmet, trigger) => {
        asked = true;
        assert.deepEqual(trigger, { kind: "lead_request", reason: "PRIVATE reason" });
        if (scenario === "question cancel") cancelled = true;
        return scenario === "question budget" ? { outcome: "continue" } : question(unmet, trigger);
      };
      if (scenario === "unwired") delete probe.ctx.askCompletionQuestion;
      if (scenario === "optional seams") { delete probe.ctx.reportIteration; delete probe.ctx.checkpoint; delete probe.ctx.worktreeFingerprint; }
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
      if (scenario.includes("cancel")) {
        await assert.rejects(running, /run cancelled/);
        assert.deepEqual(probe.holdReasons, []);
        if (scenario === "checkpoint cancel" || scenario === "fingerprint cancel") assert.equal(probe.attemptCalls.length, 0);
      } else {
        const result = await running;
        assert.equal(turns.length, 1);
        if (scenario.includes("pause") && scenario !== "question pause") {
          assert.equal(paused, true);
          assert.ok(result.pausedAt);
          assert.deepEqual(probe.holdReasons, []);
        } else {
          if (scenario === "question pause") {
            assert.equal(paused, false);
            assert.equal(result.pausedAt, undefined);
          }
          assert.ok(result.completionHeld);
          assert.match(result.completionHeld.reason, scenario === "final iteration" ? /iteration/ :
            scenario === "wall" ? /wall/ : scenario.includes("budget") ? /budget exhausted/ : /lead requested/);
          assert.doesNotMatch(result.completionHeld.reason, /PRIVATE|repeated completion attempts/);
        }
        assert.equal(asked, scenario.startsWith("question") || scenario === "pending milestone" || scenario === "optional seams");
      }
    });
  }

  for (const satisfied of [false, true]) {
    it(`issue3089 Claude owner decision request ${satisfied ? "finalizes when satisfied" : "asks on the first attempt"}`, async () => {
      const input = { owner_decision_requested: { reason: "Owner wants to defer" } };
      const scripted = fakeTurns([[signalDone(input), resultSuccess()]]);
      const { turns } = scripted;
      const queryFn: SdkQueryFn = params => (async function* () {
        const rpc = await connectSignalRpc(params.options.mcpServers!.uzi);
        try {
          const admitted = await rpc.request("tools/call", { name: "signal_done", arguments: input });
          assert.equal(admitted.error, undefined);
          assert.notEqual((admitted.result as { isError?: boolean }).isError, true,
            "actual SDK handler accepts a reason-only completion request");
          yield* scripted.queryFn(params);
        } finally {
          await rpc.close();
        }
      })();
      const probe = makeCompletionCtx({
        unmetScript: [satisfied ? [] : ["m1"]],
        askQuestionScript: [{ outcome: "expired" }],
        // Baseline ignores the new field. Stop at the next ACK rather than
        // reaching three attempts (which would conceal the missing first window).
        budgetExhaustedFromIteration: 2,
      });
      Object.assign(probe.ctx, {
        kind: "issue", interactive: false, planApproved: true, approvedPlan: "plan", sessionId: "sess-1",
        frozenMilestones: [{ id: "m1", title: "Remaining work" }],
      });

      const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

      assert.deepStrictEqual(probe.askQuestionCalls, satisfied ? [] : [["m1"]],
        "an explicit owner decision request asks immediately iff authoritative unmet is nonempty");
      assert.deepStrictEqual(probe.order, satisfied
        ? ["checkpoint", "attempt"]
        : ["checkpoint", "attempt", "askQuestion", "hold"]);
      assert.strictEqual(probe.attemptCalls.length, 1, "no retry-to-stall before asking");
      assert.deepStrictEqual(probe.attemptCalls[0]!.declared, [],
        "requesting a decision does not declare the remaining milestone complete");
      assert.strictEqual(turns.length, 1, "no additional SDK query before the owner window");
      assert.strictEqual(Boolean(result.completionHeld), !satisfied);
    });
  }

  it("(a) checkpoints BEFORE the attempt, passing the declaration + head from the worktree fingerprint", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone({ milestones_completed: ["m1"] }), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [[]] }); // empty unmet → finalize after one attempt
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // The checkpoint capture runs BEFORE the attempt (capture-before-denial), and exactly once each.
    assert.deepStrictEqual(probe.order.slice(0, 2), ["checkpoint", "attempt"]);
    assert.strictEqual(probe.attemptCalls.length, 1);
    // head is the FIRST line of the worktree fingerprint; the fingerprint is passed whole.
    assert.strictEqual(probe.attemptCalls[0]!.head, "TIP");
    assert.strictEqual(probe.attemptCalls[0]!.worktreeFingerprint, "TIP\n M src/x.ts");
    assert.deepStrictEqual(probe.attemptCalls[0]!.declared, ["m1"]);
    // unmet empty → finalized (branch returned), no hold.
    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(result.completionHeld, undefined);
    assert.deepStrictEqual(probe.holdReasons, []);
  });

  it("(d) unmet empty on the first attempt proceeds straight to finalize", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [[]] });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(probe.attemptCalls.length, 1);
    assert.deepStrictEqual(probe.holdReasons, []);
  });

  it("(b) a decreasing unmet set re-prompts the SAME session (no hold, no publish) then finalizes when it empties", async () => {
    const { queryFn, turns } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [["m1", "m2"], ["m1"], []] });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(probe.attemptCalls.length, 3, "one attempt per signal_done until unmet empties");
    assert.deepStrictEqual(probe.holdReasons, [], "a shrinking unmet set never holds");
    assert.strictEqual(result.branch, "agent/issue-5", "empties → finalize");
    // The unmet ids were injected as a same-session follow-up (rendered into the next turn's prompt).
    assert.ok(turns.some((t) => (t.promptText ?? "").includes("m1")), "the unmet ids re-prompt the lead");
  });

  it("(c) three identical (unmet, head, worktree) attempts route to enterCompletionHold exactly once", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"]] }); // constant unmet every attempt
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(probe.attemptCalls.length, 3, "STALL_LIMIT identical attempts before the hold");
    assert.deepStrictEqual(probe.holdReasons.length, 1, "enterCompletionHold called exactly once");
    assert.match(probe.holdReasons[0]!, /completion blocked/);
    // A hold is neither a publish nor a plain fail: the executor returns with completionHeld set.
    assert.ok(result.completionHeld, "completionHeld latched so the runner skips finalization");
    assert.match(result.completionHeld!.reason, /completion blocked/);
  });

  it("(e) a POST-attempt MAX_ITERATIONS exhaustion routes to enterCompletionHold", async () => {
    // iter1 signal_done → attempt (unmet non-empty) → re-prompt; iter2 non-done work turn hits the
    // iteration cap (max=2), which — post-attempt — enters the hold instead of failing.
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      [assistantText("still working"), resultSuccess()],
    ]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"]], config: { max_iterations: 2 } });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(probe.holdReasons.length, 1, "post-attempt max-iter holds");
    assert.match(probe.holdReasons[0]!, /iteration budget/);
    assert.ok(result.completionHeld);
  });

  it("(e) a POST-attempt WALL trip routes to enterCompletionHold", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      (signal) => hangUntilAbort(signal), // wall trips on the re-prompt turn
    ]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]],
      config: { max_iterations: 10, idle_timeout_seconds: 100, run_timeout_seconds: 0.05 },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(probe.holdReasons.length, 1, "post-attempt wall holds");
    assert.match(probe.holdReasons[0]!, /wall-clock timeout/);
    assert.ok(result.completionHeld);
  });

  it("(e) a POST-attempt IDLE trip routes to enterCompletionHold", async () => {
    // Mirrors the WALL case but trips the IDLE timer instead (idle_timeout_seconds small,
    // run_timeout_seconds large so the wall never fires first). REASON_IDLE shares WALL's
    // post-attempt routing branch, so a bug isolated to REASON_IDLE — e.g. a wrong constant —
    // would slip past the WALL case; this pins the idle path + its reason string on its own.
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      (signal) => hangUntilAbort(signal), // idle trips on the re-prompt turn (no agent activity)
    ]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]],
      config: { max_iterations: 10, idle_timeout_seconds: 0.05, run_timeout_seconds: 100 },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(probe.attemptCalls.length, 1, "the idle trip is POST-attempt (>= 1 attempt)");
    assert.deepStrictEqual(probe.holdReasons.length, 1, "post-attempt idle holds");
    assert.match(probe.holdReasons[0]!, /no agent activity within the idle timeout/);
    assert.ok(result.completionHeld);
    assert.match(result.completionHeld!.reason, /no agent activity within the idle timeout/);
  });

  it("(e) a POST-attempt NO_PROGRESS stall routes to enterCompletionHold", async () => {
    // iter1 signal_done → attempt → re-prompt; then three identical refusals with an unchanged tree
    // trip the #281 detector, which — post-attempt — enters the hold instead of failing.
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      [assistantText("I decline this task."), resultSuccess()],
      [assistantText("I decline this task."), resultSuccess()],
      [assistantText("I decline this task."), resultSuccess()],
    ]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"]], config: { max_iterations: 20 } });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(probe.holdReasons.length, 1, "post-attempt no-progress holds");
    assert.match(probe.holdReasons[0]!, /no progress|made no progress/);
    assert.ok(result.completionHeld);
  });

  it("(f) a PRE-attempt WALL trip still throws the legacy terminal failure (no hold)", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], (signal) => hangUntilAbort(signal)]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]],
      config: { max_iterations: 10, idle_timeout_seconds: 100, run_timeout_seconds: 0.05 },
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /wall-clock timeout/);
    assert.strictEqual(probe.attemptCalls.length, 0, "no completion attempt happened before the wall trip");
    assert.deepStrictEqual(probe.holdReasons, [], "pre-attempt exhaustion must not enter the hold");
  });

  it("(f) a PRE-attempt IDLE trip still throws the legacy terminal failure (no hold)", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], (signal) => hangUntilAbort(signal)]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]],
      config: { max_iterations: 10, idle_timeout_seconds: 0.05, run_timeout_seconds: 100 },
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /no agent activity within the idle timeout/);
    assert.strictEqual(probe.attemptCalls.length, 0, "no completion attempt happened before the idle trip");
    assert.deepStrictEqual(probe.holdReasons, [], "pre-attempt exhaustion must not enter the hold");
  });

  it("(g) a NON-interlocked run finalizes on signal_done, never touching the completion seams", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ interlock: false, unmetScript: [["m1"]] });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(probe.attemptCalls.length, 0, "no attempt on a legacy run");
    assert.deepStrictEqual(probe.holdReasons, []);
    assert.strictEqual(result.completionHeld, undefined);
  });

  it("legacy fallback: with enterCompletionHold UNWIRED, three identical attempts throw (not hold)", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"]], wireHold: false });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /completion blocked/);
    assert.strictEqual(probe.attemptCalls.length, 3, "still bounded by STALL_LIMIT even without the seam");
  });

  it("(h) a FALSE enterCompletionHold return falls back to the legacy throw (the run is not held)", async () => {
    // PRD #1226 M4: the hold seam is WIRED but enterCompletionHold could not enter the hold
    // (capture unverified, or the ACK was not `paused`) and returned false. The three-identical-
    // attempt no-progress route must then throw the legacy REASON, NOT latch completionHeld.
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"]], holdReturns: false });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /completion blocked/);
    assert.strictEqual(probe.holdReasons.length, 1, "the hold WAS attempted (routeCompletionHold called it)");
    // The run is emphatically NOT marked held: a false return means keep-live-then-legacy-throw.
    assert.strictEqual(probe.attemptCalls.length, 3, "still bounded by STALL_LIMIT");
  });

  it("(i) a POST-attempt served budget_exhausted steer routes to the hold", async () => {
    // iter1: signal_done → attempt (unmet non-empty → completionAttempted=true, re-prompt). From
    // iter2 the server serves budgetExhausted:true; the loop-top budget steer — post-attempt on an
    // interlocked run — enters the hold with the budget reason, even though no WALL/IDLE tripped.
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      [assistantText("still working"), resultSuccess()],
    ]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]],
      config: { max_iterations: 20 },
      budgetExhaustedFromIteration: 2,
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(probe.holdReasons.length, 1, "the budget steer routed to the hold exactly once");
    assert.match(probe.holdReasons[0]!, /completion budget exhausted/);
    assert.ok(result.completionHeld, "completionHeld latched so the runner skips finalization");
    assert.match(result.completionHeld!.reason, /completion budget exhausted/);
  });

  it("(i) a PRE-attempt served budget_exhausted steer does NOT route (no completion attempt yet)", async () => {
    // The server serves budgetExhausted:true from iteration 1, BEFORE any completion attempt. The
    // steer is gated on completionAttempted, so it must NOT hold; the run proceeds to signal_done
    // and finalizes (empty unmet). This pins the completionAttempted gate on the budget steer.
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({
      unmetScript: [[]], // empty unmet → finalize on the first attempt
      config: { max_iterations: 20 },
      budgetExhaustedFromIteration: 1,
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepStrictEqual(probe.holdReasons, [], "a pre-attempt budget steer must not hold");
    assert.strictEqual(result.branch, "agent/issue-5", "the run finalized normally");
    assert.strictEqual(result.completionHeld, undefined);
  });

  // PRD #1226 M5 (D6): the completion-question LIVE window at the completion-STALL point ONLY. The
  // stall now asks the owner (askCompletionQuestion) BEFORE parking; a "continue" resumes the SAME
  // session, an "expired" routes to the M4 hold, and an UNWIRED seam keeps the M4 direct-park.
  it("(m5) a 'continue' at the stall resumes the SAME session with the guidance injected, resets the streak, and does NOT park", async () => {
    // Three identical no-progress attempts trip the stall; the owner answers 'continue' with
    // guidance. The streak resets so the SAME session runs a 4th attempt, which finds the contract
    // satisfied (unmet empties) and finalizes — proving the window RESUMED rather than parked, the
    // streak reset (a 4th attempt ran), and the owner guidance rode the rework follow-up.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"], ["m1"], ["m1"], []],
      askQuestionScript: [
        { outcome: "continue", guidance: "wire up the m1 handler and add a test" },
      ],
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(probe.askQuestionCalls.length, 1, "the stall asked the owner exactly once");
    assert.deepStrictEqual(probe.askQuestionCalls[0], ["m1"], "the unmet set was passed to the question");
    assert.strictEqual(probe.attemptCalls.length, 4, "the streak reset let a 4th attempt run in the SAME session");
    assert.deepStrictEqual(probe.holdReasons, [], "a continued run never parks");
    assert.strictEqual(result.branch, "agent/issue-5", "the resumed attempt emptied unmet → finalize");
    assert.strictEqual(result.completionHeld, undefined);
    assert.ok(
      turns.some((t) => (t.promptText ?? "").includes("wire up the m1 handler")),
      "the owner guidance was injected into the resumed completion-rework follow-up",
    );
  });

  it("(m5) an 'expired' window routes to enterCompletionHold (park) and never throws REASON_QUESTION_TIMEOUT", async () => {
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({
      unmetScript: [["m1"]], // constant unmet → the stall trips at STALL_LIMIT
      askQuestionScript: [{ outcome: "expired" }],
    });
    // The run RESOLVES (no reject): the expired window parks via enterCompletionHold and does NOT
    // surface REASON_QUESTION_TIMEOUT — this window has no fail-closed throw.
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(probe.askQuestionCalls.length, 1, "the stall asked the owner");
    assert.strictEqual(probe.attemptCalls.length, 3, "bounded by STALL_LIMIT; the expired window did not reset the streak");
    assert.strictEqual(probe.holdReasons.length, 1, "the expired window routed to enterCompletionHold exactly once");
    assert.match(probe.holdReasons[0]!, /completion blocked/);
    assert.ok(result.completionHeld, "the run parked (completionHeld latched)");
    assert.match(result.completionHeld!.reason, /completion blocked/);
  });

  it("(m5) with askCompletionQuestion UNWIRED the stall parks directly (M4 fallback, unchanged)", async () => {
    // No askQuestionScript ⇒ ctx.askCompletionQuestion is unset. The stall must route STRAIGHT to
    // enterCompletionHold at attempt 3, exactly as M4 behaved — no question, no streak reset.
    const { queryFn } = fakeTurns([[submitPlan("plan"), resultSuccess()], [signalDone(), resultSuccess()]]);
    const probe = makeCompletionCtx({ unmetScript: [["m1"]] });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.strictEqual(probe.askQuestionCalls.length, 0, "the unwired seam is never consulted");
    assert.strictEqual(probe.attemptCalls.length, 3, "parks at STALL_LIMIT with no extra attempts");
    assert.strictEqual(probe.holdReasons.length, 1, "routed straight to the hold");
    assert.ok(result.completionHeld);
    assert.match(result.completionHeld!.reason, /completion blocked/);
  });
});
