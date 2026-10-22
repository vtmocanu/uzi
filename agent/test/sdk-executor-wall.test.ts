import { overflowClock, drain, NATIVE_LIMIT, REPRO_DELAY } from "./overflow-clock.js";
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
import { nonexistentWorktreeFactory, nullLogger } from "./helpers.js";

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
function signalDone(sessionId = "sess-1", input: Record<string, unknown> = {}): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t2", name: "mcp__uzi__signal_done", input }] },
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
const nonexistentWorktree = nonexistentWorktreeFactory("uzi-wall");

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
    pullFollowUps: () => [],
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

describe("SDK overflow wall expiry M1", () => {
  for (const mode of ["finish", "cancel"] as const) it(
    `SDK oversized wall cleanup cancels rearmed chunk on ${mode}`, async t => {
      const clock = overflowClock(t);
      const controller = new AbortController();
      let turnSignal: AbortSignal | undefined;
      let rearmed = false;
      const queryFn: SdkQueryFn = params => (async function* () {
        for await (const _ of params.prompt) { /* drain */ }
        turnSignal = params.options.abortController!.signal;
        clock.move(NATIVE_LIMIT); clock.deliver(d => d >= NATIVE_LIMIT); await drain();
        rearmed = clock.pending().some(r => r.delay === REPRO_DELAY - NATIVE_LIMIT);
        if (mode === "cancel") controller.abort();
        else yield signalDone();
        yield resultSuccess();
      })();
      const { ctx, spies } = makeCtx({ planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
        signal: controller.signal, config: { run_timeout_seconds: REPRO_DELAY / 1000 } });
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn, timeoutNow: clock.now }).run(ctx);
      if (mode === "cancel") await assert.rejects(running, { message: "run cancelled" });
      else assert.equal((await running).walled, undefined);
      assert.equal(rearmed, true, "cleanup starts with a live second chunk");
      assert.equal(clock.pending().some(r => r.delay === REPRO_DELAY - NATIVE_LIMIT), false);
      const aborted = turnSignal!.aborted;
      const abortCalls = t.mock.method(AbortController.prototype, "abort");
      clock.move(REPRO_DELAY - NATIVE_LIMIT); clock.deliver(); await drain();
      assert.equal(abortCalls.mock.callCount(), 0, "old remainder cannot abort after cleanup");
      assert.equal(turnSignal!.aborted, aborted);
      assert.equal(spies.parkForWallCalls, 0, "cancel or finish cannot become a stale wall park");
      clock.assertSafe();
    },
  );
  it("SDK oversized wall rearms then parks once at full deadline", async t => {
    const clock = overflowClock(t);
    const queryFn: SdkQueryFn = params => (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      const signal = params.options.abortController!.signal;
      clock.move(NATIVE_LIMIT); clock.deliver(d => d >= NATIVE_LIMIT); await drain();
      assert.equal(signal.aborted, false, "first chunk is not the wall deadline");
      clock.move(98_999_353); clock.deliver(d => d === 98_999_354); await drain();
      assert.equal(signal.aborted, false);
      clock.move(1); clock.deliver(d => d === 98_999_354); await drain();
      assert.equal(signal.aborted, true, "full relative budget expires");
      yield resultSuccess();
    })();
    const { ctx, spies } = makeCtx({ planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
      config: { run_timeout_seconds: REPRO_DELAY / 1000 } });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn, timeoutNow: clock.now }).run(ctx);
    assert.ok(result.walled);
    assert.equal(spies.parkForWallCalls, 1);
    clock.assertSafe();
    assert.ok(clock.requests.filter(r => r.delay === NATIVE_LIMIT || r.delay === 98_999_354).every(r => r.unrefs === 1));
  });
});

