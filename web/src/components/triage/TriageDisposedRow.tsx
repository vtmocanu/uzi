import { formatElapsed } from "../../lib/runBadge";
import { Badge } from "../ui";

// resolvedAgo renders a disposition's resolve time as a coarse "resolved Xh ago", lifted from
// the run panel's DispositionControls. It reuses the app's one duration helper (formatElapsed,
// runBadge) so the wording matches the running badge and every other elapsed string. The panel
// shows only a relative time, never the actor — under owner-only scope the setter is always the
// owner. An unparseable timestamp degrades to a bare "resolved".
function resolvedAgo(at: string | Date): string {
  const t = at instanceof Date ? at.getTime() : Date.parse(at);
  if (!Number.isFinite(t)) return "resolved";
  return `resolved ${formatElapsed(Date.now() - t)} ago`;
}

// TriageDisposedRow is the run page's disposed state — the row a settled recommendation shows
// in place of the action row (PRD #1183). It survives the swap to the shared triage set
// unchanged: "resolved Xh ago", the server-computed `stale` badge, and an Undo.
//
// `stale` is a DIFFERENT signal from the filed-link staleness — it is the server's
// rationale-hash compare (the judge re-ran and this recommendation changed since you resolved
// it), so it is RENDERED, never recomputed in the browser (the client never sees a hash). Undo
// reverts the disposition; the caller owns what happens after (the refetch and the successor
// focus).
export function TriageDisposedRow({
  resolvedAt,
  stale = false,
  onUndo,
  busy = false,
}: {
  resolvedAt: string | Date;
  stale?: boolean;
  onUndo: () => void;
  busy?: boolean;
}) {
  return (
    <div className="mt-2 flex flex-wrap items-center gap-2 text-xs">
      <span className="text-faint">{resolvedAgo(resolvedAt)}</span>
      {stale && (
        <Badge
          tone="warning"
          title="The judge re-ran and this recommendation's rationale changed since you resolved it."
        >
          recommendation changed since you resolved
        </Badge>
      )}
      <button
        type="button"
        disabled={busy}
        onClick={onUndo}
        className="font-medium text-faint underline underline-offset-2 transition-colors hover:text-fg disabled:opacity-50"
      >
        Undo
      </button>
    </div>
  );
}
