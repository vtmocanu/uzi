import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { FollowUpOutcome, RunContext } from "../src/executor.js";
import type { IterationBudget } from "../src/protocol.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";

/**
 * PRD #1189 M1 (D6) — the worker honors a SERVED wall-clock extension.
 *
 * The server now serves a run's TOTAL wall budget (budget_total_seconds = COALESCE(
 * budget_wall_seconds, RUN_TIMEOUT) + budget_extension_seconds) on the running-report ACK, as
 * IterationBudget.totalWallSeconds. The sdk-executor re-arms its hard wall UPWARD to the largest
 * served wall it has ever seen (a monotonic high-water mark, maxServedWallMs, that REPLACED the
 * one-shot `wallScaled` boolean latch). The invariant proved here:
 *   - a served value above the current high-water lifts wallRemainingMs by the delta;
 *   - a SECOND, larger served value lifts it AGAIN (the old latch swallowed this — the regression);
 *   - a served value <= the high-water never shortens the wall (an extension only ever grows it);
 *   - totalWallSeconds is PREFERRED over wallSeconds, falling back to wallSeconds for an older api.
 *
 * These are executor-level tests: the wall is armed per turn (armWall/disarmWall) and re-armed at
 * the loop top from the served budget, so the observable effect of the re-arm is that a turn whose
 * in-turn compute would trip the ORIGINAL (or un-re-armed) wall completes instead. Turns burn real
 * in-turn time (the only way scripted turns exercise the wall); the SURVIVE margin is always huge
 * (a served hour vs a sub-second turn) so contention cannot flake it, while the un-re-armed wall a
 * mutation would leave in place is always far below the turn's duration so the guard has teeth.
 *
 * Harness mirrors sdk-executor.test.ts / interactive-task-park.test.ts (their helpers are not
 * exported, so — like interactive-task-park.test.ts — this file carries its own copy).
 */

// ── scripted SDK messages ────────────────────────────────────────────────────
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
      content: [{ type: "tool_use", id: "t1", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }],
    },
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

interface Turn {
  options: SdkOptions;
  promptText?: string;
}

/**
 * A fake `query` that replays one scripted stream per turn. Each turn optionally burns
 * `sleeps[i]` (clamped to the last entry) of REAL wall-clock time in-turn — between consuming the
 * prompt and yielding its messages — which is exactly the window driveTurn arms the wall around
 * (armWall → … → disarmWall), so the sleep is what disarmWall debits. sleeps[0] is the PLAN turn;
 * the loop turns are sleeps[1..]. An empty `sleeps` makes every turn instant.
 */
function fakeTurns(
  scripts: SDKMessage[][],
  sleeps: number[] = [],
): { queryFn: SdkQueryFn; turns: Turn[] } {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    const sleepMs = sleeps.length ? sleeps[Math.min(i, sleeps.length - 1)]! : 0;
    i++;
    const turn: Turn = { options: params.options };
    turns.push(turn);
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        turn.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      if (sleepMs > 0) await new Promise((r) => setTimeout(r, sleepMs));
      for (const m of script) yield m;
    })();
  };
  return { queryFn, turns };
}

let homeDir: string;
let saved: Record<string, string | undefined>;

