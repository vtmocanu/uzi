// @vitest-environment jsdom
// PRD #2602 M1: the board Progress cell, one case per server-derived state plus the
// absent/unknown cases that must render nothing (an old server, a terminal run, a newer
// enum member).
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunProgressCell } from "./RunProgressCell";
import type { RunProgress } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";

afterEach(cleanup);

const STATUS_SINCE = "2026-10-09T06:50:00Z";
const HEALTH_SINCE = "2026-10-09T16:41:00Z";

function progress(over: Partial<RunProgress>): RunProgress {
  return {
    state: "none",
    pct: null,
    milestone_done: 0,
    milestone_total: 0,
    active_milestone_id: "",
    phase: "",
    ...over,
  };
}

type CellRun = Parameters<typeof RunProgressCell>[0]["run"];
function renderCell(over: Partial<CellRun>) {
  const run: CellRun = { status: "running", status_since: null, health_since: null, ...over };
  return render(<RunProgressCell run={run} />);
}

const cellOf = (c: HTMLElement) => c.querySelector("[data-run-progress]");

describe("RunProgressCell (PRD #2602)", () => {
  it("percent: NN% without ≈ and a 0-100 progressbar with a descriptive name", () => {
    const { container } = renderCell({
      progress: progress({ state: "percent", pct: 70, milestone_done: 2, milestone_total: 3 }),
    });
    const bar = screen.getByRole("progressbar");
    expect(bar.getAttribute("aria-valuenow")).toBe("70");
    expect(bar.getAttribute("aria-valuemin")).toBe("0");
    expect(bar.getAttribute("aria-valuemax")).toBe("100");
    expect(bar.getAttribute("aria-label")).toBe("Run progress: about 70%, 2 of 3 milestones done");
    expect(container.textContent).toBe("70%");
    expect(container.textContent).not.toContain("≈");
    // The fill is the brand tone at the percentage width, not a utilisation colour.
    const fill = bar.querySelector(".bg-brand") as HTMLElement;
    expect(fill.style.width).toBe("70%");
  });

  it("percent with a missing pct renders nothing rather than a fabricated 0%", () => {
    const { container } = renderCell({ progress: progress({ state: "percent", pct: null }) });
    expect(container.innerHTML).toBe("");
  });

  it("waiting: warn flag with `since HH:MM` from status_since", () => {
    const { container } = renderCell({
      status: "awaiting_input",
      status_since: STATUS_SINCE,
      progress: progress({ state: "waiting" }),
    });
    expect(cellOf(container)?.getAttribute("data-run-progress")).toBe("waiting");
    const flag = screen.getByText("waits on you");
    expect(flag.className).toContain("text-warn");
    expect(container.textContent).toContain(`since ${formatLocalTime(STATUS_SINCE)}`);
  });

  it("waiting without status_since omits the sub-line", () => {
    const { container } = renderCell({ status: "awaiting_approval", progress: progress({ state: "waiting" }) });
    expect(screen.getByText("waits on you")).toBeTruthy();
    expect(container.textContent).not.toContain("since");
  });

  it("stalled: danger flag with `since HH:MM` from health_since (not status_since)", () => {
    const { container } = renderCell({
      status_since: STATUS_SINCE,
      health_since: HEALTH_SINCE,
      progress: progress({ state: "stalled", milestone_done: 1, milestone_total: 3 }),
    });
    const flag = screen.getByText("stalled");
    expect(flag.className).toContain("text-danger");
    expect(flag.textContent).toBe("◼ stalled");
    expect(container.textContent).toContain(`since ${formatLocalTime(HEALTH_SINCE)}`);
    expect(container.textContent).not.toContain(`since ${formatLocalTime(STATUS_SINCE)}`);
    // A flag replaces the percent (PRD D4).
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it.each([
    ["limit_wait", {}, "limit wait"],
    ["pool_wait", {}, "waiting for pool"],
    ["recovery_wait", {}, "recovery wait"],
    ["paused", {}, "paused"],
    ["paused", { hold_reason: "credential_disabled" }, "waiting: credential disabled"],
  ] as const)("parked (%s) keeps today's park wording", (status, extra, text) => {
    const { container } = renderCell({ status, ...extra, progress: progress({ state: "parked" }) });
    expect(cellOf(container)?.getAttribute("data-run-progress")).toBe("parked");
    expect(container.textContent?.trim()).toBe(text);
    // The pause mark is a drawn, aria-hidden glyph (U+23F8 is tofu in many UI fonts).
    expect(container.querySelector('[data-glyph="pause"]')?.getAttribute("aria-hidden")).toBe("true");
  });

  it("queued: faint `queued`", () => {
    const { container } = renderCell({ status: "queued", progress: progress({ state: "queued" }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.textContent).toBe("queued");
    expect(cell.className).toContain("text-faint");
  });

  it("planning: plan-tone `planning`", () => {
    renderCell({ is_planning: true, progress: progress({ state: "planning" }) });
    expect(screen.getByText("planning").className).toContain("text-plan");
    expect(screen.getByText("planning").getAttribute("data-run-progress")).toBe("planning");
  });

  it("none: a faint em dash with a screen-reader name", () => {
    const { container } = renderCell({ progress: progress({ state: "none" }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.className).toContain("text-faint");
    expect(cell.textContent).toBe("—no progress estimate");
    expect(cell.querySelector('[aria-hidden="true"]')?.textContent).toBe("—");
  });

  it.each([
    ["absent (pre-feature server)", undefined],
    ["null (terminal run)", null],
  ])("renders nothing when progress is %s", (_label, value) => {
    const { container } = renderCell({ progress: value });
    expect(container.innerHTML).toBe("");
  });

  it("renders nothing for an unknown (newer server) state", () => {
    const { container } = renderCell({
      progress: progress({ state: "something_new" as unknown as RunProgress["state"] }),
    });
    expect(container.innerHTML).toBe("");
  });
});
