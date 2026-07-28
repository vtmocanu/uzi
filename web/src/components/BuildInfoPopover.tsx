// The sidebar footer's version badge and its build-info popover (PRD #175,
// mockup Variant A). The footer used to be a `div` rendering one string; it is now
// a button that opens the full coordinate set on hover, focus AND tap.
//
// PRESENTATIONAL ON PURPOSE — it takes `info` as a prop and calls no hook that
// fetches. AppShell owns `useBuildInfo()` and the module-scope promise behind it.
// That split is what makes this testable: the promise is memoised at module scope
// with no reset seam, and vitest isolates per FILE, so a component reading it
// directly could not be exercised with two fixtures (fully-stamped and unstamped
// `dev`) in one test file — the second would reuse the first's already-resolved
// promise and its assertion would pass or fail for the wrong reason. Taking the
// data as a prop removes the hazard rather than working around it.

import { useId, useState } from "react";
import type { BuildInfo } from "../lib/api";
import { cx } from "./ui";

// displayVersion prefixes a "v" only for a numeric version, so "0.6.0" reads
// "v0.6.0" while "dev"/"demo" stay as-is and never become "vdev".
export function displayVersion(version: string): string {
  return /^\d/.test(version) ? `v${version}` : version;
}

// ageInDays is the project's age, computed HERE from `founded` rather than sent by
// the server (PRD Decision Log): one source of truth, and it stays correct between
// releases without a deploy. Day granularity, so the client-clock dependency is
// acceptable.
//
// Returns null for anything unparseable. `founded` is always present in a real
// response, but a hand-rolled test double or an older server is not a reason to
// render "NaN days old" in the shell.
export function ageInDays(founded: string | undefined, now: number = Date.now()): number | null {
  if (!founded) return null;
  // Date.parse of a bare YYYY-MM-DD is UTC midnight per ES2015+, which is what we
  // want: the founding date is a calendar fact, not a local-time instant.
  const t = Date.parse(founded);
  if (Number.isNaN(t)) return null;
  const days = Math.floor((now - t) / 86_400_000);
  return days < 0 ? null : days;
}

// formatDay renders an ISO date or RFC3339 timestamp as "3 Jul 2026". en-GB is
// pinned rather than left to the host locale so the string is deterministic in
// tests and identical for every operator reading the same instance.
export function formatDay(iso: string | undefined): string | null {
  if (!iso) return null;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return null;
  return new Intl.DateTimeFormat("en-GB", {
    day: "numeric",
    month: "short",
    year: "numeric",
    timeZone: "UTC",
  }).format(new Date(t));
}

// formatCount groups thousands ("2,105"). en-US pinned for the same determinism
// reason as formatDay.
export function formatCount(n: number): string {
  return n.toLocaleString("en-US");
}

// formatUptime renders a duration compactly: "3d 4h", "4h 12m", "12m", "48s".
// Two units at most — this is a glance value, not a stopwatch.
export function formatUptime(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86_400);
  const h = Math.floor((s % 86_400) / 3_600);
  const m = Math.floor((s % 3_600) / 60);
  if (d > 0) return h > 0 ? `${d}d ${h}h` : `${d}d`;
  if (h > 0) return m > 0 ? `${h}h ${m}m` : `${h}h`;
  if (m > 0) return `${m}m`;
  return `${s}s`;
}

// liveUptimeSeconds re-bases the server's reading against the wall clock. The
// build-info fetch is memoised for the life of the page, so a session left open
// for hours would otherwise keep reporting the uptime the API had at mount —
// "up 12s" long after the fact. Deriving the process start instant once and
// measuring from it keeps the popover honest as the session ages.
//
// The caveat, and it is the reason this is not perfect: an API restart is
// invisible until the page reloads, so this OVERSTATES after a restart where the
// raw value understates. Overstating is the same direction the number was already
// drifting, and both are bounded by the session's own length.
export function liveUptimeSeconds(
  uptimeSeconds: number,
  fetchedAtMs: number,
  now: number = Date.now(),
): number {
  return uptimeSeconds + Math.max(0, (now - fetchedAtMs) / 1000);
}

// A single definition-list row. Rendered only when the field is present: an
// un-stamped build must show a SHORTER popover, never a row reading "unknown",
// which is the same "claiming to know things it does not" the server's omit rule
// exists to prevent.
function Row({ label, value }: { label: string; value: string }) {
  return (
    <>
      <dt className="text-faint">{label}</dt>
      <dd className="text-right font-mono text-fg">{value}</dd>
    </>
  );
}

