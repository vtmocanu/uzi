import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import {
  type Executor,
  type FollowUpOutcome,
  type RunContext,
} from "../src/executor.js";
import { REASON_FOLLOWUP_NOT_PARKED } from "../src/runner.js";
import type { PlanVerdict } from "../src/steering.js";
import { SteeringChannel } from "../src/steering.js";
import type { ClaimResponse, UserInput } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import { makeClaim, nullLogger } from "./helpers.js";
import {
  api,
  fx,
  fakeGitlab,
  installHarness,
  runner,
} from "./runner-harness.js";

/**
 * PRD #517 M3 — interactive task runs park on `signal_done` and resume on a follow-up.
 *
 * Three layers, each tested where the mechanism actually lives (the ask_user precedent,
 * runner-ask-user.test.ts, established this split: a SteeringChannel test cannot observe
 * its caller, and a runner test cannot observe the SDK session):
 *
 *   - SteeringChannel.awaitFollowUp — drain-after-arm + the idle clock (steering.ts).
 *   - The SdkExecutor loop park — checkpoint→park→fold/continue-or-break, budget reset
 *     (sdk-executor.ts).
 *   - RunRunner's awaitFollowUp callback — the awaiting_followup report + ack + the
 *     consume-before-report ordering (runner.ts).
 *
 * Every test names the call-site mutation that reddens it.
 */

// ── scripted SDK messages (mirrors sdk-executor.test.ts) ─────────────────────
function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "text", text }] },
  } as unknown as SDKMessage;
}
function submitPlan(plan: string, sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: {
      content: [
        { type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } },
      ],
    },
  } as unknown as SDKMessage;
}
function signalDone(sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: {
      content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input: {} }],
    },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: false,
    num_turns: 1,
    session_id: sessionId,
  } as unknown as SDKMessage;
}

interface Turn {
  options: SdkOptions;
  promptText?: string;
}

/** A fake `query` that replays one scripted stream per turn (invocation), recording the
 *  resume id and prompt each turn saw (mirrors sdk-executor.test.ts's fakeTurns). */
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

/** Like fakeTurns, but each turn's stream burns `sleepMs` of REAL wall-clock time in-turn
 *  (between consuming the prompt and yielding its messages). driveTurn arms the wall budget
 *  around this window (armWall→…→disarmWall), so the sleep is what disarmWall debits — the
 *  only way to exercise the wall-clock trip with scripted turns, which are otherwise instant.
 *  Used by the per-follow-up wall-reset test. */
function fakeTurnsSlow(
  scripts: SDKMessage[][],
  sleepMs: number,
): { queryFn: SdkQueryFn; turns: Turn[] } {
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
      await new Promise((r) => setTimeout(r, sleepMs)); // debited from the wall budget
      for (const m of script) yield m;
    })();
  };
  return { queryFn, turns };
}

/** Like fakeTurnsSlow, but the in-turn sleep is PER-TURN (`sleeps[i]` for turn i, clamped
 *  to the last entry). Lets a test keep the plan turn cheap while making one specific
 *  resumed follow-up turn burn a chosen amount — needed to isolate the wall-scaling latch,
 *  whose effect shows only on a resumed turn and would otherwise be masked by the plan turn
 *  sharing the same unscaled `initialWallMs` budget under a uniform sleep. */
function fakeTurnsSlowPerTurn(
  scripts: SDKMessage[][],
  sleeps: number[],
): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    const sleepMs = sleeps[Math.min(i, sleeps.length - 1)]!;
    i++;
    const turn: Turn = { options: params.options };
    turns.push(turn);
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        turn.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      await new Promise((r) => setTimeout(r, sleepMs)); // debited from the wall budget
      for (const m of script) yield m;
    })();
  };
  return { queryFn, turns };
}

