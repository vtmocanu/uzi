// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QuestionPanel } from "./QuestionPanel";
import type { OpenQuestion, QuestionPayload } from "../lib/runQuestion";

afterEach(cleanup);

const FREE_TEXT: QuestionPayload = {
  questionId: "q-1",
  questions: [{ question: "What retention window?", header: "Retention", options: [], multiSelect: false }],
};

const SINGLE_SELECT: QuestionPayload = {
  questionId: "q-2",
  questions: [
    {
      question: "Which backend?",
      header: "Storage",
      options: [
        { label: "Postgres table", description: "one more migration" },
        { label: "In-memory ring", description: "lost on restart" },
      ],
      multiSelect: false,
    },
  ],
};

const MULTI_SELECT: QuestionPayload = {
  questionId: "q-3",
  questions: [
    {
      question: "Which fields?",
      header: "Fields",
      options: [
        { label: "heartbeat age", description: "" },
        { label: "active runs", description: "" },
        { label: "worker version", description: "" },
      ],
      multiSelect: true,
    },
  ],
};

// The ordinal is a FEED-derived fact, not a payload field (D-R), so the panel takes it
// alongside the question. Default 1 — the ordinary single-question case.
function open(question: QuestionPayload, ordinal = 1): OpenQuestion {
  return { question, ordinal };
}

function bodyOf(onAnswer: ReturnType<typeof vi.fn>): { question_id: string; answers: string[] } {
  return JSON.parse(onAnswer.mock.calls[0][0] as string);
}

