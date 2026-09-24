import { beforeEach, afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { AnswerVerdict, PlanVerdict } from "../src/steering.js";
import type { AskUserQuestion } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { MAX_LEAD_FINAL_MESSAGE_LEN, PLAN_MISSING_NUDGE, REASON_PLAN_MISSING } from "../src/plan-missing.js";

// A worktree path that is UNIQUE PER PROCESS AND PER CALL, and that deliberately
// never exists. Both halves matter. Non-existence is the point of these fixtures --
// the executor must not require the worktree on disk -- so mkdtempSync is wrong here
// even though it is right for the skills tests further down, which do need real files.
// Uniqueness is a RACE FIX: skillsPluginDir() derives `<dirname>/.uzi-skills-<basename>`
// as a SIBLING, the executor materializes that directory, and `node --test` runs test
// FILES concurrently. This file and sdk-executor.test.ts both used the literal
// "/tmp/does-not-need-to-exist", so both drove the identical plugin dir and raced:
// measured 1 failure in 6 on the isolated pair, surfacing as ENOTEMPTY from one file's
// recursive remove or ENOENT from the other's mkdir. Two different literals would have
// been the same defect with a longer fuse; the basename has to be unique.
let nonexistentWorktreeSeq = 0;
function nonexistentWorktree(): string {
  return path.join(os.tmpdir(), `uzi-nonexistent-wt-${process.pid}-${nonexistentWorktreeSeq++}`);
}


// PRD #88 M4/M5 at the executor level: the pre-run park (which must NOT hit
// REASON_NO_PLAN at either planning-turn site), the answer-then-re-plan round trip,
// the shared per-run question budget, and the unwired/cap fail-open paths.
//
// Same harness shape as sdk-executor.test.ts: `queryFn` is faked, so every path here
// is provable with dummy credentials and no live Anthropic session.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
let homeDir: string;

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
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
function askUser(questions: AskUserQuestion[], sessionId = "sess-1"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content: [{ type: "tool_use", id: "t3", name: "mcp__uzi__ask_user", input: { questions } }] },
  } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

const Q: AskUserQuestion[] = [{ question: "Which database?", header: "DB" }];

interface Turn {
  options: SdkOptions;
  promptText?: string;
}

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
  asked: AskUserQuestion[][];
}

function makeCtx(
  overrides: Partial<RunContext> = {},
  answers: AnswerVerdict[] = [{ kind: "answer", answers: ["postgres"] }],
): Probe {
  const emits: EmittedMessage[] = [];
  const asked: AskUserQuestion[][] = [];
  let call = 0;
  const verdict: PlanVerdict = { kind: "approve", selection: { status: "absent" } };
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
    askUser: async (questions) => {
      asked.push(questions);
      const a = answers[Math.min(call, answers.length - 1)]!;
      call++;
      return a;
    },
    pullFollowUp: () => undefined,
    ...overrides,
  };
  return { ctx, emits, asked };
}

beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-askhome-"));
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
});

