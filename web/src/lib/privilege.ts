// Presentation helpers for the PRD #5 privilege surfacing: mapping a connection's
// privilege state to a badge, counting findings, and pulling one repo's findings
// out of a report. Pure — the pages own the rendering.

import type { PrivilegeFinding, PrivilegeReport, PrivilegeStatus } from "./api";
import type { BadgeTone } from "../components/ui";

export interface PrivilegeBadge {
  tone: BadgeTone;
  label: string;
}

// The kind of finding a caller wants counted/rendered: "violations" are the
// blocking tier, "warnings" the advisory one. Per-repo findings now carry a coded
// severity (PRD #66 D5), so "violations" maps to severity "block" and "warnings"
// to "warn"; the token half still uses its own violations/warnings string slices.
type FindingKind = "violations" | "warnings";

const SEVERITY_FOR: Record<FindingKind, PrivilegeSeverityValue> = {
  violations: "block",
  warnings: "warn",
};

type PrivilegeSeverityValue = PrivilegeFinding["severity"];

// countFindings totals the token findings plus every repo's coded findings of the
// given kind (block→violations, warn→warnings) across a report.
export function countFindings(
  report: PrivilegeReport | null,
  kind: FindingKind,
): number {
  if (!report) return 0;
  const wantSeverity = SEVERITY_FOR[kind];
  let n = report.token[kind].length;
  for (const r of report.repos) {
    n += r.findings.filter((finding) => finding.severity === wantSeverity).length;
  }
  return n;
}

// privilegeBadge maps a connection's status (+ report for counts) to a badge.
// A null status is the grandfathered/never-checked state — rendered "unchecked",
// never as a ✓.
export function privilegeBadge(
  status: PrivilegeStatus | null,
  report: PrivilegeReport | null,
): PrivilegeBadge {
  switch (status) {
    case "ok":
      return { tone: "ok", label: "least-privilege ✓" };
    case "warnings": {
      const n = countFindings(report, "warnings");
      return { tone: "warning", label: `${n} warning${n === 1 ? "" : "s"}` };
    }
    case "violations": {
      const n = countFindings(report, "violations");
      return { tone: "danger", label: `${n} violation${n === 1 ? "" : "s"}` };
    }
    case "error":
      return { tone: "danger", label: "check failed" };
    default:
      return { tone: "neutral", label: "unchecked" };
  }
}

// repoFindings returns one repo's coded findings from a report split by severity
// (block→violations, warn→warnings), or null when the repo has no entry or no
// findings (so the Repos page badges only the repos that need attention).
export function repoFindings(
  report: PrivilegeReport | null,
  repoId: string,
): { violations: PrivilegeFinding[]; warnings: PrivilegeFinding[] } | null {
  const r = report?.repos.find((x) => x.repo_id === repoId);
  if (!r || r.findings.length === 0) return null;
  const violations = r.findings.filter((finding) => finding.severity === "block");
  const warnings = r.findings.filter((finding) => finding.severity === "warn");
  return { violations, warnings };
}
