// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import {
  autoChipFor,
  autoStatusChip,
  formatAgo,
  formatCountdown,
  paceForecast,
  rowForecast,
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

  // The three states that mean "opted in, and the selector SKIPS it" warn. That is
  // R7's silent no-op made visible — the reason the status is surfaced at all.
  it("warns on every state where the selector skips the token", () => {
    for (const s of ["no_reading", "unmeasured", "stale"] as AutoStatus[]) {
      expect(autoStatusChip(s).tone).toBe("warning");
    }
  });

  // …and `below_threshold` deliberately does NOT (web-ux F4). It is the one state
  // that means the opposite in the case that matters: per D10, when every pooled
  // token is below the threshold the emptiest of them is STILL picked. Four states
  // sharing one amber said "not in play" about a token that is very much in play.
  it("does not warn on low headroom, which is still picked when the whole pool is low", () => {
    expect(autoStatusChip("below_threshold").tone).not.toBe("warning");
    // And it stays visually distinct from the calm "not opted in" state, so the row
    // does not read as though nothing were wrong.
    expect(autoStatusChip("below_threshold").tone).not.toBe(autoStatusChip("not_pooled").tone);
  });

  // autoChipFor is what the row actually renders, and its three client-only states
  // are the ones that used to degrade to the PRE-FEATURE appearance: no chip at all,
  // which is indistinguishable from a healthy pooled token (web-ux F1/F9).
  describe("autoChipFor", () => {
    it("shows nothing for an un-pooled token", () => {
      expect(autoChipFor(false, "not_pooled", "ready").kind).toBe("hidden");
    });

    it("says it is checking while the meters load, rather than showing nothing", () => {
      const d = autoChipFor(true, undefined, "pending");
      expect(d.kind).toBe("chip");
      expect(d.kind === "chip" && d.chip.label).toBe("checking…");
    });

    it("admits it does not know when the meters fetch fails", () => {
      const d = autoChipFor(true, undefined, "failed");
      expect(d.kind === "chip" && d.chip.label).toBe("eligibility unknown");
    });

    // A token the meters list does not mention is the same epistemic state as a
    // failed fetch: we have no answer for it, and saying nothing would look healthy.
    it("admits it does not know when the token has no meter row", () => {
      const d = autoChipFor(true, undefined, "ready");
      expect(d.kind === "chip" && d.chip.label).toBe("eligibility unknown");
    });

    // 🔴 The F1 contradiction. The toggle says pooled and the status says not pooled —
    // two independently-fetched sources disagreeing about one token, which a slow
    // /me/rate-limits after a fast /me/secrets reproduces against a real server.
    // Rendering it puts a checked box beside "not in pool".
    it("suppresses the chip when the toggle and the status contradict each other", () => {
      expect(autoChipFor(true, "not_pooled", "ready").kind).toBe("hidden");
    });

    it("shows the server's answer when the two agree", () => {
      const d = autoChipFor(true, "no_reading", "ready");
      expect(d.kind === "chip" && d.chip.label).toBe("never polled");
    });
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

describe("paceForecast (PRD #310 — anchored single-reading projection)", () => {
  const W5 = 18_000; // 5h window, seconds
  const RESET = 1_000_000; // an arbitrary anchored reset boundary, seconds

  // pace() builds a reading whose ELAPSED (nowSec − windowStart) is exactly `E`
  // seconds, then feeds paceForecast a MILLISECOND nowMs — so a stub that forgets to
  // divide nowMs by 1000 sees a wildly different elapsed and cannot pass these.
  function pace(pct: number, E: number, source?: Parameters<typeof paceForecast>[4], w = W5, reset = RESET) {
    const nowSec = reset - w + E; // ⇒ elapsed = nowSec − (reset − w) = E
    return paceForecast(pct, reset, w, nowSec * 1000, source);
  }

  it("bands strictly on the projection at 85/86/115/116 (D7)", () => {
    // elapsed = W5/2 ⇒ factor 2 ⇒ projected = 2·pct, exact at the boundaries.
    const half = W5 / 2;
    expect(pace(42.5, half).state).toBe("safe"); // projected 85 → safe (strict lower band)
    expect(pace(43, half)).toEqual({ state: "on_pace", projectedPct: 86 });
    expect(pace(57.5, half)).toEqual({ state: "on_pace", projectedPct: 115 });
    expect(pace(58, half)).toEqual({ state: "over", projectedPct: 116 });
  });

  it("projects the anchored math for a known (pct, elapsed, window)", () => {
    // pct 80 at 80% elapsed (14400/18000) ⇒ 80 × 18000 / 14400 = 100.
    expect(pace(80, 14_400)).toEqual({ state: "on_pace", projectedPct: 100 });
    // pct 50 at half-elapsed ⇒ 100.
    expect(pace(50, W5 / 2)).toEqual({ state: "on_pace", projectedPct: 100 });
  });

  it("divides a MILLISECOND nowMs by 1000 — a real-scale nowMs projects, not collapse to 0", () => {
    // The regression this PRD exists to fix: a Date.now()-scale nowMs (~1.78e12 ms)
    // fed raw into elapsed makes projected ≈ 0 (silent forever). Here elapsed comes
    // out 14400s (1h from reset on an 18000s window) ⇒ projected 100.
    const nowMs = Date.parse("2026-07-15T12:00:00Z"); // ~1.78e12 ms
    const reset = Math.floor(nowMs / 1000) + 3_600; // 1h out, SECONDS
    expect(paceForecast(80, reset, W5, nowMs)).toEqual({ state: "on_pace", projectedPct: 100 });
  });

  it("suppresses the early window at/below the floor, projects just above it", () => {
    // floor = max(18000/50, 900) = 900. elapsed 900 → safe; 901 → projects.
    expect(pace(50, 900).state).toBe("safe"); // at the floor → suppressed
    expect(pace(50, 899).state).toBe("safe"); // below the floor → suppressed
    expect(pace(50, 901).state).not.toBe("safe"); // just past the floor → projects
  });

  it("clamps an absurd early projection to MAX_PROJECTED_PCT (999)", () => {
    // pct 90 at elapsed 901 ⇒ 90 × 18000 / 901 ≈ 1798 → clamped to 999.
    expect(pace(90, 901)).toEqual({ state: "over", projectedPct: 999 });
  });

  it("is silent at/over the cap, on a limit_report source, and with a null reset", () => {
    expect(pace(100, W5 / 2).state).toBe("safe"); // already at the cap (would else project)
    expect(pace(58, W5 / 2, "limit_report").state).toBe("safe"); // park-time inference
    expect(paceForecast(58, null, W5, (RESET - W5 / 2) * 1000).state).toBe("safe"); // no horizon
  });

  it("is silent on a passed reset (elapsed exceeds the window ⇒ under-reads) and on a NaN nowMs", () => {
    // reset already behind now: elapsed 21000 > 18000, so 90 × 18000 / 21000 ≈ 77 → safe.
    // A stub that ignored elapsed and banded pct 90 alone would wrongly fire here.
    expect(pace(90, 21_000).state).toBe("safe");
    // A non-finite nowMs must not leak a NaN projection: !(elapsed > 0) rejects it.
    expect(paceForecast(58, RESET, W5, Number.NaN)).toEqual({ state: "safe", projectedPct: 0 });
    // Clock skew — reset far enough in the FUTURE that now precedes the window start
    // (elapsed ≤ 0) — is rejected the same way.
    expect(paceForecast(58, RESET, W5, (RESET - W5 - 100) * 1000).state).toBe("safe");
  });

  it("uses the 7-day window duration when handed it (the slow window now projects)", () => {
    const W7 = 604_800;
    // floor = max(604800/50, 900) = 12096. A 7d window at 99% one day out:
    // elapsed = 604800 − 86400 = 518400 ⇒ 99 × 604800 / 518400 ≈ 115.5 → over.
    const nowSec = RESET - W7 + (W7 - 86_400);
    expect(paceForecast(99, RESET, W7, nowSec * 1000).state).toBe("over");
  });
});

describe("rowForecast (PRD #310 — render-side stale short-circuit)", () => {
  const W5 = 18_000;
  const RESET = 1_000_000;
  const nowMs = (RESET - W5 / 2) * 1000; // elapsed = W5/2 ⇒ factor 2

  it("delegates to paceForecast for a live (non-stale) row", () => {
    // pct 58 at half-elapsed ⇒ projected 116 → over.
    expect(rowForecast(false, 58, RESET, W5, nowMs, "usage_endpoint").state).toBe("over");
  });

  it("is silent for a stale row even with a would-be-over reading (no forecast off a frozen bar)", () => {
    expect(rowForecast(true, 58, RESET, W5, nowMs, "usage_endpoint")).toEqual({ state: "safe", projectedPct: 0 });
  });
});
