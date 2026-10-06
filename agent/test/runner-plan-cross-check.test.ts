import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { MessageBatcher } from "../src/batcher.js";
import { UsageRecorder } from "../src/usage-recorder.js";
import type { PlanCrossCheckCandidate, PlanCrossCheckStateRequest } from "../src/client.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, homeDir, installHarness, runner, runnerWith, fakeGitlab,
  planWithMilestonesThenDoneQuery } from "./runner-harness.js";
import { freshClaim, gates, holdGateAck, newApi, row, send, routed, statuses, until } from "./gate-revision-harness.js";

installHarness();
beforeEach(() => {
  newApi();
  api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
});
const PLAN = "# PLAN\n- local proposal";
const NORMALIZED = "# PLAN\n- server normalized proposal";
const digest = "a".repeat(64);
const child = "11111111-1111-4111-8111-111111111111";
const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 };

function claim() { return freshClaim(1, { kind: "issue", auto_approve: true, plan_cross_check_required: true }); }
function proof(runId: string, generation = 1) {
  return { lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
    claim_generation: generation, plan_cross_check_settled: true, gate_revision: 0,
    current_plan_sha256: createHash("sha256").update("").digest("hex") };
}
function answer(runId: string, body: Record<string, unknown>, verdict = "approve", reason = verdict, overrides = {}) {
  const { stage: _stage, claim_generation: _generation, ...candidate } = body;
  return { result: "candidate", round: 1, checker_run_id: child, candidate_digest: digest,
    candidate_generation: 1, candidate, verdict, reason_class: reason, findings: null,
    deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof(runId), ...overrides };
}
function checkedExec(before?: (ctx: RunContext) => Promise<void>) {
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
async function start(exec: Executor, c = claim()) {
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
    planApprovalTimeoutMs: 4000 }).execute(c);
  return forge;
}
function terminal(runId: string, condition: string) {
  const states = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  assert.ok(states.some((s) => s.status === "failed" && s.failure_reason?.includes(condition)),
    `named terminal failure: ${condition}; got ${JSON.stringify(states)}`);
  assert.ok(!states.some((s) => s.status === "running" && s.plan_md), "no unchecked approval");
  assert.ok(!states.some((s) => s.status === "completed" || s.status === "recovery_wait"), "no completion/recovery");
}

