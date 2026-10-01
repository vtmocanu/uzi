import { Badge } from "./ui";
import type { Worker } from "../lib/api";
import { formatUptimeSince } from "../lib/formatUptimeSince";
import { stripUnsafeChars } from "../lib/safeText";

// WorkerPendingOutcomeBadge marks a worker holding run OUTCOMES it has journaled but not yet
// delivered to the api (issue #1994): reported_runs entries flagged terminal_pending. Such an
// entry still reads phase "running" on the wire, and its run row is still nonterminal, so the
// worker's busy/active_runs (and so WorkerRunBadge beside this pill) keep counting it; this pill
// marks which of that load is an undelivered outcome rather than an execution. It renders
// nothing when no entry is pending, including on an older api that omits the field.
//
// The title lists each pending run with its claim generation and how long it has been pending
// (formatUptimeSince, the same count-up buckets the uptime token uses; the age is omitted when
// terminal_pending_since is null or unparseable). Run ids are server-validated UUIDs, but they
// still pass through stripUnsafeChars like every other worker-reported string on this page.
export function WorkerPendingOutcomeBadge({
  worker,
  nowMs,
}: {
  worker: Pick<Worker, "reported_runs">;
  // Injectable clock for tests; defaults to the render-time Date.now().
  nowMs?: number;
}) {
  const pending = (worker.reported_runs ?? []).filter((r) => r.terminal_pending === true);
  if (pending.length === 0) return null;
  const now = nowMs ?? Date.now();
  const lines = pending.map((r) => {
    const age = r.terminal_pending_since ? formatUptimeSince(r.terminal_pending_since, now) : "";
    const base = `run ${stripUnsafeChars(r.run_id)} (generation ${r.claim_generation})`;
    return age ? `${base}, pending ${age}` : base;
  });
  const label = pending.length === 1 ? "1 pending outcome" : `${pending.length} pending outcomes`;
  return (
    <Badge
      tone="warning"
      dot
      title={`Outcome journaled on this worker but not yet delivered to the api.\n${lines.join("\n")}`}
    >
      {label}
    </Badge>
  );
}
