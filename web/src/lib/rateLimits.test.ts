// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import {
  autoStatusChip,
  formatAgo,
  formatCountdown,
  rowState,
  sortAdminRows,
  statusBadge,
  worstWindow,
  type RowState,
} from "./rateLimits";
import type { AdminRateLimitUser, AutoStatus, MyRateLimits } from "./api";
// ?raw rather than node:fs — the web tsconfig carries no node types, and the same
// choice is made for the same reason in workerSizes.test.ts and
// WorkerUpgradeBadge.test.tsx.
import rateLimitsSource from "./rateLimits.ts?raw";

const NOW = Date.parse("2026-07-15T12:00:00Z");
const NOW_SECS = Math.floor(NOW / 1000);

function ok(pct5: number, pct7: number, over: Partial<Extract<MyRateLimits, { status: "ok" }>> = {}): MyRateLimits {
  return {
    status: "ok",
    five_hour: { pct: pct5, resets_at: NOW_SECS + 5000 },
    seven_day: { pct: pct7, resets_at: NOW_SECS + 200_000 },
    source: "usage_endpoint",
    synced_at: new Date(NOW - 2 * 60_000).toISOString(),
    stale: false,
    ...over,
  };
}

// A user row carries ONE READING PER TOKEN since PRD #104 M5; these sort tests are
// about a user's single credential, so row() wraps it as their default. A
// "no_token" reading becomes the EMPTY list the API actually sends.
function row(id: string, name: string, limits: MyRateLimits, vault_locked = false): AdminRateLimitUser {
  return {
    id,
    name,
    email: `${name}@x`,
    vault_locked,
    tokens:
      limits.status === "no_token"
        ? []
        : [
            {
              secret_id: `sec-${id}`,
              label: "default",
              is_default: true,
              // PRD #111 M2 rides every token row; these fixtures are about the
              // rate-limit classification, so they stay un-pooled.
              auto_eligible: false,
              auto_status: "not_pooled" as const,
              limits,
            },
          ],
  };
}

describe("formatCountdown (Decision 7)", () => {
  it("renders days/hours/minutes and the null / past edges", () => {
    expect(formatCountdown(null, NOW)).toBeNull();
    expect(formatCountdown(NOW_SECS + 2 * 86_400 + 4 * 3600, NOW)).toBe("2d 4h");
    expect(formatCountdown(NOW_SECS + 3600 + 23 * 60, NOW)).toBe("1h 23m");
    expect(formatCountdown(NOW_SECS + 44 * 60, NOW)).toBe("44m");
    expect(formatCountdown(NOW_SECS + 30, NOW)).toBe("<1m");
    expect(formatCountdown(NOW_SECS - 10, NOW)).toBe("now");
  });
});

describe("formatAgo", () => {
  it("renders compact relative time and tolerates a bad timestamp", () => {
    expect(formatAgo(new Date(NOW - 30_000).toISOString(), NOW)).toBe("30s ago");
    expect(formatAgo(new Date(NOW - 2 * 60_000).toISOString(), NOW)).toBe("2m ago");
    expect(formatAgo(new Date(NOW - 3 * 3600_000).toISOString(), NOW)).toBe("3h ago");
    expect(formatAgo(new Date(NOW - 2 * 86_400_000).toISOString(), NOW)).toBe("2d ago");
    expect(formatAgo("not-a-date", NOW)).toBe("just now");
  });
});

describe("rowState — the five row states", () => {
  const cases: [string, MyRateLimits, boolean, RowState][] = [
    ["no token", { status: "no_token" }, false, "no_token"],
    ["unavailable", { status: "unavailable" }, false, "unavailable"],
    ["stale reading", ok(31, 12, { stale: true }), true, "stale"],
    ["live, both low", ok(8, 27), false, "live_ok"],
    ["live, one window in warn band", ok(62, 83), false, "live_warn"],
    ["live, one window in danger band", ok(97, 71), false, "live_danger"],
  ];
  it.each(cases)("classifies %s", (_label, limits, _locked, expected) => {
    expect(rowState(limits)).toBe(expected);
  });
});

describe("statusBadge", () => {
  it("maps each state to its pill", () => {
    expect(statusBadge({ status: "no_token" }, false)).toMatchObject({ tone: "neutral", label: "no token" });
    expect(statusBadge({ status: "unavailable" }, false)).toMatchObject({ tone: "neutral", label: "no reading yet" });
    expect(statusBadge(ok(31, 12, { stale: true }), true)).toMatchObject({ tone: "neutral", label: "🔒 vault locked" });
    expect(statusBadge(ok(31, 12, { stale: true }), false)).toMatchObject({ tone: "neutral", label: "stale" });
    expect(statusBadge(ok(8, 27), false)).toMatchObject({ tone: "ok", label: "Live", dot: true });
    // A warn window stays "Live" (the bar carries the amber), not an escalated pill.
    expect(statusBadge(ok(62, 83), false)).toMatchObject({ tone: "ok", label: "Live" });
    // An 85–94 danger-band window paints a red bar (danger tone) but the pill stays
    // a green "Live" — the badge stays decoupled at ≥95, and no window here is ≥95.
    expect(statusBadge(ok(88, 76), false)).toMatchObject({ tone: "ok", label: "Live" });
    expect(statusBadge(ok(97, 71), false)).toMatchObject({ tone: "danger", label: "5h nearly out" });
    expect(statusBadge(ok(71, 97), false)).toMatchObject({ tone: "danger", label: "7d nearly out" });
    expect(statusBadge(ok(96, 99), false)).toMatchObject({ tone: "danger", label: "5h & 7d nearly out" });
  });
});