describe("U2 real runner checked gate", () => {
  for (const httpStatus of [400, 409]) {
    it(`negative canonical storage ACK ${httpStatus} prevents implementation`, async () => {
      const c = claim();
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body) });
      api.failStateWhen(c.run_id, (b) => b.status === "running" && b.candidate_digest === digest,
        { httpStatus, runStatus: "cancelled" });
      const { exec, verdicts } = checkedExec();
      await start(exec, c);
      assert.deepEqual(verdicts, [], "no implementation verdict after negative storage ACK");
      assert.ok(!statuses(c.run_id).includes("completed"));
    });
  }
  for (const decision of ["approve", "revise"]) {
    it(`server wall park at ${decision} ACK preserves clone and emits no terminal`, async () => {
      const c = claim();
      let clone = "";
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, decision, decision) });
      api.failStateWhen(c.run_id, (b) => decision === "approve"
        ? b.status === "running" && b.candidate_digest === digest : b.status === "awaiting_approval",
        { httpStatus: 409, runStatus: "paused", disposition: "stale_claim", holdReason: "budget_exhausted" });
      const { exec, verdicts } = checkedExec(async (ctx) => { clone = ctx.worktreePath; });
      const forge = fakeGitlab();
      await runnerWith(() => ({ executor: exec, homeDir }), forge.gitlab, undefined, nullLogger(),
        { planCrossCheckTiming: timing, planApprovalTimeoutMs: 2000 }).execute(c);
      assert.deepEqual(verdicts, [], "park does not approve");
      const observed = { statuses: statuses(c.run_id), cloneRetained: fs.existsSync(clone),
        homeRetained: fs.existsSync(homeDir) };
      assert.ok(!observed.statuses.includes("failed") && observed.cloneRetained && observed.homeRetained,
        `server park must stay nonterminal and preserve clone + HOME: ${JSON.stringify(observed)}`);
    });
  }
  it("stale canonical storage ACK ends quietly", async () => {
    const c = claim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body) });
    api.failStateWhen(c.run_id, (b) => b.status === "running" && b.candidate_digest === digest,
      { httpStatus: 409, runStatus: "running", disposition: "stale_claim" });
    await start(checkedExec().exec, c);
    assert.ok(!statuses(c.run_id).includes("failed"), `superseded owner must not terminal-report: ${JSON.stringify(statuses(c.run_id))}`);
  });


  it("stores the server canonical bundle, including explicit empty arrays, before returning cross_check approval", async () => {
    const c = claim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      candidate: { ...body, plan_md: NORMALIZED, milestones: [], required_tools: [], required_capabilities: [], size_class: "l" },
    }) });
    const { exec, verdicts } = checkedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 1, JSON.stringify(api.states));
    const submitted = api.crossCheckRequests[0]!.body;
    assert.deepEqual(submitted.milestones, []);
    assert.deepEqual(submitted.required_capabilities, []);
    assert.deepEqual(submitted.required_tools, []);
    assert.match(String(submitted.base_commit), /^[a-f0-9]{40}$/);
    const stored = api.states.find((s) => s.runId === c.run_id && s.body.plan_md === NORMALIZED)!.body;
    assert.deepEqual([stored.milestones, stored.required_tools, stored.required_capabilities], [[], [], []]);
    assert.equal(stored.candidate_digest, digest);
    assert.equal(stored.size_class, "l");
    assert.equal(verdicts[0]!.kind, "approve");
    assert.ok(verdicts[0]?.kind === "approve" && verdicts[0].approval === "cross_check");
    assert.equal(gates(c.run_id).length, 0);
  });

  for (const [verdict, reason] of [["revise", "revise"], ["block", "block"],
    ...["malformed", "model_error", "model_timeout", "checker_unavailable", "confinement_failed",
      "timed_out", "superseded", "interrupted"].map((r) => ["failed", r])]) {
    it(`forces a human gate for ${reason}, with no unchecked implementation`, async () => {
      const c = claim();
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, verdict, reason) });
      api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan", "decline")); });
      const { exec, verdicts } = checkedExec();
      await start(exec, c);
      assert.equal(api.crossCheckRequests.length, 1);
      assert.equal((gates(c.run_id)[0] as PlanCrossCheckStateRequest).plan_cross_check_gate_reason, reason);
      assert.deepEqual(verdicts.map((v) => v.kind), ["reject"]);
      assert.ok(!statuses(c.run_id).includes("recovery_wait"));
    });
  }

  for (const [status, reason, expected, refusal] of [
    [400, "candidate_invalid", "candidate_refused", "candidate_invalid"],
    [400, "candidate_too_large", "candidate_refused", "candidate_too_large"],
    [413, "envelope_too_large", "candidate_refused", "envelope_too_large"],
    [409, "checker_unavailable", "checker_unavailable", undefined],
    [409, "interrupted", "interrupted", undefined],
  ] as const) {
    it(`forces the supported human disposition on submit ${reason}`, async () => {
      const c = claim();
      api.crossCheckHandler = () => ({ status, body: { reason } });
      api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan")); });
      await start(checkedExec().exec, c);
      const gate = gates(c.run_id)[0] as PlanCrossCheckStateRequest;
      assert.equal(gate.plan_cross_check_gate_reason, expected);
      assert.equal(gate.plan_cross_check_refusal, refusal);
      assert.equal(api.crossCheckRequests.length, 1);
    });
  }

  for (const [reason, override, verdict, checkerReason] of [
    ["checker_failed", { candidate_generation: 2 }, "approve", "approve"],
    ["checker_failed", { claim_generation: 2 }, "approve", "approve"],
    ["checker_failed", { candidate_digest: "b".repeat(64), candidate_generation: 3 }, "approve", "approve"],
    // cross_check.go returns the historical candidate generation with current claim proof.
    ["interrupted", { candidate_generation: 1, claim_generation: 2 }, "failed", "interrupted"],
    ["checker_failed", { candidate_digest: "b".repeat(64) }, "pending", ""],
    ["checker_failed", { candidate_digest: "invalid" }, "pending", ""],
    ["checker_failed", { candidate_digest: "invalid" }, "invalid_submit", ""],
    ["checker_failed", { plan_cross_check_settled: false }, "approve", "approve"],
  ] as const) {
    it(`forces human rejection for ${verdict} ${JSON.stringify(override)} with intact preparation`, async () => {
      const c = verdict === "failed" ? freshClaim(2, { auto_approve: true, plan_cross_check_required: true }) : claim();
      let context!: RunContext;
      let submitted!: Record<string, unknown>;
      let pending!: ReturnType<typeof answer>;
      api.crossCheckHandler = ({ runId, body, method }) => {
        if (method === "POST") {
          submitted = body;
          assert.equal(api.usageRequests.length, 1, "usage preparation ACK precedes submit");
          assert.ok(api.messages(runId).some((m) => (m.payload as { text?: string }).text === "prepared feed"),
            "message preparation ACK precedes submit");
          context.emit({ kind: "status", payload: { text: "reserved feed" } });
          pending = answer(runId, body, verdict === "invalid_submit" ? "approve" : verdict,
            verdict === "invalid_submit" ? "approve" : checkerReason, verdict === "pending" ? {} : override);
          return { status: 200, body: pending };
        }
        return { status: 200, body: answer(runId, submitted, "approve", "approve",
          { ...override, deadline_at: pending.deadline_at }) };
      };
      const held = holdGateAck(c.run_id, PLAN);
      let rejectId = 0;
      api.onState(c.run_id, (b) => {
        if (b.status === "awaiting_approval") rejectId = send(c.run_id, row("reject_plan", "decline"))[0]!.id;
      });
      const { exec, verdicts } = checkedExec(async (ctx) => {
        context = ctx;
        ctx.emit({ kind: "status", payload: { text: "prepared feed" } });
        const leg = ctx.usage!.startLeg();
        leg.observeAssistant({ message: { id: "prepared-usage", model: "claude", usage: { input_tokens: 3 } } });
        leg.close();
      });
      let applied = false;
      const report = client.reportPlanCrossCheckGateState.bind(client);
      client.reportPlanCrossCheckGateState = async (runId, fields, signal) => {
        const ack = await report(runId, fields, signal);
        if (fields.status === "awaiting_approval") {
          assert.equal(fields.claim_generation, c.claim_generation);
          assert.ok(ack.applied && !ack.staleClaim);
          assert.equal(ack.reconciliation?.claimGeneration, c.claim_generation);
          assert.equal(ack.reconciliation?.planCrossCheckSettled, true);
          applied = true;
        }
        return ack;
      };
      const done = start(exec, c);
      try {
        assert.ok(await until(() => rejectId > 0));
        await routed(c.run_id, rejectId);
        assert.equal(applied, false, "held forced gate ACK cannot release transport");
        assert.equal(verdicts.length, 0, "human verdict waits for applied ACK");
        assert.ok(!api.messages(c.run_id).some((m) => (m.payload as { text?: string }).text === "reserved feed"));
        held.release();
        await done;
        assert.equal(applied, true);
        const gate = gates(c.run_id)[0] as PlanCrossCheckStateRequest;
        assert.equal(gate.plan_cross_check_gate_reason, reason);
        assert.equal(gate.plan_cross_check_refusal, reason === "checker_failed" ? "submit_failed" : undefined);
        assert.deepEqual(verdicts.map((v) => v.kind), ["reject"]);
        assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1);
        assert.ok(api.messages(c.run_id).some((m) => (m.payload as { text?: string }).text === "reserved feed"),
          "applied same-generation forced gate ACK releases normal feed");
        assert.ok(!api.states.some((s) => s.runId === c.run_id && s.body.status === "running" && s.body.plan_md),
          "no canonical unchecked approval");
        assert.ok(!statuses(c.run_id).includes("completed") && !statuses(c.run_id).includes("recovery_wait"));
      } finally {
        held.release();
        await done;
        client.reportPlanCrossCheckGateState = report;
      }
    });
  }

  it("inactive preparation remains terminal before an identity fallback", async () => {
    const c = claim();
    let context!: RunContext;
    api.crossCheckHandler = ({ runId, body }) => {
      assert.ok(context.usage instanceof UsageRecorder);
      context.usage.release();
      return { status: 200, body: answer(runId, body, "approve", "approve", { candidate_generation: 2 }) };
    };
    const { exec, verdicts } = checkedExec(async (ctx) => { context = ctx; });
    await start(exec, c);
    terminal(c.run_id, "preparation receipts irrecoverably lost");
    assert.deepEqual(verdicts, []);
    assert.equal(gates(c.run_id).length, 0);
    assert.equal(api.crossCheckRequests.length, 1);
  });

  it("a real usage 400 irrecoverably loses preparation receipts and terminates before submit", async () => {
    const c = claim();
    api.usageHandler = () => ({ status: 400, body: { error: "usage refused" } });
    api.crossCheckHandler = () => { throw new Error("must never submit"); };
    const { exec, verdicts } = checkedExec(async (ctx) => {
      const leg = ctx.usage!.startLeg();
      leg.observeAssistant({ message: { id: "usage-1", model: "claude", usage: { input_tokens: 3 } } });
      leg.close();
    });
    const condition = "plan cross-check: preparation receipts irrecoverably lost";
    const emit = MessageBatcher.prototype.emit;
    let escalations = 0;
    MessageBatcher.prototype.emit = function (message) {
      if (JSON.stringify(message.payload).includes(condition)) escalations++;
      return emit.call(this, message);
    };
    try { await start(exec, c); } finally { MessageBatcher.prototype.emit = emit; }
    terminal(c.run_id, condition);
    assert.equal(escalations, 1, "one escalation attempt even when the failed reservation rejects it");
    assert.deepEqual(verdicts, [], "no implementation verdict or fabricated settlement");
    assert.equal(api.usageRequests.length, 1);
    assert.equal(api.crossCheckRequests.length, 0);
    assert.equal(gates(c.run_id).length, 0);
  });

  for (const revised of [false, true]) {
    it(`unrecoverable ${revised ? "revised" : "initial"} human ACK uses named terminal failure`, async () => {
      const c = claim();
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
      const revisedPlan = "# PLAN revised";
      api.failStateWhen(c.run_id, (b) => b.status === "awaiting_approval" && (!revised || b.plan_md === revisedPlan), { httpStatus: 400 });
      api.onState(c.run_id, (b) => { if (revised && b.status === "awaiting_approval") send(c.run_id, row("revise_plan", "revise")); });
      const exec: Executor = { run: async (ctx) => {
        const v = await ctx.gatePlan!(PLAN);
        if (v.kind === "revise") await ctx.gatePlan!(revisedPlan, undefined, undefined, v.inputId);
        throw new Error("must not implement");
      } };
      await start(exec, c);
      terminal(c.run_id, "human-presentation ACK unrecoverable");
      assert.equal(api.crossCheckRequests.length, 1);
      assert.equal(gates(c.run_id).length, revised ? 1 : 0);
    });
  }

  for (const final of ["approve_plan", "reject_plan"] as const) {
    it(`dropped initial ACK + queued revise before identical retry ACK resolves revised ${final}`, async () => {
      const c = claim();
      const revisedPlan = "# PLAN human revision";
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
      let release!: () => void;
      const retryAck = new Promise<void>((resolve) => { release = resolve; });
      const initial = (b: { status: string; plan_md?: string }) => b.status === "awaiting_approval" && b.plan_md === PLAN;
      api.afterPersistState(c.run_id, initial, "drop");
      let attempts = 0;
      let reviseId = 0;
      let finalId = 0;
      let context!: RunContext;
      const seen: PlanVerdict[] = [];
      api.onState(c.run_id, (b) => {
        if (initial(b)) {
          attempts++;
          if (attempts === 1) context.emit({ kind: "status", payload: { text: "initial ACK reserved feed" } });
          if (attempts === 2) {
            api.afterPersistState(c.run_id, initial, retryAck);
            reviseId = send(c.run_id, row("revise_plan", "human changes"))[0]!.id;
          }
        }
        if (b.status === "awaiting_approval" && b.plan_md === revisedPlan)
          finalId = send(c.run_id, row(final, "final"))[0]!.id;
      });
      const exec: Executor = { run: async (ctx) => {
        context = ctx;
        const first = await ctx.gatePlan!(PLAN);
        seen.push(first);
        assert.equal(first.kind, "revise");
        if (first.kind !== "revise") throw new Error("expected revise");
        const last = await ctx.gatePlan!(revisedPlan, [], undefined, first.inputId);
        seen.push(last);
        if (last.kind === "reject") throw new PlanRejectedError(last.reason);
        return { branch: ctx.branch };
      } };
      const done = start(exec, c);
      try {
        assert.ok(await until(() => reviseId > 0));
        await routed(c.run_id, reviseId);
        assert.equal(attempts, 2, "persisted first ACK was dropped and retried");
        assert.equal(seen.length, 0, "unconfirmed presentation cannot consume queued revise");
        assert.ok(!api.messages(c.run_id).some((m) => (m.payload as { text?: string }).text === "initial ACK reserved feed"));
        const presentations = gates(c.run_id);
        assert.deepEqual(presentations[0], presentations[1], "retry preserves complete body and presentation id");
        release();
        await done;
        assert.equal(api.crossCheckRequests.length, 1, "no second check or candidate overwrite");
        assert.deepEqual(gates(c.run_id).map((g) => g.plan_md), [PLAN, PLAN, revisedPlan]);
        assert.notEqual(gates(c.run_id)[2]!.presentation_id, presentations[0]!.presentation_id);
        assert.deepEqual(seen.map((v) => v.kind), ["revise", final === "approve_plan" ? "approve" : "reject"]);
        assert.ok(api.isApplied(c.run_id, reviseId), "revise input settled on revised applied ACK");
        assert.ok(api.isApplied(c.run_id, finalId), "current final verdict receipt settled");
        assert.equal(api.messages(c.run_id).filter((m) => (m.payload as { text?: string }).text === "initial ACK reserved feed").length, 1,
          "delivery restored exactly once after confirmed retry");
        assert.ok(!statuses(c.run_id).includes("recovery_wait"));
        assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate).length, 1);
      } finally {
        release();
        await done;
      }
    });
  }

  it("late round-one POSTs are refused after no_row fallback commit while its ACK is held", async () => {
    const c = claim();
    let context!: RunContext;
    let releasePosts!: () => void;
    const posts = new Promise<void>((resolve) => { releasePosts = resolve; });
    api.crossCheckHandler = async ({ runId, method, body }) => {
      if (method === "GET") return { status: 200, body: { result: "no_row", reason_class: "no_candidate", ...proof(runId) } };
      context.emit({ kind: "status", payload: { text: "held candidate feed" } });
      await posts;
      return { status: 200, body: answer(runId, body) };
    };
    const held = holdGateAck(c.run_id, PLAN);
    let rejectId = 0;
    api.onState(c.run_id, (b) => {
      if (b.status === "awaiting_approval") rejectId = send(c.run_id, row("reject_plan", "late decline"))[0]!.id;
    });
    const { exec, verdicts } = checkedExec(async (ctx) => { context = ctx; });
    const done = start(exec, c);
    try {
      assert.ok(await until(() => rejectId > 0));
      await routed(c.run_id, rejectId);
      const submitted = api.crossCheckRequests.filter((r) => r.method === "POST");
      assert.equal(submitted.length, 3, "bounded identical candidate attempts");
      for (const attempt of submitted) assert.deepEqual(attempt.body, submitted[0]!.body);
      assert.equal(api.crossCheckRequests.filter((r) => r.method === "GET").length, 3);
      assert.equal((gates(c.run_id)[0] as PlanCrossCheckStateRequest).plan_cross_check_gate_reason, "checker_failed");
      assert.equal((gates(c.run_id)[0] as PlanCrossCheckStateRequest).plan_cross_check_refusal, "submit_failed");
      assert.equal(verdicts.length, 0);
      assert.ok(!api.messages(c.run_id).some((m) => (m.payload as { text?: string }).text === "held candidate feed"),
        "settled no_row proofs and persisted-but-unconfirmed gate cannot release feed");
      releasePosts();
      assert.ok(await until(() => api.crossCheckReplies.filter((r) => r.method === "POST").length === 3));
      assert.deepEqual(api.crossCheckReplies.filter((r) => r.method === "POST").map((r) => r.status), [409, 409, 409]);
      assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate).length, 0);
      assert.equal(verdicts.length, 0, "late replies grant no authority");
      held.release();
      await done;
      assert.deepEqual(verdicts.map((v) => v.kind), ["reject"]);
      assert.equal(api.messages(c.run_id).filter((m) => (m.payload as { text?: string }).text === "held candidate feed").length, 3);
      const seqs = api.messages(c.run_id).map((m) => m.seq);
      assert.deepEqual(seqs, Array.from({ length: seqs.length }, (_, i) => i + 1));
    } finally {
      releasePosts();
      held.release();
      await done;
      assert.ok(await until(() => api.crossCheckReplies.filter((r) => r.method === "POST").length ===
        api.crossCheckRequests.filter((r) => r.method === "POST").length), "all started handlers settled");
    }
  });

  it("dropped submit ACK resolves the current candidate by GET and stores one canonical bundle", async () => {
    const c = claim();
    let stored!: ReturnType<typeof answer>;
    api.crossCheckHandler = ({ runId, method, body }) => {
      if (method === "GET") return { status: 200, body: stored };
      stored = answer(runId, body, "approve", "approve", {
        candidate: { ...body, plan_md: NORMALIZED, milestones: [], required_tools: [], required_capabilities: [] },
      });
      return { status: 200, body: stored, drop: true };
    };
    const { exec, verdicts } = checkedExec();
    await start(exec, c);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.method), ["POST", "GET"]);
    assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate && r.dropped).length, 1);
    assert.equal(api.states.filter((s) => s.runId === c.run_id && s.body.plan_md === NORMALIZED).length, 1);
    const canonical = api.states.find((s) => s.runId === c.run_id && s.body.plan_md === NORMALIZED)!.body;
    assert.deepEqual([canonical.milestones, canonical.required_tools, canonical.required_capabilities], [[], [], []]);
    assert.equal(canonical.candidate_digest, digest);
    assert.ok(verdicts[0]?.kind === "approve" && verdicts[0].approval === "cross_check");
    assert.equal(gates(c.run_id).length, 0);
  });

  for (const decision of ["approve", "revise"] as const) {
    it(`concurrent feed producers and session sender preserve sequences through ${decision} proof`, async () => {
      const c = claim();
      let context!: RunContext;
      let releasePost!: () => void;
      const post = new Promise<void>((resolve) => { releasePost = resolve; });
      let entered = false;
      api.crossCheckHandler = async ({ runId, body }) => {
        assert.equal(api.usageRequests.length, 1, "usage ACK before candidate POST");
        assert.ok(api.messages(runId).some((m) => (m.payload as { text?: string }).text === "before reservation"),
          "message ACK before candidate POST");
        entered = true;
        await post;
        return { status: 200, body: answer(runId, body, decision, decision) };
      };
      const held = decision === "revise" ? holdGateAck(c.run_id, PLAN) : undefined;
      let rejectId = 0;
      api.onState(c.run_id, (b) => {
        if (b.status === "awaiting_approval") rejectId = send(c.run_id, row("reject_plan", "decline"))[0]!.id;
      });
      const { exec, verdicts } = checkedExec(async (ctx) => {
        context = ctx;
        ctx.emit({ kind: "status", payload: { text: "before reservation" } });
        const leg = ctx.usage!.startLeg();
        leg.observeAssistant({ message: { id: "concurrent-usage", model: "claude", usage: { input_tokens: 3 } } });
        leg.close();
      });
      // This race intentionally keeps the POST within its owned request deadline.
      const forge = fakeGitlab();
      const done = runner(exec, forge.gitlab, undefined, {
        planCrossCheckTiming: { ...timing, requestMs: 1000 }, planApprovalTimeoutMs: 4000,
      }).execute(c);
      try {
        assert.ok(await until(() => entered));
        await Promise.all(Array.from({ length: 4 }, async (_, i) => {
          context.emit({ kind: "status", payload: { text: `producer ${i}` } });
        }));
        context.onSessionId!("concurrent-session");
        const feed = api.messages(c.run_id);
        feed.push({ seq: feed.at(-1)!.seq + 1, kind: "status", payload: { text: "server checker event" } });
        assert.ok(!feed.some((m) => String((m.payload as { text?: string }).text).startsWith("producer ")));
        releasePost();
        if (held) {
          assert.ok(await until(() => rejectId > 0));
          await routed(c.run_id, rejectId);
          assert.equal(verdicts.length, 0);
          assert.ok(!api.messages(c.run_id).some((m) => String((m.payload as { text?: string }).text).startsWith("producer ")));
          held.release();
        }
        await done;
        assert.deepEqual(verdicts.map((v) => v.kind), [decision === "approve" ? "approve" : "reject"]);
        assert.ok(api.states.some((s) => s.runId === c.run_id && s.body.session_id === "concurrent-session"),
          "independent state sender settles after the transport reservation releases");
        const messages = api.messages(c.run_id);
        for (let i = 0; i < 4; i++)
          assert.equal(messages.filter((m) => (m.payload as { text?: string }).text === `producer ${i}`).length, 1);
        assert.equal(messages.filter((m) => (m.payload as { text?: string }).text === "server checker event").length, 1);
        assert.deepEqual(messages.map((m) => m.seq), Array.from({ length: messages.length }, (_, i) => i + 1));
        assert.equal(api.crossCheckRequests.length, 1);
      } finally {
        releasePost();
        held?.release();
        await done;
      }
    });
  }

  it("dropped revised ACK preserves revision identity after the previous revision was confirmed", async () => {
    const c = claim();
    const plans = [PLAN, "# PLAN revision one", "# PLAN revision two"];
    let release!: () => void;
    const retry = new Promise<void>((resolve) => { release = resolve; });
    const matches = (b: { status: string; plan_md?: string }) => b.status === "awaiting_approval" && b.plan_md === plans[1];
    api.afterPersistState(c.run_id, matches, "drop");
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
    let attempts = 0;
    const reviseIds: number[] = [];
    let approvalId = 0;
    api.onState(c.run_id, (b) => {
      if (b.status !== "awaiting_approval") return;
      if (b.plan_md === PLAN) reviseIds.push(send(c.run_id, row("revise_plan", "first change"))[0]!.id);
      if (matches(b) && ++attempts === 2) {
        api.afterPersistState(c.run_id, matches, retry);
        reviseIds.push(send(c.run_id, row("revise_plan", "second change"))[0]!.id);
      }
      if (b.plan_md === plans[2]) approvalId = send(c.run_id, row("approve_plan"))[0]!.id;
    });
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      let settles: number | undefined;
      for (const plan of plans) {
        const v = await ctx.gatePlan!(plan, [], undefined, settles);
        seen.push(v);
        if (v.kind === "approve") return { branch: ctx.branch };
        assert.equal(v.kind, "revise");
        if (v.kind !== "revise") throw new Error("expected revise");
        settles = v.inputId;
      }
      throw new Error("missing approval");
    } };
    const done = start(exec, c);
    try {
      assert.ok(await until(() => reviseIds.length === 2));
      await routed(c.run_id, reviseIds[1]!);
      assert.deepEqual(seen.map((v) => v.kind), ["revise"], "previous presentation confirmed; retry still unconfirmed");
      assert.deepEqual(gates(c.run_id)[1], gates(c.run_id)[2], "same revision body/id across drop");
      release();
      await done;
      assert.deepEqual(seen.map((v) => v.kind), ["revise", "revise", "approve"]);
      assert.deepEqual(gates(c.run_id).map((g) => g.plan_md), [plans[0], plans[1], plans[1], plans[2]]);
      assert.equal(new Set(gates(c.run_id).map((g) => g.presentation_id)).size, 3);
      for (const id of [...reviseIds, approvalId]) assert.ok(api.isApplied(c.run_id, id), `receipt ${id} settled`);
      assert.equal(api.crossCheckRequests.length, 1);
      assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate).length, 1);
      assert.ok(!statuses(c.run_id).includes("recovery_wait"));
    } finally {
      release();
      await done;
    }
  });

  it("scan refusal precedes any candidate upload", async () => {
    const c = claim();
    const original = git.scanPatchForSecrets.bind(git);
    git.scanPatchForSecrets = async () => ({ trusted: false, findings: [] });
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan")); });
    try { await start(checkedExec().exec, c); } finally { git.scanPatchForSecrets = original; }
    assert.equal(api.crossCheckRequests.length, 0);
    const gate = gates(c.run_id)[0] as PlanCrossCheckStateRequest;
    assert.equal(gate.plan_cross_check_gate_reason, "planning_diff_refused");
    assert.equal(gate.plan_cross_check_diff_refusal, "scan_failed");
  });

  it("lossy UTF-8 capture is refused before scan or upload", async () => {
    const c = claim();
    let scanned = false;
    git.capturePlanningDiff = async () => Buffer.from([0xff]);
    git.scanPatchForSecrets = async () => { scanned = true; return { trusted: true, findings: [] }; };
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan")); });
    await start(checkedExec().exec, c);
    assert.equal(scanned, false);
    assert.equal(api.crossCheckRequests.length, 0);
    assert.equal((gates(c.run_id)[0] as PlanCrossCheckStateRequest).plan_cross_check_diff_refusal, "diff_failed");
  });

  it("captures and scans identical planning bytes against the immutable clone base", async () => {
    const c = claim();
    let capturedBase = "";
    let scanned = "";
    const capture = git.capturePlanningDiff.bind(git);
    const scan = git.scanPatchForSecrets.bind(git);
    git.capturePlanningDiff = async (clone, base) => { capturedBase = base; return capture(clone, base); };
    git.scanPatchForSecrets = async (diff) => { scanned = diff; return scan(diff); };
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body) });
    await start(checkedExec(async (ctx) => {
      fs.writeFileSync(path.join(ctx.worktreePath, "planning.txt"), "planning bytes\n");
    }).exec, c);
    const submitted = api.crossCheckRequests[0]!.body as unknown as PlanCrossCheckCandidate;
    assert.equal(submitted.base_commit, capturedBase);
    assert.equal(submitted.planning_diff, scanned);
    assert.ok(scanned.includes("planning bytes"));
  });

  it("real runner APPROVE reaches SDK implementation with server normalization and []", async () => {
    const c = claim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      candidate: { ...body, plan_md: NORMALIZED, milestones: [], required_capabilities: [], required_tools: [], size_class: "s" },
    }) });
    const base = planWithMilestonesThenDoneQuery([{ id: "local", title: "local milestone" }]);
    const prompts: string[] = [];
    const query: SdkQueryFn = (params) => {
      const delegated = base(params);
      return (async function* () {
        // SdkQueryFn prompt is an async iterable; wrap rather than consuming twice.
        yield* delegated;
      })();
    };
    const wrapped: SdkQueryFn = (params) => {
      const original = params.prompt;
      params.prompt = (async function* () {
        for await (const frame of original) {
          prompts.push(JSON.stringify(frame));
          yield frame;
        }
      })();
      return query(params);
    };
    await start(new SdkExecutor(nullLogger(), homeDir, { queryFn: wrapped }), c);
    assert.equal(api.crossCheckRequests.length, 1);
    assert.ok(prompts.slice(1).some((p) => p.includes("server normalized proposal")), "SDK adopts server prose");
    assert.ok(!prompts.slice(1).some((p) => p.includes("local milestone")), "SDK clears local milestone list");
    assert.ok(statuses(c.run_id).includes("completed"));
  });
});

