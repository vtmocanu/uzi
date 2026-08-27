import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  StubExecutor,
  STUB_INFLIGHT_SENTINEL,
  STUB_INTERLEAVE_SENTINEL,
  STUB_INTERLEAVE_STREAM,
  STUB_LOOP_SENTINEL,
  STUB_STALL_SENTINEL,
  type EmittedMessage,
  type RunContext,
} from "../src/executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { disableAutoMaintenance } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

// A throwaway git worktree the stub can write its marker into and commit. No
// origin, no network — run() only makes a LOCAL commit (push + MR is the runner).
function makeWorktree(): { path: string; cleanup: () => void } {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-stub-wt-"));
  const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
  execFileSync("git", ["init", "-b", "main", dir], { env, stdio: "pipe" });
  // The stub COMMITS in here, and a commit ends by spawning a detached
  // `git maintenance run --auto` that outlives it and keeps writing inside .git —
  // same teardown race as the repo fixture (issue #127).
  disableAutoMaintenance(dir, env);
  // No rmSync retry here, deliberately: disableAutoMaintenance above suppresses the only
  // git writer in this dir (the stub's commit, executor.ts:470), and the skills-plugin dir
  // is a sibling OUTSIDE it. A retry would guard nothing identifiable while adding up to
  // 2750 ms of blocking sleep per stuck directory on a real failure (issue #127 review).
  return { path: dir, cleanup: () => fs.rmSync(dir, { recursive: true, force: true }) };
}

function makeCtx(overrides: Partial<RunContext> = {}): { ctx: RunContext; emitted: EmittedMessage[] } {
  const emitted: EmittedMessage[] = [];
  const ctx: RunContext = {
    runId: "run-stub-interleave",
    issueIid: 7,
    issueTitle: "E2E interleave",
    issueDescription: "implements prds/43-intra-run-parallel-subagents.md",
    worktreePath: "",
    branch: "agent/issue-7",
    emit: (m) => emitted.push(m),
    ...overrides,
  };
  return { ctx, emitted };
}

// The scripted frames only (worker infra status/text messages filtered out), in
// emit order, projected to the fields the E2E later asserts on after persistence.
function scripted(emitted: EmittedMessage[]): { agent: string | undefined; step: unknown }[] {
  return emitted
    .filter((m) => typeof (m.payload as Record<string, unknown>).step === "number")
    .map((m) => ({ agent: m.agent, step: (m.payload as Record<string, unknown>).step }));
}

