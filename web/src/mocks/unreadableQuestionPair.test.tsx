// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { listMessages } from "./store";
import { deriveOpenQuestion } from "../lib/runQuestion";
import { RunEventRow } from "../components/RunEvent";
import { UnreadableQuestion } from "../components/QuestionPanel";

afterEach(cleanup);

// 🔴 THIS FILE EXISTS BECAUSE THE DEFECT SPANNED TWO COMPONENTS, AND EACH ONE'S OWN TEST
// WAS GREEN.
//
// The card tells the user "The raw question is in the activity log below". Measured in
// the browser: the log contained the literal line `unrenderable question event` and
// nothing else. Both sites ran the same `parseQuestionPayload`, and the STRICT parse —
// which requires `question_id` because an answer must name one — discarded question text
// that was perfectly displayable.
//
// Neither unit test could see it. QuestionPanel's test renders the card in isolation and
// never reads the feed; RunEvent's test supplies its own payload, WITH an id. The promise
// and the thing that keeps it lived in different files, so only a test that renders BOTH
// against the SAME payload can fail when they disagree.
//
// The payload is taken from the shipped mock fixture rather than built here, deliberately:
// a hand-built one would be a third source of truth and could drift from the state the
// demo actually reaches. This is the producer-output rule (D-X) applied to a fixture — a
// test that supplies the value cannot notice the producer changing it.
const RUN = "run-unreadable-question";

function seededQuestion() {
  const q = [...listMessages(RUN)].filter((m) => m.kind === "question").pop();
  if (!q) throw new Error("fixture lost its question message");
  return q;
}

describe("the unreadable-question card's promise is kept by the feed", () => {
  it("the panel REFUSES the payload — the state the card exists for", () => {
    expect(deriveOpenQuestion([...listMessages(RUN)])).toBeNull();
  });

  it("the card makes a claim about the log, and the log honours it", () => {
    // Rendered together on purpose. If the card's wording changes, or the row stops
    // salvaging, this is the only test that notices.
    render(<UnreadableQuestion busy={false} onCancel={vi.fn()} />);
    expect(screen.getByText(/raw question is in the activity log below/)).toBeTruthy();

    const { container } = render(<RunEventRow msg={seededQuestion()} live={false} />);
    expect(container.textContent).toContain("Which cache TTL?");
    expect(container.textContent).toContain("TTL");
    expect(container.textContent).not.toContain("unrenderable");
  });

  it("still falls back to the muted line when there is genuinely NOTHING to show", () => {
    // The salvage must not degrade into "always render something". A payload with no
    // usable question text has nothing to salvage, and dressing that up as a question
    // card would be worse than the muted line — it would show an empty box where the
    // user expects the text the card just promised.
    const { container } = render(
      <RunEventRow msg={{ ...seededQuestion(), payload: { questions: [] } }} live={false} />,
    );
    expect(container.textContent).toContain("unrenderable question event");
  });
});
