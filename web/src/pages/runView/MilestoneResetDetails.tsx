import type { Run } from "../../lib/apiTypes";
import { stripUnsafeChars } from "../../lib/safeText";

function text(value: unknown): string {
  return stripUnsafeChars(JSON.stringify(value, (_key, v: unknown) => typeof v === "string" ? stripUnsafeChars(v) : v) ?? "Unavailable");
}

export function MilestoneResetDetails({ run }: { run: Run }) {
  return (
    <div className="space-y-2 break-words text-sm">
      <p>Frozen milestone IDs and titles: {text(run.milestones)}</p>
      <p>Proposed milestone IDs and titles: {text(run.milestones_candidate)}</p>
      {Array.isArray(run.milestones_candidate) && run.milestones_candidate.length === 0 &&
        <p>Explicit empty replacement: no milestones will remain.</p>}
      <p>Completed: {text(run.milestones_completed)}; in progress: {text(run.milestones_in_progress)}</p>
      <p>Agents: {text(run.milestones_agents)}</p>
      <p>Scope ceiling: {text(run.scope_ceiling)}; stop decision: {text(run.stop_kind)} {text(run.stop_reason)}</p>
      <p>Previously accepted criteria: {text(run.completion_accepted)}</p>
      <p>Previously deferred criteria: {text(run.completion_deferred)}</p>
      <p>Reset clears progress and agents, scope ceiling and stop decisions, accepted and deferred criteria,
        and invalidates permits. Old decisions remain in the audit history. Reused milestone IDs do not retain completion.</p>
    </div>
  );
}
