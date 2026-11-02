import type { Run } from "../lib/apiTypes";
import { stripDisplayCell } from "../lib/safeText";

/** Displays the stored decision independently of claim-time credential metadata. */
export function RunHarnessDecision({ run }: { run: Pick<Run, "harness_decision"> }) {
  const decision = run.harness_decision;
  if (!decision) {
    return <p className="text-xs text-faint">Harness decision at creation time: not recorded (legacy or unavailable).</p>;
  }
  const score = (value: number | null) => value === null ? "unknown" : `${value}%`;
  return (
    <section aria-label="Harness decision at creation time" className="space-y-1 text-xs text-muted">
      <p>Harness decision at creation time: preferred {stripDisplayCell(decision.preferred)}, chosen {stripDisplayCell(decision.chosen)}. Reason: {stripDisplayCell(decision.reason)}.</p>
      <p>Preferred comparison score: {score(decision.preferred_score)}; measured headroom: {score(decision.preferred_measured)}; capacity hold: {decision.preferred_held ? "yes" : "no"}.</p>
      <p>Other harness comparison score: {score(decision.other_score)}; measured headroom: {score(decision.other_measured)}; capacity hold: {decision.other_held ? "yes" : "no"}.</p>
      <p>Stored when this run was created, not live usage. A capacity hold can reduce a comparison score without a measured limit reading.</p>
    </section>
  );
}
