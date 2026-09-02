import type {
  Run,
  RunListItem,
  RunMessage,
  RunUsage,
} from "../../lib/api";
import { daysAgo, minsAgo, minsAhead } from "./time";
import { mockRepos } from "./forge";
import { mockWorkers } from "./workers";
import { SAMPLE_PLAN } from "./plans";

// ── Past-run history (ux-tweaks item 3) ──────────────────────────────────────
// The runs page's past section groups by date (days this week → weeks this month →
// months beyond) and reveals progressively ("Show 50 more"), and seven hand-written
// terminal fixtures cannot exhibit either. mockHistoryRuns generates ~150 terminal
// runs DETERMINISTICALLY — titles, repos, workers, statuses and MR states cycle;
// timestamps walk back from NOW through the current week, the current month's
// weeks, then six prior months — so the demo shows every grouping grain and needs
// three reveals to exhaust, and looks the same on every refresh (a demo that
// reshuffles is worse than no demo). All terminal on purpose: the runs-in-progress
// badge count and the judge fixtures' reachable-state coverage are unaffected.

const HIST_TITLES = [
  "Deduplicate webhook deliveries on retry",
  "Add index for the runs-by-owner query",
  "Fix flaky worker heartbeat test",
  "Surface pipeline status on the MR chip",
  "Rotate forge PAT without downtime",
  "Batch notification inserts per fire",
  "Add CSV export to the usage table",
  "Harden the SSRF allowlist parser",
  "Retry claim on serialization failure",
  "Trim stale branches after merge",
  "Cache repo metadata for the board",
  "Fix timezone drift in schedule fires",
  "Add per-model cost breakdown",
  "Migrate secrets to envelope v2",
  "Reduce bundle size of the run view",
  "Add keyboard shortcuts to the board",
  "Fix orphaned tool_result pairing",
  "Speed up the seed sync fan-out",
  "Add health probe to the controller",
  "Label sync races the poller — fix",
  "Document the vault unlock flow",
  "Add rate-limit forecast to dashboard",
  "Fix double-count in phase folding",
  "Prune completed judge meta-runs",
] as const;
const HIST_REPOS = ["repo-uzi", "repo-atlas", "repo-payments"] as const;
const HIST_WORKERS = ["w-laptop", "w-ci", "w-nas", "w-hosted-eu", "w-mira"] as const;

function histRun(i: number, minsBack: number): Run {
  // Mostly completed, a steady trickle of failed and (deliberately) cancelled rows so
  // the calm "stopped" pill and the danger "failed" pill both appear down the history.
  const status = i % 9 === 4 ? "failed" : i % 13 === 7 ? "cancelled" : "completed";
  const cancelled = status === "cancelled";
  const ranMins = 23 + (i % 6) * 17; // 23–108 minutes of work
  const merged = status === "completed" && i % 4 !== 3;
  const hasMr = status === "completed" || i % 3 === 0;
  return {
    id: `run-hist-${i}`,
    repo_id: HIST_REPOS[i % HIST_REPOS.length],
    issue_iid: 400 + i,
    issue_title: HIST_TITLES[i % HIST_TITLES.length],
    issue_description: "Generated demo history — see mockHistoryRuns in mocks/data.ts.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status,
    requeue_count: 0,
    iteration_count: 1 + (i % 4),
    auto_approve: i % 5 === 0,
    worker_id: HIST_WORKERS[i % HIST_WORKERS.length],
    branch: `agent/issue-${400 + i}`,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: hasMr ? `https://gitlab.example.com/demo/-/merge_requests/${900 + i}` : null,
    issue_web_url: null,
    mr_iid: hasMr ? 900 + i : null,
    mr_state: hasMr ? (merged ? "merged" : status === "completed" ? "opened" : "closed") : null,
    failure_reason: status === "failed" ? "gate red: vitest — 2 failed" : null,
    stop_kind: cancelled ? "cancelled" : null,
    stop_reason: cancelled ? "wrong branch, restarting" : null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-console",
    anthropic_secret_label: "console-key",
    anthropic_select_reason: "auto",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(minsBack + ranMins),
    started_at: minsAgo(minsBack + ranMins),
    finished_at: minsAgo(minsBack),
    created_at: minsAgo(minsBack + ranMins + 8),
    updated_at: minsAgo(minsBack),
  };
}