describe("PRD #88 M4 — a planning turn may ask before it plans", () => {
  // The core M4 fix. Before it, this run FAILED with REASON_NO_PLAN: the planning
  // turn ended with questions and no plan, which is precisely what the pre-run
  // trigger produces. The feature's own path hard-failed the run it was clarifying.
  it("parks instead of failing, then re-plans on the answer", async () => {
    const { queryFn, turns } = fakeTurns([
      [askUser(Q), resultSuccess()], // planning turn 1: asks, no plan
      [submitPlan("# Plan\n- use postgres"), resultSuccess()], // re-plan after the answer
      [assistantText("implementing"), signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);

    assert.equal(result.branch, "agent/issue-5");
    assert.equal(probe.asked.length, 1, "the question should have been put to the human exactly once");
    assert.deepEqual(probe.asked[0], Q);
    // The re-plan turn is told the answer AND that a plan is what comes next — the
    // implement-loop wording ("continue the work") would be wrong before a plan exists.
    assert.match(turns[1]!.promptText ?? "", /postgres/);
    assert.match(turns[1]!.promptText ?? "", /submit_plan/);
    // The `question` run-message is emitted by runner.askUser, one layer down — this
    // test fakes ctx.askUser, so asserting on it here would be asserting across a
    // boundary this test does not exercise. What the EXECUTOR owns is the `answer`
    // echo, and that is what is checked.
    assert.ok(probe.emits.some((m) => m.kind === "answer"), "the executor must echo the answer to the feed");
  });

  // The second throw site, inside #41's revision loop. Fixing only the first would
  // leave this failing exactly the way M4 exists to prevent, and it is the harder of
  // the two to reproduce by hand.
  it("parks inside the REVISION turn too, not just the first planning turn", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# v1"), resultSuccess()], // planning turn → gate
      [askUser(Q), resultSuccess()], // revision turn: asks, no plan
      [submitPlan("# v2"), resultSuccess()], // re-plan after the answer
      [assistantText("implementing"), signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    let gate = 0;
    probe.ctx.gatePlan = async () => {
      gate++;
      return gate === 1
        ? { kind: "revise", feedback: "use a real database" }
        : { kind: "approve", selection: { status: "absent" } };
    };
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(result.branch, "agent/issue-5");
    assert.equal(probe.asked.length, 1, "a revision turn that asks must park, not throw");
  });

  // The contract NARROWS rather than loosens: the original error survives for the
  // case it was written for. Issue #1593: a turn that ends in PROSE is now nudged and
  // parked instead (see the #1593 suite below), so this pins the text-free turn.
  it("still fails with REASON_NO_PLAN when the turn neither plans nor asks", async () => {
    const { queryFn } = fakeTurns([[resultSuccess()]]);
    const probe = makeCtx();
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /without submitting a plan/,
    );
    assert.equal(probe.asked.length, 0);
  });

  // Without a bound, a lead that answers every answer with another question never
  // plans and never fails. The distinct reason is deliberate: "it never planned" and
  // "it only ever asked" are different problems with different fixes, and an operator
  // reading a failed run should not have to guess which one happened.
  it("fails with a DISTINCT reason when the lead only ever asks", async () => {
    const { queryFn } = fakeTurns([[askUser(Q), resultSuccess()]]); // asks forever
    const probe = makeCtx({ config: { question_max: 2 } });
    await assert.rejects(
      new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      /kept asking clarifying questions without ever submitting a plan/,
    );
  });

  it("aborts the run when the human cancels at a pre-run park", async () => {
    const { queryFn } = fakeTurns([[askUser(Q), resultSuccess()]]);
    const probe = makeCtx({}, [{ kind: "cancel" }]);
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /run cancelled/);
  });
});