describe("StubExecutor — PRD #43 M5 interleaved multi-agent stream", () => {
  it("emits the scripted interleaved stream in order with per-agent attribution", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      issueDescription: `implements prds/43-intra-run-parallel-subagents.md ${STUB_INTERLEAVE_SENTINEL}`,
    });
    try {
      await new StubExecutor(nullLogger()).run(ctx);
    } finally {
      wt.cleanup();
    }

    // Exactly the scripted frames, in emit order, each with the right agent + 1-based step.
    assert.deepStrictEqual(
      scripted(emitted),
      STUB_INTERLEAVE_STREAM.map((f, i) => ({ agent: f.agent, step: i + 1 })),
      "the emitted stream must match STUB_INTERLEAVE_STREAM exactly (order + attribution)",
    );

    // The interleave is real: at least one agent name recurs NON-ADJACENTLY (the
    // property that makes name-based attribution non-trivial to preserve).
    const agents = STUB_INTERLEAVE_STREAM.map((f) => f.agent);
    const hasNonAdjacentRepeat = agents.some(
      (a, i) => agents.indexOf(a) < i - 1 && agents[i - 1] !== a,
    );
    assert.ok(hasNonAdjacentRepeat, "the script must repeat at least one agent name non-adjacently");
  });

  // PRD #99 M2: the same scripted stream now carries per-instance attribution, so
  // M3/M5 have a real two-parallel-coders fixture to render and assert against.
  it("carries two DISTINCT coder instances with distinct labels, non-adjacently", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      issueDescription: `implements prds/43-intra-run-parallel-subagents.md ${STUB_INTERLEAVE_SENTINEL}`,
    });
    try {
      await new StubExecutor(nullLogger()).run(ctx);
    } finally {
      wt.cleanup();
    }

    const frames = emitted.filter((m) => typeof (m.payload as Record<string, unknown>).step === "number");
    const coders = frames.filter((m) => m.agent === "coder");
    assert.equal(coders.length, 2, "the fixture must have exactly two coder invocations");
    assert.notEqual(
      coders[0]?.agentInstance,
      coders[1]?.agentInstance,
      "the two coders must be DISTINCT invocations — this is the merge the PRD exists to prevent",
    );
    assert.ok(coders[0]?.agentInstance && coders[1]?.agentInstance, "both coder frames must carry an instance id");
    assert.notEqual(coders[0]?.agentLabel, coders[1]?.agentLabel, "each invocation must be named by its own task");

    // Non-adjacency is the load-bearing property: two CONTIGUOUS coder frames
    // would render correctly even under today's consecutive-author grouping, so a
    // contiguous fixture could not catch the bug this PRD fixes.
    const idxA = frames.indexOf(coders[0]!);
    const idxB = frames.indexOf(coders[1]!);
    assert.ok(idxB - idxA > 1, "the two coder invocations must be separated by another agent's frame");

    // The lead is the parentless actor: it carries neither key, which is the NULL
    // side of the fixture (and what the web's role-name fallback renders).
    for (const lead of frames.filter((m) => m.agent === "lead")) {
      const rec = lead as unknown as Record<string, unknown>;
      assert.ok(!("agentInstance" in rec), "a lead frame must not carry an agentInstance key");
      assert.ok(!("agentLabel" in rec), "a lead frame must not carry an agentLabel key");
    }
  });

  it("emits NO scripted frames when the sentinel is absent (no leak into normal runs)", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path });
    try {
      await new StubExecutor(nullLogger()).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.deepStrictEqual(scripted(emitted), [], "a run without the sentinel must emit no scripted frames");
  });
});

describe("StubExecutor — PRD #47 M6 run-health sentinels", () => {
  // healthPauseMs: 0 makes the stall/loop/in-flight pauses instant so the unit test
  // runs fast; the real E2E leaves it at the STUB_HEALTH_* defaults.
  const runWith = async (sentinel: string) => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path, issueDescription: `x ${sentinel}` });
    try {
      await new StubExecutor(nullLogger(), { healthPauseMs: 0 }).run(ctx);
    } finally {
      wt.cleanup();
    }
    return emitted;
  };
  const tools = (emitted: EmittedMessage[], kind: "tool_use" | "tool_result") =>
    emitted.filter((m) => m.kind === kind);

  it("UZI_STUB_LOOP emits four IDENTICAL tool calls (name+input) so the window hash flags looping", async () => {
    const emitted = await runWith(STUB_LOOP_SENTINEL);
    const uses = tools(emitted, "tool_use");
    assert.equal(uses.length, 4, "four tool_use calls");
    const fingerprints = new Set(
      uses.map((m) => JSON.stringify([m.payload.name, m.payload.input])),
    );
    assert.equal(fingerprints.size, 1, "all four calls share one name+input fingerprint");
    // Each call has its matching result, so none is left in flight.
    assert.equal(tools(emitted, "tool_result").length, 4, "each call has a result");
  });

  it("UZI_STUB_INFLIGHT emits a tool_use held open past the pause, then its result", async () => {
    const emitted = await runWith(STUB_INFLIGHT_SENTINEL);
    const [use] = tools(emitted, "tool_use");
    const [res] = tools(emitted, "tool_result");
    assert.ok(use && res, "exactly one tool_use and one tool_result");
    assert.equal(tools(emitted, "tool_use").length, 1, "exactly one tool_use");
    assert.equal(tools(emitted, "tool_result").length, 1, "exactly one tool_result");
    // The use precedes its result and they share an id (the detector matches on it).
    assert.ok(emitted.indexOf(use) < emitted.indexOf(res), "the tool_use is emitted before its result");
    assert.equal(use.payload.id, res.payload.tool_use_id, "the result references the tool_use id");
  });

  it("UZI_STUB_STALL brackets its pause with a quiet-then-resume status pair", async () => {
    const emitted = await runWith(STUB_STALL_SENTINEL);
    const texts = emitted.filter((m) => m.kind === "status").map((m) => String(m.payload.text));
    assert.ok(texts.some((t) => t.includes("pausing")), "emits a pause marker before going quiet");
    assert.ok(texts.some((t) => t.includes("resuming")), "emits a resume marker (the activity bump that self-clears)");
  });

  it("emits no health tool calls when no sentinel is present", async () => {
    const wt = makeWorktree();
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path });
    try {
      await new StubExecutor(nullLogger(), { healthPauseMs: 0 }).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.equal(tools(emitted, "tool_use").length, 0, "a normal run emits no stub tool calls");
  });
});