// ── the SdkExecutor loop park ────────────────────────────────────────────────
describe("SdkExecutor interactive task park (PRD #517 M3)", () => {
  let sdkHome: string;
  let saved: Record<string, string | undefined>;

  beforeEach(() => {
    sdkHome = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-parkhome-"));
    saved = {
      UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN,
      UZI_FORGE_PAT: process.env.UZI_FORGE_PAT,
    };
    process.env.UZI_WORKER_TOKEN = "dummy-join-token-do-not-scan-9999";
    process.env.UZI_FORGE_PAT = "dummy-forge-pat-do-not-scan-8888";
  });
  afterEach(() => {
    fs.rmSync(sdkHome, { recursive: true, force: true });
    for (const [k, v] of Object.entries(saved)) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  });

  // A worktree path that never exists — the executor must not require it on disk, and a
  // unique-per-call basename avoids the skills-plugin-dir race sdk-executor.test.ts notes.
  let wtSeq = 0;
  function nonexistentWorktree(): string {
    return path.join(os.tmpdir(), `uzi-park-wt-${process.pid}-${wtSeq++}`);
  }

  function makeCtx(overrides: Partial<RunContext> = {}): {
    ctx: RunContext;
    iterations: number[];
  } {
    const iterations: number[] = [];
    const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
    const ctx: RunContext = {
      runId: "r1",
      issueIid: 5,
      issueTitle: "Refactor the poller",
      issueDescription: "interactive task",
      worktreePath: nonexistentWorktree(),
      branch: "agent/issue-5",
      emit: () => {},
      oauthToken: "dummy-oauth-token-do-not-scan-0000",
      agents: [],
      config: null,
      sessionId: null,
      onSessionId: () => {},
      gatePlan: async () => approve,
      pullFollowUp: () => undefined,
      reportIteration: (n) => {
        iterations.push(n);
      },
      ...overrides,
    };
    return { ctx, iterations };
  }

  it("parks on done, folds the follow-up into a second turn resuming the same session, then re-parks", async () => {
    // Mutation A: drop the `await ctx.checkpoint?.({reap:true})` before the park →
    // `checkpoints` is empty. Mutation B: `break` instead of `continue` after folding →
    // turns.length is 2 and no second turn runs. Mutation C: set `followUp` but do not
    // reset iteration / do not fold → turns[2] omits the follow-up text.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()], // planning
      [assistantText("t1"), signalDone(), resultSuccess()], // loop 1 → done → park
      [assistantText("t2"), signalDone(), resultSuccess()], // loop 2 (after follow-up) → done → park (idle)
    ]);
    const outcomes: FollowUpOutcome[] = [
      { kind: "followup", body: "now add tests" },
      { kind: "ended", reason: "idle" },
    ];
    const events: string[] = [];
    const checkpoints: Array<{ reap: boolean }> = [];
    const parkIdle: number[] = [];
    const { ctx } = makeCtx({
      interactive: true,
      checkpoint: async (opts) => {
        checkpoints.push({ reap: opts.reap });
        events.push("checkpoint");
      },
      awaitFollowUp: async (idleMs) => {
        parkIdle.push(idleMs);
        events.push("park");
        return outcomes.shift()!;
      },
    });

    const result = await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);

    assert.strictEqual(result.branch, "agent/issue-5");
    // The run parked after EACH signal_done, and each park was preceded by a reap:true
    // checkpoint-push (the "awaiting_followup report happens" in production is what
    // awaitFollowUp does — see the RunRunner test below).
    assert.strictEqual(parkIdle.length, 2, "parked after each signal_done");
    assert.deepStrictEqual(
      checkpoints,
      [{ reap: true }, { reap: true }],
      "each park checkpoint-pushes with reap:true",
    );
    assert.deepStrictEqual(
      events,
      ["checkpoint", "park", "checkpoint", "park"],
      "the checkpoint-push strictly precedes the park at every boundary",
    );
    // A SECOND loop turn ran (plan + 2 loop turns), resuming the SAME SDK session.
    assert.strictEqual(turns.length, 3, "a second loop turn ran after the follow-up");
    assert.strictEqual(turns[1]!.options.resume, "sess-1");
    assert.strictEqual(
      turns[2]!.options.resume,
      "sess-1",
      "the follow-up turn resumed the same session, not a fresh one",
    );
    // The follow-up was folded into the second turn as untrusted user input; the first
    // loop turn did NOT carry it.
    assert.match(turns[2]!.promptText ?? "", /now add tests/);
    assert.match(turns[2]!.promptText ?? "", /UNTRUSTED INPUT/);
    assert.doesNotMatch(turns[1]!.promptText ?? "", /now add tests/);
  });

  it("resets the iteration budget for each follow-up so a long session does not exhaust it", async () => {
    // Budget of 2. The first task signals done at iteration 1 and parks. The follow-up's
    // task then takes TWO turns (one working, one done). WITHOUT the per-follow-up reset
    // (`iteration = 0`) the loop carries iteration=1 across the park and trips
    // REASON_MAX_ITERATIONS on the follow-up's second turn; WITH the reset each follow-up
    // gets a fresh budget and the run completes. Deleting `iteration = 0` reddens this.
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()], // planning
      [assistantText("t1"), signalDone(), resultSuccess()], // loop 1 → done → park
      [assistantText("f working"), resultSuccess()], // follow-up loop 1 (NOT done)
      [assistantText("f done"), signalDone(), resultSuccess()], // follow-up loop 2 → done → park (idle)
    ]);
    const outcomes: FollowUpOutcome[] = [
      { kind: "followup", body: "a bigger task" },
      { kind: "ended", reason: "idle" },
    ];
    const { ctx } = makeCtx({
      config: { max_iterations: 2 },
      interactive: true,
      checkpoint: async () => {},
      awaitFollowUp: async () => outcomes.shift()!,
    });

    const result = await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);
    assert.strictEqual(
      result.branch,
      "agent/issue-5",
      "each follow-up gets a fresh iteration budget",
    );
  });

  it("throws the cancel signal (not a normal finalize) when a parked run is cancelled", async () => {
    // Fix 1: a cancelled parked run is still awaiting_followup server-side; finalizing it as
    // `completed` would push its branch and open an MR. It must instead reach the terminal
    // CANCEL path — the executor throws the same cancel signal the plan gate / clarification
    // park use, which the runner maps to `failed` (server routes stop_kind='cancelled' to
    // CancelRunByWorker). Mutation: revert Fix 1 so `{ended, reason:"cancelled"}` shares idle's
    // `break` → run() RESOLVES normally (returns the branch) instead of throwing, reddening the
    // assert.rejects below.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("t1"), signalDone(), resultSuccess()], // loop 1 → done → park → cancelled
    ]);
    let checkpointCalls = 0;
    const { ctx } = makeCtx({
      interactive: true,
      checkpoint: async () => {
        checkpointCalls++;
      },
      awaitFollowUp: async () => ({ kind: "ended", reason: "cancelled" }),
    });

    await assert.rejects(
      () => new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx),
      /cancel/i,
      "a cancelled park must throw the cancel signal, not return a completed result",
    );
    // The run DID park (checkpoint-pushed) before the cancel, and no follow-up turn ran after.
    assert.strictEqual(checkpointCalls, 1, "the run parked (checkpoint-pushed) before the cancel");
    assert.strictEqual(turns.length, 2, "plan + one loop turn; no turn ran after the cancel");
  });

  it("finalizes gracefully (completed, no throw) when a parked run is stopped", async () => {
    // PRD #517 M4: a graceful `stop` is a CLEAN completion, not an abort. `{ended,
    // reason:"stopped"}` shares the idle break — push + open MR iff open_mr → report
    // `completed` — and must NOT throw the cancel signal (unlike `cancelled`). The server
    // pre-stamped stop_kind='stopped' and SetRunCompleted preserves it, so the run lands
    // completed-with-stopped and fires --review iff requested. Mutation: route `stopped`
    // to the cancel-signal throw (as `cancelled` does) → run() REJECTS instead of returning
    // the branch, reddening the assert below.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("t1"), signalDone(), resultSuccess()], // loop 1 → done → park → stopped
    ]);
    let checkpointCalls = 0;
    const { ctx } = makeCtx({
      interactive: true,
      checkpoint: async () => {
        checkpointCalls++;
      },
      awaitFollowUp: async () => ({ kind: "ended", reason: "stopped" }),
    });

    const result = await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);
    assert.strictEqual(
      result.branch,
      "agent/issue-5",
      "a stopped park finalizes as a completed result (push + MR), never throws",
    );
    // The run parked (checkpoint-pushed) before the stop, and no follow-up turn ran after.
    assert.strictEqual(checkpointCalls, 1, "the run parked (checkpoint-pushed) before the stop");
    assert.strictEqual(turns.length, 2, "plan + one loop turn; no turn ran after the stop");
  });

  it("resets the WALL budget for each follow-up so summed in-turn time does not trip REASON_WALL", async () => {
    // Fix 2: each follow-up is a fresh task with a fresh wall budget (bounded per-follow-up,
    // not per-conversation). Budget is 300ms and every turn burns 60ms of real in-turn time.
    // WITH the reset each of the many follow-ups re-arms at the full 300ms and none trips (a
    // single 60ms turn against 300ms is a 5x margin). WITHOUT it (delete
    // `state.wallRemainingMs = initialWallMs`) the 60ms debits accumulate across plan + 7 turns
    // = 480ms > 300ms and armWall trips REASON_WALL mid-session, so run() rejects and the
    // turns.length assert below (all 8 turns ran) reddens. max_iterations is high so only the
    // wall — not the iteration budget — is under test.
    const { queryFn, turns } = fakeTurnsSlow(
      [
        [submitPlan("plan"), resultSuccess()], // planning
        [assistantText("work"), signalDone(), resultSuccess()], // every loop turn: done → park
      ],
      60,
    );
    // park #1 (loop1) … #6 return a follow-up; park #7 ends the session idle.
    const outcomes: FollowUpOutcome[] = [
      { kind: "followup", body: "task 2" },
      { kind: "followup", body: "task 3" },
      { kind: "followup", body: "task 4" },
      { kind: "followup", body: "task 5" },
      { kind: "followup", body: "task 6" },
      { kind: "followup", body: "task 7" },
      { kind: "ended", reason: "idle" },
    ];
    const { ctx } = makeCtx({
      config: { max_iterations: 30, run_timeout_seconds: 0.3 },
      interactive: true,
      checkpoint: async () => {},
      awaitFollowUp: async () => outcomes.shift()!,
    });

    const result = await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "the long interactive session did not trip the wall");
    // plan + loop1 + 6 follow-up turns = 8 turns all ran, proving the wall never tripped
    // (a REASON_WALL trip would have rejected the run before the later turns).
    assert.strictEqual(turns.length, 8, "every follow-up turn ran on its own fresh wall budget");
  });

  it("re-applies the server's wall-budget scaling on a resumed follow-up (re-arms the wallScaled latch)", async () => {
    // Fix 4 (this M4): a follow-up is a fresh task, so the applied-once `wallScaled` latch must
    // be RE-ARMED at the follow-up reset (`wallScaled = false` beside `state.wallRemainingMs =
    // initialWallMs`). Otherwise the latch stays `true` from the first turn and the resumed
    // follow-up's `served.wallSeconds > initialWallMs` scaling (~sdk-executor.ts:1462) is
    // SKIPPED, leaving the resumed turn on the unscaled default wall.
    //
    // Setup: initialWallMs = 300ms; every reportIteration serves wallSeconds = 0.9 (900ms), so
    // the applied-once scaling adds 900-300 = 600ms → a scaled budget of 900ms. Per-turn sleeps
    // isolate the effect: the plan turn burns ~0 (its budget is the unscaled 300ms), turn 1
    // burns 50ms (comfortably inside its scaled 900ms), and the RESUMED turn burns 450ms.
    //   WITH the re-arm: the resumed turn re-scales to 900ms → 450ms burn is a 2x margin, the
    //   turn completes, the run parks again and ends idle → run() resolves.
    //   WITHOUT it (delete `wallScaled = false`): the latch is still set, the resumed turn is
    //   NOT re-scaled and keeps the reset 300ms budget → the 450ms burn trips REASON_WALL
    //   mid-turn and run() REJECTS. (The plan turn does NOT falsely trip: its budget is the
    //   same unscaled 300ms but it burns ~0, which is why the sleep must be per-turn — a
    //   uniform sleep would trip the plan turn identically and stop discriminating.)
    const { queryFn, turns } = fakeTurnsSlowPerTurn(
      [
        [submitPlan("plan"), resultSuccess()], // planning (unscaled 300ms budget, ~0 burn)
        [assistantText("t1"), signalDone(), resultSuccess()], // loop1 → done → park
        [assistantText("t2"), signalDone(), resultSuccess()], // resumed follow-up → done → park (idle)
      ],
      [0, 50, 450],
    );
    const outcomes: FollowUpOutcome[] = [
      { kind: "followup", body: "the resumed task" },
      { kind: "ended", reason: "idle" },
    ];
    const { ctx } = makeCtx({
      config: { max_iterations: 30, run_timeout_seconds: 0.3 },
      interactive: true,
      checkpoint: async () => {},
      awaitFollowUp: async () => outcomes.shift()!,
      // Serve a wall budget larger than initialWallMs on every iteration report so the
      // applied-once scaling has a delta to add — and, on the resumed turn, re-add.
      reportIteration: async () => ({ wallSeconds: 0.9 }),
    });

    const result = await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);
    assert.strictEqual(
      result.branch,
      "agent/issue-5",
      "the resumed follow-up re-applied the server's wall scaling and did not trip the wall",
    );
    // plan + loop1 + resumed follow-up turn = 3 turns; the resumed turn completing (rather than
    // the run rejecting mid-turn) is what proves the re-scaling fired.
    assert.strictEqual(turns.length, 3, "the resumed follow-up turn ran to completion on a re-scaled wall");
  });

  it("emits first-turn scaffolding only on the genuine first turn, never on a resumed follow-up", async () => {
    // Fix 3: the "plan approved" framing and the base-commit note are first-turn-ONLY, keyed on
    // the whole run's first turn (hasParked), NOT the per-follow-up iteration counter. Mutation:
    // revert Fix 3 (`first: iteration === 1`) → after `iteration = 0` the resumed turn is
    // iteration 1 again, so turn 2 re-emits "Your plan was approved" + the ORIGINAL base commit,
    // reddening the doesNotMatch asserts. The follow-up body must still appear either way.
    const baseCommit = "a".repeat(40);
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("t1"), signalDone(), resultSuccess()], // loop 1 (genuine first turn) → park
      [assistantText("t2"), signalDone(), resultSuccess()], // loop 2 (resumed follow-up) → park idle
    ]);
    const outcomes: FollowUpOutcome[] = [
      { kind: "followup", body: "please also update the docs" },
      { kind: "ended", reason: "idle" },
    ];
    const { ctx } = makeCtx({
      interactive: true,
      baseCommit,
      checkpoint: async () => {},
      awaitFollowUp: async () => outcomes.shift()!,
    });

    await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);

    const firstTurn = turns[1]!.promptText ?? "";
    const resumeTurn = turns[2]!.promptText ?? "";
    // The genuine first turn carries the first-turn-only scaffolding.
    assert.match(firstTurn, /Your plan was approved/, "first turn has the plan-approved framing");
    assert.match(firstTurn, /Your branch was created at commit/, "first turn has the base-commit note");
    // The resumed follow-up turn suppresses BOTH.
    assert.doesNotMatch(resumeTurn, /Your plan was approved/, "resume must not re-frame as newly approved");
    assert.doesNotMatch(resumeTurn, /Your branch was created at commit/, "resume must not re-inject the base commit");
    // But the follow-up body itself still appears, untrusted-fenced.
    assert.match(resumeTurn, /please also update the docs/, "the follow-up body still appears");
    assert.match(resumeTurn, /UNTRUSTED INPUT/, "and is rendered as untrusted input");
  });

  it("parks on the claim-configured task_idle_timeout_seconds, falling back to the constant when absent", async () => {
    // PRD #517 M5: the park's idle bound is server-configured on the claim
    // (config.task_idle_timeout_seconds), not the bare TASK_FOLLOWUP_IDLE_MS constant.
    // Mutation: revert the park to `awaitFollowUp(TASK_FOLLOWUP_IDLE_MS)` → the configured
    // 120s no longer reaches the park and parkIdle[0] reads 1_800_000 (the constant),
    // reddening the deepStrictEqual([120_000]) assert.
    const withConfig = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("t1"), signalDone(), resultSuccess()], // → done → park (idle)
    ]);
    const parkIdle: number[] = [];
    const { ctx } = makeCtx({
      interactive: true,
      config: { task_idle_timeout_seconds: 120 },
      checkpoint: async () => {},
      awaitFollowUp: async (idleMs) => {
        parkIdle.push(idleMs);
        return { kind: "ended", reason: "idle" };
      },
    });
    await new SdkExecutor(nullLogger(), sdkHome, { queryFn: withConfig.queryFn }).run(ctx);
    assert.deepStrictEqual(
      parkIdle,
      [120_000],
      "the park used config.task_idle_timeout_seconds * 1000",
    );

    // A missing field falls back to TASK_FOLLOWUP_IDLE_MS (30m = 1_800_000ms). config:null
    // exercises the ctx.config?.task_idle_timeout_seconds optional-chain → seconds() fallback.
    // This also guards the unit: seconds()'s fallback is in SECONDS, so the ms constant is
    // passed ÷1000 — a `seconds(..., TASK_FOLLOWUP_IDLE_MS)` bug would read 1_800_000_000 here.
    const noConfig = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("t1"), signalDone(), resultSuccess()],
    ]);
    const parkIdleFallback: number[] = [];
    const { ctx: ctx2 } = makeCtx({
      interactive: true,
      config: null,
      checkpoint: async () => {},
      awaitFollowUp: async (idleMs) => {
        parkIdleFallback.push(idleMs);
        return { kind: "ended", reason: "idle" };
      },
    });
    await new SdkExecutor(nullLogger(), sdkHome, { queryFn: noConfig.queryFn }).run(ctx2);
    assert.deepStrictEqual(
      parkIdleFallback,
      [30 * 60 * 1000],
      "a missing task_idle_timeout_seconds falls back to TASK_FOLLOWUP_IDLE_MS (30m)",
    );
  });

  it("a non-interactive run finalizes on done without parking or checkpoint-parking", async () => {
    // Byte-identical-to-today control. Mutation: guard the park block on `ctx.awaitFollowUp`
    // ALONE (dropping the `ctx.interactive` term) → awaitFollowUp fires here and the assert
    // below reddens. With the guard intact a non-interactive run never enters the block.
    const { queryFn, turns } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [assistantText("t1"), signalDone(), resultSuccess()],
    ]);
    let awaitCalls = 0;
    let checkpointCalls = 0;
    const { ctx } = makeCtx({
      interactive: false,
      awaitFollowUp: async () => {
        awaitCalls++;
        return { kind: "ended", reason: "idle" };
      },
      checkpoint: async () => {
        checkpointCalls++;
      },
    });

    const result = await new SdkExecutor(nullLogger(), sdkHome, { queryFn }).run(ctx);

    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(awaitCalls, 0, "a non-interactive run never parks for a follow-up");
    assert.strictEqual(checkpointCalls, 0, "and never checkpoint-parks on done");
    assert.strictEqual(turns.length, 2, "plan + one loop turn, then finalize");
  });
});

