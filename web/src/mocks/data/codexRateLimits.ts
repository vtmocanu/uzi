import type {
  CodexAccountRateLimit,
  CodexAdminRateLimitRow,
  CodexRateLimitBucket,
  CodexRateLimitStatus,
  CodexRateLimitWindow,
} from "../../lib/api";
import { NOW, minsAgo } from "./time";

// ── Codex per-account rate limits (PRD #1209 M3) ─────────────────────────────
// The Codex sibling of data/rateLimits.ts. These fixtures make the WHOLE feature
// demoable under VITE_UZI_MOCK=1: every closed-set status, partial windows, a
// NONSTANDARD window duration (a 3-hour bucket, proving the forecast reads the
// reported duration and not a hardcoded 5h/7d), an exhausted (limit_reached)
// account, several buckets per account, and duplicate aliases that collapse to one
// account/budget. Percentages/labels only — no token, login blob or provider id.
const NOW_SECS = Math.floor(NOW / 1000);
const H = 3600;
const D = 86_400;
const MIN = 60;

// win builds one utilization window. usedPct null models a PARTIAL window (Codex
// reported the bucket but no percentage for this slot). resetIn null models a window
// with no reported horizon (the forecast and countdown both suppress). reset_at is
// derived from NOW so the demo countdowns stay fresh; reset_after_seconds carries the
// same relative value so a window that reports only the relative form is exercised too.
function win(
  usedPct: number | null,
  windowSecs: number | null,
  resetIn: number | null,
): CodexRateLimitWindow {
  return {
    used_percent: usedPct,
    limit_window_seconds: windowSecs,
    reset_after_seconds: resetIn,
    reset_at: resetIn == null ? null : NOW_SECS + resetIn,
  };
}

function bucket(
  id: string,
  displayName: string | undefined,
  primary: CodexRateLimitWindow | null,
  secondary: CodexRateLimitWindow | null,
  opts: { allowed?: boolean | null; limitReached?: boolean | null } = {},
): CodexRateLimitBucket {
  const b: CodexRateLimitBucket = {
    id,
    allowed: opts.allowed ?? true,
    limit_reached: opts.limitReached ?? false,
    primary,
    secondary,
  };
  if (displayName !== undefined) b.display_name = displayName;
  return b;
}

function acct(
  account_id: string,
  aliases: string[],
  is_default: boolean,
  status: CodexRateLimitStatus,
  buckets: CodexRateLimitBucket[],
  opts: { lastSuccessMins?: number; stale?: boolean; reason?: "provider_rejected" } = {},
): CodexAccountRateLimit {
  const a: CodexAccountRateLimit = { account_id, aliases, is_default, status, buckets };
  if (opts.lastSuccessMins != null) a.last_success_at = minsAgo(opts.lastSuccessMins);
  if (opts.stale) a.stale = true;
  if (opts.reason) a.reason = opts.reason;
  return a;
}

// vlad's default subscription account, shaped like the real api (codexauth/usage.go): its
// MAIN limit is the bucket with id "codex" and an EMPTY display name (the top-level
// rate_limit), a healthy 5-hour/7-day pair whose caption the sidebar and the Settings card
// hide (PRD #1653 D-W3), PLUS an additional bucket on a NONSTANDARD 3-hour window, which
// keeps its "Code (3-hour)" caption. The 3-hour bucket is the proof that the surface reads
// the reported limit_window_seconds (10800 → "3h") rather than a hardcoded 5h/7d map — its
// forecast anchors to 10800s, so its ghost/marker geometry differs from what a 5h
// assumption would draw.
const acctDefault = acct(
  "cdx-acct-default",
  ["personal-codex"],
  true,
  "fresh",
  [
    // The main limit: id "codex", empty display name (never captioned in the UI).
    bucket("codex", "", win(28, 5 * H, 1 * H + 40 * MIN), win(46, 7 * D, 3 * D + 2 * H)),
    // NONSTANDARD 3-hour window (10800s). Renders the "3h" chip; the forecast uses 10800,
    // never 18000. 63% is a warn-tone bar climbing toward a near reset.
    bucket("code", "Code (3-hour)", win(63, 3 * H, 38 * MIN), null),
  ],
  { lastSuccessMins: 2 },
);

// vlad's SECOND linked account. Duplicate aliases (the server row carries "team-codex"
// twice) collapse to ONE account/budget in the UI. It shows a PARTIAL primary window
// (used_percent null → no bar, no forecast) beside a hot secondary, and a separate
// EXHAUSTED (limit_reached) bucket pinned at 100%.
const acctTeam = acct(
  "cdx-acct-team",
  ["team-codex", "team-codex"],
  false,
  "fresh",
  [
    bucket("requests", "Requests", win(null, 5 * H, 30 * MIN), win(91, 7 * D, 5 * D)),
    bucket("tokens", "Tokens", win(100, 7 * D, 2 * D), null, { allowed: false, limitReached: true }),
  ],
  { lastSuccessMins: 4 },
);

// A stale account: an aged reading (dimmed everywhere), no live countdown (resets null).
const acctStale = acct(
  "cdx-acct-stale",
  ["archived-login"],
  false,
  "stale",
  [bucket("requests", "Requests", win(52, 5 * H, null), win(38, 7 * D, null))],
  { lastSuccessMins: 200, stale: true },
);