describe("PRD #88 — fail-open and the shared budget", () => {
  // D-A: the opposite of gatePlan, which throws when unwired. An ungated plan pushes
  // unreviewed work; an unaskable question only loses a clarification, so killing the
  // run would be the worse failure — and would break every executor that never wired it.
  it("continues without a human when askUser is unwired", async () => {
    const { queryFn, turns } = fakeTurns([
      [askUser(Q), resultSuccess()],
      [submitPlan("# Plan"), resultSuccess()],
      [assistantText("implementing"), signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ askUser: undefined });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(result.branch, "agent/issue-5");
    assert.ok(
      probe.emits.some((m) => m.kind === "status" && /no way to reach a human/.test(String(m.payload["text"]))),
      "the feed must say the question could not be delivered — the lead judged a guess costly",
    );
    assert.match(turns[1]!.promptText ?? "", /best judgment/);
  });

  // The budget is per RUN. M4 lets the lead ask before it plans, so a counter that
  // reset between the planning and implement phases would silently be worth
  // 2 x QUESTION_MAX.
  it("shares one question budget across the planning and implement phases", async () => {
    const { queryFn } = fakeTurns([
      [askUser(Q), resultSuccess()], // planning: park 1 (budget 1/2)
      [submitPlan("# Plan"), resultSuccess()],
      [askUser(Q), resultSuccess()], // implement: park 2 (budget 2/2)
      [askUser(Q), resultSuccess()], // implement: over cap → no park
      [assistantText("implementing"), signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx({ config: { question_max: 2 } });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(probe.asked.length, 2, "the third question must be refused — the budget spans both phases");
    assert.ok(
      probe.emits.some((m) => m.kind === "status" && /maximum number of clarifying questions/.test(String(m.payload["text"]))),
      "the feed must say the cap was reached",
    );
  });

  it("emits the answer to the feed so the round trip is auditable", async () => {
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      [askUser(Q), resultSuccess()],
      [assistantText("implementing"), signalDone(), resultSuccess()],
    ]);
    const probe = makeCtx();
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    const answer = probe.emits.find((m) => m.kind === "answer");
    assert.ok(answer, "an `answer` run-message must be emitted");
    assert.deepEqual(answer!.payload["answers"], ["postgres"]);
  });

  // A clarification is not an implement/review iteration. Counting it would let a
  // talkative lead burn the loop budget on questions rather than on work.
  it("does not spend an implement iteration on a clarification round trip", async () => {
    const iterations: number[] = [];
    const { queryFn } = fakeTurns([
      [submitPlan("# Plan"), resultSuccess()],
      [askUser(Q), resultSuccess()], // iteration 1: asks
      [assistantText("implementing"), signalDone(), resultSuccess()], // still iteration 1
    ]);
    const probe = makeCtx({
      reportIteration: (n) => {
        iterations.push(n);
      },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepEqual(iterations, [1, 1], "the clarification turn must not advance the iteration counter");
  });
});

// Issue #1593: a planning turn that ends in PROSE ONLY (no submit_plan, no ask_user) is nudged
// once with fixed text, then parked on the owner through the worker-authored askPlanMissing,
// instead of failing REASON_NO_PLAN outright. A turn with no text at all still fails as before.
describe("issue #1593 — a prose-only planning turn is nudged, then parked on the owner", () => {
  const PROSE = "I think we should refactor the widget; let me know what you think.";
  const prose = (text = PROSE, sessionId = "sess-1"): SDKMessage[] => [assistantText(text, sessionId), resultSuccess(sessionId)];
  const plan = (md = "# Plan"): SDKMessage[] => [submitPlan(md), resultSuccess()];
  const done: SDKMessage[] = [assistantText("implementing"), signalDone(), resultSuccess()];
  const cards = (emits: EmittedMessage[]) => emits.filter((m) => m.kind === "status" && m.payload["event"] === "plan_missing");
  const nudges = (turns: Turn[]) => turns.filter((t) => (t.promptText ?? "").includes(PLAN_MISSING_NUDGE)).length;

  it("nudges once with the fixed prompt, then gates the plan the nudged turn submits", async () => {
    const { queryFn, turns } = fakeTurns([prose(), plan("# nudged"), done]);
    const gated: string[] = [];
    let parks = 0;
    const probe = makeCtx({
      gatePlan: async (md) => { gated.push(md); return { kind: "approve", selection: { status: "absent" } }; },
      askPlanMissing: async () => { parks++; return { kind: "cancel" }; },
    });
    const result = await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(result.branch, "agent/issue-5");
    assert.match(turns[1]!.promptText ?? "", new RegExp(PLAN_MISSING_NUDGE.slice(0, 40)));
    assert.equal(nudges(turns), 1);
    assert.deepEqual(gated, ["# nudged"]);
    assert.equal(parks, 0);
    assert.equal(cards(probe.emits).length, 0);
  });

  it("routes a question on the nudged turn through the existing clarification path", async () => {
    const { queryFn, turns } = fakeTurns([prose(), [askUser(Q), resultSuccess()], plan(), done]);
    const probe = makeCtx({ askPlanMissing: async () => { throw new Error("must not park"); } });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(probe.asked.length, 1);
    assert.equal(nudges(turns), 1);
  });

  it("gives the initial plan turn and a revision turn one nudge each", async () => {
    const { queryFn, turns } = fakeTurns([prose(), plan("# v1"), prose(), plan("# v2"), done]);
    let gate = 0;
    const probe = makeCtx({
      gatePlan: async () => (++gate === 1 ? { kind: "revise", feedback: "tighten it" } : { kind: "approve", selection: { status: "absent" } }),
      askPlanMissing: async () => { throw new Error("must not park"); },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.equal(gate, 2);
    assert.equal(nudges(turns), 2);
    assert.ok((turns[1]!.promptText ?? "").includes(PLAN_MISSING_NUDGE));
    assert.ok((turns[3]!.promptText ?? "").includes(PLAN_MISSING_NUDGE));
  });

  it("parks on the owner after a nudged prose turn and resumes on guidance only", async () => {
    const { queryFn, turns } = fakeTurns([prose(), prose(), plan("# guided"), done]);
    const parkArgs: unknown[][] = [];
    const gated: string[] = [];
    const probe = makeCtx({
      gatePlan: async (md) => { gated.push(md); return { kind: "approve", selection: { status: "absent" } }; },
      askPlanMissing: async (...args: unknown[]) => { parkArgs.push(args); return { kind: "answer", answers: ["use the server path"] }; },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.deepEqual(parkArgs, [[]], "the park takes no model text");
    const guided = turns[2]!.promptText ?? "";
    assert.match(guided, /use the server path/);
    assert.ok(!guided.includes(PROSE), "the lead's prose never re-enters a prompt");
    assert.equal(nudges(turns), 1);
    assert.deepEqual(gated, ["# guided"], "the plan after guidance still goes to the gate");
    assert.equal(cards(probe.emits).length, 1);
    assert.equal(cards(probe.emits)[0]!.payload["lead_final_message"], PROSE);
    const answers = probe.emits.filter((m) => m.kind === "answer");
    assert.equal(answers.length, 1);
    assert.equal(answers[0]!.agent, "worker");
    assert.equal(probe.asked.length, 0, "the fallback park is not a lead question");
  });

  it("resumes the prose turn's own session for the nudge and for the guidance turn", async () => {
    const { queryFn, turns } = fakeTurns([prose(PROSE, "sess-prose-1"), prose(PROSE, "sess-prose-2"), plan("# guided"), done]);
    const probe = makeCtx({ askPlanMissing: async () => ({ kind: "answer", answers: ["go"] }) });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.ok((turns[1]!.promptText ?? "").includes(PLAN_MISSING_NUDGE));
    assert.equal(turns[1]!.options.resume, "sess-prose-1", "the nudge resumes the prose turn's session");
    assert.match(turns[2]!.promptText ?? "", /go/);
    assert.equal(turns[2]!.options.resume, "sess-prose-2", "the guidance turn resumes the nudged turn's session");
  });

  it("treats a whitespace-only submit_plan as no plan (nudged, never gated)", async () => {
    const { queryFn, turns } = fakeTurns([[assistantText(PROSE), submitPlan("  \n "), resultSuccess()], plan("# real"), done]);
    const gated: string[] = [];
    const probe = makeCtx({
      gatePlan: async (md) => { gated.push(md); return { kind: "approve", selection: { status: "absent" } }; },
      askPlanMissing: async () => { throw new Error("must not park"); },
    });
    await new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx);
    assert.ok((turns[1]!.promptText ?? "").includes(PLAN_MISSING_NUDGE));
    assert.deepEqual(gated, ["# real"]);
  });

  it("fails REASON_PLAN_MISSING when the turn after guidance is prose again, without a second nudge or park", async () => {
    const { queryFn, turns } = fakeTurns([prose(), prose(), prose(), plan()]);
    let parks = 0;
    let gates = 0;
    const probe = makeCtx({
      gatePlan: async () => { gates++; return { kind: "approve", selection: { status: "absent" } }; },
      askPlanMissing: async () => { parks++; return { kind: "answer", answers: ["try again"] }; },
    });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
      (e: Error) => e.message === REASON_PLAN_MISSING);
    assert.equal(parks, 1);
    assert.equal(gates, 0);
    assert.equal(turns.length, 3);
    assert.equal(nudges(turns), 1);
    assert.equal(cards(probe.emits).length, 2);
  });

  it("maps a cancelled owner park to the cancel reason", async () => {
    const { queryFn, turns } = fakeTurns([prose(), prose()]);
    const probe = makeCtx({ askPlanMissing: async () => ({ kind: "cancel" }) });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx), /run cancelled/);
    assert.equal(turns.length, 2);
  });

  for (const mode of ["unwired", "unattended"] as const) {
    it(`fails REASON_PLAN_MISSING with a bounded status card when no human can answer: ${mode}`, async () => {
      const secret = "sekret-" + "value-1593";
      const long = "x".repeat(MAX_LEAD_FINAL_MESSAGE_LEN) + " tail";
      const { queryFn } = fakeTurns([prose(`${secret} ${long} ${secret} tail`)]);
      const probe = makeCtx({
        redactText: (s) => s.split(secret).join("[REDACTED]"),
        askPlanMissing: mode === "unwired" ? undefined : async () => ({ kind: "unattended" }),
      });
      await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn }).run(probe.ctx),
        (e: Error) => e.message === REASON_PLAN_MISSING);
      const c = cards(probe.emits);
      assert.equal(c.length, 1);
      assert.equal(c[0]!.agent, "worker");
      const msg = String(c[0]!.payload["lead_final_message"]);
      assert.ok(msg.length <= MAX_LEAD_FINAL_MESSAGE_LEN);
      assert.ok(msg.startsWith("[truncated]…"), "the head was cut: the card shows the message's tail");
      assert.ok(msg.endsWith(" [REDACTED] tail"));
      assert.ok(!msg.includes(secret));
    });
  }

  it("keeps REASON_NO_PLAN for a no-plan turn with no text, initial and on revision", async () => {
    const empty = fakeTurns([[resultSuccess()]]);
    const probe = makeCtx({ askPlanMissing: async () => { throw new Error("must not park"); } });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn: empty.queryFn }).run(probe.ctx), /without submitting a plan/);
    assert.equal(empty.turns.length, 1);
    assert.equal(cards(probe.emits).length, 0);

    const rev = fakeTurns([plan("# v1"), [resultSuccess()]]);
    const r = makeCtx({ gatePlan: async () => ({ kind: "revise", feedback: "again" }) });
    await assert.rejects(new SdkExecutor(nullLogger(), homeDir, { queryFn: rev.queryFn }).run(r.ctx), /without submitting a plan/);
    assert.equal(rev.turns.length, 2);
  });
});
