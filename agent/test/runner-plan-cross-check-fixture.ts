// Shared fixture for the runner-plan-cross-check suites (PRD #2460, U2 checked gate).
import { beforeEach } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import type { Logger } from "../src/log.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, runner } from "./runner-harness.js";
import { freshClaim, newApi } from "./gate-revision-harness.js";

export function installCheckedGateFixture(): void {
  beforeEach(() => {
    newApi();
    api.checkedTransport = true;
    api.usageHandler = () => ({ status: 200, body: {} });
  });
}
export const PLAN = "# PLAN\n- local proposal";
export const NORMALIZED = "# PLAN\n- server normalized proposal";
export const digest = "a".repeat(64);
export const child = "11111111-1111-4111-8111-111111111111";
export function diagnosticLogger() {
  const warnings: { message: string; fields?: Record<string, unknown> }[] = [];
  const log: Logger = { ...nullLogger(),
    warn: (message, fields) => { warnings.push({ message, fields }); },
    child: () => log,
  };
  return { log, warnings };
}
export const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 };

export function claim(generation = 1) { return freshClaim(generation, { kind: "issue", auto_approve: true, plan_cross_check_required: true }); }
export function proof(runId: string, generation = 1) {
  return { lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
    claim_generation: generation, plan_cross_check_settled: true, gate_revision: 0,
    current_plan_sha256: createHash("sha256").update("").digest("hex") };
}
export function answer(runId: string, body: Record<string, unknown>, verdict = "approve", reason = verdict, overrides = {}) {
  const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
  return { result: "candidate", round, checker_run_id: child, candidate_digest: digest,
    candidate_generation: 1, candidate, verdict, reason_class: reason, findings: null,
    deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof(runId), ...overrides };
}
export function checkedExec(before?: (ctx: RunContext) => Promise<void>) {
  const verdicts: PlanVerdict[] = [];
  const exec: Executor = { run: async (ctx) => {
    await before?.(ctx);
    const verdict = await ctx.gatePlan!(PLAN);
    verdicts.push(verdict);
    if (verdict.kind === "reject") throw new PlanRejectedError(verdict.reason);
    if (verdict.kind !== "approve") throw new Error("not approved");
    return { branch: ctx.branch };
  } };
  return { exec, verdicts };
}
export function start(exec: Executor, c = claim()) {
  const forge = fakeGitlab();
  const run = runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
    planApprovalTimeoutMs: 4000 });
  return Object.assign(run.execute(c).then(() => forge), { shutdown: () => run.shutdown() });
}
export function terminal(runId: string, condition: string) {
  const states = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  assert.ok(states.some((s) => s.status === "failed" && s.failure_reason?.includes(condition)),
    `named terminal failure: ${condition}; got ${JSON.stringify(states)}`);
  assert.ok(!states.some((s) => s.status === "running" && s.plan_md), "no unchecked approval");
  assert.ok(!states.some((s) => s.status === "completed" || s.status === "recovery_wait"), "no completion/recovery");
}

// Runner paths that reach native planning capture walk Linux /proc; Linux CI runs them.
export const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";
