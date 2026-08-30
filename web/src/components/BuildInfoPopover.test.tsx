// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import {
  BuildInfoPopover,
  ageInDays,
  displayVersion,
  formatCount,
  formatDay,
  formatStamp,
  formatUptime,
  liveUptimeSeconds,
} from "./BuildInfoPopover";
import { mockBuildInfo, mockBuildInfoNoUptime, mockBuildInfoUnstamped } from "../mocks/data";
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

  it("keeps the TIME on a build stamp, in UTC and explicitly labelled", () => {
    // Two images can ship the same day, which is exactly when someone consults
    // this field — a day-granular stamp is silent in the case it exists for.
    expect(formatStamp("2026-07-26T18:42:07Z")).toBe("26 Jul 2026 18:42 UTC");
    // Not the runner's local day/hour: an instant late in the UTC day must not
    // roll, and midnight must render 00:00 rather than 24:00.
    expect(formatStamp("2026-07-27T23:30:00Z")).toBe("27 Jul 2026 23:30 UTC");
    expect(formatStamp("2026-07-27T00:05:00Z")).toBe("27 Jul 2026 00:05 UTC");
    // An offset instant is normalised to UTC, not rendered as written.
    expect(formatStamp("2026-07-26T20:42:07+02:00")).toBe("26 Jul 2026 18:42 UTC");
    expect(formatStamp(undefined)).toBeNull();
    expect(formatStamp("nonsense")).toBeNull();
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

  it("renders the PRDs row as `N done · M open` when both counts are stamped", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    const pop = popover();
    expect(pop.textContent).toContain("PRDs");
    // Variant A: done in the accent, `· open` faint — asserted on the visible text.
    expect(pop.textContent).toContain("80 done · 32 open");
  });

  it("carries the FULL 40-char SHA into the DOM while displaying seven", () => {
    // PRD :72 stamps the full SHA "so the stored value stays greppable and
    // linkable", and M1 gates it on len==40 && hex to guarantee it. Before this,
    // the panel rendered .slice(0,7) and nothing else, so the full value never
    // reached the document and an operator was back to prefix-matching by hand.
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    const full = "366a282d52095312f54b99698b241ac872e20284";

    // EXACTLY TWO rows carry `full`, and the count is the assertion: `full` means
    // "the display is an abbreviation of this", so adding it to Founded or Uptime —
    // whose displays are not abbreviations — would be wrong and would otherwise
    // ship green. It also bounds the a11y exposure noted at Row: a titled row is
    // only safe while it has text content.
    expect(popover().querySelectorAll("dd[data-full]")).toHaveLength(2);
    // Displayed short…
    expect(screen.getByText("366a282")).toBeTruthy();
    // …carried long, in both a machine-readable attribute and a human-readable one.
    const commitDd = [...popover().querySelectorAll<HTMLElement>("dd[data-full]")].find(
      (el) => el.dataset.full === full,
    );
    expect(commitDd).toBeTruthy();
    expect(commitDd!.getAttribute("title")).toBe(full);
    expect(commitDd!.textContent).toBe("366a282");
  });

  it("carries the raw RFC3339 build stamp alongside the minute-granular display", () => {
    render(
      <BuildInfoPopover
        info={{ ...mockBuildInfo, built_at: "2026-07-26T18:42:07Z" }}
        now={NOW}
      />,
    );
    const builtDd = [...popover().querySelectorAll<HTMLElement>("dd[data-full]")].find(
      (el) => el.dataset.full === "2026-07-26T18:42:07Z",
    );
    expect(builtDd).toBeTruthy();
    // The seconds are the thing the rendered form drops, and the one time you
    // want them is when two builds are minutes apart.
    expect(builtDd!.getAttribute("title")).toBe("2026-07-26T18:42:07Z");
    expect(builtDd!.textContent).toBe("26 Jul 2026 18:42 UTC");
  });

  it("renders the build time, not just the day", () => {
    render(
      <BuildInfoPopover
        info={{ ...mockBuildInfo, built_at: "2026-07-26T18:42:07Z" }}
        now={NOW}
      />,
    );
    expect(popover().textContent).toContain("26 Jul 2026 18:42 UTC");
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
  it("OMITS the LDFLAGS rows the server did not send, and KEEPS uptime", () => {
    render(<BuildInfoPopover info={mockBuildInfoUnstamped} now={NOW} />);

    const pop = popover();
    // "dev", never "vdev".
    expect(pop.textContent).toContain("uzi dev");
    expect(screen.getByRole("button", { name: "dev" })).toBeTruthy();
    // Age still works — it is computed here from `founded`, which is always sent.
    expect(pop.textContent).toContain("25 days old");
    expect(pop.textContent).toContain("Founded");
    // The three STAMPED fields are absent, and so are their labels. A row reading
    // "Built —" would be the same "claiming to know things it does not" the
    // server's omit rule exists to prevent.
    expect(pop.textContent).not.toContain("Built");
    expect(pop.textContent).not.toContain("Commit");
    // No commit count either: M3 is independently droppable, so an age-only
    // subtitle is a supported final state, not a loading intermediate.
    expect(pop.textContent).not.toContain("commits");
    // …but UPTIME IS PRESENT, and that is the point of this fixture. A laptop
    // stack omits only what ldflags would have stamped; the process is running, so
    // `handler.New()`'s startedAt is set and Version emits uptime_seconds. Asserting
    // its ABSENCE here — as this test used to — pinned a body no server sends.
    expect(pop.textContent).toContain("Uptime");
    expect(pop.textContent).toContain("4m");
  });

  it("does not render an uptime row when uptime itself is unknown", () => {
    // The two-key body: a struct-literal `Handler` leaves startedAt zero and Version
    // omits rather than reporting two millennia. A real wire shape, but a TEST
    // construction — not a laptop's, which is the distinction mockBuildInfoNoUptime
    // exists to keep.
    //
    // Guards the difference between "absent" and 0: a bare `omitempty` int on the
    // server would have swallowed a genuine 0, and rendering "0s" for UNKNOWN here
    // would reintroduce the same conflation from the other end.
    const { rerender } = render(<BuildInfoPopover info={mockBuildInfoNoUptime} now={NOW} />);
    expect(popover().textContent).not.toContain("Uptime");

    rerender(<BuildInfoPopover info={{ ...mockBuildInfoNoUptime, uptime_seconds: 0 }} now={NOW} />);
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

  it("omits the PRDs row on a dev build that stamps neither count", () => {
    // The laptop shape carries no prds_done/prds_open, exactly as it carries no
    // commit/built_at — the row simply is not there, never "0" or "unknown".
    render(<BuildInfoPopover info={mockBuildInfoUnstamped} now={NOW} />);
    expect(popover().textContent).not.toContain("PRDs");
  });

  it("omits the PRDs row when only one of the pair is present (a half-known count)", () => {
    // Both-or-neither: the two counts are stamped together in CI, so a lone field
    // is treated as unknown and renders nothing rather than a misleading half-row.
    const { rerender } = render(
      <BuildInfoPopover info={{ ...mockBuildInfoUnstamped, prds_done: 80 }} now={NOW} />,
    );
    expect(popover().textContent).not.toContain("PRDs");
    expect(popover().textContent).not.toContain("80 done");

    rerender(
      <BuildInfoPopover info={{ ...mockBuildInfoUnstamped, prds_open: 32 }} now={NOW} />,
    );
    expect(popover().textContent).not.toContain("PRDs");
    expect(popover().textContent).not.toContain("32 open");
  });

  it("degrades a null or NaN PRD count to absent, not to a 'NaN' row", () => {
    render(
      <BuildInfoPopover
        info={{
          ...mockBuildInfoUnstamped,
          prds_done: null as unknown as number,
          prds_open: Number.NaN,
        }}
        now={NOW}
      />,
    );
    const text = popover().textContent ?? "";
    expect(text).not.toContain("PRDs");
    expect(text).not.toContain("NaN");
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

  it("closes on Escape after FOCUS opened it", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    fireEvent.focus(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
    fireEvent.keyDown(document, { key: "Escape" });
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("closes on Escape after HOVER opened it, with focus elsewhere", () => {
    // The handler used to live on the button's onKeyDown, so this case did
    // nothing: a mouse user hovers the badge open, presses Escape, and the key
    // event goes to whatever actually has focus. APG's tooltip pattern wants Esc
    // to dismiss a hover-shown tooltip too.
    render(
      <>
        <button type="button">elsewhere</button>
        <BuildInfoPopover info={mockBuildInfo} now={NOW} />
      </>,
    );
    const other = screen.getByRole("button", { name: "elsewhere" });
    other.focus();
    expect(document.activeElement).toBe(other);

    fireEvent.mouseEnter(popover().parentElement!);
    expect(popover().getAttribute("data-open")).toBe("true");

    fireEvent.keyDown(document, { key: "Escape" });
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("ignores other keys", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    fireEvent.focus(trigger());
    fireEvent.keyDown(document, { key: "a" });
    expect(popover().getAttribute("data-open")).toBe("true");
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

describe("BuildInfoPopover — update signal: pip + popover row (PRD #836 M4)", () => {
  // A stamped build the server says is behind. published_at is 3 calendar days
  // before NOW (2026-07-28), so the age helper reused for the row reads "3 days ago".
  const behind: BuildInfo = {
    ...mockBuildInfo,
    latest: {
      version: "v0.5.0",
      name: "Hosted worker drain controls",
      published_at: "2026-07-25T00:00:00Z",
      notes_url: "https://github.com/vtmocanu/uzi/releases/tag/v0.5.0",
      security: false,
    },
    update_available: true,
    far_behind: false,
  };
  const pip = () => document.querySelector("[data-update-pip]");
  const updateRow = () => document.querySelector("[data-update-row]");

  it("lights the pip on the badge when the server reports update_available === true", () => {
    render(<BuildInfoPopover info={behind} now={NOW} />);
    expect(pip()).not.toBeNull();
    // Purely visual, so it must not leak into the trigger's accessible name.
    expect(screen.getByRole("button", { name: "v0.4.2" })).toBeTruthy();
  });

  it("shows NO pip when update_available is explicitly false (checked, up to date)", () => {
    // The distinct middle state: the check ran and the instance is current. The pip
    // renders only from `=== true`, never a client-side compare.
    render(
      <BuildInfoPopover info={{ ...behind, update_available: false }} now={NOW} />,
    );
    expect(pip()).toBeNull();
  });

  it("shows NO pip when update_available is omitted (never checked / disabled)", () => {
    // The unstamped dev fixture carries neither the flag nor a `latest` object — the
    // unknown state, distinct from a checked `false`.
    render(<BuildInfoPopover info={mockBuildInfoUnstamped} now={NOW} />);
    expect(pip()).toBeNull();
  });

  it("hides the pip in the collapsed rail (no room), even when an update is available", () => {
    render(<BuildInfoPopover info={behind} collapsed now={NOW} />);
    expect(pip()).toBeNull();
  });

  it("renders the update row with the v-prefixed version and a `· N days ago` tail", () => {
    render(<BuildInfoPopover info={behind} now={NOW} />);
    const row = updateRow();
    expect(row).not.toBeNull();
    expect(row!.textContent).toContain("v0.5.0");
    expect(row!.textContent).toContain("available");
    expect(row!.textContent).toContain("3 days ago");
    // The server-supplied tag is already v-prefixed and displayVersion is idempotent,
    // so it must not double up to "vv0.5.0".
    expect(row!.textContent).not.toContain("vv0.5.0");
  });

  it("omits the `· N days ago` tail gracefully when published_at is absent", () => {
    render(
      <BuildInfoPopover
        info={{ ...behind, latest: { version: "v0.5.0" } }}
        now={NOW}
      />,
    );
    const row = updateRow();
    expect(row).not.toBeNull();
    expect(row!.textContent).toContain("v0.5.0");
    expect(row!.textContent).toContain("available");
    expect(row!.textContent).not.toContain("days ago");
    expect(row!.textContent).not.toContain("null");
  });

  it("renders NOTHING when latest is omitted (unknown state), even if a flag leaks true", () => {
    // `update_available` true but no `latest` object is not a shape our server sends,
    // but the row must degrade to absent rather than throwing on latest.version.
    render(
      <BuildInfoPopover
        info={{ ...mockBuildInfo, latest: undefined, update_available: true }}
        now={NOW}
      />,
    );
    expect(updateRow()).toBeNull();
    // …and with both the flag and latest omitted (the true unknown state), nothing.
    cleanup();
    render(
      <BuildInfoPopover
        info={{ ...mockBuildInfo, latest: undefined, update_available: undefined }}
        now={NOW}
      />,
    );
    expect(updateRow()).toBeNull();
  });

  it("shows the admin `Update guide` affordance linking to admin settings", () => {
    render(<BuildInfoPopover info={behind} isAdmin now={NOW} />);
    const link = screen.getByRole("link", { name: /Update guide/ });
    expect(link.getAttribute("href")).toBe("/admin/settings");
    expect(updateRow()!.textContent).not.toContain("Ask your operator");
  });

  it("shows the member `Ask your operator` note with no link", () => {
    render(<BuildInfoPopover info={behind} isAdmin={false} now={NOW} />);
    expect(updateRow()!.textContent).toContain("Ask your operator");
    expect(screen.queryByRole("link", { name: /Update guide/ })).toBeNull();
  });
});

describe("BuildInfoPopover — PRDs row links to the repo prds/ directory (PRD #415 M2)", () => {
  // A directory is served under /tree/, not /blob/ (PRD #415 M3 review nit).
  const PRDS_URL = "https://github.com/vtmocanu/uzi/tree/main/prds";

  it("renders the PRDs count as an external link, keeping the `N done · M open` text", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    const link = screen.getByRole("link", { name: /80 done.*32 open/ });
    expect(link.getAttribute("href")).toBe(PRDS_URL);
    // External target → new tab + a dropped opener, the app's rule for off-app links.
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noopener noreferrer");
    // The Variant-A visual is unchanged, just wrapped in the anchor.
    expect(link.textContent).toContain("80 done");
    expect(link.textContent).toContain("32 open");
  });

  it("renders no PRDs link when the counts are absent", () => {
    render(<BuildInfoPopover info={mockBuildInfoUnstamped} now={NOW} />);
    expect(screen.queryByRole("link")).toBeNull();
  });
});

describe("BuildInfoPopover — Changelog button + focus-within (PRD #415 M2)", () => {
  const trigger = () => screen.getByRole("button", { name: "v0.4.2" });
  const changelogButton = () => screen.getByRole("button", { name: "Changelog" });

  it("renders no Changelog button when no handler is wired", () => {
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} />);
    expect(screen.queryByRole("button", { name: "Changelog" })).toBeNull();
  });

  it("renders a Changelog button inside the popover panel when a handler is wired", () => {
    const onOpen = vi.fn();
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={onOpen} />);
    // The button lives INSIDE the popover panel (the role="tooltip" node), not
    // loose in the footer — that is where the drawer opener belongs.
    expect(popover().contains(changelogButton())).toBe(true);
  });

  it("mouse path: hover opens the panel and clicking Changelog fires the callback", () => {
    const onOpen = vi.fn();
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={onOpen} />);
    const host = popover().parentElement!;
    fireEvent.mouseEnter(host);
    expect(popover().getAttribute("data-open")).toBe("true");
    fireEvent.click(changelogButton());
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it("keyboard path: Tab from the version button reaches Changelog WITHOUT the panel closing, and activating it fires the callback", () => {
    const onOpen = vi.fn();
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={onOpen} />);
    // Focus the trigger — the host's focus-within opens the panel.
    fireEvent.focus(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
    // Tab moves focus from the trigger to the Changelog button. Before the fix, the
    // trigger's own onBlur fired first and closed the panel, so the button could
    // never take focus. With focus-within on the host, a blur whose relatedTarget is
    // still inside the host keeps it open.
    fireEvent.blur(trigger(), { relatedTarget: changelogButton() });
    expect(popover().getAttribute("data-open")).toBe("true");
    fireEvent.focus(changelogButton());
    expect(popover().getAttribute("data-open")).toBe("true");
    // Activating the (now focused) button opens the drawer.
    fireEvent.click(changelogButton());
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it("closes the panel only when focus leaves the whole host", () => {
    const onOpen = vi.fn();
    render(
      <>
        <button type="button">outside</button>
        <BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={onOpen} />
      </>,
    );
    fireEvent.focus(trigger());
    expect(popover().getAttribute("data-open")).toBe("true");
    // Focus moving to a control OUTSIDE the host closes it.
    const outside = screen.getByRole("button", { name: "outside" });
    fireEvent.blur(trigger(), { relatedTarget: outside });
    expect(popover().getAttribute("data-open")).toBe("false");
  });

  it("puts a hover bridge INSIDE the panel, so it is a host descendant", () => {
    // The mb-2 gap between the panel bottom and the trigger top is over neither the
    // host's in-flow button nor the out-of-flow panel, so a pointer crossing it fires
    // the host's onMouseLeave and the panel closes mid-transit. The bridge is a
    // transparent child of the panel that fills that gap: because it is a DESCENDANT
    // of the host, moving onto it produces no host mouseleave. jsdom can't measure the
    // pixel gap, so assert the mechanism — the bridge exists inside the panel.
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={vi.fn()} />);
    const bridge = popover().querySelector("[data-hover-bridge]");
    expect(bridge).not.toBeNull();
    expect(popover().contains(bridge)).toBe(true);
  });

  it("mouse transit onto the bridge keeps the panel open and Changelog reachable", () => {
    const onOpen = vi.fn();
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={onOpen} />);
    const host = popover().parentElement!;
    fireEvent.mouseEnter(host);
    expect(popover().getAttribute("data-open")).toBe("true");
    // Moving onto the bridge is a move onto a host descendant, so no host mouseLeave
    // is produced — the panel stays open and the Changelog button is reachable.
    const bridge = popover().querySelector("[data-hover-bridge]")!;
    fireEvent.mouseEnter(bridge);
    expect(popover().getAttribute("data-open")).toBe("true");
    expect(changelogButton()).toBeTruthy();
    fireEvent.click(changelogButton());
    expect(onOpen).toHaveBeenCalledTimes(1);
  });

  it("keeps the bridge inside the CLOSED panel, so it inherits pointer-events-none", () => {
    // On initial render the panel is closed and carries pointer-events-none opacity-0.
    // The bridge adds NO pointer-events class of its own, so a closed panel makes it
    // non-interactive too — it is not a hover target while the popover is shut.
    render(<BuildInfoPopover info={mockBuildInfo} now={NOW} onOpenChangelog={vi.fn()} />);
    const pop = popover();
    expect(pop.getAttribute("data-open")).toBe("false");
    expect(pop.className).toContain("pointer-events-none");
    expect(pop.className).toContain("opacity-0");
    const bridge = pop.querySelector("[data-hover-bridge]")!;
    expect(pop.contains(bridge)).toBe(true);
    // The inheritance only holds because the bridge sets NO pointer-events class of
    // its own — a future `pointer-events-auto` on it would make the shut popover a
    // hover target again, defeating the fix while this test's panel-level checks
    // stayed green. Guard the invariant directly.
    expect(bridge.className).not.toContain("pointer-events");
  });
});
