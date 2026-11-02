// PRD #3708 U5 (M2): a Codex lead's gated plan is cross-checked on Claude exactly like a Claude
// lead's, and every outcome opens the ordinary human gate. Mirrors gated-plan-cross-check.test.ts
// (Claude lead) with a claim carrying claim.secrets.codex.
import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, StateRequest } from "../src/protocol.js";
import type { PlanVerdict } from "../src/steering.js";
import type { PlanCrossCheckStateRequest } from "../src/client.js";
import { api, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { feed, freshClaim, gates, newApi, row, send } from "./gate-revision-harness.js";

installHarness();
beforeEach(() => {
  newApi();
  api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
});

const PLAN = "# PLAN\n- local proposal";
const NORMALIZED = "# PLAN\n- server normalized proposal";
const digest = "b".repeat(64);
const child = "22222222-2222-4222-8222-222222222222";
const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 };
// Runner paths that reach native planning capture walk Linux /proc; Linux CI runs them.
const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";
const PASS_LINE = /^plan cross-check: passed after (\d+) rounds?$/;

const codexSecrets = {
  forge_pat: "fixture-forge-pat-000000",
  codex: {
    auth_mode: "subscription" as const, access_token: "fixture-codex-access-token-abc123",
    capability: "fixture-codex-capability-abc123", generation: 1, chatgpt_account_id: "verified-account",
    chatgpt_plan_type: null,
  },
};
function codexGatedClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return freshClaim(1, { kind: "issue", auto_approve: false, plan_cross_check_gated: true,
    plan_cross_check_codex_lead: true, secrets: codexSecrets, ...overrides });
}
function proof(runId: string) {
  return { lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
    claim_generation: 1, plan_cross_check_settled: true, gate_revision: 0,
    current_plan_sha256: createHash("sha256").update("").digest("hex") };
}
function canonicalOf(body: Record<string, unknown>) {
  return { plan_md: NORMALIZED, milestones: [{ title: "canonical", done: false }],
    required_capabilities: [], required_tools: [], size_class: "s",
    base_commit: body.base_commit, planning_diff: body.planning_diff };
}
function answer(runId: string, body: Record<string, unknown>, verdict: string, overrides = {}) {
  const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
  return { result: "candidate", round, checker_run_id: child, candidate_digest: digest,
    candidate_generation: 1, candidate, verdict, reason_class: verdict, findings: null,
    deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof(runId), ...overrides };
}
function humanApproves(runId: string) {
  api.onState(runId, (b) => { if (b.status === "awaiting_approval") send(runId, row("approve_plan")); });
}
function gateReason(g: StateRequest): string | undefined {
  return (g as PlanCrossCheckStateRequest).plan_cross_check_gate_reason ?? undefined;
}
function gatedExec(plans: string[] = [PLAN]) {
  const verdicts: PlanVerdict[] = [];
  const exec: Executor = { run: async (ctx: RunContext) => {
    for (const plan of plans) {
      const v = await ctx.gatePlan!(plan);
      verdicts.push(v);
      if (!(v.kind === "revise" && v.automatic)) break;
    }
    return { branch: ctx.branch };
  } };
  return { exec, verdicts };
}
async function start(exec: Executor, c: ClaimResponse) {
  const run = runner(exec, fakeGitlab().gitlab, undefined, { planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 });
  await run.execute(c);
}
const passLine = (runId: string) => feed(runId).filter((t) => PASS_LINE.test(t));

describe("gated plan cross-check: Codex lead", () => {
  it("APPROVE runs the check and opens one human gate with no reason", { skip: LINUX_CAPTURE }, async () => {
    const c = codexGatedClaim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", {
      candidate: canonicalOf(body), automatic_revision_limit: 2, automatic_rounds_enabled: true }) });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 1, "the Codex lead's gated candidate is submitted");
    const [gate, ...rest] = gates(c.run_id);
    assert.ok(gate && rest.length === 0, "exactly one human gate");
    assert.equal(gate.plan_md, NORMALIZED);
    assert.equal(gate.candidate_digest, digest);
    assert.equal(Object.hasOwn(gate, "plan_cross_check_gate_reason"), false, "no worker-declared reason on a pass");
    assert.equal(verdicts.length, 1);
    assert.equal(verdicts[0]!.kind, "approve");
    assert.notEqual((verdicts[0] as { approval?: string }).approval, "cross_check", "the human approved, not the checker");
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 1 round"]);
  });

  it("REVISE then APPROVE spares the human allowance and opens the gate after two rounds", { skip: LINUX_CAPTURE }, async () => {
    const c = codexGatedClaim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body,
      body.round === 1 ? "revise" : "approve", {
        candidate: canonicalOf(body), automatic_revision_limit: 2, automatic_rounds_enabled: true,
        findings: body.round === 1 ? { summary: "tighten", items: [] } : null }) });
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec([PLAN, `${PLAN} 2`]);
    await start(exec, c);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2]);
    assert.deepEqual(verdicts.map((v) => v.kind), ["revise", "approve"]);
    assert.ok(verdicts[0]!.kind === "revise" && verdicts[0]!.automatic, "the revise is automatic, not a human revise");
    assert.equal(gates(c.run_id).length, 1, "the automatic revise published no gate");
    assert.equal(Object.hasOwn(gates(c.run_id)[0]!, "plan_cross_check_gate_reason"), false);
    assert.deepEqual(passLine(c.run_id), ["plan cross-check: passed after 2 rounds"]);
  });

  it("an api without codex-lead support presents the ordinary gate and never submits", { skip: LINUX_CAPTURE }, async () => {
    const c = codexGatedClaim({ plan_cross_check_codex_lead: undefined });
    api.crossCheckHandler = () => { throw new Error("an unsupported Codex lead is never submitted"); };
    api.crossCheckLatestHandler = () => { throw new Error("no discovery for an unsupported Codex lead"); };
    humanApproves(c.run_id);
    const { exec, verdicts } = gatedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 0);
    assert.equal(api.crossCheckLatestRequests.length, 0);
    const [gate, ...rest] = gates(c.run_id);
    assert.ok(gate && rest.length === 0, "exactly one ordinary human gate");
    assert.equal(Object.hasOwn(gate, "candidate_digest"), false, "no pass binding");
    assert.equal(gate.plan_md, PLAN, "the lead's own plan is presented");
    assert.equal(gateReason(gate), "codex_lead_unsupported");
    assert.equal(passLine(c.run_id).length, 0);
    assert.equal(verdicts[0]!.kind, "approve");
  });
});
