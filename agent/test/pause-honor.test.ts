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

// A scripted turn is either a fixed message list or a function of the turn's abort signal (so a
// turn can HANG until the run's cancel/pause aborts it — a genuine in-flight abort), mirroring
// sdk-executor.test.ts.
type Script = SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>);

/** A fake `query` that replays one scripted stream per turn (invocation). */
function fakeTurns(scripts: Script[]): { queryFn: SdkQueryFn; turns: Turn[] } {
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
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turns };
}

/** A turn that never yields until its signal aborts — a genuinely in-flight turn (same shape and
 *  reason as sdk-executor.test.ts's copy). */
function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    // eslint-disable-next-line require-yield
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener(
          "abort",
          () => {
            clearInterval(keepAlive);
            resolve();
          },
          { once: true },
        );
      });
    },
  };
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
  it("a now pause during empty-turn backoff uses the pause park", async () => {
    const cancel = new AbortController();
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      // The timer fires only after the empty turn yields back to the event loop,
      // while the recovery wrapper is waiting between retries.
      async function* () {
        setTimeout(() => cancel.abort(new PauseNowSignal()), 0);
        yield {
          type: "result", subtype: "success", is_error: false,
          num_turns: 0, session_id: "sess-1",
        } as unknown as SDKMessage;
      },
    ]);
    const probe = makeCtx({ signal: cancel.signal });
    const result = await new SdkExecutor(nullLogger(), homeDir, {
      queryFn,
      emptyTurnBackoffBaseMs: 100,
      emptyTurnMaxRetries: 2,
    }).run(probe.ctx);
    assert.equal(probe.parkCalls.length, 1, "the typed pause reaches parkForPause");
    assert.ok(result.pausedAt, "the run pauses instead of failing or recovery-parking");
    assert.equal(turns.length, 2, "no SDK retry starts after the pause request");
  });

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

