import type { HealthCheck, HealthDoc } from "./api";

// Shared, presentation-free derivations over a HealthDoc (PRD #1484). The Health page, the
// Overview admin card and the app-wide danger banner all read the SAME verdict wording and
// the SAME attention ordering from here, so the three surfaces can never disagree about what
// the document says. HealthDoc carries no verdict or cause field — those are derived from the
// severity tally so the copy stays true for any danger/warn cause.

// Attention = every non-ok, non-na check. na cannot apply on this deployment and ok is
// passing; neither is attention.
export function isAttention(c: HealthCheck): boolean {
  return c.severity !== "ok" && c.severity !== "na";
}

// Worst-first rank for the attention list (danger, then unknown, then warn). Internal: the
// only consumer is attentionChecks below.
const RANK: Record<string, number> = { danger: 0, unknown: 1, warn: 2 };

// attentionChecks returns the doc's non-ok/non-na checks, worst first.
export function attentionChecks(doc: HealthDoc): HealthCheck[] {
  return doc.checks.filter(isAttention).sort((a, b) => (RANK[a.severity] ?? 9) - (RANK[b.severity] ?? 9));
}

// healthVerdict is the headline + sub the verdict card, the overview card and the banner
// share. Derived from `counts` ONLY, so the copy stays true for any danger/warn cause rather
// than describing one scenario. Punctuation follows the mock (comma for the warn line, colon
// for the danger one).
export function healthVerdict(status: string, counts: HealthDoc["counts"]): { title: string; sub: string } {
  if (status === "danger") {
    // danger implies counts.danger > 0. Warns present alongside are not blocking, so the
    // headline counts only the blocking (danger) checks.
    const d = counts.danger;
    return {
      title: `uzi cannot run work: ${d} blocking ${d === 1 ? "check" : "checks"}`,
      sub: "Work is blocked until these clear. Start with the flagged checks below.",
    };
  }
  if (status === "warn" || status === "unknown") {
    // warn implies at least one warn or unknown check. The aggregate signal is worded
    // "warning" everywhere (the tab pip's aria-label too), so unknowns fold in here.
    const n = counts.warn + counts.unknown;
    return {
      title: `${n} ${n === 1 ? "warning" : "warnings"}, nothing is blocked`,
      sub: "Work is still flowing. These are the quiet no-ops that otherwise only surface as a log line.",
    };
  }
  return {
    title: "All systems normal",
    sub: "Every check uzi can make about itself is passing.",
  };
}
