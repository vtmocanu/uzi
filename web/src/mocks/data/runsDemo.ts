import type {
  Run,
  RunMessage,
  SteerInput,
} from "../../lib/api";
import { minsAgo } from "./time";
import { LIVE_RUN_ID } from "./boards";

// ── Crew-roster demo runs (PRD #95 M2) ───────────────────────────────────────
// These exist to exercise the crew-state ladder + the collapse/Follow UX in mock
// mode: a multi-agent BURST that used to yank the pane (now updates the crew strip
// + one-liners in place), plus health-varied runs so every crew state renders.
// Combined with the existing seeds — run-live (working), run-awaiting (waiting at
// gate), run-done (done), run-queued (empty) — these two cover the remaining
// stalled + waiting_worker + non-active idle/waiting split.

let crewSeq = 0;
const cm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++crewSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

// A three-agent burst: lead handed off long ago (idle), coder spoke recently
// (waiting), reviewer is the newest speaker (the active one). On run-crew's
// `looping` health the active reviewer reads amber `stalled`, never green.
export const mockCrewMessages: RunMessage[] = [
  cm("status", null, { event: "init", model: "claude-opus-4-8" }, 12),
  cm("text", "lead", { text: "Scoping the crew-roster work; handing the implementation to coder." }, 11),
  cm("tool_use", "lead", { id: "cw-1", name: "Read", input: { file_path: "prds/95-activity-pane-v2.md" } }, 11),
  cm("tool_result", "lead", { tool_use_id: "cw-1", content: "# PRD 95 — activity pane v2…" }, 11),
  cm("text", "coder", { text: "Building the crew strip + collapse-by-default accordion." }, 1),
  cm("tool_use", "coder", { id: "cw-2", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 1),
  cm("tool_result", "coder", { tool_use_id: "cw-2", content: "ok" }, 1),
  cm("tool_use", "coder", { id: "cw-3", name: "Bash", input: { command: "cd web && npm run typecheck" } }, 0.9),
  cm("tool_result", "coder", { tool_use_id: "cw-3", content: "typecheck clean" }, 0.8),
  cm("tool_use", "coder", { id: "cw-4", name: "Bash", input: { command: "cd web && npx vitest run src/components/ActivityFeed.test.tsx" } }, 0.6),
  cm("tool_result", "coder", { tool_use_id: "cw-4", content: "✓ 29 tests passed" }, 0.5),
  cm("text", "reviewer", { text: "Reviewing the ladder — checking that a long tool call still reads working." }, 0.3),
  cm("tool_use", "reviewer", { id: "cw-5", name: "Grep", input: { pattern: "STALE_MS", path: "web/src" } }, 0.2),
  cm("tool_use", "reviewer", { id: "cw-6", name: "Bash", input: { command: "go test ./..." } }, 0.1),
];

let degradedSeq = 0;
const gm = (kind: string, agent: string | null, payload: unknown, minAgo: number): RunMessage => ({
  seq: ++degradedSeq,
  kind,
  agent,
  agent_instance: null,
  agent_label: null,
  payload,
  created_at: minsAgo(minAgo),
});

// run-degraded: the worker went quiet (health=waiting_worker), so the whole crew
// reads `waiting` regardless of who spoke last.
export const mockDegradedMessages: RunMessage[] = [
  gm("status", null, { event: "init", model: "claude-opus-4-8" }, 8),
  gm("text", "lead", { text: "Working the fix; last heartbeat was a while ago." }, 7),
  gm("tool_use", "lead", { id: "dg-1", name: "Bash", input: { command: "go build ./..." } }, 6),
];

function demoIssueRun(over: Partial<Run> & Pick<Run, "id" | "status" | "health">): Run {
  return {
    repo_id: "repo-uzi",
    issue_iid: 24,
    issue_title: over.issue_title ?? "Crew roster demo run",
    issue_description: "Demo run exercising the PRD #95 crew-state ladder.",
    kind: "issue",
    title: null,
    resume_of_run_id: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w-laptop",
    branch: "agent/issue-24",
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
    claimed_at: minsAgo(12),
    started_at: minsAgo(12),
    finished_at: null,
    created_at: minsAgo(13),
    updated_at: minsAgo(1),
    ...over,
  };
}

// ── PRD #99: per-instance lane demo runs ─────────────────────────────────────
// Every other fixture in this file hardcodes both attribution columns to null, so
// before these existed `VITE_UZI_MOCK=1 npm run dev` showed NONE of PRD #99: every
// run coalesced to legacy role lanes and the feature had no demoable state. That is
// the same blind spot that let both code suites stay green while the lane grouping
// was removable (see the M5 mutation record) — a fixture that cannot express a
// feature cannot demo it OR guard it.
let laneSeq = 0;
const nm = (
  kind: string,
  agent: string | null,
  instance: string | null,
  label: string | null,
  payload: unknown,
  minAgo: number,
): RunMessage => ({
  seq: ++laneSeq,
  kind,
  agent,
  agent_instance: instance,
  agent_label: label,
  payload,
  created_at: minsAgo(minAgo),
});

// run-lanes — the PRD headline. Two `coder` invocations running in parallel, with
// DISTINCT instance ids and their own task labels, interleaved NON-adjacently (a
// reviewer frame sits between them). Non-adjacency is the load-bearing part: two
// contiguous coder frames render correctly even under the pre-#99 consecutive-author
// grouping, so they would not show the fix. The lead's own turns carry neither
// column, so its lane is the role-keyed fallback beside the two instance-keyed ones.
export const mockLaneMessages: RunMessage[] = [
  nm("status", null, null, null, { event: "init", model: "claude-opus-4-8" }, 18),
  nm("text", "lead", null, null, { text: "Dispatching two coders in parallel: API wiring and the web gate." }, 17),
  nm("tool_use", "lead", null, null, { id: "ln-1", name: "Agent", input: { description: "API wiring", subagent_type: "coder" } }, 17),
  nm("tool_use", "lead", null, null, { id: "ln-2", name: "Agent", input: { description: "web gate UX", subagent_type: "coder" } }, 17),
  nm("tool_use", "coder", "toolu_01coderA", "API wiring", { id: "ln-3", name: "Read", input: { file_path: "api/internal/store/queries/runtime.sql" } }, 12),
  nm("tool_result", "coder", "toolu_01coderA", "API wiring", { tool_use_id: "ln-3", content: "-- name: InsertRunMessage :execrows" }, 12),
  nm("tool_use", "reviewer", "toolu_01review", "audit the wire threading", { id: "ln-4", name: "Grep", input: { pattern: "agent_instance", path: "api" } }, 9),
  nm("tool_use", "coder", "toolu_01coderB", "web gate UX", { id: "ln-5", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 7),
  nm("tool_result", "coder", "toolu_01coderB", "web gate UX", { tool_use_id: "ln-5", content: "1 change applied" }, 7),
  // The same instance speaking again, several frames later: it folds back into its
  // OWN lane instead of opening a fresh near-empty bar (Problem 1's fix).
  nm("tool_use", "coder", "toolu_01coderA", "API wiring", { id: "ln-6", name: "Bash", input: { command: "cd api && go test ./internal/store/..." } }, 3),
  nm("tool_result", "coder", "toolu_01coderA", "API wiring", { tool_use_id: "ln-6", content: "ok  gitlab.example.com/myorg/uzi/api/internal/store  0.42s" }, 2),
  nm("tool_use", "coder", "toolu_01coderB", "web gate UX", { id: "ln-7", name: "Bash", input: { command: "cd web && npx vitest run src/components/ActivityFeed.test.tsx" } }, 0.3),
];

// run-busy — the conditional role rollup, and specifically the ONE state pairing
// nobody had seen on screen: a doubled role whose worst state is `waiting` while one
// of its own lanes is visibly pulsing `working`. worstStateFor takes the MIN of
// STATE_PRIORITY and `waiting`(1) outranks `working`(2), so the chip reads `waiting`
// and does not pulse while the lane below it does. That is by design (the chip is a
// worst-state summary, not a most-active one) but nothing on screen says so, and the
// no-legend decision removed the obvious place to say it. This fixture exists so the
// pairing is browsable rather than theoretical — see the open question in PRD #99.
//
// Shape: `tester` is doubled — toolu_01testX is the newest speaker (active → working)
// and toolu_01testY spoke 0.4 min ago, inside the 45s recency window (→ waiting).
let busySeq = 0;
const bm = (
  kind: string,
  agent: string | null,
  instance: string | null,
  label: string | null,
  payload: unknown,
  minAgo: number,
): RunMessage => ({
  seq: ++busySeq,
  kind,
  agent,
  agent_instance: instance,
  agent_label: label,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockBusyMessages: RunMessage[] = [
  bm("status", null, null, null, { event: "init", model: "claude-opus-4-8" }, 22),
  bm("text", "lead", null, null, { text: "Six subagents across the migration and the web rebuild." }, 21),
  bm("tool_use", "researcher", "toolu_01resrch", "survey prior art in inspiration/", { id: "b-1", name: "Read", input: { file_path: "inspiration/bottega/README.md" } }, 20),
  // A long label, over the 48-rune layout clamp: it renders truncated with an
  // ellipsis, which is the only on-screen signal that text was dropped.
  bm("tool_use", "coder", "toolu_01coder1", "thread agent_instance and agent_label through the entire message wire, end to end", { id: "b-2", name: "Bash", input: { command: "cd api && go build ./... && go test ./..." } }, 12),
  bm("tool_use", "coder", "toolu_01coder2", "rebuild the activity pane around per-instance lanes", { id: "b-3", name: "Edit", input: { file_path: "web/src/components/ActivityFeed.tsx" } }, 10),
  // A subagent frame with an instance but NO label: its lane titles as the bare role,
  // no `·` suffix and no placeholder (the label-absent degradation).
  bm("tool_use", "auditor", "toolu_01audit1", null, { id: "b-4", name: "Grep", input: { pattern: "CLAUDE_CODE_OAUTH_TOKEN", path: "agent/src" } }, 8),
  bm("tool_use", "documenter", "toolu_01docs01", "docs: run activity page", { id: "b-5", name: "Edit", input: { file_path: "docs/run-activity.md" } }, 7),
  bm("tool_use", "reviewer", "toolu_01review", "review the migration + wire threading", { id: "b-6", name: "Read", input: { file_path: "api/internal/store/migrations/00075_run_message_instance.sql" } }, 6),
  // The doubled role. Y spoke recently (waiting), X is the newest speaker (working) —
  // so the `tester ×2` chip rolls up as `waiting` while X's lane pulses.
  bm("tool_use", "tester", "toolu_01testY", "unit: RunEvent", { id: "b-7", name: "Bash", input: { command: "npx vitest run src/components/RunEvent.test.tsx" } }, 0.4),
  bm("tool_use", "tester", "toolu_01testX", "e2e: approval gate", { id: "b-8", name: "Bash", input: { command: "./e2e/run-e2e.sh" } }, 0.1),
];

// mockSeededMessages backs the PRD #209 seeded-plan demo run (run-seeded). A seeded run
// SKIPS the Phase-1 planning turn, so there is deliberately NO `plan` message here — the
// plan arrives with the claim (SeededPlanPanel renders run.plan_md, not a feed message).
// Kept short and lead-only so the "roster pending" state on the run page stays coherent:
// the worker has not yet reported the clone's .claude/agents/, so no subagent lanes have
// opened.
export const mockSeededMessages: RunMessage[] = [
  {
    seq: 1,
    kind: "status",
    agent: null,
    agent_instance: null,
    agent_label: null,
    payload: { event: "init", model: "claude-opus-4-8" },
    created_at: minsAgo(4),
  },
  {
    seq: 2,
    kind: "text",
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload: {
      text: "Implementing the plan you supplied at create time — skipping the planning turn and the approval gate.",
    },
    created_at: minsAgo(3),
  },
];

// ── PRD #237: live-only token counts (run mid-first-turn) ────────────────────
// run-live-tokens exercises the run-page state this issue adds: a running run that has
// emitted per-call assistant `usage` frames but NO result frame yet, so the usage panel
// must show a LIVE / in-flight token section BEFORE any confirmed/billed figure exists.
// Every other running fixture either carries no usage frames or (like run-done) a result
// frame, so none of them could exhibit the live-before-confirmed state on its own.
//
// Two dedup properties the panel relies on are baked in deliberately: the SDK emits
// several frames per API call all repeating ONE usage, so the fixture includes
// byte-identical (agent_instance, usage) pairs — two lead frames (instance null) and two
// coder frames (same non-null instance) — that the panel must COLLAPSE rather than
// double-count. Model attribution is kept internally consistent per agent: lead on opus
// (agent_instance null), coder on sonnet (non-null agent_instance, mirroring
// mockLaneMessages), so the live breakdown spans exactly two model keys.
let liveTokensSeq = 0;
const ltm = (
  kind: string,
  agent: string | null,
  instance: string | null,
  label: string | null,
  payload: unknown,
  minAgo: number,
): RunMessage => ({
  seq: ++liveTokensSeq,
  kind,
  agent,
  agent_instance: instance,
  agent_label: label,
  payload,
  created_at: minsAgo(minAgo),
});

export const mockLiveTokensMessages: RunMessage[] = [
  // Init frame → the strip's main-thread (lead) model; matches the lead's opus frames.
  ltm("status", null, null, null, { event: "init", model: "claude-opus-4-8" }, 6),
  // Lead's first API call (opus). The SDK repeats one usage across several frames per
  // call, so this exact usage appears TWICE back-to-back — the panel must collapse the
  // duplicate (agent_instance=null, usage) to a single contribution.
  ltm("text", "lead", null, null, { text: "Scoping the token-panel work and dispatching a coder for the web side.", model: "claude-opus-4-8", usage: { input_tokens: 12_400, cache_read_input_tokens: 88_000, cache_creation_input_tokens: 1_500, output_tokens: 3_200 } }, 6),
  ltm("text", "lead", null, null, { text: "Scoping the token-panel work and dispatching a coder for the web side.", model: "claude-opus-4-8", usage: { input_tokens: 12_400, cache_read_input_tokens: 88_000, cache_creation_input_tokens: 1_500, output_tokens: 3_200 } }, 6),
  ltm("tool_use", "lead", null, null, { id: "lt-1", name: "Agent", input: { description: "web token panel", subagent_type: "coder" } }, 5),
  // Coder subagent (sonnet), a distinct instance/lane. Its first call's usage ALSO
  // repeats byte-for-byte across two adjacent frames sharing the SAME agent_instance —
  // the second dedup case (a per-instance one, not just the lead's null-instance one).
  ltm("text", "coder", "toolu_01coderLT", "web token panel", { text: "Reading the run-view usage rendering to add the live section.", model: "claude-sonnet-5", usage: { input_tokens: 22_600, cache_read_input_tokens: 143_000, cache_creation_input_tokens: 0, output_tokens: 8_400 } }, 4),
  ltm("text", "coder", "toolu_01coderLT", "web token panel", { text: "Reading the run-view usage rendering to add the live section.", model: "claude-sonnet-5", usage: { input_tokens: 22_600, cache_read_input_tokens: 143_000, cache_creation_input_tokens: 0, output_tokens: 8_400 } }, 4),
  ltm("tool_use", "coder", "toolu_01coderLT", "web token panel", { id: "lt-2", name: "Read", input: { file_path: "web/src/components/RunUsage.tsx" } }, 4),
  // A SECOND, distinct call from each agent (new usage numbers) so the live totals are a
  // real sum across calls, not one repeated figure. Coder still on sonnet, lead still on
  // opus → two model keys in the live breakdown.
  ltm("text", "coder", "toolu_01coderLT", "web token panel", { text: "Adding the in-flight token section above the confirmed figures.", model: "claude-sonnet-5", usage: { input_tokens: 24_900, cache_read_input_tokens: 167_400, cache_creation_input_tokens: 0, output_tokens: 9_700 } }, 2),
  ltm("text", "lead", null, null, { text: "Watching the coder's progress before the reviewer pass.", model: "claude-opus-4-8", usage: { input_tokens: 13_800, cache_read_input_tokens: 96_500, cache_creation_input_tokens: 0, output_tokens: 2_100 } }, 1),
  // Deliberately NO result frame: the run is mid-first-turn, so there are live tokens but
  // nothing confirmed/billed yet — the whole point of this fixture.
];

export const mockLaneRuns: Run[] = [
  demoIssueRun({
    id: "run-lanes",
    issue_iid: 99,
    issue_title: "Two parallel coders (PRD #99 headline case)",
    issue_description: "Demo run: two same-role subagent invocations that must not merge into one lane.",
    branch: "agent/issue-99",
    status: "running",
    health: "ok",
  }),
  demoIssueRun({
    id: "run-busy",
    issue_iid: 99,
    issue_title: "Busy crew: role rollup, doubled tester, clamped label",
    issue_description: "Demo run: enough lanes and doubled roles to trigger the conditional role rollup.",
    branch: "agent/issue-99",
    status: "running",
    health: "ok",
  }),
  // run-stalled — the SAME message stream as run-busy, differing only in run health.
  // `stalled` is not a per-lane property: crewStateFor returns it only for the ACTIVE
  // lane and only when the RUN's health is degraded, so the mockup's headline case
  // ("the one stalled tester is the first thing you see") could not be browsed on any
  // healthy fixture. Reusing run-busy's stream rather than authoring a third one keeps
  // run-busy meaning exactly what its ordering tests say about it — the two runs differ
  // by one field, so the rollup's stalled-first behaviour is visible as a DIFF against
  // the run beside it rather than as a separate story.
  demoIssueRun({
    id: "run-stalled",
    issue_iid: 99,
    // web-ux F23. `open_failed` had no mock run, so the LONGEST string in the
    // vocabulary could only ever be seen as a rendering preview — and F20's overflow
    // was findable exactly because a long string was on screen. This also fixes a
    // second thing: a *running* run carrying NO credential is a shape no real instance
    // produces, since a running run has by definition claimed one.
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "open_failed",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    issue_title: "Busy crew, degraded: the stalled role sorts to the front",
    issue_description: "Demo run: same stream as run-busy with a looping health flag, so the active tester lane reads stalled and its role chip leads the rollup.",
    branch: "agent/issue-99",
    status: "running",
    health: "looping",
    health_reason: "no new tool calls in the last few minutes",
    health_since: minsAgo(3),
  }),
  // run-live-tokens (PRD #237) — a running run mid-first-turn: assistant `usage` frames
  // have arrived but NO result frame has, so the usage panel shows a LIVE / in-flight
  // token section with nothing confirmed yet. Streams mockLiveTokensMessages.
  demoIssueRun({
    id: "run-live-tokens",
    issue_iid: 237,
    issue_title: "Live token counts before the first result frame (PRD #237)",
    issue_description: "Demo run: per-call usage frames with no result frame, so tokens are live-only.",
    branch: "agent/issue-237",
    status: "running",
    health: "ok",
  }),
];

export const mockCrewRuns: Run[] = [
  demoIssueRun({
    id: "run-crew",
    issue_title: "Crew roster: looping worker (stalled active speaker)",
    status: "running",
    health: "looping",
    health_reason: "no new tool calls in the last few minutes",
    health_since: minsAgo(3),
  }),
  demoIssueRun({
    id: "run-degraded",
    // The other half of F23: `pool_empty`, the second string with no mock run.
    anthropic_secret_id: "sec-default",
    anthropic_secret_label: "default",
    anthropic_select_reason: "pool_empty",
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    issue_title: "Crew roster: worker went quiet (waiting on a worker)",
    status: "running",
    health: "waiting_worker",
    health_reason: "the worker's heartbeat is stale",
    health_since: minsAgo(2),
  }),
];

// Sample steer queue per run (PRD #95), so M3's SteerQueueCard has demo data across
// every delivery state without needing a live consume: NULL consumed_at → Queued,
// set → Delivered; the run's status decides the gate/terminal copy client-side.
const steerInput = (
  id: number,
  body: string,
  createdMinAgo: number,
  consumedMinAgo: number | null,
): SteerInput => ({
  id,
  body,
  created_at: minsAgo(createdMinAgo),
  consumed_at: consumedMinAgo == null ? null : minsAgo(consumedMinAgo),
  kind: "follow_up",
  disposition: null,
});

// PRD #634: an operator scope-ceiling directive. It is never consumed (consumed_at
// stays null) — its state lives entirely in `disposition` ("applied" | "declined" |
// "superseded" | null pending), which drives the card's chip.
const scopeInput = (
  id: number,
  body: string,
  createdMinAgo: number,
  disposition: string | null,
): SteerInput => ({
  id,
  body,
  created_at: minsAgo(createdMinAgo),
  consumed_at: null,
  kind: "scope",
  disposition,
});

export const mockRunInputs: Record<string, SteerInput[]> = {
  // Live run: a pending scope directive (PRD #634), one delivered follow-up, one still
  // queued (newest-first).
  [LIVE_RUN_ID]: [
    scopeInput(8, "scope ceiling → complete through milestone 2 of 4", 0, null),
    steerInput(2, "also add a Prometheus histogram for heartbeat age", 1, null),
    steerInput(1, "focus on the metrics endpoint first", 3, 2),
  ],
  // At the gate: a follow-up consumed while parked → "Delivered — applies after approval".
  "run-awaiting": [steerInput(3, "prefer email over Slack for the first cut", 5, 4)],
  // PRD #517: parked awaiting a follow-up. The follow-up was consumed (non-null
  // consumed_at) so SteerQueueCard renders the "Delivered — resumes the run" parked chip.
  "run-awaiting-followup": [
    steerInput(7, "also skip the poll while the drag preview is animating out", 4, 2),
  ],
  // Finished run: the operator retargeted the ceiling twice before the run finalized, so
  // this queue exhibits the three terminal scope dispositions (PRD #634) — an earlier
  // directive superseded by a later one, a declined one, and the applied one the run
  // finalized at — plus a follow-up that was never consumed → "Not delivered — run
  // finished". With the live run's pending directive, all four disposition pills are
  // reachable in mock mode.
  "run-done": [
    scopeInput(9, "scope ceiling → complete through milestone 3 of 5", 200, "applied"),
    scopeInput(11, "scope ceiling → complete through milestone 4 of 5", 205, "declined"),
    scopeInput(10, "scope ceiling → complete through milestone 2 of 5", 210, "superseded"),
    steerInput(4, "one more nit: memoize the tool index", 186, null),
  ],
  "run-crew": [
    steerInput(6, "check the reduced-motion path too", 1, null),
    steerInput(5, "make sure a long tool call still reads working", 6, 5),
  ],
};
