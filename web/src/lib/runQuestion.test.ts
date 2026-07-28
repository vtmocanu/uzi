import { describe, it, expect } from "vitest";
import type { RunMessage } from "./api";
import {
  answersReady,
  composeAnswer,
  deriveOpenQuestion,
  encodeAnswerBody,
  parseAnswerPayload,
  parseQuestionPayload,
  questionOrdinal,
} from "./runQuestion";

function msg(partial: Partial<RunMessage> & { kind: string; seq: number }): RunMessage {
  return {
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: {},
    created_at: "2026-07-28T00:00:00.000Z",
    ...partial,
  };
}

function questionMsg(seq: number, questionId: string): RunMessage {
  return msg({
    kind: "question",
    seq,
    payload: {
      question_id: questionId,
      questions: [{ question: "Which backend?", header: "Storage" }],
    },
  });
}

describe("parseQuestionPayload", () => {
  it("reads the payload shape M1 froze at 39cffd4c, filling the optional fields", () => {
    const p = parseQuestionPayload({
      question_id: "q-1",
      questions: [
        { question: "Free text one?", header: "H1" },
        {
          question: "Pick one?",
          header: "H2",
          options: [{ label: "A", description: "first" }],
          multiSelect: true,
        },
      ],
    });
    expect(p).not.toBeNull();
    expect(p!.questionId).toBe("q-1");
    // Absent `options`/`multiSelect` normalize rather than staying undefined, so no
    // render site has to re-do the optionality check.
    expect(p!.questions[0]).toEqual({ question: "Free text one?", header: "H1", options: [], multiSelect: false });
    expect(p!.questions[1].options).toEqual([{ label: "A", description: "first" }]);
    expect(p!.questions[1].multiSelect).toBe(true);
  });

  it("IGNORES a stray `generation`, which the wire no longer carries (D-R)", () => {
    // The field was removed at 39cffd4c because the design doc called it "the
    // stale-answer discriminator" and nothing ever read it back — an inert display value
    // wearing the name of a struck mechanism. A payload from an older worker may still
    // carry it; it must not become a second staleness key here by accident.
    const p = parseQuestionPayload({
      question_id: "q-1",
      generation: 7,
      questions: [{ question: "q", header: "h" }],
    });
    expect(p).toEqual({ questionId: "q-1", questions: [{ question: "q", header: "h", options: [], multiSelect: false }] });
    expect("generation" in p!).toBe(false);
  });

  it("returns null without a question_id — the one field with no safe default", () => {
    // The answer body must NAME the question and the api rejects an empty id, so a panel
    // rendered from an id-less payload would offer a composer whose every submit 400s.
    expect(parseQuestionPayload({ questions: [{ question: "q", header: "h" }] })).toBeNull();
    expect(parseQuestionPayload({ question_id: "   ", questions: [{ question: "q", header: "h" }] })).toBeNull();
  });

  it("returns null when nothing renderable survives, but keeps a partly-bad list", () => {
    expect(parseQuestionPayload({ question_id: "q-1", questions: [] })).toBeNull();
    expect(parseQuestionPayload({ question_id: "q-1", questions: [{ question: "  ", header: "h" }] })).toBeNull();
    expect(parseQuestionPayload({ question_id: "q-1", questions: "not-an-array" })).toBeNull();
    const partly = parseQuestionPayload({
      question_id: "q-1",
      questions: [{ question: "", header: "dropped" }, { question: "kept", header: "h" }],
    });
    expect(partly!.questions.map((q) => q.question)).toEqual(["kept"]);
  });

  it("drops malformed OPTIONS instead of failing the question", () => {
    // Asymmetric with the rule above, deliberately: every question is answerable by free
    // text, so a lost chip is a convenience gone. Rejecting the payload would hide a
    // question the run is parked on, leaving no way to unpark it short of the deadline.
    const p = parseQuestionPayload({
      question_id: "q-1",
      questions: [
        {
          question: "Pick?",
          header: "H",
          options: [{ label: "ok", description: "d" }, { label: "  " }, "nope", { description: "no label" }],
        },
      ],
    });
    expect(p!.questions[0].options).toEqual([{ label: "ok", description: "d" }]);
  });

  it("rejects a non-object payload", () => {
    expect(parseQuestionPayload(null)).toBeNull();
    expect(parseQuestionPayload("q")).toBeNull();
    expect(parseQuestionPayload([{ question_id: "q" }])).toBeNull();
  });
});

describe("parseAnswerPayload", () => {
  it("keeps string answers and drops the rest", () => {
    expect(parseAnswerPayload({ answers: ["a", 3, null, "b"] })).toEqual({ answers: ["a", "b"] });
  });
  it("degrades an absent/odd payload to an empty list rather than throwing", () => {
    expect(parseAnswerPayload(null)).toEqual({ answers: [] });
    expect(parseAnswerPayload({})).toEqual({ answers: [] });
    expect(parseAnswerPayload({ answers: "a" })).toEqual({ answers: [] });
  });
});

