// Pure helpers for the Codex per-account rate-limit meters (PRD #1209 M3), the Codex
// sibling of lib/rateLimits.ts. Codex reports a NESTED shape — a set of named buckets,
// each with up to two windows, each window carrying its OWN reported length — so unlike
// the Anthropic side (a fixed 5h/7d pair) the forecast MUST read each window's
// server-reported `limit_window_seconds` rather than any hardcoded map. Kept free of the
// api client so it unit-tests without the mock; types are imported type-only (erased at
// build).

import { toneFor } from "../components/Meter";
import { rowForecast, type PaceForecast } from "./rateLimits";
import type {
  BadgeTone,
} from "../components/ui";
import type {
  CodexAccountRateLimit,
  CodexAdminRateLimitRow,
  CodexRateLimitStatus,
  CodexRateLimitWindow,
} from "./api";

// codexResetEpoch is the window's effective ABSOLUTE reset time in epoch seconds — the
// horizon the countdown and the forecast anchor to. Codex may report an absolute
// `reset_at`, a relative `reset_after_seconds`, both, or neither; prefer the absolute,
// else derive it from the relative against now, else null (no horizon).
export function codexResetEpoch(win: CodexRateLimitWindow, nowMs: number): number | null {
  if (win.reset_at != null) return win.reset_at;
  if (win.reset_after_seconds != null) return Math.floor(nowMs / 1000) + win.reset_after_seconds;
  return null;
}

// formatCodexWindowLabel is the compact window chip ("5h", "7d", "3h", "1w"→"7d"),
// derived ENTIRELY from the server-reported `limit_window_seconds`. This is the visible
// proof that the surface reads the reported duration: a 3-hour bucket renders "3h", never
// a hardcoded "5h". Falls back to "window" when the length is missing/impossible.
export function formatCodexWindowLabel(limitWindowSeconds: number | null): string {
  const s = limitWindowSeconds;
  if (s == null || s <= 0) return "window";
  if (s % 86_400 === 0) return `${s / 86_400}d`;
  if (s % 3_600 === 0) return `${s / 3_600}h`;
  if (s % 60 === 0) return `${s / 60}m`;
  return `${s}s`;
}

// codexWindowForecast is the anchored pace forecast for ONE Codex window, feeding
// paceForecast (via rowForecast's shared stale short-circuit) the window's OWN reported
// duration — never a 5h/7d constant. It returns a silent "safe" (no ghost/marker) whenever
// the projection would be meaningless or misleading:
//   - used_percent null      — a partial window with no reading;
//   - limit_window_seconds null/≤0 — no window length to anchor to (impossible timing);
//   - no reset horizon       — neither reset_at nor reset_after_seconds reported;
//   - the account is stale   — projecting off a frozen number is misleading (rowForecast).
// paceForecast itself also silences an already-exhausted (≥100%) window, so a
// limit_reached bucket draws its full bar with no forecast overlay.
export function codexWindowForecast(
  stale: boolean,
  win: CodexRateLimitWindow,
  nowMs: number,
): PaceForecast {
  const SAFE: PaceForecast = { state: "safe", projectedPct: 0 };
  if (win.used_percent == null) return SAFE;
  if (win.limit_window_seconds == null || win.limit_window_seconds <= 0) return SAFE;
  const resetsAt = codexResetEpoch(win, nowMs);
  if (resetsAt == null) return SAFE;
  return rowForecast(stale, win.used_percent, resetsAt, win.limit_window_seconds, nowMs);
}

// codexAccountLabel names WHICH account a meter describes from its linked aliases — a
// name, never a token or raw provider id. Duplicate aliases collapse to one (a Set), so a
// server row that carries the same alias twice still reads as one account; an account with
// no alias falls back to a neutral noun.
export function codexAccountLabel(account: Pick<CodexAccountRateLimit, "aliases">): string {
  const named = [...new Set(account.aliases.filter((a) => a.trim() !== ""))];
  return named.length > 0 ? named.join(", ") : "Codex account";
}

// isCodexAccountShownInSidebar is the ONE place the "default always, extras by choice" rule
// is spelled for Codex, keyed on account_id (the Codex sibling of isShownInSidebar). The
// subscription default account always rides the rail; every other linked account shows only
// when the user checked it (its id is in sidebar_codex_account_ids).
export function isCodexAccountShownInSidebar(
  account: Pick<CodexAccountRateLimit, "account_id" | "is_default">,
  sidebarAccountIds: string[],
): boolean {
  if (account.is_default) return true;
  return sidebarAccountIds.includes(account.account_id);
}

// hasCodexReading is true only for the two statuses that carry a renderable meter (a
// current or an aged reading). Every other status — pending, no_reading, vault_locked,
// credential_action_required, polling_disabled — has no bar to draw and shows an explicit
// state instead. The sidebar rail renders ONLY accounts for which this is true.
export function hasCodexReading(status: CodexRateLimitStatus): boolean {
  return status === "fresh" || status === "stale";
}

export interface CodexStatusBadge {
  tone: BadgeTone;
  label: string;
  /** The one-line "what this means", for the badge tooltip + sr-only description. */
  hint: string;
}

