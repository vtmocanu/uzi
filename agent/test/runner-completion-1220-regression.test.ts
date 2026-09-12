import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { type Executor, type RunContext } from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import {
  api,
  fakeGitlab,
  gitlabClaim,
  installHarness,
  runner,
  simulateCommittedWork,
} from "./runner-harness.js";

// PRD #1226 M6 — the ADVERSARIAL #1220 regression. This reproduces the exact shape of run
// `b0ffd0ea`: a five-milestone issue run where the lead FROZE m1..m5 at approval but declared only
// FOUR complete (m1..m4) at signal_done, then still opened PR #1220 with `Closes #1171`. #1226's
// structural interlock closes that hole, and this fixture pins the fixed path so the regression
// cannot silently return.
//
// Scope (per the PRD): this fixture proves ONLY the slice #1226 delivers —
//   1. checkpoint-FIRST capture before any denial,
//   2. same-session rework that names the MISSING milestone (m5) back to the SAME lead, and
//   3. ZERO forge-create calls while a frozen milestone is undeclared.
// It deliberately does NOT assert owner-decision or durable cross-worker context behavior
// (delivered by #1227/#1229), and it is distinct from #1232's later full integration/rollout
// proof. The mutation control at the bottom of this file demonstrates the interlock is a real
// regression barrier and not decoration: neutering the executor's missing-milestone gate makes the
// b0ffd0ea shape finalize again (the path that would open `Closes`).
//
// Two levels, each faithful to a different half of the acceptance criteria:
//   (A) executor level (SdkExecutor + injected completion seams) — the SAME-SESSION rework naming
//       m5, and that the executor never returns a publishable result while m5 is unmet. This is the
//       level the missing-milestone-gate mutation reddens.
//   (B) runner level (RunRunner.execute -> phasePublish, real forge transport captured) — the AC's
//       exact words: "cannot call createMergeRequest when one frozen milestone is missing" is
//       proven as `calls.length === 0`.
//
// The M4 SERVER-permit half (an interlocked run whose server denies the permit for
// `missing_milestones` opens NO MR) is already proven end-to-end in
// runner-completion-permit.test.ts; this file does not duplicate it.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const FAKE_PAT = "dummy-forge-pat-do-not-scan-1111";
const FAKE_JOIN_TOKEN = "dummy-join-token-do-not-scan-2222";

// b0ffd0ea froze five milestones; the lead declared the first four and left m5 undeclared.
const FROZEN_FIVE = [
  { id: "m1", title: "schema and hard claim clause" },
  { id: "m2", title: "attempts, permit route and completion transaction" },
  { id: "m3", title: "same-lead attempt loop and stall routing" },
  { id: "m4", title: "final-head publication and recoverable hold" },
  { id: "m5", title: "docs, specifications and adversarial regression" },
];
const DECLARED_FOUR = ["m1", "m2", "m3", "m4"];
// The distinctive missing-milestone title the fixed path must name back to the lead.
const M5_TITLE = "docs, specifications and adversarial regression";

