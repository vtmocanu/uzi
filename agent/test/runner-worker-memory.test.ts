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
import { MessageBatcher } from "../src/batcher.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, homeDir, fakeGitlab, gitlabClaim, input, installHarness,
  runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function until(predicate: () => boolean) {
  const deadline = Date.now() + 3000;
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
    const started = performance.now();
    try {
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: verdict === "deadline" ? 200 : 5000 }).execute(f.claim);
      if (executorError) throw executorError;
      assert.equal(proof, true, "actual RunRunner executor reached the first trusted accessor");
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
