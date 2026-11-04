import type { Run } from "../lib/apiTypes";
import { stripDisplayCell } from "../lib/safeText";

/** Plain-language explanation of the automatic-default reasons, keyed on the chosen harness. */
function autoReasonText(reason: string, chosen: string): string | null {
  const winner = chosen === "codex" ? "Codex" : "Claude";
  const loser = chosen === "codex" ? "Claude" : "Codex";
  switch (reason) {
    case "auto_soonest_reset":
      return `chosen per run by weekly limits: ${winner}'s weekly allowance resets sooner than ${loser}'s`;
    case "auto_only_qualified":
      return `chosen per run by weekly limits: only ${winner} had enough headroom`;
    case "auto_fallback":
      return "Claude used: usage was unknown for one account, or both were near their limits";
    default:
      return null;
  }
}

/** Weekly reset as a UTC date-time, "unknown" when absent (null, or rows created before resets were recorded). */
function resetText(value: string | null | undefined): string {
  if (!value) return "unknown";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return stripDisplayCell(value);
  return `${parsed.toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

/** Displays the stored decision independently of claim-time credential metadata. */
export function RunHarnessDecision({ run }: { run: Pick<Run, "harness_decision"> }) {
  const decision = run.harness_decision;
  if (!decision) {
    return <p className="text-xs text-faint">Harness decision at creation time: not recorded (legacy or unavailable).</p>;
  }
  const score = (value: number | null) => value === null ? "unknown" : `${value}%`;
  const auto = decision.reason.startsWith("auto_");
  const explanation = autoReasonText(decision.reason, decision.chosen);
  const preferredLabel = auto ? "Claude" : "Preferred";
  const otherLabel = auto ? "Codex" : "Other harness";
  return (
    <section aria-label="Harness decision at creation time" className="space-y-1 text-xs text-muted">
      <p>Harness decision at creation time: preferred {stripDisplayCell(decision.preferred)}, chosen {stripDisplayCell(decision.chosen)}. Reason: {stripDisplayCell(decision.reason)}.</p>
      {explanation && <p>{explanation}.</p>}
      <p>{preferredLabel} comparison score: {score(decision.preferred_score)}; measured headroom: {score(decision.preferred_measured)}; capacity hold: {decision.preferred_held ? "yes" : "no"}; weekly reset: {resetText(decision.preferred_weekly_reset_at)}.</p>
      <p>{otherLabel} comparison score: {score(decision.other_score)}; measured headroom: {score(decision.other_measured)}; capacity hold: {decision.other_held ? "yes" : "no"}; weekly reset: {resetText(decision.other_weekly_reset_at)}.</p>
      <p>Stored when this run was created, not live usage. A capacity hold can reduce a comparison score without a measured limit reading.</p>
    </section>
  );
}
