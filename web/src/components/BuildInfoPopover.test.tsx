// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import {
  BuildInfoPopover,
  ageInDays,
  displayVersion,
  formatCount,
  formatDay,
  formatUptime,
  liveUptimeSeconds,
} from "./BuildInfoPopover";
import { mockBuildInfo, mockBuildInfoUnstamped } from "../mocks/data";
import type { BuildInfo } from "../lib/api";

// BOTH fixtures live in ONE file, which is only possible because this component
// takes `info` as a prop. AppShell memoises GET /api/version in a module-scope
// promise with no reset seam and vitest isolates per FILE, so a component reading
// that promise directly could not be driven with two different responses here —
// the second render would reuse the first's resolved value and its assertion would
// pass for the wrong reason. That is why the popover is presentational.

afterEach(cleanup);

// A fixed "now" so the age and uptime assertions are not hostage to the wall
// clock. mockBuildInfo.founded is 2026-07-03.
const NOW = Date.parse("2026-07-28T12:00:00Z");

function popover() {
  return screen.getByRole("tooltip");
}

describe("formatters", () => {
  it("prefixes a display v only for a numeric version", () => {
    expect(displayVersion("0.6.0")).toBe("v0.6.0");
    expect(displayVersion("dev")).toBe("dev");
    expect(displayVersion("demo")).toBe("demo");
  });

  it("computes the age client-side from founded, at day granularity", () => {
    expect(ageInDays("2026-07-03", Date.parse("2026-07-28T00:00:00Z"))).toBe(25);
    // Same calendar day → 0, not 1: floor, so a fresh instance never claims a day.
    expect(ageInDays("2026-07-28", Date.parse("2026-07-28T23:59:00Z"))).toBe(0);
  });

  it("returns null rather than NaN for a missing or unparseable founded", () => {
    expect(ageInDays(undefined, NOW)).toBeNull();
    expect(ageInDays("", NOW)).toBeNull();
    expect(ageInDays("not-a-date", NOW)).toBeNull();
    // A clock skewed behind the founding date would otherwise render "-2 days old".
    expect(ageInDays("2026-07-03", Date.parse("2026-07-01T00:00:00Z"))).toBeNull();
  });

  it("formats a day in UTC, independent of the host locale and timezone", () => {
    expect(formatDay("2026-07-03")).toBe("3 Jul 2026");
    // An RFC3339 instant late in the UTC day must not roll to the 28th (or back to
    // the 26th) because the runner sits in another zone.
    expect(formatDay("2026-07-27T23:30:00Z")).toBe("27 Jul 2026");
    expect(formatDay(undefined)).toBeNull();
    expect(formatDay("nonsense")).toBeNull();
  });

  it("groups the commit count", () => {
    expect(formatCount(2105)).toBe("2,105");
    expect(formatCount(7)).toBe("7");
  });

  it("renders a duration in at most two units", () => {
    expect(formatUptime(48)).toBe("48s");
    expect(formatUptime(12 * 60)).toBe("12m");
    expect(formatUptime(4 * 3600 + 12 * 60)).toBe("4h 12m");
    expect(formatUptime(3 * 86400 + 4 * 3600)).toBe("3d 4h");
    expect(formatUptime(3 * 86400)).toBe("3d");
    // 0 is a legitimate uptime in a process's first second — the server omits
    // UNKNOWN, so anything that arrives here is a real reading.
    expect(formatUptime(0)).toBe("0s");
  });

  it("re-bases uptime against the wall clock so a long-lived session stays honest", () => {
    const fetched = Date.parse("2026-07-28T12:00:00Z");
    // Two hours after the fetch the popover must not still report the reading the
    // API gave at mount.
    expect(liveUptimeSeconds(60, fetched, fetched + 2 * 3600_000)).toBe(60 + 2 * 3600);
    // A clock that jumped backwards must not subtract.
    expect(liveUptimeSeconds(60, fetched, fetched - 5000)).toBe(60);
  });
});

describe("BuildInfoPopover — fully-stamped build", () => {
  it("shows every coordinate, with the SHA truncated for display", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);

    const pop = popover();
    expect(pop.textContent).toContain("uzi v0.4.2");
    expect(pop.textContent).toContain("25 days old");
    expect(pop.textContent).toContain("2,105 commits");
    expect(pop.textContent).toContain("Founded");
    expect(pop.textContent).toContain("3 Jul 2026");
    expect(pop.textContent).toContain("Built");
    expect(pop.textContent).toContain("Commit");
    // Truncated to 7 for display; the full 40 stays in the response so it is
    // greppable.
    expect(pop.textContent).toContain("366a282");
    expect(pop.textContent).not.toContain("366a282d52095312f54b99698b241ac872e20284");
    expect(pop.textContent).toContain("Uptime");
    expect(pop.textContent).toContain("3d 4h");
  });

  it("labels the trigger with the display version and drops the native title", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    const trigger = screen.getByRole("button", { name: "v0.4.2" });
    // The old footer carried title={`uzi ${label}`}. A browser tooltip firing
    // alongside this popover is two panels saying different things.
    expect(trigger.getAttribute("title")).toBeNull();
  });
});

