// The mock run engine: timer-driven scripts that make a run feel alive with no
// backend. A script is a list of (delay, step) pairs; each step persists a
// message (store.appendMessage → broadcast) or patches run state. The plan step
// parks the run in awaiting_approval; handleInput() resumes/rejects/cancels it,
// mirroring the real worker protocol's semantics.

import type { IssueProposal, RunInputKind } from "../lib/api";
import { HEARTBEAT_MILESTONES, SAMPLE_PLAN } from "./data";
import { appendMessage, getRun, listMessages, nextProposalId, patchRun, putProposal } from "./store";

type Step = () => void;
interface Timed {
  delay: number; // ms after the previous step
  step: Step;
}

const timers = new Map<string, number[]>();
const started = new Set<string>();
// PRD #88: the clarification question each run is currently parked on, mirroring
// `runs.open_question_id`. The mock keeps it because the SERVER keeps it: an `answer`
// naming any other id is rejected (409 "that question has already been answered or
// replaced"), and a mock that accepted any answer would let the demo — and every test
// written against it — pass on a contract the shipped api does not honour.
const openQuestions = new Map<string, string>();

function schedule(runId: string, steps: Timed[]) {
  let at = 0;
  const ids: number[] = [];
  for (const { delay, step } of steps) {
    at += delay;
    ids.push(window.setTimeout(step, at));
  }
  timers.set(runId, [...(timers.get(runId) ?? []), ...ids]);
}

function clearTimers(runId: string) {
  for (const id of timers.get(runId) ?? []) window.clearTimeout(id);
  timers.delete(runId);
}

// ── Scripts ──────────────────────────────────────────────────────────────────

// PRD #40: an optional per-call `usage` rides a message so the demo run view can
// render its per-agent breakdown (Decision 11) — the same shape the worker attaches
// to a surviving assistant message.
// PRD #99: `attr` carries the subagent INVOCATION identity (instance id + task label)
// so the scripted demo exercises the live lane path, not just the seeded fixtures — a
// frame with a new agent_instance must open a NEW lane mid-run, and that is precisely
// the path that only exists over the socket (applyFrame builds the RunMessage from the
// frame; there is no REST re-read). Optional, so every lead/infra call site is unchanged
// and keeps emitting NULL attribution.
type Attr = { instance?: string | null; label?: string | null };

const say = (
  runId: string,
  agent: string | null,
  text: string,
  usage?: unknown,
  attr: Attr = {},
): Timed[] => [
  { delay: 900, step: () => appendMessage(runId, "text", agent, usage ? { text, usage } : { text }, attr) },
];

const tool = (
  runId: string,
  agent: string,
  name: string,
  input: Record<string, unknown>,
  result: string,
  opts: { error?: boolean; runFor?: number; attr?: Attr } = {},
): Timed[] => {
  const id = `mock-${Math.random().toString(36).slice(2, 9)}`;
  const attr = opts.attr ?? {};
  return [
    { delay: 700, step: () => appendMessage(runId, "tool_use", agent, { id, name, input }, attr) },
    {
      delay: opts.runFor ?? 1400,
      step: () =>
        appendMessage(
          runId,
          "tool_result",
          agent,
          {
            tool_use_id: id,
            content: result,
            ...(opts.error ? { is_error: true } : {}),
          },
          // The result must carry the SAME identity as its call, or a lane holds the
          // call and its own result lands in a different one.
          attr,
        ),
    },
  ];
};

