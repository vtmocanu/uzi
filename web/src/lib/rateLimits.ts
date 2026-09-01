// Pure helpers for the Claude rate-limit meters (PRD #53): countdown/relative-time
// formatting (Decision 7 — resets stored as epochs, rendered client-side), the row
// classifier the three surfaces + the admin sort share, and a small clock hook so
// countdowns tick between the 60s polls. Kept free of the api client so it unit-
// tests without the mock; types are imported type-only (erased at build).

import { toneFor } from "../components/Meter";
import { useNow as useNowClock } from "./useNow";
import type { AdminRateLimitUser, AutoStatus, MyRateLimits, RateLimitSource, TokenRateLimits } from "./api";
import type { BadgeTone } from "../components/ui";

// formatCountdown renders "resets in <this>" (Decision 7): "2d 4h", "1h 23m",
// "44m", or "<1m". null resets_at → null (no countdown). A reset already in the
// past reads "now" (the window is refreshing). nowMs is injectable for tests.
export function formatCountdown(resetsAt: number | null, nowMs = Date.now()): string | null {
  if (resetsAt == null) return null;
  const total = Math.floor(resetsAt - nowMs / 1000);
  if (total <= 0) return "now";
  const d = Math.floor(total / 86_400);
  const h = Math.floor((total % 86_400) / 3_600);
  const m = Math.floor((total % 3_600) / 60);
  if (d >= 1) return `${d}d ${h}h`;
  if (h >= 1) return `${h}h ${m}m`;
  if (m >= 1) return `${m}m`;
  return "<1m";
}

// formatResetLabel renders a 7-day window's absolute reset as "resets <Day HH:MM>"
// in the VIEWER'S LOCAL timezone (weekday included so it is unambiguous), the mock's
// line under each token name (PRD #310 M3 / D4 — the weekly quota users plan around;
// the per-window relative countdowns stay in the utilization column). null / non-
// finite resets_at → null (the caller omits the line). resetsAt is a SECONDS epoch,
// like resets_at everywhere else in this module. Built via Intl.DateTimeFormat +
// formatToParts (not getHours) so the weekday abbrev and 24h HH:MM come out locale/
// tz-correct and we compose exactly "resets <Weekday> <HH:MM>" with no locale
// separator. Absolute, so it takes no nowMs — unlike its formatCountdown sibling.
export function formatResetLabel(resetsAt: number | null): string | null {
  if (resetsAt == null || !Number.isFinite(resetsAt)) return null;
  const parts = new Intl.DateTimeFormat(undefined, {
    weekday: "short",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    hourCycle: "h23", // midnight is "00:00", never "24:00" (some ICU builds render h24 otherwise)
  }).formatToParts(new Date(resetsAt * 1000));
  const at = (type: Intl.DateTimeFormatPartTypes) => parts.find((p) => p.type === type)?.value ?? "";
  return `resets ${at("weekday")} ${at("hour")}:${at("minute")}`;
}

