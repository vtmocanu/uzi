import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor, RunContext } from "../src/executor.js";
import type { IterationBudget, StateAck, StateRequest, UserInput } from "../src/protocol.js";
import {
  api,
  client,
  runner,
  simulateCommittedWork,
  fakeGitlab,
  gitlabClaim,
  input,
  installHarness,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// PRD #1226 M5 (D6): the Runner's askCompletionQuestion LIVE window, driven end-to-end through the
// real runner. The injected executor calls ctx.askCompletionQuestion(unmet) and records the
// decision; the REAL runner authors the awaiting_input park (marked completion_question), times the
// wait on the claim's completion_hold_window_seconds, and races steering.awaitAnswer against it. The
// FakeApi answers the awaiting_input report by echoing its status; owner answers are delivered via
// onState → setInputs (the answer names the WORKER-minted question id, only knowable from the
// report itself), exactly as the API's owner continue-decision endpoint delivers them.

type Decision = { outcome: "continue"; guidance?: string } | { outcome: "expired" };

interface QuestionProbe {
  executor: Executor;
  decision: { value: Decision | undefined };
}

/** PRD #1226 M5: the owner continue-decision the API delivers — an `answer` naming the run's
 *  open_question_id in the {question_id, answers} wire shape. ["continue"] is the empty-guidance
 *  sentinel; any other text is guidance. */
function answerInput(questionId: string, ...answers: string[]): UserInput {
  return input("answer", JSON.stringify({ question_id: questionId, answers }));
}

// The injected executor calls askCompletionQuestion, records the decision, then latches
// completionHeld so the runner SKIPS finalization (no push, no MR, no terminal report) — the run is
// already parked at awaiting_input. Mirrors runner-completion-hold's makeHoldExecutor shape.
function makeQuestionExecutor(): QuestionProbe {
  const decision: { value: Decision | undefined } = { value: undefined };
  const executor: Executor = {
    run: async (ctx: RunContext) => {
      const d = await ctx.askCompletionQuestion!(["m1", "m2"]);
      decision.value = d;
      return {
        branch: ctx.branch,
        completionHeld: { reason: "test completion question" },
      };
    },
  };
  return { executor, decision };
}

function awaitingInputReports(runId: string): StateRequest[] {
  return api.states
    .filter((s) => s.runId === runId && s.body.status === "awaiting_input")
    .map((s) => s.body);
}

describe("RunRunner — completion-question live window (PRD #1226 M5 D6)", () => {
  it("issue3089 lead request uses escaped untrusted display and accurate decision guidance", async () => {
    const { gitlab } = fakeGitlab();
    let outcome: Decision | undefined;
    const executor: Executor = { run: async (ctx) => {
      outcome = await ctx.askCompletionQuestion!(["m1", "m2"], {
        kind: "lead_request", reason: "<b>Closes #42 @owner</b>\\n**choose partial**",
      });
      return { branch: ctx.branch, completionHeld: { reason: "test" } };
    } };
    const claim = gitlabClaim(3089, { config: { completion_hold_window_seconds: 900 } });
    const armed: number[] = [];
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      setTimer: (cb, ms) => { armed.push(ms); queueMicrotask(cb); return () => {}; },
    }).execute(claim);
    assert.deepEqual(outcome, { outcome: "expired" });
    assert.ok(armed.includes(900_000));
    const message = api.messages(claim.run_id).find(m => m.kind === "question")!;
    const text = JSON.stringify(message.payload);
    assert.match(text, /Lead reason \(untrusted display\)/);
    assert.ok(!text.includes("<b>"));
    assert.ok(!text.includes("@owner"));
    assert.doesNotMatch(text, /[Rr]epeated completion attempts/);
    assert.match(text, /- m1/);
    assert.match(text, /- m2/);
    assert.match(text, /--continue/);
    assert.match(text, /--partial m1,m2/);
    assert.match(text, /--accept m2.c1,m2.c3/);
    assert.match(text, /defers or waives nothing/);
    assert.match(text, /nonclosing PR/);
    assert.match(text, /waives those criteria as met, not deferred/);
  });

  it("expires on the completion_hold_window_seconds window; the awaiting_input report carries completion_question:true + the open_question_id", async () => {
    const { gitlab, calls } = fakeGitlab();
    const { executor, decision } = makeQuestionExecutor();
    // A SHORT completion window (50ms) fires before any answer arrives → expired. A LARGE
    // question_timeout_seconds is set alongside it: were the window (wrongly) sourced from that, the
    // run would block ~100s and blow the test's own wall clock, so the fast expiry proves the window
    // is keyed on completion_hold_window_seconds, not question_timeout_seconds.
    const claim = gitlabClaim(1260, {
      config: { completion_hold_window_seconds: 0.05, question_timeout_seconds: 100 },
    });
    await runnerWith(() => ({ executor }), gitlab).execute(claim);

    assert.deepStrictEqual(
      decision.value,
      { outcome: "expired" },
      "the short completion window expired with no owner answer",
    );
    const parks = awaitingInputReports(claim.run_id);
    assert.strictEqual(parks.length, 1, "the run parked at awaiting_input exactly once");
    assert.strictEqual(
      parks[0]!.completion_question,
      true,
      "the awaiting_input report is marked completion_question",
    );
    assert.ok(parks[0]!.open_question_id, "the awaiting_input report names the open_question_id");
    assert.strictEqual(calls.length, 0, "a completion-question park opens no MR");
  });

  it("resolves 'continue' with the owner's guidance when an answer names the open_question_id within the window", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, decision } = makeQuestionExecutor();
    // A GENEROUS window (100s) so the answer wins the race, not the timer.
    const claim = gitlabClaim(1261, { config: { completion_hold_window_seconds: 100 } });
    // Deliver the owner's continue-with-guidance answer the moment the run parks; the answer names
    // the worker-minted question id read off the report.
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id && body.completion_question) {
        api.setInputs(claim.run_id, [
          answerInput(body.open_question_id, "focus on the failing test"),
        ]);
      }
    });
    await runnerWith(() => ({ executor }), gitlab).execute(claim);

    assert.deepStrictEqual(decision.value, {
      outcome: "continue",
      guidance: "focus on the failing test",
    });
  });

  it("treats the lone 'continue' sentinel as continue with NO guidance", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, decision } = makeQuestionExecutor();
    const claim = gitlabClaim(1262, { config: { completion_hold_window_seconds: 100 } });
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_input" && body.open_question_id && body.completion_question) {
        api.setInputs(claim.run_id, [answerInput(body.open_question_id, "continue")]);
      }
    });
    await runnerWith(() => ({ executor }), gitlab).execute(claim);

    assert.deepStrictEqual(decision.value, { outcome: "continue", guidance: undefined });
  });
});