// planningScript: init → explore → plan → park in awaiting_approval.
function planningScript(runId: string): Timed[] {
  const run = getRun(runId);
  const iid = run?.issue_iid ?? 0;
  return [
    {
      delay: 400,
      step: () => appendMessage(runId, "status", null, { event: "init", model: "claude-sonnet-4-6" }),
    },
    ...say(runId, "lead", `Reading the PRD linked from issue #${iid} and mapping the affected code before proposing a plan.`, {
      input_tokens: 38_200,
      cache_read_input_tokens: 401_500,
      cache_creation_input_tokens: 2_000,
      output_tokens: 14_800,
    }),
    ...tool(runId, "lead", "Read", { file_path: "prds/13-worker-metrics.md" }, "# PRD 13 — Worker heartbeat metrics\n\nExpose heartbeat freshness per worker…"),
    ...tool(runId, "lead", "Grep", { pattern: "heartbeat", path: "api/internal" }, "api/internal/handler/worker.go:88\napi/internal/store/queries/workers.sql:14\napi/internal/poller/sweeper.go:31"),
    // PRD #116: a handled guardrail denial (the live #115 case — spawning the SDK
    // built-in `Explore` subagent). `error: true` sets is_error, so the feed can
    // demo the neutral "⊘ blocked" chip in a STREAMING run, not just the fixtures.
    ...tool(
      runId,
      "lead",
      "Agent",
      { subagent_type: "Explore", description: "map the heartbeat plumbing" },
      "denied by guardrail: only the run's assembled subagents may be invoked",
      { error: true },
    ),
    {
      delay: 1200,
      step: () =>
        appendMessage(runId, "thinking", "lead", {
          text: "The sweeper already computes staleness; the metrics endpoint should reuse that rather than duplicating the window arithmetic. A read-only /api/metrics/workers handler plus one sqlc query should cover it.",
        }),
    },
    ...tool(runId, "lead", "Read", { file_path: "api/internal/poller/sweeper.go" }, "func (e *Engine) sweep(ctx context.Context) { … WORKER_HEARTBEAT_STALE … }"),
    {
      delay: 1600,
      step: () => {
        appendMessage(runId, "plan", "lead", { text: SAMPLE_PLAN() });
        // PRD #40: the plan TURN ends with its own result frame (cumulative usage so
        // far), so the per-phase table shows a "Plan" phase distinct from implement.
        appendMessage(runId, "status", null, {
          event: "result",
          subtype: "success",
          duration_ms: 5 * 60_000,
          num_turns: 9,
          total_cost_usd: 0.24,
          usage: { input_tokens: 21_400, cache_read_input_tokens: 188_000, cache_creation_input_tokens: 0, output_tokens: 6_100 },
          modelUsage: { "claude-sonnet-5": { inputTokens: 21_400, outputTokens: 6_100, cacheReadInputTokens: 188_000, cacheCreationInputTokens: 0, costUSD: 0.24 } },
        });
        appendMessage(runId, "status", null, { text: "plan submitted — awaiting approval" });
        // PRD #122: at the gate the milestones are PRE-APPROVAL candidates, not yet
        // frozen. Clear any frozen state so the gate shows the candidate list cleanly;
        // approval (the first iteration bump below) freezes them into `milestones`.
        patchRun(runId, {
          status: "awaiting_approval",
          plan_md: SAMPLE_PLAN(),
          milestones_candidate: HEARTBEAT_MILESTONES,
          milestones: null,
          milestones_completed: null,
          milestones_in_progress: null,
        });
      },
    },
  ];
}

// The scripted run's subagent invocations. Module-level so a lane's frames share one
// identity across the whole script — a per-call literal would silently mint a new lane
// on every frame, which is the failure this feature exists to prevent.
const ATTR_A: Attr = { instance: "toolu_01mockCoderA", label: "metrics query + handler" };
const ATTR_B: Attr = { instance: "toolu_01mockCoderB", label: "route wiring + tests" };
const ATTR_REVIEW: Attr = { instance: "toolu_01mockReview", label: "review the diff" };

