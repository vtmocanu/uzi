// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunPriorityBadge } from "./RunPriorityBadge";

afterEach(cleanup);

// PRD #320 M6: the queue-priority pill for the list surfaces. The class→pill mapping is
// unit-tested in runBadge.test.ts (priorityBadge); this pins the component's two gates —
// QUEUED-only and non-null mapping — since only a queued run ever carries a class and a
// stale one on a running/terminal row must never render.
describe("RunPriorityBadge (PRD #320 M6)", () => {
  it("renders the Deprioritized pill for a queued background run, with its tooltip", () => {
    render(<RunPriorityBadge priority="background" status="queued" />);
    const badge = screen.getByText("Deprioritized");
    expect(badge).toBeTruthy();
    expect(badge.getAttribute("title")).toMatch(/yield/i);
  });

  it("renders the Expedited pill for a queued expedited run", () => {
    render(<RunPriorityBadge priority="expedited" status="queued" />);
    expect(screen.getByText("Expedited")).toBeTruthy();
  });

  it("renders nothing for a normal queued run (no pill)", () => {
    const { container } = render(<RunPriorityBadge priority="normal" status="queued" />);
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when the priority is absent (a pre-#320 api ⇒ normal)", () => {
    const { container } = render(<RunPriorityBadge status="queued" />);
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing on a non-queued run even with a (stale) class", () => {
    const { container } = render(<RunPriorityBadge priority="expedited" status="running" />);
    expect(container.firstChild).toBeNull();
  });
});