// Minutes-back offsets, walked oldest-last: a run every ~9h across the last 7 days
// (today, yesterday and the current week's weekday buckets), every ~26h out to day
// 28 (the current month's week buckets), then every ~40h across six prior months
// (the month buckets). The small modular jitter keeps timestamps off round hours.
const HIST_OFFSETS_MIN: number[] = (() => {
  const out: number[] = [];
  for (let h = 3; h < 7 * 24; h += 9) out.push(h * 60 + (h % 5) * 7);
  for (let d = 7; d < 28; d++) out.push(d * 1440 + (d % 7) * 300);
  for (let h = 28 * 24; h < 210 * 24; h += 40) out.push(h * 60 + (h % 11) * 13);
  return out;
})();

export const mockHistoryRuns: Run[] = HIST_OFFSETS_MIN.map((m, i) => histRun(i, m));

// ── Other users' active runs (ux-tweaks amendment 2026-08-14 (2)) ─────────────
// The factory card shows OTHER users' runs only — the admin's own runs live in the
// Active section above it — so the demo needs runs the session admin does NOT own,
// or the filtered card would render permanently empty (before this, adminListRuns
// stamped every run with the session email: the demo factory list was 100% "mine",
// which is exactly the duplication the amendment is about). The mock Run has no
// owner column; mockOtherRunOwners IS that column: these runs are seeded into
// state.runs (so an admin can open them), excluded from the caller-scoped
// listRuns / runsInProgressCount, and attributed by adminListRuns from this map.
export const mockOtherRunOwners: Record<string, string> = {
  "run-mira-embed": "mira@uzi.local",
  "run-andrei-queued": "andrei@uzi.local",
};

export const mockOtherUserRuns: Run[] = [
  {
    id: "run-mira-embed",
    repo_id: "repo-atlas",
    issue_iid: 61,
    issue_title: "Embed the changelog in the release email",
    issue_description: "Another user's in-flight run — factory-card demo fixture.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 2,
    auto_approve: false,
    worker_id: "w-mira",
    branch: "agent/issue-61",
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    stop_reason: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: "sec-mira",
    anthropic_secret_label: "mira-key",
    anthropic_select_reason: "default",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: minsAgo(18),
    started_at: minsAgo(18),
    finished_at: null,
    created_at: minsAgo(20),
    updated_at: minsAgo(2),
  },
  {
    // Queued and NOT the viewer's own: the factory card must render this as a plain
    // "queued" pill — another owner's vault state is unknown to the viewing admin
    // (PRD #32), and with own rows filtered out the waiting-for-unlock badge has no
    // business on this list at all.
    id: "run-andrei-queued",
    repo_id: "repo-uzi",
    issue_iid: 77,
    issue_title: "Add retry budget to the seed sync",
    issue_description: "Another user's queued run — factory-card demo fixture.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    status: "queued",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: null,
    branch: null,
    model: null,
    override_subagent_model: false,
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    stop_reason: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: minsAgo(7),
    updated_at: minsAgo(7),
  },
];

// runListItem decorates a Run into the list shape the API returns.
// PRD #40 demo: usage for a run that actually ran (terminal or running); a queued /
// awaiting_approval run has none, so its list row shows no tok/cost — exactly the
// pre-feature "never a fake 0" behavior. A running run shows a smaller "so far".
function demoRunUsage(r: Run): RunUsage | null {
  if (r.status === "queued" || r.status === "claimed" || r.status === "awaiting_approval") return null;
  // History rows (ux-tweaks item 3) spread deterministically across 0.3x–2.8x so a
  // 150-run past list doesn't repeat one implausible figure down the page. The
  // hand-written fixtures keep their documented 1.33M/$1.87 exactly as before.
  const histScale = r.id.startsWith("run-hist-") ? 0.3 + (hashCode(r.id) % 100) / 40 : 1;
  const scale = (r.status === "running" ? 0.4 : 1) * histScale;
  const round = (n: number) => Math.round(n * scale);
  return {
    input_tokens: round(114_400),
    cache_read_tokens: round(1_170_000),
    cache_creation_tokens: 0,
    output_tokens: round(48_200),
    cost_usd: Math.round(187 * scale) / 100,
  };
}

