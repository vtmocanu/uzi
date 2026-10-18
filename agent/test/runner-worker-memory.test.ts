import assert from "node:assert/strict";
import { AsyncLocalStorage } from "node:async_hooks";
import { test } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { performance } from "node:perf_hooks";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, MemoryReservationRequest, StateAck } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { PauseNowSignal } from "../src/steering.js";
import { MessageBatcher } from "../src/batcher.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, homeDir, fakeGitlab, gitlabClaim, input, installHarness,
  runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function until(predicate: () => boolean, timeoutMs = 3000) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    assert.ok(Date.now() < deadline, "bounded runner memory fixture wait");
    await new Promise(resolve => setTimeout(resolve, 2));
  }
}

async function setup(count = 0, configured = settings) {
  api.memoryRegisterNonce = "original-memory-registration";
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence"]);
  await client.register("memory-fixture", "base");
  const claim: ClaimResponse = { ...gitlabClaim(2396), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 },
    memory_intervention_count: count };
  claim.secrets = { ...claim.secrets, codex: {
    auth_mode: "api_key", capability: "fixture-capability", access_token: "fixture-access",
  } };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  let usage = 0, sampleId = 0, used = count;
  const parkReceipts: StateAck[] = [];
  const reportState = client.reportState.bind(client);
  client.reportState = async (...args) => {
    const ack = await reportState(...args);
    if (args[1].recovery_cause === "worker_memory_pressure") parkReceipts.push(ack);
    return ack;
  };
  const reservations: MemoryReservationRequest[] = [];
  const accepted = new Map<string, number>();
  client.reserveMemoryIntervention = async request => {
    reservations.push(request);
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true,
      allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  const memory = new WorkerMemoryRuntime(configured, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time,
        currentBytes: usage, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { memory, claim, reservations, parkReceipts, pressure: (bytes: number) => { usage = bytes; } };
}

function releaseGate() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
}

// Observe custody on the actual flight without importing the runner's private type.
type MemoryFlightProof = {
  parked?: boolean;
  retainedLocalCustody?: boolean;
  workerMemory?: {
    operation?: Promise<void>; settled?: boolean; closing?: Promise<void>;
    feedback: Map<string, { operation?: Promise<boolean> }>;
  };
  steering: {
    terminalLifecycleSignal(): AbortSignal;
    seedPauseRequested(mode: string): void;
    requestDiskStop(): void;
    isCancelled(): boolean;
    claimFence(): string | undefined;
    tripCredentialSwitch(generation: number): void;
    pendingCredentialSwitch(): number | undefined;
  };
};

