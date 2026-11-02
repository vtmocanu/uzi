import type { PlanCrossCheckRound, Run, RunMessage } from "../../lib/apiTypes";
import { minsAgo, minsAhead } from "./time";
import { SAMPLE_PLAN } from "./plans";

// PRD #3708: plan cross-check "checked before your review". Two demo runs:
//  - run-gated-checked: three automatic rounds (revise, revise, approve), parked at the human
//    gate. Each candidate's "Cross-check dispositions" section answers ONLY the round before
//    it, so each round's response is that round's own, never an accumulated list.
//  - run-gated-checking: round 2 in flight, with a deadline, before any gate exists.

const round1Findings = {
  summary: "Two gaps in the delivery path.",
  items: [
    { file: "api/internal/notifier/email.go", severity: "warning", summary: "Delivery is not idempotent.",
      rationale: "A requeued run replays the parked transition and would send a second email." },
    { file: "api/internal/notifier", severity: "info", summary: "Templates read the raw issue title.",
      rationale: "Issue titles are untrusted forge text; escape them before rendering." },
  ],
};
const round2Findings = {
  summary: "One remaining concern.",
  items: [
    { file: "api/internal/store/queries/notify.sql", severity: "warning", summary: "The dedupe key ignores the gate revision.",
      rationale: "A revised plan re-parks at a new revision and should notify again." },
  ],
};

// Each candidate keeps only its preceding round's dispositions.
const round1Response = "- **Delivery is not idempotent:** addressed. Step 2 records a sent marker per run before sending.\n" +
  "- **Templates read the raw issue title:** declined. The templater already escapes every field.";
const round2Response = "- **The dedupe key ignores the gate revision:** addressed. The marker is keyed on (run, gate revision).";

const candidate = (dispositions: string) => `${SAMPLE_PLAN()}\n\n## Cross-check dispositions\n\n${dispositions}\n`;

// The presented plan is round 3's candidate: it answers round 2 and nothing earlier.
const GATED_PRESENTED_PLAN = candidate(round2Response);

const checkedRounds: PlanCrossCheckRound[] = [
  { round: 1, verdict: "revise", reason_class: "revise", findings: round1Findings, decided_at: minsAgo(48),
    response: { state: "reported", text: round1Response } },
  { round: 2, verdict: "revise", reason_class: "revise", findings: round2Findings, decided_at: minsAgo(31),
    response: { state: "reported", text: round2Response } },
  { round: 3, verdict: "approve", reason_class: "approve", findings: { summary: "No blocking issues.", items: null },
    decided_at: minsAgo(14), response: { state: "no_following_candidate", text: "" } },
];

const checker = {
  checker_run_id: null, checker_model: "gpt-6.1-sol", checker_effort: "high",
  checker_model_source: "worker default", checker_effort_source: "cross-check default",
} as const;

export function gatedCrossCheckRuns(base: Run): Run[] {
  return [
    {
      ...base,
      id: "run-gated-checked",
      issue_iid: 37,
      issue_title: "Notify owners when a revised plan re-parks",
      branch: null,
      plan_md: GATED_PRESENTED_PLAN,
      plan_cross_check_required: false,
      plan_cross_check_gated: true,
      plan_cross_check_gate_reason: null,
      plan_cross_check_summary: {
        ...checker, round: 3, verdict: "approve", reason_class: "approve",
        findings: checkedRounds[2].findings,
        usage: { input_tokens: 2100, output_tokens: 900, cache_read_tokens: 4200, cache_creation_tokens: 300, cost_usd: 0.0188, cost_status: "metered" },
        historical: false,
        rounds: checkedRounds,
      },
      status_since: minsAgo(14),
      created_at: minsAgo(60),
      updated_at: minsAgo(14),
    },
    {
      ...base,
      id: "run-gated-checking",
      status: "running",
      issue_iid: 38,
      issue_title: "Digest notifications for parked runs",
      branch: "agent/issue-38",
      plan_md: null,
      plan_cross_check_required: false,
      plan_cross_check_gated: true,
      plan_cross_check_gate_reason: null,
      plan_cross_check_summary: {
        ...checker, round: 2, verdict: "pending", reason_class: null, findings: null, usage: null,
        historical: false,
        deadline_at: minsAhead(24),
        rounds: [
          { ...checkedRounds[0], decided_at: minsAgo(9) },
          { round: 2, verdict: "pending", reason_class: null, findings: null, decided_at: null,
            response: { state: "no_following_candidate", text: "" } },
        ],
      },
      status_since: minsAgo(6),
      created_at: minsAgo(20),
      updated_at: minsAgo(6),
    },
  ];
}

let seq = 0;
const gm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++seq, kind, agent, agent_instance: null, agent_label: null, payload, created_at: minsAgo(minAgo),
});

// The gated run's feed: three candidates and their checks, then the human gate.
export const mockGatedCheckedMessages: RunMessage[] = [
  gm("status", null, { event: "init", model: "claude-sonnet-4-6" }, 59),
  gm("plan", "lead", { text: SAMPLE_PLAN() }, 52),
  gm("cross_check", null, { stage: "plan", round: 1, verdict: "revise", reason_class: "revise", findings: round1Findings, checker_run_id: null }, 48),
  gm("plan", "lead", { text: candidate(round1Response) }, 35),
  gm("cross_check", null, { stage: "plan", round: 2, verdict: "revise", reason_class: "revise", findings: round2Findings, checker_run_id: null }, 31),
  gm("plan", "lead", { text: GATED_PRESENTED_PLAN }, 18),
  gm("cross_check", null, { stage: "plan", round: 3, verdict: "approve", reason_class: "approve", checker_run_id: null }, 14),
  gm("status", null, { text: "plan cross-check: passed after 3 rounds" }, 14),
];