// hashCode is a tiny stable string hash — enough to spread demo runs deterministically
// across the judge-badge states without pulling in a dependency.
function hashCode(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) | 0;
  return Math.abs(h);
}

// demoJudge gives the demo run list every judge-badge state (PRD #98 M4): unjudged,
// a clean verdict with no count, and a verdict carrying a to-triage count. Keyed off the
// run id so a given run looks the same on every render — a demo that reshuffles its
// badges on refresh is worse than no demo.
function demoJudge(r: Run): Pick<RunListItem, "judge_verdict" | "judge_todo_count"> {
  // Only finished runs get judged (PRD #46 enqueues at the terminal transition).
  if (r.status !== "completed" && r.status !== "failed") {
    return { judge_verdict: null, judge_todo_count: 0 };
  }
  const bucket = hashCode(r.id) % 4;
  switch (bucket) {
    case 0:
      return { judge_verdict: null, judge_todo_count: 0 }; // judged-less: no badge at all
    case 1:
      return { judge_verdict: "ideal", judge_todo_count: 0 }; // ⚖ ideal
    case 2:
      return { judge_verdict: "ok", judge_todo_count: 0 }; // ⚖ ok
    default:
      return { judge_verdict: "issues", judge_todo_count: 2 }; // ⚖ issues · 2
  }
}

export function runListItem(r: Run, ownerEmail?: string): RunListItem {
  const repo = mockRepos.find((x) => x.id === r.repo_id);
  const worker = mockWorkers.find((w) => w.id === r.worker_id);
  const usage = demoRunUsage(r);
  return {
    ...r,
    repo_path: repo?.path_with_namespace ?? r.repo_id ?? "",
    worker_name: worker?.name ?? null,
    ...demoJudge(r),
    ...(usage ? { usage } : {}),
    ...(ownerEmail ? { owner_email: ownerEmail } : {}),
  };
}

// ── Seeded message history for the completed run ─────────────────────────────

let doneSeq = 0;
const doneAt = (minAgo: number) => minsAgo(minAgo);
const dm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++doneSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: doneAt(minAgo),
});

