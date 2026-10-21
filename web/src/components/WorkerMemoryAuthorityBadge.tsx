import type { Worker } from "../lib/api";
import { formatUptimeSince } from "../lib/formatUptimeSince";
import { Badge } from "./ui";

// memoryAuthorityDetail is the badge's full explanation: how long ago the api began
// withholding guard-bound runs, what still runs, and the remedy, as plain text.
function memoryAuthorityDetail(lostAt: string, nowMs: number): string {
  const age = formatUptimeSince(lostAt, nowMs);
  const when = age ? `Lost memory authority ${age} ago` : "Lost memory authority";
  return [
    `${when}: the api withholds guard-bound runs from this worker (judge, job, cross-check, review and isolated-fetch runs still run).`,
    "Restart the worker container to regain it.",
  ].join(" ");
}

/**
 * The lost-memory-authority pill (issue #3142).
 *
 * Non-null `memory_authority_lost_at` means the api began withholding guard-bound runs
 * from this worker because the worker lost memory authority. The state is in-process on
 * the api and does not survive an api restart; the operator action is to restart the
 * worker container.
 *
 * `warning`-toned with a dot, matching the `fleet.memory_authority` health check's warn
 * severity. The visible WORD carries the state (never colour alone); the `title` adds the
 * age and remedy for pointer users, and the same sentence follows as a sibling sr-only
 * span because a `title` on a generic <span> is not reliably announced.
 */
export function WorkerMemoryAuthorityBadge({
  worker,
  nowMs,
}: {
  worker: Pick<Worker, "memory_authority_lost_at">;
  nowMs?: number;
}) {
  const lostAt = worker.memory_authority_lost_at;
  if (!lostAt) return null;
  const detail = memoryAuthorityDetail(lostAt, nowMs ?? Date.now());
  return (
    <>
      <Badge tone="warning" dot title={detail}>
        memory authority lost
      </Badge>
      <span className="sr-only">{detail}</span>
    </>
  );
}
