import type { HarnessDecision, Run } from "../lib/apiTypes";
import { stripDisplayCell } from "../lib/safeText";

/** Plain-language explanation of the automatic-default reasons, keyed on the chosen harness. */
function autoReasonText(decision: HarnessDecision): string | null {
  const { reason, chosen } = decision;
  const winner = chosen === "codex" ? "Codex" : "Claude";
  const loser = chosen === "codex" ? "Claude" : "Codex";
  switch (reason) {
    case "auto_soonest_reset": {
      // In automatic decisions the preferred side is Claude and the other side is Codex.
      const winnerReset = chosen === "codex" ? decision.other_weekly_reset_at : decision.preferred_weekly_reset_at;
      const loserReset = chosen === "codex" ? decision.preferred_weekly_reset_at : decision.other_weekly_reset_at;
      const winnerTime = winnerReset ? Date.parse(winnerReset) : Number.NaN;
      const loserTime = loserReset ? Date.parse(loserReset) : Number.NaN;
      // The server ranks a known future reset above a missing one, then the earlier reset.
      if (Number.isFinite(winnerTime) && Number.isNaN(loserTime)) {
        return `Chosen per run by weekly limits: only ${winner} had a known upcoming weekly reset`;
      }
      if (winnerTime < loserTime) {
        return `Chosen per run by weekly limits: ${winner}'s weekly allowance resets sooner than ${loser}'s`;
      }
      return `Both accounts had headroom; ${winner} chosen (no earlier known weekly reset, so more headroom, then Claude, decides)`;
    }
    case "auto_only_qualified":
      return `Chosen per run by weekly limits: only ${winner} had enough headroom`;
    case "auto_fallback":
      return "Claude used: usage was unknown for one account, or both were near their limits";
    default:
      return null;
  }
}

/**
 * Reasons recorded before any usage reading: every reading would show as unknown/no, so
 * the component says what happened instead. The stored reason does not say which
 * exclusion applied, so the text stays generic (docs/cli.md lists the causes).
 */
function unmeasuredText(reason: string): string | null {
  switch (reason) {
    case "off":
      return "Overflow is off in your settings, so no usage was compared";
    case "excluded":
      return "Overflow was not considered for this run, so no usage was compared";
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
  const unmeasured = unmeasuredText(decision.reason);
  const explanation = autoReasonText(decision) ?? unmeasured;
  const preferredLabel = auto ? "Claude" : "Preferred";
  const otherLabel = auto ? "Codex" : "Other harness";
  return (
    <section aria-label="Harness decision at creation time" className="space-y-1 text-xs text-muted">
      {auto
        ? <p>Harness decision at creation time: automatic (Default · automatic), chosen {stripDisplayCell(decision.chosen)}. Reason: {stripDisplayCell(decision.reason)}.</p>
        : <p>Harness decision at creation time: preferred {stripDisplayCell(decision.preferred)}, chosen {stripDisplayCell(decision.chosen)}. Reason: {stripDisplayCell(decision.reason)}.</p>}
      {explanation && <p>{explanation}.</p>}
      {!unmeasured && <>
        <p>{preferredLabel} comparison score: {score(decision.preferred_score)}; measured headroom: {score(decision.preferred_measured)}; capacity hold: {decision.preferred_held ? "yes" : "no"}; weekly reset: {resetText(decision.preferred_weekly_reset_at)}.</p>
        <p>{otherLabel} comparison score: {score(decision.other_score)}; measured headroom: {score(decision.other_measured)}; capacity hold: {decision.other_held ? "yes" : "no"}; weekly reset: {resetText(decision.other_weekly_reset_at)}.</p>
        <p>Stored when this run was created, not live usage. A capacity hold can reduce a comparison score without a measured limit reading.</p>
      </>}
    </section>
  );
}