export const mockDoneMessages: RunMessage[] = [
  // The init frame is the strip's model — the run's MAIN-THREAD model, i.e. the
  // lead's — so it must match the lead's assistant frames below (opus), or the demo
  // would show a strip model that no agent row accounts for.
  dm("status", null, { event: "init", model: "claude-opus-4-8" }, 219),
  // PRD #93: `model` rides the same assistant frame as `usage` — the coder pinned
  // sonnet while lead/reviewer ran on opus, so the demo run shows a mixed column.
  dm("text", "lead", { text: "Reading the PRD and the current run-view rendering to scope the fold-results work.", model: "claude-opus-4-8", usage: { input_tokens: 38_200, cache_read_input_tokens: 401_500, cache_creation_input_tokens: 2_000, output_tokens: 14_800 } }, 218),
  dm("tool_use", "lead", { id: "tu-1", name: "Read", input: { file_path: "prds/11-run-view-ux.md" } }, 218),
  dm("tool_result", "lead", { tool_use_id: "tu-1", content: "# PRD 11 — Run view UX\n\nFold tool results under their calls…" }, 218),
  dm("tool_use", "lead", { id: "tu-2", name: "Grep", input: { pattern: "tool_result", path: "web/src" } }, 217),
  dm("tool_result", "lead", { tool_use_id: "tu-2", content: "web/src/components/RunEvent.tsx:12\nweb/src/components/ActivityFeed.tsx:44" }, 217),
  dm("plan", "lead", { text: SAMPLE_PLAN() }, 216),
  // PRD #40: the plan turn's own result frame → a distinct "Plan" per-phase row.
  // issue #199 (defect 4): `num_turns` is PER-INVOCATION, not a running total, so the
  // plan frame (16) deliberately EXCEEDS the implement frame (11) below. A decreasing
  // sequence is the one shape a cumulative counter cannot produce, so it disambiguates
  // the per-phase table's summed total — a rising pair reads as a double-count.
  dm("status", null, {
    event: "result",
    subtype: "success",
    duration_ms: 5 * 60_000,
    num_turns: 16,
    total_cost_usd: 0.24,
    usage: { input_tokens: 21_400, cache_read_input_tokens: 188_000, cache_creation_input_tokens: 0, output_tokens: 6_100 },
    modelUsage: {
      "claude-sonnet-5": { inputTokens: 27_100, outputTokens: 11_600, cacheReadInputTokens: 402_000, cacheCreationInputTokens: 0, costUSD: 0.19 },
      "claude-sonnet-4-6": { inputTokens: 7_600, outputTokens: 3_200, cacheReadInputTokens: 118_000, cacheCreationInputTokens: 0, costUSD: 0.05 },
    },
  }, 216),
  dm("status", null, { text: "plan submitted — awaiting approval" }, 216),
  dm("status", null, { text: "plan approved by vlad@uzi.local" }, 205),
  dm("text", "coder", { text: "Implementing the id-based pairing index and the fold-under-call rendering.", model: "claude-sonnet-5", usage: { input_tokens: 51_600, cache_read_input_tokens: 583_900, cache_creation_input_tokens: 0, output_tokens: 24_100 } }, 204),
  dm("tool_use", "coder", { id: "tu-3", name: "Edit", input: { file_path: "web/src/components/RunEvent.tsx" } }, 203),
  dm("tool_result", "coder", { tool_use_id: "tu-3", content: "ok" }, 203),
  dm("tool_use", "coder", { id: "tu-4", name: "Bash", input: { command: "cd web && npx vitest run src/components/RunEvent.test.tsx" } }, 200),
  dm("tool_result", "coder", { tool_use_id: "tu-4", content: "✓ 14 tests passed" }, 199),
  // PRD #116: the real #115 story — the lead tried to spawn the SDK built-in
  // `Explore` subagent, the subagent guard denied it (guardrails.ts), and the run
  // carried on. It must read as a neutral "⊘ blocked" chip, not a red error.
  dm("tool_use", "lead", { id: "tu-5", name: "Agent", input: { subagent_type: "Explore", description: "map the remaining render paths" } }, 198),
  dm("tool_result", "lead", { tool_use_id: "tu-5", content: "denied by guardrail: only the run's assembled subagents may be invoked", is_error: true }, 198),
  dm("text", "lead", { text: "I'll use the researcher subagent instead." }, 197),
  dm("text", "reviewer", { text: "Pairing is by id, orphan results render standalone, and the cap keeps folding correct at the boundary. One nit: memoize the index. Approved after that.", model: "claude-opus-4-8", usage: { input_tokens: 18_900, cache_read_input_tokens: 149_700, cache_creation_input_tokens: 0, output_tokens: 7_600 } }, 195),
  dm("tool_use", "coder", { id: "tu-6", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 192),
  dm("tool_result", "coder", { tool_use_id: "tu-6", content: "ok" }, 192),
  dm("tool_use", "coder", { id: "tu-7", name: "Bash", input: { command: "cd web && npm run typecheck && npm test" } }, 190),
  dm("tool_result", "coder", { tool_use_id: "tu-7", content: "typecheck clean\n✓ 61 tests passed" }, 188),
  // PRD #333: an off-task bug the coder noticed on the way to the render work and flagged
  // WITHOUT stopping its turn — the info/blue finding card, keyed to the open `find-1`
  // coordinate so File/Dismiss are live in the demo.
  dm("finding", "coder", {
    id: "find-1",
    title: "Leaked ticker in sweepLoop never stopped on shutdown",
    location: "api/internal/store/sweeper.go#sweepLoop",
    labels: ["bug"],
    confidence: "high",
  }, 187),
  // PRD #634 M5: the worker finalized the run's committed slice at the operator's scope
  // ceiling and emitted a steer_ack. Payload keys mirror sdk-executor.ts's emit exactly
  // (text/directive/ceiling/completed); the feed row renders the text and the ActivityFeed
  // live region announces a fixed phrase.
  dm("steer_ack", "worker", {
    text: "operator scope ceiling reached (2/4); finalizing the committed slice, starting no further milestone",
    directive: "scope",
    ceiling: 2,
    completed: 2,
  }, 185),
  dm("status", null, { text: "pushing branch agent/issue-18 and opening the MR" }, 185),
  // issue #199 (defect 4): implement `num_turns` (11) sits BELOW the plan frame's (16) —
  // per-invocation, not cumulative. Heavier tokens/cost in fewer turns is legitimate
  // (turns ≠ tokens): the implement burst ran larger turns than the plan exploration.
  dm("status", null, {
    event: "result",
    subtype: "success",
    duration_ms: 2_100_000,
    num_turns: 11,
    total_cost_usd: 1.87,
    usage: { input_tokens: 114_400, cache_read_input_tokens: 1_170_000, cache_creation_input_tokens: 0, output_tokens: 48_200 },
    modelUsage: {
      "claude-sonnet-5": { inputTokens: 138_000, outputTokens: 92_000, cacheReadInputTokens: 3_100_000, cacheCreationInputTokens: 0, costUSD: 1.19 },
      "claude-opus-4-8": { inputTokens: 46_000, outputTokens: 27_000, cacheReadInputTokens: 640_000, cacheCreationInputTokens: 0, costUSD: 0.68 },
    },
  }, 184),
];

// ── Awaiting-approval history (parked at the plan gate) ───────────────────────

let awaitSeq = 0;
const am = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++awaitSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockAwaitingMessages: RunMessage[] = [
  am("status", null, { event: "init", model: "claude-sonnet-4-6" }, 9),
  am("text", "lead", { text: "Reading the PRD for issue #21 and mapping how run-owner notifications should be delivered." }, 9),
  am("tool_use", "lead", { id: "aw-1", name: "Read", input: { file_path: "prds/9-approval-notify.md" } }, 8),
  am("tool_result", "lead", { tool_use_id: "aw-1", content: "# PRD 9 — Plan-approval notifications\n\nEmail the run owner when a plan parks…" }, 8),
  am("plan", "lead", { text: SAMPLE_PLAN() }, 6),
  am("status", null, { text: "plan submitted — awaiting approval" }, 6),
];

// ── Unreadable-question history (PRD #88) ────────────────────────────────────

let unreadableSeq = 0;
const um = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++unreadableSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

// The `question` message here is DELIBERATELY UNRENDERABLE: it carries no `question_id`.
// That is not a hypothetical shape — it is what a surface sees from a truncated payload,
// or from a worker mid-rollout that emits a question the current parse cannot use. The
// api rejects an answer that names no question, so the run genuinely cannot be answered;
// the panel's job is to SAY that instead of leaving the run at "needs your answer" with
// nothing on screen until the deadline fails it.
//
// Keep the field ABSENT rather than empty-string: both are unusable, and absent is the
// shape a producer change actually yields.
export const mockUnreadableQuestionMessages: RunMessage[] = [
  um("status", null, { event: "init", model: "claude-sonnet-4-6" }, 20),
  um("text", "lead", { text: "Mapping the poll path before I propose a caching strategy." }, 19),
  um("question", "lead", { questions: [{ question: "Which cache TTL?", header: "TTL" }] }, 14),
];

// ── Failed-run history (short) ───────────────────────────────────────────────

let failSeq = 0;
const fm = (kind: string, agent: string | null, payload: unknown): RunMessage => ({
  seq: ++failSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: daysAgo(1.15),
});

export const mockFailedMessages: RunMessage[] = [
  fm("status", null, { event: "init", model: "claude-opus-4-8" }), // matches the lead's frames below
  fm("text", "lead", { text: "Benchmarking the pool under load before proposing settings.", model: "claude-opus-4-8", usage: { input_tokens: 8_200, cache_read_input_tokens: 42_000, cache_creation_input_tokens: 0, output_tokens: 1_900 } }),
  fm("tool_use", "lead", { id: "f-1", name: "Bash", input: { command: "go test -bench=Pool ./internal/store/..." } }),
  fm("tool_result", "lead", { tool_use_id: "f-1", content: "benchmark hung — no output after 40m", is_error: true }),
  fm("error", null, { text: "run timed out after 2h0m0s (RUN_TIMEOUT)" }),
  // PRD #40 (Decision 4): a failed run still spent tokens — a usage-bearing error
  // result frame, so the run view shows its pre-death usage.
  fm("error", null, {
    event: "result",
    subtype: "error_during_execution",
    errors: ["run timed out after 2h0m0s (RUN_TIMEOUT)"],
    num_turns: 4,
    duration_ms: 2 * 60 * 60_000,
    total_cost_usd: 0.11,
    usage: { input_tokens: 8_200, cache_read_input_tokens: 42_000, cache_creation_input_tokens: 0, output_tokens: 1_900 },
    modelUsage: {
      "claude-sonnet-5": { inputTokens: 10_400, outputTokens: 3_100, cacheReadInputTokens: 96_000, cacheCreationInputTokens: 0, costUSD: 0.09 },
      "claude-sonnet-4-6": { inputTokens: 3_200, outputTokens: 900, cacheReadInputTokens: 27_000, cacheCreationInputTokens: 0, costUSD: 0.02 },
    },
  }),
];

// ── Parked-on-a-usage-limit history (PRD #35) ────────────────────────────────
// The stream behind run-limit-wait. It is the only fixture carrying the two new
// message kinds, so it is what makes their render cases reachable in mock mode —
// and mock mode is how this gets browser-validated at all, since a non-mock
// `vite dev`/`preview` of this repo proxies /api at whatever real stack is running.
//
// It carries BOTH kinds even though one run can only have parked, because they are
// two different rows (warn vs. danger) and the pair in one visible stream is the
// only way to see that the distinction survives. The `limit_hit` here is the earlier
// death of a run that had NOT opted in, replayed in the same log for that reason.

let limitSeq = 0;
const lm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++limitSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockLimitWaitMessages: RunMessage[] = [
  lm("status", null, { event: "init", model: "claude-opus-4-8" }, 139),
  lm("text", "lead", { text: "Reading the worker heartbeat path to scope the metrics endpoint." }, 138),
  lm("tool_use", "lead", { id: "lw-1", name: "Read", input: { file_path: "api/internal/handler/workers.go" } }, 138),
  lm("tool_result", "lead", { tool_use_id: "lw-1", content: "// Heartbeat records liveness for the claim path…" }, 137),
  // The first park. Two keys only — `rate_limit_type` and an ISO `resets_at` — which
  // is the whole payload the worker emits. There is deliberately no `attempt`: the
  // count is incremented server-side after this message is written, so a
  // worker-supplied one would be a stale N-1. The run row's limit_wait_count is the
  // live value and the run-view strip renders it.
  lm("limit_wait", "worker", { rate_limit_type: "five_hour", resets_at: minsAgo(51) }, 92),
  lm("status", null, { text: "resumed after the usage window reopened" }, 44),
  lm("text", "coder", { text: "Picking the metrics endpoint back up from the plan." }, 43),
  lm("tool_use", "coder", { id: "lw-2", name: "Edit", input: { file_path: "api/internal/handler/metrics.go" } }, 40),
  lm("tool_result", "coder", { tool_use_id: "lw-2", content: "ok" }, 40),
  // The second park — the live one, whose reset matches the run row's limit_resets_at.
  // The two parks differ ONLY in resets_at, which is exactly what distinguishes the
  // rows now that no count rides them: they must not read as duplicates.
  lm("limit_wait", "worker", { rate_limit_type: "five_hour", resets_at: minsAhead(154) }, 6),
  // A limit_hit from an opted-OUT run, replayed here so the danger-toned variant is
  // visible somewhere in mock mode. Its window name is one this build knows; the
  // untrusted-value path is covered by tests rather than by a hostile fixture.
  lm("limit_hit", "worker", { rate_limit_type: "seven_day_opus", resets_at: minsAhead(3_400) }, 5),
  // A park with NO keys at all — the shape when the SDK frame carried neither field.
  // Both are OMITTED rather than null, so this is what "unknown" actually looks like
  // on the wire, and the row still has to say the one thing it is for.
  lm("limit_wait", "worker", {}, 5),
];