// CODEX_STATUS_BADGES is EXHAUSTIVE over CodexRateLimitStatus by type, so a status added
// server-side without a rendering here fails typecheck rather than painting a blank badge.
// It is a MAP, not a computation (the same D21 reasoning as the Anthropic side): the server
// derives the closed-set status; the client renders it, never re-derives it.
const CODEX_STATUS_BADGES: Record<CodexRateLimitStatus, CodexStatusBadge> = {
  no_subscription: {
    tone: "neutral",
    label: "No subscription",
    hint: "No linked Codex subscription account, so there are no subscription windows to meter.",
  },
  pending: {
    tone: "neutral",
    label: "Pending",
    hint: "Linked, but no usage reading yet. A reading appears within a few minutes.",
  },
  no_reading: {
    tone: "warning",
    label: "No reading yet",
    hint: "A poll was attempted but returned no usage figures. uzi will retry on the next poll.",
  },
  fresh: {
    tone: "ok",
    label: "Live",
    hint: "A current usage reading.",
  },
  stale: {
    tone: "neutral",
    label: "Stale",
    hint: "The last usage reading has aged past the freshness window; what you see may be behind.",
  },
  vault_locked: {
    tone: "neutral",
    label: "🔒 Vault locked",
    hint: "Your vault is locked, so uzi cannot read the login to poll. Readings stay frozen until you unlock it.",
  },
  credential_action_required: {
    tone: "danger",
    label: "Action required",
    hint: "This Codex login needs re-authentication before uzi can read its usage again. Replace the login in your credentials.",
  },
  polling_disabled: {
    tone: "neutral",
    label: "Polling off",
    hint: "Usage polling is turned off on this instance, so this snapshot will not refresh.",
  },
};

// PROVIDER_REJECTED_HINT replaces the status hint when the server says the provider REJECTED
// the saved login's refresh material (issue #1594): a re-paste from the same, shared login
// would be rejected again, so the fix is a login used only by uzi.
const PROVIDER_REJECTED_HINT =
  "The saved Codex login was rejected by the provider; add a login used only by uzi.";

// codexLoginRejected reports whether the account's hint is the provider-rejected one: the
// server's reason, under action-required or a disabled poller (polling_disabled outranks
// the reauth status server-side, and must not hide the rejection). The server omits the
// reason under vault_locked, where unlocking is the only actionable step.
export function codexLoginRejected(account: Pick<CodexAccountRateLimit, "status" | "reason">): boolean {
  return (
    account.reason === "provider_rejected" &&
    (account.status === "credential_action_required" || account.status === "polling_disabled")
  );
}

// codexStatusBadge renders the server's per-account status verbatim (a MAP lookup), and is
// the ONE place the provider-rejected refinement overrides the hint, so the Settings card
// and the admin table cannot drift. The stored union is closed, so there is no
// unknown-value branch here — unlike the Anthropic auto-status chip, this field is not
// deployed ahead of the bundle by a newer server.
export function codexStatusBadge(
  account: Pick<CodexAccountRateLimit, "status" | "reason">,
): CodexStatusBadge {
  const badge = CODEX_STATUS_BADGES[account.status];
  // Tone and label stay the status's own ("Action required" / "Polling off"); only the hint
  // is refined.
  if (codexLoginRejected(account)) {
    return { ...badge, hint: PROVIDER_REJECTED_HINT };
  }
  return badge;
}

// codexAccountWorstPct is the highest used_percent across every window of every bucket on
// a reading account (fresh or stale), or null when the account has no numeric reading at
// all (every window partial). Used for the admin sort tie-break and to know whether a
// reading account actually has a bar to lead with.
export function codexAccountWorstPct(account: CodexAccountRateLimit): number | null {
  if (!hasCodexReading(account.status)) return null;
  let worst: number | null = null;
  for (const b of account.buckets) {
    for (const w of [b.primary, b.secondary]) {
      if (w && w.used_percent != null) worst = Math.max(worst ?? 0, w.used_percent);
    }
  }
  return worst;
}

// Sort rank for one account: a fresh reading rises by tone (danger → warn → ok), then a
// stale reading, then a login needing action, then everything with no reading. Mirrors the
// Anthropic RANK so the two admin tables lead with the same "nearest a wall first" logic.
function codexAccountRank(account: CodexAccountRateLimit): number {
  if (account.status === "fresh") {
    const pct = codexAccountWorstPct(account);
    if (pct != null) {
      const tone = toneFor(pct);
      return tone === "danger" ? 0 : tone === "warn" ? 1 : 2;
    }
    return 5; // fresh but every window partial — nothing to rank on
  }
  if (account.status === "stale") return 3;
  if (account.status === "credential_action_required") return 4;
  return 5; // pending / no_reading / vault_locked / polling_disabled
}

// A user is ranked by their MOST urgent account (the min rank), so a user with one
// exhausted account among several healthy ones still leads. 6 sinks a user with no
// linked account below every account-bearing row.
function codexUserRank(user: CodexAdminRateLimitRow): number {
  if (user.accounts.length === 0) return 6;
  return Math.min(...user.accounts.map(codexAccountRank));
}

// A user's worst reading pct across all their accounts, for the rank tie-break; -1 when
// none of their accounts carries a numeric reading.
function codexUserWorstPct(user: CodexAdminRateLimitRow): number {
  let worst = -1;
  for (const a of user.accounts) {
    const p = codexAccountWorstPct(a);
    if (p != null) worst = Math.max(worst, p);
  }
  return worst;
}

// sortCodexAdminRows orders the admin table danger → warn → ok → stale → action-required →
// no-reading, tie-broken by worst pct desc then name — the Codex sibling of sortAdminRows.
// A user's rank is their most urgent account's, so the capacity view leads with whoever is
// nearest a wall. Alias capacity is never double-counted: one account = one budget, and a
// user's worst pct is a max over accounts, not a sum.
export function sortCodexAdminRows(users: CodexAdminRateLimitRow[]): CodexAdminRateLimitRow[] {
  return [...users].sort((a, b) => {
    const ra = codexUserRank(a);
    const rb = codexUserRank(b);
    if (ra !== rb) return ra - rb;
    const pa = codexUserWorstPct(a);
    const pb = codexUserWorstPct(b);
    if (pa !== pb) return pb - pa;
    return a.name.localeCompare(b.name);
  });
}