describe("StubExecutor — PRD #41 plan gate revision loop", () => {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const revise = (feedback: string): PlanVerdict => ({ kind: "revise", feedback });

  // A gate that replays a scripted sequence of verdicts (last entry sticks), recording
  // the plan text handed to each gatePlan call so the test can assert the stub re-gates
  // a REVISED plan rather than the original — mirroring sdk-executor.test's makeCtx.
  function gatePlanScript(verdicts: PlanVerdict[]): { gatePlan: RunContext["gatePlan"]; plans: string[] } {
    const plans: string[] = [];
    let call = 0;
    return {
      gatePlan: async (planMd) => {
        plans.push(planMd);
        return verdicts[Math.min(call++, verdicts.length - 1)]!;
      },
      plans,
    };
  }

  it("revise → revised plan → re-gate → approve: emits plan_feedback + plan_revising and implements the run", async () => {
    const wt = makeWorktree();
    const gate = gatePlanScript([revise("add a rollback step"), approve]);
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path, gatePlan: gate.gatePlan });
    try {
      await new StubExecutor(nullLogger(), { planGate: true }).run(ctx);
    } finally {
      wt.cleanup();
    }

    // The revision was recorded on the feed: a plan_feedback carrying the reviewer's
    // words, then a 1-based plan_revising round — each precedes the re-gate (feed never
    // lags the gate).
    const feedbacks = emitted.filter((m) => m.kind === "plan_feedback");
    const revisings = emitted.filter((m) => m.kind === "plan_revising");
    assert.equal(feedbacks.length, 1, "one plan_feedback for the single revise");
    assert.equal(feedbacks[0]!.payload.feedback, "add a rollback step", "feedback carried verbatim");
    assert.equal(revisings.length, 1, "one plan_revising for the single revise");
    assert.equal(revisings[0]!.payload.round, 1, "round is 1-based");
    assert.ok(
      emitted.indexOf(feedbacks[0]!) < emitted.indexOf(revisings[0]!),
      "plan_feedback precedes plan_revising",
    );

    // The gate ran twice; the second call received a REVISED plan (the original plus a
    // revision marker), NOT the un-revised plan the stub first submitted.
    assert.equal(gate.plans.length, 2, "gatePlan called once per round (revise + approve)");
    assert.ok(gate.plans[1]!.startsWith(gate.plans[0]!), "the revised plan extends the original");
    assert.notEqual(gate.plans[1], gate.plans[0], "the re-gated plan differs from the original");
    assert.match(gate.plans[1]!, /revision 1/, "the revised plan carries a revision marker");

    // On approve the stub implemented: it announced approval and committed its work.
    assert.ok(
      emitted.some((m) => m.kind === "status" && String(m.payload.text).includes("plan approved")),
      "the run proceeds to implement after approval",
    );
    assert.ok(
      emitted.some((m) => m.kind === "text" && String(m.payload.text).includes("committed locally")),
      "the stub committed its work (implemented)",
    );
  });

  it("does not revise when the plan is approved on the first gate call", async () => {
    const wt = makeWorktree();
    const gate = gatePlanScript([approve]);
    const { ctx, emitted } = makeCtx({ worktreePath: wt.path, gatePlan: gate.gatePlan });
    try {
      await new StubExecutor(nullLogger(), { planGate: true }).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.equal(gate.plans.length, 1, "a first-call approve gates exactly once");
    assert.equal(emitted.filter((m) => m.kind === "plan_feedback").length, 0, "no plan_feedback without a revise");
    assert.equal(emitted.filter((m) => m.kind === "plan_revising").length, 0, "no plan_revising without a revise");
  });
});

