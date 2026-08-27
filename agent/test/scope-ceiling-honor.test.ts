import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import type { IterationBudget, Milestone, MilestoneProgress } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// PRD #634 M6 — the WORKER half of run-scope steering: the scope-ceiling HONOR GATE at
// the implement-loop top (sdk-executor.ts, PRD #634 M3/M4). The gate reads the server's
// fresh `{scopeCeiling, completedCount}` off each iteration's `reportIteration` ack and,
// when the completed count has reached the operator's ceiling, finalizes the committed
// slice (sets ExecutorResult.scopeCapped, emits a `steer_ack`) and starts NO further
// milestone — regardless of whether the lead cooperatively checkpointed.
//
// Same harness shape as sdk-executor.test.ts / ask-user-executor.test.ts: `queryFn` is
// faked via fakeTurns([...]), so every path here is provable with dummy credentials and
// NO live Anthropic session. The MCP signals are scripted as tool_use blocks the executor
// observes in the stream (submit_plan carries the frozen milestone list; the honor gate
// is exercised WITHOUT any checkpoint/signal_done tool call, so the ONLY way the loop
// terminates in tests 1/3/4 is the gate itself).

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

// A worktree path UNIQUE PER PROCESS AND PER CALL that deliberately never exists — the
// executor must not require the worktree on disk. A per-file-unique basename is a RACE
// FIX (see the long note in sdk-executor.test.ts): skillsPluginDir() derives a sibling
// `.uzi-skills-<basename>` the executor materializes, and `node --test` runs files
// concurrently, so a literal shared across files races on that dir. This file's basename
// prefix is distinct from the two siblings' on purpose.
let nonexistentWorktreeSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-scope-honor-wt-${process.pid}-${nonexistentWorktreeSeq++}`);
}

// --- scripted SDK messages ---------------------------------------------------

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
// submit_plan carrying a frozen milestone list (PRD #122 M1 shape). The list length is
// what ExecutorResult.scopeCapped.total reports, so tests assert against it.
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

// Six frozen milestones so `total` is a distinct, checkable number in the scope tests.
const SIX_MILESTONES: Milestone[] = [
  { id: "m1", title: "milestone one" },
  { id: "m2", title: "milestone two" },
  { id: "m3", title: "milestone three" },
  { id: "m4", title: "milestone four" },
  { id: "m5", title: "milestone five" },
  { id: "m6", title: "milestone six" },
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

interface CheckpointCall {
  reap: boolean;
  progress?: MilestoneProgress;
}

interface Probe {
  ctx: RunContext;
  emits: EmittedMessage[];
  iterations: number[];
  checkpoints: CheckpointCall[];
}

/**
 * Minimal RunContext builder (copied from the sibling executor tests, trimmed to what the
 * implement loop reads). `kind` is left absent, so `isIssueRun` is true by default — the
 * honor gate is issue-run-only. `reportIteration` is the controlled per-iteration budget
 * source: it returns the `{scopeCeiling, completedCount}` the gate reads.
 */
function makeCtx(
  overrides: Partial<RunContext> = {},
  verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } },
): Probe {
  const emits: EmittedMessage[] = [];
  const iterations: number[] = [];
  const checkpoints: CheckpointCall[] = [];
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
    // Spy on the checkpoint boundary. The loop's own iteration-boundary FALLBACK calls
    // this with reap:false; a COOPERATIVE (model-driven) checkpoint calls it with
    // reap:true. Tests discriminate on `reap`.
    checkpoint: async (opts) => {
      checkpoints.push({ reap: opts.reap, progress: opts.progress });
    },
    // Default: report the iteration but serve no budget (gate never fires). Individual
    // tests override this to drive scopeCeiling/completedCount.
    reportIteration: async (n) => {
      iterations.push(n);
      return undefined;
    },
    ...overrides,
  };
  return { ctx, emits, iterations, checkpoints };
}

let homeDir: string;
let saved: Record<string, string | undefined>;

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-scopehome-"));
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

describe("PRD #634 M6 — worker scope-ceiling honor gate", () => {
  // Test 1 (finding-2's fragile case). The gate MUST finalize at the ceiling even when the
  // lead never cooperatively checkpointed on the honoring turn — that is the whole point of
  // placing it at the loop top rather than in the checkpoint tool. The turns never call the
  // checkpoint tool nor signal_done, so the ONLY way the loop terminates is the gate.
  it("finalizes at the ceiling with NO cooperative checkpoint and starts no further milestone", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", SIX_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), resultSuccess()], // loop iter 1 (completed 2/4)
      [assistantText("iteration 2 work"), resultSuccess()], // loop iter 2 (completed 3/4)
      // iter 3 never drives a turn: the gate fires at the loop top before driveTurn.
    ]);
    // completedCount climbs to the ceiling on iteration 3.
    const budgets: Record<number, IterationBudget> = {
      1: { scopeCeiling: 4, completedCount: 2 },
      2: { scopeCeiling: 4, completedCount: 3 },
      3: { scopeCeiling: 4, completedCount: 4 }, // >= ceiling → gate fires
    };
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return budgets[n];
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // Terminated via the gate: scopeCapped is set with the ceiling-reaching count and the
    // frozen list length as total.
    assert.ok(result.scopeCapped, "the run must finalize via the scope honor gate (scopeCapped set)");
    assert.equal(result.scopeCapped!.completedCount, 4, "completedCount is the count that reached the ceiling");
    assert.equal(result.scopeCapped!.total, SIX_MILESTONES.length, "total is the frozen milestone list length");
    assert.equal(result.branch, "agent/issue-5", "the committed slice is finalized on the branch");

    // The gate fired at loop-top on iteration 3 BEFORE a third implement turn ran: exactly
    // two implement turns drove (iterations 1 and 2), so turns = 1 plan + 2 implement.
    assert.deepEqual(probe.iterations, [1, 2, 3], "reportIteration ran through the honoring iteration");
    assert.equal(turns.length, 3, "one planning turn + two implement turns; NO turn for a 5th/6th milestone");

    // No COOPERATIVE checkpoint fired on the honoring turn — the turns never called the
    // checkpoint tool. Only the loop's own iteration-boundary fallbacks (reap:false) ran,
    // one after each of the two implement turns.
    assert.ok(
      !probe.checkpoints.some((c) => c.reap === true),
      "no cooperative (reap:true) checkpoint may fire; the gate must not depend on one",
    );
    assert.equal(probe.checkpoints.length, 2, "only the two loop-fallback (reap:false) checkpoints ran");
    assert.ok(probe.checkpoints.every((c) => c.reap === false), "the fallback checkpoints are reap:false");
  });

  // Test 1b (PRD #634 follow-up M1 — the ceiling-0 STOP). A `uzi run stop` before ANY
  // milestone completes maps to scope_ceiling=0, and the run's lead never reported progress,
  // so the state ACK carries milestones_completed:null ⇒ completedCount 0 (see
  // read-run-ack.test.ts). completedCount 0 >= ceiling 0 must fire the gate at the loop top
  // on the very FIRST iteration, before any implement turn drives. Regression: when null read
  // as `undefined` the gate's `typeof completedCount === "number"` was false and the STOP was
  // silently dropped.
  it("finalizes immediately at a ceiling of 0 (uzi run stop before any milestone)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", SIX_MILESTONES), resultSuccess()], // planning turn
      // NO implement turn is queued: the gate must fire at the loop top on iteration 1.
    ]);
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return { scopeCeiling: 0, completedCount: 0 }; // 0 >= 0 → fire before the first turn
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // Terminated via the gate with a completed count of 0 and the frozen list length as total.
    assert.ok(result.scopeCapped, "the ceiling-0 STOP must finalize via the scope honor gate");
    assert.equal(result.scopeCapped!.completedCount, 0, "no milestone completed before the STOP");
    assert.equal(result.scopeCapped!.total, SIX_MILESTONES.length, "total is the frozen milestone list length");
    assert.equal(result.branch, "agent/issue-5", "the (empty) committed slice is finalized on the branch");

    // The gate fired at loop-top on iteration 1 BEFORE any implement turn: only the planning
    // turn drove, and reportIteration ran exactly once.
    assert.deepEqual(probe.iterations, [1], "reportIteration ran the honoring iteration only");
    assert.equal(turns.length, 1, "only the planning turn drove; NO implement turn ran");

    // Exactly one steer_ack was emitted at the gate.
    const acks = probe.emits.filter((m) => m.kind === "steer_ack");
    assert.equal(acks.length, 1, "exactly one steer_ack is emitted when the gate fires");
    assert.equal(acks[0]!.payload["completed"], 0, "the ack carries the completed count of 0");
    assert.equal(acks[0]!.payload["ceiling"], 0, "the ack carries the honored ceiling of 0");
  });

  // Test 2. Below the ceiling the gate is inert: the loop proceeds normally and completes
  // on signal_done, leaving scopeCapped absent.
  it("does NOT fire below the ceiling; the run completes normally via signal_done", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", SIX_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), resultSuccess()], // loop iter 1 (2/4, proceeds)
      [assistantText("iteration 2 work"), signalDone(), resultSuccess()], // loop iter 2 → done
    ]);
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return { scopeCeiling: 4, completedCount: 2 }; // 2 < 4 every iteration
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.branch, "agent/issue-5", "the run completed via signal_done");
    assert.equal(result.scopeCapped, undefined, "the gate must NOT cap a run below the ceiling");
    assert.deepEqual(probe.iterations, [1, 2], "the loop ran two normal iterations then finished on done");
    assert.ok(
      !probe.emits.some((m) => m.kind === "steer_ack"),
      "no steer_ack when the gate never fires",
    );
  });

  // Test 3 (SC2 supersede). The worker honors the LATEST ceiling read EACH iteration. The
  // operator raises the ceiling to 5 before the count reaches 4, so the gate must NOT fire
  // at completedCount=4 (4 < 5) and MUST fire once the count reaches the new ceiling of 5.
  it("honors a freshly-read raised ceiling: does not fire at 4/5, fires at 5/5", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones("# Plan", SIX_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), resultSuccess()], // loop iter 1 (3, ceiling 4)
      [assistantText("iteration 2 work"), resultSuccess()], // loop iter 2 (4, ceiling RAISED to 5)
      // iter 3: count 5 >= raised ceiling 5 → gate fires at loop top, no turn.
    ]);
    const budgets: Record<number, IterationBudget> = {
      1: { scopeCeiling: 4, completedCount: 3 }, // 3 < 4, proceed
      2: { scopeCeiling: 5, completedCount: 4 }, // ceiling raised to 5; 4 < 5, MUST NOT fire
      3: { scopeCeiling: 5, completedCount: 5 }, // 5 >= 5, fire on the superseding ceiling
    };
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return budgets[n];
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.scopeCapped, "the run finalizes once the count reaches the raised ceiling");
    assert.equal(
      result.scopeCapped!.completedCount,
      5,
      "the gate fires at completedCount=5 against the superseding ceiling of 5, not at 4/5",
    );
    // iteration 2 (count 4, ceiling 5) drove a real turn, proving the gate did not fire at
    // the stale ceiling of 4: turns = 1 plan + 2 implement, and iteration 3 ran the ack.
    assert.deepEqual(probe.iterations, [1, 2, 3], "reportIteration ran through iteration 3");
    assert.equal(turns.length, 3, "iteration 2 drove a turn (4/5 did not cap); the gate fired only at 5/5");
  });

  // Test 4. When the gate fires it emits a structured steer_ack (PRD #634 M4) so the
  // operator sees the scope directive was applied without parsing run state.
  it("emits a steer_ack with directive:scope carrying the ceiling and completed count", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", SIX_MILESTONES), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), resultSuccess()], // loop iter 1 (1/2, proceeds)
      // iter 2: 2/2 → gate fires.
    ]);
    const budgets: Record<number, IterationBudget> = {
      1: { scopeCeiling: 2, completedCount: 1 },
      2: { scopeCeiling: 2, completedCount: 2 }, // >= ceiling → fire
    };
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return budgets[n];
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.scopeCapped, "the gate fired");
    const ack = probe.emits.find((m) => m.kind === "steer_ack");
    assert.ok(ack, "a steer_ack run-message must be emitted at the gate");
    assert.equal(ack!.agent, "worker", "the steer_ack is attributed to the worker");
    assert.equal(ack!.payload["directive"], "scope", "the directive is `scope`");
    assert.equal(ack!.payload["ceiling"], 2, "the ack carries the honored ceiling");
    assert.equal(ack!.payload["completed"], 2, "the ack carries the completed count");
    assert.match(
      String(ack!.payload["text"]),
      /finalizing at 2 completed/,
      "the human-readable text names the completed count it is finalizing at",
    );
  });

  // Test 4b (PRD #634 M3 P3a — overshoot). When the lead completes SEVERAL milestones in
  // one turn, the served completedCount can EXCEED the ceiling (gate is `>=`), so the ack
  // text must read sensibly rather than a reversed ratio like "(4/3)". Here the lead
  // completed 4 against a ceiling of 3: the gate fires, scopeCapped carries 4, and the
  // text names "finalizing at 4 completed … ceiling was 3" — never a nonsensical ratio.
  it("reads sensibly when completedCount OVERSHOOTS the ceiling (lead did 4 in one turn, ceiling 3)", async () => {
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones("# Plan", SIX_MILESTONES), resultSuccess()], // planning turn
      // iter 1: served 4/3 → 4 >= 3, gate fires at the loop top, no turn.
    ]);
    const probe = makeCtx({
      reportIteration: async (n) => {
        probe.iterations.push(n);
        return { scopeCeiling: 3, completedCount: 4 }; // 4 > 3: overshoot in one turn
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.ok(result.scopeCapped, "the gate fires when the count overshoots the ceiling");
    assert.equal(
      result.scopeCapped!.completedCount,
      4,
      "the finalized count is the served 4, not clamped to the ceiling of 3",
    );
    const ack = probe.emits.find((m) => m.kind === "steer_ack");
    assert.ok(ack, "a steer_ack is emitted on overshoot too");
    assert.equal(ack!.payload["ceiling"], 3, "the ack still carries the honored ceiling of 3");
    assert.equal(ack!.payload["completed"], 4, "the ack carries the overshot completed count of 4");
    const text = String(ack!.payload["text"]);
    assert.match(text, /finalizing at 4 completed/, "the text names the completed count sensibly");
    assert.match(text, /ceiling was 3/, "the text names the operator ceiling as context, not a ratio");
    assert.doesNotMatch(text, /\(4\/3\)/, "the text must NOT print a reversed/nonsensical ratio");
  });

  // Test 5. The advisory pullFollowUp path is untouched by the honor gate: with NO numeric
  // scopeCeiling served, the gate never fires, the follow-up is folded into the next turn
  // exactly as before, and the run completes on signal_done with scopeCapped absent.
  it("leaves the advisory follow-up path unchanged when no scope ceiling is served", async () => {
    const followUps = ["please also add tests"];
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()], // planning turn
      [assistantText("first pass"), resultSuccess()], // loop iter 1 (no done) → pulls follow-up
      [assistantText("second pass"), signalDone(), resultSuccess()], // loop iter 2 carries the follow-up, done
    ]);
    let served = 0;
    const probe = makeCtx({
      pullFollowUp: () => followUps.shift(),
      reportIteration: async (n) => {
        served++;
        probe.iterations.push(n);
        return undefined; // no scopeCeiling → the honor gate can never fire
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.branch, "agent/issue-5", "the run completed via signal_done");
    assert.equal(result.scopeCapped, undefined, "the gate never fires without a numeric scopeCeiling");
    assert.ok(
      !probe.emits.some((m) => m.kind === "steer_ack"),
      "no steer_ack on the advisory path",
    );
    // The follow-up was folded into iteration 2's prompt as UNTRUSTED user input — the
    // pre-existing behavior, unchanged (turns[0]=plan, turns[1]=iter1, turns[2]=iter2).
    assert.match(turns[2]!.promptText ?? "", /please also add tests/, "the follow-up rode the next turn");
    assert.doesNotMatch(turns[1]!.promptText ?? "", /please also add tests/, "iter 1 did not carry it");
    assert.ok(served >= 2, "the loop reported at least two iterations before completing");
  });

  // Test 6 (the isIssueRun guard's negative). The honor gate is `if (isIssueRun && served &&
  // typeof served.scopeCeiling === "number" && ... completedCount >= scopeCeiling)`
  // (sdk-executor.ts). Every test above is an issue run (makeCtx leaves `kind` absent, so
  // isIssueRun is true), so a regression deleting `isIssueRun &&` would pass them all. Here
  // a NON-issue run (kind:"prompt" ⇒ isIssueRun false) is served a budget that WOULD fire
  // the gate on the very first loop iteration if the guard were gone (completedCount ===
  // scopeCeiling). It must NOT fire: the run proceeds and completes via its normal
  // signal_done path, scopeCapped absent, no steer_ack. The same non-issue-kind harness the
  // sibling findings test uses (sdk-executor.test.ts) drives kind through this exact loop.
  it("does NOT fire on a non-issue run even at the ceiling (gate is scoped to issue runs)", async () => {
    const { queryFn, turns } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()], // planning turn
      [assistantText("iteration 1 work"), signalDone(), resultSuccess()], // loop iter 1 → done
    ]);
    const probe = makeCtx({
      // kind:"prompt" makes isIssueRun false — the whole point of this test. Every other
      // ctx field is a normal run; only the guard discriminator changes.
      kind: "prompt",
      reportIteration: async (n) => {
        probe.iterations.push(n);
        // completedCount === scopeCeiling: on an ISSUE run this fires the gate at loop top
        // BEFORE the first implement turn. On a non-issue run the guard must skip it.
        return { scopeCeiling: 2, completedCount: 2 };
      },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // The gate did not fire: scopeCapped is absent and no steer_ack was emitted.
    assert.equal(result.scopeCapped, undefined, "the honor gate is issue-run-only; a non-issue run must not cap");
    assert.ok(
      !probe.emits.some((m) => m.kind === "steer_ack"),
      "no steer_ack on a non-issue run — the gate never fired",
    );
    // The run completed via its normal path: the implement turn DROVE (the gate did not
    // break at loop top before it), then signal_done ended it. If the guard were removed,
    // the gate would break on iteration 1 before any implement turn — turns.length would be
    // 1 and scopeCapped would be set.
    assert.equal(turns.length, 2, "one planning turn + one implement turn drove (gate did not fire at loop top)");
    assert.deepEqual(probe.iterations, [1], "iteration 1 ran a real turn and signaled done; the gate did not cap it");
  });
});
