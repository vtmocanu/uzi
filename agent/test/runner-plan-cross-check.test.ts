import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { MessageBatcher } from "../src/batcher.js";
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
    planApprovalTimeoutMs: 2000 }).execute(c);
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

  for (const [condition, override] of [
    ["invalid or stale candidate identity", { candidate_generation: 2 }],
    ["terminal settlement receipts unavailable", { claim_generation: 2 }],
    ["invalid or stale candidate identity", { candidate_digest: "b".repeat(64), candidate_generation: 3 }],
  ]) {
    it(`rejects ${JSON.stringify(override)} before approval`, async () => {
      const c = claim();
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", override) });
      await start(checkedExec().exec, c);
      terminal(c.run_id, condition as string);
      assert.equal(gates(c.run_id).length, 0);
    });
  }

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
    it(`lost initial ACK + queued revise retains presentation and resolves revised ${final}`, async () => {
      const c = claim();
      const revisedPlan = "# PLAN human revision";
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
      const held = holdGateAck(c.run_id, PLAN);
      let reviseId = 0;
      const seen: PlanVerdict[] = [];
      api.onState(c.run_id, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === PLAN && !reviseId)
          reviseId = send(c.run_id, row("revise_plan", "human changes"))[0]!.id;
        if (b.status === "awaiting_approval" && b.plan_md === revisedPlan) send(c.run_id, row(final, "final"));
      });
      const exec: Executor = { run: async (ctx) => {
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
      assert.ok(await until(() => reviseId > 0));
      await routed(c.run_id, reviseId);
      assert.equal(seen.length, 0, "unconfirmed presentation cannot consume queued revise");
      held.release();
      await done;
      assert.equal(api.crossCheckRequests.length, 1, "no second check");
      assert.deepEqual(gates(c.run_id).map((g) => g.plan_md), [PLAN, revisedPlan]);
      assert.deepEqual(seen.map((v) => v.kind), ["revise", final === "approve_plan" ? "approve" : "reject"]);
      assert.ok(api.isApplied(c.run_id, reviseId), "revise input settled on revised applied ACK");
      assert.ok(!statuses(c.run_id).includes("recovery_wait"));
    });
  }

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
