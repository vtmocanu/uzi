import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { Outbox } from "../src/outbox.js";
import { Worker } from "../src/worker.js";
import type { Logger } from "../src/log.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { createHash } from "node:crypto";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { MessageBatcher } from "../src/batcher.js";
import { UsageRecorder } from "../src/usage-recorder.js";
import type { PlanCrossCheckCandidate, PlanCrossCheckStateRequest } from "../src/client.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, homeDir, installHarness, runner, runnerWith, fakeGitlab,
  assistant, resultOk, planWithMilestonesThenDoneQuery } from "./runner-harness.js";
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
function diagnosticLogger() {
  const warnings: { message: string; fields?: Record<string, unknown> }[] = [];
  const log: Logger = { ...nullLogger(),
    warn: (message, fields) => { warnings.push({ message, fields }); },
    child: () => log,
  };
  return { log, warnings };
}
const timing = { preparationMs: 100, backoffMs: [1, 2] as const, requestMs: 100, pollMs: 5 };

function claim(generation = 1) { return freshClaim(generation, { kind: "issue", auto_approve: true, plan_cross_check_required: true }); }
function proof(runId: string, generation = 1) {
  return { lead_last_seq: Math.max(0, ...api.messages(runId).map((m) => m.seq)),
    claim_generation: generation, plan_cross_check_settled: true, gate_revision: 0,
    current_plan_sha256: createHash("sha256").update("").digest("hex") };
}
function answer(runId: string, body: Record<string, unknown>, verdict = "approve", reason = verdict, overrides = {}) {
  const { stage: _stage, claim_generation: _generation, round = 1, ...candidate } = body;
  return { result: "candidate", round, checker_run_id: child, candidate_digest: digest,
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
function start(exec: Executor, c = claim()) {
  const forge = fakeGitlab();
  const run = runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
    planApprovalTimeoutMs: 4000 });
  return Object.assign(run.execute(c).then(() => forge), { shutdown: () => run.shutdown() });
}
function terminal(runId: string, condition: string) {
  const states = api.states.filter((s) => s.runId === runId).map((s) => s.body);
  assert.ok(states.some((s) => s.status === "failed" && s.failure_reason?.includes(condition)),
    `named terminal failure: ${condition}; got ${JSON.stringify(states)}`);
  assert.ok(!states.some((s) => s.status === "running" && s.plan_md), "no unchecked approval");
  assert.ok(!states.some((s) => s.status === "completed" || s.status === "recovery_wait"), "no completion/recovery");
}