function enroll(ctx: RunContext, toolId = "original-tool") {
  const captured = ctx.capturedMemory!()!;
  let cancellations = 0;
  const entry = captured.commands.register(captured.context, {
    source: "codex-model-command", toolId, attemptId: "original-attempt",
    command: "inert fixture", root: { pid: 10, startTime: "10" },
    observe: async () => {
      const time = performance.now();
      return { available: true, complete: true, startedAtMs: time, completedAtMs: time,
        rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
    },
    cancel: async authority => { assert.equal(authority.authorized(), true); cancellations++; },
    drain: async () => ({ confirmed: true }),
  });
  return { captured, entry, cancellations: () => cancellations };
}

test("runner pressure joins the original tool/feed, refuses retries, rearms at sustained low Q and carries allowance across approval/input", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  let proof = false, executorError: unknown;
  const executor: Executor = { run: async ctx => {
    try {
      const first = enroll(ctx);
      f.pressure(950);
      const result = await first.captured.commands.watchOutcome(first.entry);
      ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
      assert.equal(first.cancellations(), 1);
      assert.equal(result.tool_id, "original-tool");
      assert.equal(result.attempt_id, "original-attempt");
      assert.equal(result.claim_generation, 7);
      assert.equal(first.captured.commands.admitsCommands(first.captured.context), false);
      assert.throws(() => enroll(ctx, "retry"), /untrusted memory command/);
      f.pressure(100);
      await until(() => first.captured.commands.admitsCommands(first.captured.context));
      await until(() => f.memory.sinkReceipts.length === 0);
      assert.equal(first.captured.commands.remaining(first.captured.context), 1);
      api.onState(f.claim.run_id, body => {
        if (body.status === "awaiting_approval") {
          assert.equal(first.captured.commands.isCurrent(first.captured.context), true);
          assert.equal(first.captured.commands.phaseAdmits(first.captured.context), false);
          assert.equal(ctx.capturedMemory!()!.context, first.captured.context);
          api.setInputs(f.claim.run_id, [input("approve_plan")]);
        }
        if (body.status === "awaiting_input") {
          const waiting = ctx.capturedMemory!()!;
          assert.equal(waiting.commands.isCurrent(waiting.context), true);
          assert.equal(waiting.commands.admitsCommands(waiting.context), false);
          api.setInputs(f.claim.run_id, [input("answer", JSON.stringify({
            question_id: body.open_question_id, answers: "proceed",
          }))]);
        }
      });
      await ctx.gatePlan!("Fixture plan");
      const runningBeforeCapture = api.states.filter(s => s.body.status === "running").length;
      const afterApproval = ctx.capturedMemory!()!;
      assert.equal(api.states.filter(s => s.body.status === "running").length, runningBeforeCapture,
        "approval capture must defer the running write until credential release");
      assert.equal(afterApproval.commands.admitsCommands(afterApproval.context), false,
        "fresh approval epoch stays phase-held before Running ACK");
      assert.notEqual(afterApproval.context, first.captured.context);
      assert.equal(afterApproval.flight, first.captured.flight);
      assert.equal(afterApproval.commands.remaining(afterApproval.context), 1, "approval renewal must not refund spent allowance");
      await ctx.askUser!([{ question: "Proceed?", header: "Choice" }]);
      const afterInput = ctx.capturedMemory!()!;
      assert.equal(afterInput.context, afterApproval.context);
      assert.equal(afterInput.commands.admitsCommands(afterInput.context), true);
      assert.equal(afterInput.commands.remaining(afterInput.context), 1, "input renewal must not refund spent allowance");
      assert.equal(first.captured.commands.isCurrent(first.captured.context), false);
      proof = true;
    } catch (error) { executorError = error; throw error; }
    throw new Error("fixture completed its proof");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    assert.equal(proof, true, "actual runner lifecycle proof reached");
    const feed = api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure");
    assert.equal(feed.length, 1, "one structured feedback event for original intervention");
    assert.equal(feed[0]!.payload.claim_generation, 7);
    assert.equal(JSON.stringify(feed).includes("original-memory-registration"), false);
    assert.equal(f.reservations.length, 1);
  } finally { await f.memory.stop(); await monitoring; }
});

test("runner human revise keeps the original phase context without a premature Running write", async () => {
  const f = await setup();
  let proof = false, executorError: unknown;
  const executor: Executor = { run: async ctx => {
    try {
      const original = ctx.capturedMemory!()!;
      const originalRevision = original.commands.phaseRevision(original.context);
      const before = api.states.filter(s => s.body.status === "running").length;
      let gates = 0;
      api.onState(f.claim.run_id, body => {
        if (body.status !== "awaiting_approval") return;
        gates++;
        assert.equal(ctx.capturedMemory!()!.context, original.context);
        assert.equal(original.commands.isCurrent(original.context), true);
        assert.equal(original.commands.phaseAdmits(original.context), false,
          "phase closes synchronously before an owner verdict");
        assert.equal(original.commands.phaseAdmits(original.context, originalRevision), false,
          "a reservation from the prior phase cannot gain signal authority");
        api.appendInputs(f.claim.run_id, [{
          ...input(gates === 1 ? "revise_plan" : "approve_plan", "Tighten the plan"),
          id: gates,
        }]);
      });
      const revise = await ctx.gatePlan!("First plan");
      assert.equal(revise.kind, "revise");
      assert.ok(revise.kind === "revise" && revise.inputId === 1);
      assert.equal(ctx.capturedMemory!()!.context, original.context);
      assert.equal(original.commands.admitsCommands(original.context), true);
      assert.equal(original.commands.phaseAdmits(original.context, originalRevision), false);
      assert.equal(api.states.filter(s => s.body.status === "running").length, before,
        "human revision continues with the API row awaiting approval");
      assert.equal(api.states.at(-1)!.body.status, "awaiting_approval");
      const approve = await ctx.gatePlan!("Revised plan", undefined, undefined, 1);
      assert.equal(approve.kind, "approve");
      assert.equal(gates, 2);
      assert.equal(api.states.filter(s => s.body.status === "running").length, before,
        "approval does not report Running before credentials are released");
      const successor = ctx.capturedMemory!()!;
      assert.notEqual(successor.context, original.context);
      assert.equal(successor.commands.admitsCommands(successor.context), false);
      await ctx.reportIteration!(1);
      assert.equal(successor.commands.admitsCommands(successor.context), true,
        "normal Running ACK releases the successor's exact phase hold");
      proof = true;
    } catch (error) { executorError = error; throw error; }
    throw new Error("fixture completed its proof");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    assert.equal(proof, true);
    assert.equal(f.reservations.length, 0);
  } finally { await f.memory.stop(); }
});

for (const verdict of ["empty", "capture_failure", "ambiguous", "rotated", "deadline"] as const) {
  test(`runner exhausted memory ${verdict}: retained owner hold keeps clone/session and never reports model failure`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    let delivered = false, prepared = false, captureAttempts = 0, captureAttemptsAtPark = -1;
    const captureScope = new AsyncLocalStorage<{ ownerCapture: boolean }>();
    const positiveCaptureBudgetMs = 5000;
    const deadlineBudgetMs = 200;
    const deadlineElapsedBoundMs = 1500;
    const transportRescueAfterMs = 2000;
    api.onState(f.claim.run_id, body => {
      if (body.recovery_cause === "worker_memory_pressure") captureAttemptsAtPark = captureAttempts;
    });
    const transportRescue = new AbortController();
    let rescueTimer: ReturnType<typeof setTimeout> | undefined;
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => {
        const operation = captureScope.getStore();
        assert.ok(operation, "owner settlement runs inside a bounded Git recovery operation");
        operation.ownerCapture = true;
        prepared = true;
        assert.equal(fs.existsSync(homeDir + "/session-proof"), true);
        return { kind: "observed_empty" };
      },
      run: async ctx => {
        const recoveryOperation = git.withRecoveryOperation.bind(git);
        git.withRecoveryOperation = (signal, deadline, action) => recoveryOperation(signal, deadline,
          ownedSignal => captureScope.run({ ownerCapture: false }, () => action(ownedSignal)));
        git.worktreeStatus = async () => verdict === "capture_failure" ? ["M fixture"] : [];
        // Concurrent settle transfers can also call commitWipMarker; count only the owned operation.
        git.commitWipMarker = async () => {
          if (captureScope.getStore()?.ownerCapture) captureAttempts++;
          return false;
        };
        const command = enroll(ctx);
        f.pressure(950);
        const result = await command.captured.commands.watchOutcome(command.entry);
        delivered = result.code === "worker_memory_pressure" && result.tool_id === "original-tool";
        ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
        if (verdict === "rotated") client.revokeMemoryAuthority();
        if (verdict === "deadline") {
          client.postMessages = async (_id, _messages, _generation, signal) => {
            const owned = AbortSignal.any([transportRescue.signal, ...(signal ? [signal] : [])]);
            await new Promise<void>((_resolve, reject) => {
              if (owned.aborted) reject(owned.reason);
              else owned.addEventListener("abort", () => reject(owned.reason), { once: true });
            });
          };
        }
        throw new Error("model abort is not a test failure");
      },
    };
    if (verdict === "empty" || verdict === "ambiguous") git.worktreeStatus = async () => [];
    if (verdict === "ambiguous") api.overrideStateStatus(f.claim.run_id, "running");
    try {
      // Rescue the unfixed transport so RED is an elapsed assertion, never the test timeout.
      if (verdict === "deadline") rescueTimer = setTimeout(() => transportRescue.abort(), transportRescueAfterMs);
      const started = performance.now();
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory,
          codexBoundaryDeadlineMs: verdict === "deadline" ? deadlineBudgetMs : positiveCaptureBudgetMs }).execute(f.claim);
      if (verdict === "deadline") assert.ok(performance.now() - started < deadlineElapsedBoundMs,
        "memory teardown must finish within 1500ms, well before the 120000ms test timeout");
      assert.equal(delivered, true, "matching pressure tool result delivered");
      assert.equal(fs.existsSync(homeDir + "/session-proof"), true, "memory preservation integration must keep original session");
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true, "memory preservation integration must keep original clone");
      assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false,
        "memory preservation integration must produce a quiet retained stop");
      assert.ok(f.reservations.length === 1);
      if (verdict === "empty" || verdict === "capture_failure") {
        const parks = api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure");
        assert.equal(parks.length, 1, "owner submits exactly one memory hold");
        assert.deepEqual(parks[0]!.body, {
          status: "recovery_wait", recovery_cause: "worker_memory_pressure",
          register_nonce: "original-memory-registration", claim_generation: 7, memory_episode: 3,
        }, "source-only hold must not claim a latest remote checkpoint");
        assert.equal(f.parkReceipts.length, 1);
        assert.deepEqual(f.parkReceipts[0]!.memoryParkAccepted, {
          run_id: f.claim.run_id, worker_id: client.memoryIncarnation!.worker_id,
          register_nonce: "original-memory-registration", claim_generation: 7, memory_episode: 3,
        }, "real client accepts the matching public worker DTO");
      }
      if (verdict === "capture_failure") {
        assert.equal(prepared, true, "owner capture settled separately from victim cancellation");
        assert.equal(captureAttemptsAtPark, 3, "owner capture attempts bounded to three before park submission");
      }
      if (verdict === "rotated" || verdict === "deadline")
        assert.equal(api.states.some(s => s.body.recovery_cause === "worker_memory_pressure"), false);
    } finally {
      if (rescueTimer) clearTimeout(rescueTimer);
      transportRescue.abort();
      await f.memory.stop(); await monitoring;
    }
  });
}

