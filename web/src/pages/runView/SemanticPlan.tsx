import { useState } from "react";
import { stripUnsafeChars } from "../../lib/safeText";
import { decodeSemanticText, type SemanticGateDTO } from "../../lib/semanticGate";

// Decision log: reuse PlanPanel's tokens and Plex type; left-aligned criteria
// disclosures retain the entire ledger. Quoted frozen text is the signature:
// it keeps source newlines from impersonating trusted labels, without altering
// a byte of the approval echo. Native disclosures need no motion or new settings.
function quoted(value: unknown): string {
  return stripUnsafeChars(JSON.stringify(value, null, 2) ?? "");
}
function Data({ label, value }: { label: string; value: unknown }) {
  return <div className="min-w-0"><dt className="text-muted">{label}</dt>
    <dd className="whitespace-pre-wrap break-words font-mono text-xs">{quoted(value)}</dd></div>;
}
function FrozenText({ label, raw }: { label: string; raw: string }) {
  const [expanded, setExpanded] = useState(false);
  return <details onToggle={e => setExpanded(e.currentTarget.open)} className="min-w-0 rounded-md border border-edge p-3">
    <summary className="cursor-pointer text-sm font-medium focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring">{label}</summary>
    {expanded && <pre className="mt-2 whitespace-pre-wrap break-words text-xs text-fg">{quoted(decodeSemanticText(raw))}</pre>}
  </details>;
}
export function SemanticPlan({ gate }: { gate: SemanticGateDTO }) {
  const p = gate.presentation;
  const c = p.candidate;
  const coverage = p.coverage;
  return <section aria-label="Semantic plan review" className="min-w-0 space-y-4 border-b border-edge pb-4 text-sm text-fg">
    <div>
      <h3 className="font-semibold">Semantic plan review</h3>
      <p className="text-muted">Review the frozen inputs, criteria and coverage before approving this candidate.</p>
      <p className="mt-2 font-medium text-warn">Owner decision required. Coverage: {stripUnsafeChars(coverage.outcome)}.</p>
      <p className="text-muted">Maintainer authorization is pending. Approval authorizes the listed conditions; verification has not been performed.</p>
    </div>
    <dl className="grid min-w-0 gap-3 sm:grid-cols-2">
      <Data label="Candidate identity" value={{ version: c.candidate_identity_version, revision: c.revision, digest: c.digest, plan_hash: c.plan_hash, plan_fields_digest: c.plan_fields_digest, source_outcome: c.source_outcome }} />
      <Data label="Exact owner approval identity and authorizations" value={gate.approval} />
      <Data label="Report identity and binding" value={{ ...p.binding, claim_generation: p.claim_generation, review_id: p.review_id, invocation_id: p.invocation_id, report_digest: p.report_digest }} />
      <Data label="Coverage budget" value={{ consumed_tokens: coverage.charged_tokens, remaining_tokens: coverage.remaining_tokens, consumed_seconds: p.consumed_seconds, remaining_seconds: p.remaining_seconds, consumed_reviews: coverage.reviews, remaining_reviews: p.remaining_reviews, invocations: coverage.invocations, deadline: coverage.deadline }} />
      <Data label="Candidate coverage" value={c.coverage ?? "Unavailable"} />
    </dl>
    <div className="space-y-2">
      <h4 className="font-medium">Criteria ({c.criteria.length})</h4>
      {c.criteria.map((criterion, i) => <details key={i} className="min-w-0 rounded-md border border-edge bg-surface p-3">
        <summary className="cursor-pointer break-words font-medium focus-visible:outline focus-visible:outline-2 focus-visible:outline-ring">{quoted(criterion.id)}: {quoted(criterion.text)}</summary>
        <dl className="mt-3 space-y-3">
          <Data label="Milestone" value={criterion.milestone_id} />
          <Data label="Kind" value={criterion.kind} />
          <Data label="Done when" value={criterion.done_when} />
          <Data label="Evidence requirements and paths" value={criterion.evidence_requirements} />
          <Data label="Citations (byte offsets)" value={criterion.citations} />
          {criterion.maintainer && <Data label="Pending maintainer authorization: reason, conditions and verification steps" value={criterion.maintainer} />}
        </dl>
      </details>)}
    </div>
    <div className="space-y-2">
      <h4 className="font-medium">Frozen sources</h4>
      {c.sources.map((ref, i) => <div key={i} className="min-w-0 space-y-2">
        <dl><Data label="Source identity, type, path/ref, hash and completeness" value={ref} /></dl>
        <FrozenText label={`Read complete source ${i + 1} (quoted)`} raw={gate.sources.find(source => source.id === ref.id)!.bytes} />
      </div>)}
      <FrozenText label="Read complete frozen plan (quoted)" raw={gate.plan} />
    </div>
    <div className="space-y-3">
      <h4 className="font-medium">Coverage report</h4>
      {!coverage.report ? <p className="text-warn">Coverage report unavailable. Owner decision required.</p> : <>
        <dl><Data label="Report binding" value={{ candidate_digest: coverage.report.candidate_digest, source_digest: coverage.report.source_digest, criteria_digest: coverage.report.criteria_digest }} /></dl>
        {coverage.report.requirements.map((result, i) => <dl key={i} className="border-l-2 border-edge pl-3"><Data label="Requirement result, explanation, uncertainties and citations (byte offsets)" value={result} /></dl>)}
        {coverage.report.criteria.map((result, i) => <dl key={i} className="border-l-2 border-edge pl-3"><Data label="Criterion atomicity and explanation" value={result} /></dl>)}
      </>}
      <dl><Data label="Pending maintainer authorizations" value={p.maintainers} /></dl>
    </div>
  </section>;
}
