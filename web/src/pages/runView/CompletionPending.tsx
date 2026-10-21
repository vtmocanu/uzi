import { stripUnsafeChars } from "../../lib/safeText";
import { isCompletionPending } from "../../lib/semanticGate";

// Plain-text list of unverified maintainer criteria. Every value is a text node
// (React escapes it; stripUnsafeChars drops bidi and control characters) and the
// list is never interactive: nothing here is a checkbox or an approval.
function Lines({ label, items }: { label: string; items: string[] }) {
  return <div className="min-w-0">
    <dt className="text-muted">{label}</dt>
    {items.length === 0
      ? <dd className="text-muted">None listed.</dd>
      : <dd><ul className="list-disc space-y-1 pl-5">
        {items.map((item, i) => <li key={i} className="whitespace-pre-wrap break-words">{stripUnsafeChars(item)}</li>)}
      </ul></dd>}
  </div>;
}

export function CompletionPending({ run }: { run: { completion_pending?: unknown } }) {
  const value = run.completion_pending;
  if (value === undefined || value === null) return null;
  if (!isCompletionPending(value)) {
    return <section aria-label="Unverified maintainer criteria" className="rounded-md border border-edge p-3 text-sm text-muted">
      Pending maintainer criteria could not be read.
    </section>;
  }
  if (value.criteria.length === 0) return null;
  return <section aria-labelledby="completion-pending-heading" className="min-w-0 space-y-3 rounded-md border border-edge p-3 text-sm text-fg">
    <div>
      <h3 id="completion-pending-heading" className="font-semibold">Unverified maintainer criteria ({value.criteria.length})</h3>
      <p className="text-muted">These criteria are unverified. They grant no completion authority and nothing here marks them as met.</p>
    </div>
    <ul className="space-y-3">
      {value.criteria.map((c, i) => <li key={i} className="min-w-0 rounded-md border border-edge bg-surface p-3">
        <p className="break-words font-medium">
          <span className="font-mono text-xs">{stripUnsafeChars(c.criterion_id)}</span>
          {" "}(milestone <span className="font-mono text-xs">{stripUnsafeChars(c.milestone_id)}</span>) - Unverified
        </p>
        <p className="mt-1 whitespace-pre-wrap break-words">{stripUnsafeChars(c.text)}</p>
        <dl className="mt-2 space-y-2">
          <div className="min-w-0"><dt className="text-muted">Reason</dt>
            <dd className="whitespace-pre-wrap break-words">{stripUnsafeChars(c.reason)}</dd></div>
          <Lines label="Conditions" items={c.conditions} />
          <Lines label="Verification steps" items={c.verification_steps} />
        </dl>
      </li>)}
    </ul>
  </section>;
}