describe("SdkExecutor wall park (PRD #1497 M2)", () => {
  for (const outcome of ["parked", "undeliverable", "refused", "cancelled"] as const) {
    it(`wall park ${outcome} racing cancellation preserves its sink outcome`, async () => {
      let cancelled = false, parks = 0, dispatches = 0;
      const { ctx } = makeCtx({
        planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
        pauseModeRequested: () => "wall",
        cancelRequested: () => cancelled,
        parkForWall: async () => { parks++; cancelled = true; return outcome; },
      });
      const queryFn: SdkQueryFn = () => { dispatches++; throw new Error("unexpected provider dispatch"); };
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      if (outcome === "parked" || outcome === "undeliverable")
        assert.deepEqual((await running).walled, { reason: "run exceeded its wall-clock timeout" });
      else await assert.rejects(running, { message: "run cancelled" });
      assert.equal(parks, 1);
      assert.equal(dispatches, 0);
    });
  }

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

  it("3282 a REFUSED wall park (turn catch) clears the sticky wall mode and restarts the turn (extension after the input aborted the turn)", async () => {
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
    const { ctx, spies } = makeCtx({ signal: runSignal.signal,
      reportIteration: async () => ({ completionAttempts: 0 }),
      takeWallParkRefresh: () => ({ completionAttempts: 0 }),
    });
    spies.parkForWallOutcome = "refused";
    spiesRef.s = spies;
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(spies.parkForWallCalls, 1, "the wall park was attempted");
    assert.strictEqual(spies.clearWallModeCalls, 1, "a refused wall park clears the sticky wall mode");
    assert.strictEqual(result.walled, undefined, "a refused park does NOT park — the run continued");
    assert.strictEqual(result.branch, "agent/issue-5", "the restarted turn finished the run");
  });

  it("3282 a REFUSED wall park (loop-top boundary) clears the sticky wall mode and continues (extension before the input arrived)", async () => {
    // The loop starts with a seeded `wall` mode (pauseModeRequested → 'wall'), so the loop-top wall
    // branch fires before the first implement turn. parkForWall returns "refused" (the owner already
    // extended), so the executor clears the wall mode and falls through to run the turn (signal_done).
    const { queryFn } = fakeTurns([
      [submitPlan("plan"), resultSuccess()],
      [signalDone(), resultSuccess()],
    ]);
    const { ctx, spies } = makeCtx({
      reportIteration: async () => ({ completionAttempts: 0 }),
      takeWallParkRefresh: () => ({ completionAttempts: 0 }),
    });
    spies.mode.value = "wall"; // seeded on a re-claim; seedPauseFallback fires the loop-top branch
    spies.parkForWallOutcome = "refused";
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(spies.parkForWallCalls, 1, "the loop-top wall branch attempted the park");
    assert.strictEqual(spies.clearWallModeCalls, 1, "a refused loop-top wall park clears the sticky wall mode");
    assert.strictEqual(result.walled, undefined, "a refused park does NOT park");
    assert.strictEqual(result.branch, "agent/issue-5", "the run continued and finished");
  });
});

