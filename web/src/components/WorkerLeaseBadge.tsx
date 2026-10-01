import type { Worker } from "../lib/api";
import { formatCountdown } from "../lib/rateLimits";
import { useNow } from "../lib/useNow";
import { Badge } from "./ui";

// leaseRemaining is the "1h 12m" in "leased · 1h 12m left", or null when there is
// nothing honest to show: no lease, an unparseable instant, or a lease that has
// already run out. An expired lease renders NOTHING rather than "0m left" or a
// negative: the api stops sending the field once the lease ends, so a past value is
// only ever a stale poll or browser clock skew, and the worker is about to be removed.
// It reuses the rate-limit countdown's buckets ("2d 4h" / "1h 23m" / "44m" / "<1m")
// so every countdown in the app reads alike. nowMs is injectable for tests.
export function leaseRemaining(expiresAt: string | undefined, nowMs: number): string | null {
  if (!expiresAt) return null;
  const at = Date.parse(expiresAt);
  if (!Number.isFinite(at) || at <= nowMs) return null;
  return formatCountdown(at / 1000, nowMs);
}

/**
 * The leased pill (PRD #2006 M2): a finished ephemeral worker kept warm, idle, for a
 * same-owner, same-repository, same-branch follow-up run until its lease ends.
 *
 * Like the `docker` and `ephemeral` badges it is a WORD with the remaining time as
 * visible text, not an icon: Badge is a bare <span> (ARIA role `generic`, where naming
 * is prohibited), so text is the only thing that reaches sighted, keyboard and
 * screen-reader users at once. The `title` only adds the absolute end time and the
 * why, never information the visible label lacks.
 *
 * `plan`-toned like `ephemeral`, which it always sits beside: a lease is a calm,
 * normal phase of that worker's life, not an alert. It must NOT enter needsAttention.
 *
 * Its own 30s clock keeps the countdown ageing between the page's 10s fleet polls
 * (and when a poll is skipped), and drops the badge at expiry without a refetch.
 */
export function WorkerLeaseBadge({ worker }: { worker: Worker }) {
  const now = useNow(worker.ephemeral_lease_expires_at ? 30_000 : null);
  const left = leaseRemaining(worker.ephemeral_lease_expires_at, now);
  if (left == null) return null;
  const until = new Date(Date.parse(worker.ephemeral_lease_expires_at!)).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
  return (
    <Badge
      tone="plan"
      title={`Its run has finished. Kept warm until ${until} so a follow-up run on the same repository and branch can reuse it, then removed. Still counts toward your ephemeral worker limit.`}
    >
      leased · {left} left
    </Badge>
  );
}
