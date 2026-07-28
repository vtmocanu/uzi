// @vitest-environment jsdom
// PRD #118: the Select primitive must MERGE a caller-supplied className with its
// base field styling (like Input/Textarea), not clobber the base. This positive
// pin proves the two compose — base tokens AND caller classes both survive.

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Select, StatusPill } from "./ui";

afterEach(cleanup);

describe("StatusPill", () => {
  // PRD #88: RUN_STATUS_TONES falls to `{ tone: "neutral" }` for an unknown status, so a
  // status missing from the map renders as a calm grey pill — the same treatment
  // `cancelled` gets. That is the exact wrong reading for a run blocked on a human, and
  // it fails silently: the label still reads correctly, only the colour lies.
  it("gives awaiting_input the same warn treatment as the plan gate", () => {
    const { container: parked } = render(<StatusPill status="awaiting_input" />);
    const { container: gated } = render(<StatusPill status="awaiting_approval" />);
    const parkedPill = parked.firstElementChild as HTMLElement;
    const gatedPill = gated.firstElementChild as HTMLElement;
    expect(parkedPill.className).toBe(gatedPill.className);
    // The pulse rides the inner dot, so a tone-only assertion would miss it.
    expect(parkedPill.querySelector(".animate-pulse")).not.toBeNull();
    // The label is the only thing that distinguishes them, and it must.
    expect(parkedPill.textContent).toContain("awaiting input");
    expect(gatedPill.textContent).toContain("awaiting approval");
  });

  it("still falls back to neutral for a genuinely unknown status", () => {
    const { container } = render(<StatusPill status="future_status" />);
    expect(container.textContent).toContain("future status");
  });
});

describe("Select", () => {
  it("merges a caller className with the base field styling", () => {
    render(
      <Select className="h-8 custom-x">
        <option value="">x</option>
      </Select>,
    );
    const select = screen.getByRole("combobox");
    // Base styling from INPUT_CLASS survives...
    expect(select.className).toContain("border-edge");
    expect(select.className).toContain("bg-raised");
    // ...and so do the caller-supplied classes.
    expect(select.className).toContain("h-8");
    expect(select.className).toContain("custom-x");
  });
});
