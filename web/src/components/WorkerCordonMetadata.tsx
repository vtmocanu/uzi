import type { Worker } from "../lib/api";
import { stripDisplayCell } from "../lib/safeText";

/** Read-only attribution, independent of connectivity, custody and automatic draining. */
export function WorkerCordonMetadata({ worker }: { worker: Worker }) {
  if (!worker.manual_cordoned_at) return null;
  const actor = stripDisplayCell(worker.manual_cordoned_by_label).trim()
    || stripDisplayCell(worker.manual_cordoned_by).trim()
    || "unknown actor";
  const stamp = stripDisplayCell(worker.manual_cordoned_at);
  const parsed = Date.parse(stamp);
  const iso = Number.isFinite(parsed) ? new Date(parsed).toISOString() : undefined;
  const reason = stripDisplayCell(worker.manual_cordon_reason).trim();
  return (
    <div className="min-w-0 max-w-full whitespace-normal break-words [overflow-wrap:anywhere] text-xs text-muted">
      <span>Manual cordon by <span title={actor}>{actor}</span></span>
      {" · "}
      {iso ? <time dateTime={iso} title={iso}>{iso}</time> : <span>time unavailable</span>}
      {reason && <div title={reason}>Reason: {reason}</div>}
    </div>
  );
}
