// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { handleInput } from "./engine";
import { listMessages, patchRun, state } from "./store";
import {
  deriveOpenQuestion,
  encodeAnswerBody,
  parseQuestionPayload,
  questionOrdinal,
} from "../lib/runQuestion";
import type { RunMessage } from "../lib/api";

// PRD #88 D-N: THE MOCK ENGINE IS A CONTRACT, NOT A FIXTURE.
//
// `mockApi` is what every web/ test and every browser validation of the answer journey
// exercises, and it is the code we do NOT ship — so a divergence from the real wire is
// a demo that lies AND a suite that asserts a fiction, with nothing to say so. The
// precedent is not hypothetical: `revise_plan` landed in `RunInputKind` with PRD #41 and
// fell through `handleInput`'s switch with no default and no exhaustiveness check, so
// mock mode rendered a Request-changes button that silently no-opped.
//
// So this file does not snapshot the demo. It asserts ONE CASE PER REIMPLEMENTED
// BEHAVIOUR — a golden taken from the fixture would agree with a broken engine on
// everything it covered and read as full coverage. The final test counts the payload
// shapes the fixture must contain, so a case quietly dropped from the script reddens
// here rather than going unnoticed until someone browses the demo.

const RUN_ID = "run-live";

function messagesOfKind(kind: string): RunMessage[] {
  return listMessages(RUN_ID).filter((m) => m.kind === kind);
}

// The web tsconfig's lib predates Array.prototype.at, so index arithmetic it is.
function last(kind: string): RunMessage | undefined {
  const of = messagesOfKind(kind);
  return of[of.length - 1];
}

function latestQuestion() {
  const q = last("question");
  return q ? parseQuestionPayload(q.payload) : null;
}

/** Runs every timer the engine scheduled, including ones scheduled BY a timer. */
function drain() {
  for (let i = 0; i < 40; i++) vi.advanceTimersByTime(5_000);
}

beforeEach(() => {
  vi.useFakeTimers();
  // A clean log + a gated run, so each test drives the same starting point without
  // depending on which script a previous test left mid-flight.
  state.messages.set(RUN_ID, []);
  patchRun(RUN_ID, { status: "awaiting_approval" });
});

afterEach(() => {
  vi.clearAllTimers();
  vi.useRealTimers();
});

