// Issue #3985: the two runner seams the main settlement suite cannot reach.
//  - a REPLACEMENT revise swallowed by PlanRevisionRecovery (plan_revision_retry negotiated): the
//    settlement names the replacement's input id, read back from the recovery's dispatchSource();
//  - a human revise returned by an existing checked human wait through continueExistingPlanGate.
// Real RunRunner callbacks, WorkerClient and state choke point against the FakeApi; only the
// server's settlement answer is scripted.
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import type { Executor } from "../src/executor.js";
import { PlanRevisionReplacedError } from "../src/executor.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import type { PlanVerdict } from "../src/steering.js";
import { api, client, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { feed, freshClaim, gates, newApi, row, send, until } from "./gate-revision-harness.js";

installHarness();

const FEATURE = "clarification_revision_settlement_v1";

function scriptSettlement(): void {
  api.stateAckWire = (body, run) => body.status !== "clarification_settled" ? undefined
    : { status: 200, body: { run: { ...run, status: "awaiting_approval" }, disposition: "clarification_settled" } };
}

function answerQuestions(id: string): void {
  api.onState(id, body => {
    if (body.status === "awaiting_input" && body.open_question_id)
      send(id, row("answer", JSON.stringify({ question_id: body.open_question_id, answers: ["continue"] })));
  });
}

describe("#3985 clarification settlement: replacement revise under plan_revision_retry", () => {
  it("names the REPLACEMENT revise input id and gate revision, not the original", async t => {
    newApi();
    api.planRevisionRetries = true;
    client.protocolFeatures = [...client.protocolFeatures, "plan_revision_retry_v1", FEATURE];
    scriptSettlement();
    const claim = freshClaim(1, { plan_cross_check_required: true, auto_approve: false });
    const id = claim.run_id;
    answerQuestions(id);
    const seen: { first?: PlanVerdict; replacement?: PlanVerdict; answered?: boolean; error?: unknown } = {};
    const executor: Executor = {
      async run(ctx) {
        try {
          seen.first = await ctx.gatePlan!("# Plan A");
          assert.equal(seen.first.kind, "revise");
          await ctx.beforePlanRevisionTurn!();
          try {
            // redrives 0: the allowance is exhausted at once and the owner wait takes the replacement.
            await ctx.retryPlanRevisionTurn!(new TransientRecoveryError("inline transient", { providerOrigin: true }));
          } catch (error) {
            if (!(error instanceof PlanRevisionReplacedError)) throw error;
            seen.replacement = error.verdict;
          }
          assert.equal(seen.replacement?.kind, "revise", "the replacement was swallowed by the recovery");
          const answer = await ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]);
          seen.answered = answer.kind === "answer";
        } catch (error) {
          seen.error = error;
        }
        throw new Error("fixture reached its end");
      },
    };
    const r = runner(executor, fakeGitlab().gitlab, undefined, { planRevisionRetryPolicy: { redrives: 0 } });
    const done = r.execute(claim);
    t.after(async () => { send(id, row("cancel")); await done.catch(() => undefined); });
    assert.ok(await until(() => gates(id).length === 1), "the first gate was presented");
    const [original] = send(id, row("revise_plan", "I"));
    assert.ok(await until(() => feed(id).some(text => text.includes("retry allowance exhausted"))), "the allowance was exhausted");
    const [replacement] = send(id, row("revise_plan", "J"));
    assert.ok(await until(() => seen.answered === true || seen.error !== undefined, 10_000), "the fixture finished");
    assert.equal(seen.error, undefined, String(seen.error));
    assert.notEqual(replacement!.id, original!.id);
    assert.equal((seen.first as { inputId?: number }).inputId, original!.id);
    const replaced = seen.replacement as { inputId?: number; sourceGateRevision?: number };
    assert.equal(replaced.inputId, replacement!.id);
    const settlements = api.states.filter(s => s.body.status === "clarification_settled").map(s => s.body);
    assert.equal(settlements.length, 1);
    assert.equal(settlements[0]!.clarification_settlement?.revision_input_id, replacement!.id,
      "the settlement binds the replacement, not the superseded original");
    assert.equal(settlements[0]!.clarification_settlement?.gate_revision, replaced.sourceGateRevision);
    assert.equal(api.states.some(s => s.body.status === "running" && api.states.indexOf(s) > api.states.findIndex(q => q.body.status === "awaiting_input")), false,
      "an answer to a revise clarification is not plan approval");
  });
});

