import { stripUnsafeChars } from "../../lib/safeText";
import type { SemanticDriftObservation, SemanticDriftSource } from "../../lib/apiTypes";
import { isSemanticDrift } from "../../lib/semanticGate";

// Plain-text record of source drift observations (PRD #1230 D5). A detector's log: every value is a
// text node (React escapes it; stripUnsafeChars drops bidi and control characters), nothing here is
// interactive, and nothing here says a criterion is met or changes the approved contract.
const PHASE: Record<SemanticDriftObservation["phase"], string> = {
  presentation: "When the plan was presented for approval",
  publication: "When the work was published",
};

function sourceLine(s: SemanticDriftSource, label?: string): string {
  const parts: string[] = [s.outcome];
  if (s.reason) parts.push(s.reason);
  if (s.checkbox_only) parts.push("checkbox state only");
  return `${label ?? s.source_id}: ${parts.join(", ")}`;
}

function Links({ label, items }: { label: string; items: string[] }) {
  if (items.length === 0) return null;
  return <p className="break-words">{label}: <span className="font-mono text-xs">{items.map(stripUnsafeChars).join(", ")}</span></p>;
}

export function SemanticDrift({ run }: { run: { semantic_drift?: unknown } }) {
  const value = run.semantic_drift;
  if (value === undefined || value === null) return null;
  if (!isSemanticDrift(value)) {
    return <section aria-label="Source drift observations" className="rounded-md border border-edge p-3 text-sm text-muted">
      Source drift observations could not be read.
    </section>;
  }
  if (value.observations.length === 0 && value.dropped === 0 && !value.contradiction) return null;
  return <section aria-labelledby="semantic-drift-heading" className="min-w-0 space-y-3 rounded-md border border-edge p-3 text-sm text-fg">
    <div>
      <h3 id="semantic-drift-heading" className="font-semibold">Source drift observations ({value.observations.length})</h3>
      <p className="text-muted">
        The issue and its linked PRDs were compared with what was approved. This is a record only: the approved contract is
        unchanged and nothing here marks a criterion as met.
      </p>
    </div>
    {value.contradiction && <p className="rounded-md border border-warn/40 bg-warn/10 p-2 font-medium text-warn">
      Contradiction: owner review needed. A source changed or could not be checked after approval.
    </p>}
    {value.dropped > 0 && <p className="text-muted">{value.dropped} older observation{value.dropped === 1 ? "" : "s"} no longer kept.</p>}
    <ul className="space-y-3">
      {value.observations.map((o, i) => <li key={i} className="min-w-0 rounded-md border border-edge bg-surface p-3">
        <p className="font-medium">
          {PHASE[o.phase]}
          {" "}<span className="font-normal text-muted">({stripUnsafeChars(o.observed_at)})</span>
          {o.pushed_sha && <>{" "}<span className="font-mono text-xs">{stripUnsafeChars(o.pushed_sha.slice(0, 12))}</span></>}
        </p>
        <ul className="mt-1 list-disc space-y-1 pl-5 break-words">
          <li>{stripUnsafeChars(sourceLine(o.issue, "Issue " + o.issue.source_id))}</li>
          {o.prds.map((p, j) => <li key={j}>{stripUnsafeChars(sourceLine(p))}</li>)}
        </ul>
        <div className="mt-2 space-y-1">
          <Links label="PRD links added to the issue" items={o.links_added} />
          <Links label="PRD links removed from the issue" items={o.links_removed} />
          {o.links_truncated && <p className="text-muted">Some PRD links were not compared.</p>}
        </div>
      </li>)}
    </ul>
  </section>;
}
