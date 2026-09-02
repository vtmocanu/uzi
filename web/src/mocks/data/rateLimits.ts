import type {
  AdminRateLimitUser,
  MyRateLimits,
  RateLimitSource,
  TokenRateLimits,
} from "../../lib/api";
import { NOW, minsAgo } from "./time";

// ── Claude rate limits (PRD #53) ─────────────────────────────────────────────
// Resets are epoch SECONDS derived from NOW so the demo countdowns stay fresh.
const NOW_SECS = Math.floor(NOW / 1000);
const H = 3600;
const D = 86_400;
const MIN = 60;

function okReading(
  pct5: number,
  in5: number,
  pct7: number,
  in7: number,
  syncedMins = 2,
  source: RateLimitSource = "usage_endpoint",
  stale = false,
): MyRateLimits {
  return {
    status: "ok",
    five_hour: { pct: pct5, resets_at: NOW_SECS + in5 },
    seven_day: { pct: pct7, resets_at: NOW_SECS + in7 },
    source,
    synced_at: minsAgo(syncedMins),
    stale,
  };
}

// The signed-in demo user's own readings (Settings card + sidebar). Since PRD #104
// this is ONE READING PER TOKEN: the default matches mockup frame A (8% / 27%,
// both green under the PRD #115 bands, "Live"), and the console key is busier so
// the two meter pairs are visibly different rather than duplicates.
export const mockMyRateLimits: MyRateLimits = okReading(8, 1 * H + 23 * MIN, 27, 2 * D + 4 * H);

// 🔴 auto_eligible HERE MUST AGREE WITH mockSecrets, TOKEN FOR TOKEN. The settings
// row draws its toggle from mockSecrets and its chip from this list, so a
// disagreement renders a checked box beside "not in pool" — which is what the demo
// build shipped until this was fixed, and exactly the contradiction the feature
// exists to prevent. The component now suppresses a chip that disagrees with the
// toggle rather than drawing it, so a mistake here degrades to a missing chip; that
// is a backstop, not a licence to let these drift.
export const mockMyTokenRateLimits: TokenRateLimits[] = [
  {
    secret_id: "sec-default",
    label: "default",
    is_default: true,
    // NOT pooled — the reserved-credential case D2 exists for. Its contrast with
    // console-key below is the thing worth seeing on a cold load.
    auto_eligible: false,
    auto_status: "not_pooled" as const,
    limits: mockMyRateLimits,
  },
  {
    // 34% / 22% — busier than the default but still both green under the #115
    // bands, so vlad reads "Live" on both tokens (mockup frame A) and is the
    // admin table's live_ok row.
    secret_id: "sec-console",
    label: "console-key",
    is_default: false,
    // Pooled AND pickable: the healthy chip, which had been rendering on no row at all.
    auto_eligible: true,
    auto_status: "eligible" as const,
    limits: okReading(34, 2 * H + 5 * MIN, 22, 3 * D + 2 * H, 3),
  },
  {
    // F2: `never polled` — pooled, but uzi has NEVER read a usage figure for it, so
    // the selector can never pick it. This is R7's silent no-op, and without a
    // fixture carrying it the state the chip mechanism exists to surface was
    // unreachable in the demo build. `unavailable` is what a token with no gauge row
    // actually returns.
    secret_id: "sec-never-polled",
    label: "refused-key",
    is_default: false,
    auto_eligible: true,
    auto_status: "no_reading" as const,
    limits: { status: "unavailable" as const },
  },
  {
    // F2: `low headroom` — pooled, current, and nearly spent. Distinct from the three
    // above in the way F4 is about: per D10 this token IS still picked when every
    // pooled token is this low, so it must not wear the same "skipped" tone.
    secret_id: "sec-low",
    label: "nearly-spent",
    is_default: false,
    auto_eligible: true,
    auto_status: "below_threshold" as const,
    limits: okReading(93, 40 * MIN, 88, 1 * D, 3),
  },
  {
    // PRD #217 M3: a `limit_report` reading — the only fixture that makes the
    // park-time source disclosure (D6) visible under VITE_UZI_MOCK=1. This token
    // just hit its five-hour wall, so the park wrote that window 100% consumed with
    // source `limit_report` and DID NOT bump synced_at (D3): the reading is 100%
    // but the "updated 14m ago" line beside it is OLDER, which is exactly the state
    // the "Recorded at usage limit" badge exists to explain. Kept NON-pooled so it
    // stays outside pooledFixtureStatus while still agreeing with mockSecrets.
    secret_id: "sec-parked",
    label: "parked-key",
    is_default: false,
    auto_eligible: false,
    auto_status: "not_pooled" as const,
    limits: okReading(100, 35 * MIN, 40, 2 * D + 5 * H, 14, "limit_report"),
  },
];