describe("#3985 clarification settlement: human revise through continueExistingPlanGate", () => {
  it("a checked human wait resolving to a human revise binds the later settlement to that revise", async t => {
    newApi();
    api.checkedTransport = true;
    api.usageHandler = () => ({ status: 200, body: {} });
    client.protocolFeatures = [...client.protocolFeatures, FEATURE];
    scriptSettlement();
    const claim = freshClaim(1, { kind: "issue", auto_approve: false, plan_cross_check_gated: true });
    const id = claim.run_id;
    api.crossCheckHandler = ({ runId, body }) => {
      const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
      return { status: 200, body: { result: "candidate", round, checker_run_id: "22222222-2222-4222-8222-222222222222",
        candidate_digest: "b".repeat(64), candidate_generation: 1,
        candidate: { ...candidate, plan_md: "# PLAN", milestones: [{ title: "canonical", done: false }],
          required_capabilities: [], required_tools: [], size_class: "s" },
        verdict: "approve", reason_class: "approve", findings: null, automatic_revision_limit: 2, automatic_rounds_enabled: true,
        deadline_at: new Date(Date.now() + 60_000).toISOString(),
        lead_last_seq: Math.max(0, ...api.messages(runId).map(m => m.seq)), claim_generation: 1, plan_cross_check_settled: true,
        gate_revision: 0, current_plan_sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" } };
    };
    answerQuestions(id);
    const seen: { verdict?: PlanVerdict; answered?: boolean; error?: unknown; otherwise: number } = { otherwise: 0 };
    const executor: Executor = {
      async run(ctx) {
        try {
          void ctx.gatePlan!("# PLAN").catch(() => undefined);
          assert.ok(await until(() => gates(id).length === 1), "the checked human gate was presented");
          const pending = ctx.continueExistingPlanGate!(async () => { seen.otherwise++; return { kind: "reject", reason: "re-planned" } as PlanVerdict; });
          send(id, row("revise_plan", "revise it"));
          seen.verdict = await pending;
          assert.equal(seen.verdict.kind, "revise");
          const answer = await ctx.askUser!([{ question: "Continue planning?", header: "Plan" }]);
          seen.answered = answer.kind === "answer";
        } catch (error) {
          seen.error = error;
        }
        throw new Error("fixture reached its end");
      },
    };
    const r = runner(executor, fakeGitlab().gitlab, undefined, {
      planCrossCheckTiming: { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 }, planApprovalTimeoutMs: 4000 });
    const done = r.execute(claim);
    t.after(async () => { send(id, row("cancel")); await done.catch(() => undefined); });
    assert.ok(await until(() => seen.answered === true || seen.error !== undefined, 10_000), "the fixture finished");
    assert.equal(seen.error, undefined, String(seen.error));
    assert.equal(seen.otherwise, 0, "the existing human wait was used");
    const revise = seen.verdict as { inputId?: number; sourceGateRevision?: number };
    assert.ok(revise.inputId !== undefined && revise.sourceGateRevision !== undefined, JSON.stringify(seen.verdict));
    const settlements = api.states.filter(s => s.body.status === "clarification_settled").map(s => s.body);
    assert.equal(settlements.length, 1, api.states.map(s => s.body.status).join(","));
    assert.equal(settlements[0]!.clarification_settlement?.revision_input_id, revise.inputId);
    assert.equal(settlements[0]!.clarification_settlement?.gate_revision, revise.sourceGateRevision);
  });
});
