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
    // warn implies at least one warn or unknown check. The verdict headline folds unknowns
    // into "warning" (the pips' healthPipLabel names unknown separately, since a pip has no
    // headline to lean on).
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

// healthPipLabel is the accessible name of the attention pips (the sidebar Admin item, the
// Admin > Health tab and the collapsed rail's sr-only text), PRD #1648 D9: "N health checks
// need attention: X danger, Y unknown, Z warning", naming only the non-zero severities,
// worst first. N = 1 reads "1 health check needs attention: ..." (singular noun and verb).
export function healthPipLabel(counts: HealthDoc["counts"]): string {
  const n = counts.danger + counts.unknown + counts.warn;
  const parts = (
    [
      [counts.danger, "danger"],
      [counts.unknown, "unknown"],
      [counts.warn, "warning"],
    ] as const
  )
    .filter(([c]) => c > 0)
    .map(([c, word]) => `${c} ${word}`);
  const head = n === 1 ? "1 health check needs attention" : `${n} health checks need attention`;
  return parts.length > 0 ? `${head}: ${parts.join(", ")}` : head;
}
