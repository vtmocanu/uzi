import type { Worker } from "../lib/api";
import { stripDisplayCell } from "../lib/safeText";

// Independent read-only pills: automatic wording/load semantics remain keyed on
// draining_since and run/cross-check counts. Manual attribution is display-only.
export function WorkerCordonBadge({ worker }: { worker: Worker }) {
  const manual = !!worker.manual_cordoned_at;
  if (!worker.draining_since && !manual && !worker.effective_cordoned) return null;
  const draining = worker.active_runs > 0 || worker.active_cross_checks > 0;
  const label = draining ? "draining" : "cordoned";
  const title = draining
    ? "Cordoned — finishing its current runs and cross-checks, not claiming new runs."
    : "Cordoned — not claiming new runs.";
  const actor = stripDisplayCell(worker.manual_cordoned_by_label).trim()
    || stripDisplayCell(worker.manual_cordoned_by).trim() || "unknown actor";
  const at = stripDisplayCell(worker.manual_cordoned_at);
  const reason = stripDisplayCell(worker.manual_cordon_reason).trim();
  const manualTitle = `Cordoned (manual) — ${actor}${at ? ` · ${at}` : ""}${reason ? ` · ${reason}` : ""}`;
  const pill = "inline-flex items-center gap-1 rounded-md border border-dashed border-edge-strong bg-neutral-surface text-neutral-fg px-1.5 py-0.5 text-[11px] font-medium whitespace-nowrap";
  return (
    <>
      {worker.draining_since && <span title={title} className={pill}>{label}</span>}
      {manual && <span title={manualTitle} className={pill}>cordoned (manual)</span>}
      {!worker.draining_since && !manual && <span className={pill}>intake fenced</span>}
    </>
  );
}