// tokenised wraps a single reading as a one-token list, for the personas whose
// fixture is a single credential.
function tokenised(limits: MyRateLimits, label = "default"): TokenRateLimits[] {
  return limits.status === "no_token"
    ? [] // token-less is an EMPTY list since M5, not a status
    : [
        {
          secret_id: `sec-${label}`,
          label,
          is_default: true,
          auto_eligible: false,
          auto_status: "not_pooled" as const,
          limits,
        },
      ];
}

// Per-persona readings so a demo login as a seeded non-admin reaches every own-
// reading state; anyone else gets the live-ok default (u-admin). warn (radu) and
// stale (mihai) are here so the sidebar-dim + Settings "Stale" badge and a warn-
// tone bar are browsable, not just visible in the admin table.
export const mockMyRateLimitsByUser: Record<string, TokenRateLimits[]> = {
  "u-admin": mockMyTokenRateLimits, // live ok, TWO tokens
  "u-radu": tokenised(okReading(62, 44 * MIN, 83, 1 * D + 9 * H, 3)), // warn (7d 83%)
  "u-mira": tokenised(okReading(97, 2 * H + 10 * MIN, 71, 4 * D + 1 * H, 1)), // danger (5h 97%)
  // stale own-reading: no live countdown (resets null), aged synced_at, stale flag.
  "u-mihai": tokenised({
    status: "ok",
    five_hour: { pct: 31, resets_at: null },
    seven_day: { pct: 12, resets_at: null },
    source: "header_probe",
    synced_at: minsAgo(180),
    stale: true,
  }),
  "u-andrei": tokenised({ status: "unavailable" }),
  "u-dan": [], // token-less: an EMPTY list, not a no_token status (PRD #104 M5)
};

// The admin all-users table (mockup frame C) plus a warn row and an unavailable
// row, so every row state is demonstrable: live-ok, live-warn, live-danger,
// stale+vault-locked, unavailable, no_token.
export const mockAdminRateLimits: AdminRateLimitUser[] = [
  // vlad holds TWO tokens, so the admin table's per-user grouping is demonstrable.
  { id: "u-admin", email: "vlad@example.com", name: "vlad", vault_locked: false, tokens: mockMyTokenRateLimits },
  { id: "u-radu", email: "radu@example.com", name: "radu", vault_locked: false, tokens: tokenised(okReading(62, 44 * MIN, 83, 1 * D + 9 * H, 3)) },
  { id: "u-ana", email: "ana@example.com", name: "ana", vault_locked: false, tokens: tokenised(okReading(97, 2 * H + 10 * MIN, 71, 4 * D + 1 * H, 1)) },
  // sorin demonstrates the new PRD #115 85–94 danger band: 5h at 88% paints a red
  // bar (danger tone ≥85) but the status pill stays a green "Live" because no
  // window has crossed 95 (the badge stays decoupled at ≥95).
  { id: "u-sorin", email: "sorin@example.com", name: "sorin", vault_locked: false, tokens: tokenised(okReading(88, 3 * H + 5 * MIN, 76, 3 * D + 6 * H, 4)) },
  { id: "u-mihai", email: "mihai@example.com", name: "mihai", vault_locked: true, tokens: tokenised(okReading(31, 0, 12, 0, 180, "header_probe", true)) },
  { id: "u-dana", email: "dana@example.com", name: "dana", vault_locked: false, tokens: tokenised({ status: "unavailable" }) },
  { id: "u-irina", email: "irina@example.com", name: "irina", vault_locked: false, tokens: [] },
];