// Runner paths that reach native planning capture walk Linux /proc; Linux CI runs them.
const LINUX_CAPTURE = process.platform !== "linux" && "Linux /proc capture required";
describe("U2 real runner checked gate", () => {
  it("two automatic revisions select explicit rounds and store only round-three canonical approval", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const seen: PlanVerdict[] = [];
    const events: string[] = [];
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    const capture = git.capturePlanningDiff.bind(git);
    let captures = 0;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      const reservation = reserve.call(this);
      return { ...reservation,
        release: (response) => {
          const released = reservation.release(response);
          assert.equal(released, true);
          events.push(`release:${response.result === "candidate" ? response.round : 0}`);
          return released;
        },
        releaseAppliedGate: () => { throw new Error("automatic path must never release a human gate"); },
      };
    };
    git.capturePlanningDiff = async (...args) => {
      captures++;
      await capture(...args);
      return Buffer.from(`diff candidate ${captures}`);
    };
    api.crossCheckHandler = ({ runId, body }) => {
      const round = Number(body.round);
      assert.equal(gates(runId).length, 0);
      assert.equal(api.states.filter((s) => s.runId === runId && s.body.plan_md).length, 0);
      return { status: 200, body: answer(runId, body, round < 3 ? "revise" : "approve", round < 3 ? "revise" : "approve", {
        automatic_revision_limit: 2, automatic_rounds_enabled: true,
        findings: round < 3 ? { summary: `fix round ${round}`, items: [] } : null,
        candidate: { plan_md: NORMALIZED, milestones: [{ title: "canonical", done: false }],
          required_capabilities: [], required_tools: [], size_class: "s",
          base_commit: body.base_commit, planning_diff: body.planning_diff },
      }) };
    };
    const exec: Executor = { run: async (ctx) => {
      // Three bounded candidates; any wrong verdict fails immediately.
      for (let round = 1; round <= 3; round++) {
        const verdict = await ctx.gatePlan!(`${PLAN} ${round}`);
        events.push(`return:${round}`);
        seen.push(verdict);
        assert.equal(gates(c.run_id).length, 0);
        if (round < 3) {
          assert.deepEqual(verdict, { kind: "revise", automatic: true, round,
            feedback: `fix round ${round}`, items: [] });
          assert.ok(!("inputId" in verdict));
          assert.equal(ctx.cancelRequested!(), false);
        } else {
          assert.equal(verdict.kind, "approve");
          assert.ok(verdict.kind === "approve" && verdict.approval === "cross_check");
          assert.equal(verdict.canonical.plan, NORMALIZED);
          assert.deepEqual(verdict.canonical.milestones, [{ title: "canonical", done: false }]);
        }
      }
      return { branch: ctx.branch };
    } };
    try {
      await start(exec, c);
      assert.equal(captures, 3);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2, 3]);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.body.planning_diff),
        ["diff candidate 1", "diff candidate 2", "diff candidate 3"]);
      assert.equal(api.crossCheckLatestRequests.length, 0);
      assert.deepEqual(events, ["release:1", "return:1", "release:2", "return:2", "release:3", "return:3"]);
      const stored = api.states.filter((s) => s.runId === c.run_id && s.body.plan_md);
      assert.equal(stored.length, 1);
      assert.equal(stored[0]!.body.plan_md, NORMALIZED);
      assert.equal(stored[0]!.body.candidate_digest, digest);
      assert.deepEqual(stored[0]!.body.milestones, [{ title: "canonical", done: false }]);
      assert.deepEqual(seen.map((v) => v.kind), ["revise", "revise", "approve"]);
    } finally {
      MessageBatcher.prototype.reserveCandidateTransport = reserve;
      git.capturePlanningDiff = capture;
    }
  });

  for (const failure of ["release", "cancel"] as const) {
    it(`automatic ${failure} failure never returns a revision turn`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim();
      const reserve = MessageBatcher.prototype.reserveCandidateTransport;
      let context: RunContext;
      let releases = 0;
      MessageBatcher.prototype.reserveCandidateTransport = function () {
        const reservation = reserve.call(this);
        return { ...reservation, release: (response) => {
          releases++;
          if (failure === "release") return false;
          const released = reservation.release(response);
          send(c.run_id, row("cancel"));
          return released;
        } };
      };
      api.crossCheckHandler = async ({ runId, body }) => {
        if (failure === "cancel") {
          send(c.run_id, row("cancel"));
          assert.ok(await until(() => context.cancelRequested!()));
        }
        return { status: 200, body: answer(runId, body, "revise", "revise", {
          automatic_revision_limit: 2, automatic_rounds_enabled: true,
        }) };
      };
      const { exec, verdicts } = checkedExec(async (ctx) => { context = ctx; });
      try {
        await start(exec, c);
        assert.equal(verdicts.length, 0);
        assert.equal(gates(c.run_id).length, 0);
        if (failure === "release") {
          assert.equal(releases, 1);
          terminal(c.run_id, "revision settlement receipts unavailable");
        }
      } finally { MessageBatcher.prototype.reserveCandidateTransport = reserve; }
    });
  }

  it("shutdown after decided reservation release prevents automatic return at the state barrier", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    let done: ReturnType<typeof start>;
    let released = false;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      const reservation = reserve.call(this);
      return { ...reservation, release: (response) => {
        released = reservation.release(response);
        // Runs after checkPlan's synchronous cancellation check, before the runner
        // resumes from its decision promise and awaits the actual state barrier.
        queueMicrotask(() => done.shutdown());
        return released;
      } };
    };
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise", {
      automatic_revision_limit: 2, automatic_rounds_enabled: true,
    }) });
    const { exec, verdicts } = checkedExec();
    try {
      done = start(exec, c);
      await done;
      assert.equal(released, true, "real decided-proof release completed");
      assert.equal(verdicts.length, 0, "no executor revision after lifecycle cancellation");
      assert.equal(gates(c.run_id).length, 0);
      assert.equal(api.crossCheckRequests.length, 1);
    } finally { MessageBatcher.prototype.reserveCandidateTransport = reserve; }
  });

  it("reclaimed generation discovers eligible round two once and then uses its explicit counter", { skip: LINUX_CAPTURE }, async () => {
    const c = claim(2);
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
      automatic_rounds_enabled: true, next_round: 2, next_round_eligible: true, fallback_reason: "",
    } });
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body,
      body.round === 2 ? "revise" : "approve", body.round === 2 ? "revise" : "approve", {
        ...proof(runId, 2), candidate_generation: 2,
        automatic_revision_limit: 2, automatic_rounds_enabled: true,
      }) });
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      seen.push(await ctx.gatePlan!(PLAN));
      assert.ok(seen[0]!.kind === "revise" && seen[0]!.automatic);
      seen.push(await ctx.gatePlan!(NORMALIZED));
      assert.equal(seen[1]!.kind, "approve");
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [2, 3]);
    assert.deepEqual(api.crossCheckLatestRequests, [{ runId: c.run_id, generation: 2 }]);
    assert.equal(gates(c.run_id).length, 0);
  });

  for (const reason of ["", "approve"] as const) {
    it(`current-generation metadata ${reason || "pending"} retries its exact round and never grants approval`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim(2);
      api.crossCheckLatestHandler = () => ({ status: 200, body: {
        result: "latest", round: 2, candidate_generation: 2, automatic_revision_limit: 2,
        automatic_rounds_enabled: true, next_round: null, next_round_eligible: false, fallback_reason: reason,
      } });
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
        ...proof(runId, 2), candidate_generation: 2, automatic_revision_limit: 2, automatic_rounds_enabled: true,
      }) });
      const { exec, verdicts } = checkedExec();
      await start(exec, c);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [2]);
      assert.equal(verdicts[0]!.kind, "approve");
      assert.equal(gates(c.run_id).length, 0);
    });
  }

  for (const metadata of [
    { candidate_generation: 3 },
    { next_round: 4, next_round_eligible: true },
    { fallback_reason: "unknown" },
  ]) {
    it(`invalid or future metadata is terminal without submit or approval: ${JSON.stringify(metadata)}`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim(2);
      api.crossCheckLatestHandler = () => ({ status: 200, body: {
        result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
        automatic_rounds_enabled: true, next_round: 2, next_round_eligible: true, fallback_reason: "",
        ...metadata,
      } });
      const { exec, verdicts } = checkedExec();
      await start(exec, c);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(verdicts.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      terminal(c.run_id, "plan cross-check:");
    });
  }

  it("automatic revision can fall back to a local human revision without fresh discovery or checks", { skip: LINUX_CAPTURE }, async () => {
    const c = claim(2);
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body,
      body.round === 1 ? "revise" : "block", body.round === 1 ? "revise" : "block", {
        ...proof(runId, 2), candidate_generation: 2, automatic_revision_limit: 2, automatic_rounds_enabled: true,
      }) });
    api.onState(c.run_id, (body) => {
      if (body.status === "awaiting_approval")
        send(c.run_id, row(body.plan_md === "human revision" ? "reject_plan" : "revise_plan", "human feedback"));
    });
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      seen.push(await ctx.gatePlan!(PLAN));
      assert.ok(seen[0]!.kind === "revise" && seen[0]!.automatic);
      const human = await ctx.gatePlan!(NORMALIZED);
      seen.push(human);
      assert.ok(human.kind === "revise" && !human.automatic);
      seen.push(await ctx.gatePlan!("human revision", undefined, undefined, human.inputId));
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.deepEqual(seen.map((v) => v.kind), ["revise", "revise", "reject"]);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2]);
    assert.equal(api.crossCheckLatestRequests.length, 1);
    assert.equal(gates(c.run_id).length, 2);
  });

  for (const reason of ["block", "timed_out", "model_error", "checker_failed", "revisions_exhausted"] as const) {
    it(`reclaimed ${reason} metadata preserves fallback without submitting a child`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim(2);
      api.crossCheckLatestHandler = () => ({ status: 200, body: {
        result: "latest", round: reason === "revisions_exhausted" ? 3 : 1, candidate_generation: 1,
        automatic_revision_limit: 2, automatic_rounds_enabled: true,
        next_round: null, next_round_eligible: false, fallback_reason: reason,
      } });
      api.crossCheckHandler = () => { throw new Error("must never submit"); };
      api.onState(c.run_id, (body) => {
        if (body.status === "awaiting_approval") send(c.run_id, row("reject_plan", "human rejection"));
      });
      const { exec, verdicts } = checkedExec();
      await start(exec, c);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(api.crossCheckLatestRequests.length, 1);
      assert.equal(gates(c.run_id).length, 1);
      assert.equal((gates(c.run_id)[0]! as PlanCrossCheckStateRequest).plan_cross_check_gate_reason, reason);
      assert.equal(verdicts[0]!.kind, "reject");
    });
  }

  for (const action of ["approve_plan", "revise_plan", "reject_plan"] as const) {
    it(`retryable preparation publishes once before replay and restores human ${action}`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim();
      const held = holdGateAck(c.run_id, PLAN);
      const attempts: OutgoingMessage[][] = [];
      const reserve = MessageBatcher.prototype.reserveCandidateTransport;
      MessageBatcher.prototype.reserveCandidateTransport = function () {
        this.emit({ kind: "status", payload: { text: "preparation prefix" } });
        api.failMessagesNext(1);
        return reserve.call(this);
      };
      const post = client.postMessages.bind(client);
      const report = client.reportPlanCrossCheckGateState.bind(client);
      let applied = false;
      let inputId = 0;
      const seen: PlanVerdict[] = [];
      client.postMessages = async (id, msgs, generation, signal) => {
        if (id === c.run_id && msgs.some((m) => (m.payload as { text?: string }).text === "preparation prefix")) {
          attempts.push(structuredClone(msgs));
          if (attempts.length > 1) {
            assert.equal(gates(c.run_id).length, 1);
            assert.equal(applied, true, "retry waits for the applied forced gate ACK");
          }
        }
        return post(id, msgs, generation, signal);
      };
      client.reportPlanCrossCheckGateState = async (id, fields, signal) => {
        const ack = await report(id, fields, signal);
        if (id === c.run_id && fields.status === "awaiting_approval" && fields.plan_md === PLAN) {
          assert.equal(fields.plan_cross_check_gate_reason, "checker_failed");
          assert.equal(fields.plan_cross_check_refusal, "submit_failed");
          applied = ack.applied;
        }
        return ack;
      };
      api.onState(c.run_id, (b) => {
        if (b.status === "awaiting_approval")
          inputId = send(c.run_id, row(b.plan_md === PLAN ? action : "reject_plan", "human choice"))[0]!.id;
      });
      const exec: Executor = { run: async (ctx) => {
        const leg = ctx.usage!.startLeg();
        leg.observeAssistant({ message: { id: "preparation-usage", model: "claude", usage: { input_tokens: 3 } } });
        leg.close();
        const first = await ctx.gatePlan!(PLAN);
        seen.push(first);
        if (first.kind === "revise") seen.push(await ctx.gatePlan!("# PLAN human revised", undefined, undefined, first.inputId));
        const final = seen.at(-1)!;
        if (final.kind === "reject") throw new PlanRejectedError(final.reason);
        return { branch: ctx.branch };
      } };
      const done = start(exec, c);
      try {
        assert.ok(await until(() => inputId > 0));
        await routed(c.run_id, inputId);
        assert.equal(attempts.length, 1, "no retry before applied ACK");
        assert.equal(seen.length, 0, "queued verdict cannot act before receipts and ACK");
        assert.equal(api.crossCheckRequests.length, 0, "failed initial preparation never submits");
        held.release();
        await done;
        assert.equal(attempts.length, 2);
        assert.deepEqual(attempts[1], attempts[0], "exact assigned prefix replay");
        assert.deepEqual(seen.map((v) => v.kind), action === "revise_plan" ? ["revise", "reject"] :
          [action === "approve_plan" ? "approve" : "reject"]);
        assert.ok(api.isApplied(c.run_id, inputId), "human input receipt settles after restoration");
        assert.equal(api.crossCheckRequests.length, 0);
        assert.equal(gates(c.run_id).length, action === "revise_plan" ? 2 : 1);
      } finally {
        held.release();
        done.shutdown();
        try { await done; } finally {
          client.postMessages = post;
          client.reportPlanCrossCheckGateState = report;
          MessageBatcher.prototype.reserveCandidateTransport = reserve;
        }
      }
    });
  }

  it("applied preparation gate resumes after real credential switch give-up without republishing", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    const report = client.reportPlanCrossCheckGateState.bind(client);
    const post = client.postMessages.bind(client);
    const requests: PlanCrossCheckStateRequest[] = [];
    let preparationAttempts = 0;
    let switchAttempts = 0;
    let gaveUp = false;
    let applied = false;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      this.emit({ kind: "status", payload: { text: "switch prefix" } });
      api.failMessagesNext(1);
      return reserve.call(this);
    };
    client.postMessages = async (id, msgs, generation, signal) => {
      if (id === c.run_id && msgs.some((m) => (m.payload as { text?: string }).text === "switch prefix")) {
        preparationAttempts++;
        if (preparationAttempts > 1) assert.ok(applied && gaveUp, "retry retains the applied ACK across give-up");
      }
      return post(id, msgs, generation, signal);
    };
    client.reportPlanCrossCheckGateState = async (id, fields, signal) => {
      const ack = await report(id, fields, signal);
      if (id === c.run_id && fields.status === "awaiting_approval") {
        requests.push(fields);
        applied = ack.applied;
        api.requestCredentialSwitch(c.run_id, 1);
        // Keep the real applied receipt until the real input control interrupts its owner.
        if (!signal?.aborted) await new Promise<void>((resolve) => signal!.addEventListener("abort", () => resolve(), { once: true }));
      }
      return ack;
    };
    api.onState(c.run_id, (b) => {
      if (b.status === "credential_switch_failed") {
        gaveUp = true;
        send(c.run_id, row("reject_plan", "reject restored preparation"));
      }
    });
    const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn: planWithMilestonesThenDoneQuery([]) });
    const exec: Executor = { run: async (ctx) => {
      git.worktreeStatus = async () => ["M src/impl.ts"];
      git.commitWipMarker = async () => false;
      const switchAttempt = ctx.attemptCredentialSwitch!;
      ctx.attemptCredentialSwitch = async () => {
        assert.equal(++switchAttempts, 1, "one real switch attempt");
        const outcome = await switchAttempt();
        assert.equal(outcome, "gave_up");
        return outcome;
      };
      return sdk.run(ctx);
    } };
    const forge = fakeGitlab();
    try {
      await runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
        recoveryRetryMs: 1, planApprovalTimeoutMs: 2000 }).execute(c);
      assert.equal(switchAttempts, 1);
      assert.equal(requests.length, 1, "one frozen strict request and applied ACK");
      assert.equal(gates(c.run_id).length, 1, "no second publication on callback resumption");
      assert.equal(preparationAttempts, 2);
      assert.equal(api.crossCheckRequests.length, 0);
      terminal(c.run_id, "reject restored preparation");
    } finally {
      MessageBatcher.prototype.reserveCandidateTransport = reserve;
      client.reportPlanCrossCheckGateState = report;
      client.postMessages = post;
    }
  });

  it("owner cancellation aborts and settles remaining preparation HTTP before any queued approval", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    const post = client.postMessages.bind(client);
    let attempts = 0;
    let entered = false;
    let settled = false;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      this.emit({ kind: "status", payload: { text: "cancel prefix" } });
      api.failMessagesNext(1);
      return reserve.call(this);
    };
    client.postMessages = async (id, msgs, generation, signal) => {
      if (id === c.run_id && msgs.some((m) => (m.payload as { text?: string }).text === "cancel prefix")) {
        attempts++;
        if (attempts === 2) {
          try { return await post(id, msgs, generation, signal); }
          finally { settled = true; }
        }
      }
      return post(id, msgs, generation, signal);
    };
    api.holdMessagesUntilAbort = (id, body) => {
      if (id !== c.run_id || attempts !== 2 || !(body.messages as OutgoingMessage[]).some(
        (m) => (m.payload as { text?: string }).text === "cancel prefix")) return false;
      entered = true;
      send(c.run_id, row("cancel"));
      return true;
    };
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("approve_plan")); });
    const { exec, verdicts } = checkedExec();
    try {
      await start(exec, c);
      assert.equal(entered, true);
      assert.equal(settled, true, "client HTTP attempt settles before execution exits");
      assert.ok(await until(() => api.abortedHeldMessages === 1), "server observes cancelled response");
      assert.equal(attempts, 2, "no third attempt after cancellation");
      assert.equal(verdicts.length, 0);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 1);
      assert.ok(!api.states.some((s) => s.runId === c.run_id && s.body.failure_reason?.includes("preparation ACKs unrecoverable")));
    } finally {
      api.holdMessagesUntilAbort = undefined;
      MessageBatcher.prototype.reserveCandidateTransport = reserve;
      client.postMessages = post;
    }
  });

  it("pending checker expires at its fixed local deadline despite transient status failures", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    let stored!: ReturnType<typeof answer>;
    api.crossCheckHandler = ({ runId, method, body }) => {
      if (method === "GET") return { status: 503, body: { error: "temporary status failure" } };
      stored = answer(runId, body, "pending", "", { deadline_at: new Date(Date.now() + 500).toISOString() });
      return { status: 200, body: stored };
    };
    let rejection = 0;
    api.onState(c.run_id, (b) => {
      if (b.status === "awaiting_approval") {
        assert.ok(Date.now() >= Date.parse(stored.deadline_at), "fallback uses locally stored deadline");
        rejection = send(c.run_id, row("reject_plan", "reject expired checker"))[0]!.id;
      }
    });
    const { exec, verdicts } = checkedExec();
    const done = start(exec, c);
    try {
      await done;
      assert.deepEqual(verdicts.map((v) => v.kind), ["reject"]);
      assert.ok(api.isApplied(c.run_id, rejection));
      assert.equal(api.crossCheckRequests.filter((r) => r.method === "POST").length, 1);
      assert.ok(api.crossCheckRequests.filter((r) => r.method === "GET").length > 1);
      assert.ok(api.crossCheckReplies.filter((r) => r.method === "GET").every((r) => r.status === 503));
      assert.equal(stored.verdict, "pending", "server never supplied a timed_out verdict");
      assert.equal((gates(c.run_id)[0] as PlanCrossCheckStateRequest).plan_cross_check_gate_reason, "timed_out");
      assert.equal(gates(c.run_id).length, 1);
      assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate).length, 1);
      terminal(c.run_id, "reject expired checker");
    } finally { done.shutdown(); await done; }
  });

  for (const control of ["cancel", "shutdown"] as const) {
    it(`owner ${control} aborts a held pending checker status socket before runner settlement`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim();
      let stored!: ReturnType<typeof answer>;
      api.crossCheckHandler = ({ runId, body, method }) => {
        assert.equal(method, "POST", "held GET must not fabricate a status answer");
        stored = answer(runId, body, "pending", "");
        return { status: 200, body: stored };
      };
      let entered = false;
      let settled = false;
      const status = client.planCrossCheckStatus;
      client.planCrossCheckStatus = async (...args) => {
        try { return await status.apply(client, args); } finally { settled = true; }
      };
      api.holdCrossCheckStatusUntilAbort = (id) => {
        if (id !== c.run_id) return false;
        entered = true;
        return true;
      };
      const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "checked-held-stop-"));
      const { log, warnings } = diagnosticLogger();
      const recovery = new RecoveryCoordinator({ client: {
        reserveRecoveryCapture: async () => { throw new Error("held stop must not capture"); },
        getRecoveryCaptureStatus: async () => { throw new Error("held stop must not query capture"); },
        uploadRecoveryBundle: async () => { throw new Error("held stop must not upload"); },
        listRecoveryHolds: async (id) => ({ run_id: id, holds: [] }),
        releaseRecoveryCustody: async (id) => ({ run_id: id, released: true, holds_released: 1 }),
      } as RecoveryArchiveClient, git, log, recoveryRoot: path.join(root, "recovery"),
      workerToken: "held-stop-fixture" });
      const pin = recovery.pin.bind(recovery);
      let pins = 0;
      recovery.pin = async (input) => { pins++; return pin(input); };
      let clone = "";
      const session = path.join(root, "session");
      const { exec, verdicts } = checkedExec(async (ctx) => {
        clone = ctx.worktreePath;
        fs.mkdirSync(session);
        fs.writeFileSync(path.join(session, "transcript"), "resume");
        fs.mkdirSync(skillsPluginDir(clone), { recursive: true });
        fs.writeFileSync(path.join(skillsPluginDir(clone), "marker"), "resume");
      });
      const close = MessageBatcher.prototype.close;
      let closes = 0;
      MessageBatcher.prototype.close = async function () { closes++; return close.call(this); };
      const forge = fakeGitlab();
      const run = runnerWith(() => ({ executor: exec, homeDir: session }), forge.gitlab, undefined, log, {
        recovery, planCrossCheckTiming: { ...timing, requestMs: 1000 }, planApprovalTimeoutMs: 4000,
      });
      let executionError: unknown;
      const done = run.execute(c).catch((error: unknown) => { executionError = error; });
      try {
        assert.ok(await until(() => entered));
        assert.equal(settled, false);
        if (control === "cancel") {
          const id = send(c.run_id, row("cancel"))[0]!.id;
          await routed(c.run_id, id);
        } else run.shutdown();
        await done;
        assert.ok(settled, "owned client request settles before runner returns");
        assert.ok(await until(() => api.abortedHeldCrossCheckStatuses === 1), "server observes socket cancellation");
        assert.equal(api.crossCheckRequests.length, 2, "one candidate and one held status request");
        assert.deepEqual(api.crossCheckRequests.map((r) => r.method), ["POST", "GET"]);
        assert.equal(verdicts.length, 0);
        assert.equal(gates(c.run_id).length, 0);
        assert.equal(api.crossCheckReplies.length, 1, "no manufactured status reply");
        assert.equal(executionError, undefined, "lifecycle settlement must not throw from a cancelled reservation");
        assert.equal(closes, 1, "existing lifecycle closes the batcher exactly once");
        const refusals = warnings.filter((w) => w.message === "worker diagnostic refused by failed candidate reservation");
        assert.ok(refusals.length > 0, "operator sees diagnostic refusal");
        assert.ok(refusals.every((w) => w.fields?.reason === "cancelled"));
        if (control === "cancel") {
          assert.equal(statuses(c.run_id).filter((s) => s === "failed").length, 1, "cancel keeps the generic terminal");
          assert.ok(!fs.existsSync(session), "cancel cleans up session");
        } else {
          assert.ok(!statuses(c.run_id).includes("failed"), "shutdown leaves the run for the sweeper");
          assert.ok(pins >= 2, "shutdown reaches the existing durable generation pin after the initial pin");
          assert.equal(fs.readFileSync(path.join(session, "transcript"), "utf8"), "resume");
          assert.equal(fs.readFileSync(path.join(skillsPluginDir(clone), "marker"), "utf8"), "resume");
        }
        assert.ok(!fs.existsSync(clone), "normal close removes the runner clone");
        assert.ok(!statuses(c.run_id).includes("recovery_wait"));
        assert.ok(!statuses(c.run_id).includes("completed"));
        assert.ok(!api.states.some((s) => s.runId === c.run_id && s.body.status === "running" && s.body.plan_md));
      } finally {
        run.shutdown();
        try { await done; } finally {
          api.holdCrossCheckStatusUntilAbort = undefined;
          client.planCrossCheckStatus = status;
          MessageBatcher.prototype.close = close;
          fs.rmSync(root, { recursive: true, force: true });
        }
      }
    });
  }

  it("healthy worker error diagnostic remains deliverable through ordinary terminal reporting", async () => {
    const c = claim();
    await start({ run: async () => { throw new Error("ordinary diagnostic"); } }, c);
    terminal(c.run_id, "ordinary diagnostic");
    assert.ok(api.messages(c.run_id).some((m) => m.kind === "error" && m.agent === "worker" &&
      (m.payload as { text?: string }).text === "ordinary diagnostic"));
  });

  for (const crossCheckFailure of [false, true]) {
    it(`unrelated diagnostic emit exception propagates with identical refusal text (checked failure: ${crossCheckFailure})`, { skip: crossCheckFailure && LINUX_CAPTURE }, async () => {
      const c = claim();
      if (crossCheckFailure) api.usageHandler = () => ({ status: 400, body: { error: "usage refused" } });
      const { exec } = checkedExec(async (ctx) => {
        const leg = ctx.usage!.startLeg();
        leg.observeAssistant({ message: { id: "unrelated-usage", model: "claude", usage: { input_tokens: 1 } } });
        leg.close();
      });
      const emit = MessageBatcher.prototype.emit;
      const unrelated = new Error("candidate transport reservation refused an event");
      MessageBatcher.prototype.emit = function (message) {
        if (message.kind === "error" && message.agent === "worker") throw unrelated;
        return emit.call(this, message);
      };
      try {
        await assert.rejects(start(crossCheckFailure ? exec : {
          run: async () => { throw new Error("ordinary failure"); },
        }, c), (error: unknown) => error === unrelated);
        assert.ok(!statuses(c.run_id).includes("failed"), "unrelated exception is not silently consumed");
      } finally { MessageBatcher.prototype.emit = emit; }
    });
  }

  it("reservation acquisition failure terminates after one call without candidate or human publication", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    let attempts = 0;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      attempts++;
      throw new Error("fixture reservation acquisition failure");
    };
    const { exec, verdicts } = checkedExec();
    try {
      await start(exec, c);
      assert.equal(attempts, 1, "no second reservation handle");
      assert.equal(verdicts.length, 0);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      terminal(c.run_id, "transport reservation unavailable");
    } finally { MessageBatcher.prototype.reserveCandidateTransport = reserve; }
  });

  it("real disabled outbox is a permanent runner preparation failure before any candidate", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "checked-disabled-"));
    const blocked = path.join(root, "file");
    fs.writeFileSync(blocked, "not a directory");
    const outbox = new Outbox({ root: path.join(blocked, "outbox"), log: nullLogger(),
      runMaxBytes: 1024, maxBytes: 4096, retentionMs: 86_400_000 });
    await outbox.init();
    assert.ok(outbox.isDisabled());
    const { exec, verdicts } = checkedExec();
    const forge = fakeGitlab();
    const run = runner(exec, forge.gitlab, undefined, { outbox, planCrossCheckTiming: timing });
    try {
      await run.execute(c);
      terminal(c.run_id, "transport permanently failed (outbox_unavailable)");
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      assert.equal(verdicts.length, 0);
    } finally { run.shutdown(); fs.rmSync(root, { recursive: true, force: true }); }
  });

  it("runner cannot borrow an ACK when a current-generation external outbox drain retires its tracked identity", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    c.last_seq = 1;
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "checked-external-retire-"));
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
    await outbox.init();
    await outbox.appendSegment(c.run_id, 1, [{ seq: 1, kind: "status", payload: { text: "externally delivered original" } }]);
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    let reserves = 0;
    let externalDrain: Promise<{ retired: boolean; staleRetired: number }> | undefined;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      reserves++;
      const reservation = reserve.call(this);
      // The real drainer queues first on the real outbox lock. It delivers at the same
      // generation but outside the reservation's delivery callback.
      externalDrain = outbox.drainRun(c.run_id, async (messages, generation) => {
        assert.equal(generation, c.claim_generation);
        await client.postMessages(c.run_id, messages, generation);
      });
      return reservation;
    };
    const { exec, verdicts } = checkedExec();
    const forge = fakeGitlab();
    const run = runner(exec, forge.gitlab, undefined, { outbox, planCrossCheckTiming: { ...timing, preparationMs: 1000 } });
    try {
      await run.execute(c);
      assert.ok(externalDrain);
      assert.deepEqual(await externalDrain, { retired: true, staleRetired: 0 });
      assert.equal(reserves, 1);
      assert.ok(!outbox.hasUndrainedMessages(c.run_id), "external delivery legitimately retired the actual record");
      assert.equal(api.messages(c.run_id).filter((m) => (m.payload as { text?: string }).text === "externally delivered original").length, 1);
      terminal(c.run_id, "preparation receipts irrecoverably lost");
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      assert.equal(verdicts.length, 0);
    } finally {
      run.shutdown();
      try { await externalDrain; } finally {
        MessageBatcher.prototype.reserveCandidateTransport = reserve;
        fs.rmSync(root, { recursive: true, force: true });
      }
    }
  });

  it("actual held producers overflow the bounded reservation before candidate preparation", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    let reserves = 0;
    let produced = 0;
    let refused = 0;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      reserves++;
      const reservation = reserve.call(this);
      // At most 64 events, each below the individual wire cap; stop on the first refusal.
      const text = "held producer payload ".repeat(4000);
      for (let i = 0; i < 64; i++) {
        try { this.emit({ kind: "status", payload: { text, producer: i } }); produced++; }
        catch { refused++; break; }
      }
      return reservation;
    };
    const { exec, verdicts } = checkedExec();
    const { log, warnings } = diagnosticLogger();
    const forge = fakeGitlab();
    const run = runnerWith(() => ({ executor: exec }), forge.gitlab, undefined, log,
      { planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 });
    try {
      await run.execute(c);
      assert.equal(reserves, 1);
      assert.ok(produced > 0 && produced < 64, "real buffer limit reached");
      assert.equal(refused, 1);
      assert.ok(warnings.some((w) => w.message === "worker diagnostic refused by failed candidate reservation" &&
        w.fields?.reason === "overflow"), "operator sees the bounded overflow reason");
      terminal(c.run_id, "transport permanently failed (overflow)");
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      assert.equal(verdicts.length, 0);
    } finally { MessageBatcher.prototype.reserveCandidateTransport = reserve; }
  });

  it("public Worker heartbeats advance while its real checked runner holds the human state ACK", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
    const held = holdGateAck(c.run_id, PLAN);
    let rejectId = 0;
    api.onState(c.run_id, (b) => {
      if (b.status === "awaiting_approval") rejectId = send(c.run_id, row("reject_plan", "finish heartbeat proof"))[0]!.id;
    });
    const { exec, verdicts } = checkedExec();
    const forge = fakeGitlab();
    const run = runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 });
    const register = client.register;
    const heartbeat = client.heartbeat;
    const claimRun = client.claimRun;
    const claimChat = client.claimChat;
    let claimed = false;
    let heartbeats = 0;
    client.register = async () => ({});
    client.heartbeat = async (...args) => {
      const response = await heartbeat.apply(client, args);
      heartbeats++;
      return response;
    };
    client.claimRun = async () => {
      if (claimed) return null;
      claimed = true;
      return c;
    };
    client.claimChat = async () => null;
    const controller = new AbortController();
    const config = { workerName: "checked-heartbeat", workerTemplate: "base",
      pollIntervalMs: 5, heartbeatIntervalMs: 10, chatPollMs: 5, chatSessions: 1,
      maxConcurrentRuns: 1, dockerWiring: {}, dataDir: homeDir } as Config;
    const worker = new Worker(config, client, run,
      { execute: async () => {} } as unknown as ChatRunner,
      { execute: async () => {} } as unknown as JudgeRunner,
      { execute: async () => {} } as unknown as ReviewRunner,
      nullLogger(), () => ({ ok: true, missing: [] }));
    const done = worker.run(controller.signal);
    try {
      assert.ok(await until(() => rejectId > 0));
      await routed(c.run_id, rejectId);
      const before = heartbeats;
      assert.ok(await until(() => heartbeats >= before + 3, 2000),
        "three successful public heartbeat ticks during the real held checked-state ACK");
      assert.equal(verdicts.length, 0);
      assert.ok(!api.isApplied(c.run_id, rejectId));
      assert.equal(gates(c.run_id).length, 1);
      assert.equal(api.crossCheckRequests.length, 1);
      held.release();
      assert.ok(await until(() => statuses(c.run_id).includes("failed")));
      assert.ok(api.isApplied(c.run_id, rejectId));
      terminal(c.run_id, "finish heartbeat proof");
    } finally {
      held.release();
      run.shutdown();
      controller.abort();
      try { await done; } finally {
        client.register = register;
        client.heartbeat = heartbeat;
        client.claimRun = claimRun;
        client.claimChat = claimChat;
      }
    }
  });

  it("pending candidate usage failure shares the original preparation budget and falls back before another check", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    let context!: RunContext;
    api.crossCheckHandler = ({ runId, body, method }) => {
      assert.equal(method, "POST", "no fresh status check after retryable preparation failure");
      const leg = context.usage!.startLeg();
      leg.observeAssistant({ message: { id: "pending-usage", model: "claude", usage: { input_tokens: 4 } } });
      leg.close();
      api.usageHandler = () => ({ status: 503, body: { error: "transient usage" } });
      return { status: 200, body: answer(runId, body, "pending", "") };
    };
    const held = holdGateAck(c.run_id, PLAN);
    let queued = 0;
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") queued = send(c.run_id, row("reject_plan"))[0]!.id; });
    const { exec, verdicts } = checkedExec(async (ctx) => { context = ctx; });
    const done = start(exec, c);
    try {
      assert.ok(await until(() => queued > 0));
      await routed(c.run_id, queued);
      // Observe beyond the recorder's first retry backoff while the forced ACK is held.
      assert.equal(await until(() => api.usageRequests.length > 1, 1200), false,
        "no independent usage retry before the applied forced gate ACK");
      assert.equal(api.usageRequests.length, 1, "one failed usage request before forced ACK");
      assert.equal(verdicts.length, 0);
      api.usageHandler = () => ({ status: 200, body: {} });
      held.release();
      await done;
      assert.equal(api.usageRequests.length, 2, "only one remaining preparation attempt after initial success and pending failure");
      assert.deepEqual(api.usageRequests[1], api.usageRequests[0], "exact usage receipt replay");
      assert.deepEqual(verdicts.map((v) => v.kind), ["reject"]);
      assert.equal(api.crossCheckRequests.length, 1);
    } finally {
      held.release();
      done.shutdown();
      await done;
    }
  });

  it("three preparation failures terminate only after applied forced gate and preserve assigned replay on close", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    const root = fs.mkdtempSync(path.join(fs.realpathSync(os.tmpdir()), "checked-preparation-"));
    const outbox = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
    await outbox.init();
    let archived = false;
    const archive: RecoveryArchiveClient = {
      reserveRecoveryCapture: async () => ({ capture_id: "preparation-capture", state: "preparing" }),
      getRecoveryCaptureStatus: async (_id, captureId) => ({ capture_id: captureId, state: "preparing", manifest_bound: false }),
      uploadRecoveryBundle: async (_id, captureId, _manifest, bundle) => {
        for await (const _chunk of bundle) { /* Consume the real Git bundle. */ }
        archived = true;
        return { capture_id: captureId, state: "available", manifest_bound: true };
      },
      releaseRecoveryCustody: async (id) => ({ run_id: id, released: true, holds_released: 1 }),
      listRecoveryHolds: async (id) => ({ run_id: id, holds: [] }),
    };
    const recovery = new RecoveryCoordinator({ client: archive, git, log: nullLogger(),
      recoveryRoot: path.join(root, "recovery"), workerToken: "preparation-worker-fixture" });
    const held = holdGateAck(c.run_id, PLAN);
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      this.emit({ kind: "status", payload: { text: "persistent prefix" } });
      api.failMessagesNext(100);
      return reserve.call(this);
    };
    const post = client.postMessages.bind(client);
    const report = client.reportPlanCrossCheckGateState.bind(client);
    const attempts: OutgoingMessage[][] = [];
    let applied = false;
    let queued = 0;
    let clone = "";
    client.postMessages = async (id, msgs, generation, signal) => {
      if (id === c.run_id && msgs.some((m) => (m.payload as { text?: string }).text === "persistent prefix")) {
        attempts.push(structuredClone(msgs));
        if (attempts.length > 1) assert.equal(applied, true);
      }
      return post(id, msgs, generation, signal);
    };
    client.reportPlanCrossCheckGateState = async (id, fields, signal) => {
      const ack = await report(id, fields, signal);
      if (id === c.run_id && fields.status === "awaiting_approval") applied = ack.applied;
      return ack;
    };
    api.onState(c.run_id, (b) => {
      if (b.status === "awaiting_approval") queued = send(c.run_id, row("approve_plan"), row("revise_plan", "queued"))[0]!.id;
    });
    const { exec, verdicts } = checkedExec(async (ctx) => {
      clone = ctx.worktreePath;
      fs.writeFileSync(path.join(clone, "preparation-custody.txt"), "retain planning work");
      const leg = ctx.usage!.startLeg();
      leg.observeAssistant({ message: { id: "persistent-usage", model: "claude", usage: { input_tokens: 3 } } });
      leg.close();
    });
    const forge = fakeGitlab();
    const run = runner(exec, forge.gitlab, undefined, { outbox, recovery, planCrossCheckTiming: timing,
      planApprovalTimeoutMs: 4000 });
    const done = Object.assign(run.execute(c), { shutdown: () => run.shutdown() });
    try {
      assert.ok(await until(() => queued > 0));
      await routed(c.run_id, queued);
      assert.equal(attempts.length, 1);
      assert.equal(verdicts.length, 0);
      held.release();
      await done;
      assert.equal(applied, true);
      assert.equal(attempts.length, 3, "preparation attempts counted separately from close");
      assert.deepEqual(attempts[1], attempts[0]);
      assert.deepEqual(attempts[2], attempts[0]);
      terminal(c.run_id, "plan cross-check: preparation ACKs unrecoverable");
      assert.equal(verdicts.length, 0);
      assert.equal(api.crossCheckRequests.length, 0);
      assert.equal(gates(c.run_id).length, 1);
      assert.equal(api.isApplied(c.run_id, queued), false);
      assert.ok(outbox.hasUndrainedMessages(c.run_id), "close spills the reserved assigned prefix");
      const recovered: OutgoingMessage[] = [];
      await outbox.drainRun(c.run_id, async (msgs) => { recovered.push(...msgs); });
      for (const original of attempts[0]!) assert.ok(recovered.some((m) => JSON.stringify(m) === JSON.stringify(original)));
      assert.equal(archived, true, "existing terminal custody uploads a real Git bundle");
    } finally {
      held.release();
      done.shutdown();
      try { await done; } finally {
        client.postMessages = post;
        client.reportPlanCrossCheckGateState = report;
        MessageBatcher.prototype.reserveCandidateTransport = reserve;
        fs.rmSync(root, { recursive: true, force: true });
      }
    }
  });

  for (const httpStatus of [400, 409]) {
    it(`negative canonical storage ACK ${httpStatus} prevents implementation`, { skip: LINUX_CAPTURE }, async () => {
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
    it(`server wall park at ${decision} ACK preserves clone and emits no terminal`, { skip: LINUX_CAPTURE }, async () => {
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
  it("stale canonical storage ACK ends quietly", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body) });
    api.failStateWhen(c.run_id, (b) => b.status === "running" && b.candidate_digest === digest,
      { httpStatus: 409, runStatus: "running", disposition: "stale_claim" });
    await start(checkedExec().exec, c);
    assert.ok(!statuses(c.run_id).includes("failed"), `superseded owner must not terminal-report: ${JSON.stringify(statuses(c.run_id))}`);
  });


  it("stores the server canonical bundle, including explicit empty arrays, before returning cross_check approval", { skip: LINUX_CAPTURE }, async () => {
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
    it(`forces a human gate for ${reason}, with no unchecked implementation`, { skip: LINUX_CAPTURE }, async () => {
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
    it(`forces the supported human disposition on submit ${reason}`, { skip: LINUX_CAPTURE }, async () => {
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
    it(`forces human rejection for ${verdict} ${JSON.stringify(override)} with intact preparation`, { skip: LINUX_CAPTURE }, async () => {
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
        done.shutdown();
        try { await done; } finally { client.reportPlanCrossCheckGateState = report; }
      }
    });
  }

  it("inactive preparation remains terminal before an identity fallback", { skip: LINUX_CAPTURE }, async () => {
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
    assert.equal(verdicts.length, 0);
    assert.equal(gates(c.run_id).length, 0);
    assert.equal(api.crossCheckRequests.length, 1);
  });

  it("a real usage 400 irrecoverably loses preparation receipts and terminates before submit", { skip: LINUX_CAPTURE }, async () => {
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
    it(`unrecoverable ${revised ? "revised" : "initial"} human ACK uses named terminal failure`, { skip: LINUX_CAPTURE }, async () => {
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
    it(`dropped initial ACK + queued revise before identical retry ACK resolves revised ${final}`, { skip: LINUX_CAPTURE }, async () => {
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

  for (const droppedRound of [0, 1]) {
    for (const final of ["approve_plan", "reject_plan"] as const) {
      it(`SDK dropped ${droppedRound === 0 ? "initial" : "revised"} ACK routes next revise before retry ACK then current ${final}`, { skip: LINUX_CAPTURE }, async () => {
        const c = claim();
        const plans = [PLAN, "# PLAN SDK revision B", "# PLAN SDK revision C"].slice(0, droppedRound + 2);
        const milestones = [
          [{ id: "original", title: "original milestone must disappear" }],
          droppedRound === 0 ? [] : [{ id: "middle", title: "middle milestone must disappear" }],
          [],
        ];
        api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
        const matches = (b: { status: string; plan_md?: string }) =>
          b.status === "awaiting_approval" && b.plan_md === plans[droppedRound];
        api.afterPersistState(c.run_id, matches, "drop");
        let releaseRetry!: () => void;
        const retryAck = new Promise<void>((resolve) => { releaseRetry = resolve; });
        let releaseFinal!: () => void;
        const finalAck = new Promise<void>((resolve) => { releaseFinal = resolve; });
        const report = client.reportPlanCrossCheckGateState;
        const requests: PlanCrossCheckStateRequest[] = [];
        const snapshots: string[] = [];
        client.reportPlanCrossCheckGateState = async (id, fields, signal) => {
          if (id === c.run_id && fields.status === "awaiting_approval") {
            requests.push(fields);
            snapshots.push(JSON.stringify(fields));
          }
          return report.call(client, id, fields, signal);
        };
        let attempts = 0;
        let context!: RunContext;
        const reviseIds: number[] = [];
        let finalId = 0;
        api.onState(c.run_id, (b) => {
          if (b.status !== "awaiting_approval") return;
          if (matches(b)) {
            if (++attempts === 1) context.emit({ kind: "status", payload: { text: "SDK held delivery" } });
            if (attempts === 2) {
              api.afterPersistState(c.run_id, matches, retryAck);
              reviseIds.push(send(c.run_id, row("revise_plan", "change the current SDK plan"))[0]!.id);
            }
          } else if (b.plan_md === plans.at(-1)) {
            api.afterPersistState(c.run_id, (next) => next.status === "awaiting_approval" && next.plan_md === plans.at(-1), finalAck);
            finalId = send(c.run_id, row(final, "current SDK decision"))[0]!.id;
          } else {
            reviseIds.push(send(c.run_id, row("revise_plan", "first SDK revision"))[0]!.id);
          }
        });
        const prompts: string[] = [];
        const resumedSessions: Array<string | undefined> = [];
        let turns = 0;
        const queryFn: SdkQueryFn = (params) => {
          const turn = turns++;
          resumedSessions.push(params.options.resume);
          assert.ok(turn <= plans.length, "bounded scripted SDK turns");
          if (turn === plans.length) {
            assert.equal(final, "approve_plan", "reject must never implement");
            assert.ok(api.isApplied(c.run_id, finalId), "implementation needs current approval receipt");
          }
          return (async function* () {
            let prompt = "";
            for await (const frame of params.prompt) prompt += JSON.stringify(frame);
            prompts[turn] = prompt;
            yield assistant(turn < plans.length ? [{
              type: "tool_use", id: `sdk-plan-${turn}`, name: "mcp__uzi__submit_plan",
              input: { plan_md: plans[turn], milestones: milestones[turn] },
            }] : [{ type: "tool_use", id: "sdk-done", name: "mcp__uzi__signal_done", input: {} }]);
            yield resultOk();
          })();
        };
        let rejected: unknown;
        const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn });
        const exec: Executor = { run: async (ctx) => {
          context = ctx;
          try { return await sdk.run(ctx); } catch (error) { rejected = error; throw error; }
        } };
        const forge = fakeGitlab();
        const run = runner(exec, forge.gitlab, undefined, {
          planCrossCheckTiming: { ...timing, requestMs: 1000 }, planApprovalTimeoutMs: 8000,
        });
        const done = run.execute(c);
        try {
          assert.ok(await until(() => reviseIds.length === droppedRound + 1), JSON.stringify({ states: api.states, turns, rejected: String(rejected) }));
          const pendingRevise = reviseIds.at(-1)!;
          await routed(c.run_id, pendingRevise);
          assert.equal(attempts, 2);
          assert.equal(turns, droppedRound + 1, "held retry cannot start another SDK revision or implementation");
          assert.ok(!api.isApplied(c.run_id, pendingRevise));
          if (droppedRound === 1)
            assert.ok(!api.isApplied(c.run_id, reviseIds[0]!), "first revise waits for revised applied ACK");
          const retryIndex = droppedRound;
          assert.deepEqual(gates(c.run_id)[retryIndex + 1], gates(c.run_id)[retryIndex], "transport retry keeps full request identity");
          assert.equal(JSON.stringify(gates(c.run_id)[retryIndex + 1]), snapshots[retryIndex], "identical full JSON, id, generation and session");
          assert.equal(requests[retryIndex]!.claim_generation, c.claim_generation);
          assert.equal(requests[retryIndex]!.session_id, "sess-e2e");
          if (droppedRound === 0)
            assert.ok(!api.messages(c.run_id).some((m) => (m.payload as { text?: string }).text === "SDK held delivery"));
          releaseRetry();
          assert.ok(await until(() => finalId > 0));
          await routed(c.run_id, finalId);
          assert.equal(turns, plans.length, "final ACK still blocks implementation");
          assert.ok(!api.isApplied(c.run_id, pendingRevise), "last revise waits for final presentation ACK");
          assert.ok(!api.isApplied(c.run_id, finalId));
          if (droppedRound === 1) assert.ok(api.isApplied(c.run_id, reviseIds[0]!));
          releaseFinal();
          await done;
          assert.equal(turns, plans.length + (final === "approve_plan" ? 1 : 0));
          assert.deepEqual(gates(c.run_id).map((g) => g.plan_md),
            [...plans.slice(0, droppedRound + 1), plans[droppedRound], ...plans.slice(droppedRound + 1)]);
          assert.deepEqual(requests.map((g) => JSON.stringify(g)), snapshots, "original strict request objects remain unchanged");
          assert.equal(new Set(requests.map((g) => g.presentation_id)).size, plans.length,
            "only normal human revisions allocate fresh presentation ids");
          for (const id of [...reviseIds, finalId]) assert.ok(api.isApplied(c.run_id, id), `receipt ${id} settled`);
          assert.equal(api.crossCheckRequests.length, 1);
          const candidate = api.crossCheckRequests[0]!.body;
          assert.equal(candidate.plan_md, PLAN, "historical checker candidate remains original");
          assert.deepEqual(candidate.milestones, milestones[0]);
          assert.equal(api.crossCheckReplies.filter((r) => r.acceptedCandidate).length, 1);
          assert.equal(api.messages(c.run_id).filter((m) => (m.payload as { text?: string }).text === "SDK held delivery").length, 1);
          assert.ok(!statuses(c.run_id).includes("recovery_wait"));
          assert.deepEqual(gates(c.run_id).at(-1)!.milestones, []);
          if (final === "approve_plan") {
            assert.ok(statuses(c.run_id).includes("completed"));
            assert.equal(resumedSessions.at(-1), "sess-e2e", "implementation resumes the session that submitted the current plan");
            assert.ok(prompts.at(-1)!.includes("Your plan was approved"));
            assert.ok(!prompts.at(-1)!.includes("original milestone must disappear"));
            assert.ok(!prompts.at(-1)!.includes("middle milestone must disappear"));
          } else {
            assert.ok(rejected instanceof PlanRejectedError);
            terminal(c.run_id, "current SDK decision");
          }
        } finally {
          releaseRetry();
          releaseFinal();
          run.shutdown();
          try { await done; } finally { client.reportPlanCrossCheckGateState = report; }
        }
      });
    }
  }

  for (const switchBeforeRevision of [false, true]) {
    it(`checked SDK retains absolute human deadline through held revision planning${switchBeforeRevision ? " and switch give-up" : ""}`, { skip: LINUX_CAPTURE }, async () => {
      const c = claim();
      api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "revise", "revise") });
      const budgetMs = 1000;
      let firstPublishedAt = 0;
      let revisedPublishedAt = 0;
      let revisionEntered = false;
      let releaseRevision!: () => void;
      const revisionHold = new Promise<void>((resolve) => { releaseRevision = resolve; });
      let reviseId = 0;
      let switches = 0;
      let turns = 0;
      const revised = "# PLAN after original approval deadline";
      api.onState(c.run_id, (b) => {
        if (b.status === "awaiting_approval" && b.plan_md === PLAN && !firstPublishedAt) {
          firstPublishedAt = Date.now();
          if (switchBeforeRevision) api.requestCredentialSwitch(c.run_id, 1);
          else reviseId = send(c.run_id, row("revise_plan", "slow revision"))[0]!.id;
        }
        if (b.status === "credential_switch_failed")
          reviseId = send(c.run_id, row("revise_plan", "slow revision after give-up"))[0]!.id;
        if (b.status === "awaiting_approval" && b.plan_md === revised) revisedPublishedAt = Date.now();
      });
      const queryFn: SdkQueryFn = (params) => {
        const turn = turns++;
        assert.ok(turn < 2, "expired approval cannot start implementation");
        return (async function* () {
          for await (const _frame of params.prompt) { /* Drain each SDK prompt once. */ }
          if (turn === 1) { revisionEntered = true; await revisionHold; }
          yield assistant([{ type: "tool_use", id: `deadline-plan-${turn}`, name: "mcp__uzi__submit_plan",
            input: { plan_md: turn === 0 ? PLAN : revised, milestones: [] } }]);
          yield resultOk();
        })();
      };
      const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn });
      const exec: Executor = { run: async (ctx) => {
        if (switchBeforeRevision) {
          git.worktreeStatus = async () => ["M src/impl.ts"];
          git.commitWipMarker = async () => false;
          const attempt = ctx.attemptCredentialSwitch!;
          ctx.attemptCredentialSwitch = async () => {
            assert.equal(++switches, 1, "one real give-up");
            const result = await attempt();
            assert.equal(result, "gave_up");
            return result;
          };
        }
        return sdk.run(ctx);
      } };
      const forge = fakeGitlab();
      const run = runner(exec, forge.gitlab, undefined, { planCrossCheckTiming: timing,
        planApprovalTimeoutMs: budgetMs, recoveryRetryMs: 1 });
      const done = run.execute(c);
      try {
        assert.ok(await until(() => revisionEntered));
        await routed(c.run_id, reviseId);
        assert.ok(!api.isApplied(c.run_id, reviseId), "revision receipt waits for new presentation");
        // The original gate anchors this wait; no per-poll or per-revision deadline mutation.
        await new Promise<void>((resolve) => setTimeout(resolve, Math.max(0, firstPublishedAt + budgetMs + 100 - Date.now())));
        assert.equal(gates(c.run_id).length, 1);
        releaseRevision();
        assert.ok(await until(() => revisedPublishedAt > 0));
        assert.ok(await until(() => statuses(c.run_id).includes("failed"), 700),
          "revised human waiter must expire immediately, not grant another 1000ms");
        await done;
        terminal(c.run_id, "plan approval timed out");
        assert.equal(turns, 2);
        assert.equal(switches, switchBeforeRevision ? 1 : 0);
        assert.equal(gates(c.run_id).length, 2, "only normal revision publishes a fresh gate");
        assert.equal(api.crossCheckRequests.length, 1);
        assert.ok(api.isApplied(c.run_id, reviseId));
      } finally { releaseRevision(); run.shutdown(); await done; }
    });
  }

  it("late round-one POSTs are refused after no_row fallback commit while its ACK is held", { skip: LINUX_CAPTURE }, async () => {
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

  it("dropped submit ACK resolves the current candidate by GET and stores one canonical bundle", { skip: LINUX_CAPTURE }, async () => {
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

  it("automatic REVISE releases concurrent feed producers and the session barrier before the next planning turn", { skip: LINUX_CAPTURE }, async () => {
    const c = claim();
    let context!: RunContext;
    let releasePost!: () => void;
    const post = new Promise<void>((resolve) => { releasePost = resolve; });
    let entered = false;
    const events: string[] = [];
    const reserve = MessageBatcher.prototype.reserveCandidateTransport;
    const capture = git.capturePlanningDiff.bind(git);
    const reportState = client.reportState.bind(client);
    let captures = 0;
    let sessionSends = 0;
    const session = "automatic-concurrent-session";
    MessageBatcher.prototype.reserveCandidateTransport = function () {
      const reservation = reserve.call(this);
      return { ...reservation,
        release: (response) => {
          if (response.result === "candidate" && response.round === 1)
            assert.equal(sessionSends, 0, "session sender is still behind the checked-state barrier at transport release");
          const released = reservation.release(response);
          assert.equal(released, true, "decided proof releases the real transport reservation");
          events.push(`release:${response.result === "candidate" ? response.round : 0}`);
          return released;
        },
        releaseAppliedGate: () => { throw new Error("automatic revision must not settle a human gate"); },
      };
    };
    git.capturePlanningDiff = async (...args) => {
      captures++;
      await capture(...args);
      return Buffer.from(`fresh concurrent candidate ${captures}`);
    };
    client.reportState = async (runId, body, ...args) => {
      if (runId === c.run_id && body.status === "running" && body.session_id === session) {
        assert.ok(events.includes("release:1"), "session HTTP cannot cross the held checked-state barrier");
        sessionSends++;
        events.push("session-send");
      }
      return reportState(runId, body, ...args);
    };
    api.crossCheckHandler = async ({ runId, body }) => {
      const round = Number(body.round);
      assert.equal(gates(runId).length, 0);
      assert.equal(api.states.filter((s) => s.runId === runId && s.body.plan_md).length, 0);
      if (round === 1) {
        assert.equal(api.usageRequests.length, 1, "usage ACK precedes candidate POST");
        assert.ok(api.messages(runId).some((m) => (m.payload as { text?: string }).text === "before automatic reservation"));
        entered = true;
        await post;
      } else {
        assert.equal(round, 2, "next candidate uses the explicit fresh round");
        assert.ok(sessionSends > 0, "independent session sender drains before the next candidate");
      }
      return { status: 200, body: answer(runId, body, round === 1 ? "revise" : "approve", round === 1 ? "revise" : "approve", {
        automatic_revision_limit: 2, automatic_rounds_enabled: true,
        findings: round === 1 ? { summary: "fresh candidate required", items: [] } : null,
        candidate: { plan_md: NORMALIZED, milestones: [{ title: "canonical concurrent", done: false }],
          required_capabilities: [], required_tools: [], size_class: "s",
          base_commit: body.base_commit, planning_diff: body.planning_diff },
      }) };
    };
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      context = ctx;
      ctx.emit({ kind: "status", payload: { text: "before automatic reservation" } });
      const leg = ctx.usage!.startLeg();
      leg.observeAssistant({ message: { id: "automatic-concurrent-usage", model: "claude", usage: { input_tokens: 3 } } });
      leg.close();
      const revised = await ctx.gatePlan!(PLAN);
      events.push("revision-return");
      seen.push(revised);
      assert.deepEqual(revised, { kind: "revise", automatic: true, round: 1,
        feedback: "fresh candidate required", items: [] });
      assert.ok(!("inputId" in revised));
      assert.ok(events.indexOf("release:1") < events.indexOf("session-send"));
      assert.ok(events.indexOf("session-send") < events.indexOf("revision-return"),
        "independent sender passed the released checked-state barrier before the next planning turn");
      assert.equal(gates(c.run_id).length, 0);
      assert.ok(!statuses(c.run_id).includes("awaiting_approval"));
      events.push("next-planning-turn");
      const approved = await ctx.gatePlan!(`${PLAN} fresh round two`);
      seen.push(approved);
      assert.ok(approved.kind === "approve" && approved.approval === "cross_check");
      assert.equal(approved.canonical.plan, NORMALIZED);
      assert.deepEqual(approved.canonical.milestones, [{ title: "canonical concurrent", done: false }]);
      return { branch: ctx.branch };
    } };
    const forge = fakeGitlab();
    const run = runner(exec, forge.gitlab, undefined, {
      planCrossCheckTiming: { ...timing, requestMs: 4000 }, planApprovalTimeoutMs: 4000,
    });
    const done = run.execute(c);
    try {
      assert.ok(await until(() => entered));
      await Promise.all(Array.from({ length: 4 }, async (_, i) => {
        context.emit({ kind: "status", payload: { text: `automatic producer ${i}` } });
      }));
      context.onSessionId!(session);
      const feed = api.messages(c.run_id);
      feed.push({ seq: feed.at(-1)!.seq + 1, kind: "status", payload: { text: "automatic server checker event" } });
      // Let the already queued sender run to its barrier while the POST remains held.
      await new Promise<void>((resolve) => setImmediate(resolve));
      assert.equal(sessionSends, 0);
      assert.ok(!api.states.some((s) => s.runId === c.run_id && s.body.session_id === session));
      assert.ok(!feed.some((m) => String((m.payload as { text?: string }).text).startsWith("automatic producer ")));
      assert.deepEqual(events, []);
      releasePost();
      await done;
      assert.deepEqual(seen.map((v) => v.kind), ["revise", "approve"], "no deadlock after automatic release");
      assert.ok(api.states.some((s) => s.runId === c.run_id && s.body.session_id === session),
        "independent sender's state was persisted");
      assert.equal(captures, 2);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2]);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.body.plan_md), [PLAN, `${PLAN} fresh round two`]);
      assert.deepEqual(api.crossCheckRequests.map((r) => r.body.planning_diff),
        ["fresh concurrent candidate 1", "fresh concurrent candidate 2"]);
      assert.equal(api.crossCheckLatestRequests.length, 0);
      assert.equal(gates(c.run_id).length, 0);
      assert.ok(!statuses(c.run_id).includes("awaiting_approval"));
      const stored = api.states.filter((s) => s.runId === c.run_id && s.body.plan_md);
      assert.equal(stored.length, 1, "only approved canonical bundle is stored");
      assert.equal(stored[0]!.body.plan_md, NORMALIZED);
      assert.equal(stored[0]!.body.candidate_digest, digest);
      assert.deepEqual(stored[0]!.body.milestones, [{ title: "canonical concurrent", done: false }]);
      const messages = api.messages(c.run_id);
      for (let i = 0; i < 4; i++)
        assert.equal(messages.filter((m) => (m.payload as { text?: string }).text === `automatic producer ${i}`).length, 1);
      assert.equal(messages.filter((m) => (m.payload as { text?: string }).text === "automatic server checker event").length, 1);
      assert.deepEqual(messages.map((m) => m.seq), Array.from({ length: messages.length }, (_, i) => i + 1));
    } finally {
      releasePost();
      run.shutdown();
      await done;
      MessageBatcher.prototype.reserveCandidateTransport = reserve;
      git.capturePlanningDiff = capture;
      client.reportState = reportState;
    }
  });

  for (const decision of ["approve", "revise"] as const) {
    it(`concurrent feed producers and session sender preserve sequences through ${decision} proof`, { skip: LINUX_CAPTURE }, async () => {
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

  it("dropped revised ACK preserves revision identity after the previous revision was confirmed", { skip: LINUX_CAPTURE }, async () => {
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

  it("scan refusal precedes any candidate upload", { skip: LINUX_CAPTURE }, async () => {
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

  it("logs only the fixed refusal vocabulary and reports the sub-code in the gate reason", async () => {
    const c = claim();
    const hostile = "bounded runner process failed: UZI-PLANNING-REFUSAL unsupported_symlink\n"
      + "Error: ENOENT private-name\nUZI-PLANNING-REFUSAL secret_detected";
    git.capturePlanningDiff = async () => { throw new Error(hostile); };
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("reject_plan")); });
    const { log, warnings } = diagnosticLogger();
    const forge = fakeGitlab();
    await runnerWith(() => ({ executor: checkedExec().exec, homeDir }), forge.gitlab, undefined, log,
      { planCrossCheckTiming: timing, planApprovalTimeoutMs: 4000 }).execute(c);
    const gate = gates(c.run_id)[0] as PlanCrossCheckStateRequest;
    assert.equal(gate.plan_cross_check_gate_reason, "planning_diff_refused");
    assert.equal(gate.plan_cross_check_diff_refusal, "unsupported_entry");
    const refused = warnings.filter((w) => w.message === "plan cross-check: planning diff refused");
    assert.deepEqual(refused.map((w) => w.fields), [{ refusal: "unsupported_entry", diagnostic: "unsupported_symlink" }]);
    const feed = api.messages(c.run_id).map((m) => (m.payload as { text?: string }).text);
    assert.ok(feed.includes("plan cross-check: planning diff refused (unsupported_entry: unsupported_symlink)"), JSON.stringify(feed));
    assert.ok(!JSON.stringify([warnings, feed]).includes("private-name"));
    assert.equal(api.crossCheckRequests.length, 0);
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

  it("captures and scans identical planning bytes against the immutable clone base", { skip: LINUX_CAPTURE }, async () => {
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

  it("real runner APPROVE reaches SDK implementation with server normalization and []", { skip: LINUX_CAPTURE }, async () => {
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
it(`checked canonical storage resumes after real switch give-up with ${committed ? "committed held ACK" : "write not committed"}`, { timeout: 10_000, skip: LINUX_CAPTURE }, async () => {
  const c = claim();
  api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
    candidate: { ...body, plan_md: NORMALIZED, milestones: [], required_capabilities: [], required_tools: [], size_class: "s" },
  }) });
  let requested = false;
  let gaveUp = false;
  let attempts = 0;
  let applied = false;
  let retried = false;
  let switchDelivered = false;
  let initialAbortedAfterControl = false;
  const getInputs = client.getInputs.bind(client);
  client.getInputs = async (runId) => {
    const result = await getInputs(runId);
    if (runId === c.run_id && result.credentialSwitch?.generation === 1) switchDelivered = true;
    return result;
  };
  let releaseBeforeDrop!: () => void;
  const beforeDrop = new Promise<void>((resolve) => { releaseBeforeDrop = resolve; });
  let releaseAck!: () => void;
  const heldAck = new Promise<void>((resolve) => { releaseAck = resolve; });
  let releaseInitialAck!: () => void;
  const initialAck = new Promise<void>((resolve) => { releaseInitialAck = resolve; });
  const canonical = (b: PlanCrossCheckStateRequest) => b.status === "running" && b.candidate_digest === digest;
  const requests: PlanCrossCheckStateRequest[] = [];
  const snapshots: string[] = [];
  const report = client.reportPlanCrossCheckGateState.bind(client);
  client.reportPlanCrossCheckGateState = async (runId, body, signal) => {
    if (canonical(body)) {
      requests.push(body);
      snapshots.push(JSON.stringify(body));
      if (requests.length === 1) signal?.addEventListener("abort", () => {
        initialAbortedAfterControl = switchDelivered;
      }, { once: true });
    }
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
  }, committed ? undefined : beforeDrop);
  if (committed) api.afterPersistState(c.run_id, (b) => {
    if (!canonical(b)) return false;
    requested = true;
    api.requestCredentialSwitch(c.run_id, 1);
    return true;
  }, initialAck);
  api.onState(c.run_id, (b) => {
    if (b.status === "credential_switch_failed") {
      gaveUp = true;
      assert.ok(switchDelivered, "real inputs control delivered before give-up");
      assert.ok(initialAbortedAfterControl, "owned HTTP request aborted after inputs control");
      releaseBeforeDrop();
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
    assert.equal(api.states.filter((s) => s.runId === c.run_id && canonical(s.body)).length, committed ? 2 : 1,
      "the held initial request cannot commit after dropping is restored");
    assert.equal(api.crossCheckRequests.length, 1);
    assert.equal(gates(c.run_id).length, 0, "no human publication");
    assert.ok(!statuses(c.run_id).includes("recovery_wait"));
    assert.ok(statuses(c.run_id).includes("completed"));
    assert.ok(prompts.slice(1).some((p) => p.includes("server normalized proposal")));
    assert.ok(!prompts.slice(1).some((p) => p.includes("local milestone")));
    assert.equal(api.messages(c.run_id).filter((m) => m.kind === "plan").length, 1, "no second initial plan emission");
  } finally {
    releaseBeforeDrop();
    releaseInitialAck();
    releaseAck();
    restore();
    run.shutdown();
    try { await done; } finally {
      client.getInputs = getInputs;
      client.reportPlanCrossCheckGateState = report;
    }
  }
});

for (const [checked, revised] of [[true, false], [false, false], [true, true]] as const)
it(`${checked ? "checked" : "ordinary"} confirmed human wait survives one real switch give-up${revised ? " then human revision" : ""}`, { skip: checked && LINUX_CAPTURE }, async () => {
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

// PRD #2460: a Codex lead (claim.secrets.codex) takes the same checked path as a Claude lead.
// It no longer parks as codex_lead_unsupported when the api signals plan_cross_check_codex_lead
// (an older api: see the "older api" describe below).
describe("RunRunner checked gate for a Codex lead (PRD #2460)", () => {
  const codexClaim = () => freshClaim(1, { kind: "issue", auto_approve: true, plan_cross_check_required: true,
    plan_cross_check_codex_lead: true,
    secrets: { forge_pat: "fixture-forge-pat-000000", codex: {
      auth_mode: "subscription" as const, access_token: "fixture-codex-access-token-abc123",
      capability: "fixture-codex-capability-abc123", generation: 1, chatgpt_account_id: "verified-account",
      chatgpt_plan_type: null } } });
  const canonicalOf = (body: Record<string, unknown>) => ({
    candidate: { ...body, plan_md: NORMALIZED, milestones: [{ title: "canonical", done: false }],
      required_capabilities: [], required_tools: [], size_class: "s" } });

  it("submits a candidate and takes the approved canonical plan (acceptance 1)", { skip: LINUX_CAPTURE }, async () => {
    const c = codexClaim();
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", canonicalOf(body)) });
    const { exec, verdicts } = checkedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 1, "the Codex lead's candidate is submitted for a cross-check");
    assert.equal(verdicts.length, 1);
    const v = verdicts[0]!;
    assert.ok(v.kind === "approve" && v.approval === "cross_check");
    assert.equal(v.kind === "approve" && v.approval === "cross_check" ? v.canonical.plan : "", NORMALIZED);
    const states = api.states.filter((s) => s.runId === c.run_id).map((s) => s.body);
    assert.ok(!states.some((s) => (s as { plan_cross_check_gate_reason?: string }).plan_cross_check_gate_reason === "codex_lead_unsupported"), "no unsupported park");
    assert.ok(!states.some((s) => s.status === "awaiting_approval"), "no human gate on APPROVE");
    assert.ok(statuses(c.run_id).includes("completed"));
  });

  // The Codex executor wires neither attemptCredentialSwitch nor deferCredentialSwitch, so a held-state
  // switch signal that reached a Codex lead mid-gate would take the runner's safety-net release. The
  // server never stamps one for a Codex run (ErrCredentialOverrideHarnessUnsupported); this proves the
  // worker side stays safe if one arrived anyway: the run is released NON-terminal and nothing
  // implements the checked approval.
  it("a credential switch signal at a Codex lead's checked gate releases the claim; it never fails or implements the run", { skip: LINUX_CAPTURE }, async () => {
    const c = codexClaim();
    api.crossCheckHandler = ({ runId, body }) => {
      api.requestCredentialSwitch(runId, 1);
      api.armStateAckCredentialSwitch(runId, 1);
      return { status: 200, body: answer(runId, body, "approve", "approve", canonicalOf(body)) };
    };
    let implemented = false;
    const exec: Executor = { run: async (ctx) => {
      const verdict = await ctx.gatePlan!(PLAN);
      // Both transports are armed (the inputs poll and the canonical running report's state ack), so
      // the test does not depend on which wins; gatePlan normally rejects with CredentialSwitchSignal.
      // This check is defensive only.
      ctx.signal?.throwIfAborted();
      if (verdict.kind === "approve") implemented = true;
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    const states = api.states.filter((s) => s.runId === c.run_id).map((s) => s.body);
    assert.ok(!states.some((s) => s.status === "failed"), `a switch never fails the run: ${JSON.stringify(states.map((s) => s.status))}`);
    assert.ok(states.some((s) => s.status === "credential_switch"), `the safety-net release ran: ${JSON.stringify(states.map((s) => s.status))}`);
    assert.ok(!states.some((s) => s.status === "completed"), "the run does not complete on the old claim");
    assert.equal(implemented, false, "no implementation follows the interrupted gate");
  });

  it("a reclaimed Codex lead discovers its eligible round once, like a Claude lead", { skip: LINUX_CAPTURE }, async () => {
    const c = freshClaim(2, { kind: "issue", auto_approve: true, plan_cross_check_required: true,
      plan_cross_check_codex_lead: true, secrets: codexClaim().secrets });
    api.crossCheckLatestHandler = () => ({ status: 200, body: {
      result: "latest", round: 1, candidate_generation: 1, automatic_revision_limit: 2,
      automatic_rounds_enabled: true, next_round: 2, next_round_eligible: true, fallback_reason: "",
    } });
    api.crossCheckHandler = ({ runId, body }) => ({ status: 200, body: answer(runId, body, "approve", "approve", {
      ...proof(runId, 2), candidate_generation: 2, automatic_revision_limit: 2, automatic_rounds_enabled: true,
    }) });
    const { exec, verdicts } = checkedExec();
    await start(exec, c);
    assert.deepEqual(api.crossCheckLatestRequests, [{ runId: c.run_id, generation: 2 }], "discovery ran for the Codex lead");
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [2]);
    assert.equal(verdicts[0]!.kind, "approve");
  });

  it("returns an automatic revision and submits round 2 after REVISE (acceptance 2)", { skip: LINUX_CAPTURE }, async () => {
    const c = codexClaim();
    api.crossCheckHandler = ({ runId, body }) => {
      const round = Number(body.round);
      return { status: 200, body: answer(runId, body, round < 2 ? "revise" : "approve", round < 2 ? "revise" : "approve", {
        automatic_revision_limit: 2, automatic_rounds_enabled: true,
        findings: round < 2 ? { summary: "tighten scope", items: [] } : null, ...canonicalOf(body) }) };
    };
    const seen: PlanVerdict[] = [];
    const exec: Executor = { run: async (ctx) => {
      for (let round = 1; round <= 2; round++) {
        const verdict = await ctx.gatePlan!(`${PLAN} ${round}`);
        seen.push(verdict);
        if (round === 1) assert.deepEqual(verdict, { kind: "revise", automatic: true, round: 1, feedback: "tighten scope", items: [] });
      }
      return { branch: ctx.branch };
    } };
    await start(exec, c);
    assert.deepEqual(api.crossCheckRequests.map((r) => r.body.round), [1, 2]);
    assert.deepEqual(seen.map((v) => v.kind), ["revise", "approve"]);
    assert.equal(gates(c.run_id).length, 0, "no human gate was published");
  });
});

// PRD #2460 rework: a new worker against an older api (no plan_cross_check_codex_lead) must not submit a
// Codex lead's check (the old api answers 409 cross_check_refused, unrecoverable for a non-Claude lead).
// It parks at the human gate as codex_lead_unsupported; this is a capability check, not a 409 mapping.
describe("Codex lead against an older api (no plan_cross_check_codex_lead)", () => {
  const oldApiCodexClaim = (generation: number) => freshClaim(generation, { kind: "issue", auto_approve: true,
    plan_cross_check_required: true,
    secrets: { forge_pat: "fixture-forge-pat-000000", codex: {
      auth_mode: "subscription" as const, access_token: "fixture-codex-access-token-abc123",
      capability: "fixture-codex-capability-abc123", generation: 1, chatgpt_account_id: "verified-account",
      chatgpt_plan_type: null } } });
  const assertParked = (c: { run_id: string }) => {
    const states = api.states.filter((s) => s.runId === c.run_id).map((s) => s.body);
    assert.ok(states.some((s) => s.status === "awaiting_approval" &&
      (s as { plan_cross_check_gate_reason?: string }).plan_cross_check_gate_reason === "codex_lead_unsupported"),
      `parked as codex_lead_unsupported: ${JSON.stringify(states)}`);
    assert.ok(api.messages(c.run_id).some((m) => m.kind === "status" &&
      (m.payload as { text?: string }).text === "plan cross-check: not yet supported for a Codex lead"));
    assert.ok(!states.some((s) => s.status === "failed"), "the run is not failed");
  };
  const humanApproves = (c: { run_id: string }) =>
    api.onState(c.run_id, (b) => { if (b.status === "awaiting_approval") send(c.run_id, row("approve_plan", "human ok")); });

  it("never submits a cross-check and parks at the human gate", { skip: LINUX_CAPTURE }, async () => {
    const c = oldApiCodexClaim(1);
    api.crossCheckHandler = () => ({ status: 409, body: { reason: "cross_check_refused" } });
    humanApproves(c);
    const { exec } = checkedExec();
    await start(exec, c);
    assert.equal(api.crossCheckRequests.length, 0, "no submit to an api that would refuse it");
    assertParked(c);
  });

  it("a reclaimed generation-2 Codex claim skips latest-round discovery and parks", { skip: LINUX_CAPTURE }, async () => {
    const c = oldApiCodexClaim(2);
    api.crossCheckLatestHandler = () => ({ status: 200, body: { result: "no_row" } });
    api.crossCheckHandler = () => ({ status: 409, body: { reason: "cross_check_refused" } });
    humanApproves(c);
    const { exec } = checkedExec();
    await start(exec, c);
    assert.equal(api.crossCheckLatestRequests.length, 0, "discovery skipped");
    assert.equal(api.crossCheckRequests.length, 0);
    assertParked(c);
  });
});