for (const verdict of ["accepted", "ambiguous", "generation", "deadline"] as const) {
  test(`runner initially exhausted allowance ${verdict}: first accessor stops before model admission and retains owner custody`, async () => {
    const f = await setup(2);
    api.setOwnershipStatus(f.claim.run_id, "claimed", 7);
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    let captures = 0, enrollments = 0, modelRequests = 0, proof = false;
    let started: number | undefined;
    let executorError: unknown;
    const begin = f.memory.begin.bind(f.memory);
    f.memory.begin = (...args) => {
      captures++;
      assert.equal(args[0].memory_intervention_count, 2);
      assert.deepEqual(args[0].memory_policy, { version: 1, max_interventions: 2 });
      const context = begin(...args);
      assert.equal(f.memory.commands.remaining(context), 0);
      return context;
    };
    const register = f.memory.commands.register.bind(f.memory.commands);
    f.memory.commands.register = (...args) => { enrollments++; return register(...args); };
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
      run: async ctx => {
        try {
          assert.equal(api.states.at(-1)!.body.status, "running",
            "normal claimed redispatch reports Running before trusted capture");
          // This fixture owns the separate ownership route projection explicitly.
          api.setOwnershipStatus(f.claim.run_id, "running", 7);
          if (verdict === "generation") api.setOwnershipStatus(f.claim.run_id, "running", 8);
          if (verdict === "deadline") client.postMessages = async (_id, _messages, _generation, signal) => {
            await new Promise<void>((_resolve, reject) => {
              assert.ok(signal, "owner transport carries its bounded operation signal");
              if (signal.aborted) reject(signal.reason);
              else signal.addEventListener("abort", () => reject(signal.reason), { once: true });
            });
          };
          let launch: ReturnType<NonNullable<RunContext["capturedMemory"]>>;
          started = performance.now();
          assert.throws(() => { launch = ctx.capturedMemory!(); }, /retained recovery/i);
          if (launch!) { enroll(ctx); modelRequests++; }
          assert.throws(() => ctx.capturedMemory!(), /retained recovery/i,
            "a repeated accessor cannot reset the exhausted allowance");
          proof = true;
        } catch (error) { executorError = error; throw error; }
        throw new Error("fixture stops original model execution");
      },
    };
    git.worktreeStatus = async () => [];
    if (verdict === "ambiguous") api.overrideStateStatus(f.claim.run_id, "running");
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: verdict === "deadline" ? 200 : 5000 }).execute(f.claim);
      if (executorError) throw executorError;
      assert.equal(proof, true, "actual RunRunner executor reached the first trusted accessor");
      assert.ok(started !== undefined, "elapsed clock anchored at the first trusted accessor");
      if (verdict === "deadline") assert.ok(performance.now() - started < 1500,
        "initial exhaustion shares the bounded owner deadline");
      assert.equal(captures, 1, "hold prevents a successor context and allowance reset");
      assert.equal(enrollments, 0, "no model command root exists to signal");
      assert.equal(modelRequests, 0);
      assert.equal(f.reservations.length, 0, "spent charges are neither reserved again nor refunded");
      assert.equal(fs.existsSync(homeDir + "/session-proof"), true);
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
      assert.equal(api.states.some(s => s.body.status === "failed" || s.body.status === "completed"), false);
      assert.equal(api.messages(f.claim.run_id).some(m => m.kind === "tool_result" ||
        m.payload.event === "worker_memory_pressure"), false,
        "allowance exhaustion must not invent pressure telemetry or a tool outcome");
      const parks = api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure");
      if (verdict === "accepted" || verdict === "ambiguous") {
        assert.equal(parks.length, 1);
        assert.deepEqual(parks[0]!.body, {
          status: "recovery_wait", recovery_cause: "worker_memory_pressure",
          register_nonce: "original-memory-registration", claim_generation: 7, memory_episode: 3,
        });
      } else assert.equal(parks.length, 0, "uncertain ownership or expired deadline cannot park");
      if (verdict === "accepted") {
        assert.deepEqual(f.parkReceipts[0]!.memoryParkAccepted, {
          run_id: f.claim.run_id, worker_id: client.memoryIncarnation!.worker_id,
          register_nonce: "original-memory-registration", claim_generation: 7, memory_episode: 3,
        }, "exact public owner ACK confirms the park");
      } else assert.equal(f.parkReceipts.some(ack => ack.memoryParkAccepted !== undefined), false);
    } finally { await f.memory.stop(); }
  });
}

test("memory owned flush joins a slow periodic ACK before confirming feedback", async () => {
  let ack!: () => void;
  let started!: () => void;
  const sending = new Promise<void>(resolve => { started = resolve; });
  const transport = {
    postMessages: async () => {
      started();
      await new Promise<void>(resolve => { ack = resolve; });
    },
  } as unknown as WorkerClient;
  const batcher = new MessageBatcher(transport, "memory-owned-ack", 0, 0, nullLogger());
  batcher.emit({ kind: "status", payload: { event: "worker_memory_pressure" } });
  // The periodic timer is unref'd, so keep the readiness wait alive and bounded.
  await Promise.race([sending, new Promise<never>((_, reject) => {
    const timer = setTimeout(() => reject(new Error("periodic send did not start")), 500);
    sending.finally(() => clearTimeout(timer));
  })]);
  assert.equal(batcher.bufferedCount(), 0, "the pending HTTP request already removed the buffer");
  let confirmed = false;
  const boundary = new AbortController();
  const owned = batcher.flushOwned(boundary.signal).then(result => { confirmed = result; return result; });
  try {
    await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(confirmed, false, "an empty buffer cannot confirm pending feedback");
    ack();
    assert.equal(await owned, true, "only the real periodic ACK confirms feedback");
  } finally {
    ack();
    boundary.abort();
    await owned;
    await batcher.close(boundary.signal);
  }
});

test("memory owned flush cancellation joins original settlement and preserves replay", async () => {
  let started!: () => void;
  let settle!: () => void;
  let cancelled = false, calls = 0;
  const sending = new Promise<void>(resolve => { started = resolve; });
  const transport = {
    postMessages: async (_id: string, _messages: unknown, _generation: unknown, signal?: AbortSignal) => {
      calls++;
      started();
      await new Promise<void>((_resolve, reject) => {
        signal!.addEventListener("abort", () => {
          cancelled = true;
          settle = () => reject(new Error("original send cancelled"));
        }, { once: true });
      });
    },
  } as unknown as WorkerClient;
  const batcher = new MessageBatcher(transport, "memory-owned-abort", 0, 0, nullLogger());
  batcher.emit({ kind: "status", payload: { event: "worker_memory_pressure" } });
  await Promise.race([sending, new Promise<never>((_, reject) => {
    const timer = setTimeout(() => reject(new Error("periodic send did not start")), 500);
    sending.finally(() => clearTimeout(timer));
  })]);
  const boundary = new AbortController();
  let completed = false;
  const owned = batcher.flushOwned(boundary.signal).then(result => { completed = true; return result; });
  boundary.abort();
  try {
    assert.equal(cancelled, true, "caller abort reaches the original request");
    await new Promise(resolve => setTimeout(resolve, 20));
    assert.equal(completed, false, "owned completion stays unsettled until the transport settles");
    settle();
    assert.equal(await owned, false);
    assert.equal(batcher.bufferedCount(), 1, "the original sequence remains buffered for replay");
    assert.equal(calls, 1, "aborted ownership must not launch another request");
  } finally {
    settle();
    await owned;
    await batcher.close(boundary.signal);
  }
});

function memoryOutbox(root: string, fail = false) {
  return new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 128 * 1024 * 1024, retentionMs: 86400000,
    rawWrite: async (write, context) => {
      if (fail && context.kind === "segment") throw new Error("fixture disk failure");
      await write();
    } });
}

function memoryOutboxRoot() {
  const scratch = path.resolve("../.uzi/scratch");
  fs.mkdirSync(scratch, { recursive: true });
  return fs.mkdtempSync(path.join(scratch, "memory-tail-"));
}