// implementScript: approve → implement ⇄ review → push → MR → completed.
function implementScript(runId: string): Timed[] {
  const run = getRun(runId);
  const iid = run?.issue_iid ?? 0;
  const branch = `agent/issue-${iid}`;
  return [
    // PRD #99: TWO parallel `coder` invocations, deliberately INTERLEAVED rather than
    // run to completion one after the other. Watching this live is the only way to see
    // the behaviour the feature exists for — a second `agent_instance` opening its own
    // lane mid-run, and each one's later turns folding back into its OWN lane instead
    // of spawning a fresh bar. Contiguous frames would look identical under the old
    // consecutive-author grouping, so the interleaving is the point, not decoration.
    ...say(runId, "coder", "Plan approved — implementing. Adding the query, the handler, and wiring the route.", {
      input_tokens: 51_600,
      cache_read_input_tokens: 583_900,
      cache_creation_input_tokens: 0,
      output_tokens: 24_100,
    }, ATTR_A),
    ...tool(runId, "coder", "Edit", { file_path: "api/internal/store/queries/workers.sql" }, "ok", { attr: ATTR_A }),
    // Unit B opens its lane here, several frames in — the mid-run lane birth.
    ...tool(runId, "coder", "Write", { file_path: "api/internal/handler/routes.go" }, "ok", { attr: ATTR_B }),
    ...tool(runId, "coder", "Bash", { command: "cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate" }, "sqlc: wrote internal/store/workers.sql.go", { attr: ATTR_A }),
    ...tool(runId, "coder", "Write", { file_path: "api/internal/handler/metrics.go" }, "ok", { attr: ATTR_A }),
    ...tool(
      runId,
      "coder",
      "Bash",
      { command: "cd api && go test ./internal/handler/..." },
      "--- FAIL: TestWorkerMetrics_Stale (0.02s)\n    metrics_test.go:41: staleness window off by one",
      // Unit A's failure: it is A that wrote metrics.go a frame earlier, and A that
      // diagnoses and fixes it below. Attributing the FAIL to B left a reader who
      // expanded both lanes finding the diagnosis in the wrong unit.
      { error: true, runFor: 2600, attr: ATTR_A },
    ),
    ...say(runId, "coder", "The staleness comparison used `>` where the sweeper uses `>=`. Aligning with the sweeper and re-running.", undefined, ATTR_A),
    ...tool(runId, "coder", "Edit", { file_path: "api/internal/handler/metrics.go" }, "ok", { attr: ATTR_A }),
    ...tool(runId, "coder", "Bash", { command: "cd api && go test ./..." }, "ok  \tuzi/api/internal/handler\t0.31s\nok  \tuzi/api/internal/store\t0.44s", { runFor: 2800, attr: ATTR_B }),
    {
      delay: 900,
      // PRD #122: the plan is approved and being implemented, so FREEZE the candidate
      // milestones into the approved list and advance progress — two reported complete,
      // one in progress — alongside the iteration bump. This is where the checklist and
      // the M{done}/{total} badge start moving in the live demo.
      step: () =>
        patchRun(runId, {
          iteration_count: 1,
          milestones: HEARTBEAT_MILESTONES,
          milestones_candidate: null,
          milestones_completed: ["hb-1", "hb-2"],
          milestones_in_progress: ["hb-3"],
        }),
    },
    ...say(
      runId,
      "reviewer",
      "Reviewed the diff: the endpoint is read-only, reuses the sweeper's staleness rule, and the new query is covered by tests. No blocking findings.",
      { input_tokens: 18_900, cache_read_input_tokens: 149_700, cache_creation_input_tokens: 0, output_tokens: 7_600 },
      ATTR_REVIEW,
    ),
    {
      delay: 800,
      step: () => appendMessage(runId, "status", null, { text: `pushing branch ${branch} and opening the MR` }),
    },
    {
      delay: 1600,
      step: () => {
        appendMessage(runId, "status", null, {
          event: "result",
          subtype: "success",
          duration_ms: 21 * 60_000 + 44_000,
          num_turns: 61,
          total_cost_usd: 1.87,
          // PRD #40: cumulative run usage (the strip/per-phase table fold from here).
          usage: {
            input_tokens: 114_400,
            cache_read_input_tokens: 1_170_000,
            cache_creation_input_tokens: 0,
            output_tokens: 48_200,
          },
          modelUsage: {
            "claude-sonnet-5": {
              inputTokens: 114_400,
              outputTokens: 48_200,
              cacheReadInputTokens: 1_170_000,
              cacheCreationInputTokens: 0,
              costUSD: 1.87,
            },
          },
        });
        patchRun(runId, {
          status: "completed",
          mr_iid: 57,
          branch,
          iteration_count: 2,
          // PRD #122: the run finished — every milestone reported complete, none left in
          // progress, so the checklist reads a full count on the completed hero.
          milestones: HEARTBEAT_MILESTONES,
          milestones_completed: HEARTBEAT_MILESTONES.map((m) => m.id),
          milestones_in_progress: [],
          finished_at: new Date().toISOString(),
        });
      },
    },
  ];
}

