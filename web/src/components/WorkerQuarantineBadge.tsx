import type { Worker } from "../lib/api";
import { formatUptimeSince } from "../lib/formatUptimeSince";
import { stripUnsafeChars } from "../lib/safeText";
import { Badge } from "./ui";

// quarantineDetail is the badge's full explanation: the latch age, the worker-reported
// cause and the remedy, as one plain-text sentence group.
function quarantineDetail(
  latchedAt: string,
  cause: string | null | undefined,
  nowMs: number,
): string {
  const age = formatUptimeSince(latchedAt, nowMs);
  const when = age ? `Quarantined ${age} ago` : "Quarantined";
  const reported = cause ? stripUnsafeChars(cause).trim() : "";
  return [
    `${when}: the worker found an unreadable process it could not attribute, running as its own user. It claims no runs and starts no new agent turns until its container restarts.`,
    // Quoted, so the untrusted text has a visible boundary however it ends.
    reported ? `Reported cause: "${reported}".` : "No cause was reported.",
    "Restart the worker's container to clear it.",
  ].join(" ");
}

/**
 * The residue-quarantine pill (issue #2213).
 *
 * Non-null `residue_quarantined_at` means the worker latched a quarantine: it found an
 * unreadable, unattributed same-uid process, so it claims nothing and starts no
 * forge-credentialed git or new Claude/Codex turn until its container restarts. The
 * api overlays both fields from the worker's last heartbeat, so a heartbeat without
 * the latch clears them and the pill disappears on the next poll.
 *
 * `warning`-toned with a dot, matching the `fleet.quarantine` health check's warn
 * severity: the worker is out of service until someone acts, but nothing is lost.
 * The visible WORD carries the state (never colour alone); the `title` adds the age,
 * the cause and the remedy for pointer users, and the same sentence follows as a
 * sibling sr-only span because a `title` on a generic <span> is not reliably announced.
 *
 * The cause is UNTRUSTED worker self-report. The api sanitizes and bounds it; here it
 * is rendered only as React text and a React-escaped attribute (never as markup), and
 * Cc/Cf characters are stripped again so a bidi override cannot reorder the sentence.
 */
export function WorkerQuarantineBadge({
  worker,
  nowMs,
}: {
  worker: Pick<Worker, "residue_quarantined_at" | "residue_quarantine_cause">;
  nowMs?: number;
}) {
  const latchedAt = worker.residue_quarantined_at;
  if (!latchedAt) return null;
  const detail = quarantineDetail(latchedAt, worker.residue_quarantine_cause, nowMs ?? Date.now());
  return (
    <>
      <Badge tone="warning" dot title={detail}>
        quarantined
      </Badge>
      <span className="sr-only">{detail}</span>
    </>
  );
}