test("memory tail persists before spill threshold and reopens with exact seq/generation without ACK", async () => {
  const root = memoryOutboxRoot();
  const outbox = memoryOutbox(root);
  await outbox.init();
  let calls = 0;
  const transport = { postMessages: async () => { calls++; throw new Error("offline"); } } as unknown as WorkerClient;
  const batcher = new MessageBatcher(transport, "memory-tail", 40, 60000, nullLogger(), undefined, undefined,
    { outbox, generation: 7, transientTripMs: 30000 });
  const signal = new AbortController();
  try {
    batcher.emit({ kind: "status", payload: { event: "worker_memory_pressure", intervention_id: "original-id" } });
    batcher.emit({ kind: "tool_result", payload: { tool_id: "original-tool", attempt_id: "original-attempt" } });
    assert.equal(await batcher.flushOwned(signal.signal), false);
    assert.equal(batcher.isSpilled(), false);
    assert.equal(calls, 1);
    assert.equal(await batcher.preserveOwnedTail(signal.signal), true);
    assert.equal(await batcher.flushOwned(signal.signal), false, "durability is not an ACK");
    const reopened = memoryOutbox(root);
    await reopened.init();
    const replay: unknown[] = [];
    await reopened.drainRun("memory-tail", async (messages, generation) => { replay.push({ messages, generation }); });
    assert.deepEqual(replay, [{ generation: 7, messages: [
      { seq: 41, kind: "status", payload: { event: "worker_memory_pressure", intervention_id: "original-id" } },
      { seq: 42, kind: "tool_result", payload: { tool_id: "original-tool", attempt_id: "original-attempt" } },
    ] }]);
    assert.equal(calls, 1, "preservation does not retry network");
  } finally {
    signal.abort();
    await batcher.close(signal.signal);
    fs.rmSync(root, { recursive: true, force: true });
  }
});

test("memory tail cannot claim persistence or dispose an unsettled uncooperative original send", async () => {
  const root = memoryOutboxRoot();
  const outbox = memoryOutbox(root);
  await outbox.init();
  let started!: () => void, rejectSend!: (error: Error) => void;
  const sending = new Promise<void>(resolve => { started = resolve; });
  let originalSignal: AbortSignal | undefined;
  const transport = { postMessages: async (_id: string, _messages: unknown, _generation: unknown, signal?: AbortSignal) => {
    originalSignal = signal;
    started();
    await new Promise<void>((_resolve, reject) => { rejectSend = reject; });
  } } as unknown as WorkerClient;
  const batcher = new MessageBatcher(transport, "uncooperative-tail", 8, 0, nullLogger(), undefined, undefined, { outbox, generation: 7 });
  batcher.emit({ kind: "status", payload: { intervention_id: "original-id" } });
  await sending;
  const budget = new AbortController();
  let settled = false;
  const operation = batcher.preserveOwnedTail(budget.signal).then(value => { settled = true; return value; });
  budget.abort();
  try {
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(originalSignal?.aborted, true);
    assert.equal(settled, false);
    assert.equal(outbox.hasUndrainedMessages("uncooperative-tail"), false);
    rejectSend(new Error("late original failure"));
    assert.equal(await operation, false);
    assert.equal(batcher.bufferedCount(), 1, "original tail remains in local custody");
  } finally {
    rejectSend(new Error("fixture cleanup"));
    await operation;
    await batcher.close(budget.signal);
    fs.rmSync(root, { recursive: true, force: true });
  }
});

