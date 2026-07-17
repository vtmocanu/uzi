// CLI-token render helpers (PRD #64 M6). Pure, so the Settings section can flag a
// stale token without owning the arithmetic — and so the rule is unit-testable.

import type { CliToken } from "./api";

// CLI_TOKEN_STALE_DAYS is the staleness threshold M6 surfaces as a render-time
// hint ONLY: no new column, endpoint, policy, or auto-expiry (PRD Decision — the
// "low, your call" item, scoped to a comparison against a column already on
// screen). The argument that earns it: a webui-minted uzc_ never expires and
// nothing caps the count, so the list grows monotonically and "revoke each one"
// degrades exactly as it matters most.
export const CLI_TOKEN_STALE_DAYS = 90;

const DAY_MS = 24 * 60 * 60 * 1000;

// isCliTokenStale reports whether a live token has gone unused for
// CLI_TOKEN_STALE_DAYS+. "Unused for 90+ days" means the last time it was used
// was that long ago; a never-used token (last_used_at null) is measured from
// created_at, since it has been unused since it existed. A revoked token is never
// flagged — it is already dead, so nudging the user to revoke it is noise.
export function isCliTokenStale(token: CliToken, now: number = Date.now()): boolean {
  if (token.revoked) return false;
  const reference = token.last_used_at ?? token.created_at;
  const t = Date.parse(reference);
  if (Number.isNaN(t)) return false;
  return now - t >= CLI_TOKEN_STALE_DAYS * DAY_MS;
}
