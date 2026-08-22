import type { Worker } from "../lib/api";

/**
 * The draining/cordoned pill (PRD #496 M2).
 *
 * A dashed-border NEUTRAL pill, deliberately NOT a solid BadgeTone, so it can't be
 * confused with the amber `outdated`/run badges — a cordon is a normal operational
 * state (the worker finishes its in-flight runs and claims nothing new), not an alert.
 *
 * Keyed on `draining_since` (hosted-only by construction — every non-null write is
 * WHERE kind='hosted', so no `kind` check is needed), NEVER on `upgrade_status`/`busy`/
 * `kind`: draining is orthogonal to those. The label splits on `active_runs`, not `busy`
 * — a chat-only cordoned worker is busy:true with active_runs:0 and must read `cordoned`.
 *
 * It must NOT enter needsAttention/Fleet counts — a cordon is a normal state, not an
 * alert. No stripUnsafeChars is needed: the only text is static copy, never a
 * worker-controlled string.
 */
export function WorkerCordonBadge({ worker }: { worker: Worker }) {
  if (!worker.draining_since) return null;
  const draining = worker.active_runs > 0;
  const label = draining ? "draining" : "cordoned";
  const title = draining
    ? "Cordoned — finishing its current runs, not claiming new ones."
    : "Cordoned — not claiming new runs.";
  return (
    <span
      title={title}
      className="inline-flex items-center gap-1 rounded-md border border-dashed border-edge-strong bg-neutral-surface text-neutral-fg px-1.5 py-0.5 text-[11px] font-medium whitespace-nowrap"
    >
      {label}
    </span>
  );
}
