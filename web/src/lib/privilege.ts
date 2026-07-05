// Presentation helpers for the PRD #5 privilege surfacing: mapping a connection's
// privilege state to a badge, counting findings, and pulling one repo's findings
// out of a report. Pure — the pages own the rendering.

import type { PrivilegeReport, PrivilegeStatus } from "./api";
import type { BadgeTone } from "../components/ui";

export interface PrivilegeBadge {
  tone: BadgeTone;
  label: string;
}

// countFindings totals the token findings plus every repo's findings of the
// given kind across a report.
export function countFindings(
  report: PrivilegeReport | null,
  kind: "violations" | "warnings",
): number {
  if (!report) return 0;
  let n = report.token[kind].length;
  for (const r of report.repos) n += r[kind].length;
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

// repoFindings returns one repo's findings from a report, or null when the repo
// has no entry or no findings (so the Repos page badges only the repos that need
// attention).
export function repoFindings(
  report: PrivilegeReport | null,
  repoId: string,
): { violations: string[]; warnings: string[] } | null {
  const r = report?.repos.find((x) => x.repo_id === repoId);
  if (!r || (r.violations.length === 0 && r.warnings.length === 0)) return null;
  return { violations: r.violations, warnings: r.warnings };
}