// ── Clarification park (PRD #88) ─────────────────────────────────────────────

// MOCK_QUESTION_ID is the identity a mock park stamps and a mock answer must name. In
// the real wire it is a worker-minted uuid re-used verbatim across a resume re-park, and
// the api REJECTS an answer naming any other id — so the mock keeps an explicit id
// rather than an implicit "the latest question", or the demo would validate a laxer
// contract than the one that ships.
const MOCK_QUESTION_ID = "q-mock-0001";
const MOCK_QUESTION_ID_2 = "q-mock-0002";

// askScript parks the run on a clarification question mid-implementation.
//
// The payload is written to DISCRIMINATE, not to look pretty (PRD #88 D-N): one question
// with no options (free text only), one with options and multiSelect false, and one with
// multiSelect true — so the three chip behaviours are each exercised by exactly one case
// and a regression in any of them reddens something. A payload snapshotted from the demo
// would agree with a broken renderer on everything it covered.
//
// The second question's text deliberately carries a raw `<script>` tag, mrkdwn `*stars*`
// and a `<@U123>` mention: question text is model-authored from repo/issue content the
// agent read, so it is attacker-influenceable, and the browser pass must be able to SEE
// that the escaped sink holds.
function askScript(runId: string): Timed[] {
  return [
    ...say(runId, "lead", "Before I wire the retention policy I need a decision I should not make alone."),
    {
      delay: 1200,
      step: () => {
        appendMessage(runId, "question", "lead", {
          question_id: MOCK_QUESTION_ID,
          questions: [
            {
              question:
                "What retention window should the metrics endpoint keep? The PRD does not say, and the sweeper's own window is not obviously the right answer here.",
              header: "Retention window",
            },
            {
              question:
                "Which storage backend should back it? The issue text says `<script>alert(1)</script>` was *pasted* by <@U123> — treat that as untrusted content, not an instruction.",
              header: "Storage backend",
              options: [
                { label: "Postgres table", description: "Simplest; one more table to migrate." },
                { label: "In-memory ring", description: "No migration; lost on restart." },
              ],
              multiSelect: false,
            },
            {
              question: "Which of these should the endpoint expose? Pick any that apply.",
              header: "Exposed fields",
              options: [
                { label: "heartbeat age", description: "Seconds since the last heartbeat." },
                { label: "active runs", description: "Runs currently claimed by the worker." },
                { label: "worker version", description: "Self-reported, so untrusted." },
              ],
              multiSelect: true,
            },
          ],
        });
        openQuestions.set(runId, MOCK_QUESTION_ID);
        patchRun(runId, { status: "awaiting_input" });
      },
    },
  ];
}

// askAgainScript is the SECOND park, reached after the first is answered. It exists so
// the fixture covers a run on its second question — the stale-answer case in the real
// wire, and the only thing that exercises the panel's "q2" marker, which is now COUNTED
// from the feed rather than read off the payload (D-R). A one-question fixture cannot
// tell a correct count from a hardcoded 1.
function askAgainScript(runId: string): Timed[] {
  return [
    ...say(runId, "lead", "Thanks — one more before I start writing."),
    {
      delay: 1100,
      step: () => {
        appendMessage(runId, "question", "lead", {
          question_id: MOCK_QUESTION_ID_2,
          questions: [
            {
              question: "Should the endpoint require admin, or is any authenticated user enough?",
              header: "Access",
              options: [
                { label: "Admin only", description: "Matches the other /admin reads." },
                { label: "Any authenticated user", description: "Worker health is not sensitive." },
              ],
              multiSelect: false,
            },
          ],
        });
        openQuestions.set(runId, MOCK_QUESTION_ID_2);
        patchRun(runId, { status: "awaiting_input" });
      },
    },
  ];
}