// A worktree path that never exists (the executor must not require it on disk) with a
// unique-per-call basename to avoid the skills-plugin-dir race sdk-executor.test.ts documents.
let wtSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-extend-wt-${process.pid}-${wtSeq++}`);
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext } {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
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
    ...overrides,
  };
  return { ctx };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-extendhome-"));
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

describe("SdkExecutor served wall extension (PRD #1189 M1)", () => {
  it("(a) mr_rework, armed wall == frozen budget: a served budget_total lifts the wall so a slow turn survives past the original wall", async () => {
    // The acceptance-bar (a) case: an mr_rework run has NO milestone-scaling headroom, so its armed
    // wall IS the frozen budget (initialWallMs = run_timeout_seconds). The 300ms initial wall would
    // trip a 600ms loop turn; the served totalWallSeconds (1h) re-arms the wall upward, so the run
    // drives past the original wall to done. Mutation: drop the `state.wallRemainingMs += servedMs -
    // maxServedWallMs` re-arm (or gate it out for totalWallSeconds) → the loop turn runs on the
    // unscaled 300ms wall and trips REASON_WALL, so run() rejects and the branch assert reddens.
    const { queryFn } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [assistantText("slow rework"), signalDone(), resultSuccess()],
      ],
      [0, 600],
    );
    const { ctx } = makeCtx({
      kind: "mr_rework",
      config: { run_timeout_seconds: 0.3 }, // frozen budget == armed wall; no milestone headroom
      reportIteration: async () => ({ totalWallSeconds: 3600 }),
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "the served total re-armed the wall past the frozen budget");
  });

  it("(b) milestone-scaled issue run: a wallSeconds milestone scale AND a later budget_total extension each re-arm the wall", async () => {
    // The acceptance-bar (b) case: an issue run first gets milestone headroom (served wallSeconds >
    // initialWallMs), then an owner extension arrives as a larger totalWallSeconds. BOTH must lift
    // the wall. iter1's 400ms turn would trip the 300ms initial wall unless the wallSeconds scale
    // fired; iter2's 700ms turn survives only because the totalWallSeconds extension re-armed AGAIN
    // above the milestone-scaled high-water. Deleting either re-arm reddens this.
    const served: IterationBudget[] = [];
    const { queryFn, turns } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [assistantText("milestone 1 work"), resultSuccess()], // iter1: not done
        [assistantText("milestone 2 work"), signalDone(), resultSuccess()], // iter2: done
      ],
      [0, 400, 700],
    );
    let call = 0;
    const { ctx } = makeCtx({
      kind: "issue",
      config: { run_timeout_seconds: 0.3 },
      reportIteration: async () => {
        call++;
        const b: IterationBudget = call === 1 ? { wallSeconds: 1.0 } : { totalWallSeconds: 5 };
        served.push(b);
        return b;
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5");
    assert.strictEqual(turns.length, 3, "plan + two loop turns ran; neither turn tripped the wall");
    assert.deepStrictEqual(
      served,
      [{ wallSeconds: 1.0 }, { totalWallSeconds: 5 }],
      "the milestone scale (wallSeconds) then the extension (totalWallSeconds) were both served",
    );
  });

  it("a SECOND, larger served total re-lifts the wall past the first (the regression the one-shot latch caused)", async () => {
    // The core regression guard. The OLD `wallScaled` boolean latched after the first re-arm and
    // SWALLOWED every later bump; the monotonic maxServedWallMs high-water re-arms on each strictly
    // larger served value. iter1 serves 0.8s (wall → 800ms); the 400ms turn consumes half, leaving
    // ~400ms. iter2 serves 5s — under the latch bug the wall would still be ~400ms and the 700ms
    // turn would trip REASON_WALL; with the monotonic re-arm the wall lifts to ~4.6s and the run
    // completes. Reinstating the one-shot latch reddens this.
    const served: IterationBudget[] = [];
    const { queryFn, turns } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [assistantText("w1"), resultSuccess()], // iter1: not done (400ms, on the first bump)
        [assistantText("w2"), signalDone(), resultSuccess()], // iter2: done (700ms, needs the second bump)
      ],
      [0, 400, 700],
    );
    let call = 0;
    const { ctx } = makeCtx({
      config: { run_timeout_seconds: 0.3 },
      reportIteration: async () => {
        call++;
        const b: IterationBudget = call === 1 ? { totalWallSeconds: 0.8 } : { totalWallSeconds: 5 };
        served.push(b);
        return b;
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "the second, larger served total re-armed the wall again");
    assert.strictEqual(turns.length, 3, "both loop turns ran; the second bump was not swallowed");
    assert.deepStrictEqual(
      served.map((b) => b.totalWallSeconds),
      [0.8, 5],
      "two strictly-increasing totals were served, and the wall grew on each",
    );
  });

  it("a SMALLER served total never shortens the wall (upward-only re-arm)", async () => {
    // The high-water is compared with strict `>`, so a served value BELOW it is inert — the worker
    // never shrinks its own wall (an extension only ever grows it). iter1 serves 5s (wall → ~5s);
    // iter2 serves 0.2s, which is smaller than BOTH the high-water and the 600ms turn that follows.
    // With the guard intact the 600ms turn runs on the ~5s wall and completes; a mutation that
    // OVERWROTE the wall with the served value (`state.wallRemainingMs = servedMs`) would drop it to
    // 200ms and the 600ms turn would trip REASON_WALL, reddening this.
    const { queryFn, turns } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [assistantText("w1"), resultSuccess()], // iter1: not done (instant; wall re-armed to ~5s)
        [assistantText("w2"), signalDone(), resultSuccess()], // iter2: done (600ms, on the un-shortened wall)
      ],
      [0, 0, 600],
    );
    let call = 0;
    const { ctx } = makeCtx({
      config: { run_timeout_seconds: 0.3 },
      reportIteration: async () => {
        call++;
        return call === 1 ? { totalWallSeconds: 5 } : { totalWallSeconds: 0.2 };
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "the smaller served total left the wall un-shortened");
    assert.strictEqual(turns.length, 3, "the 600ms turn survived the un-shortened wall");
  });

  it("prefers totalWallSeconds over wallSeconds when both ride the same ACK", async () => {
    // The re-arm reads `served.totalWallSeconds ?? served.wallSeconds`. This ACK carries BOTH: a
    // tiny wallSeconds (50ms, BELOW the 300ms initial wall, so it would NOT re-arm) and a large
    // totalWallSeconds (1h). The 600ms loop turn survives only if the TOTAL was used. A mutation
    // that read wallSeconds first would leave the wall at 300ms and the 600ms turn would trip.
    const { queryFn } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [assistantText("slow work"), signalDone(), resultSuccess()],
      ],
      [0, 600],
    );
    const { ctx } = makeCtx({
      config: { run_timeout_seconds: 0.3 },
      reportIteration: async () => ({ totalWallSeconds: 3600, wallSeconds: 0.05 }),
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "totalWallSeconds won over the smaller wallSeconds");
  });

  it("falls back to wallSeconds when totalWallSeconds is absent (older api, back-compat)", async () => {
    // An un-upgraded api serves only wallSeconds (totalWallSeconds undefined). The `?? wallSeconds`
    // fallback keeps the PRD #122 M2 behavior: the milestone-scaled wall still re-arms. The 600ms
    // loop turn survives on the served 1h wall. Removing the `?? served.wallSeconds` fallback would
    // leave an older-api run on its unscaled 300ms wall and trip the 600ms turn, reddening this.
    const { queryFn } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()],
        [assistantText("slow work"), signalDone(), resultSuccess()],
      ],
      [0, 600],
    );
    const { ctx } = makeCtx({
      config: { run_timeout_seconds: 0.3 },
      reportIteration: async () => ({ wallSeconds: 3600 }),
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(result.branch, "agent/issue-5", "an older api's wallSeconds still scales the wall");
  });

  it("resets the wall high-water on a follow-up so a later, smaller served total re-arms from initialWallMs", async () => {
    // The follow-up reset (maxServedWallMs = initialWallMs, alongside state.wallRemainingMs =
    // initialWallMs). An interactive run parks on done and resumes on a follow-up; each follow-up is
    // a fresh task on a fresh wall. iter1 serves a HIGH total (2s → high-water 2s). After the park +
    // reset, the resumed turn's ACK serves 0.9s — LESS than the prior high-water (2s) but MORE than
    // initialWallMs (300ms). Only because the reset dropped the high-water back to 300ms does 0.9s
    // count as growth and re-arm the wall to 900ms, so the resumed turn's 450ms burn survives.
    // Mutation: delete `maxServedWallMs = initialWallMs` at the follow-up reset → the high-water
    // stays 2s, 0.9s < 2s is treated as no growth, the resumed turn runs on the reset 300ms wall and
    // its 450ms burn trips REASON_WALL, so run() rejects and this reddens.
    const { queryFn, turns } = fakeTurns(
      [
        [submitPlan("plan"), resultSuccess()], // plan (unscaled 300ms budget, 0 burn)
        [assistantText("t1"), signalDone(), resultSuccess()], // loop1 → done → park (50ms burn on the 2s wall)
        [assistantText("t2"), signalDone(), resultSuccess()], // resumed follow-up → done → park idle (450ms burn)
      ],
      [0, 50, 450],
    );
    const outcomes: FollowUpOutcome[] = [
      { kind: "followup", body: "the resumed task" },
      { kind: "ended", reason: "idle" },
    ];
    let call = 0;
    const { ctx } = makeCtx({
      config: { max_iterations: 30, run_timeout_seconds: 0.3 },
      interactive: true,
      checkpoint: async () => {},
      awaitFollowUp: async () => outcomes.shift()!,
      reportIteration: async () => {
        call++;
        // First task's turn: a high served total (2s). The resumed follow-up's turn: 0.9s — below
        // the prior high-water, above initialWallMs, so it only re-arms if the reset fired.
        return call === 1 ? { totalWallSeconds: 2.0 } : { totalWallSeconds: 0.9 };
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(ctx);
    assert.strictEqual(
      result.branch,
      "agent/issue-5",
      "the resumed follow-up re-armed from the reset high-water and did not trip the wall",
    );
    assert.strictEqual(turns.length, 3, "the resumed follow-up turn ran to completion on a re-armed wall");
  });
});
