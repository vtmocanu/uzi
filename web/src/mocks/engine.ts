// The mock run engine: timer-driven scripts that make a run feel alive with no
// backend. A script is a list of (delay, step) pairs; each step persists a
// message (store.appendMessage → broadcast) or patches run state. The plan step
// parks the run in awaiting_approval; handleInput() resumes/rejects/cancels it,
// mirroring the real worker protocol's semantics.

import type { RunInputKind } from "../lib/api";
import { SAMPLE_PLAN } from "./data";
import { appendMessage, getRun, patchRun } from "./store";

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

const say = (runId: string, agent: string | null, text: string): Timed[] => [
  { delay: 900, step: () => appendMessage(runId, "text", agent, { text }) },
];

const tool = (
  runId: string,
  agent: string,
  name: string,
  input: Record<string, unknown>,
  result: string,
  opts: { error?: boolean; runFor?: number } = {},
): Timed[] => {
  const id = `mock-${Math.random().toString(36).slice(2, 9)}`;
  return [
    { delay: 700, step: () => appendMessage(runId, "tool_use", agent, { id, name, input }) },
    {
      delay: opts.runFor ?? 1400,
      step: () =>
        appendMessage(runId, "tool_result", agent, {
          tool_use_id: id,
          content: result,
          ...(opts.error ? { is_error: true } : {}),
        }),
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
    ...say(runId, "lead", `Reading the PRD linked from issue #${iid} and mapping the affected code before proposing a plan.`),
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
        appendMessage(runId, "status", null, { text: "plan submitted — awaiting approval" });
        patchRun(runId, { status: "awaiting_approval", plan_md: SAMPLE_PLAN() });
      },
    },
  ];
}

// implementScript: approve → implement ⇄ review → push → MR → completed.
function implementScript(runId: string): Timed[] {
  const run = getRun(runId);
  const iid = run?.issue_iid ?? 0;
  const branch = `agent/issue-${iid}`;
  return [
    ...say(runId, "coder", "Plan approved — implementing. Adding the query, the handler, and wiring the route."),
    ...tool(runId, "coder", "Edit", { file_path: "api/internal/store/queries/workers.sql" }, "ok"),
    ...tool(runId, "coder", "Bash", { command: "cd api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0 generate" }, "sqlc: wrote internal/store/workers.sql.go"),
    ...tool(runId, "coder", "Write", { file_path: "api/internal/handler/metrics.go" }, "ok"),
    ...tool(
      runId,
      "coder",
      "Bash",
      { command: "cd api && go test ./internal/handler/..." },
      "--- FAIL: TestWorkerMetrics_Stale (0.02s)\n    metrics_test.go:41: staleness window off by one",
      { error: true, runFor: 2600 },
    ),
    ...say(runId, "coder", "The staleness comparison used `>` where the sweeper uses `>=`. Aligning with the sweeper and re-running."),
    ...tool(runId, "coder", "Edit", { file_path: "api/internal/handler/metrics.go" }, "ok"),
    ...tool(runId, "coder", "Bash", { command: "cd api && go test ./..." }, "ok  \tuzi/api/internal/handler\t0.31s\nok  \tuzi/api/internal/store\t0.44s", { runFor: 2800 }),
    {
      delay: 900,
      step: () => patchRun(runId, { iteration_count: 1 }),
    },
    ...say(
      runId,
      "reviewer",
      "Reviewed the diff: the endpoint is read-only, reuses the sweeper's staleness rule, and the new query is covered by tests. No blocking findings.",
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
          duration_ms: 4 * 60_000,
          num_turns: 24,
          total_cost_usd: 0.92,
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
        finished_at: new Date().toISOString(),
      });
      return;
    case "follow_up":
      appendMessage(runId, "status", null, { text: `follow-up from ${"you"}: ${body}` });
      schedule(runId, say(runId, "lead", `Noted — folding that into the current work: “${body}”.`));
      return;
  }
}