// formatAgo renders an ISO timestamp as "2m ago" / "3h ago" / "1d ago" — the
// "updated N ago" line under the meters. nowMs is injectable for tests.
export function formatAgo(iso: string, nowMs = Date.now()): string {
  const ms = nowMs - Date.parse(iso);
  if (!Number.isFinite(ms)) return "just now";
  const s = Math.max(0, Math.floor(ms / 1000));
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

// RowState is the single classification the meters, badges, and admin sort agree
// on. warn/danger split so the admin table can label a danger row "N nearly out"
// while a warn row stays "Live" (the mockup's radu at 83% is still Live).
export type RowState =
  | "live_ok"
  | "live_warn"
  | "live_danger"
  | "stale"
  | "unavailable"
  | "no_token";

// worstPct is the higher of the two windows — a user is as worth flagging at 97%
// on 5h as at 97% on 7d.
function worstPct(limits: Extract<MyRateLimits, { status: "ok" }>): number {
  return Math.max(limits.five_hour.pct, limits.seven_day.pct);
}

export interface WorstWindow {
  label: "5-hour" | "7-day";
  pct: number;
  resets_at: number | null;
}

// worstWindow names the more-utilized of the two windows (not just its pct — that
// is worstPct) so the aria-live announcer (PRD #54) can say WHICH window drove a
// tone crossing and read its countdown. Tie → 5-hour, the shorter and more urgent
// window.
export function worstWindow(limits: Extract<MyRateLimits, { status: "ok" }>): WorstWindow {
  const { five_hour: five, seven_day: seven } = limits;
  return seven.pct > five.pct
    ? { label: "7-day", pct: seven.pct, resets_at: seven.resets_at }
    : { label: "5-hour", pct: five.pct, resets_at: five.resets_at };
}

export function rowState(limits: MyRateLimits): RowState {
  if (limits.status === "no_token") return "no_token";
  if (limits.status === "unavailable") return "unavailable";
  if (limits.stale) return "stale";
  const tone = toneFor(worstPct(limits));
  return tone === "danger" ? "live_danger" : tone === "warn" ? "live_warn" : "live_ok";
}

// worstTokenState reduces a user's N token meters to the single most urgent state
// (PRD #104 M5), which is what the admin table sorts and badges a USER by: a user
// with one exhausted credential and two healthy ones is still worth surfacing. An
// EMPTY token list is "no_token" — since M5 that is how the API says a user holds
// no credential at all (there is no per-token status to report when there is none).
function worstTokenState(tokens: TokenRateLimits[]): RowState {
  if (tokens.length === 0) return "no_token";
  return tokens.map((t) => rowState(t.limits)).reduce((a, b) => (RANK[a] <= RANK[b] ? a : b));
}

// worstTokenReading returns the token whose reading is most urgent, so a caller
// that needs a representative reading (the admin 5h% tie-break, the announcer)
// picks the same one the state came from. undefined when the user holds none.
export function worstTokenReading(tokens: TokenRateLimits[]): TokenRateLimits | undefined {
  if (tokens.length === 0) return undefined;
  return [...tokens].sort((a, b) => {
    const ra = RANK[rowState(a.limits)];
    const rb = RANK[rowState(b.limits)];
    if (ra !== rb) return ra - rb;
    return fiveHourPct(b.limits) - fiveHourPct(a.limits);
  })[0];
}

// Sort rank: the most urgent rows rise (Decision: admin sorts danger, then warn,
// then by 5h% desc). Non-actionable states (stale / unavailable / no_token) sink
// below every live reading so the capacity view leads with who is near a wall.
const RANK: Record<RowState, number> = {
  live_danger: 0,
  live_warn: 1,
  live_ok: 2,
  stale: 3,
  unavailable: 4,
  no_token: 5,
};

function fiveHourPct(limits: MyRateLimits): number {
  return limits.status === "ok" ? limits.five_hour.pct : -1;
}

// sortAdminRows returns a new array ordered danger → warn → ok → stale →
// unavailable → no_token, tie-broken by 5h% desc then name. Since PRD #104 a user
// holds N tokens, so a USER is ranked by their most urgent one (worstTokenState):
// the capacity view must still lead with whoever is nearest a wall, and one
// exhausted credential among three is exactly that.
export function sortAdminRows(users: AdminRateLimitUser[]): AdminRateLimitUser[] {
  return [...users].sort((a, b) => {
    const ra = RANK[worstTokenState(a.tokens)];
    const rb = RANK[worstTokenState(b.tokens)];
    if (ra !== rb) return ra - rb;
    const pa = fiveHourPct(worstTokenReading(a.tokens)?.limits ?? { status: "unavailable" });
    const pb = fiveHourPct(worstTokenReading(b.tokens)?.limits ?? { status: "unavailable" });
    if (pa !== pb) return pb - pa;
    return a.name.localeCompare(b.name);
  });
}

export interface StatusBadge {
  tone: BadgeTone;
  label: string;
  dot: boolean;
}

// statusBadge maps a row to its status pill. A live reading is a green "Live";
// only a ≥95% window escalates the pill to "5h nearly out" / "7d nearly out" /
// "5h & 7d nearly out" (a warn window keeps "Live" — the bar carries the amber).
// The pill stays decoupled at ≥95 even though the danger TONE now steps at 85:
// an 85–94 danger-band row paints a red bar (via the danger tone) but keeps a
// green "Live" pill because no window has crossed 95 (Decision 3). A stale row is
// a neutral pill: "vault locked" when the vault is the cause (Decision 3), else
// "stale". vaultLocked has no bearing on a live row.
export function statusBadge(limits: MyRateLimits, vaultLocked: boolean): StatusBadge {
  switch (rowState(limits)) {
    case "no_token":
      return { tone: "neutral", label: "no token", dot: false };
    case "unavailable":
      return { tone: "neutral", label: "no reading yet", dot: false };
    case "stale":
      return { tone: "neutral", label: vaultLocked ? "🔒 vault locked" : "stale", dot: false };
    case "live_ok":
    case "live_warn":
      return { tone: "ok", label: "Live", dot: true };
    case "live_danger": {
      const ok = limits as Extract<MyRateLimits, { status: "ok" }>;
      const which = [
        ok.five_hour.pct >= 95 ? "5h" : null,
        ok.seven_day.pct >= 95 ? "7d" : null,
      ].filter(Boolean);
      if (which.length === 0) return { tone: "ok", label: "Live", dot: true };
      return { tone: "danger", label: `${which.join(" & ")} nearly out`, dot: true };
    }
  }
}

// ── Auto-selection eligibility (PRD #111 M2) ─────────────────────────────────
//
// 🔴 A MAP, NOT A COMPUTATION, AND THAT IS THE WHOLE DESIGN (D21). The server ships
// the answer as a string because the eligibility gate has exactly ONE
// implementation — autoselect.Classify in Go — which the ranker and this page both
// consume. Anything here that reads `limits.five_hour.pct`, subtracts from 100, or
// compares a synced_at is a second implementation of that gate, and the two would
// drift into the worst possible failure: this card telling a user a token is
// eligible while the selector silently skips it, with nothing failing anywhere.
//
// So the only thing below is label and colour.

export interface AutoStatusChip {
  tone: BadgeTone;
  label: string;
  /** The one-line "why", for the chip's title/tooltip. */
  hint: string;
}

// AUTO_STATUS_CHIPS is EXHAUSTIVE over AutoStatus by type, so adding a status
// server-side without a rendering here fails typecheck rather than painting a blank
// chip. `not_pooled` is deliberately included and deliberately not rendered by the
// settings row (the toggle beside it already says so) — but it still needs an entry,
// because the admin view has no toggle to say it.
const AUTO_STATUS_CHIPS: Record<AutoStatus, AutoStatusChip> = {
  eligible: {
    tone: "ok",
    label: "in pool",
    hint: "Auto-selection can pick this token: it is opted in, its usage reading is current, and it has headroom.",
  },
  not_pooled: {
    tone: "neutral",
    label: "not in pool",
    hint: "Auto-selection never picks this token. Opt it in to let an auto worker spend it.",
    // Required by the exhaustive Record above, and NOT because any surface renders
    // it today: the settings row suppresses this state (the unchecked box beside it
    // already says so) and the admin view does not read auto_status at all. An
    // earlier version of this comment claimed the admin view needed it, which read
    // as though that page rendered the chip — it does not. Widening the admin
    // surface is PRD #104 M5's, not this PRD's.
  },
  no_reading: {
    tone: "warning",
    label: "never polled",
    hint: "Opted in, but uzi has never read a usage figure for this token, so auto-selection cannot rank it — it will never be picked. A credential the usage endpoint refuses stays in this state.",
  },
  unmeasured: {
    tone: "warning",
    label: "no usage data",
    hint: "Opted in and polled, but the reading carried no percentage for at least one window, so there is no headroom to rank on.",
  },
  stale: {
    tone: "warning",
    label: "stale reading",
    hint: "Opted in, but the last usage reading is too old to steer a choice — auto-selection skips it until a fresh one lands. A token uzi is currently backing off from also reads this way.",
  },
  below_threshold: {
    // NOT `warning`, deliberately, and the difference is not cosmetic. The three
    // states above mean the selector SKIPS this token. This one means the opposite in
    // the case that matters: per D10, when every pooled token is below the threshold
    // the emptiest of them is still picked — "least consumed" has a best answer even
    // when every answer is poor. Wearing the same amber as `stale` would say "not in
    // play" about a token that is very much in play.
    tone: "info",
    label: "low headroom",
    hint: "Opted in and current, but below the headroom threshold, so auto-selection prefers other tokens. It is still picked if every pooled token is this low.",
  },
};

// AutoChipState is what the settings row actually renders, which is NOT the same as
// the server's status (PRD #111 M2, web-ux F1/F9). Three states exist only on the
// client and none of them is a server answer:
//
//   - "pending": the meters fetch has not resolved. Before this existed, a pooled
//     token rendered NO chip while loading — visually identical to a healthy one,
//     which is the silent no-op D11 exists to prevent arriving through a spinner.
//   - "unknown": the meters fetch FAILED, or returned no row for this token. Same
//     hazard through the error path, and the honest answer is "we could not check".
//   - "contradiction": the toggle says pooled and the status says not_pooled. Two
//     independently-fetched sources disagreeing about one token — a slow
//     /me/rate-limits after a fast /me/secrets reproduces it against a real server.
//     Rendering it would show a checked box beside "not in pool"; we show nothing
//     rather than assert a contradiction, and it resolves on the next poll.
export type AutoChipState =
  | { kind: "hidden" }
  | { kind: "chip"; chip: AutoStatusChip };

// autoChipFor decides what a POOLED token's row shows. Callers gate on
// secret.auto_eligible before calling: an un-pooled token shows nothing at all,
// because the unchecked box beside it already says so.
export function autoChipFor(
  pooled: boolean,
  status: AutoStatus | string | undefined,
  fetchState: "pending" | "ready" | "failed",
): AutoChipState {
  if (!pooled) return { kind: "hidden" };
  if (fetchState === "pending") {
    return {
      kind: "chip",
      chip: {
        tone: "neutral",
        label: "checking…",
        hint: "Checking whether auto-selection can currently pick this token.",
      },
    };
  }
  if (fetchState === "failed" || status === undefined) {
    return {
      kind: "chip",
      chip: {
        tone: "neutral",
        label: "eligibility unknown",
        hint: "uzi could not read this token's usage figures just now, so whether auto-selection can pick it is unknown. It is still opted in.",
      },
    };
  }
  // The contradiction. Suppressed rather than rendered — see AutoChipState.
  if (status === "not_pooled") return { kind: "hidden" };
  return { kind: "chip", chip: autoStatusChip(status) };
}

// autoStatusChip renders the server's answer. It takes the string the API sent and
// nothing else — no reading, no clock — which is what makes "the UI cannot disagree
// with the selector" a property rather than a habit.
//
// An UNRECOGNISED value renders as an honest unknown rather than being dropped or
// guessed at: the server is deployed separately, so a newer API can ship a status
// this bundle has never heard of, and inventing a rendering for it would be the
// exact lie this whole mechanism exists to prevent.
export function autoStatusChip(status: AutoStatus | string): AutoStatusChip {
  return (
    AUTO_STATUS_CHIPS[status as AutoStatus] ?? {
      tone: "neutral",
      label: "unknown",
      hint: `This uzi build does not recognise the eligibility state “${status}”. It is reported as-is rather than guessed at.`,
    }
  );
}

// ── Anchored pace forecast (PRD #310) ────────────────────────────────────────
//
// Is a window on track to exhaust BEFORE it resets? The unified 5h/7d windows are
// ANCHORED — a fixed boundary that fully resets — measured 2026-08-13 against the
// live `anthropic_rate_limits` headers (see specs/ai.md §523). That makes the
// projection valid from a SINGLE reading (no in-session sample series), so the
// forecast is ALWAYS visible, on idle and slow-moving 7-day windows alike:
//
//   nowSec    = nowMs / 1000                                   // nowMs is MILLISECONDS
//   elapsed   = nowSec − (resetsAtSec − windowDurationSec)     // resets/duration in SECONDS
//   projected = pct × windowDurationSec / elapsed
//
// Feeding raw nowMs (not nowMs/1000) into elapsed collapses projected to ~0 and the
// forecast is silent forever — the exact failure #310 exists to fix. formatCountdown
// divides nowMs by 1000 the same way (rateLimits.ts:17).
//
// DISPLAY-ONLY (PRD #309 D2, #310 D6): this drives no backend decision and must
// NEVER become an input to auto-selection or any gate — it is imported only by
// rendering code.

// The anchored window lengths in SECONDS (PRD #310 D1): 5h = 18000 (confirmed by the
// measured +5h reset jump), 7d = 604800. The surfaces index this by the literal
// window key to feed rowForecast.
export const WINDOW_DURATION = { "5h": 18000, "7d": 604800 } as const;

// Not exported: consumers compare `forecast.state === "over"` structurally; the
// wrapper needs no named import. Keeping it local avoids an orphaned export.
type PaceState = "over" | "on_pace" | "safe";

export interface PaceForecast {
  // over: projected past the cap before reset; on_pace: lands ~at the cap; safe:
  // headroom to spare OR suppressed (silence = safe, PRD #309 D3).
  state: PaceState;
  // The projected utilization AT reset, rounded and clamped to a sane display range.
  // Meaningful only when state !== "safe"; 0 on the safe/silent path.
  projectedPct: number;
}

// Cap the projected % to a sane display range: a tiny elapsed early in a window
// extrapolates to absurd magnitudes ("projected 1600%"), which the bands already
// read as "over" — the number past the cap is a rough hint, not a precise promise.
// The ghost width is clamped to 100 separately by the wrapper.
const MAX_PROJECTED_PCT = 999;

// paceForecast projects a single window's landing utilization from ONE reading via
// the anchored formula above. PURE and wall-clock-free — nowMs is injected. Returns
// a silent "safe" whenever the projection would be meaningless or misleading:
//   - pct already ≥ 100, or a `limit_report` source — AT the cap / a park-time
//     inference, not "on track to be" (PRD #309 D8);
//   - resets_at null — no horizon to anchor to;
//   - !(elapsed > 0) — a passed reset, clock skew, or a NaN nowMs (the `!(x > 0)`
//     form rejects the non-finite case a bare `<=` would not);
//   - elapsed ≤ floor = max(windowDurationSec/50, 900) — early-window suppression (a
//     tiny elapsed extrapolates wildly), checked BEFORE the divide so it doubles as
//     the divide-by-zero guard: floor is always ≥ 900, so the divide only runs at
//     elapsed > 900 (PRD #310 D3);
//   - projected ≤ 85 — headroom to spare (strict lower band, PRD #309 D7).
// Otherwise banded strictly on the RAW projection (PRD #309 D7): > 115 → over, else
// on_pace. The returned projectedPct is the CLAMPED, rounded value for display.
export function paceForecast(
  pct: number,
  resetsAtSec: number | null,
  windowDurationSec: number,
  nowMs: number,
  source?: RateLimitSource,
): PaceForecast {
  const SAFE: PaceForecast = { state: "safe", projectedPct: 0 };
  if (pct >= 100) return SAFE; // D8: already at the cap, not heading there
  if (source === "limit_report") return SAFE; // D8: park-time inference
  if (resetsAtSec == null) return SAFE;
  const nowSec = nowMs / 1000; // nowMs is MILLISECONDS; resets/duration are SECONDS
  const elapsed = nowSec - (resetsAtSec - windowDurationSec);
  // `!(x > 0)` rather than `x <= 0` so a non-finite elapsed (NaN from a bad nowMs)
  // is also rejected, never propagating to a NaN projectedPct.
  if (!(elapsed > 0)) return SAFE; // passed reset / clock skew / non-finite
  // Floor BEFORE the divide: always ≥ 900, so the divide only runs at elapsed > 900.
  const floor = Math.max(windowDurationSec / 50, 900);
  if (elapsed <= floor) return SAFE; // early-window suppression (PRD #310 D3)
  const projected = (pct * windowDurationSec) / elapsed;
  const projectedPct = Math.round(Math.max(0, Math.min(MAX_PROJECTED_PCT, projected)));
  if (projected <= 85) return SAFE; // headroom to spare (strict lower band, D7)
  return { state: projected > 115 ? "over" : "on_pace", projectedPct };
}

// rowForecast is paceForecast with the stale short-circuit the three surfaces share.
// A stale reading draws NO forecast (projecting off a frozen number is misleading —
// PRD #309 D8 kin). Centralised so a new surface can't forget it: the three surfaces
// call ONLY rowForecast, never paceForecast directly.
export function rowForecast(
  stale: boolean,
  pct: number,
  resetsAtSec: number | null,
  windowDurationSec: number,
  nowMs: number,
  source?: RateLimitSource,
): PaceForecast {
  if (stale) return { state: "safe", projectedPct: 0 };
  return paceForecast(pct, resetsAtSec, windowDurationSec, nowMs, source);
}

// useNow ticks a Date.now() clock so a rendered countdown re-derives between the
// 60s polls (Decision 7). Default 30s: fine-grained enough for "1h 23m" without
// re-rendering every second.
export function useNow(intervalMs = 30_000): number {
  return useNowClock(intervalMs);
}