describe("QuestionPanel", () => {
  it("blocks the submit until every question has an answer", () => {
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(FREE_TEXT)} busy={false} onAnswer={onAnswer} />);
    const send = screen.getByRole("button", { name: "Send answer" }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    fireEvent.change(screen.getByLabelText("Your answer"), { target: { value: "14 days" } });
    expect(send.disabled).toBe(false);
    fireEvent.click(send);
    expect(bodyOf(onAnswer)).toEqual({ question_id: "q-1", answers: ["14 days"] });
  });

  it("sends the question's OWN id, not a positional guess", () => {
    // The api rejects an answer naming any other question (409 stale), and a requeue
    // re-parks on the SAME id — so the id is the whole staleness guard and must be
    // carried verbatim from the payload the panel was handed.
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={onAnswer} />);
    fireEvent.click(screen.getByRole("button", { name: /Postgres table/ }));
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(bodyOf(onAnswer).question_id).toBe("q-2");
  });

  it("single-select REPLACES the previous pick", () => {
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={onAnswer} />);
    const postgres = screen.getByRole("button", { name: /Postgres table/ });
    const ring = screen.getByRole("button", { name: /In-memory ring/ });
    fireEvent.click(postgres);
    fireEvent.click(ring);
    expect(postgres.getAttribute("aria-pressed")).toBe("false");
    expect(ring.getAttribute("aria-pressed")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(bodyOf(onAnswer).answers).toEqual(["In-memory ring"]);
  });

  it("multiSelect ACCUMULATES picks", () => {
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(MULTI_SELECT)} busy={false} onAnswer={onAnswer} />);
    fireEvent.click(screen.getByRole("button", { name: /heartbeat age/ }));
    fireEvent.click(screen.getByRole("button", { name: /worker version/ }));
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(bodyOf(onAnswer).answers).toEqual(["heartbeat age, worker version"]);
  });

  it("a chip is deselectable, back to blocking the submit", () => {
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={onAnswer} />);
    const postgres = screen.getByRole("button", { name: /Postgres table/ });
    fireEvent.click(postgres);
    fireEvent.click(postgres);
    expect(postgres.getAttribute("aria-pressed")).toBe("false");
    expect((screen.getByRole("button", { name: "Send answer" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("free text REFINES a pick instead of replacing it", () => {
    // Options are a convenience, never a closed set (PRD #88 D1). Letting either half
    // win would silently discard something the user clicked or typed.
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={onAnswer} />);
    fireEvent.click(screen.getByRole("button", { name: /Postgres table/ }));
    fireEvent.change(screen.getByLabelText("Your answer"), { target: { value: "partitioned by day" } });
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(bodyOf(onAnswer).answers).toEqual(["Postgres table — partitioned by day"]);
  });

  it("keeps multi-question answers INDEX-ALIGNED with the questions", () => {
    // The worker zips the two arrays back together, so a shifted or short array
    // attributes an answer to the wrong question — silently, and in the agent's context
    // rather than anywhere a human would see it.
    const onAnswer = vi.fn();
    const three: QuestionPayload = {
      questionId: "q-multi",
      questions: [
        { question: "First?", header: "A", options: [], multiSelect: false },
        { question: "Second?", header: "B", options: [{ label: "opt-b", description: "" }], multiSelect: false },
        { question: "Third?", header: "C", options: [], multiSelect: false },
      ],
    };
    render(<QuestionPanel open={open(three)} busy={false} onAnswer={onAnswer} />);
    fireEvent.change(screen.getByLabelText("Answer to question 1"), { target: { value: "one" } });
    fireEvent.click(screen.getByRole("button", { name: /opt-b/ }));
    fireEvent.change(screen.getByLabelText("Answer to question 3"), { target: { value: "three" } });
    // Question 2 is answered by its chip alone, so the submit unblocks with only two
    // textareas filled.
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(bodyOf(onAnswer).answers).toEqual(["one", "opt-b", "three"]);
  });

  it("scopes a single-select replacement to its OWN question", () => {
    const onAnswer = vi.fn();
    const two: QuestionPayload = {
      questionId: "q-two",
      questions: [
        {
          question: "First?",
          header: "A",
          options: [{ label: "a1", description: "" }, { label: "a2", description: "" }],
          multiSelect: false,
        },
        { question: "Second?", header: "B", options: [{ label: "b1", description: "" }], multiSelect: false },
      ],
    };
    render(<QuestionPanel open={open(two)} busy={false} onAnswer={onAnswer} />);
    fireEvent.click(screen.getByRole("button", { name: /a1/ }));
    fireEvent.click(screen.getByRole("button", { name: /b1/ }));
    // Picking in question 2 must not clear question 1's pick.
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(bodyOf(onAnswer).answers).toEqual(["a1", "b1"]);
  });

  it("renders question text through the escaped sink and option labels as text children", () => {
    // Both strings are model-authored from repo/issue content the agent read. The label
    // check is about ATTRIBUTES because that is the channel a textContent assertion
    // cannot see — the trap D-K names.
    const hostile: QuestionPayload = {
      questionId: "q-x",
      questions: [
        {
          question: "Use <script>alert(1)</script> or <img src=x onerror=alert(2)>?",
          header: "Header <script>alert(3)</script>",
          options: [{ label: 'javascript:alert(4)" title="pwn', description: "<script>alert(5)</script>" }],
          multiSelect: false,
        },
      ],
    };
    const { container } = render(<QuestionPanel open={open(hostile)} busy={false} onAnswer={vi.fn()} />);
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("a")).toBeNull();
    expect(container.querySelector("[title]")).toBeNull();
    expect(container.textContent).toContain('javascript:alert(4)" title="pwn');
  });

  it("disables every control while an action is in flight", () => {
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={true} onAnswer={vi.fn()} onCancel={vi.fn()} />);
    expect((screen.getByRole("button", { name: /Postgres table/ }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Send answer" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Cancel run" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("offers Cancel run only when a handler is wired", () => {
    const onCancel = vi.fn();
    const { rerender } = render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={vi.fn()} />);
    expect(screen.queryByRole("button", { name: "Cancel run" })).toBeNull();
    rerender(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={vi.fn()} onCancel={onCancel} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("marks a LATER question with its ordinal, and marks the first with nothing", () => {
    // The ordinal comes from counting the feed (D-R), never from the payload — which no
    // longer carries one. A parked run the user meets days later must say which round it
    // is on; the first question needs no marker, since "q1" is the only thing it could be.
    const { rerender } = render(<QuestionPanel open={open(SINGLE_SELECT, 3)} busy={false} onAnswer={vi.fn()} />);
    expect(screen.getByText("q3")).toBeTruthy();
    rerender(<QuestionPanel open={open(SINGLE_SELECT, 1)} busy={false} onAnswer={vi.fn()} />);
    expect(screen.queryByText(/^q\d+$/)).toBeNull();
  });
});
