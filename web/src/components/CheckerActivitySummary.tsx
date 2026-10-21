import { useMemo } from "react";
import type { Run, RunMessage } from "../lib/apiTypes";
import { checkActivityState } from "../lib/checkActivity";

/**
 * Cross-check activity evidence (PRD #3166): one line saying what the checker run read and
 * searched, derived from the run's own status messages. Renders only for a cross_check run and
 * only when there is something to say; paths and counts reach the page as plain text.
 */
export function CheckerActivitySummary({ run, messages }: { run: Run; messages: readonly RunMessage[] }) {
  const line = useMemo(
    () => (run.kind === "cross_check" ? checkActivityState(run, messages) : undefined),
    [run, messages],
  );
  if (line === undefined) return null;
  return (
    <p data-testid="checker-activity-summary" className="text-xs text-muted">
      <span className="font-medium text-fg">Checker activity:</span> {line}
    </p>
  );
}