describe("worstWindow (PRD #54)", () => {
  const okOnly = (limits: MyRateLimits) => limits as Extract<MyRateLimits, { status: "ok" }>;

  it("names the 5-hour window when it is the more utilized", () => {
    expect(worstWindow(okOnly(ok(97, 71)))).toMatchObject({ label: "5-hour", pct: 97 });
  });

  it("names the 7-day window when it is the more utilized", () => {
    expect(worstWindow(okOnly(ok(71, 97)))).toMatchObject({ label: "7-day", pct: 97 });
  });

  it("breaks a tie toward the 5-hour window (shorter, more urgent)", () => {
    const w = worstWindow(okOnly(ok(88, 88)));
    expect(w.label).toBe("5-hour");
    expect(w.pct).toBe(88);
  });

  it("carries the winning window's resets_at", () => {
    expect(worstWindow(okOnly(ok(20, 90))).resets_at).toBe(NOW_SECS + 200_000);
  });
});

describe("sortAdminRows", () => {
  it("orders danger → warn → ok → stale → unavailable → no_token", () => {
    const users = [
      row("6", "irina", { status: "no_token" }),
      row("3", "vlad", ok(8, 27)),
      row("1", "ana", ok(97, 71)),
      row("5", "dana", { status: "unavailable" }),
      row("2", "radu", ok(62, 83)),
      row("4", "mihai", ok(31, 12, { stale: true }), true),
    ];
    expect(sortAdminRows(users).map((u) => u.name)).toEqual([
      "ana",
      "radu",
      "vlad",
      "mihai",
      "dana",
      "irina",
    ]);
  });

  it("tie-breaks two live-ok rows by 5h% desc", () => {
    const users = [row("a", "low", ok(10, 20)), row("b", "high", ok(35, 5))];
    expect(sortAdminRows(users).map((u) => u.name)).toEqual(["high", "low"]);
  });
});

// ── autoStatusChip (PRD #111 M2, D21) ────────────────────────────────────────
//
// The point of these is NOT that the labels are pretty. It is that this module
// maps a server-supplied string and derives nothing: the eligibility gate has one
// implementation, in Go, and a second one here would drift into telling a user a
// token is eligible while the selector skips it.
describe("autoStatusChip", () => {
  it("has a distinct rendering for every status the server can send", () => {
    const statuses: AutoStatus[] = [
      "eligible",
      "not_pooled",
      "no_reading",
      "unmeasured",
      "stale",
      "below_threshold",
    ];
    const labels = statuses.map((s) => autoStatusChip(s).label);
    // Distinct, because two statuses sharing a label makes them indistinguishable
    // to the user they are shown to — which is the whole reason they are separate
    // statuses rather than one "not eligible".
    expect(new Set(labels).size).toBe(statuses.length);
    for (const s of statuses) {
      expect(autoStatusChip(s).hint.length).toBeGreaterThan(0);
    }
  });

  it("greens only 'eligible' and stays calm for 'not_pooled'", () => {
    expect(autoStatusChip("eligible").tone).toBe("ok");
    // not_pooled is a setting, not a problem: a user who has not opted a token in
    // must not be shown a warning about it.
    expect(autoStatusChip("not_pooled").tone).toBe("neutral");
  });

  // The three states that mean "opted in, and it will never be picked as things
  // stand" all warn. That is R7's silent no-op made visible — the reason the
  // status is surfaced at all.
  it("warns on every opted-in-but-unpickable state", () => {
    for (const s of ["no_reading", "unmeasured", "stale", "below_threshold"] as AutoStatus[]) {
      expect(autoStatusChip(s).tone).toBe("warning");
    }
  });

  // The server deploys separately, so a newer API can send a status this bundle
  // has never heard of. Guessing a rendering for it would be exactly the lie the
  // server-side classification exists to prevent.
  it("reports an unrecognised status honestly instead of guessing", () => {
    const chip = autoStatusChip("something_new_from_a_newer_api");
    expect(chip.label).toBe("unknown");
    expect(chip.hint).toContain("something_new_from_a_newer_api");
  });

  // The guard rail for the whole design: this module must contain no second
  // implementation of the gate. A `100 -` or a synced_at comparison here IS that
  // second implementation, and nothing else would fail when the two disagreed.
  it("derives nothing — no headroom arithmetic, no staleness comparison", () => {
    // Strip comments first: that module's own prose explains the rule by NAMING the
    // forbidden shapes, so matching them uncommented would fail the test it documents.
    const code = rateLimitsSource.replace(/\/\/.*$/gm, "").replace(/\/\*[\s\S]*?\*\//g, "");
    expect(code).not.toMatch(/100\s*-/);
    expect(code).not.toMatch(/synced_at/);
    expect(code).not.toMatch(/auto_eligible/);
  });
});