describe("3282 Claude historical completion wall routing", () => {
  for (const route of ["spent", "pending", "budget", "trip", "refusal", "refusal-extension", "extension", "iteration"] as const) {
    for (const accepted of (route === "extension" || route === "iteration" ? [true] : [true, false])) it(`3282 ${route} history without a flight attempt; hold ${accepted}`, async () => {
      let dispatches = 0, parks = 0, takes = 0, holds = 0;
      const cancelled = { value: false };
      const { ctx } = makeCtx({
        planApproved: true, approvedPlan: "approved", sessionId: "sess-1", completionInterlock: true, recordCompletionAttempt: async () => ({ unmet: [], attemptCount: 2 }),
        config: { max_iterations: 1, run_timeout_seconds: 0.02 },
        cancelRequested: () => cancelled.value,
        reportIteration: async () => route.startsWith("refusal") ? undefined : ({
          completionAttempts: 1, totalWallSeconds: route === "trip" ? 0.02 : 60,
          usedWallSeconds: route === "spent" ? 60 : 0,
          pauseRequested: route === "pending", budgetExhausted: route === "budget",
        }),
        pauseModeRequested: () => route === "pending" ? "wall" : null,
        enterCompletionHold: async (_reason, options) => {
          holds++;
          assert.equal(options?.allowDegradedCapture, true);
          return accepted;
        },
        parkForWall: async () => {
          assert.equal(++parks, 1, "3282 historical wall must not retry wall parking");
          return "refused";
        },
        takeWallParkRefresh: () => {
          takes++;
          return { completionAttempts: 1, totalSeconds: route === "refusal-extension" ? 120 : 60, usedSeconds: 60 };
        },
      });
      const queryFn: SdkQueryFn = params => {
        dispatches++;
        assert.ok(dispatches <= 1, "no model redrive after a historical wall");
        return (async function* () {
          for await (const _ of params.prompt) { /* drain */ }
          if (["trip", "refusal", "refusal-extension"].includes(route))
            yield* hangUntilAbort(params.options.abortController!.signal) as AsyncIterable<SDKMessage>;
          else if (route === "extension") yield signalDone();
          yield resultSuccess();
        })();
      };
      const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
      if (route === "iteration") {
        await assert.rejects(running, /iteration/);
        assert.equal(holds, 0, "history alone must not change iteration routing");
      } else {
        const result = await running;
        if (route === "extension") {
          assert.equal(holds, 0); assert.equal(result.completionHeld, undefined);
        } else {
          assert.ok(accepted ? result.completionHeld : result.walled);
          assert.equal(holds, 1);
        }
      }
      assert.equal(dispatches, ["spent", "pending", "budget"].includes(route) ? 0 : 1);
      assert.equal(parks, route.startsWith("refusal") ? 1 : 0);
      assert.equal(takes, parks, "refusal evidence is consumed once");
    });
  }

  it("3282 history survives zero and missing ACKs before a pending wall", async () => {
    let reports = 0, dispatches = 0, holds = 0;
    const { ctx } = makeCtx({
      planApproved: true, approvedPlan: "approved", sessionId: "sess-1", completionInterlock: true, recordCompletionAttempt: async () => ({ unmet: [], attemptCount: 2 }),
      config: { max_iterations: 5 },
      reportIteration: async () => {
        reports++;
        return reports === 1 ? { completionAttempts: 1 }
          : reports === 2 ? { completionAttempts: 0 } : { pauseRequested: true };
      },
      pauseModeRequested: () => reports >= 3 ? "wall" : null,
      enterCompletionHold: async () => { holds++; return true; },
    });
    const { queryFn } = fakeTurns([() => {
      dispatches++;
      return (async function* () { yield resultSuccess(); })();
    }]);
    assert.ok((await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx)).completionHeld);
    assert.equal(dispatches, 2); assert.equal(holds, 1);
  });

  it("3282 cancellation racing a resumed historical hold wins", async () => {
    let cancelled = false, holds = 0;
    let release!: () => void, entered!: () => void;
    const admitted = new Promise<void>(resolve => { entered = resolve; });
    const barrier = new Promise<void>(resolve => { release = resolve; });
    const { ctx } = makeCtx({
      planApproved: true, approvedPlan: "approved", sessionId: "sess-1", completionInterlock: true, recordCompletionAttempt: async () => ({ unmet: [], attemptCount: 2 }),
      reportIteration: async () => ({ completionAttempts: 1, budgetExhausted: true }),
      cancelRequested: () => cancelled,
      enterCompletionHold: async () => { holds++; entered(); await barrier; return true; },
    });
    const { queryFn } = fakeTurns([[signalDone(), resultSuccess()]]);
    const running = new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    let watchdog: NodeJS.Timeout | undefined;
    try {
      await Promise.race([admitted, running.then(() => {
        throw new Error("3282 run ended before historical hold admission");
      }), new Promise<never>((_, reject) => {
        watchdog = setTimeout(() => reject(new Error("3282 hold admission timed out")), 2000);
      })]);
      cancelled = true;
    } finally { clearTimeout(watchdog); release(); }
    await assert.rejects(running, /cancel/);
    assert.equal(holds, 1);
  });
});


