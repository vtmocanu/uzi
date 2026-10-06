import type { HealthCheck, HealthDoc } from "./api";

// Shared, presentation-free derivations over a HealthDoc (PRD #1484). The Health page, the
// Overview admin card and the app-wide danger banner all read the SAME verdict wording and
// the SAME attention ordering from here, so the three surfaces can never disagree about what
// the document says. HealthDoc carries no verdict or cause field — those are derived from the
// document and scoped checks so the copy distinguishes owner attention from instance blockers.

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

// Present blocking is authoritative. An older server omitting it retains the conservative
// status/tally/first-danger presentation, including the banner's snooze expiry timer.
export function healthVerdict(doc: HealthDoc | Omit<HealthDoc, "blocking">): { title: string; sub: string; blocking: boolean; cause: HealthCheck | undefined } {
  const legacy = !("blocking" in doc);
  const dangers = doc.checks.filter((c) => c.severity === "danger" && (legacy || c.scope === "instance"));
  const blocking = legacy ? doc.status === "danger" : doc.blocking;
  const cause = dangers[0];
  if (blocking) {
    const d = legacy ? doc.counts.danger : dangers.length;
    return {
      title: `uzi cannot run work: ${d} blocking ${d === 1 ? "check" : "checks"}`,
      sub: "Work is blocked until these clear. Start with the flagged checks below.",
      blocking,
      cause,
    };
  }
  if (doc.status === "danger" || doc.status === "warn" || doc.status === "unknown") {
    const n = doc.checks.filter(isAttention).length;
    return {
      title: `${n} ${n === 1 ? "check needs" : "checks need"} attention${doc.status === "danger" ? "; no instance-wide blocker detected" : ""}`,
      sub: "Review the flagged checks below for evidence and what to do.",
      blocking,
      cause,
    };
  }
  return {
    title: "All systems normal",
    sub: "Every check uzi can make about itself is passing.",
    blocking,
    cause,
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
