// @vitest-environment jsdom
// PRD #2602 M1: the board Progress cell, one case per server-derived state plus the
// absent/unknown cases that must render nothing (an old server, a terminal run, a newer
// enum member).
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RunProgressCell, stallTitle } from "./RunProgressCell";
import { formatElapsed } from "../lib/runBadge";
import type { RunProgress } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";

afterEach(cleanup);

const NOW = Date.parse("2026-10-09T18:46:00Z");
const STATUS_SINCE = "2026-10-09T18:40:00Z"; // 6m before NOW
const HEALTH_SINCE = "2026-10-09T16:41:00Z"; // 2h 5m before NOW
const dur = (iso: string, now = NOW) => formatElapsed(now - Date.parse(iso));

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
function renderCell(over: Partial<CellRun>, now: number = NOW) {
  const run: CellRun = { status: "running", status_since: null, health_since: null, ...over };
  return render(<RunProgressCell run={run} now={now} />);
}

// The sr-only sentence must occur exactly once, so assistive tech does not hear it twice.
const srSince = (iso: string) => `, for ${dur(iso)}, since ${formatLocalTime(iso)}`;
const occurrences = (hay: string, needle: string) => hay.split(needle).length - 1;

const cellOf = (c: HTMLElement) => c.querySelector("[data-run-progress]");

describe.each([
  ["stalled", "Stalled: no progress is being made; the percentage returns once activity resumes."],
  ["looping", "Looping: the run keeps repeating the same steps; the percentage returns once it moves on."],
] as const)("stallTitle (%s)", (health, base) => {
  it.each([
    ["absent", undefined, ""],
    ["empty", "", ""],
    ["lowercase", "no tool call for 25m", " Reason: no tool call for 25m"],
    ["identifier", "tool_call_timeout", " Reason: tool_call_timeout"],
    ["whitespace and case", "  lowerCase reason  ", " Reason:   lowerCase reason  "],
    ["unsafe characters", "no\u202E tool\u0007 call", " Reason: no tool call"],
    ["sanitized to empty", "\u202E\u0007", ""],
  ])("preserves the base sentence with a %s reason", (_label, health_reason, suffix) => {
    expect(stallTitle({ health, health_reason })).toBe(base + suffix);
  });
});