// revisedPlanScript: reject → acknowledge → new plan → park again.
function revisedPlanScript(runId: string, reason: string): Timed[] {
  return [
    ...say(runId, "lead", `Understood — revising the plan: ${reason || "incorporating your feedback"}.`),
    ...tool(runId, "lead", "Grep", { pattern: "prometheus", path: "api" }, "api/go.mod:14"),
    {
      delay: 1500,
      step: () => {
        const plan = `${SAMPLE_PLAN()}\n\n> Revised after rejection: ${reason || "(no reason given)"}`;
        appendMessage(runId, "plan", "lead", { text: plan });
        appendMessage(runId, "status", null, { text: "revised plan submitted — awaiting approval" });
        // PRD #122: the re-gate still shows the milestones as PRE-APPROVAL candidates.
        patchRun(runId, {
          status: "awaiting_approval",
          plan_md: plan,
          milestones_candidate: HEARTBEAT_MILESTONES,
          milestones: null,
          milestones_completed: null,
          milestones_in_progress: null,
        });
      },
    },
  ];
}

// ── Public API ───────────────────────────────────────────────────────────────

// ensureLive starts a run's planning script exactly once, the first time
// something watches it (socket subscribe or REST fetch of a running seeded run).
export function ensureLive(runId: string) {
  const run = getRun(runId);
  if (!run || started.has(runId)) return;
  // Chat conversations (PRD #39) are driven by scheduleChatReply, not the
  // issue-run planning script — subscribing to one must never inject a plan flow.
  if (run.kind === "chat") return;
  if (run.status !== "running" && run.status !== "queued" && run.status !== "claimed") return;
  started.add(runId);
  schedule(runId, planningScript(runId));
}

// startNewRun drives a board-started run through the queue → claim → planning
// lifecycle so the demo shows the real state machine, just faster.
export function startNewRun(runId: string) {
  started.add(runId);
  schedule(runId, [
    { delay: 700, step: () => patchRun(runId, { status: "claimed", worker_id: "w-laptop", claimed_at: new Date().toISOString() }) },
    {
      delay: 900,
      step: () => {
        patchRun(runId, { status: "running", started_at: new Date().toISOString() });
        schedule(runId, planningScript(runId));
      },
    },
  ]);
}

// ── Chat reply engine (PRD #39) ──────────────────────────────────────────────

// A crude keyword lift so the scripted Grep looks like it searched for something
// relevant to the user's question.
function searchTerm(userText: string): string {
  const m = userText.match(/\b([a-z][a-z_]{4,})\b/i);
  return m ? m[1].toLowerCase() : "run";
}

// scheduleChatReply simulates the chat agent answering a turn: a brief prose
// reply, one read-only tool call against the baked source (/opt/uzi-src), then a
// file-citing answer. When the user asks to file/propose something, it appends a
// human-gated issue proposal (registered so confirm/dismiss can resolve it).
export function scheduleChatReply(runId: string, userText: string) {
  const wantsIssue = /\b(issue|bug|feature|propose|file (an?|the)|open (an?|the)|create (an?|the))\b/i.test(userText);
  const term = searchTerm(userText);
  const steps: Timed[] = [
    ...say(runId, "chat", "Looking into that against uzi's baked source…"),
    ...tool(
      runId,
      "chat",
      "Grep",
      { pattern: term, path: "/opt/uzi-src/api" },
      "api/internal/workersvc/service.go:388\napi/internal/store/queries/runtime.sql:154",
    ),
    ...say(
      runId,
      "chat",
      `Here's what I found: the relevant path is \`api/internal/workersvc/service.go\`. Ask me to go deeper on any part.`,
    ),
  ];
  if (wantsIssue) {
    steps.push({
      delay: 900,
      step: () => {
        const proposal: IssueProposal = {
          id: nextProposalId(),
          run_id: runId,
          repo_path: "vtmocanu/uzi",
          title: truncateTitle(userText),
          description: `Draft from our conversation:\n\n> ${userText}\n\nRefine before creating if needed.`,
          labels: ["PRD"],
          status: "pending",
          created_at: new Date().toISOString(),
        };
        putProposal(proposal);
        appendMessage(runId, "text", "chat", {
          text: "Here is a draft — review it and click **Create issue** if it looks right.",
        });
        appendMessage(runId, "proposal", "chat", proposal);
      },
    });
  }
  schedule(runId, steps);
}

