// @vitest-environment jsdom
// PRD #118: the Select primitive must MERGE a caller-supplied className with its
// base field styling (like Input/Textarea), not clobber the base. This positive
// pin proves the two compose — base tokens AND caller classes both survive.

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Select, StatusPill } from "./ui";
import { runBadge } from "../lib/runBadge";
import type { RunStatus } from "../lib/api";

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
    // The label is the only thing that distinguishes them, and it must. (This asserted
    // "awaiting input" until the web-ux pass: correct about the de-underscored fallback,
    // and pinning the exact phrasing runBadge's own comment rejects. The tone assertion
    // above is what this test is FOR; the label belongs to the two tests below.)
    expect(parkedPill.textContent).not.toBe(gatedPill.textContent);
    expect(gatedPill.textContent).toContain("awaiting approval");
  });

  it("still falls back to neutral for a genuinely unknown status", () => {
    const { container } = render(<StatusPill status="future_status" />);
    expect(container.textContent).toContain("future status");
  });

  it("does NOT print the phrasing runBadge deliberately rejects", () => {
    // Two independent renderings of one status had silently diverged: runBadge says
    // "needs your answer" for awaiting_input, with the reasoning that "awaiting input"
    // reads as machine-waiting-for-machine — while this pill printed exactly that, on the
    // run header and every Runs-list row. Free until now only because awaiting_approval
    // happens to read fine de-underscored.
    const { container } = render(<StatusPill status="awaiting_input" />);
    expect(container.textContent).toContain("needs your answer");
    expect(container.textContent).not.toContain("awaiting input");
  });

  it("agrees with runBadge on the label for every status BOTH of them render", () => {
    // The real invariant, and the one that catches the NEXT divergence rather than this
    // one: a status carried by both surfaces must read the same in both. Anchored on
    // runBadge's own output, so adding a status to one and not the other reddens here.
    for (const status of ["awaiting_input", "awaiting_approval", "queued", "completed"]) {
      const badge = runBadge(
        {
          id: "r",
          status: status as RunStatus,
          mr_iid: null,
          mr_web_url: null,
          mr_state: null,
          failure_reason: null,
          stop_kind: null,
          health: "ok",
          health_reason: null,
          health_since: null,
          owner_name: "V",
          worker_name: null,
          is_mine: true,
          run_count: 1,
          created_at: "2026-07-28T00:00:00Z",
          updated_at: "2026-07-28T00:00:00Z",
        },
        Date.parse("2026-07-28T00:00:00Z"),
      );
      if (badge.kind !== "badge") continue;
      const { container, unmount } = render(<StatusPill status={status} />);
      expect(container.textContent?.trim()).toBe(badge.label);
      unmount();
    }
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