describe("3282 Claude historical interruption regressions", () => {
  it("3282 historical-only idle remains an idle error without hold or wall park", async () => {
    const { ctx, spies } = makeCtx({ planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
      completionInterlock: true, config: { idle_timeout_seconds: 0.02, run_timeout_seconds: 5 },
      reportIteration: async () => ({ completionAttempts: 1 }),
    });
    const { queryFn } = fakeTurns([hangUntilAbort]);
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx), /run stalled: no agent activity within the idle timeout/);
    assert.deepEqual(spies.enterCompletionHoldCalls, []);
    assert.equal(spies.parkForWallCalls, 0);
  });

  for (const accepted of [true, false]) it(`3282 wall PauseNowSignal during first implement turn uses history; hold ${accepted}`, async () => {
    const controller = new AbortController();
    let mode: "wall" | null = null;
    let dispatches = 0, holds = 0;
    const { ctx, spies } = makeCtx({ planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
      completionInterlock: true, signal: controller.signal,
      reportIteration: async () => ({ completionAttempts: 1 }),
      pauseModeRequested: () => mode,
      enterCompletionHold: async (_reason, options) => { holds++; assert.equal(options?.allowDegradedCapture, true); return accepted; },
    });
    const queryFn: SdkQueryFn = params => (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      dispatches++;
      assert.equal(dispatches, 1);
      mode = "wall";
      controller.abort(new PauseNowSignal());
      yield* hangUntilAbort(params.options.abortController!.signal) as AsyncIterable<SDKMessage>;
    })();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.ok(accepted ? result.completionHeld : result.walled);
    assert.equal(holds, 1); assert.equal(dispatches, 1);
    assert.equal(spies.parkForWallCalls, 0);
  });

  it("3282 owner now pause precedes historical budget exhaustion", async () => {
    let pauses = 0, dispatches = 0;
    const { ctx, spies } = makeCtx({ planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
      completionInterlock: true,
      reportIteration: async () => ({ completionAttempts: 1, budgetExhausted: true, pauseRequested: true }),
      pauseModeRequested: () => "now",
      parkForPause: async () => { pauses++; return true; },
    });
    const queryFn: SdkQueryFn = () => { dispatches++; throw new Error("owner pause must precede dispatch"); };
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.ok(result.pausedAt); assert.equal(pauses, 1); assert.equal(dispatches, 0);
    assert.deepEqual(spies.enterCompletionHoldCalls, []); assert.equal(spies.parkForWallCalls, 0);
  });
});


describe("3282 Claude lead-request refresh precedence", () => {
  for (const boundary of ["owner now", "final iteration"] as const) it(`3282 lead-request spent refresh preserves ${boundary} precedence`, async () => {
    let reports = 0, dispatches = 0, attempts = 0, pauses = 0, holds = 0;
    const reportIterations: number[] = [];
    const { ctx, spies } = makeCtx({
      kind: "issue", interactive: false,
      planApproved: true, approvedPlan: "approved", sessionId: "sess-1",
      completionInterlock: true,
      config: { max_iterations: boundary === "final iteration" ? 1 : 3 },
      reportIteration: async iteration => {
        reportIterations.push(iteration);
        reports++;
        assert.ok(reports <= 2, "only the loop ACK and lead-request refresh are allowed");
        return { completionAttempts: reports === 1 ? 0 : 1, totalWallSeconds: 60,
          usedWallSeconds: reports === 1 ? 0 : 60,
          pauseRequested: reports === 2 && boundary === "owner now" };
      },
      pauseModeRequested: () => reports >= 2 && boundary === "owner now" ? "now" : null,
      recordCompletionAttempt: async () => { attempts++; return { unmet: ["m2"], attemptCount: 1 }; },
      parkForPause: async () => { pauses++; return true; },
      enterCompletionHold: async (reason, options) => {
        holds++;
        assert.equal(boundary, "final iteration");
        assert.equal(reason, "run reached its milestone-scaled implement/review iteration budget without completing");
        assert.equal(options?.allowDegradedCapture, undefined);
        return true;
      },
    });
    const queryFn: SdkQueryFn = params => (async function* () {
      assert.equal(++dispatches, 1, "the refresh must stop before another provider turn");
      for await (const _ of params.prompt) { /* drain */ }
      yield signalDone("sess-1", { owner_decision_requested: { reason: "Need owner input" } });
      yield resultSuccess();
    })();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.equal(reports, 2); assert.deepEqual(reportIterations, [1, 1], "the second ACK must be the lead-request refresh, not a new loop iteration");
    assert.equal(dispatches, 1); assert.equal(attempts, 1);
    assert.equal(pauses, boundary === "owner now" ? 1 : 0);
    assert.equal(holds, boundary === "final iteration" ? 1 : 0);
    assert.equal(Boolean(result.pausedAt), boundary === "owner now");
    assert.equal(Boolean(result.completionHeld), boundary === "final iteration");
    assert.equal(result.walled, undefined); assert.equal(spies.parkForWallCalls, 0);
  });
});