for (const committed of [false, true])
it(`checked canonical storage resumes after real switch give-up with ${committed ? "committed held ACK" : "write not committed"}`, { timeout: 10_000 }, async () => {
  const c = claim();
  api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
    candidate: { ...body, plan_md: NORMALIZED, milestones: [], required_capabilities: [], required_tools: [], size_class: "s" },
  }) });
  let requested = false;
  let gaveUp = false;
  let attempts = 0;
  let applied = false;
  let retried = false;
  let releaseAck!: () => void;
  const heldAck = new Promise<void>((resolve) => { releaseAck = resolve; });
  let releaseInitialAck!: () => void;
  const initialAck = new Promise<void>((resolve) => { releaseInitialAck = resolve; });
  const canonical = (b: PlanCrossCheckStateRequest) => b.status === "running" && b.candidate_digest === digest;
  const requests: PlanCrossCheckStateRequest[] = [];
  const snapshots: string[] = [];
  const report = client.reportPlanCrossCheckGateState.bind(client);
  client.reportPlanCrossCheckGateState = async (runId, body, signal) => {
    if (canonical(body)) { requests.push(body); snapshots.push(JSON.stringify(body)); }
    const ack = await report(runId, body, signal);
    if (canonical(body)) {
      assert.ok(ack.applied && ack.status === "running" && !ack.staleClaim);
      applied = true;
    }
    return ack;
  };
  const restore = api.dropStatesWhen(c.run_id, (b) => {
    if (!canonical(b)) return false;
    if (!committed && !requested) {
      requested = true;
      api.requestCredentialSwitch(c.run_id, 1);
    }
    return !committed || requested;
  });
  if (committed) api.afterPersistState(c.run_id, (b) => {
    if (!canonical(b)) return false;
    requested = true;
    api.requestCredentialSwitch(c.run_id, 1);
    return true;
  }, initialAck);
  api.onState(c.run_id, (b) => {
    if (b.status === "credential_switch_failed") {
      gaveUp = true;
      releaseInitialAck();
      assert.equal(applied, false);
      assert.equal(api.states.filter((s) => s.runId === c.run_id && canonical(s.body)).length, committed ? 1 : 0);
      restore();
      api.afterPersistState(c.run_id, (next) => {
        if (!canonical(next)) return false;
        retried = true;
        return true;
      }, heldAck);
    }
  });
  const prompts: string[] = [];
  const base = planWithMilestonesThenDoneQuery([{ id: "local", title: "local milestone" }]);
  let turns = 0;
  const queryFn: SdkQueryFn = (params) => {
    if (turns++ > 0) assert.ok(applied, "implementation requires the applied canonical ACK");
    const original = params.prompt;
    params.prompt = (async function* () {
      for await (const frame of original) { prompts.push(JSON.stringify(frame)); yield frame; }
    })();
    return base(params);
  };
  const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn });
  const exec: Executor = { run: async (ctx) => {
    git.worktreeStatus = async () => ["M src/impl.ts"];
    git.commitWipMarker = async () => false;
    const switchAttempt = ctx.attemptCredentialSwitch!;
    ctx.attemptCredentialSwitch = async () => {
      if (++attempts > 1) throw new Error("repeated switch without a new request");
      const outcome = await switchAttempt();
      assert.equal(outcome, "gave_up");
      return outcome;
    };
    return sdk.run(ctx);
  } };
  const forge = fakeGitlab();
  const run = runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
    recoveryRetryMs: 1, planApprovalTimeoutMs: 2000 });
  const done = run.execute(c);
  try {
    assert.ok(await until(() => retried, 3000), `canonical operation must retry after give-up: ${JSON.stringify(api.states)}`);
    assert.ok(gaveUp);
    assert.equal(applied, false, "held retry ACK cannot approve");
    assert.equal(turns, 1, "no implementation before ACK");
    assert.equal(requests.length, 2, "resume the original operation once");
    assert.equal(requests[1], requests[0], "same immutable report object");
    assert.equal(snapshots[1], snapshots[0], "same complete request, generation and session");
    assert.equal(requests[1]!.plan_md, NORMALIZED);
    assert.equal(requests[1]!.claim_generation, 1);
    assert.deepEqual([requests[1]!.milestones, requests[1]!.required_tools, requests[1]!.required_capabilities], [[], [], []]);
    releaseAck();
    await done;
    assert.equal(attempts, 1);
    assert.equal(api.crossCheckRequests.length, 1);
    assert.equal(gates(c.run_id).length, 0, "no human publication");
    assert.ok(!statuses(c.run_id).includes("recovery_wait"));
    assert.ok(statuses(c.run_id).includes("completed"));
    assert.ok(prompts.slice(1).some((p) => p.includes("server normalized proposal")));
    assert.ok(!prompts.slice(1).some((p) => p.includes("local milestone")));
    assert.equal(api.messages(c.run_id).filter((m) => m.kind === "plan").length, 1, "no second initial plan emission");
  } finally {
    releaseInitialAck();
    releaseAck();
    run.shutdown();
    await done;
    client.reportPlanCrossCheckGateState = report;
  }
});

