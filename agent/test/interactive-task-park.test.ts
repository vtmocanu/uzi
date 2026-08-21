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
});
