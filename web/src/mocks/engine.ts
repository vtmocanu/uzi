// The mock run engine: timer-driven scripts that make a run feel alive with no
// backend. A script is a list of (delay, step) pairs; each step persists a
// message (store.appendMessage → broadcast) or patches run state. The plan step
// parks the run in awaiting_approval; handleInput() resumes/rejects/cancels it,
// mirroring the real worker protocol's semantics.

import type { IssueProposal, RunInputKind } from "../lib/api";
import { SAMPLE_PLAN } from "./data";
import { appendMessage, getRun, nextProposalId, patchRun, putProposal } from "./store";

type Step = () => void;
interface Timed {
  delay: number; // ms after the previous step
  step: Step;
}

const timers = new Map<string, number[]>();
const started = new Set<string>();

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
        patchRun(runId, { status: "awaiting_approval", plan_md: SAMPLE_PLAN() });
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
      { error: true, runFor: 2600, attr: ATTR_B },
    ),
    ...say(runId, "coder", "The staleness comparison used `>` where the sweeper uses `>=`. Aligning with the sweeper and re-running.", undefined, ATTR_A),
    ...tool(runId, "coder", "Edit", { file_path: "api/internal/handler/metrics.go" }, "ok", { attr: ATTR_A }),
    ...tool(runId, "coder", "Bash", { command: "cd api && go test ./..." }, "ok  \tuzi/api/internal/handler\t0.31s\nok  \tuzi/api/internal/store\t0.44s", { runFor: 2800, attr: ATTR_B }),
    {
      delay: 900,
      step: () => patchRun(runId, { iteration_count: 1 }),
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
          finished_at: new Date().toISOString(),
        });
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
        patchRun(runId, { status: "awaiting_approval", plan_md: plan });
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

// handleInput mirrors POST /api/runs/:id/inputs.
export function handleInput(runId: string, kind: RunInputKind, body: string) {
  const run = getRun(runId);
  if (!run) return;
  switch (kind) {
    case "approve_plan":
      if (run.status !== "awaiting_approval") return;
      patchRun(runId, { status: "running" });
      appendMessage(runId, "status", null, { text: "plan approved — resuming the session" });
      schedule(runId, implementScript(runId));
      return;
    case "reject_plan":
      if (run.status !== "awaiting_approval") return;
      patchRun(runId, { status: "running" });
      appendMessage(runId, "status", null, { text: "plan rejected — sending feedback to the agent" });
      schedule(runId, revisedPlanScript(runId, body));
      return;
    case "cancel":
      clearTimers(runId);
      appendMessage(runId, "status", null, { text: "cancel requested" });
      patchRun(runId, {
        status: run.status === "queued" ? "cancelled" : "failed",
        failure_reason: run.status === "queued" ? null : "run cancelled",
        // The server stamps the deliberate-stop signal (PRD #33), so a cancel that
        // lands as `failed` still renders the calm "stopped" badge, not "failed".
        stop_kind: "cancelled",
        finished_at: new Date().toISOString(),
      });
      return;
    case "follow_up":
      // A follow-up is NEVER written to run_messages (PRD #95 Decision 4) — it lives
      // only in run_user_inputs (the steer queue), so the mock must not echo it into
      // the activity log the way it used to. It surfaces in the SteerQueueCard, not
      // here; the worker's eventual response rides the normal message stream.
      return;
  }
}
