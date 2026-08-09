// Pure, framework-free per-state run-duration token (issue #256 M2), the web twin of
// the CLI's runAgeCell (api/cmd/uzi/run.go). Derived entirely client-side from the
// timestamps a run already carries, so no new API field is needed.

import { formatElapsed } from "./runBadge";

// RunDurationInput is the minimal run shape runDurationLabel reads. The three lifecycle
// timestamps are BOTH optional and nullable (Decision 6) so a single type accepts the
// full Run (where claimed/started/finished are `string | null`) AND the board's narrow
// LatestRun (which carries only created_at/updated_at — the keys are simply absent) by
// structural typing.
export interface RunDurationInput {
  status: string;
  created_at: string;
  updated_at: string;
  claimed_at?: string | null;
  started_at?: string | null;
  finished_at?: string | null;
}

// parseTs parses an RFC3339 timestamp to epoch ms, or null when the value is
// missing/unparseable — so callers return "" rather than a fabricated token.
function parseTs(iso: string | null | undefined): number | null {
  if (iso == null) return null;
  const t = Date.parse(iso);
  return Number.isNaN(t) ? null : t;
}

/**
 * runDurationLabel folds a run into a small duration token whose VERB and ANCHOR both
 * depend on the run's state (Decision 2, mirroring the CLI twin runAgeCell in
 * api/cmd/uzi/run.go and the runBadge/formatElapsed discipline):
 *
 *   - queued              → `queued <elapsed>`  since created_at (time waiting to be claimed)
 *   - claimed             → `claimed <elapsed>` since claimed_at ?? created_at
 *   - running             → `running <elapsed>` since started_at ?? created_at
 *   - awaiting_approval /
 *     awaiting_input /
 *     limit_wait           → `waiting <elapsed>` since updated_at (time parked in that state)
 *   - completed / failed /
 *     cancelled (terminal) → `ran <elapsed>`, the STATIC span finished_at − started_at, i.e.
 *                            how long it actually ran, independent of nowMs
 *   - anything else        → ""
 *
 * The `""`-when-missing convention is deliberate: whenever the chosen anchor is
 * null/undefined or unparseable — including a terminal run that never started (cancelled
 * or failed before start_at was stamped), where the static span cannot be computed — the
 * token is "" rather than a fabricated "queued 0s". nowMs is passed in (never read from
 * Date.now inside) so the live buckets are deterministic under test; the terminal bucket
 * ignores nowMs entirely because its span is static. The input is never mutated.
 */
export function runDurationLabel(run: RunDurationInput, nowMs: number): string {
  switch (run.status) {
    case "queued":
      return liveToken("queued", run.created_at, nowMs);
    case "claimed":
      return liveToken("claimed", run.claimed_at ?? run.created_at, nowMs);
    case "running":
      return liveToken("running", run.started_at ?? run.created_at, nowMs);
    case "awaiting_approval":
    case "awaiting_input":
    case "limit_wait":
      return liveToken("waiting", run.updated_at, nowMs);
    case "completed":
    case "failed":
    case "cancelled": {
      // Static ran-span, not a live age: only meaningful when the run both started and
      // finished. Missing either end (never started) → "".
      const started = parseTs(run.started_at);
      const finished = parseTs(run.finished_at);
      if (started == null || finished == null) return "";
      return `ran ${formatElapsed(finished - started)}`;
    }
    default:
      return "";
  }
}

// liveToken renders `<verb> <elapsed>` for a non-terminal state, or "" when the anchor
// is missing/unparseable.
function liveToken(
  verb: string,
  anchor: string | null | undefined,
  nowMs: number,
): string {
  const t = parseTs(anchor);
  if (t == null) return "";
  return `${verb} ${formatElapsed(nowMs - t)}`;
}