describe("StubExecutor — PRD #35 Decision 6b pre-approved resume", () => {
  const approve: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
  const skipText = "skipping the planning turn";

  // gatePlan that RECORDS its calls, so "the gate was never entered" is assertable
  // as a count rather than inferred from the absence of a message.
  function countingGate(): { gatePlan: (planMd: string) => Promise<PlanVerdict>; calls: string[] } {
    const calls: string[] = [];
    return {
      gatePlan: async (planMd: string) => {
        calls.push(planMd);
        return approve;
      },
      calls,
    };
  }

  it("planApproved + approvedPlan: never enters the gate, says so once, and still implements", async () => {
    const wt = makeWorktree();
    const gate = countingGate();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      gatePlan: gate.gatePlan,
      planApproved: true,
      approvedPlan: "## An already-approved plan\n\n- do the thing",
    });
    try {
      await new StubExecutor(nullLogger(), { planGate: true }).run(ctx);
    } finally {
      wt.cleanup();
    }
    // The load-bearing one: a resumed run must not park a second time in front of a
    // human who already approved. Counting the gate calls is what proves the skip;
    // the feed line is a report OF the skip and could be emitted by code that gated
    // anyway.
    assert.equal(gate.calls.length, 0, "a pre-approved resume must never call gatePlan");
    assert.equal(
      emitted.filter((m) => String(m.payload.text ?? "").includes(skipText)).length,
      1,
      "the skip is reported exactly once (the M6 e2e asserts this exact count on the feed)",
    );
    assert.ok(
      emitted.some((m) => m.kind === "text" && String(m.payload.text).includes("committed locally")),
      "the run still implements after skipping the gate",
    );
  });

  // Both halves of the condition, one test each. Without these the branch would pass
  // its happy-path test while an implement loop with no instructions, or an ordinary
  // first run, silently took the skip.
  it("planApproved WITHOUT approvedPlan still gates (an empty plan has nothing to implement)", async () => {
    const wt = makeWorktree();
    const gate = countingGate();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      gatePlan: gate.gatePlan,
      planApproved: true,
      approvedPlan: "   ",
    });
    try {
      await new StubExecutor(nullLogger(), { planGate: true }).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.equal(gate.calls.length, 1, "a whitespace-only approvedPlan must fall through to the gate");
    assert.equal(emitted.filter((m) => String(m.payload.text ?? "").includes(skipText)).length, 0);
  });

  it("an ordinary first run (no planApproved) gates exactly as before", async () => {
    const wt = makeWorktree();
    const gate = countingGate();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      gatePlan: gate.gatePlan,
      approvedPlan: "## carried by the claim but not approved",
    });
    try {
      await new StubExecutor(nullLogger(), { planGate: true }).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.equal(gate.calls.length, 1, "approvedPlan alone must not skip the gate");
    assert.equal(emitted.filter((m) => String(m.payload.text ?? "").includes(skipText)).length, 0);
  });

  // PRD #209 (D4 row 2 + item 5): a SEEDED run (no session — the stub never checks one)
  // skips the gate AND records on the feed that the plan was supplied externally, so a
  // reader can tell a user-authored plan from a resumed approval. The stub's discriminator
  // ignores sessionId already, so this exercises the seeded feed line specifically.
  it("a seeded run skips the gate and records that the plan was supplied externally", async () => {
    const wt = makeWorktree();
    const gate = countingGate();
    const { ctx, emitted } = makeCtx({
      worktreePath: wt.path,
      gatePlan: gate.gatePlan,
      planApproved: true,
      seeded: true,
      approvedPlan: "## A plan the user supplied at create time",
    });
    try {
      await new StubExecutor(nullLogger(), { planGate: true }).run(ctx);
    } finally {
      wt.cleanup();
    }
    assert.equal(gate.calls.length, 0, "a seeded plan must never call gatePlan");
    // The skip is reported exactly once, and its wording names the seeded provenance
    // rather than mislabelling a fresh seeded run as a "resume".
    const skips = emitted.filter((m) => String(m.payload.text ?? "").includes(skipText));
    assert.equal(skips.length, 1, "the skip is reported exactly once");
    assert.match(String(skips[0]!.payload.text), /seeded/, "the feed names the external provenance");
    assert.ok(
      emitted.some((m) => m.kind === "text" && String(m.payload.text).includes("committed locally")),
      "the run still implements after skipping the gate",
    );
  });
});