for (const store of ["missing", "disabled", "write_failure", "refused"] as const) {
  test(`memory tail ${store} returns false and retains exact local replay`, async () => {
    const root = memoryOutboxRoot();
    const outbox = store === "missing" ? undefined : memoryOutbox(store === "disabled" ? path.join(root, "blocked", "outbox") : root,
      store === "write_failure");
    if (store === "disabled") fs.writeFileSync(path.join(root, "blocked"), "not a directory");
    await outbox?.init();
    const transport = { postMessages: async () => { throw new Error("offline"); } } as unknown as WorkerClient;
    const batcher = new MessageBatcher(transport, store === "refused" ? "../invalid" : "failed-tail", 12, 60000,
      nullLogger(), undefined, undefined, { outbox, generation: 7 });
    const budget = new AbortController();
    try {
      batcher.emit({ kind: "status", payload: { intervention_id: "original-id" } });
      assert.equal(await batcher.preserveOwnedTail(budget.signal), false);
      assert.equal(batcher.bufferedCount(), 1);
    } finally {
      budget.abort();
      await batcher.close(budget.signal);
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
}

for (const fail of [false, true]) {
  test(`runner memory close preserves original feedback intent/custody with durable tail=${!fail}`, async () => {
    const root = memoryOutboxRoot();
    const outbox = memoryOutbox(root, fail);
    await outbox.init();
    const f = await setup(1);
    const monitoring = f.memory.run();
    let original: { workerMemory?: { feedback: Map<string, { operation?: Promise<boolean> }>;
      tailPreservation?: Promise<boolean>; tailDurable?: boolean; closing?: Promise<void> };
      retainedLocalCustody?: boolean; batcher: MessageBatcher } | undefined;
    const executor: Executor = { run: async ctx => {
      const command = enroll(ctx);
      original = command.captured.flight as typeof original;
      client.postMessages = async () => { throw new Error("offline before spill threshold"); };
      f.pressure(950);
      await command.captured.commands.watchOutcome(command.entry);
      // The feedback/capture signal may already be expired: cleanup gets its own budget.
      await original!.batcher.close(AbortSignal.abort(new Error("prior boundary expired")));
      throw new Error("quiet retained stop");
    } };
    try {
      await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, outbox, codexBoundaryDeadlineMs: 200 }).execute(f.claim);
      const memory = original!.workerMemory!;
      await memory.closing;
      assert.ok(memory.tailPreservation, "original operation stays explicitly owned");
      assert.equal(await memory.tailPreservation, !fail);
      assert.equal(memory.tailDurable, !fail);
      assert.equal(original!.retainedLocalCustody, true);
      assert.equal(memory.feedback.size, 1, "disk writes never delete ACK-only intent");
      assert.ok([...memory.feedback.values()][0]!.operation, "original feedback send promise stays owned");
      assert.equal(api.states.some(s => s.body.recovery_cause === "worker_memory_pressure"), false);
      if (!fail) {
        const reopened = memoryOutbox(root);
        await reopened.init();
        const feedback: { seq: number; generation: number; intervention_id: unknown }[] = [];
        await reopened.drainRun(f.claim.run_id, async (messages, generation) => {
          for (const message of messages) if (message.payload.event === "worker_memory_pressure")
            feedback.push({ seq: message.seq, generation, intervention_id: message.payload.intervention_id });
        });
        assert.equal(feedback.length, 1);
        assert.equal(feedback[0]!.generation, 7);
        assert.equal(feedback[0]!.intervention_id, [...memory.feedback.keys()][0]);
      } else assert.equal(outbox.hasUndrainedMessages(f.claim.run_id), false);
    } finally {
      await f.memory.stop(); await monitoring;
      fs.rmSync(root, { recursive: true, force: true });
    }
  });
}

test("M3 late native park ACK stays owned after deadline and cannot mutate a reclaimed successor", async () => {
  const f = await setup(1);
  const monitoring = f.memory.run();
  const release = releaseGate();
  const successorRelease = releaseGate();
  const originalReadiness = git.normalizeRetainedSourceObjects;
  let readiness = 0;
  let original: MemoryFlightProof | undefined;
  let successor: ReturnType<NonNullable<RunContext["capturedMemory"]>>;
  let nativeAck: StateAck | undefined, parkSignal: AbortSignal | undefined;
  let successorWork: Promise<void> | undefined;
  let started: number | undefined, rescue: ReturnType<typeof setTimeout> | undefined;
  const report = client.reportState.bind(client);
  client.reportState = async (...args) => {
    const ack = await report(...args);
    if (args[1].recovery_cause === "worker_memory_pressure") {
      nativeAck = ack;
      parkSignal = args[2];
      // Start rescue at the blocked port, leaving room for the original 200ms stop,
      // successor's 5000ms local capture and intervening fixture work. Finally releases and joins it.
      rescue = setTimeout(() => release.resolve(), 10000);
      // Intentionally ignore abort after a REAL client-validated receipt.
      await release.promise;
    }
    return ack;
  };
  fs.writeFileSync(homeDir + "/session-proof", "original session");
  git.worktreeStatus = async () => ["M fixture"];
  git.commitWipMarker = async () => false;
  const executor: Executor = {
    settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
    run: async ctx => {
      const command = enroll(ctx);
      original = command.captured.flight as MemoryFlightProof;
      started = performance.now();
      f.pressure(950);
      const result = await command.captured.commands.watchOutcome(command.entry);
      ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
      throw new Error("quiet retained stop");
    },
  };
  const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
    { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 200 });
  const work = runner.execute(f.claim);
  try {
    await until(() => nativeAck !== undefined);
    assert.ok(nativeAck!.memoryParkAccepted, "receipt derives from the native public memory-hold DTO");
    await work;
    assert.ok(started !== undefined, "elapsed clock anchored at the pressure trigger");
    assert.ok(performance.now() - started < 1500, "runner deadline does not wait for uncooperative park return");
    const operation = original!.workerMemory!.operation!;
    assert.ok(operation, "original preservation promise remains on its flight");
    assert.notEqual(original!.workerMemory!.settled, true, "original work has not settled");
    assert.equal(parkSignal!.aborted, true);
    assert.notEqual(original!.parked, true, "native acceptance alone cannot mark an expired flight parked");
    assert.equal(original!.retainedLocalCustody, true);
    assert.equal(fs.readFileSync(homeDir + "/session-proof", "utf8"), "original session");
    assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
    assert.equal(api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure").length, 1);
    assert.equal(api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure").length, 1);

    f.pressure(100);
    git.worktreeStatus = async () => [];
    api.memoryRegisterNonce = "successor-memory-registration";
    await client.register("memory-fixture", "base");
    const next = { ...f.claim, claim_generation: 8, memory_episode: 4, memory_intervention_count: 1 };
    api.setOwnershipStatus(next.run_id, "running", 8);
    // Host-only successor models the Codex sandbox profile with readiness already satisfied.
    git.normalizeRetainedSourceObjects = async () => { readiness++; };
    const nextExecutor: Executor = { sandboxesCommands: true, run: async ctx => {
      assert.equal(readiness, 1, "retained readiness completed before successor admission");
      successor = ctx.capturedMemory!();
      await successorRelease.promise;
      throw new Error("successor proof complete");
    } };
    const successorLog: string[] = [];
    const log = nullLogger();
    log.child = () => log;
    log.warn = log.error = (message, fields) => { successorLog.push(message + JSON.stringify(fields)); };
    successorWork = runnerWith(() => ({ executor: nextExecutor }), fakeGitlab().gitlab, undefined, log,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000 }).execute(next);
    try {
      await until(() => successor !== undefined, 5000);
    } catch {
      assert.fail("successor executor admission: " + JSON.stringify(api.states) + successorLog.join("\\n"));
    }
    assert.equal(readiness, 1, "reclaim normalized the retained source once");
    assert.notEqual(successor!.flight, original);
    assert.equal(successor!.context.claim_generation, 8);
    assert.equal(successor!.context.memory_episode, 4);
    assert.equal(successor!.context.register_nonce, "successor-memory-registration");
    assert.equal(successor!.context.flight, successor!.flight);
    assert.equal(successor!.commands.remaining(successor!.context), 1, "reclaim uses persisted count, never resets allowance");
    assert.equal(successor!.commands.admitsCommands(successor!.context), true);
    const nextFlight = successor!.flight as MemoryFlightProof;
    assert.notEqual(original!.workerMemory!.settled, true, "controlled release, not rescue, settles old park");
    const nextParked = nextFlight.parked;
    release.resolve();
    await operation;
    assert.equal(original!.workerMemory!.operation, operation, "late completion settles the same original promise");
    assert.equal(original!.workerMemory!.settled, true);
    assert.notEqual(original!.parked, true);
    assert.equal(nextFlight.parked, nextParked, "old acceptance cannot mark successor parked");
    assert.equal(nextFlight.workerMemory!.operation, undefined);
    assert.equal(successor!.commands.admitsCommands(successor!.context), true);
    assert.equal(successor!.commands.remaining(successor!.context), 1);
  } finally {
    clearTimeout(rescue);
    release.resolve();
    successorRelease.resolve();
    await work;
    await original?.workerMemory?.operation;
    await original?.workerMemory?.closing;
    await successorWork;
    git.normalizeRetainedSourceObjects = originalReadiness;
    await f.memory.stop(); await monitoring;
    await Promise.allSettled(f.memory.sinkReceipts.map(receipt => receipt.completion));
  }
});

for (const interruption of ["registration", "generation", "shutdown", "cancel"] as const) {
  test(`M3 capture interruption ${interruption} keeps original custody and prevents stale mutations`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    const captureScope = new AsyncLocalStorage<boolean>();
    let original: MemoryFlightProof | undefined;
    let reads = 0, writes = 0, fetches = 0;
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
      run: async ctx => {
        const recover = git.withRecoveryOperation.bind(git);
        git.withRecoveryOperation = (signal, deadline, action) =>
          recover(signal, deadline, owned => captureScope.run(true, () => action(owned)));
        const command = enroll(ctx);
        original = command.captured.flight as MemoryFlightProof;
        f.pressure(950);
        const result = await command.captured.commands.watchOutcome(command.entry);
        ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
        throw new Error("quiet retained stop");
      },
    };
    const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000 });
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    git.worktreeStatus = async () => {
      if (!captureScope.getStore()) return [];
      reads++;
      if (interruption === "registration") client.revokeMemoryAuthority();
      if (interruption === "generation") api.setOwnershipStatus(f.claim.run_id, "running", 8);
      if (interruption === "shutdown") runner.shutdown();
      if (interruption === "cancel") {
        api.setInputs(f.claim.run_id, [input("cancel")]);
        await until(() => original!.steering.terminalLifecycleSignal().aborted);
      }
      return ["M fixture"];
    };
    const commit = git.commitWipMarker.bind(git);
    git.commitWipMarker = async (...args) => {
      if (captureScope.getStore()) { writes++; return false; }
      return commit(...args);
    };
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = async (...args) => {
      if (captureScope.getStore()) fetches++;
      return fetch(...args);
    };
    try {
      await runner.execute(f.claim);
      await original?.workerMemory?.operation;
      assert.equal(reads, 1, "interruption occurs inside actual owner capture after quiescence");
      assert.equal(writes, 0, "ownership/lifecycle recheck prevents the next WIP write");
      assert.equal(fetches, 0, "no stale tracking fetch follows interruption");
      assert.equal(api.states.some(s => s.body.recovery_cause === "worker_memory_pressure"), false);
      assert.notEqual(original!.parked, true);
      assert.equal(original!.retainedLocalCustody, true);
      assert.equal(fs.readFileSync(homeDir + "/session-proof", "utf8"), "original session");
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
      assert.equal(original!.workerMemory!.feedback.size, 0, "original feedback was actually ACKed");
      assert.equal(api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure").length, 1);
      assert.equal(f.memory.sinkReceipts.some(receipt => receipt.context.flight === original), false,
        "original sink receipts are settled, never transferred to a run-ID successor");
    } finally {
      await original?.workerMemory?.operation;
      await original?.workerMemory?.closing;
      await f.memory.stop(); await monitoring;
      await Promise.allSettled(f.memory.sinkReceipts.map(receipt => receipt.completion));
    }
  });
}