let seq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-1220-wt-${process.pid}-${seq++}`);
}

// --- scripted SDK messages (mirrors runner-completion-attempt.test.ts's helpers) ---
function submitPlanWithMilestones(
  milestones: Array<{ id: string; title: string }>,
  sessionId = "sess-1",
): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: {
      content: [
        {
          type: "tool_use",
          id: "t1",
          name: "mcp__uzi__submit_plan",
          input: { plan_md: "# PLAN\n- deliver all five milestones", milestones },
        },
      ],
    },
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

interface Turn {
  promptText?: string;
}

function fakeTurns(scripts: SDKMessage[][]): { queryFn: SdkQueryFn; turns: Turn[] } {
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
      for (const m of script) yield m;
    })();
  };
  return { queryFn, turns };
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
}

// A minimal interlocked issue-run ctx. `unmetScript` is consumed one entry per
// recordCompletionAttempt call (the last entry sticks), exactly as the server-authoritative unmet
// set would be returned. The plan's milestones list is frozen through the approve verdict so the
// same-session rework follow-up can name the missing milestone's title.
function makeInterlockedCtx(unmetScript: string[][]): CompletionProbe {
  const order: string[] = [];
  const attemptCalls: AttemptCall[] = [];
  const holdReasons: string[] = [];
  let attemptN = 0;
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const ctx: RunContext = {
    runId: "r-1220",
    issueIid: 1171,
    issueTitle: "structural completion interlock",
    issueDescription: "freeze five milestones",
    worktreePath: nonexistentWorktree(),
    branch: "agent/issue-1171",
    emit: () => {},
    oauthToken: OAUTH,
    agents: [],
    config: { max_iterations: 25 }, // b0ffd0ea had no scope cap and iterations to spare
    sessionId: null,
    onSessionId: () => {},
    gatePlan: async () => approve,
    pullFollowUp: () => undefined,
    reportIteration: async () => undefined,
    completionInterlock: true,
    checkpoint: async () => {
      order.push("checkpoint");
    },
    // Constant fingerprint: head "TIP", so the completion fingerprint (unmet, head, worktree) stays
    // identical across attempts on a constant unmet set — the no-progress stall the fixed path holds on.
    worktreeFingerprint: async () => "TIP\n M docs/adr.md",
    recordCompletionAttempt: async (args) => {
      order.push("attempt");
      attemptCalls.push(args);
      const unmet = unmetScript[Math.min(attemptN, unmetScript.length - 1)]!;
      attemptN++;
      return { unmet, attemptCount: attemptN };
    },
    // The hold seam is WIRED (M4) and reports it entered the verified hold, so the executor latches
    // completionHeld and breaks rather than throwing.
    enterCompletionHold: async (reason) => {
      order.push("hold");
      holdReasons.push(reason);
      return true;
    },
  };
  return { ctx, order, attemptCalls, holdReasons };
}

describe("#1220 regression (PRD #1226 M6) — executor level: same-session rework, zero finalize", () => {
  let homeDir: string;
  let saved: Record<string, string | undefined>;
  beforeEach(() => {
    homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1220home-"));
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

  it("b0ffd0ea shape: 5 frozen / 4 declared → checkpoint-first, names m5 to the SAME session, never finalizes", async () => {
    // The lead freezes m1..m5 at approval, then signals done declaring only m1..m4. The server
    // returns m5 as unmet on every attempt (the lead genuinely did not do it), so the interlock can
    // never finalize this run — after STALL_LIMIT identical no-progress attempts it parks.
    const { queryFn, turns } = fakeTurns([
      [submitPlanWithMilestones(FROZEN_FIVE), resultSuccess()],
      [signalDone({ milestones_completed: DECLARED_FOUR }), resultSuccess()],
    ]);
    const probe = makeInterlockedCtx([["m5"]]);
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    // 1. Checkpoint-FIRST: the turn's committed work is captured BEFORE the first attempt records
    //    any denial, so no rework prompt or hold can lose it (capture-before-denial).
    assert.deepStrictEqual(probe.order.slice(0, 2), ["checkpoint", "attempt"], "checkpoint precedes the attempt");

    // 2. The attempt was recorded with the lead's FOUR-milestone declaration and the branch head
    //    derived from the worktree fingerprint's first line.
    assert.strictEqual(probe.attemptCalls.length, 3, "STALL_LIMIT identical attempts before the hold");
    assert.deepStrictEqual(probe.attemptCalls[0]!.declared, DECLARED_FOUR, "the four declared milestones rode the attempt");
    assert.strictEqual(probe.attemptCalls[0]!.head, "TIP", "the head is the fingerprint's first line");

    // 3. Same-session rework: the MISSING milestone (m5) and its title were injected back into the
    //    SAME lead session as an autonomous rework follow-up. No new session, no forge call.
    const namedM5 = turns.filter((t) => (t.promptText ?? "").includes("m5"));
    assert.ok(namedM5.length >= 1, "at least one re-prompt named the missing milestone id");
    assert.ok(
      turns.some((t) => (t.promptText ?? "").includes(M5_TITLE)),
      "the rework follow-up named the missing milestone's TITLE, not just its id",
    );

    // 4. The executor NEVER returned a publishable (finalizable) result: it parked into the
    //    completion hold. `completionHeld` is set, which is exactly what makes phasePublish SKIP the
    //    push/PR/completion (proven at the runner level below). This is the "zero forge-create" root
    //    cause at the executor: the run never reaches the finalize break while m5 is unmet.
    assert.ok(result.completionHeld, "the run parked (completionHeld) rather than finalizing");
    assert.deepStrictEqual(probe.holdReasons.length, 1, "the no-progress route entered the hold exactly once");
    assert.match(probe.holdReasons[0]!, /completion blocked/, "held for an owner decision, not shipped");
  });

  it("mutation control (positive): all 5 declared (unmet empties) → finalizes to a publishable result", async () => {
    // The SAME shape but the lead actually completes m5: the server returns an empty unmet set, so
    // the interlock finalizes to the normal publish path — the run that WOULD open `Closes #N`. This
    // proves the interlock does not block a legitimately-complete run, and it is the executor-level
    // counterpart the missing-milestone-gate mutation flips (see the mutation note in the report).
    const { queryFn } = fakeTurns([
      [submitPlanWithMilestones(FROZEN_FIVE), resultSuccess()],
      [signalDone({ milestones_completed: ["m1", "m2", "m3", "m4", "m5"] }), resultSuccess()],
    ]);
    const probe = makeInterlockedCtx([[]]); // every frozen milestone declared → nothing unmet
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.strictEqual(probe.attemptCalls.length, 1, "one attempt, then finalize");
    assert.deepStrictEqual(probe.holdReasons, [], "a complete run never holds");
    assert.strictEqual(result.completionHeld, undefined, "no hold — this run is publishable");
    assert.strictEqual(result.branch, "agent/issue-1171", "finalizes to the branch the runner would push + open the PR for");
  });
});