function truncateTitle(s: string): string {
  const t = s.trim().replace(/\s+/g, " ");
  return t.length > 72 ? `${t.slice(0, 71)}…` : t;
}

/** A refusal the real api answers with, reproduced verbatim so a surface built against
 *  the mock meets the same status and the same wording it will meet in production.
 *  `mockApi` turns it into the ApiError the client would receive. */
export interface InputRejection {
  status: number;
  message: string;
}

// The three the api returns for an `answer` (handler/workers.go). 409 rather than 400 for
// the two conflict cases: the request was well-formed and the caller did nothing wrong,
// the question simply moved on.
const NOT_PARKED: InputRejection = { status: 409, message: "run is not waiting for an answer" };
const STALE_ANSWER: InputRejection = {
  status: 409,
  message: "that question has already been answered or replaced",
};
const MALFORMED_ANSWER: InputRejection = {
  status: 400,
  message: "invalid answer: body must be JSON {question_id, answers}",
};

// handleInput mirrors POST /api/runs/:id/inputs.
//
// 🔴 THIS SWITCH IS A CONTRACT, NOT A CONVENIENCE (PRD #88 D-N). `mockApi` is what every
// web/ test and every browser validation exercises, and it is the one implementation we
// do NOT ship — so a kind that falls through here produces a demo that lies and a suite
// that asserts a fiction, with no typecheck and no failing test to say so. That is not
// hypothetical: `revise_plan` landed in `RunInputKind` with PRD #41 and fell straight
// through this switch, so the mock rendered a Request-changes button that silently
// no-opped, and PRD #88's `answer` would have reproduced it exactly.
//
// The `never` guard in the default arm is the part that matters more than any case:
// adding a kind to `RunInputKind` now fails `tsc` here until it is handled, so the next
// one cannot repeat this quietly.
//
// It returns a rejection where the REAL api rejects, rather than resolving 200 and doing
// nothing. Silence is what made the revise_plan defect invisible for a whole PRD; a mock
// that swallows a refusal teaches a surface to be built against a laxer contract than
// the one that ships.
export function handleInput(runId: string, kind: RunInputKind, body: string): InputRejection | null {
  const run = getRun(runId);
  if (!run) return null;
  switch (kind) {
    case "approve_plan":
      if (run.status !== "awaiting_approval") return null;
      patchRun(runId, { status: "running" });
      appendMessage(runId, "status", null, { text: "plan approved — resuming the session" });
      // PRD #88: the approved run stops to ask before it starts writing, so the demo
      // shows the clarification park on the path a user actually walks.
      schedule(runId, askScript(runId));
      return null;
    case "reject_plan":
      if (run.status !== "awaiting_approval") return null;
      patchRun(runId, { status: "running" });
      appendMessage(runId, "status", null, { text: "plan rejected — sending feedback to the agent" });
      schedule(runId, revisedPlanScript(runId, body));
      return null;
    // PRD #41. Distinct from reject_plan on the wire and here: a revision keeps the run
    // at the gate conceptually (the plan returns for approval) and the UI shows a
    // "revising" parked state derived from the `plan_revising` message, which the mock
    // must therefore actually emit. Missing since #41 landed; fixed with #88 rather than
    // left as a knowingly-wrong mock, since the exhaustiveness guard below would
    // otherwise have to be introduced already failing.
    case "revise_plan": {
      if (run.status !== "awaiting_approval") return null;
      const round = countPlanFeedback(runId) + 1;
      appendMessage(runId, "plan_feedback", null, { feedback: body });
      appendMessage(runId, "plan_revising", "lead", { round });
      schedule(runId, revisedPlanScript(runId, body));
      return null;
    }
    case "cancel":
      clearTimers(runId);
      openQuestions.delete(runId);
      appendMessage(runId, "status", null, { text: "cancel requested" });
      patchRun(runId, {
        status: run.status === "queued" ? "cancelled" : "failed",
        failure_reason: run.status === "queued" ? null : "run cancelled",
        // The server stamps the deliberate-stop signal (PRD #33), so a cancel that
        // lands as `failed` still renders the calm "stopped" badge, not "failed".
        stop_kind: "cancelled",
        finished_at: new Date().toISOString(),
      });
      return null;
    case "follow_up":
      // A follow-up is NEVER written to run_messages (PRD #95 Decision 4) — it lives
      // only in run_user_inputs (the steer queue), so the mock must not echo it into
      // the activity log the way it used to. It surfaces in the SteerQueueCard, not
      // here; the worker's eventual response rides the normal message stream.
      return null;
    // PRD #88. The two server-side rejections are reproduced deliberately, because they
    // are what stops a surface built against the mock from being laxer than the wire:
    // an answer for a run that is not parked is a 409, and one naming a question other
    // than the open one is a 409 too (typically a reply to question N arriving after the
    // lead asked N+1).
    case "answer": {
      if (run.status !== "awaiting_input") return NOT_PARKED;
      const parsed = parseAnswerInput(body);
      if (!parsed) return MALFORMED_ANSWER;
      const open = openQuestions.get(runId);
      if (!open || parsed.question_id !== open) return STALE_ANSWER;
      openQuestions.delete(runId);
      appendMessage(runId, "answer", "lead", { answers: parsed.answers });
      patchRun(runId, { status: "running" });
      appendMessage(runId, "status", null, { text: "answer received — resuming the session" });
      // The first answer leads to a SECOND question, the second to the
      // implementation. Keyed on which question was answered, not on a counter, so the
      // branch is readable from the fixture.
      schedule(runId, open === MOCK_QUESTION_ID ? askAgainScript(runId) : implementScript(runId));
      return null;
    }
    default: {
      // Exhaustiveness guard — see the header. If this line stops compiling, a new
      // RunInputKind was added and this switch must learn it; do NOT widen the type
      // here to make it build.
      const unhandled: never = kind;
      void unhandled;
      return null;
    }
  }
}

// countPlanFeedback counts the revision rounds already recorded, so the `plan_revising`
// message carries the round number the panel's counter reads.
function countPlanFeedback(runId: string): number {
  return listMessages(runId).filter((m) => m.kind === "plan_feedback").length;
}

// parseAnswerInput reads the `answer` steering body. It is JSON — the only kind that is
// (PRD #88 D-P C2) — because an answer must name the question it answers. A malformed
// body FAILS SAFE (null → the mock drops it), which is the server's rule too and the
// deliberate opposite of parseAgentSelection's malformed→default fallback: an answer
// that cannot say what it answers has no safe reading.
function parseAnswerInput(body: string): { question_id: string; answers: string[] } | null {
  try {
    const raw: unknown = JSON.parse(body);
    if (!raw || typeof raw !== "object") return null;
    const rec = raw as Record<string, unknown>;
    const id = typeof rec["question_id"] === "string" ? rec["question_id"].trim() : "";
    if (id === "") return null;
    const answers = Array.isArray(rec["answers"])
      ? rec["answers"].filter((a): a is string => typeof a === "string")
      : [];
    return { question_id: id, answers };
  } catch {
    return null;
  }
}