test("M3 delayed feedback beyond deadline retains original send and sink receipt without parking", async () => {
  const f = await setup(1);
  const monitoring = f.memory.run();
  const release = releaseGate();
  let original: MemoryFlightProof | undefined, sending = false, sendSignal: AbortSignal | undefined;
  let started: number | undefined, rescue: ReturnType<typeof setTimeout> | undefined;
  const post = client.postMessages.bind(client);
  client.postMessages = async (...args) => {
    await post(...args);
    if (args[1].some(message => message.payload.event === "worker_memory_pressure")) {
      sending = true;
      sendSignal = args[3];
      rescue = setTimeout(() => release.resolve(), 2000);
      await release.promise; // Native feed ACK is withheld by an intentionally uncooperative adapter.
    }
  };
  fs.writeFileSync(homeDir + "/session-proof", "original session");
  const executor: Executor = { run: async ctx => {
    const command = enroll(ctx);
    original = command.captured.flight as MemoryFlightProof;
    started = performance.now();
    f.pressure(950);
    await command.captured.commands.watchOutcome(command.entry);
    throw new Error("quiet retained stop");
  } };
  const work = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
    { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 200 }).execute(f.claim);
  try {
    await until(() => sending);
    const receipt = f.memory.sinkReceipts.find(r => r.kind === "feedback" && r.context.flight === original);
    assert.ok(receipt, "original feedback sink owns its completion receipt");
    const pending = [...original!.workerMemory!.feedback.values()][0]!;
    const operation = pending.operation!;
    assert.ok(operation);
    let settled = false;
    void operation.then(() => { settled = true; });
    await work;
    assert.ok(started !== undefined, "elapsed clock anchored at the pressure trigger");
    assert.ok(performance.now() - started < 1500, "uncooperative feedback does not defeat owner deadline");
    assert.equal(settled, false);
    assert.equal(sendSignal!.aborted, true);
    assert.ok(f.memory.sinkReceipts.includes(receipt), "timeout does not discard uncompleted sink receipt");
    assert.equal(original!.retainedLocalCustody, true);
    assert.equal(fs.readFileSync(homeDir + "/session-proof", "utf8"), "original session");
    assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
    assert.equal(api.states.some(s => s.body.recovery_cause === "worker_memory_pressure"), false);
    release.resolve();
    await operation;
    await receipt.completion;
    assert.equal(pending.operation, operation, "late ACK settles the original send");
    assert.equal(original!.workerMemory!.feedback.size, 1, "late ACK cannot erase intent after delivery authority expired");
    assert.equal(f.memory.sinkReceipts.includes(receipt), false, "only settlement retires the receipt");
    assert.notEqual(original!.parked, true);
    assert.equal(api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure").length, 1);
  } finally {
    clearTimeout(rescue); release.resolve();
    await work;
    await original?.workerMemory?.operation;
    await original?.workerMemory?.closing;
    await f.memory.stop(); await monitoring;
    await Promise.allSettled(f.memory.sinkReceipts.map(receipt => receipt.completion));
  }
});

test("M3 late mocked historical outcome resolves only the original ticket after expired wait", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  const release = releaseGate();
  let reportWork: Promise<unknown> | undefined, reportSignal: AbortSignal | undefined;
  let proof = false, executorError: unknown;
  const report = client.reportMemoryInterventionOutcome.bind(client);
  client.reportMemoryInterventionOutcome = (...args) => {
    reportSignal ??= args[1];
    reportWork = (async () => {
      const ack = await report(...args);
      await release.promise; // Deliberately ignore abort; this exact promise is joined below.
      return ack;
    })();
    return reportWork as ReturnType<typeof report>;
  };
  const rescue = setTimeout(() => release.resolve(), 2000);
  const executor: Executor = { run: async ctx => {
    try {
      const command = enroll(ctx);
      f.pressure(950);
      const result = await command.captured.commands.watchOutcome(command.entry);
      assert.equal(result.outcome, "confirmed_drained");
      await until(() => reportSignal !== undefined);
      await until(() => reportSignal!.aborted);
      assert.equal(command.captured.commands.pressureHeld(command.captured.context), true);
      assert.equal(command.captured.commands.admitsCommands(command.captured.context), false,
        "expired wait alone supplies no authority");
      assert.equal(f.reservations.length, 1);
      release.resolve(); await reportWork;
      f.pressure(100);
      await ctx.reportIteration!(1);
      await new Promise(resolve => setTimeout(resolve, 30)); // Three configured rearm windows, after the late ACK.
      assert.equal(command.captured.commands.pressureHeld(command.captured.context), false,
        "mocked exact historical proof plus fresh sustained Q resolves the original ticket");
      assert.equal(command.captured.commands.admitsCommands(command.captured.context), true);
      assert.equal(command.captured.commands.remaining(command.captured.context), 1);
      assert.equal(f.reservations.length, 1, "deferred outcome never spends another intervention");
      proof = true;
    } catch (error) { executorError = error; throw error; }
    finally { release.resolve(); await reportWork; }
    throw new Error("deferred outcome proof complete");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    assert.equal(proof, true);
  } finally {
    clearTimeout(rescue); release.resolve(); await reportWork;
    await f.memory.stop(); await monitoring;
  }
});

test("M3 reordered Running ACK cannot release the input phase token or clear pressure", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  const release = releaseGate();
  let oldAck: StateAck | undefined, executorError: unknown, proof = false;
  let oldWork: Promise<unknown> | undefined, inputWork: Promise<unknown> | undefined;
  const rescue = setTimeout(() => {
    release.resolve();
    api.setInputs(f.claim.run_id, [input("cancel")]);
  }, 2000);
  const executor: Executor = { run: async ctx => {
    try {
      const command = enroll(ctx);
      f.pressure(950);
      await command.captured.commands.watchOutcome(command.entry);
      assert.equal(command.captured.commands.pressureHeld(command.captured.context), true);
      const report = client.reportState.bind(client);
      let delay = true;
      client.reportState = async (...args) => {
        const ack = await report(...args);
        if (delay && args[1].status === "running") {
          delay = false; oldAck = ack;
          await release.promise;
        }
        return ack;
      };
      oldWork = Promise.resolve(ctx.reportIteration!(1));
      await until(() => oldAck !== undefined);
      assert.equal(oldAck!.status, "running");
      inputWork = ctx.askUser!([{ question: "Proceed?", header: "Choice" }]);
      await until(() => api.states.at(-1)?.body.status === "awaiting_input");
      const waiting = ctx.capturedMemory!()!;
      assert.equal(waiting.context, command.captured.context, "pressure-held lifecycle keeps its original context");
      assert.equal(waiting.commands.phaseAdmits(waiting.context), false);
      release.resolve(); await oldWork;
      assert.equal(waiting.commands.phaseAdmits(waiting.context), false, "reordered Running ACK cannot release current input hold");
      assert.equal(waiting.commands.pressureHeld(waiting.context), true, "Running ACK cannot clear pressure latch");
      assert.equal(waiting.commands.admitsCommands(waiting.context), false);
      const question = api.states.findLast(s => s.body.status === "awaiting_input")!.body.open_question_id;
      api.setInputs(f.claim.run_id, [input("answer", JSON.stringify({ question_id: question, answers: "proceed" }))]);
      await inputWork;
      const resumed = ctx.capturedMemory!()!;
      assert.equal(resumed.context, waiting.context);
      assert.equal(resumed.commands.phaseAdmits(resumed.context), true, "matching input resume ACK releases its own phase");
      assert.equal(resumed.commands.pressureHeld(resumed.context), true);
      assert.equal(resumed.commands.admitsCommands(resumed.context), false);
      assert.equal(resumed.commands.remaining(resumed.context), 1);
      f.pressure(100);
      await until(() => resumed.commands.admitsCommands(resumed.context));
      assert.equal(f.reservations.length, 1);
      proof = true;
    } catch (error) { executorError = error; throw error; }
    finally { release.resolve(); await oldWork; await inputWork?.catch(() => {}); }
    throw new Error("lifecycle proof complete");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    assert.equal(proof, true);
  } finally {
    clearTimeout(rescue); release.resolve();
    await f.memory.stop(); await monitoring;
  }
});

