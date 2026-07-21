// Pure helpers for the Claude rate-limit meters (PRD #53): countdown/relative-time
// formatting (Decision 7 — resets stored as epochs, rendered client-side), the row
// classifier the three surfaces + the admin sort share, and a small clock hook so
// countdowns tick between the 60s polls. Kept free of the api client so it unit-
// tests without the mock; types are imported type-only (erased at build).

import { useEffect, useState } from "react";
import { toneFor } from "../components/Meter";
import type { AdminRateLimitUser, MyRateLimits, TokenRateLimits } from "./api";
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
export function worstTokenState(tokens: TokenRateLimits[]): RowState {
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
// only a ≥95% (danger) window escalates the pill to "5h nearly out" / "7d nearly
// out" / "5h & 7d nearly out" (a warn window keeps "Live" — the bar carries the
// amber). A stale row is a neutral pill: "vault locked" when the vault is the
// cause (Decision 3), else "stale". vaultLocked has no bearing on a live row.
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
      return { tone: "danger", label: `${which.join(" & ")} nearly out`, dot: true };
    }
  }
}

// useNow ticks a Date.now() clock so a rendered countdown re-derives between the
// 60s polls (Decision 7). Default 30s: fine-grained enough for "1h 23m" without
// re-rendering every second.
export function useNow(intervalMs = 30_000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(id);
  }, [intervalMs]);
  return now;
}