describe("mock engine: the clarification park (PRD #88)", () => {
  it("parks at awaiting_input on a question the panel can actually derive", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
    // Derived through the SHIPPED rule, not by reaching into the payload: if
    // deriveOpenQuestion and the mock ever disagree, the demo renders a park with no
    // composer and this is what catches it.
    const open = deriveOpenQuestion([...listMessages(RUN_ID)]);
    expect(open).not.toBeNull();
    expect(open!.question.questions.length).toBeGreaterThan(0);
    expect(open!.ordinal).toBe(1);
  });

  it("resumes to running on an answer that names the open question", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const open = latestQuestion()!;
    handleInput(RUN_ID, "answer", encodeAnswerBody(open.questionId, open.questions.map(() => "ok")));
    expect(state.runs.get(RUN_ID)!.status).toBe("running");
    expect(last("answer")!.payload).toEqual({ answers: open.questions.map(() => "ok") });
  });

  it("REJECTS an answer naming a different question — the 409 the api returns", () => {
    // Not decoration: a Slack reply to question N landing after the lead asked N+1 is
    // the live case, and a mock that accepted any answer would let a surface be built
    // against a laxer contract than the one that ships. Asserting the STATUS as well as
    // the no-op is what makes this a contract test rather than a no-crash test — a mock
    // that silently swallowed the refusal would satisfy the message count alone.
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const rejection = handleInput(RUN_ID, "answer", encodeAnswerBody("q-not-the-open-one", ["ok"]));
    expect(rejection).toEqual({ status: 409, message: "that question has already been answered or replaced" });
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
    expect(messagesOfKind("answer")).toHaveLength(0);
  });

  it("REJECTS a malformed answer body rather than defaulting it", () => {
    // Fail-safe, deliberately unlike parseAgentSelection's malformed→own fallback: an
    // answer that cannot say what it answers has no safe reading.
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    expect(handleInput(RUN_ID, "answer", "not json")!.status).toBe(400);
    expect(handleInput(RUN_ID, "answer", JSON.stringify({ answers: ["ok"] }))!.status).toBe(400);
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
    expect(messagesOfKind("answer")).toHaveLength(0);
  });

  it("REJECTS an answer while the run is not parked at all", () => {
    patchRun(RUN_ID, { status: "running" });
    const rejection = handleInput(RUN_ID, "answer", encodeAnswerBody("q-mock-0001", ["ok"]));
    expect(rejection).toEqual({ status: 409, message: "run is not waiting for an answer" });
    expect(messagesOfKind("answer")).toHaveLength(0);
  });

  it("asks a SECOND question after the first is answered, and the feed COUNTS it as 2", () => {
    // The multi-round path is #88's designed path (QUESTION_MAX = 5), not its rare one —
    // and a run on its second question is the only thing that exercises the panel's
    // round marker at all. The ordinal is counted here (D-R): no payload carries one, so
    // a fixture with a single question could not tell a real count from a hardcoded 1.
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const first = latestQuestion()!;
    handleInput(RUN_ID, "answer", encodeAnswerBody(first.questionId, first.questions.map(() => "ok")));
    drain();
    const second = latestQuestion()!;
    expect(second.questionId).not.toBe(first.questionId);
    expect(deriveOpenQuestion([...listMessages(RUN_ID)])!.ordinal).toBe(2);
    expect(questionOrdinal([...listMessages(RUN_ID)], last("question")!.seq)).toBe(2);
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
  });

  it("an answer to the FIRST question is stale once the second is open", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const first = latestQuestion()!;
    handleInput(RUN_ID, "answer", encodeAnswerBody(first.questionId, ["ok"]));
    drain();
    const before = messagesOfKind("answer").length;
    handleInput(RUN_ID, "answer", encodeAnswerBody(first.questionId, ["late reply to q1"]));
    expect(messagesOfKind("answer")).toHaveLength(before);
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
  });

  it("reaches the implementation once both questions are answered", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const first = latestQuestion()!;
    handleInput(RUN_ID, "answer", encodeAnswerBody(first.questionId, ["ok"]));
    drain();
    const second = latestQuestion()!;
    handleInput(RUN_ID, "answer", encodeAnswerBody(second.questionId, ["ok"]));
    drain();
    expect(state.runs.get(RUN_ID)!.status).toBe("completed");
  });

  it("clears the park on cancel, so a later answer cannot revive it", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const open = latestQuestion()!;
    handleInput(RUN_ID, "cancel", "");
    handleInput(RUN_ID, "answer", encodeAnswerBody(open.questionId, ["ok"]));
    expect(messagesOfKind("answer")).toHaveLength(0);
  });

  it("returns no rejection for the kinds the api accepts", () => {
    // The mirror of the three above: a rejection channel that fired on the happy path
    // would surface an error banner on every successful action, and only asserting the
    // refusals would not notice.
    expect(handleInput(RUN_ID, "approve_plan", "")).toBeNull();
    drain();
    const open = latestQuestion()!;
    expect(handleInput(RUN_ID, "answer", encodeAnswerBody(open.questionId, ["ok"]))).toBeNull();
    expect(handleInput(RUN_ID, "follow_up", "note for later")).toBeNull();
  });
});

describe("mock engine: revise_plan no longer falls through (PRD #41, fixed with #88)", () => {
  it("emits the feedback and the revising marker, then re-gates with a new plan", () => {
    // The failure this replaces was SILENT: the button submitted, the mock did nothing,
    // and the demo sat at the gate. Asserting the messages rather than only the end
    // status is what distinguishes "revised" from "never left the gate".
    handleInput(RUN_ID, "revise_plan", "use the sweeper's window");
    expect(last("plan_feedback")!.payload).toEqual({ feedback: "use the sweeper's window" });
    expect(last("plan_revising")!.payload).toEqual({ round: 1 });
    drain();
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_approval");
    expect(messagesOfKind("plan").length).toBeGreaterThan(0);
  });

  it("counts rounds from the feed rather than a parallel counter", () => {
    handleInput(RUN_ID, "revise_plan", "first");
    drain();
    handleInput(RUN_ID, "revise_plan", "second");
    expect(last("plan_revising")!.payload).toEqual({ round: 2 });
  });
});