test("runtime freezes and validates configured policy; first null claim seeds it while persisted policy/count win", async () => {
  const configured = { ...settings, maxInterventions: 4 };
  const f = await setup(0, configured);
  const memory = f.memory;
  configured.maxInterventions = 9;
  assert.deepEqual(memory.configuredMemoryPolicy, { version: 1, max_interventions: 4 });
  assert.equal(Object.isFrozen(memory.configuredMemoryPolicy), true);
  for (const maxInterventions of [0, 10000, 1.5, NaN])
    assert.throws(() => new WorkerMemoryRuntime({ ...settings, maxInterventions }, client, nullLogger()), /invalid memory guard settings/);
  let calls = 0, executorError: unknown;
  const executor: Executor = { run: async ctx => {
    try {
      const captured = ctx.capturedMemory!()!;
      assert.deepEqual(captured.context.memory_policy, {
        version: 1, max_interventions: calls === 0 ? 4 : 2,
      });
      assert.equal(captured.commands.remaining(captured.context), calls === 0 ? 4 : 1);
      calls++;
    } catch (error) { executorError = error; throw error; }
    throw new Error("fixture end");
  } };
  try {
    const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: memory });
    await r.execute({ ...f.claim, memory_policy: null });
    if (executorError) throw executorError;
    assert.equal(calls, 1, "first null-policy claim enters the actual runner");
    await r.execute({ ...f.claim, memory_intervention_count: 1 });
    if (executorError) throw executorError;
    assert.equal(calls, 2, "persisted allowance remains authoritative");
  } finally { await memory.stop(); }
});

test("runner fails closed on incomplete enabled Codex claims; Claude has no accessor", async () => {
  const f = await setup();
  let calls = 0;
  const executor: Executor = { run: async ctx => {
    calls++;
    assert.equal(ctx.capturedMemory, undefined);
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    for (const memory_policy of [undefined, { version: 1 as const, max_interventions: 0 }]) {
      await r.execute({ ...f.claim, memory_policy });
      assert.equal(calls, 0, "invalid claim must stop before executor admission");
    }
    const claude = { ...f.claim, secrets: { forge_pat: f.claim.secrets.forge_pat } };
    await r.execute(claude);
    assert.equal(calls, 1);
  } finally { await f.memory.stop(); }
});


for (const phase of ["drain", "feedback", "preserve"] as const) {
  test(`runner feature shrink after signal during ${phase} keeps original result and source custody`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    const release = releaseGate();
    let reached = false, signals = 0;
    let original: MemoryFlightProof | undefined;
    let result: import("../src/protocol.js").WorkerMemoryPressureResult | undefined;
    let command: ReturnType<typeof enroll> | undefined;
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    const post = client.postMessages.bind(client);
    client.postMessages = async (...args) => {
      if (phase === "feedback" && args[1].some(message => message.payload.event === "worker_memory_pressure")) {
        reached = true; await release.promise;
      }
      return post(...args);
    };
    const recover = git.withRecoveryOperation.bind(git);
    git.withRecoveryOperation = async (signal, deadline, action) => {
      if (phase === "preserve" && signals === 1 && result) { reached = true; await release.promise; }
      return recover(signal, deadline, action);
    };
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
      run: async ctx => {
        const captured = ctx.capturedMemory!()!;
        original = captured.flight as MemoryFlightProof;
        const entry = captured.commands.register(captured.context, {
          source: "codex-model-command", toolId: "original-tool", attemptId: "original-attempt",
          command: "inert fixture", root: { pid: 10, startTime: "10" },
          observe: async () => {
            const now = performance.now();
            return { available: true, complete: true, startedAtMs: now, completedAtMs: now,
              rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
          },
          cancel: async authority => { assert.equal(authority.authorized(), true); signals++; },
          drain: async () => {
            if (phase === "drain") { reached = true; await release.promise; }
            return { confirmed: true };
          },
        });
        command = { captured, entry, cancellations: () => signals };
        f.pressure(950);
        result = await captured.commands.watchOutcome(entry);
        ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
        throw new Error("quiet retained stop");
      },
    };
    const work = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    try {
      await until(() => reached);
      if (phase === "preserve")
        await until(() => f.memory.sinkReceipts.some(r => r.context === command!.captured.context && r.kind === "preserve"));
      assert.equal(signals, 1);
      // Exercise the public feature-cache writer; heartbeat shrink uses the same replacement.
      client.protocolFeatures = ["claim_generation_fence"];
      assert.equal(client.memoryIncarnation, undefined);
      assert.equal(command!.captured.commands.isCurrent(command!.captured.context), false);
      if (phase === "feedback" || phase === "preserve")
        assert.ok(f.memory.sinkReceipts.some(r => r.context === command!.captured.context && r.kind === phase));
      release.resolve();
      await work;
      assert.ok(result);
      assert.equal(result.code, "worker_memory_pressure");
      assert.equal(result.command_id, command!.entry.commandId);
      assert.equal(result.tool_id, "original-tool");
      assert.equal(result.attempt_id, "original-attempt");
      assert.equal(result.claim_generation, 7); assert.equal(result.memory_episode, 3);
      assert.deepEqual(await command!.captured.commands.watchOutcome(command!.entry), result);
      assert.equal(original!.retainedLocalCustody, true);
      assert.notEqual(original!.parked, true);
      assert.equal(fs.readFileSync(homeDir + "/session-proof", "utf8"), "original session");
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
      assert.equal(f.parkReceipts.length, 0, "feature removal cannot fabricate park acceptance");
      client.protocolFeatures = ["worker_memory_pressure_v1", "claim_generation_fence"];
      assert.equal(client.memoryIncarnation, undefined);
      assert.equal(command!.captured.commands.admitsCommands(command!.captured.context), false);
      assert.equal(signals, 1); assert.equal(f.reservations.length, 1);
    } finally {
      release.resolve(); await work; await f.memory.stop(); await monitoring;
    }
  });
}

