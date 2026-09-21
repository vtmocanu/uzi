import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { RunContext, WallParkOutcome } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { PauseNowSignal } from "../src/steering.js";
import { nullLogger } from "./helpers.js";

/**
 * PRD #1497 M2 — the SdkExecutor (Claude harness) wall-park routing.
 *
 * A PRE-attempt REASON_WALL (the worker's own wall timer, before any completion attempt) takes the
 * capture-first wall park (ctx.parkForWall) instead of the legacy terminal throw — reportGenericFailure
 * is UNREACHABLE from a wall trip. A POST-attempt REASON_WALL still takes the completion hold
 * (D6/D14). A `wall` pause (a PauseNowSignal whose sticky mode is "wall") arriving AFTER the first
 * completion attempt routes to the completion hold FIRST and NEVER reaches the wall seam (D14). A
 * REFUSED wall park (the owner extended in the window) clears the sticky wall mode and restarts the
 * turn — both from the turn catch (after the turn aborted) and from the loop-top boundary.
 *
 * Harness mirrors sdk-executor-extend.test.ts (its helpers are not exported).
 */

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
  return {
    type: "result",
    subtype: "success",
    is_error: false,
    num_turns: 1,
    session_id: sessionId,
  } as unknown as SDKMessage;
}

/** A turn script: a static message list, or a function driven with the turn's abort signal so it can
 *  fire a side effect (abort ctx.signal) and hang until the trip lands. */
type Script = SDKMessage[] | ((turnSignal: AbortSignal) => AsyncIterable<unknown>);

/** Hang until `signal` aborts (the trip's turn-abort), then return — driveTurn then throws its
 *  first-wins tripReason. A ref'd keep-alive holds the event loop open so an unref'd watchdog timer
 *  (the wall) still fires (same shape/reason as sdk-executor.test.ts's copy). */
function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    // NEVER YIELDING IS THE FIXTURE (oxlint eslint(require-yield)), same as sdk-executor.test.ts.
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

/** A queryFn replaying one scripted stream per turn. Each turn optionally burns `sleeps[i]` (clamped)
 *  of REAL time in-turn (the window driveTurn arms the wall around), so a slow turn on a short wall
 *  trips REASON_WALL. */
function fakeTurns(
  scripts: Script[],
  sleeps: number[] = [],
): { queryFn: SdkQueryFn } {
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    const sleepMs = sleeps.length ? sleeps[Math.min(i, sleeps.length - 1)]! : 0;
    i++;
    return (async function* () {
      for await (const _ of params.prompt) {
        /* drain */
      }
      if (sleepMs > 0) await new Promise((r) => setTimeout(r, sleepMs));
      if (typeof script === "function") {
        yield* script(params.options.abortController!.signal) as AsyncIterable<SDKMessage>;
      } else {
        for (const m of script) yield m;
      }
    })();
  };
  return { queryFn };
}

