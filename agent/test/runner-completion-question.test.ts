import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor, RunContext } from "../src/executor.js";
import type { StateRequest, UserInput } from "../src/protocol.js";
import {
  api,
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