for (const mode of ["now", "milestone"] as const) for (const originalFailure of ["pause-signal", "memory-abort"] as const) {
  test(`memory preservation yields an owner pause after original work settles (${mode}, ${originalFailure})`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    let context: RunContext | undefined;
    let captured: MemoryFlightProof | undefined;
    let executorError: unknown;
    let pauseSent = false;
    fs.writeFileSync(homeDir + "/pause-session-proof", "retained session");
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => {
        if (!pauseSent) {
          pauseSent = true;
          api.appendInputs(f.claim.run_id, [input("pause", mode)]);
          await until(() => context?.pauseModeRequested?.() === mode);
        }
        return { kind: "observed_empty" };
      },
      run: async ctx => {
        context = ctx;
        try {
          const command = enroll(ctx);
          captured = command.captured.context.flight as unknown as MemoryFlightProof;
          f.pressure(950);
          await command.captured.commands.watchOutcome(command.entry);
          await until(() => captured?.workerMemory?.settled === true);
          assert.equal(ctx.pauseModeRequested?.(), mode);
          assert.equal(ctx.signal?.aborted, true);
          if (originalFailure === "memory-abort") {
            assert.equal(ctx.signal!.reason instanceof PauseNowSignal, false,
              "the memory operation aborted first; pause must not depend on the first error class");
          }
        } catch (error) { executorError = error; throw error; }
        throw originalFailure === "pause-signal" ? new PauseNowSignal() : ctx.signal!.reason;
      },
    };
    try {
      await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000 }).execute(f.claim);
      if (executorError) throw executorError;
      assert.equal(pauseSent, true);
      assert.equal(captured?.workerMemory?.settled, true);
      const disposition = api.states.filter(s => s.body.status === "paused" || s.body.status === "pause_failed");
      assert.equal(disposition.length, 1, "the existing owner-pause path must report exactly one disposition");
      assert.equal(f.parkReceipts.length, 0, "owner pause preempts memory parking");
      assert.equal(api.states.some(s => s.body.status === "failed"), false);
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
      assert.equal(fs.readFileSync(homeDir + "/pause-session-proof", "utf8"), "retained session");
      assert.equal(captured?.retainedLocalCustody, true);
    } finally { await f.memory.stop(); await monitoring; }
  });
}

for (const scenario of ["wall", "disk", "withdrawn", "cancel", "shutdown", "claim-fence", "credential-switch", "registration", "unsettled", "expired", "already-parked"] as const) {
  test(`memory pause handoff retains custody without an owner-pause disposition (${scenario})`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    const release = releaseGate();
    let context: RunContext | undefined, captured: MemoryFlightProof | undefined;
    let executorError: unknown, entered = false;
    let runner: ReturnType<typeof runnerWith>;
    fs.writeFileSync(homeDir + "/pause-session-proof", "retained session");
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => {
        entered = true;
        if (scenario !== "already-parked") {
          api.appendInputs(f.claim.run_id, [input("pause", scenario === "wall" ? "wall" : "now")]);
          await until(() => context?.pauseModeRequested?.() === (scenario === "wall" ? "wall" : "now"));
          if (scenario === "disk") captured!.steering.requestDiskStop();
          if (scenario === "withdrawn") {
            api.appendInputs(f.claim.run_id, [input("pause_cancel")]);
            await until(() => context?.pauseModeRequested?.() === null);
          }
          if (scenario === "cancel") {
            api.appendInputs(f.claim.run_id, [input("cancel")]);
            await until(() => captured!.steering.isCancelled());
          }
          if (scenario === "shutdown") runner.shutdown();
          if (scenario === "claim-fence") {
            api.setInputClaimGeneration(f.claim.run_id, 8);
            api.appendInputs(f.claim.run_id, [input("pause", "now")]);
            await until(() => captured!.steering.claimFence() !== undefined);
          }
          if (scenario === "credential-switch") captured!.steering.tripCredentialSwitch(7);
          if (scenario === "registration") client.revokeMemoryAuthority();
          if (scenario === "unsettled") await release.promise; // Deliberately ignore cancellation.
          if (scenario === "expired") await new Promise(resolve => setTimeout(resolve, 250));
        }
        return { kind: "observed_empty" };
      },
      run: async ctx => {
        context = ctx;
        try {
          if (scenario === "already-parked") {
            git.worktreeStatus = async () => [];
            git.commitWipMarker = async () => false;
          }
          const command = enroll(ctx);
          captured = command.captured.context.flight as unknown as MemoryFlightProof;
          f.pressure(950);
          await command.captured.commands.watchOutcome(command.entry);
          await until(() => entered);
          if (scenario === "unsettled") {
            assert.notEqual(captured.workerMemory?.settled, true);
          } else {
            await until(() => captured?.workerMemory?.settled === true);
          }
          if (scenario === "already-parked") {
            assert.equal(captured.parked, true, "the original memory park must be positively accepted first");
            assert.equal(f.parkReceipts.length, 1);
            captured.steering.seedPauseRequested("now");
          }
        } catch (error) { executorError = error; throw error; }
        throw ctx.signal?.reason ?? new PauseNowSignal();
      },
    };
    runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: scenario === "unsettled" || scenario === "expired" ? 200 : 5000 });
    try {
      await runner.execute(f.claim);
      if (executorError) throw executorError;
      const disposition = api.states.filter(s => s.body.status === "paused" || s.body.status === "pause_failed");
      assert.equal(disposition.length, 0, "unsettled work or a different lifecycle outcome cannot authorize owner pause");
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
      assert.equal(fs.readFileSync(homeDir + "/pause-session-proof", "utf8"), "retained session");
      assert.equal(captured?.retainedLocalCustody, true);
    } finally {
      release.resolve();
      await captured?.workerMemory?.operation;
      await f.memory.stop(); await monitoring;
    }
  });
}

for (const rejection of ["generation", "non-running", "released", "not-owned"] as const) {
  test(`memory pause cannot revive ownership rejected before the pause (${rejection})`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    let captured: MemoryFlightProof | undefined, executorError: unknown;
    let rejectedProbe = false, laterMutations = 0;
    const probe = client.getRunOwnership.bind(client);
    const marker = git.commitWipMarker.bind(git);
    const fetch = git.fetchAgentBranch.bind(git);
    const publish = client.publishCheckpoint.bind(client);
    const executor: Executor = {
      settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" }),
      run: async ctx => {
        try {
          const command = enroll(ctx);
          captured = command.captured.context.flight as unknown as MemoryFlightProof;
          client.getRunOwnership = async (...args: Parameters<typeof probe>) => {
            if (!rejectedProbe) {
              if (rejection === "not-owned") api.setOwnershipNotOwned(f.claim.run_id);
              else api.setOwnershipStatus(f.claim.run_id, rejection === "non-running" ? "queued" : "running",
                rejection === "generation" ? 8 : 7);
              rejectedProbe = true;
            }
            const own = await probe(...args);
            return rejection === "released" ? { ...own, claim_released: true } : own;
          };
          git.commitWipMarker = async (...args) => {
            if (rejectedProbe) laterMutations++;
            return marker(...args);
          };
          git.fetchAgentBranch = async (...args) => {
            if (rejectedProbe) laterMutations++;
            return fetch(...args);
          };
          client.publishCheckpoint = async (...args) => {
            if (rejectedProbe) laterMutations++;
            return publish(...args);
          };
          f.pressure(950);
          await command.captured.commands.watchOutcome(command.entry);
          await until(() => captured?.workerMemory?.settled === true);
          assert.equal(rejectedProbe, true);
          api.appendInputs(f.claim.run_id, [input("pause", "now")]);
          await until(() => ctx.pauseModeRequested?.() === "now");
          assert.equal(captured.steering.claimFence(), undefined,
            "this is an ownership-response rejection, not an input receipt fence");
          assert.equal(ctx.signal!.reason instanceof PauseNowSignal, false);
        } catch (error) { executorError = error; throw error; }
        throw ctx.signal!.reason;
      },
    };
    try {
      await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000 }).execute(f.claim);
      if (executorError) throw executorError;
      assert.equal(api.messages(f.claim.run_id).some(message => message.kind === "paused"), false,
        "a later pause cannot authorize a new sink after observed ownership rejection");
      assert.equal(laterMutations, 0, "no WIP, fetch or publish may start after the rejected ownership probe");
      assert.equal(captured?.retainedLocalCustody, true);
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
    } finally { await f.memory.stop(); await monitoring; }
  });
}