describe("deriveOpenQuestion", () => {
  it("returns the question when it is the latest of {question, answer} by seq", () => {
    const q = deriveOpenQuestion([msg({ kind: "text", seq: 1 }), questionMsg(2, "q-1")]);
    expect(q!.question.questionId).toBe("q-1");
    expect(q!.ordinal).toBe(1);
  });

  it("closes on a LATER answer — the window that a question-only rule gets wrong", () => {
    // The worker emits the `answer` message BEFORE it reports `running`, so between the
    // two a question-only derivation would still offer a composer for a resolved
    // question, and a second submit would 409 as stale.
    const messages = [questionMsg(1, "q-1"), msg({ kind: "answer", seq: 2, payload: { answers: ["yes"] } })];
    expect(deriveOpenQuestion(messages)).toBeNull();
  });

  it("re-opens on a question NEWER than the answer, and COUNTS it as the second", () => {
    const messages = [
      questionMsg(1, "q-1"),
      msg({ kind: "answer", seq: 2, payload: { answers: ["yes"] } }),
      questionMsg(3, "q-2"),
    ];
    const open = deriveOpenQuestion(messages);
    expect(open!.question.questionId).toBe("q-2");
    // The ordinal is COUNTED from the feed, not read off the payload — no payload here
    // carries one. That is D-R's point: the count is a fact about the run and survives a
    // requeue re-park, where a worker-held counter would be restated wrongly.
    expect(open!.ordinal).toBe(2);
  });

  it("orders by SEQ, not by array position", () => {
    // The feed merges a REST replay with live WS frames, so arrival order is not seq
    // order — deriving from `.at(-1)` would pick whichever landed last.
    const messages = [questionMsg(9, "q-late"), msg({ kind: "answer", seq: 3, payload: { answers: ["x"] } })];
    expect(deriveOpenQuestion(messages)!.question.questionId).toBe("q-late");
  });

  it("returns null for a feed with no clarification messages at all", () => {
    expect(deriveOpenQuestion([msg({ kind: "plan", seq: 1 }), msg({ kind: "text", seq: 2 })])).toBeNull();
  });

  it("returns null when the newest question is unusable", () => {
    expect(deriveOpenQuestion([msg({ kind: "question", seq: 1, payload: { questions: [] } })])).toBeNull();
  });
});

describe("questionOrdinal (the display ordinal, counted not claimed — D-R)", () => {
  const feed = [
    questionMsg(2, "q-1"),
    msg({ kind: "answer", seq: 3, payload: { answers: ["a"] } }),
    questionMsg(5, "q-2"),
    msg({ kind: "answer", seq: 6, payload: { answers: ["b"] } }),
    questionMsg(9, "q-3"),
  ];

  it("counts question messages up to and including the given seq", () => {
    expect(questionOrdinal(feed, 2)).toBe(1);
    expect(questionOrdinal(feed, 5)).toBe(2);
    expect(questionOrdinal(feed, 9)).toBe(3);
  });

  it("returns 0 for a seq that is not a question, so a caller renders no marker", () => {
    // Never 1: guessing an ordinal is exactly the class of claim D-R removed from the
    // wire, and it would be worse re-invented here.
    expect(questionOrdinal(feed, 3)).toBe(0);
    expect(questionOrdinal(feed, 99)).toBe(0);
    expect(questionOrdinal([], 1)).toBe(0);
  });

  it("counts by SEQ, not by array position", () => {
    // The feed merges a REST replay with live WS frames, so arrival order is not seq
    // order. A positional count would number the questions by whichever landed first.
    const shuffled = [feed[4], feed[0], feed[2], feed[1], feed[3]];
    expect(questionOrdinal(shuffled, 5)).toBe(2);
    expect(questionOrdinal(shuffled, 9)).toBe(3);
  });

  it("is UNAFFECTED by a re-park of the same question, unlike a worker-held counter", () => {
    // The requeue case D-R names: a worker dies, the run re-queues, and the resumed
    // worker re-parks on the SAME question. Only one `question` message exists in the
    // durable feed, so the count stays 1 — where a counter restated at each park would
    // have to be right by discipline rather than by construction.
    expect(questionOrdinal([questionMsg(2, "q-1")], 2)).toBe(1);
  });
});

describe("encodeAnswerBody", () => {
  it("produces the JSON shape the api validates", () => {
    expect(JSON.parse(encodeAnswerBody("q-1", ["a", "b"]))).toEqual({ question_id: "q-1", answers: ["a", "b"] });
  });
});

describe("composeAnswer", () => {
  it("sends free text alone when nothing is picked", () => {
    expect(composeAnswer([], "  just words  ")).toBe("just words");
  });
  it("sends the picked labels alone when there is no free text", () => {
    expect(composeAnswer(["Postgres table", "In-memory ring"], "   ")).toBe("Postgres table, In-memory ring");
  });
  it("COMBINES the two rather than letting one win", () => {
    // "Free-text answers are always allowed even when options are offered" (PRD #88 D1)
    // means the two compose. Dropping either half would silently discard something the
    // user typed or clicked.
    expect(composeAnswer(["Admin only"], "but allow the owner too")).toBe("Admin only — but allow the owner too");
  });
  it("ignores blank picks", () => {
    expect(composeAnswer(["  ", "A"], "")).toBe("A");
  });
});

describe("answersReady", () => {
  it("requires one non-blank answer per question", () => {
    expect(answersReady(["a", "b"], 2)).toBe(true);
    expect(answersReady(["a", "  "], 2)).toBe(false);
    expect(answersReady(["a"], 2)).toBe(false);
    expect(answersReady(["a", "b", "c"], 2)).toBe(false);
  });
});