// PRD #1190 rework — the two review findings against the M2 worker pause code:
//   N2: a cancel that arrives AFTER a declined `now`-park was dropped (the shared abort controller
//       was already spent), and a SECOND `now` pause silently degraded to a milestone-boundary park.
//   N1: the seeded pause mode was write-only; now the executor reads it as a first-boundary,
//       ACK-independent park fallback.
describe("PRD #1190 rework — cancel re-check, now-pause re-arm, and the seed fallback", () => {
  // N2 headline: a cancel arriving after a declined `now`-park is HONORED at the next loop-top.
  // Before the rework the `now` pause left the shared controller aborted with its once-listener
  // spent, so a subsequent cancel could neither re-fire the abort nor was re-checked — the run
  // ignored the cancel until it self-completed.
  it("honors a cancel that lands after a declined now-park (loop-top cancel re-check)", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      // iter 1: a `now` pause aborts the turn; the park is declined; a cancel arrives during the
      // declined park; iter 2's loop-top re-check must throw to the terminal cancel path.
      [assistantText("iter 2 would run if the cancel were dropped"), signalDone(), resultSuccess()],
    ]);
    const cancel = new AbortController();
    let cancelPending = false;
    const probe = makeCtx(
      {
        signal: cancel.signal,
        // The runner wires this to steering.isCancelled(); the sticky flag survives the spent
        // controller, which is the whole point of re-reading it here.
        cancelRequested: () => cancelPending,
        reportIteration: async (n) => {
          probe.iterations.push(n);
          if (n === 1) cancel.abort(new PauseNowSignal()); // a `now` pause drops iter-1's turn
          return { pauseRequested: false, completedCount: 0 };
        },
      },
      () => {
        // parkForPause declines (publish failed) AND a cancel lands right now (the owner cancels a
        // run that could not pause). The steering channel's sticky `cancelled` flag is now set.
        cancelPending = true;
        return false;
      },
    );
    await assert.rejects(
      () => new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /run cancelled/,
      "the cancel is honored at the next loop-top rather than being ignored",
    );
    // iter 1 attempted (and was declined) the now-park, then `continue`d to iter 2, whose loop-top
    // cancel re-check threw BEFORE reportIteration — so reportIteration ran once (iter 1 only) yet
    // the run still rejected with the cancel error, which is only reachable via the iter-2 re-check.
    assert.equal(probe.parkCalls.length, 1, "the declined now-park happened at iteration 1");
    assert.deepEqual(probe.iterations, [1], "iter 2's cancel re-check short-circuited before its reportIteration");
    // If the loop-top re-check were removed, iter 2 would drive scripts[1] and complete via
    // signal_done instead of rejecting — so this assertion is non-vacuous.
  });

  // N2 no-regression: a NORMAL in-flight cancel (aborting a live turn) still fails the run via the
  // turn's own abort, EVEN with cancelRequested wired — the loop-top re-check is a backstop, not a
  // replacement, and must not change the in-flight path.
  it("a normal in-flight cancel still aborts the live turn (loop-top re-check does not break it)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      (signal) => hangUntilAbort(signal), // iter 1: a genuinely in-flight turn, aborted mid-stream
    ]);
    const cancel = new AbortController();
    const probe = makeCtx({
      signal: cancel.signal,
      cancelRequested: () => cancel.signal.aborted, // realistic: a cancel both aborts AND sticks
      reportIteration: async (n) => {
        probe.iterations.push(n);
        // Abort the LIVE turn shortly after it starts hanging — a real steering cancel mid-turn.
        if (n === 1) setTimeout(() => cancel.abort(), 10);
        return { pauseRequested: false, completedCount: 0 };
      },
    });
    await assert.rejects(
      () => new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /run cancelled/,
      "an in-flight cancel fails the run via the turn abort",
    );
    // The iter-1 implement turn DID start streaming (it hung), so queryFn recorded it — proving the
    // cancel aborted a live turn, not the loop-top backstop.
    assert.equal(turns.length, 2, "the planning turn and the in-flight implement turn both drove");
    assert.deepEqual(probe.iterations, [1], "the run failed at iteration 1's live turn, not a later loop-top");
  });

  // N2 re-arm: a SECOND `now` pause still drops the (restarted) turn, delivered via the re-armable
  // ctx.onPauseNow interrupt. The shared controller is already spent by the first `now`, so the OLD
  // code degraded the second `now` to a milestone-boundary park; the interrupt re-arms the drop.
  it("re-arms: a SECOND now pause (via onPauseNow) drops the restarted turn after a declined park", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      // iter 1: first `now` via ctx.signal → declined → restart.
      // iter 2: second `now` via onPauseNow (ctx.signal already aborted) → declined → restart.
      [assistantText("final turn"), signalDone(), resultSuccess()], // iter 3 → done
    ]);
    const cancel = new AbortController();
    let pauseNowCb: (() => void) | undefined;
    let parkCount = 0;
    const probe = makeCtx(
      {
        signal: cancel.signal,
        onPauseNow: (cb) => {
          pauseNowCb = cb;
        },
        reportIteration: async (n) => {
          probe.iterations.push(n);
          if (n === 1) cancel.abort(new PauseNowSignal()); // first now-pause (shared controller)
          // Second now-pause: the controller is already aborted and its once-listener spent, so the
          // ONLY thing that can drop the turn is the re-armable interrupt.
          if (n === 2) pauseNowCb?.();
          return { pauseRequested: false, completedCount: 0 };
        },
      },
      () => {
        parkCount++;
        return false; // decline both parks so the loop continues to iteration 3
      },
    );
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(parkCount, 2, "BOTH now pauses attempted a park — the second re-armed via onPauseNow");
    assert.equal(result.branch, "agent/issue-5", "the run completed after both declined now-parks");
    assert.equal(result.pausedAt, undefined, "neither park took (both declined)");
    assert.deepEqual(probe.iterations, [1, 2, 3], "the loop restarted after each declined now-park");
    // If ctx.onPauseNow were not registered, iter 2 would drive scripts[1] and complete — parkCount
    // would be 1 — so this proves the re-arm wiring is load-bearing.
  });

  // N1: the seeded/steered pause mode is now READ by the executor at its first boundary as an
  // ACK-independent fallback. Here the ACK carries pauseRequested:FALSE (simulating an older/buggy
  // server), yet the run parks purely because ctx.pauseModeRequested reports a pending mode.
  it("parks at the first boundary from the seeded pause mode even when the ACK regresses (N1 fallback)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      // iter 1: ACK says pauseRequested:false, but the seeded mode makes the loop park anyway.
    ]);
    const probe = makeCtx({
      // The runner wires this to steering.getPauseMode(); "now" here stands in for a resume that
      // seeded the mode from claim.pause_pending/pause_mode.
      pauseModeRequested: () => "now",
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return { pauseRequested: false, completedCount: 2 }; // the ACK does NOT request a pause
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.pausedAt, "the seeded pause mode parks the run at its first boundary via the fallback");
    assert.equal(result.pausedAt!.completedCount, 2, "the latch carries the server's completed count");
    assert.deepEqual(probe.parkCalls, [{ completedCount: 2, total: 3 }], "parkForPause fired from the seed fallback");
    assert.equal(turns.length, 1, "only the planning turn drove; the fallback fired before any implement turn");
    // Deleting the ctx.pauseModeRequested read (or the seed wiring) drops the only park trigger
    // here (the ACK is false), so the run would complete instead of park — this is non-vacuous.
  });

  // N1 one-shot: the seed fallback fires at the FIRST boundary only. If that first park is declined,
  // the run continues and does NOT re-attempt a park every iteration off the sticky seeded mode
  // (the server ACK is authoritative thereafter).
  it("the seed fallback is one-shot: a declined first park does not re-park every iteration", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", THREE_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iter 1 work"), resultSuccess()], // iter 1: seed park declined → proceeds
      [assistantText("iter 2 work"), signalDone(), resultSuccess()], // iter 2: no re-park → done
    ]);
    const probe = makeCtx(
      {
        pauseModeRequested: () => "milestone", // stays set across the run (sticky), as in production
        reportIteration: async (n) => {
          probe.iterations.push(n);
          return { pauseRequested: false, completedCount: 0 }; // ACK never requests a pause
        },
      },
      () => false, // decline the (single) seed-fallback park
    );
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.pausedAt, undefined, "the declined seed park did not park the run");
    assert.equal(result.branch, "agent/issue-5", "the run continued and completed");
    assert.equal(probe.parkCalls.length, 1, "the seed fallback attempted a park exactly ONCE (iteration 1)");
    assert.deepEqual(probe.iterations, [1, 2], "the loop proceeded past the declined first-boundary park");
  });
});