describe("BuildInfoPopover — un-stamped dev build (the laptop case)", () => {
  it("OMITS the rows the server did not send rather than rendering unknowns", () => {
    render(<BuildInfoPopover info={mockBuildInfoUnstamped} now={NOW} />);

    const pop = popover();
    // "dev", never "vdev".
    expect(pop.textContent).toContain("uzi dev");
    expect(screen.getByRole("button", { name: "dev" })).toBeTruthy();
    // Age still works — it is computed here from `founded`, which is always sent.
    expect(pop.textContent).toContain("25 days old");
    expect(pop.textContent).toContain("Founded");
    // The three stamped fields are ABSENT, and so are their labels. A row reading
    // "Built —" would be the same "claiming to know things it does not" the
    // server's omit rule exists to prevent.
    expect(pop.textContent).not.toContain("Built");
    expect(pop.textContent).not.toContain("Commit");
    expect(pop.textContent).not.toContain("Uptime");
    // No commit count either: M3 is independently droppable, so an age-only
    // subtitle is a supported final state, not a loading intermediate.
    expect(pop.textContent).not.toContain("commits");
  });

  it("does not render an uptime row for an absent reading", () => {
    // Guards the difference between "absent" and 0: a bare `omitempty` int on the
    // server would have swallowed a genuine 0, and rendering "0s" for UNKNOWN here
    // would reintroduce the same conflation from the other end.
    const { rerender } = render(<BuildInfoPopover info={mockBuildInfoUnstamped} now={NOW} />);
    expect(popover().textContent).not.toContain("Uptime");

    rerender(<BuildInfoPopover info={{ ...mockBuildInfoUnstamped, uptime_seconds: 0 }} now={NOW} />);
    expect(popover().textContent).toContain("Uptime");
    expect(popover().textContent).toContain("0s");
  });

  // A `null` where the type says `number | undefined` is not reachable from OUR Go
  // server — `*int` with `omitempty` omits the key — but `api: typeof realApi`
  // cannot enforce the degraded shape (every field but version and founded is
  // optional, so a thin mock typechecks), and these two fields are the ones where
  // a wrong value is silent rather than loud.
  it("treats a null uptime as UNKNOWN, not as zero", () => {
    // Measured before the guard: `info.uptime_seconds === undefined` is false for
    // null, so it reached formatUptime and Math.floor(null) rendered "Uptime 0s" —
    // the exact absent-vs-zero conflation M1 made the server field a POINTER to
    // prevent, reintroduced at the consuming end.
    render(
      <BuildInfoPopover
        info={{ ...mockBuildInfoUnstamped, uptime_seconds: null as unknown as number }}
        now={NOW}
      />,
    );
    expect(popover().textContent).not.toContain("Uptime");
    expect(popover().textContent).not.toContain("0s");
  });

  it("treats a null commit count as UNKNOWN instead of throwing the shell down", () => {
    // Measured before the guard: this THREW (TypeError, .toLocaleString of null).
    // There is no ErrorBoundary anywhere in web/src, so a throw here unmounts the
    // tree — the same failure AppShell.buildinfo.failure.test.tsx guards for a
    // rejected fetch.
    render(
      <BuildInfoPopover
        info={{ ...mockBuildInfoUnstamped, commits: null as unknown as number }}
        now={NOW}
      />,
    );
    expect(popover().textContent).toContain("25 days old");
    expect(popover().textContent).not.toContain("commit");
  });

  it("degrades NaN and a non-string commit to UNKNOWN rather than to 'NaNs'", () => {
    render(
      <BuildInfoPopover
        info={{
          ...mockBuildInfoUnstamped,
          uptime_seconds: Number.NaN,
          commits: Number.NaN,
          commit: 12345 as unknown as string,
        }}
        now={NOW}
      />,
    );
    const text = popover().textContent ?? "";
    expect(text).not.toContain("NaN");
    expect(text).not.toContain("Uptime");
    expect(text).not.toContain("Commit");
    expect(text).toContain("25 days old");
  });

  it("survives a response missing founded without rendering NaN", () => {
    // Not a shape the server produces — `founded` is a const it always sends — but
    // an older server or a hand-rolled test double must not put "NaN days old" in
    // the chrome.
    render(<BuildInfoPopover info={{ version: "dev" } as unknown as BuildInfo} now={NOW} />);
    expect(popover().textContent).toContain("uzi dev");
    expect(popover().textContent).not.toContain("NaN");
    expect(popover().textContent).not.toContain("Founded");
  });
});

describe("BuildInfoPopover — open on hover AND focus AND tap", () => {
  const trigger = () => screen.getByRole("button", { name: "v0.4.2" });

  it("starts closed", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("opens on hover and closes when the pointer leaves", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    // The host, not the button: the pointer has to be able to travel onto the
    // panel without it closing underneath.
    const host = popover().parentElement!;
    fireEvent.mouseEnter(host);
    expect(popover().getAttribute("data-open")).toBe("true");
    fireEvent.mouseLeave(host);
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("opens on focus and closes on blur — hover is not a keyboard affordance", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    fireEvent.focus(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
    fireEvent.blur(trigger());
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("opens on tap — and a click never toggles it shut", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    fireEvent.click(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
    // A second click on desktop (where the badge is already hovered open) must
    // leave it open rather than flicker it.
    fireEvent.click(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
  });

  it("closes on Escape", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    fireEvent.focus(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
    fireEvent.keyDown(trigger(), { key: "Escape" });
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("describes the trigger with the popover, by an id scoped to this instance", () => {
    render(
      <>
        <BuildInfoPopover info={mockBuildInfo} now={NOW} />
        <BuildInfoPopover info={mockBuildInfo} now={NOW} />
      </>,
    );
    // TWO SidebarContent mounts exist simultaneously in the real shell (desktop
    // aside + mobile drawer), so a hardcoded id would put a duplicate in the
    // document and make aria-describedby ambiguous.
    const ids = screen.getAllByRole("button", { name: "v0.4.2" }).map((b) => b.getAttribute("aria-describedby"));
    expect(ids).toHaveLength(2);
    expect(ids[0]).toBeTruthy();
    expect(ids[0]).not.toBe(ids[1]);
    for (const id of ids) expect(document.getElementById(id!)).not.toBeNull();
  });
});