describe("RunProgressCell (PRD #2602)", () => {
  it("percent yields to the adjacent running status pill", () => {
    const { container } = renderCell({
      progress: progress({ state: "percent", pct: 70, milestone_done: 2, milestone_total: 3 }),
    });
    expect(container.innerHTML).toBe("");
  });

  it("percent with a missing pct renders nothing rather than a fabricated 0%", () => {
    const { container } = renderCell({ progress: progress({ state: "percent", pct: null }) });
    expect(container.innerHTML).toBe("");
  });

  it.each([
    ["awaiting_approval", "plan gate", `Waiting for your plan approval since ${formatLocalTime(STATUS_SINCE)}.`],
    ["awaiting_input", "question", `Waiting for your answer since ${formatLocalTime(STATUS_SINCE)}.`],
    ["awaiting_followup", "follow-up", `Waiting for your follow-up since ${formatLocalTime(STATUS_SINCE)}.`],
  ] as const)("waiting (%s): `waits on you · %s · <elapsed>` from status_since", (status, reason, title) => {
    const { container } = renderCell({ status, status_since: STATUS_SINCE, progress: progress({ state: "waiting" }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.getAttribute("data-run-progress")).toBe("waiting");
    expect(cell.className).not.toContain("flex-col");
    expect(dur(STATUS_SINCE)).toBe("6m");
    expect(container.textContent).toBe(`waits on you · ${reason} · 6m${srSince(STATUS_SINCE)}`);
    expect(occurrences(container.textContent ?? "", srSince(STATUS_SINCE))).toBe(1);
    // The reason is visible to assistive tech; only the duration is aria-hidden.
    const hidden = Array.from(container.querySelectorAll('[aria-hidden="true"]:not(.rounded-full)')).map((e) => e.textContent);
    expect(hidden).toEqual([" · 6m"]);
    expect(container.querySelector(".sr-only")?.textContent).toBe(srSince(STATUS_SINCE));
    expect(container.querySelector("[title]")?.className).toContain("text-warn");
    expect(container.querySelector("[title]")?.getAttribute("title")).toBe(
      title,
    );
  });

  it("a revising run (awaiting_approval, is_revising, planning progress) does not wait on you", () => {
    const { container } = renderCell({
      status: "awaiting_approval",
      is_revising: true,
      status_since: STATUS_SINCE,
      progress: progress({ state: "planning" }),
    });
    expect(screen.queryByText("waits on you")).toBeNull();
    expect(container.textContent).not.toContain("plan gate");
  });

  it.each([
    ["missing", null],
    ["unparseable", "not-a-date"],
  ])("waiting with a %s status_since keeps just `waits on you · <reason>`", (_l, since) => {
    const { container } = renderCell({
      status: "awaiting_approval",
      status_since: since,
      progress: progress({ state: "waiting" }),
    });
    expect(container.textContent).toBe("waits on you · plan gate");
    expect(container.querySelector('[aria-hidden="true"]:not(.rounded-full)')).toBeNull();
    expect(container.querySelector(".sr-only")).toBeNull();
    expect(container.querySelector("[title]")?.getAttribute("title")).toBe(
      "This run is waiting on you: a plan approval, a question or a follow-up.",
    );
  });

  it.each([
    ["valid time", STATUS_SINCE, `waits on you · 6m${srSince(STATUS_SINCE)}`],
    ["missing time", null, "waits on you"],
  ])("waiting with an unknown reason and %s uses the fallback title", (_label, since, text) => {
    const { container } = renderCell({
      status: "unknown",
      status_since: since,
      progress: progress({ state: "waiting" }),
    });
    expect(container.textContent).toBe(text);
    expect(container.querySelector("[title]")?.getAttribute("title")).toBe(
      "This run is waiting on you: a plan approval, a question or a follow-up.",
    );
  });

  it("stalled: danger flag with the elapsed time from health_since (not status_since)", () => {
    const { container } = renderCell({
      status_since: STATUS_SINCE,
      health: "stalled",
      health_reason: "no tool call for 25m",
      health_since: HEALTH_SINCE,
      progress: progress({ state: "stalled", milestone_done: 1, milestone_total: 3 }),
    });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.className).not.toContain("flex-col");
    expect(dur(HEALTH_SINCE)).toBe("2h 5m");
    expect(container.textContent).toBe(`◼ stalled · 2h 5m${srSince(HEALTH_SINCE)}`);
    expect(occurrences(container.textContent ?? "", srSince(HEALTH_SINCE))).toBe(1);
    expect(container.textContent).not.toContain("6m");
    const flag = container.querySelector("[title]") as HTMLElement;
    expect(flag.className).toContain("text-danger");
    expect(flag.getAttribute("title")).toBe(
      `Since ${formatLocalTime(HEALTH_SINCE)}. Stalled: no progress is being made; the percentage returns once activity resumes. Reason: no tool call for 25m`,
    );
    const hidden = Array.from(container.querySelectorAll('[aria-hidden="true"]')).map((e) => e.textContent);
    expect(hidden).toEqual(["◼", " · 2h 5m"]);
    // A flag replaces the percent (PRD D4).
    expect(screen.queryByRole("progressbar")).toBeNull();
  });

  it("stalled while looping: the health word rides in the tooltip and accessible name", () => {
    const { container } = renderCell({
      health: "looping",
      health_reason: "repeat_step",
      health_since: HEALTH_SINCE,
      progress: progress({ state: "stalled" }),
    });
    const flag = container.querySelector("[title]") as HTMLElement;
    expect(flag.getAttribute("title")).toBe(
      `Since ${formatLocalTime(HEALTH_SINCE)}. Looping: the run keeps repeating the same steps; the percentage returns once it moves on. Reason: repeat_step`,
    );
    expect(flag.textContent).toBe(`◼ stalled (looping) · 2h 5m${srSince(HEALTH_SINCE)}`);
    expect(screen.getByText("(looping)", { exact: false }).className).toContain("sr-only");
  });

  it.each([
    ["missing", null],
    ["unparseable", "not-a-date"],
  ])("stalled with a %s health_since is a bare `◼ stalled` with the plain tooltip", (_l, since) => {
    const { container } = renderCell({ health: "stalled", health_since: since, progress: progress({ state: "stalled" }) });
    expect(container.textContent).toBe("◼ stalled");
    expect(container.textContent).not.toContain("since");
    expect(container.querySelector("[title]")?.getAttribute("title")).toMatch(/^Stalled: /);
  });

  it("a future timestamp clamps the duration to 0s", () => {
    const future = new Date(NOW + 5 * 60_000).toISOString();
    const { container } = renderCell({ health: "stalled", health_since: future, progress: progress({ state: "stalled" }) });
    expect(container.textContent).toContain("◼ stalled · 0s");
  });

  it("re-rendering with a later clock advances the elapsed time", () => {
    const run: CellRun = {
      status: "running",
      status_since: null,
      health: "stalled",
      health_since: HEALTH_SINCE,
      progress: progress({ state: "stalled" }),
    };
    const { container, rerender } = render(<RunProgressCell run={run} now={NOW} />);
    expect(container.textContent).toContain("· 2h 5m");
    rerender(<RunProgressCell run={run} now={NOW + 60_000} />);
    expect(container.textContent).toContain("· 2h 6m");
    expect(container.textContent).not.toContain("2h 5m");
  });

  it("stalled keeps the replaced health pill's reason in its tooltip, sanitised", () => {
    const { container } = renderCell({
      health: "stalled",
      health_reason: "no tool call for 12m\u202E\u0007",
      progress: progress({ state: "stalled" }),
    });
    const title = container.querySelector("[title]")?.getAttribute("title") ?? "";
    expect(title).toBe(
      "Stalled: no progress is being made; the percentage returns once activity resumes. Reason: no tool call for 12m",
    );
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

  it("none: screen-reader-only no estimate without a visible placeholder", () => {
    const { container } = renderCell({ progress: progress({ state: "none" }) });
    const cell = cellOf(container) as HTMLElement;
    expect(cell.className).toBe("sr-only");
    expect(cell.textContent).toBe("Progress: no estimate");
    expect(cell.querySelector('[aria-hidden="true"]')).toBeNull();
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