let homeDir: string;
let saved: Record<string, string | undefined>;
let wtSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-wall-wt-${process.pid}-${wtSeq++}`);
}

interface Spies {
  parkForWallCalls: number;
  parkForWallOutcome: WallParkOutcome;
  enterCompletionHoldCalls: string[];
  clearWallModeCalls: number;
  mode: { value: "milestone" | "now" | "wall" | null };
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; spies: Spies } {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const spies: Spies = {
    parkForWallCalls: 0,
    parkForWallOutcome: "parked",
    enterCompletionHoldCalls: [],
    clearWallModeCalls: 0,
    mode: { value: null },
  };
  const ctx: RunContext = {
    runId: "r1",
    issueIid: 5,
    issueTitle: "Fix login",
    issueDescription: "please implement",
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
    reportIteration: () => {},
    checkpoint: async () => {},
    worktreeFingerprint: async () => "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef\n",
    pauseModeRequested: () => spies.mode.value,
    clearWallMode: () => {
      spies.clearWallModeCalls++;
      spies.mode.value = null;
    },
    parkForWall: async () => {
      spies.parkForWallCalls++;
      return spies.parkForWallOutcome;
    },
    enterCompletionHold: async (reason: string) => {
      spies.enterCompletionHoldCalls.push(reason);
      return true;
    },
    ...overrides,
  };
  return { ctx, spies };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-wallhome-"));
  saved = { UZI_WORKER_TOKEN: process.env.UZI_WORKER_TOKEN, UZI_FORGE_PAT: process.env.UZI_FORGE_PAT };
  process.env.UZI_WORKER_TOKEN = "dummy-join-token-do-not-scan-2222";
  process.env.UZI_FORGE_PAT = "dummy-forge-pat-do-not-scan-1111";
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
  for (const [k, v] of Object.entries(saved)) {
    if (v === undefined) delete process.env[k];
    else process.env[k] = v;
  }
});

describe("SdkExecutor wall park (PRD #1497 M2)", () => {
  it("a PRE-attempt REASON_WALL takes the capture-first wall park, not a terminal failure", async () => {
    // Plan turn instant; the implement turn hangs until the wall timer aborts it (a 0.05s wall). No
    // signal_done, so completionAttempted is false — the pre-attempt path. parkForWall returns
    // "parked", so run() RESOLVES with walled set rather than throwing REASON_WALL.
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      (signal) => hangUntilAbort(signal),
    ]);
    const { ctx, spies } = makeCtx({ config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.05 } });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.deepStrictEqual(result.walled, { reason: "run exceeded its wall-clock timeout" }, "the run parked at the wall");
    assert.strictEqual(spies.parkForWallCalls, 1, "the pre-attempt wall trip routed to parkForWall");
    assert.strictEqual(spies.enterCompletionHoldCalls.length, 0, "a pre-attempt wall NEVER enters the completion hold");
  });

  it("a POST-attempt REASON_WALL still takes the completion hold (D6/D14)", async () => {
    // Plan (instant) → implement turn signals done → a completion attempt runs (completionAttempted
    // true, unmet non-empty → rework + continue) → the rework turn hangs until the 0.1s wall aborts
    // it. Post-attempt, so the WALL routes to the completion hold, NOT the wall seam.
    const { queryFn } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [signalDone(), resultSuccess()],
        (signal) => hangUntilAbort(signal),
      ],
      [0, 0, 0],
    );
    const { ctx, spies } = makeCtx({
      config: { idle_timeout_seconds: 100, run_timeout_seconds: 0.08 },
      completionInterlock: true,
      recordCompletionAttempt: async () => ({ unmet: ["m1"], attemptCount: 1 }),
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.ok(result.completionHeld, "a post-attempt wall entered the completion hold");
    assert.strictEqual(spies.enterCompletionHoldCalls.length, 1, "enterCompletionHold was taken");
    assert.strictEqual(spies.parkForWallCalls, 0, "a post-attempt wall NEVER reaches the wall seam");
    assert.strictEqual(result.walled, undefined, "no wall park on a post-attempt wall");
  });

  it("D14: a `wall` pause arriving AFTER the first completion attempt routes to the completion hold, NOT the wall seam", async () => {
    // Turn 2 signals done → a completion attempt (completionAttempted true, unmet → rework). Turn 3
    // (the rework turn) is aborted by a `wall` PauseNowSignal: the PauseNowSignal arm sees mode 'wall'
    // AND completionAttempted true, so it routes to routeCompletionHold FIRST — the completion hold is
    // taken and NO wall_park (parkForWall) is reported.
    const runSignal = new AbortController();
    const spiesRef: { s?: Spies } = {};
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
      (signal) => ({
        async *[Symbol.asyncIterator]() {
          // Fire the `wall` pause mid-turn: set the sticky mode and abort the run signal with a
          // PauseNowSignal, exactly as the steering channel's route('pause','wall') does.
          spiesRef.s!.mode.value = "wall";
          runSignal.abort(new PauseNowSignal());
          yield* hangUntilAbort(signal);
        },
      }),
    ]);
    const { ctx, spies } = makeCtx({
      signal: runSignal.signal,
      completionInterlock: true,
      recordCompletionAttempt: async () => ({ unmet: ["m1"], attemptCount: 1 }),
    });
    spiesRef.s = spies;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.ok(result.completionHeld, "the wall pause after a completion attempt took the completion hold");
    assert.strictEqual(spies.enterCompletionHoldCalls.length, 1, "the completion-hold path (SetRunCompletionHold seam) was taken");
    assert.strictEqual(spies.parkForWallCalls, 0, "NO wall_park was reported — the completion hold wins the race");
    assert.strictEqual(result.walled, undefined, "the resulting hold is the completion hold, not a wall park");
  });

  it("a genuine `wall` PauseNowSignal with NO completion attempt PARKS through the turn catch (parked outcome)", async () => {
    // Turn 2 (the implement turn) is aborted by a `wall` PauseNowSignal — the sweep's system-authored
    // wall park delivered through the steering channel, NOT the wall timer. No completion attempt has
    // run (completionInterlock off), so the PauseNowSignal arm's `wall` branch reaches parkForWall
    // directly (it never routes to the completion hold). parkForWall returns "parked", so the run
    // latches `walled` and ends non-terminal. This proves the PARKED outcome through the PauseNowSignal
    // catch site, complementing the wall-TIMER PARKED case above.
    const runSignal = new AbortController();
    const spiesRef: { s?: Spies } = {};
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      (signal) => ({
        async *[Symbol.asyncIterator]() {
          spiesRef.s!.mode.value = "wall";
          runSignal.abort(new PauseNowSignal());
          yield* hangUntilAbort(signal);
        },
      }),
    ]);
    const { ctx, spies } = makeCtx({ signal: runSignal.signal });
    spies.parkForWallOutcome = "parked";
    spiesRef.s = spies;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.deepStrictEqual(
      result.walled,
      { reason: "run exceeded its wall-clock timeout" },
      "the `wall` PauseNowSignal parked the run through the catch site",
    );
    assert.strictEqual(spies.parkForWallCalls, 1, "the wall pause reached parkForWall");
    assert.strictEqual(spies.enterCompletionHoldCalls.length, 0, "a pre-attempt wall pause never enters the completion hold");
    assert.strictEqual(spies.clearWallModeCalls, 0, "a PARKED outcome does not clear the wall mode (only a refusal does)");
  });

  it("a REFUSED wall park (turn catch) clears the sticky wall mode and restarts the turn (extension after the input aborted the turn)", async () => {
    // Turn 2 is aborted by a `wall` PauseNowSignal; parkForWall returns "refused" (the owner extended
    // in the window). The executor clears the sticky wall mode and restarts — turn 3 signals done.
    const runSignal = new AbortController();
    const spiesRef: { s?: Spies } = {};
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      (signal) => ({
        async *[Symbol.asyncIterator]() {
          spiesRef.s!.mode.value = "wall";
          runSignal.abort(new PauseNowSignal());
          yield* hangUntilAbort(signal);
        },
      }),
      [signalDone(), resultSuccess()],
    ]);
    const { ctx, spies } = makeCtx({ signal: runSignal.signal });
    spies.parkForWallOutcome = "refused";
    spiesRef.s = spies;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(spies.parkForWallCalls, 1, "the wall park was attempted");
    assert.strictEqual(spies.clearWallModeCalls, 1, "a refused wall park clears the sticky wall mode");
    assert.strictEqual(result.walled, undefined, "a refused park does NOT park — the run continued");
    assert.strictEqual(result.branch, "agent/issue-5", "the restarted turn finished the run");
  });

  it("a REFUSED wall park (loop-top boundary) clears the sticky wall mode and continues (extension before the input arrived)", async () => {
    // The loop starts with a seeded `wall` mode (pauseModeRequested → 'wall'), so the loop-top wall
    // branch fires before the first implement turn. parkForWall returns "refused" (the owner already
    // extended), so the executor clears the wall mode and falls through to run the turn (signal_done).
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const { ctx, spies } = makeCtx();
    spies.mode.value = "wall"; // seeded on a re-claim; seedPauseFallback fires the loop-top branch
    spies.parkForWallOutcome = "refused";
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(spies.parkForWallCalls, 1, "the loop-top wall branch attempted the park");
    assert.strictEqual(spies.clearWallModeCalls, 1, "a refused loop-top wall park clears the sticky wall mode");
    assert.strictEqual(result.walled, undefined, "a refused park does NOT park");
    assert.strictEqual(result.branch, "agent/issue-5", "the run continued and finished");
  });
});
