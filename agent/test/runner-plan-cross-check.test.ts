import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import os from "node:os";
import { Outbox } from "../src/outbox.js";
import { Worker } from "../src/worker.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { PlanRejectedError, type Executor, type RunContext } from "../src/executor.js";
import { SdkExecutor } from "../src/sdk-executor.js";
import { MessageBatcher } from "../src/batcher.js";
import type { PlanCrossCheckStateRequest } from "../src/client.js";
import type { PlanVerdict } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, homeDir, installHarness, runner, runnerWith, fakeGitlab,
  planWithMilestonesThenDoneQuery } from "./runner-harness.js";
import { freshClaim, gates, holdGateAck, row, send, routed, statuses, until } from "./gate-revision-harness.js";
import { installCheckedGateFixture, PLAN, NORMALIZED, digest, diagnosticLogger, timing, claim, proof, answer, checkedExec, start, terminal, LINUX_CAPTURE } from "./runner-plan-cross-check-fixture.js";

installHarness();
installCheckedGateFixture();

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
