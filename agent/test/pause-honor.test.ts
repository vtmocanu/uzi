import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { PauseNowSignal } from "../src/steering.js";
import type { IterationBudget, Milestone } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// PRD #1190 M2 — the WORKER half of pause/resume: the loop-top pause branch (sdk-executor.ts).
// The SERVER decides the boundary and answers `pauseRequested` on the running-report ACK; the
// worker HONOURS the boolean, and — unlike the scope honor gate — the branch is NOT gated on
// isIssueRun, so a prompt run with no frozen milestones parks too. The actual park is delegated
// to ctx.parkForPause (the runner publishes the checkpoint and reports `paused`); on a park the
// loop latches ExecutorResult.pausedAt and breaks, else it CONTINUES the run.
//
// Same harness shape as scope-ceiling-honor.test.ts: `queryFn` is faked via fakeTurns([...]),
// so every path is provable with dummy credentials and NO live Anthropic session. `parkForPause`
// is a spy whose return value drives park-vs-continue.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

// A worktree path UNIQUE PER PROCESS AND PER CALL that deliberately never exists (see the long
// note in scope-ceiling-honor.test.ts — a per-file-unique basename is a race fix for the
// sibling skills plugin dir the executor materializes, since node --test runs files concurrently).
let nonexistentWorktreeSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-pause-honor-wt-${process.pid}-${nonexistentWorktreeSeq++}`);
}

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function submitPlanWithMilestones(plan: string, milestones: Milestone[], sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: {
      content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan, milestones } }],
    },
  } as unknown as SDKMessage;
}
function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }] },
  } as unknown as SDKMessage;
}
function signalDone(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

const THREE_MILESTONES: Milestone[] = [
  { id: "m1", title: "milestone one" },
  { id: "m2", title: "milestone two" },
  { id: "m3", title: "milestone three" },
];

interface Turn {
  options: SdkOptions;
  promptText?: string;
}

/** A fake `query` that replays one scripted stream per turn (invocation). */
function fakeTurns(scripts: SDKMessage[][]): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    const turn: Turn = { options: params.options };
    turns.push(turn);
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        turn.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      for (const m of script) yield m;
    })();
  };
  return { queryFn, turns };
}

interface Probe {
  ctx: RunContext;
  emits: EmittedMessage[];
  iterations: number[];
  parkCalls: { completedCount: number; total?: number }[];
}

function makeCtx(
  overrides: Partial<RunContext> = {},
  parkReturns: () => boolean = () => true,
  verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } },
): Probe {
  const emits: EmittedMessage[] = [];
  const iterations: number[] = [];
  const parkCalls: { completedCount: number; total?: number }[] = [];
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-5",
    emit: (m) => emits.push(m),
    oauthToken: OAUTH,
    agents: [],
    config: null,
    sessionId: null,
    gatePlan: async () => verdict,
    pullFollowUp: () => undefined,
    checkpoint: async () => {},
    reportIteration: async (n) => {
      iterations.push(n);
      return undefined;
    },
    // The runner's pause-park callback, faked: record the latch it was called with and return
    // park-vs-continue. Real parkForPause publishes the checkpoint and reports `paused`.
    parkForPause: async (at) => {
      parkCalls.push(at);
      return parkReturns();
    },
    ...overrides,
  };
  return { ctx, emits, iterations, parkCalls };
}

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-pausehome-"));
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

describe("PRD #1190 M2 — worker pause honor gate (loop-top)", () => {
  // The gate breaks with the pause latch when the ACK carries pauseRequested:true. The park is
  // delegated to ctx.parkForPause (true here → parked): the loop latches pausedAt and breaks
  // BEFORE any implement turn drives.
  it("parks at the loop-top boundary when the ACK carries pauseRequested:true", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      // iter 1: served pauseRequested:true → the branch fires at the loop top, no implement turn.
    ]);
    const budgets: Record<number, IterationBudget> = {
      1: { pauseRequested: true, completedCount: 2 },
    };
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return budgets[n];
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.pausedAt, "the run must park via the pause honor gate (pausedAt set)");
    assert.equal(result.pausedAt!.completedCount, 2, "the latch carries the server's completed count");
    assert.equal(result.pausedAt!.total, THREE_MILESTONES.length, "total is the frozen milestone list length");
    assert.deepEqual(probe.parkCalls, [{ completedCount: 2, total: 3 }], "parkForPause was invoked with the latch");
    // The branch fired at loop-top on iteration 1 BEFORE any implement turn: only the planning
    // turn drove, and reportIteration ran exactly once.
    assert.deepEqual(probe.iterations, [1], "reportIteration ran the honoring iteration only");
    assert.equal(turns.length, 1, "only the planning turn drove; NO implement turn ran");
    const ack = probe.emits.find((m) => m.kind === "steer_ack");
    assert.ok(ack, "a steer_ack is emitted at the pause boundary");
    assert.equal(ack!.payload["directive"], "pause", "the directive is `pause`");
    assert.equal(ack!.payload["completed"], 2, "the ack carries the completed count");
  });

  // Below/without a true boundary the branch is inert: the loop proceeds and completes on
  // signal_done, leaving pausedAt absent and parkForPause never called.
  it("does NOT park when the ACK carries pauseRequested:false", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), signalDone(), resultSuccess()], // loop iter 1 → done
    ]);
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return { pauseRequested: false, completedCount: 1 };
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.pausedAt, undefined, "a false boundary must NOT park the run");
    assert.equal(result.branch, "agent/issue-5", "the run completed via signal_done");
    assert.deepEqual(probe.parkCalls, [], "parkForPause is never called when pauseRequested is false");
    assert.ok(!probe.emits.some((m) => m.kind === "steer_ack"), "no steer_ack when the branch never fires");
  });

  // The isIssueRun guard's negative for pause: the scope gate is issue-run-only, but pause is
  // meaningful for prompt/task/self_improve too. A prompt run (isIssueRun false) with NO frozen
  // milestones must park on the first true ACK — proving the branch is NOT gated on isIssueRun.
  it("parks a prompt-kind run (no frozen milestones, isIssueRun false) on the first true ACK", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()], // planning turn (no milestones)
      // iter 1: served pauseRequested:true → parks at the loop top even on a non-issue run.
    ]);
    const probe = makeCtx({
      kind: "prompt",
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return { pauseRequested: true, completedCount: 0 };
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.pausedAt, "a prompt run MUST park — the pause branch is not gated on isIssueRun");
    assert.equal(result.pausedAt!.completedCount, 0, "no milestones completed on a prompt run");
    assert.equal(result.pausedAt!.total, undefined, "a run with no frozen milestones has no total");
    assert.deepEqual(probe.parkCalls, [{ completedCount: 0, total: undefined }], "parkForPause fired");
    assert.equal(turns.length, 1, "only the planning turn drove; the branch fired before any implement turn");
  });

  // parkForPause returning false (the runner could not publish the checkpoint, Decision 8) must
  // NOT terminate the run: the loop CONTINUES. Here iter 1 requests a pause that fails to park,
  // then iter 2 carries pauseRequested:false and the run completes normally with pausedAt absent.
  it("CONTINUES the run when parkForPause returns false (a failed checkpoint publish)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), resultSuccess()], // loop iter 1 (park declined → proceeds)
      [assistantText("iteration 2 work"), signalDone(), resultSuccess()], // loop iter 2 → done
    ]);
    const budgets: Record<number, IterationBudget> = {
      1: { pauseRequested: true, completedCount: 1 },
      2: { pauseRequested: false, completedCount: 1 },
    };
    const probe = makeCtx(
      {
        reportIteration: async (n) => {
          probe.iterations.push(n);
          return budgets[n];
        },
      },
      () => false, // parkForPause declines every park (publish failed)
    );
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.pausedAt, undefined, "a declined park must NOT set pausedAt");
    assert.equal(result.branch, "agent/issue-5", "the run continued and completed via signal_done");
    assert.equal(probe.parkCalls.length, 1, "parkForPause was attempted once (iteration 1)");
    assert.deepEqual(probe.iterations, [1, 2], "the loop continued to iteration 2 after the declined park");
    assert.equal(turns.length, 3, "the implement turn on iteration 1 DROVE after the park was declined");
  });

  // A `now` pause aborts the in-flight turn (PauseNowSignal from driveTurn). Simulated by aborting
  // ctx.signal WITH a PauseNowSignal from inside reportIteration (returning pauseRequested:false so
  // the loop-top milestone branch does NOT fire) — driveTurn's top trip guard then throws the
  // PauseNowSignal, which the loop's turn catch takes to the same park path.
  it("parks on a `now` pause abort (PauseNowSignal caught around driveTurn)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
    ]);
    const cancel = new AbortController();
    const probe = makeCtx({
      signal: cancel.signal,
      reportIteration: async (n) => {
        probe.iterations.push(n);
        // Simulate a live `now` pause landing right at the boundary: abort the shared controller
        // with a PauseNowSignal (what steering's route('pause','now') does). Return false so the
        // loop-top milestone branch does not fire — the abort drives the park via driveTurn's throw.
        if (n === 1) cancel.abort(new PauseNowSignal());
        return { pauseRequested: false, completedCount: 1 };
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.pausedAt, "a now-pause abort parks the run via the loop's turn catch");
    assert.equal(result.pausedAt!.completedCount, 1, "the latch carries the last-reported completed count");
    assert.equal(probe.parkCalls.length, 1, "parkForPause fired once from the turn catch");
    // driveTurn threw at its top trip guard (before the harness streamed), so queryFn — which
    // records a `turns` entry — was never reached for the implement turn: only the planning turn
    // produced output. This is exactly what proves the abort was taken pre-stream, as a `now`
    // pause must (nothing new was run past the last checkpoint).
    assert.equal(turns.length, 1, "the aborted implement turn threw before it streamed anything");
  });

  // A `now` pause whose park is DECLINED (publish failed) must restart the aborted turn: the loop
  // clears the sticky trip and continues, so a following clean iteration completes the run.
  it("restarts the aborted turn when a `now` park is declined, then completes", async () => {
    // Only TWO scripts: the aborted iter-1 turn throws at driveTurn's trip guard BEFORE calling
    // queryFn, so it consumes no script — the plan turn takes scripts[0] and the restarted turn
    // (iter 2) takes scripts[1].
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      [assistantText("restarted turn"), signalDone(), resultSuccess()], // iter 2: restart → done
    ]);
    const cancel = new AbortController();
    let abortedOnce = false;
    const probe = makeCtx(
      {
        signal: cancel.signal,
        reportIteration: async (n) => {
          probe.iterations.push(n);
          if (n === 1 && !abortedOnce) {
            abortedOnce = true;
            cancel.abort(new PauseNowSignal());
          }
          return { pauseRequested: false, completedCount: 0 };
        },
      },
      () => false, // decline the park (publish failed) → the loop must continue
    );
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.pausedAt, undefined, "a declined now-park must NOT park the run");
    assert.equal(result.branch, "agent/issue-5", "the restarted turn completed the run");
    assert.equal(probe.parkCalls.length, 1, "parkForPause was attempted once for the now pause");
    assert.deepEqual(probe.iterations, [1, 2], "the loop restarted at the next iteration after the declined park");
  });
});