for (const [checked, revised] of [[true, false], [false, false], [true, true]] as const)
it(`${checked ? "checked" : "ordinary"} confirmed human wait survives one real switch give-up${revised ? " then human revision" : ""}`, async () => {
  const c = claim();
  if (!checked) { c.auto_approve = false; c.plan_cross_check_required = false; }
  api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
  const queryFn: SdkQueryFn = revised
    ? (params) => planWithMilestonesThenDoneQuery([])(params)
    : planWithMilestonesThenDoneQuery([]);
  const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn });
  let attempts = 0;
  let published = false;
  let gaveUp = false;
  api.onState(c.run_id, (b) => {
    if (b.status === "awaiting_approval" && !published) {
      published = true;
      api.requestCredentialSwitch(c.run_id, 1);
    }
    if (b.status === "credential_switch_failed") {
      gaveUp = true;
      send(c.run_id, row(revised ? "revise_plan" : "reject_plan", revised ? "revise after give-up" : "reject after give-up"));
    }
    // Ordinary gates re-present under their legacy revision semantics; reject that
    // current presentation too if the first rejection was bound to the prior one.
    if ((!checked || revised) && gaveUp && b.status === "awaiting_approval")
      send(c.run_id, row("reject_plan", "reject after give-up"));
  });
  const exec: Executor = { run: async (ctx) => {
    git.worktreeStatus = async () => ["M src/impl.ts"];
    git.commitWipMarker = async () => false;
    const switchAttempt = ctx.attemptCredentialSwitch!;
    ctx.attemptCredentialSwitch = async () => {
      attempts++;
      // Bound the regression's failure path rather than looping on the same old abort.
      if (attempts > 1) throw new Error("repeated switch without a new request");
      const outcome = await switchAttempt();
      assert.equal(outcome, "gave_up");
      return outcome;
    };
    return sdk.run(ctx);
  } };
  const forge = fakeGitlab();
  await runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
    recoveryRetryMs: 1, planApprovalTimeoutMs: 2000 }).execute(c);
  assert.equal(attempts, 1, "the old aborted owner signal must not trigger a second switch");
  assert.ok(api.states.some((s) => s.runId === c.run_id && s.body.status === "failed" &&
    s.body.failure_reason?.includes("reject after give-up")), "human rejection ends the run");
  if (checked) {
    assert.equal(gates(c.run_id).length, revised ? 2 : 1, "give-up retains the gate; only human revision publishes another");
    assert.equal(api.crossCheckRequests.length, 1, "no second candidate after give-up");
  }
});
