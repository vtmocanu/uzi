// The mock server's RunDTO.progress (PRD #2602). The mock store plays the server, so it
// derives `progress` the way runToDTO does (api/internal/runprogress) — on seed and on every
// patch — rather than fixtures carrying a frozen value: the engine walks runs through
// planning → approval → implement → completed, and a static value would show a percent on
// a run the engine has since finished. Same stance as status_since in store.ts.
//
// This is a FAKE-SERVER copy of the rules for demo data only; product code never derives
// progress (PRD D3). The Go package is the source of truth; if the two drift, the mock is
// wrong. A fixture that sets `progress` explicitly keeps it (seedRun only fills undefined).
import type { Run, RunProgress } from "../lib/apiTypes";

const TERMINAL = new Set(["completed", "failed", "cancelled"]);
const WAITING = new Set(["awaiting_approval", "awaiting_input", "awaiting_followup"]);
const PARKED = new Set(["limit_wait", "pool_wait", "recovery_wait", "paused"]);
const QUEUED = new Set(["queued", "claimed"]);
const REVIEW_ROLES = new Set(["reviewer", "auditor", "fact-checker", "architect", "web-ux", "tui-ux", "dba"]);

function phaseOf(agent: string | undefined): RunProgress["phase"] {
  if (!agent) return "";
  if (REVIEW_ROLES.has(agent)) return "review";
  if (agent === "tester") return "validate";
  return "implement";
}

export function deriveMockProgress(run: Run): RunProgress | null {
  if (TERMINAL.has(run.status)) return null;
  const frozen = run.milestones ?? [];
  const completed = new Set(run.milestones_completed ?? []);
  const inProgress = new Set(run.milestones_in_progress ?? []);
  const done = frozen.filter((m) => completed.has(m.id)).length;
  const total = frozen.length;
  const active = frozen.find((m) => inProgress.has(m.id) && !completed.has(m.id))?.id ?? "";
  const base = {
    pct: null,
    milestone_done: done,
    milestone_total: total,
    active_milestone_id: active,
    phase: phaseOf(run.current_activity?.agent),
  };
  if (WAITING.has(run.status)) return { ...base, state: "waiting" };
  if (PARKED.has(run.status)) return { ...base, state: "parked" };
  if (QUEUED.has(run.status)) return { ...base, state: "queued" };
  if (run.health === "stalled" || run.health === "looping") return { ...base, state: "stalled" };
  if (run.is_planning === true) return { ...base, state: "planning" };
  if (run.kind !== "issue" || total === 0) return { ...base, state: "none" };
  const percent: RunProgress = { ...base, state: "percent", pct: Math.min(99, Math.floor(11 + (89 * done) / total)) };
  // PRD #2603: only the run-live demo carries a model-written Now line, so the parity
  // fixtures (which never use that id) still compare equal.
  if (run.id === "run-live") {
    percent.now_note = {
      text: "Running the heartbeat tests for the active milestone",
      at: new Date(Date.now() - 2 * 60_000).toISOString(),
    };
  }
  return percent;
}