// The three no-meter states, each carrying an empty bucket set (nothing to draw) and an
// explicit state in Settings instead.
const acctNoReading = acct("cdx-acct-noreading", ["new-seat"], false, "no_reading", []);
const acctPending = acct("cdx-acct-pending", ["just-linked"], false, "pending", []);
const acctReauth = acct(
  "cdx-acct-reauth",
  ["expired-login"],
  false,
  "credential_action_required",
  [],
);

// The signed-in demo user (vlad / u-admin) holds the whole spread: fresh default + fresh
// extra (with a partial window, an exhausted bucket and duplicate aliases) + stale +
// no-reading + pending + action-required. The Settings card shows every state at once;
// the sidebar shows only the fresh default (the rest are opt-in / have no reading).
export const mockMyCodexRateLimits: CodexAccountRateLimit[] = [
  acctDefault,
  acctTeam,
  acctStale,
  acctNoReading,
  acctPending,
  acctReauth,
];

// Per-persona owner readings, so a demo login as a seeded non-admin reaches every
// whole-user state (vault-locked, polling-off, no-subscription) that cannot coexist with
// a fresh account on one user. Anyone else gets vlad's rich spread.
export const mockMyCodexRateLimitsByUser: Record<string, CodexAccountRateLimit[]> = {
  "u-admin": mockMyCodexRateLimits,
  // A single fresh default with a warn-tone reading, so a clean single-account sidebar +
  // an amber bar are browsable per-persona.
  "u-mira": [
    acct(
      "cdx-mira-default",
      ["mira-codex"],
      true,
      "fresh",
      [bucket("requests", "Requests", win(57, 5 * H, 2 * H + 12 * MIN), win(72, 7 * D, 4 * D))],
      { lastSuccessMins: 1 },
    ),
  ],
  // Linked but never polled yet: the sidebar renders nothing (no reading); Settings shows
  // the account selectable BEFORE its first reading, with a "Pending" state.
  "u-andrei": [acct("cdx-andrei-default", ["andrei-codex"], true, "pending", [])],
  // no_subscription: an EMPTY list. This is also the API-key-only-default shape — the
  // read returns no subscription meter, so the sidebar and card both stay empty.
  "u-dan": [],
  // Whole-user vault lock: every account reports vault_locked even though a prior snapshot
  // exists (served as-is, dimmed in Settings, absent from the sidebar).
  "u-radu": [
    acct(
      "cdx-radu-default",
      ["radu-codex"],
      true,
      "vault_locked",
      [bucket("requests", "Requests", win(44, 5 * H, 3 * H), win(61, 7 * D, 2 * D + 6 * H))],
      { lastSuccessMins: 90 },
    ),
  ],
  // Instance polling turned off: the snapshot is stale-flagged and never refreshes.
  "u-mihai": [
    acct(
      "cdx-mihai-default",
      ["mihai-codex"],
      true,
      "polling_disabled",
      [bucket("requests", "Requests", win(19, 5 * H, null), win(8, 7 * D, null))],
      { lastSuccessMins: 240, stale: true },
    ),
  ],
};

// The admin all-users table. One row per user with ≥1 LINKED account (the server's
// EXISTS(linked) filter drops a user with none, so no_subscription users like u-dan do
// NOT appear). Covers every renderable status and the sort: vlad leads (a danger-tone
// 91% + an exhausted 100%), then mira (warn), then the no-reading / action-required /
// stale / whole-user rows sink.
export const mockAdminCodexRateLimits: CodexAdminRateLimitRow[] = [
  {
    id: "u-admin",
    email: "vlad@example.com",
    name: "vlad",
    vault_locked: false,
    accounts: [acctDefault, acctTeam],
  },
  {
    id: "u-mira",
    email: "mira@example.com",
    name: "mira",
    vault_locked: false,
    accounts: mockMyCodexRateLimitsByUser["u-mira"],
  },
  {
    id: "u-sorin",
    email: "sorin@example.com",
    name: "sorin",
    vault_locked: false,
    accounts: [
      acct(
        "cdx-sorin-default",
        ["sorin-codex"],
        true,
        "fresh",
        [bucket("requests", "Requests", win(88, 3 * H, 25 * MIN), win(64, 7 * D, 3 * D))],
        { lastSuccessMins: 3 },
      ),
    ],
  },
  {
    id: "u-andrei",
    email: "andrei@example.com",
    name: "andrei",
    vault_locked: false,
    accounts: mockMyCodexRateLimitsByUser["u-andrei"],
  },
  {
    id: "u-carmen",
    email: "carmen@example.com",
    name: "carmen",
    vault_locked: false,
    // The provider REJECTED carmen's saved login (issue #1594): the same action-required
    // badge, but the hint tells her to add a login used only by uzi.
    accounts: [
      acct("cdx-carmen-default", ["carmen-codex"], true, "credential_action_required", [], {
        reason: "provider_rejected",
      }),
    ],
  },
  {
    id: "u-radu",
    email: "radu@example.com",
    name: "radu",
    vault_locked: true,
    accounts: mockMyCodexRateLimitsByUser["u-radu"],
  },
  {
    id: "u-mihai",
    email: "mihai@example.com",
    name: "mihai",
    vault_locked: false,
    accounts: mockMyCodexRateLimitsByUser["u-mihai"],
  },
];
