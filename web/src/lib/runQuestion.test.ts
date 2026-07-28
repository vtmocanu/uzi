import { describe, it, expect } from "vitest";
import type { RunMessage } from "./api";
import {
  answersReady,
  composeAnswer,
  deriveOpenQuestion,
  encodeAnswerBody,
  parseAnswerPayload,
  parseQuestionPayload,
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

function questionMsg(seq: number, questionId: string, generation = 1): RunMessage {
  return msg({
    kind: "question",
    seq,
    payload: {
      question_id: questionId,
      generation,
      questions: [{ question: "Which backend?", header: "Storage" }],
    },
  });
}

describe("parseQuestionPayload", () => {
  it("reads the frozen M1 payload, filling the optional fields", () => {
    const p = parseQuestionPayload({
      question_id: "q-1",
      generation: 3,
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
    expect(p!.generation).toBe(3);
    // Absent `options`/`multiSelect` normalize rather than staying undefined, so no
    // render site has to re-do the optionality check.
    expect(p!.questions[0]).toEqual({ question: "Free text one?", header: "H1", options: [], multiSelect: false });
    expect(p!.questions[1].options).toEqual([{ label: "A", description: "first" }]);
    expect(p!.questions[1].multiSelect).toBe(true);
  });

  it("returns null without a question_id — the one field with no safe default", () => {
    // The answer body must NAME the question and the api rejects an empty id, so a panel
    // rendered from an id-less payload would offer a composer whose every submit 400s.
    expect(parseQuestionPayload({ generation: 1, questions: [{ question: "q", header: "h" }] })).toBeNull();
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

  it("defaults a missing or non-numeric generation to 1 rather than dropping the payload", () => {
    expect(parseQuestionPayload({ question_id: "q", questions: [{ question: "a", header: "" }] })!.generation).toBe(1);
    expect(
      parseQuestionPayload({ question_id: "q", generation: "2", questions: [{ question: "a", header: "" }] })!
        .generation,
    ).toBe(1);
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
    expect(q!.questionId).toBe("q-1");
  });

  it("closes on a LATER answer — the window that a question-only rule gets wrong", () => {
    // The worker emits the `answer` message BEFORE it reports `running`, so between the
    // two a question-only derivation would still offer a composer for a resolved
    // question, and a second submit would 409 as stale.
    const messages = [questionMsg(1, "q-1"), msg({ kind: "answer", seq: 2, payload: { answers: ["yes"] } })];
    expect(deriveOpenQuestion(messages)).toBeNull();
  });

  it("re-opens on a question NEWER than the answer (the multi-round path)", () => {
    const messages = [
      questionMsg(1, "q-1"),
      msg({ kind: "answer", seq: 2, payload: { answers: ["yes"] } }),
      questionMsg(3, "q-2", 2),
    ];
    const open = deriveOpenQuestion(messages);
    expect(open!.questionId).toBe("q-2");
    expect(open!.generation).toBe(2);
  });

  it("orders by SEQ, not by array position", () => {
    // The feed merges a REST replay with live WS frames, so arrival order is not seq
    // order — deriving from `.at(-1)` would pick whichever landed last.
    const messages = [questionMsg(9, "q-late"), msg({ kind: "answer", seq: 3, payload: { answers: ["x"] } })];
    expect(deriveOpenQuestion(messages)!.questionId).toBe("q-late");
  });

  it("returns null for a feed with no clarification messages at all", () => {
    expect(deriveOpenQuestion([msg({ kind: "plan", seq: 1 }), msg({ kind: "text", seq: 2 })])).toBeNull();
  });

  it("returns null when the newest question is unusable", () => {
    expect(deriveOpenQuestion([msg({ kind: "question", seq: 1, payload: { questions: [] } })])).toBeNull();
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