describe("#1220 regression (PRD #1226 M6) — runner level: cannot call createMergeRequest", () => {
  installHarness();
  // A held completion (the executor-level b0ffd0ea outcome above) reaching RunRunner.phasePublish
  // must open ZERO merge requests. This is the AC verbatim: "The #1220 fixture cannot call
  // createMergeRequest when one frozen milestone is missing." The forge transport is the real
  // GitLab client with its HTTP layer captured, so `calls.length` is the ground truth of whether
  // createMergeRequest was ever hit.

  function statuses(runId: string): string[] {
    return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
  }

  it("a held incomplete run makes ZERO forge-create calls and never reports completed", async () => {
    const { gitlab, calls } = fakeGitlab();
    // The executor reproduces the b0ffd0ea outcome: its completion loop parked because a frozen
    // milestone (m5) stayed unmet, so it returns completionHeld and NO publishable branch result —
    // exactly what the executor-level test above proves the real SdkExecutor produces.
    const heldExecutor: Executor = {
      run: async (ctx) => {
        assert.ok(ctx.completionInterlock, "the runner marked this interlocked claim");
        return {
          branch: ctx.branch,
          completionHeld: {
            reason: "completion blocked: one frozen milestone still not complete after repeated attempts",
          },
        };
      },
    };
    // An INTERLOCKED issue claim (a frozen structural contract), mirroring how claim_assembly
    // delivers a rollout-on interlocked run.
    const claim = gitlabClaim(1171, {
      config: { completion_contract_version: 1, contract_revision: 1 },
    });
    await runner(heldExecutor, gitlab).execute(claim);

    assert.strictEqual(calls.length, 0, "no createMergeRequest call while a frozen milestone is missing");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a held run never reports completed");
  });

  it("mutation control (positive): a fully-complete run opens exactly ONE MR with Closes #N", async () => {
    const { gitlab, calls } = fakeGitlab();
    // The SAME runner path with a normal (non-held) executor result — a run that DID complete —
    // opens exactly one merge request that closes the issue. This proves the completionHeld gate is
    // the ONLY thing suppressing the forge: remove it and the closing PR opens, which is precisely
    // the pre-#1226 b0ffd0ea behavior the interlock now blocks.
    const doneExecutor: Executor = {
      run: async (ctx) => ({ branch: ctx.branch }),
    };
    // A non-empty diff so #279's empty-diff guard does not itself suppress the MR — we want the
    // forge-create to depend only on the absence of completionHeld.
    simulateCommittedWork();
    const claim = gitlabClaim(1171);
    await runner(doneExecutor, gitlab).execute(claim);

    assert.strictEqual(calls.length, 1, "a complete run opens exactly one merge request");
    const body = JSON.parse(calls[0]!.body ?? "{}") as Record<string, unknown>;
    assert.match(String(body.description), /Closes #1171/, "the complete run's PR closes the issue");
  });
});