export function BuildInfoPopover({
  info,
  collapsed = false,
  // Injected only by tests, so the age/uptime assertions are not hostage to the
  // wall clock. Production always reads the real clock.
  now,
  fetchedAtMs,
}: {
  info: BuildInfo;
  collapsed?: boolean;
  now?: number;
  fetchedAtMs?: number;
}) {
  const [open, setOpen] = useState(false);
  // Instance-scoped: TWO SidebarContent mounts exist simultaneously (the desktop
  // aside and the mobile drawer), so a hardcoded id would put a duplicate in the
  // document and make aria-describedby ambiguous. Not a lint nit — it is the one
  // structural fact about this component's environment.
  const popId = useId();

  const label = displayVersion(info.version);
  const days = ageInDays(info.founded, now);
  const built = formatDay(info.built_at);
  const founded = formatDay(info.founded);
  const uptime =
    info.uptime_seconds === undefined
      ? null
      : formatUptime(
          fetchedAtMs === undefined
            ? info.uptime_seconds
            : liveUptimeSeconds(info.uptime_seconds, fetchedAtMs, now),
        );

  // The subtitle line: age, then the commit count when the build carries one. M3
  // is independently droppable, so "24 days old" alone is a supported final state,
  // not a loading intermediate.
  const subParts: string[] = [];
  if (days !== null) subParts.push(days === 1 ? "1 day old" : `${days} days old`);
  if (info.commits !== undefined) {
    subParts.push(info.commits === 1 ? "1 commit" : `${formatCount(info.commits)} commits`);
  }

  return (
    <div
      className="relative border-t border-edge"
      // Hover on the HOST, not the button, so the popover does not vanish the
      // instant the pointer crosses from the trigger onto the panel above it.
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
    >
      <button
        type="button"
        // aria-describedby, not aria-label: the button's own name is the version
        // string, and the popover DESCRIBES it. The popover node stays mounted (it
        // is faded, not unmounted) so the description is in the accessibility tree
        // when focus lands, rather than appearing one render later.
        //
        // No aria-expanded, deliberately: the APG tooltip pattern associates by
        // aria-describedby alone, and announcing "collapsed"/"expanded" for
        // something a screen reader can already read in full would be noise.
        aria-describedby={popId}
        // Focus opens it, which is what makes this keyboard-reachable at all. A tap
        // opens it through onClick — which always OPENS rather than toggling, so a
        // desktop click landing on an already-hovered badge cannot close it under
        // the pointer.
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onClick={() => setOpen(true)}
        onKeyDown={(e) => {
          if (e.key === "Escape") setOpen(false);
        }}
        className={cx(
          "block w-full truncate text-left font-mono text-faint transition-colors",
          "hover:bg-raised hover:text-fg focus-visible:text-fg",
          collapsed ? "px-1 py-1.5 text-center text-[9px]" : "px-3 py-1.5 text-[10px]",
        )}
      >
        {label}
      </button>
      <div
        id={popId}
        role="tooltip"
        // The visual state is a data attribute rather than an ARIA one because it
        // IS purely visual — see the aria-expanded note above. Tests read it so
        // they assert on the state itself instead of on opacity utility classes.
        data-open={open ? "true" : "false"}
        className={cx(
          // Anchored ABOVE the trigger and left-aligned to the rail, because the
          // trigger is the last row of a sidebar pinned to the viewport bottom —
          // there is no space below it. Nothing above the footer sets
          // overflow-hidden, so this is not clipped.
          //
          // z-10 is local, not global: this sits inside the desktop aside (z-30)
          // and inside the mobile drawer (z-40), so it only has to beat its own
          // siblings. Raising it to compete with the mobile overlay would be
          // wrong — in the drawer it rides ABOVE that overlay already, and on
          // desktop the overlay is lg:hidden and cannot coexist.
          "absolute bottom-full z-10 mb-2 w-[226px] rounded-lg border border-edge bg-raised p-3 shadow-2xl",
          // Collapsed the rail is 56px wide, so a left-anchored popover would hang
          // over the content; nudge it clear of the rail's right edge instead.
          collapsed ? "left-1" : "left-2",
          "transition-opacity duration-150 motion-reduce:transition-none",
          open ? "opacity-100" : "pointer-events-none opacity-0",
        )}
      >
        <div className="font-mono text-xs font-semibold text-fg">uzi {label}</div>
        {subParts.length > 0 && (
          <div className="mb-2 border-b border-edge pb-2 text-[11px] text-faint">
            {subParts.join(" · ")}
          </div>
        )}
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-[11px]">
          {founded && <Row label="Founded" value={founded} />}
          {built && <Row label="Built" value={built} />}
          {info.commit && <Row label="Commit" value={info.commit.slice(0, 7)} />}
          {uptime && <Row label="Uptime" value={uptime} />}
        </dl>
      </div>
    </div>
  );
}