// ── SteeringChannel.awaitFollowUp ────────────────────────────────────────────
describe("SteeringChannel.awaitFollowUp (PRD #517 M3)", () => {
  function fakeClient(batches: UserInput[][]): WorkerClient {
    let i = 0;
    return { getInputs: async () => batches[i++] ?? [] } as unknown as WorkerClient;
  }
  const inp = (kind: UserInput["kind"], body?: string): UserInput => ({
    id: 1,
    kind,
    body: body ?? null,
  });
  const tick = (ms = 10): Promise<void> => new Promise((r) => setTimeout(r, ms));

  it("returns a buffered follow-up immediately (drain-after-arm, no lost wakeup)", async () => {
    // The drop-on-idle race: a follow-up consumed by the poll loop BEFORE the executor arms
    // the waiter must not be lost. Mutation: delete the `if (this.followUps.length)` drain
    // branch in awaitFollowUp → the buffered follow-up is missed and the park idles instead.
    const ch = new SteeringChannel(
      fakeClient([[inp("follow_up", "next task")]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    ch.start();
    await tick(); // let the poll consume + buffer the follow-up before we park
    const outcome = await ch.awaitFollowUp(60_000);
    assert.deepStrictEqual(outcome, { kind: "followup", body: "next task" });
    await ch.stop();
  });

  it("hasPendingFollowUpOutcome reflects a buffered follow-up / stop / cancel without consuming it (issue #552 M1)", async () => {
    // The peek the interactive park uses to decide whether to report awaiting_followup (and so
    // stamp the open_followup_id watermark). Mutation: make it always return false → the park
    // report is never skipped and the mid-turn wake-guard bug returns.
    const ch = new SteeringChannel(
      fakeClient([[inp("follow_up", "next task")]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    assert.strictEqual(ch.hasPendingFollowUpOutcome(), false, "empty channel: nothing pending");
    ch.start();
    await tick(); // poll consumes + buffers the follow-up
    assert.strictEqual(
      ch.hasPendingFollowUpOutcome(),
      true,
      "a buffered follow-up is pending — the run is NOT idle",
    );
    // Non-consuming: awaitFollowUp still returns the same buffered follow-up afterwards.
    const outcome = await ch.awaitFollowUp(60_000);
    assert.deepStrictEqual(outcome, { kind: "followup", body: "next task" });
    assert.strictEqual(
      ch.hasPendingFollowUpOutcome(),
      false,
      "after the buffer drained, nothing is pending again",
    );
    await ch.stop();
  });

  it("hasPendingFollowUpOutcome is true for a buffered stop and for a cancel (issue #552 M1)", async () => {
    const stopCh = new SteeringChannel(
      fakeClient([[inp("stop", "wind down")]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    stopCh.start();
    await tick();
    assert.strictEqual(stopCh.hasPendingFollowUpOutcome(), true, "a buffered stop is pending");
    await stopCh.stop();

    const cancelCh = new SteeringChannel(
      fakeClient([[inp("cancel")]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    cancelCh.start();
    await tick(); // poll routes the cancel → sticky `cancelled`
    assert.strictEqual(cancelCh.hasPendingFollowUpOutcome(), true, "a cancel is pending");
    await cancelCh.stop();
  });

  it("ends the park with reason idle when no follow-up arrives within idleMs", async () => {
    // Mutation: drop the `now() - parkedAt >= idleMs` branch in serviceFollowUp → the park
    // never ends and the test hangs (a NAMED hang: the run would pin a slot forever).
    let clock = 1000;
    const ch = new SteeringChannel(
      fakeClient([[]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
      { now: () => clock },
    );
    ch.start();
    const parked = ch.awaitFollowUp(50); // arms with parkedAt = 1000
    clock = 1051; // advance past the idle bound; the next poll tick ends the park
    assert.deepStrictEqual(await parked, { kind: "ended", reason: "idle" });
    await ch.stop();
  });

  it("ends the park with reason idle even when getInputs throws on EVERY tick (persistent input-fetch outage)", async () => {
    // PRD #517 M5: the park's idle clock is evaluated by serviceFollowUp(), called from the
    // poll loop. A PERSISTENT run-scoped getInputs failure (e.g. a 500 on ConsumeInputs, or a
    // not-owned 404) must NOT starve that evaluation — otherwise, concurrent with a healthy
    // worker heartbeat, the park pins at awaiting_followup forever (a permanent zombie the
    // heartbeat-keyed stale-worker requeue never sees). The fake client rejects on every tick,
    // so no follow-up can ever be delivered, yet the idle bound must still finalize the park.
    // Mutation: move serviceFollowUp() back INSIDE the try block after getInputs (revert the
    // fix) → the throwing getInputs skips serviceFollowUp on every tick, the idle clock is
    // never evaluated, and this test hangs forever (never resolves → --test-timeout kills it).
    let clock = 1000;
    let calls = 0;
    const alwaysThrows = {
      getInputs: async () => {
        calls++;
        throw new Error("simulated persistent ConsumeInputs 500");
      },
    } as unknown as WorkerClient;
    const ch = new SteeringChannel(alwaysThrows, "run-1", 1, nullLogger(), new AbortController(), {
      now: () => clock,
    });
    ch.start();
    const parked = ch.awaitFollowUp(50); // arms with parkedAt = 1000
    clock = 1051; // advance past the idle bound; the next (still-throwing) poll tick ends the park
    assert.deepStrictEqual(await parked, { kind: "ended", reason: "idle" });
    assert.ok(calls > 0, "getInputs was polled (and threw) on every tick");
    await ch.stop();
  });

  it("ends the park with reason cancelled when the run is cancelled", async () => {
    // Mutation: drop the `if (this.cancelled)` arm in serviceFollowUp/awaitFollowUp → a
    // cancelled interactive run never leaves its park.
    const cancel = new AbortController();
    const ch = new SteeringChannel(
      fakeClient([[inp("cancel")]]),
      "run-1",
      1,
      nullLogger(),
      cancel,
    );
    ch.start();
    await tick(); // poll routes the cancel → sticky `cancelled`
    const outcome = await ch.awaitFollowUp(60_000);
    assert.deepStrictEqual(outcome, { kind: "ended", reason: "cancelled" });
    assert.strictEqual(cancel.signal.aborted, true);
    await ch.stop();
  });

  it("ends the park with reason stopped, AHEAD of a buffered follow-up (stop precedence)", async () => {
    // PRD #517 M4 Decision 5: an explicit `stop` wins over a QUEUED follow-up. The batch
    // carries BOTH — the follow-up first — so routing buffers the follow-up AND sets the
    // sticky stop flag; awaitFollowUp's drain-after-arm must return `stopped`, not the
    // follow-up. Mutation: check `this.followUps.length` before `this.stopRequested` in
    // awaitFollowUp → this returns { kind:"followup", body:"queued task" } and reddens.
    const ch = new SteeringChannel(
      fakeClient([[inp("follow_up", "queued task"), inp("stop", "wind down")]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    ch.start();
    await tick(); // poll routes BOTH the follow-up and the stop before we park
    const outcome = await ch.awaitFollowUp(60_000);
    assert.deepStrictEqual(outcome, { kind: "ended", reason: "stopped" });
    await ch.stop();
  });

  it("ends an ALREADY-PARKED waiter with reason stopped when a stop arrives (serviceFollowUp)", async () => {
    // The post-arm service path (park first, stop later), also AHEAD of a follow-up in the
    // same batch. awaitFollowUp is called synchronously after start() — before the first
    // poll routes — so it ARMS the waiter; the poll then routes [follow_up, stop] and
    // serviceFollowUp resolves it. Mutation: check `this.followUps.length` before
    // `this.stopRequested` in serviceFollowUp → resolves { kind:"followup" } instead.
    const ch = new SteeringChannel(
      fakeClient([[inp("follow_up", "queued task"), inp("stop")]]),
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    ch.start();
    const parked = ch.awaitFollowUp(60_000); // arm BEFORE the first poll routes anything
    const outcome = await parked;
    assert.deepStrictEqual(outcome, { kind: "ended", reason: "stopped" });
    await ch.stop();
  });

  it("ends the park with reason stopped from a SEEDED stop, with NO stop/follow-up input arriving (issue #552 M3 crash-recovery)", async () => {
    // The crash-recovery path: a graceful `uzi run stop` was consumed into stopRequested on
    // a prior worker that then DIED before winding the park down. On the requeue the fresh
    // SteeringChannel starts stopRequested=false and the already-consumed stop input never
    // re-delivers (empty input batch here) — so without the seed the park would idle for
    // ~30m. The claim re-delivers the durable stop_kind='stopped' fact as stop_pending, and
    // RunRunner calls seedStopRequested() from it, reconstructing the sticky stop state so
    // awaitFollowUp's arm-time check resolves { kind:"ended", reason:"stopped" } immediately.
    // Mutation: drop the seedStopRequested() call (or the `if (this.stopRequested)` arm in
    // awaitFollowUp) → this park never ends and the test hangs (killed by --test-timeout).
    const ch = new SteeringChannel(
      fakeClient([[]]), // no inputs at all — the stop input was consumed pre-crash
      "run-1",
      1,
      nullLogger(),
      new AbortController(),
    );
    ch.seedStopRequested(); // what RunRunner does from claim.stop_pending
    ch.start();
    const outcome = await ch.awaitFollowUp(60_000);
    assert.deepStrictEqual(outcome, { kind: "ended", reason: "stopped" });
    await ch.stop();
  });
});

// ── RunRunner's awaitFollowUp callback ───────────────────────────────────────
installHarness();

/** An executor that checkpoint-parks once, then commits and returns — the vehicle for
 *  the runner-layer park (StubExecutor never parks). */
function parkingExecutor(log: {
  outcomes: FollowUpOutcome[];
  checkpointed: boolean;
}): Executor {
  return {
    async run(ctx: RunContext): Promise<{ branch: string }> {
      await ctx.checkpoint?.({ reap: true });
      log.checkpointed = true;
      log.outcomes.push(await ctx.awaitFollowUp!(60_000));
      // Commit real work so the task push has a diff (mirrors StubExecutor). gpg off so a
      // host signing config cannot block it.
      fs.writeFileSync(path.join(ctx.worktreePath, "UZI_RUN.md"), "# interactive follow-up\n");
      execFileSync("git", ["add", "UZI_RUN.md"], { cwd: ctx.worktreePath });
      execFileSync(
        "git",
        [
          "-c",
          "user.name=uzi-agent",
          "-c",
          "user.email=uzi-agent@uzi.local",
          "-c",
          "commit.gpgsign=false",
          "commit",
          "-m",
          "uzi test: interactive follow-up handled",
        ],
        { cwd: ctx.worktreePath },
      );
      return { branch: ctx.branch };
    },
  };
}

/** Like parkingExecutor but parks with a SHORT idle bound and receives NO follow-up, so the
 *  real SteeringChannel resolves { kind:"ended", reason:"idle" }; it then commits real work
 *  and returns so the run winds down through the normal finalize (push + completed) — the M5
 *  worker-side idle finalize. */
function idleParkingExecutor(log: { outcome?: FollowUpOutcome }): Executor {
  return {
    async run(ctx: RunContext): Promise<{ branch: string }> {
      await ctx.checkpoint?.({ reap: true });
      // Short idle; no follow-up is ever delivered, so the park ends idle within a few polls.
      log.outcome = await ctx.awaitFollowUp!(40);
      fs.writeFileSync(path.join(ctx.worktreePath, "UZI_RUN.md"), "# interactive idle finalize\n");
      execFileSync("git", ["add", "UZI_RUN.md"], { cwd: ctx.worktreePath });
      execFileSync(
        "git",
        [
          "-c",
          "user.name=uzi-agent",
          "-c",
          "user.email=uzi-agent@uzi.local",
          "-c",
          "commit.gpgsign=false",
          "commit",
          "-m",
          "uzi test: interactive idle finalize",
        ],
        { cwd: ctx.worktreePath },
      );
      return { branch: ctx.branch };
    },
  };
}

/** Parks in a loop, recording every outcome, until the park ENDS (a stop/idle/cancel). The
 *  vehicle for the issue #559 watermark test: it parks once per delivered follow-up, so the
 *  runner emits one awaiting_followup report per turn and we can read the open_followup_id it
 *  stamped at each. Commits real work at the end so the run finalizes through the normal push. */
function multiParkExecutor(log: { outcomes: FollowUpOutcome[] }): Executor {
  return {
    async run(ctx: RunContext): Promise<{ branch: string }> {
      await ctx.checkpoint?.({ reap: true });
      for (;;) {
        const o = await ctx.awaitFollowUp!(60_000);
        log.outcomes.push(o);
        if (o.kind === "ended") break;
      }
      fs.writeFileSync(path.join(ctx.worktreePath, "UZI_RUN.md"), "# interactive multi-turn\n");
      execFileSync("git", ["add", "UZI_RUN.md"], { cwd: ctx.worktreePath });
      execFileSync(
        "git",
        [
          "-c",
          "user.name=uzi-agent",
          "-c",
          "user.email=uzi-agent@uzi.local",
          "-c",
          "commit.gpgsign=false",
          "commit",
          "-m",
          "uzi test: interactive multi-turn handled",
        ],
        { cwd: ctx.worktreePath },
      );
      return { branch: ctx.branch };
    },
  };
}

function taskClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  const runId = (overrides.run_id as string | undefined) ?? randomUUID();
  return makeClaim({
    run_id: runId,
    kind: "task",
    interactive: true,
    issue_iid: null,
    issue_title: "Handoff: interactive session",
    issue_description: "Work with me interactively.",
    branch: `uzi/task/${runId}`,
    base_branch: "develop",
    open_mr: false,
    repo: {
      id: "r1",
      url: "https://gitlab.example.test/org/repo",
      clone_url: fx.originPath,
    },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

describe("RunRunner interactive follow-up park (PRD #517 M3)", () => {
  it("reports awaiting_followup and resumes the run when a follow-up is delivered", async () => {
    // Mutation A: delete the `reportState({status:"awaiting_followup"})` in the callback →
    // no awaiting_followup state ever lands and the first assert reddens. Mutation B:
    // replace steering.awaitFollowUp with steering.pullFollowUp (non-blocking) → the park
    // returns undefined immediately (never consuming through the poll loop), the run either
    // crashes or never receives the injected follow-up.
    const { gitlab } = fakeGitlab();
    const log = { outcomes: [] as FollowUpOutcome[], checkpointed: false };
    const claim = taskClaim();

    // Deliver the follow-up the moment the run parks — exactly the race the poll loop must
    // handle: the input is consumed (consumed_at stamped) before the callback returns it.
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_followup") {
        api.setInputs(claim.run_id, [{ id: 1, kind: "follow_up", body: "keep going" }]);
      }
    });

    await runner(parkingExecutor(log), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      statuses.includes("awaiting_followup"),
      `the run must report awaiting_followup; statuses were ${JSON.stringify(statuses)}`,
    );
    assert.strictEqual(log.checkpointed, true, "the executor checkpoint-pushed at the park");
    assert.deepStrictEqual(
      log.outcomes,
      [{ kind: "followup", body: "keep going" }],
      "the callback delivered the injected follow-up, consumed via the poll loop",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "the run resumed past the park and finalized",
    );
  });

  it("skips the awaiting_followup park report when a follow-up is already buffered mid-turn (issue #552 M1)", async () => {
    // A follow-up that arrived MID-TURN is consumed (consumed_at stamped) + buffered by the
    // poll loop BEFORE the park. Reporting awaiting_followup here would stamp the
    // open_followup_id watermark to MAX(consumed follow_up id) INCLUDING that follow-up, so its
    // own wake `running` report would then fail the server's `id > watermark` guard and strand
    // a live run at awaiting_followup. The callback must SKIP the park report and service the
    // buffered follow-up directly (the run stays running, no spurious park). Mutation: drop the
    // `if (!steering.hasPendingFollowUpOutcome())` guard around the park report → awaiting_followup
    // is reported and the `!statuses.includes("awaiting_followup")` assert reddens.
    const { gitlab } = fakeGitlab();
    const log = { outcomes: [] as FollowUpOutcome[], checkpointed: false };
    const claim = taskClaim();
    // Seed the follow-up BEFORE the run starts: the 5ms poll loop consumes + buffers it during
    // the executor's checkpoint (real git, >> 5ms), so it is in hand when the park callback fires.
    api.setInputs(claim.run_id, [{ id: 1, kind: "follow_up", body: "keep going" }]);

    await runner(parkingExecutor(log), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("awaiting_followup"),
      `a mid-turn-buffered follow-up must NOT trigger an awaiting_followup park; statuses were ${JSON.stringify(statuses)}`,
    );
    assert.deepStrictEqual(
      log.outcomes,
      [{ kind: "followup", body: "keep going" }],
      "the buffered follow-up was serviced directly, without parking",
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "the run resumed past the skipped park and finalized",
    );
  });

  it("fails loudly when the server declines the awaiting_followup park", async () => {
    // The ack check: SetRunAwaitingFollowup matches nothing when the run went terminal or is
    // no longer ours, so the server returns a DIFFERENT status. Mutation: delete the
    // `if (parked !== "awaiting_followup") throw` check → the worker blocks on a follow-up no
    // surface can produce and the run hangs to its deadline instead of failing here.
    const { gitlab } = fakeGitlab();
    const log = { outcomes: [] as FollowUpOutcome[], checkpointed: false };
    const claim = taskClaim();
    // Every /state ack for this run comes back as `running`, so the park report's ack does
    // not confirm the transition.
    api.overrideStateStatus(claim.run_id, "running");

    await runner(parkingExecutor(log), gitlab).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "a declined park must fail the run, not hang it");
    assert.match(failed!.body.failure_reason ?? "", new RegExp(REASON_FOLLOWUP_NOT_PARKED));
    assert.deepStrictEqual(log.outcomes, [], "the executor never received a follow-up");
  });

  it("finalizes an idle park gracefully: reports completed (not failed) and pushes the branch", async () => {
    // PRD #517 M5 (SC4b / Decision 6): when no follow-up arrives within the idle bound the
    // steering park resolves { ended, reason:"idle" } and the run winds down like a normal
    // signal_done — push then `completed`, NEVER failed. No follow-up is delivered, so the
    // real SteeringChannel idles. Mutation: route `idle` to the cancel-signal throw (as
    // `cancelled` does in sdk-executor) → the run reports `failed` and the completed +
    // branch-pushed asserts redden.
    const { gitlab, calls } = fakeGitlab();
    const log: { outcome?: FollowUpOutcome } = {};
    const claim = taskClaim({ open_mr: false });

    await runner(idleParkingExecutor(log), gitlab).execute(claim);

    assert.deepStrictEqual(
      log.outcome,
      { kind: "ended", reason: "idle" },
      "the park ended idle (no follow-up arrived within the bound)",
    );

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(statuses.includes("awaiting_followup"), "the run parked before idling");
    assert.ok(
      !statuses.includes("failed"),
      `an idle finalize must NOT report failed; statuses were ${JSON.stringify(statuses)}`,
    );
    assert.strictEqual(
      statuses.at(-1),
      "completed",
      "an idle park finalizes as completed",
    );

    // The branch really landed on origin — the deliverable the user pulls (mirrors
    // runner-task.test.ts's real-push assertion, not a mock).
    const gitLog = execFileSync(
      "git",
      ["-C", fx.originPath, "log", "--oneline", `uzi/task/${claim.run_id}`],
      { encoding: "utf8" },
    );
    assert.ok(
      gitLog.includes("uzi test: interactive idle finalize"),
      "the worker's commit landed on the task branch (pushed at idle finalize)",
    );
    // open_mr:false ⇒ a push, not an MR-open: no createMergeRequest POST reached the forge.
    assert.equal(calls.length, 0, "a no-MR idle finalize opens no merge request");
  });

  it("carries open_followup_id = the pre-round-trip last-delivered id on each park report (issue #559 M2)", async () => {
    // The worker-provided wake-guard watermark. Each awaiting_followup report must carry the
    // highest follow_up id ALREADY delivered before that report — a value stable across the
    // report's DB round-trip because the next follow-up is consumed only AFTER the report
    // returns. Three parks: nothing delivered yet (0), then after follow-up id 5 (5), then
    // after follow-up id 8 (8); a final stop ends the loop. Mutation: drop `open_followup_id`
    // from the reportState call in the awaitFollowUp callback → the field is undefined on
    // every park report and the deepStrictEqual reddens.
    const { gitlab } = fakeGitlab();
    const log = { outcomes: [] as FollowUpOutcome[] };
    const claim = taskClaim();

    let park = 0;
    api.onState(claim.run_id, (body) => {
      if (body.status !== "awaiting_followup") return;
      park++;
      if (park === 1)
        api.setInputs(claim.run_id, [{ id: 5, kind: "follow_up", body: "turn 2" }]);
      else if (park === 2)
        api.setInputs(claim.run_id, [{ id: 8, kind: "follow_up", body: "turn 3" }]);
      else api.setInputs(claim.run_id, [{ id: 9, kind: "stop", body: "wind down" }]);
    });

    await runner(multiParkExecutor(log), gitlab).execute(claim);

    const watermarks = api.states
      .filter((s) => s.runId === claim.run_id && s.body.status === "awaiting_followup")
      .map((s) => s.body.open_followup_id);
    assert.deepStrictEqual(
      watermarks,
      [0, 5, 8],
      `each park must report the pre-round-trip last-delivered id; got ${JSON.stringify(watermarks)}`,
    );
    assert.deepStrictEqual(log.outcomes, [
      { kind: "followup", body: "turn 2" },
      { kind: "followup", body: "turn 3" },
      { kind: "ended", reason: "stopped" },
    ]);
  });
});
