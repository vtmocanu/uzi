// formatUptimeSince renders a worker's uptime — how long it has been online — as a
// compact duration counting UP from a past instant (PRD #251). It mirrors
// formatCountdown's buckets in rateLimits.ts ("2d 4h" / "1h 23m" / "44m" / "<1m")
// so uptime and rate-limit reset countdowns read alike across the app, but it is a
// separate function because that one counts DOWN to a future reset and reads "now"
// at the floor, whereas this counts up and floors at "<1m".
//
// Deliberately NOT named formatUptime: BuildInfoPopover.tsx already exports a
// formatUptime(seconds) with a different signature and a different floor ("0s"), so
// a second export of that name would collide and mislead. This one takes an ISO
// instant + injectable nowMs and returns "" only for an UNPARSEABLE instant.
//
// A negative elapsed (the anchor is in the future) is clock skew, not invalid: we
// only ever call this for a worker that IS online, so the honest reading is "just
// came up" — floored at "<1m", exactly as the Go twin (uptimeCell/formatUptimeDuration
// in api/cmd/uzi/worker.go) floors a negative time.Since. Returning "" here instead
// would render a dangling "· up " label with no duration when a browser clock lags
// the server's by a second, and would diverge from the Go side at the very floor
// Decision 4 says the two must share.
export function formatUptimeSince(sinceIso: string, nowMs = Date.now()): string {
  const since = Date.parse(sinceIso);
  if (Number.isNaN(since)) return "";
  const total = Math.floor((nowMs - since) / 1000);
  if (total < 60) return "<1m"; // clock-skew negatives and true sub-minute both floor here
  const d = Math.floor(total / 86_400);
  const h = Math.floor((total % 86_400) / 3_600);
  const m = Math.floor((total % 3_600) / 60);
  if (d >= 1) return `${d}d ${h}h`;
  if (h >= 1) return `${h}h ${m}m`;
  return `${m}m`; // total >= 60 here, so m >= 1
}
