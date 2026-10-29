import { Link } from "react-router-dom";

import type { HealthEvidence } from "../lib/apiTypes";

function validID(id: string | undefined): id is string {
  return !!id && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(id);
}

export function HealthEvidenceValue({ evidence }: { evidence: HealthEvidence }) {
  return validID(evidence.owner_id)
    ? <span title={`Owner ${evidence.owner_id}`}>{evidence.value}</span>
    : <>{evidence.value}</>;
}

// Old health documents carry only label/value and remain useful without a link.
export function HealthEvidenceLabel({ evidence }: { evidence: HealthEvidence }) {
  if (!validID(evidence.run_id)) {
    return <>{evidence.label}</>;
  }
  return (
    <Link
      to={`/runs/${encodeURIComponent(evidence.run_id)}`}
      title={`Run ${evidence.run_id}`}
      className="text-info hover:underline"
    >
      Run {evidence.run_id.slice(0, 8)}
    </Link>
  );
}
