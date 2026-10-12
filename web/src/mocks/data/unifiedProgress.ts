import type { Run, RunMessage } from "../../lib/api";
import { mockRuns } from "./runs";
import { mockDoneMessages, mockFailedMessages, demoTailEstimated } from "./runHistories";
import { mockAwaitingMessages } from "./runHistories";
import { mockScenario } from "../mockScenario";
import { minsAgo, secsAgo } from "./time";

// ?mock=run-progress seeds running, approval, failed, completed and assigned-owner
// examples. Each uses the current Run contract; no shipping component branches on it.
export function seedUnifiedProgress(runs: Map<string, Run>, messages: Map<string, RunMessage[]>) {
  if (mockScenario() !== "run-progress") return;
  const frozen = [
    {id: "m6", title: "Review the held-publication implementation and fix findings"},
    {id: "m7", title: "Add the owner web view and configuration guidance"},
    {id: "m8", title: "Integrate the latest main branch"},
    {id: "m9", title: "Run-recovery and CLI docs, ADR and final validation"},
  ];
  const base = mockRuns.find(r => r.id === "run-done")!;
  for (const state of ["running", "approval", "failed", "completed", "assigned"] as const) {
    const id = `run-progress-${state}`;
    const active = state === "running" || state === "assigned";
    const approval = state === "approval";
    const run: Run = {
      ...base, id, issue_iid: 2789, issue_title: state === "assigned" ? "Assigned owner, other agents active" : "Publish held work safely, then release custody",
      status: approval ? "awaiting_approval" : active ? "running" : state,
      is_planning: false, is_revising: false, auto_approve: false, iteration_count: active ? 4 : 0,
      health: "ok", health_since: null, health_reason: null,
      branch: "agent/issue-2789", model: base.model, override_subagent_model: false,
      plan_cross_check_required: false, plan_cross_check_summary: undefined,
      plan_cross_check_gate_reason: undefined,
      anthropic_select_reason: "pinned", anthropic_headroom_pct: null,
      mr_iid: state === "completed" ? 2890 : null, mr_state: state === "completed" ? "open" : null,
      mr_web_url: state === "completed" ? "https://github.com/example/uzi/pull/2890" : null,
      issue_web_url: "https://github.com/example/uzi/issues/2789", forge_type: "github",
      failure_reason: state === "failed" ? "Final validation could not complete; held work is retained for recovery." : null,
      stop_reason: null, stop_kind: null,
      milestones: approval ? null : frozen, milestones_candidate: approval ? frozen : null,
      milestones_completed: approval ? null : state === "completed" ? frozen.map(m => m.id) : ["m6", "m7", "m8"],
      milestones_in_progress: active || state === "failed" ? ["m9"] : [],
      milestones_agents: active ? [{id: "m9", agent: state === "assigned" ? "architect" : "documenter", agent_label: state === "assigned" ? "Coordinate the final review" : "Run-recovery and CLI docs"}] : [],
      milestones_live: active ? [{milestone_id: "m9", lanes: [
        {agent: "documenter", agent_instance: "progress-doc", agent_label: "Run-recovery and CLI docs", tool: "Bash", detail: "Check CLI guidance", at: secsAgo(3)},
        {agent: "spec-keeper", agent_instance: "progress-spec", agent_label: "ADR and spec supersessions", tool: "Edit", detail: "Update the durable decision", at: secsAgo(6)},
      ]}] : [],
      current_activity: active ? {agent: "documenter", agent_label: "Run-recovery and CLI docs", tool: "Bash", detail: "Check CLI guidance", at: secsAgo(3), seq: 100} : null,
      progress: approval ? {state: "waiting", pct: null, milestone_done: 0, milestone_total: 0, active_milestone_id: "", phase: ""} : active ? {
        state: "percent", pct: 77, milestone_done: 3, milestone_total: 4, active_milestone_id: "m9", phase: "implement",
        now_note: {text: "Working on the run-recovery and CLI docs.", at: secsAgo(35)},
      } : null,
      summary_intent: "Publish a failed run's unpublished work to a forge-backed ref after a fresh secret scan, keeping archives as fallback.",
      summary_plan: "Complete the owner view, integrate main, document recovery and validate the full implementation.",
      summary_deltas: [{kind: "added", text: "An owner view of held publication details."}],
      usage_estimated_tail: active || state === "failed" ? demoTailEstimated : null,
      started_at: minsAgo(151), first_started_at: minsAgo(151), claimed_at: minsAgo(151),
      finished_at: active || approval ? null : minsAgo(2), status_since: active ? minsAgo(151) : minsAgo(14),
      budget_wall_seconds: 86400, budget_total_seconds: 86400, budget_used_seconds: 9060,
      budget_max_iterations: 8, deadline_at: new Date(Date.now() + 77340_000).toISOString(),
      wait_on_limit: active, mr_rework_enabled: false,
      credential_epochs: state === "assigned" ? [{claim_generation: 1, secret_id: "sec-console", label: "console-key", select_reason: "pinned", applied_at: minsAgo(151)}] : [
        {claim_generation: 1, secret_id: "sec-default", label: "default", select_reason: "default", applied_at: minsAgo(151)},
        {claim_generation: 2, secret_id: "sec-console", label: "console-key", select_reason: "pinned", applied_at: minsAgo(90)},
      ], credential_override: null, credential_switch: null,
    };
    runs.set(id, run);
    const log = (approval ? mockAwaitingMessages : state === "failed" ? mockFailedMessages : mockDoneMessages).map(m => ({...m}));
    if (active) log.push({seq: log.length + 1, kind: "tool_use", agent: "documenter", agent_instance: "progress-doc", agent_label: "Run-recovery and CLI docs", payload: {name: "Bash", input: {description: "Check CLI guidance"}}, created_at: secsAgo(3)});
    messages.set(id, log);
  }
}
