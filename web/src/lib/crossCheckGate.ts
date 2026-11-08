// PRD #3708: the coarse result a checked-before-your-review run shows for its gate reason.
// Any other recorded reason means the check could not give a verdict: "Unavailable". The
// precise reason stays on the "Current gate" line of the plan cross-check panel.
const gatedResults: Record<string, string> = {
  worker_unsupported: "Unavailable, not supported on this worker",
  timed_out: "Timed out", model_timeout: "Timed out",
  block: "Blocked", revisions_exhausted: "Revisions exhausted",
  interrupted: "Interrupted", superseded: "Interrupted",
  revise: "Changes requested",
};

// The coarse result label for a non-empty plan cross-check gate reason.
export function planCrossCheckGateLabel(reason: string): string {
  return Object.prototype.hasOwnProperty.call(gatedResults, reason) ? gatedResults[reason] : "Unavailable";
}
