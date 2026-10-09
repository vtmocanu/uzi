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
    // Mock sizing: a 13px semibold figure over a w-16 bar.
    expect(bar.className).toContain("w-16");
    expect(bar.firstElementChild?.className).toContain("text-[13px]");
  });

  it("percent with a missing pct renders nothing rather than a fabricated 0%", () => {
    const { container } = renderCell({ progress: progress({ state: "percent", pct: null }) });
    expect(container.innerHTML).toBe("");
  });

  it.each([
    ["awaiting_approval", "plan gate"],
    ["awaiting_input", "question"],
    ["awaiting_followup", "follow-up"],
  ] as const)("waiting (%s): warn flag with `%s since HH:MM` from status_since", (status, reason) => {
    const { container } = renderCell({ status, status_since: STATUS_SINCE, progress: progress({ state: "waiting" }) });
    expect(cellOf(container)?.getAttribute("data-run-progress")).toBe("waiting");
    const flag = screen.getByText("waits on you");
    expect(flag.className).toContain("text-warn");
    expect(container.textContent).toBe(`waits on you${reason} since ${formatLocalTime(STATUS_SINCE)}`);
  });

  it("waiting without status_since keeps just the reason", () => {
    const { container } = renderCell({ status: "awaiting_approval", progress: progress({ state: "waiting" }) });
    expect(screen.getByText("waits on you")).toBeTruthy();
    expect(screen.getByText("plan gate")).toBeTruthy();
    expect(container.textContent).not.toContain("since");
  });

  it("stalled: danger flag with `since HH:MM` from health_since (not status_since)", () => {
    const { container } = renderCell({
      status_since: STATUS_SINCE,
      health: "stalled",
      health_since: HEALTH_SINCE,
      progress: progress({ state: "stalled", milestone_done: 1, milestone_total: 3 }),
    });
    const flag = screen.getByText("stalled");
    expect(flag.className).toContain("text-danger");
    expect(flag.textContent).toBe("◼ stalled");
    expect(flag.getAttribute("title")).toMatch(/^Stalled: /);
    expect(container.textContent).toContain(`since ${formatLocalTime(HEALTH_SINCE)}`);
    expect(container.textContent).not.toContain(`since ${formatLocalTime(STATUS_SINCE)}`);
    // A flag replaces the percent (PRD D4).
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("stalled while looping: the health word rides in the tooltip and accessible name", () => {
    renderCell({ health: "looping", progress: progress({ state: "stalled" }) });
    const flag = screen.getByText("stalled");
    expect(flag.getAttribute("title")).toMatch(/^Looping: /);
    expect(flag.textContent).toBe("◼ stalled (looping)");
    expect(flag.querySelector(".sr-only")?.textContent).toBe(" (looping)");
  });

  it("stalled keeps the replaced health pill's reason in its tooltip, sanitised", () => {
    renderCell({
      health: "stalled",
      health_reason: "no tool call for 12m\u202E\u0007",
      progress: progress({ state: "stalled" }),
    });
    const title = screen.getByText("stalled").getAttribute("title") ?? "";
    expect(title).toMatch(/^Stalled: .* no tool call for 12m$/);
    expect(title.includes("\u202E") || title.includes("\u0007")).toBe(false);
  });

  // queued/planning/parked repeat the adjacent status pill, so on a card row they are
  // screen-reader-only text with context; the state is still keyed by data-run-progress.
  it.each([
    ["limit_wait", {}, "limit wait"],
    ["pool_wait", {}, "waiting for pool"],
    ["recovery_wait", {}, "recovery wait"],
    ["paused", {}, "paused"],
    ["paused", { hold_reason: "credential_disabled" }, "waiting: credential disabled"],
  ] as const)("parked (%s): sr-only `Progress: <today's park word>`", (status, extra, text) => {
    const { container } = renderCell({ status, ...extra, progress: progress({ state: "parked" }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.getAttribute("data-run-progress")).toBe("parked");
    expect(cell.className).toBe("sr-only");
    expect(cell.textContent).toBe(`Progress: ${text}`);
  });

  it.each([
    ["queued", { status: "queued" }],
    ["planning", { is_planning: true }],
  ] as const)("%s: sr-only `Progress: %s`", (state, over) => {
    const { container } = renderCell({ ...over, progress: progress({ state }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.getAttribute("data-run-progress")).toBe(state);
    expect(cell.className).toBe("sr-only");
    expect(cell.textContent).toBe(`Progress: ${state}`);
  });

  it("none: a visible faint em dash, titled, with a screen-reader name", () => {
    const { container } = renderCell({ progress: progress({ state: "none" }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.className).toContain("text-faint");
    expect(cell.getAttribute("title")).toBe("No milestone plan to measure progress against");
    expect(cell.querySelector('[aria-hidden="true"]')?.textContent).toBe("—");
    expect(cell.querySelector(".sr-only")?.textContent).toBe("Progress: no estimate");
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
