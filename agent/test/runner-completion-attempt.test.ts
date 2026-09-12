import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";

// PRD #1226 M3: the checkpoint-first same-lead completion attempt loop + post-attempt failure
// routing. The SDK boundary is faked (queryFn), and the completion seams
// (recordCompletionAttempt / enterCompletionHold / checkpoint / worktreeFingerprint) are injected
// mocks, so the loop's routing is provable with no network and no live SDK. signal_done is scripted
// as an MCP tool_use block the executor observes in the stream.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

let seq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-completion-wt-${process.pid}-${seq++}`);
}

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
    pullFollowUp: () => undefined,
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

describe("SdkExecutor completion interlock (PRD #1226 M3)", () => {
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
