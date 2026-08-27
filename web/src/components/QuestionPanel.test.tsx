// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QuestionPanel, UnreadableQuestion } from "./QuestionPanel";
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
  // ── web-ux browser findings ────────────────────────────────────────────────

  it("names the noun on a SINGLE-question park, not just the count", () => {
    // `multiple` gated the whole phrase, so the modal production case read "The agent
    // stopped to ask — the run is parked until you answer." The count is the variable
    // part; the noun is not.
    const { container } = render(<QuestionPanel open={open(FREE_TEXT)} busy={false} onAnswer={vi.fn()} />);
    expect(container.textContent).toContain("stopped to ask a question");
    cleanup();
    const two: QuestionPayload = {
      questionId: "q-two",
      questions: [FREE_TEXT.questions[0], FREE_TEXT.questions[0]],
    };
    expect(
      render(<QuestionPanel open={open(two)} busy={false} onAnswer={vi.fn()} />).container.textContent,
    ).toContain("stopped to ask 2 questions");
  });

  it("focuses the first answer box on mount, and again on a NEW question", () => {
    // Measured in the browser: focus was left on <body> and the first answer box was tab
    // stop 25 of 40, behind the whole sidebar — so a keyboard user Tabbed ~26 times to
    // reach the control the app says is blocking the run.
    const { rerender } = render(<QuestionPanel open={open(FREE_TEXT)} busy={false} onAnswer={vi.fn()} />);
    const first = screen.getByLabelText("Your answer");
    expect(document.activeElement).toBe(first);
    // Blur, then hand the panel a DIFFERENT question: the second park must re-focus.
    (document.activeElement as HTMLElement).blur();
    expect(document.activeElement).not.toBe(first);
    rerender(<QuestionPanel open={open(SINGLE_SELECT, 2)} busy={false} onAnswer={vi.fn()} />);
    expect(document.activeElement).toBe(screen.getByLabelText("Your answer"));
  });

  it("groups each question's chips and names the group after its question", () => {
    // Measured: with the chips in a bare <div>, question 2's chips sat between "Answer to
    // question 1" and "Answer to question 2" in tab order and read as belonging to
    // question 1 — the grouping was carried entirely by CSS.
    const two: QuestionPayload = {
      questionId: "q-two",
      questions: [
        { question: "First?", header: "Storage", options: [{ label: "a1", description: "" }], multiSelect: false },
        { question: "Second?", header: "", options: [{ label: "b1", description: "" }], multiSelect: true },
      ],
    };
    render(<QuestionPanel open={open(two)} busy={false} onAnswer={vi.fn()} />);
    const named = screen.getByRole("group", { name: "Storage" });
    expect(named.textContent).toContain("a1");
    // A question with no header still gets a distinguishable name rather than none.
    const fallback = screen.getByRole("group", { name: "Options for question 2" });
    expect(fallback.textContent).toContain("b1");
  });

  it("conveys single-vs-multi select programmatically, not just visually", () => {
    render(<QuestionPanel open={open(MULTI_SELECT)} busy={false} onAnswer={vi.fn()} />);
    const group = screen.getByRole("group", { name: "Fields" });
    const describedBy = group.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy!)?.textContent).toContain("Pick any that apply");
    cleanup();
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={vi.fn()} />);
    const single = screen.getByRole("group", { name: "Storage" });
    expect(document.getElementById(single.getAttribute("aria-describedby")!)?.textContent).toContain("Pick one");
  });

  it("separates a chip's label from its description IN THE ACCESSIBLE NAME", () => {
    // The visual gap is a CSS margin, which contributes nothing to the accessible name —
    // so the chip announced as "Postgres table— Simplest…" with no space before the dash.
    // An accessible-name assertion is the only instrument that sees this; a screenshot
    // and a textContent check both look fine.
    render(<QuestionPanel open={open(SINGLE_SELECT)} busy={false} onAnswer={vi.fn()} />);
    const chip = screen.getByRole("button", { name: /Postgres table/ });
    expect(chip.textContent).toContain("Postgres table — one more migration");
    expect(chip.textContent).not.toContain("table—");
  });

  // ── Enhancements (user-approved) ───────────────────────────────────────────

  it("submits on Cmd/Ctrl+Enter from an answer box", () => {
    // Pairs with the focus fix: park → type → chord, without Tabbing forward past every
    // remaining chip and textarea to reach Send.
    for (const mod of [{ metaKey: true }, { ctrlKey: true }]) {
      const onAnswer = vi.fn();
      render(<QuestionPanel open={open(FREE_TEXT)} busy={false} onAnswer={onAnswer} />);
      const box = screen.getByLabelText("Your answer");
      fireEvent.change(box, { target: { value: "14 days" } });
      fireEvent.keyDown(box, { key: "Enter", ...mod });
      expect(bodyOf(onAnswer)).toEqual({ question_id: "q-1", answers: ["14 days"] });
      cleanup();
    }
  });

  it("leaves BARE Enter as a newline — the panel can hold several questions", () => {
    // ChatComposer sends on bare Enter, but it has ONE field. Here Enter must stay a
    // newline or a multi-question answer becomes unwritable. Shift+Enter needs no case of
    // its own for the same reason: it is already just a newline.
    const onAnswer = vi.fn();
    render(<QuestionPanel open={open(FREE_TEXT)} busy={false} onAnswer={onAnswer} />);
    const box = screen.getByLabelText("Your answer");
    fireEvent.change(box, { target: { value: "14 days" } });
    fireEvent.keyDown(box, { key: "Enter" });
    fireEvent.keyDown(box, { key: "Enter", shiftKey: true });
    expect(onAnswer).not.toHaveBeenCalled();
  });

  it("the chord obeys the same guards as the button", () => {
    // A chord that fires while the button is disabled would submit a blank or in-flight
    // answer — the 400 the disabled state exists to prevent.
    const onAnswer = vi.fn();
    const { rerender } = render(<QuestionPanel open={open(FREE_TEXT)} busy={false} onAnswer={onAnswer} />);
    const box = screen.getByLabelText("Your answer");
    // Empty: not ready.
    fireEvent.keyDown(box, { key: "Enter", metaKey: true });
    expect(onAnswer).not.toHaveBeenCalled();
    // Filled but busy.
    fireEvent.change(box, { target: { value: "14 days" } });
    rerender(<QuestionPanel open={open(FREE_TEXT)} busy={true} onAnswer={onAnswer} />);
    fireEvent.keyDown(screen.getByLabelText("Your answer"), { key: "Enter", metaKey: true });
    expect(onAnswer).not.toHaveBeenCalled();
  });

  it("shows the question read-only to a NON-OWNER, with no control that would 404", () => {
    // POST /inputs is user-scoped, so a non-owner admin's Send answer 404s. The run view
    // is deliberately open to them, so the CONTENT stays and only the controls go —
    // hiding the question would turn a permissions boundary into a blank page.
    render(
      <QuestionPanel open={open(SINGLE_SELECT, 2)} busy={false} canSteer={false} onAnswer={vi.fn()} onCancel={vi.fn()} />,
    );
    expect(screen.getByText(/Only they can answer it/)).toBeTruthy();
    expect(screen.getByText("Which backend?")).toBeTruthy();
    expect(screen.getByText("Postgres table")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Send answer" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel run" })).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    // Options render as INERT text, not disabled buttons: a greyed control still invites
    // a click and then refuses it, which is the affordance-that-lies canSteer exists to
    // avoid.
    expect(screen.queryByRole("button", { name: /Postgres table/ })).toBeNull();
  });

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

  it("the read-only view still marks a later question", () => {
    render(<QuestionPanel open={open(SINGLE_SELECT, 3)} busy={false} canSteer={false} onAnswer={vi.fn()} />);
    expect(screen.getByText(/q3/)).toBeTruthy();
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

describe("UnreadableQuestion (the parked-but-unanswerable state)", () => {
  it("explains that answering is impossible, not merely unavailable", () => {
    // The failure it replaces was SILENCE: no panel, no explanation, until the deadline
    // failed the run. An absent affordance reads as "not loaded yet", so the reasonable
    // response was to wait — which is exactly what cannot help.
    render(<UnreadableQuestion busy={false} onCancel={vi.fn()} />);
    expect(screen.getByText(/could not be read/)).toBeTruthy();
    expect(screen.getByText(/nothing here to answer/)).toBeTruthy();
    // It names the one thing the user CAN do, and says what happens if they do nothing.
    expect(screen.getByText(/answer deadline expires, then fail/)).toBeTruthy();
  });

  it("offers NO composer — a Send here would 400 every time", () => {
    // Same reasoning that makes parseQuestionPayload return null rather than an id-less
    // payload: the api rejects an answer that names no question.
    render(<UnreadableQuestion busy={false} onCancel={vi.fn()} />);
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button", { name: "Send answer" })).toBeNull();
  });

  it("offers Cancel run only when a handler is wired, and disables it while busy", () => {
    const onCancel = vi.fn();
    const { rerender } = render(<UnreadableQuestion busy={false} />);
    expect(screen.queryByRole("button", { name: "Cancel run" })).toBeNull();
    rerender(<UnreadableQuestion busy={false} onCancel={onCancel} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel run" }));
    expect(onCancel).toHaveBeenCalledTimes(1);
    rerender(<UnreadableQuestion busy={true} onCancel={onCancel} />);
    expect((screen.getByRole("button", { name: "Cancel run" }) as HTMLButtonElement).disabled).toBe(true);
  });
});