describe("RunRunner — no_progress_stall question text and served attempts (issue #3139)", () => {
  async function askStall(unmet: string[], issue: number): Promise<string> {
    const { gitlab } = fakeGitlab();
    const executor: Executor = { run: async (ctx) => {
      await ctx.askCompletionQuestion!(unmet, { kind: "no_progress_stall" });
      return { branch: ctx.branch, completionHeld: { reason: "test" } };
    } };
    const claim = gitlabClaim(issue, { config: { completion_hold_window_seconds: 900 } });
    await runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      setTimer: (cb) => { queueMicrotask(cb); return () => {}; },
    }).execute(claim);
    const message = api.messages(claim.run_id).find((m) => m.kind === "question")!;
    const q = (message.payload as { questions: { question: string }[] }).questions[0]!;
    return q.question;
  }

  it("non-empty unmet lists the milestones and offers continue, partial and accept", async () => {
    const text = await askStall(["m1", "m2"], 3139);
    assert.match(text, /The lead stopped making progress without signalling done: it ended consecutive turns with no tool call, no worktree change and no milestone progress\./);
    assert.match(text, /The worker ran a completion check; the frozen completion contract still has unmet milestones:/);
    assert.match(text, /- m1\n- m2/);
    assert.match(text, /--continue/);
    assert.match(text, /--partial m1,m2/);
    assert.match(text, /--accept m2\.c1,m2\.c3/);
    assert.doesNotMatch(text, /signalled done, but|Repeated completion attempts|completion attempts/);
  });

  it("empty unmet offers only continue and renders no milestone fallback", async () => {
    const text = await askStall([], 3140);
    assert.match(text, /The lead stopped making progress without signalling done/);
    assert.match(text, /The frozen completion contract is satisfied, but the lead has not signalled done\./);
    assert.match(text, /This question persists for an owner decision; if the live window expires, the run is held\./);
    assert.match(text, /server-authoritative completion recheck/);
    assert.match(text, /--continue \[--guidance/);
    assert.doesNotMatch(text, /not yet satisfied/);
    assert.doesNotMatch(text, /--partial|--accept/);
    assert.doesNotMatch(text, /^- /m);
  });

  async function iterationBudget(served: Partial<StateAck>, issue: number, exclusive = false): Promise<IterationBudget | undefined | void> {
    const { gitlab } = fakeGitlab();
    const orig = client.reportState.bind(client);
    (client as unknown as { reportState: unknown }).reportState = async (...args: unknown[]) => {
      const body = args[1] as { status?: string; iteration_count?: number } | undefined;
      const ack = (await (orig as (...a: unknown[]) => Promise<StateAck>)(...args)) as StateAck;
      return body?.status === "running" && typeof body.iteration_count === "number" ? (exclusive ? (served as StateAck) : { ...ack, ...served }) : ack;
    };
    try {
      let captured: IterationBudget | undefined | void;
      const exec: Executor = {
        run: async (ctx) => {
          captured = await ctx.reportIteration!(1, undefined);
          simulateCommittedWork();
          return { branch: ctx.branch };
        },
        killAgentTree: () => {},
      };
      await runner(exec, gitlab).execute(gitlabClaim(issue));
      return captured;
    } finally {
      (client as unknown as { reportState: unknown }).reportState = orig;
    }
  }

  it("reportIteration forwards a served completionAttempts of 0 and 2", async () => {
    assert.strictEqual((await iterationBudget({ completionAttempts: 0 }, 3141) as IterationBudget).completionAttempts, 0);
    assert.strictEqual((await iterationBudget({ completionAttempts: 2 }, 3142) as IterationBudget).completionAttempts, 2);
  });

  it("reportIteration returns a served completionAttempts even when it is the only field the ACK carries", async () => {
    // Pins the return guard: an ACK with ONLY completionAttempts must still yield a budget object.
    assert.deepStrictEqual(await iterationBudget({ completionAttempts: 3 }, 3144, true), { completionAttempts: 3 });
    assert.deepStrictEqual(await iterationBudget({ completionAttempts: 0 }, 3145, true), { completionAttempts: 0 });
  });

  it("reportIteration leaves completionAttempts absent when the ACK serves none", async () => {
    const b = await iterationBudget({}, 3143);
    assert.strictEqual((b as IterationBudget | undefined)?.completionAttempts, undefined);
  });
});