describe("the board card follows the run it names", () => {
  // 🔴 MEASURED IN THE BROWSER: the attention strip read "1 run needs an answer" while
  // the card it linked to was badged "awaiting approval". The strip comes from listRuns
  // (live); the card came from the board fixture, frozen at seed. A self-contradicting
  // demo is its own defect, and it also meant the awaiting_input CARD was un-driven —
  // no fixture seeds that status, so nothing but this sync can produce it.
  //
  // Not PRD #88-specific: every scripted status transition has been invisible on the
  // board since the mock was written. The clarification park is just the first one whose
  // two renderings were on screen together to disagree out loud.
  function cardFor(runId: string) {
    for (const board of state.boards.values()) {
      const card = board.cards.find((c) => c.latest_run?.id === runId);
      if (card) return card;
    }
    return undefined;
  }

  it("moves the card's badge with the run through the whole journey", () => {
    expect(cardFor(RUN_ID)).toBeDefined();
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    expect(state.runs.get(RUN_ID)!.status).toBe("awaiting_input");
    expect(cardFor(RUN_ID)!.latest_run!.status).toBe("awaiting_input");

    const open = latestQuestion()!;
    handleInput(RUN_ID, "answer", encodeAnswerBody(open.questionId, open.questions.map(() => "ok")));
    expect(cardFor(RUN_ID)!.latest_run!.status).toBe("running");
  });

  it("carries the stop signal too, so a cancelled card reads 'stopped' not 'failed'", () => {
    // stop_kind is what isStoppedRun keys on. Syncing status without it would badge a
    // cancelled run as breakage on the board while the run view calls it stopped — the
    // same class of contradiction one field over.
    handleInput(RUN_ID, "cancel", "");
    const run = state.runs.get(RUN_ID)!;
    const card = cardFor(RUN_ID)!.latest_run!;
    expect(card.status).toBe(run.status);
    expect(card.stop_kind).toBe("cancelled");
  });

  it("does NOT clobber the card-only projection fields", () => {
    // LatestRun is a projection, not a subset of Run: run_count/is_mine/owner_name are
    // computed for the card and a run patch must leave them alone. A wholesale spread
    // would have quietly emptied them.
    const before = { ...cardFor(RUN_ID)!.latest_run! };
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const after = cardFor(RUN_ID)!.latest_run!;
    expect(after.id).toBe(before.id);
    expect(after.run_count).toBe(before.run_count);
    expect(after.is_mine).toBe(before.is_mine);
    expect(after.owner_name).toBe(before.owner_name);
  });
});

describe("the question fixture DISCRIMINATES (D-N)", () => {
  // A count assertion over the shapes, so a case silently dropped from the script fails
  // here. Snapshotting the demo instead would lock in whatever blind spot it has.
  it("covers free-text-only, single-select, multi-select and an escaping case", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    const q = latestQuestion()!;
    const noOptions = q.questions.filter((x) => x.options.length === 0);
    const single = q.questions.filter((x) => x.options.length > 0 && !x.multiSelect);
    const multi = q.questions.filter((x) => x.multiSelect);
    expect(noOptions.length).toBeGreaterThanOrEqual(1);
    expect(single.length).toBeGreaterThanOrEqual(1);
    expect(multi.length).toBeGreaterThanOrEqual(1);
    // The escaped-sink case: question text is model-authored from repo/issue content, so
    // the browser pass needs a payload that would visibly break an unhardened renderer.
    const hostile = q.questions.filter(
      (x) => x.question.includes("<script>") && x.question.includes("*") && x.question.includes("<@U"),
    );
    expect(hostile.length).toBeGreaterThanOrEqual(1);
  });

  it("gives every option a label the panel can render as a chip", () => {
    handleInput(RUN_ID, "approve_plan", "");
    drain();
    for (const item of latestQuestion()!.questions) {
      for (const o of item.options) expect(o.label.trim()).not.toBe("");
    }
  });
});
