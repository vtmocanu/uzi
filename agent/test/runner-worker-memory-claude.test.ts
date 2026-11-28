import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { describe, it, test } from "node:test";
import { performance } from "node:perf_hooks";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, StateAck, StateRequest } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { nullLogger } from "./helpers.js";
import { SecretSet } from "../src/redact.js";
import { CredentialSwitchSignal, SteeringChannel } from "../src/steering.js";
import { CredentialSwitchOpLedger } from "../src/credential-switch-op-ledger.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { RunRunner } from "../src/runner.js";
import type { QuiesceRunOutcome } from "../src/run-quiescence.js";
import { assistant, resultOk, api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";

installHarness();

// Issue #2471 M3: a guard-bound runner enrols Claude claims (no Codex secret) too.

for (const outcome of ["false", "throw", "confirmed"] as const) {
  test(`capture policy: persisted Claude memory exhaustion / ${outcome}`, { timeout: 60_000 }, async t => {
    const mask = process.umask(0o022);
    t.after(() => { process.umask(mask); });
    const f = await setup(30_000);
    f.claim.memory_intervention_count = 2;
    let entries = 0, settlements = 0, preservationSettlements = 0, captures = 0, publications = 0;
    let flight: { sinkGate: { isHeld(): boolean };
      workerMemory?: { operation?: Promise<void>; settled?: boolean } } | undefined;
    const begin = f.memory.begin.bind(f.memory);
    t.mock.method(f.memory, "begin", (...args: Parameters<typeof begin>) => {
      const context = begin(...args);
      flight = context.flight as typeof flight;
      return context;
    });
    let clone = "", accessorReached = false;
    let callbackFailure: unknown;
    const session = path.join(homeDir, f.claim.run_id);
    const marker = git.commitWipMarker.bind(git);
    t.mock.method(git, "commitWipMarker", async (...args: Parameters<typeof marker>) => {
      captures++; return marker(...args);
    });
    const publish = client.publishCheckpoint.bind(client);
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof publish>) => {
      publications++; return publish(...args);
    });
    const executor: Executor = {
      run: async ctx => {
        entries++;
        clone = ctx.worktreePath;
        fs.mkdirSync(session, { recursive: true, mode: 0o700 });
        fs.writeFileSync(path.join(session, "session"), "original session", { mode: 0o600 });
        fs.writeFileSync(path.join(clone, "ONLY_COPY.txt"), "memory work");
        api.setOwnershipStatus(f.claim.run_id, "running", 7);
        try {
          assert.equal(typeof ctx.capturedMemory, "function");
          assert.throws(() => ctx.capturedMemory!(), /retained recovery/i);
          accessorReached = true;
        } catch (error) { callbackFailure = error; throw error; }
        throw new Error("original execution stopped after exhausted accessor");
      },
      settleWorkerCommands: async () => {
        if (!accessorReached) return { confirmed: true };
        settlements++;
        if (flight?.workerMemory?.operation && !flight.workerMemory.settled && flight.sinkGate.isHeld()) {
          preservationSettlements++;
        }
        if (outcome === "throw") throw new Error("memory command protocol failure");
        return { confirmed: outcome === "confirmed" };
      },
    };
    const runner = runnerWith(() => ({ executor, homeDir: session }), fakeGitlab().gitlab,
      undefined, nullLogger(), { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 30_000, checkpointIntervalMs: 0 });
    try {
      assert.equal(f.claim.secrets.codex, undefined);
      await runner.execute(f.claim);
      if (callbackFailure) throw callbackFailure;
      assert.equal(entries, 1);
      assert.equal(accessorReached, true, "real persisted-exhaustion accessor created the preservation hold");
      assert.ok(settlements > 0);
      assert.ok(preservationSettlements > 0, "the pending preservation operation reached settlement inside the real sink gate");
      assert.equal(api.states.some(s => ["failed", "completed"].includes(s.body.status)), false);
      const parks = api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure");
      if (outcome === "confirmed") {
        assert.ok(captures > 0 && publications > 0, "confirmed control reaches preservation capture/publication");
        assert.equal(parks.length, 1);
      } else {
        assert.deepEqual({ captures, publications }, { captures: 0, publications: 0 });
        assert.equal(parks.length, 0);
        assert.equal(api.states.some(s => s.body.status === "recovery_wait"), false);
        assert.equal(fs.readFileSync(path.join(clone, "ONLY_COPY.txt"), "utf8"), "memory work");
        assert.equal(fs.readFileSync(path.join(session, "session"), "utf8"), "original session");
      }
    } finally { runner.shutdown(); await f.memory.stop(); process.umask(mask); }
  });
}

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function setup(responseBudgetMs = settings.responseBudgetMs) {
  api.memoryRegisterNonce = "claude-memory-registration";
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence"]);
  await client.register("memory-claude-fixture", "base");
  // A Claude claim: the Codex secret is absent, the memory fields are the guard's.
  const claim: ClaimResponse = { ...gitlabClaim(2471), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: 0 };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  const memory = new WorkerMemoryRuntime({ ...settings, responseBudgetMs }, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: 1, startedAtMs: time, completedAtMs: time,
        currentBytes: 0, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { memory, claim };
}

test("a Claude claim on a guard-bound runner is enrolled; a missing memory field is refused before the executor", async () => {
  const f = await setup();
  let calls = 0;
  let accessor: unknown, secrets: unknown, subscribe: unknown;
  const executor: Executor = { run: async ctx => {
    calls++;
    accessor = ctx.capturedMemory;
    secrets = ctx.guardedCommandSecrets;
    subscribe = ctx.onMemoryAuthorityLost;
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    assert.equal(f.claim.secrets.codex, undefined, "the fixture is a Claude claim");
    // Red: the pre-change gate returned early for a Claude claim and ran it unenrolled; an
    // incomplete memory field must now stop it before executor admission.
    for (const bad of [{ memory_policy: undefined }, { memory_policy: { version: 1 as const, max_interventions: 0 } },
      { memory_episode: undefined }]) {
      await r.execute({ ...f.claim, ...bad });
      assert.equal(calls, 0, "an incomplete Claude claim must stop before executor admission");
    }
    // Green: the complete claim reaches the executor with the guard's seams.
    await r.execute(f.claim);
    assert.equal(calls, 1);
    assert.equal(typeof accessor, "function");
    assert.equal(typeof subscribe, "function");
    assert.ok(secrets instanceof SecretSet && secrets.values().includes(f.claim.secrets.forge_pat), "the run's redaction secrets ride the ctx");
  } finally { await f.memory.stop(); }
});

test("a runner without a memory runtime leaves a Claude claim unenrolled", async () => {
  const f = await setup();
  let accessor: unknown = "unset", subscribe: unknown = "unset", secrets: unknown = "unset";
  const executor: Executor = { run: async ctx => {
    accessor = ctx.capturedMemory;
    subscribe = ctx.onMemoryAuthorityLost;
    secrets = ctx.guardedCommandSecrets;
    throw new Error("fixture end");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab).execute(f.claim);
    assert.equal(accessor, undefined);
    assert.equal(subscribe, undefined);
    assert.equal(secrets, undefined);
  } finally { await f.memory.stop(); }
});

test("only the memory invalidation tells the executor; cancellation retires memory without telling it", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  const told: Error[] = [];
  let afterRetire = -1;
  let afterUnsubscribe = -1;
  const executor: Executor = { run: async ctx => {
    const off = ctx.onMemoryAuthorityLost!(stop => { told.push(stop); });
    const captured = ctx.capturedMemory!()!;
    // A flight cancel retires the memory context through its own listener: no invalidation.
    (captured.flight as { cancel: AbortController }).cancel.abort();
    await new Promise(resolve => setTimeout(resolve, 10));
    afterRetire = told.length;
    client.revokeMemoryAuthority();
    const afterRevoke = told.length;
    off();
    client.revokeMemoryAuthority();
    afterUnsubscribe = told.length;
    assert.equal(afterRevoke, 1);
    throw new Error("fixture end");
  } };
  const r = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  try {
    await r.execute(f.claim);
    assert.equal(afterRetire, 0, "retirement alone must not tell the executor");
    assert.equal(told.length, 1, "the invalidation told it exactly once");
    assert.equal(afterUnsubscribe, 1, "an unsubscribed listener hears nothing more");
    assert.equal(told[0]!.constructor.name, "RetainedRecoveryStop");
    assert.match(told[0]!.message, /retained recovery stopped/);
  } finally { await f.memory.stop(); await monitoring.catch(() => undefined); }
});

test("credential capture joins summary and confirms guarded commands before source access with the remaining budget", async () => {
  const f = await setup();
  const r = runnerWith(() => ({ executor: {} as Executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory });
  const owner = r as unknown as {
    runFreeCapture(claim: ClaimResponse, flight: unknown, deadlineAt: number, signal: undefined,
      action: () => Promise<string>): Promise<{ kind: string; value?: string }>;
  };
  let sourceCalls = 0;
  const settle = (executor: Executor, deadlineAt = Date.now() + 500) => owner.runFreeCapture(f.claim, {
    executor, survivingTickGroups: [], sinkGate: { run: (action: () => Promise<unknown>) => action() },
  }, deadlineAt, undefined, async () => { sourceCalls++; return "source"; });
  try {
    let deadlines: number[] = [];
    const unconfirmed: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async deadline => { deadlines.push(deadline); return { confirmed: false }; } };
    assert.equal((await settle(unconfirmed)).kind, "incomplete");
    assert.equal(sourceCalls, 0);
    assert.equal(deadlines.length, 1);
    assert.ok(deadlines[0]! > 0 && deadlines[0]! <= 500);
    const throwing: Executor = { run: async () => { throw new Error("unused"); },
      settleWorkerCommands: async () => { throw new Error("boom"); } };
    assert.equal((await settle(throwing)).kind, "incomplete");
    assert.equal(sourceCalls, 0);

    let releaseSummary!: () => void, releaseCommands!: () => void;
    let commandsEntered!: () => void;
    const summary = new Promise<void>(resolve => { releaseSummary = resolve; });
    const commands = new Promise<void>(resolve => { releaseCommands = resolve; });
    const entered = new Promise<void>(resolve => { commandsEntered = resolve; });
    const clock = Date.now;
    const original = clock();
    let now = original;
    let summaryBudget = 0;
    const confirmed: Executor = { run: async () => { throw new Error("unused"); },
      closeAndSettlePlanSummary: async budget => { summaryBudget = budget!; await summary; },
      settleWorkerCommands: async deadline => { deadlines.push(deadline); commandsEntered(); await commands; return { confirmed: true }; } };
    deadlines = [];
    Date.now = () => now;
    try {
      const pending = settle(confirmed, original + 500);
      await Promise.resolve();
      assert.equal(sourceCalls, 0, "pending summary prevents source work");
      now += 125;
      releaseSummary();
      await entered;
      assert.equal(summaryBudget, 500);
      assert.deepEqual(deadlines, [375], "command settle consumes the original remaining budget");
      assert.equal(sourceCalls, 0, "pending commands prevent source work");
      releaseCommands();
      assert.deepEqual(await pending, { kind: "observed_empty", value: "source" });
    } finally { Date.now = clock; releaseSummary(); releaseCommands(); }
    assert.equal(sourceCalls, 1);
    assert.equal((await settle({ run: async () => { throw new Error("unused"); } })).kind, "observed_empty");
    assert.equal(sourceCalls, 2);
  } finally { await f.memory.stop(); }
});

/** Run a Claude claim whose executor is told the memory invalidation and ends with `ending(stop)`. */
async function invalidatedRun(ending: (stop: Error) => Error) {
  const f = await setup();
  const monitoring = f.memory.run();
  let told: Error | undefined;
  const executor: Executor = { run: async ctx => {
    ctx.onMemoryAuthorityLost!(stop => { told = stop; });
    ctx.capturedMemory!();
    client.revokeMemoryAuthority();
    // Asserted outside the run: an assertion thrown here would itself report `failed`, making the control vacuous.
    throw ending(told ?? new Error("invalidation not delivered"));
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: f.memory }).execute(f.claim);
    return { failed: api.states.some(s => s.body.status === "failed"), told: told !== undefined };
  } finally { await f.memory.stop(); await monitoring.catch(() => undefined); }
}

test("a Claude executor that rejects with the carried retained stop lands in the retained-custody arm, not a failure", async () => {
  // The carrier rethrows exactly the stop the runner delivered.
  const r = await invalidatedRun(stop => stop);
  assert.ok(r.told, "the invalidation reached the executor");
  assert.equal(r.failed, false, "the retained stop reports no model failure");
});

test("a Claude executor that rebuilds the error after the invalidation is reported as a failure (the control)", async () => {
  const r = await invalidatedRun(() => new Error("rebuilt after the invalidation"));
  assert.ok(r.told, "the invalidation reached the executor");
  assert.equal(r.failed, true);
});

// ---------------------------------------------------------------------------------------------
// Issue #4806: a memory-guarded Claude run parked at a human plan gate takes an in-place credential
// switch (applied), then a human revise bound to that exact gate revision arrives. The guarded
// revision planning turn must be admitted on the NEW credential, and the revised plan must reach a
// new actionable human gate. Real runner, real SdkExecutor over a scripted SdkQueryFn, real
// WorkerMemoryRuntime, the in-place apply route of the api fake.
// ---------------------------------------------------------------------------------------------
const NONCE = "n".repeat(43);
const GEN = 7;
const SESSION = "sess-4806";
const CLARIFICATION_FEATURE = "clarification_revision_settlement_v1";
// Assembled at runtime so no token-shaped literal sits in the source.
const OLD_TOKEN = ["sk-ant-oat01-", "OLDTOKEN4806-0123456789"].join("");
const NEW_TOKEN = ["sk-ant-oat01-", "NEWTOKEN4806-0123456789"].join("");
const PLAN_V1 = "# PLAN v1\n- do it";
const PLAN_V2 = "# PLAN v2\n- do it better";
const SHA40 = "cafef00dcafef00dcafef00dcafef00dcafef00d";
const NO_SUMMARIES = {
  async generateIntentSummary(): Promise<string | null> { return null; },
  async generatePlanSummary(): Promise<null> { return null; },
} as never;
const CLEAN: QuiesceRunOutcome = {
  process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" },
};

const initFrame = (): SDKMessage => ({ type: "system", subtype: "init", session_id: SESSION, model: "m" }) as unknown as SDKMessage;
const planFrames = (plan: string) => async function* (): AsyncIterable<SDKMessage> {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }], SESSION);
  yield resultOk(SESSION);
};
const askFrames = async function* (): AsyncIterable<SDKMessage> {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "q", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Which db?" }] } }], SESSION);
  yield resultOk(SESSION);
};
const doneFrames = async function* (): AsyncIterable<SDKMessage> {
  yield initFrame();
  yield assistant([{ type: "text", text: "done implementing" }, { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} }], SESSION);
  yield resultOk(SESSION);
};

interface TurnRecord { index: number; resume: string | undefined; token: string | undefined; admits: boolean | undefined;
  remaining: number | undefined; ciFixHumanApproved: boolean | undefined; captureError?: string }
interface GateOpts {
  lane: "checked" | "ordinary";
  witnessed?: boolean;
  clarify?: boolean;
  /** memory_intervention_count of the claim. */
  count?: number;
  kind?: "issue" | "ci_fix";
  /** Control: no switch is requested; the revise follows the first gate directly. */
  control?: boolean;
  /** Request the switch (and the owner's answer) while the revision's clarification is parked. */
  switchAtQuestion?: boolean;
  /** Approve the revised gate and run to completion (default); false cancels once it is presented. */
  approve?: boolean;
  /** Runs inside the revision planning turn, after its capture and before its plan is submitted. */
  duringRevisionTurn?: (x: GateX) => Promise<void>;
}
interface GateX {
  runId: string;
  pressure(bytes: number): void;
  reservations: unknown[];
  captured(): ReturnType<NonNullable<RunContext["capturedMemory"]>>;
}

async function gateDrive(o: GateOpts) {
  newApi();
  api.memoryRegisterNonce = NONCE;
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence", "gate_revision_v1",
    "credential_switch_hold", "credential_switch_apply_v1", CLARIFICATION_FEATURE, "milestone_reconciliation_v1"]);
  await client.register("memory-gate-4806", "base");
  client.protocolFeatures = [...client.protocolFeatures, "gate_revision_v1", "credential_switch_hold", "credential_switch_apply_v1",
    ...(o.clarify ? [CLARIFICATION_FEATURE] : []), ...(o.witnessed ? ["milestone_reconciliation_v1"] : [])];
  (client as unknown as { hasInPlaceCredentialSwitchCapability: boolean }).hasInPlaceCredentialSwitchCapability = true;
  api.checkedTransport = true;
  api.usageHandler = () => ({ status: 200, body: {} });
  const claim = freshClaim(GEN, { kind: o.kind ?? "issue", auto_approve: false,
    ...(o.lane === "checked" ? { plan_cross_check_gated: true } : {}),
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 }, memory_intervention_count: o.count ?? 0 });
  claim.secrets = { ...claim.secrets, anthropic_oauth_token: OLD_TOKEN };
  const runId = claim.run_id;
  api.setOwnershipStatus(runId, "running", GEN);
  api.inPlaceInputDrain = true;
  if (o.clarify) api.stateAckWire = (body, run) => body.status !== "clarification_settled" ? undefined
    : { status: 200, body: { run: { ...run, status: "awaiting_approval" }, disposition: "clarification_settled" } };

  // The checker approves (a gated pass): the human still approves, at the gate this run then parks at.
  const digest = "a".repeat(64);
  if (o.lane === "checked") api.crossCheckHandler = async ({ runId: id, method, body }) => {
    const { stage: _s, claim_generation: _g, round = 1, ...candidate } = body as Record<string, unknown>;
    const proof = { lead_last_seq: Math.max(0, ...api.messages(id).map((m) => m.seq)), claim_generation: GEN,
      plan_cross_check_settled: true, gate_revision: 0, current_plan_sha256: "0".repeat(64) };
    if (method === "GET") return { status: 200, body: undefined };
    return { status: 200, body: { result: "candidate", round, checker_run_id: "11111111-1111-4111-8111-111111111111",
      candidate_digest: digest, candidate_generation: GEN, verdict: "approve", reason_class: "approve", findings: null,
      deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof,
      candidate: { ...candidate, plan_md: PLAN_V1, milestones: [], required_capabilities: [], required_tools: [], size_class: "s" },
      automatic_revision_limit: 3, automatic_rounds_enabled: false } };
  };

  // ---- observation seams ------------------------------------------------------------------
  const waitTrace: string[] = [];
  const proto = SteeringChannel.prototype as unknown as { awaitGateEvent: (...a: unknown[]) => Promise<unknown> };
  const realAwait = proto.awaitGateEvent;
  proto.awaitGateEvent = function (this: unknown, ...args: unknown[]) {
    waitTrace.push("enter");
    const p = realAwait.apply(this, args);
    p.then(() => waitTrace.push("settle"), (e: unknown) => waitTrace.push(e instanceof CredentialSwitchSignal ? "reject:switch" : "reject:other"));
    return p;
  };
  const acks: Array<{ body: StateRequest; ack: StateAck }> = [];
  const reportState = client.reportState.bind(client);
  client.reportState = async (...args: Parameters<typeof reportState>) => {
    const ack = await reportState(...args);
    if (args[0] === runId) acks.push({ body: args[1], ack });
    return ack;
  };
  const reportChecked = client.reportPlanCrossCheckGateState.bind(client);
  client.reportPlanCrossCheckGateState = async (...args: Parameters<typeof reportChecked>) => {
    const ack = await reportChecked(...args);
    if (args[0] === runId) acks.push({ body: args[1], ack });
    return ack;
  };
  let witnessCalls = 0;
  const seam = RunRunner.prototype as unknown as { prepareGateCheckpoint: (...a: unknown[]) => Promise<unknown> };
  const realPrepare = seam.prepareGateCheckpoint;
  if (o.witnessed) seam.prepareGateCheckpoint = async () =>
    witnessCalls++ === 0 ? { source_head: SHA40, published_checkpoint: SHA40 } : undefined;

  // ---- memory -----------------------------------------------------------------------------
  let usage = 0, sampleId = 0, used = o.count ?? 0;
  const reservations: unknown[] = [];
  const accepted = new Map<string, number>();
  client.reserveMemoryIntervention = async request => {
    reservations.push(request);
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  const memory = new WorkerMemoryRuntime({ enabled: true, reserveBytes: 100, hysteresisBytes: 100, sampleMs: 2,
    responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 }, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time, currentBytes: usage, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  const monitoring = memory.run();

  // ---- the scripted provider --------------------------------------------------------------
  const turns: TurnRecord[] = [];
  const captureErrors: string[] = [];
  let rctx: RunContext | undefined;
  const x: GateX = { runId, pressure: (bytes) => { usage = bytes; }, reservations,
    captured: () => rctx?.capturedMemory?.() };
  const scripts: Array<() => AsyncIterable<SDKMessage>> = [planFrames(PLAN_V1),
    ...(o.clarify ? [askFrames] : []), planFrames(PLAN_V2), doneFrames];
  let turnNo = 0;
  const queryFn: SdkQueryFn = (params) => {
    const index = turnNo++;
    const env = params.options.env as Record<string, string | undefined>;
    const record: TurnRecord = { index, resume: params.options.resume, token: env.CLAUDE_CODE_OAUTH_TOKEN,
      admits: undefined, remaining: undefined, ciFixHumanApproved: undefined };
    turns.push(record);
    const script = scripts[Math.min(index, scripts.length - 1)]!;
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      try {
        const captured = rctx?.capturedMemory?.();
        if (captured) {
          record.admits = captured.commands.phaseAdmits(captured.context);
          record.remaining = captured.commands.remaining(captured.context);
          record.ciFixHumanApproved = (captured.flight as { ciFixHumanApproved?: boolean }).ciFixHumanApproved;
        }
      } catch (error) { record.captureError = error instanceof Error ? error.message : String(error); }
      if (index === 1) await o.duringRevisionTurn?.(x);
      yield* script();
    })();
  };
  const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn, client, summaryRunner: NO_SUMMARIES });
  const run = sdk.run.bind(sdk);
  sdk.run = async (ctx) => {
    rctx = ctx;
    const capture = ctx.capturedMemory;
    // Name what the guard refused with: a refused capture is how a turn is stopped before the provider.
    if (capture) ctx.capturedMemory = () => {
      try { return capture(); } catch (error) { captureErrors.push(error instanceof Error ? error.constructor.name : String(error)); throw error; }
    };
    return run(ctx);
  };

  let requested = o.control === true || o.switchAtQuestion === true;
  api.onState(runId, (b) => {
    if (b.status === "awaiting_approval") {
      if (b.plan_md?.includes("PLAN v2")) { if (o.approve !== false) send(runId, row("approve_plan")); }
      else if (!requested) {
        requested = true;
        api.requestCredentialSwitch(runId, GEN, "op-1");
        api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
      }
    }
    if (b.status === "awaiting_input" && b.open_question_id) {
      if (o.switchAtQuestion) {
        api.requestCredentialSwitch(runId, GEN, "op-1");
        api.scriptApply(runId, { outcome: "applied", token: NEW_TOKEN });
      }
      send(runId, row("answer", JSON.stringify({ question_id: b.open_question_id, answers: ["postgres"] })));
    }
  });
  // The stop decision of a deferred receipt signal, and whether a guarded command could be admitted afterwards.
  const stops: Array<{ error: string; failClosed: boolean }> = [];
  const admitted: boolean[] = [];
  const classifier = RunRunner.prototype as unknown as { classifyDeferredReceiptSignal: (...a: unknown[]) => { error: unknown; failClosed: boolean } };
  const realClassify = classifier.classifyDeferredReceiptSignal;
  classifier.classifyDeferredReceiptSignal = function (this: unknown, ...args: unknown[]) {
    const stop = realClassify.apply(this, args);
    stops.push({ error: stop.error instanceof Error ? stop.error.constructor.name : String(stop.error), failClosed: stop.failClosed });
    const cap = (() => { try { return rctx?.capturedMemory?.(); } catch { return undefined; } })();
    if (cap) {
      const sample = (): void => { admitted.push(cap.commands.isCurrent(cap.context, cap.flight, cap.lifecycleRevision) && cap.commands.admissionReason(cap.context) === undefined); };
      sample();
      sampler = setInterval(sample, 1);
    }
    return stop;
  };
  let sampler: NodeJS.Timeout | undefined;

  const runner = runnerWith(() => ({ executor: sdk }), fakeGitlab().gitlab, undefined, nullLogger(), {
    memoryRuntime: memory, recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, switchSupersedeWaitMs: 2_000,
    questionTimeoutMs: 20_000, codexBoundaryDeadlineMs: 1_000, planCrossCheckTiming: { preparationMs: 3000, backoffMs: [1, 2], requestMs: 100, pollMs: 5 },
    credentialSwitchLedger: new CredentialSwitchOpLedger(path.join(homeDir, "..", "ledger-4806")),
    quiesceRun: async () => CLEAN,
  });
  let finished = false;
  const done = runner.execute(claim).finally(() => { finished = true; });
  const waitFor = async (pred: () => boolean, ms = 20_000): Promise<boolean> => until(() => finished || pred(), ms).then(() => pred());
  const gateAcks = () => acks.filter(a => a.body.status === "awaiting_approval");
  let revise: ReturnType<typeof send>[number] | undefined;
  let beforeRevise: { gates: number; presentationIds: string[]; revision: number | undefined } | undefined;
  try {
    const applied = o.control || o.switchAtQuestion ? await waitFor(() => gateAcks().length >= 1 && waitTrace.length >= 1) : await waitFor(() => api.applyRequests.length >= 1);
    const rejoined = applied && (o.control || o.switchAtQuestion || await waitFor(() => waitTrace.filter(e => e === "enter").length >= 2));
    if (rejoined) {
      const presented = acks.filter(a => a.body.status === "awaiting_approval");
      beforeRevise = { gates: presented.length, presentationIds: presented.map(a => String(a.body.presentation_id)),
        revision: presented.at(-1)?.ack.gateRevision };
      [revise] = send(runId, row("revise_plan", "please revise"));
    }
    if (o.approve === false) {
      if (revise && await waitFor(() => acks.filter(a => a.body.status === "awaiting_approval").length >= 2 || api.states.some(s => s.runId === runId && s.body.status === "recovery_wait")))
        await waitFor(() => revise !== undefined && api.isApplied(runId, revise.id), 5_000);
      send(runId, row("cancel"));
    }
    await done;
  } finally {
    proto.awaitGateEvent = realAwait;
    classifier.classifyDeferredReceiptSignal = realClassify;
    clearInterval(sampler);
    client.reportState = reportState;
    client.reportPlanCrossCheckGateState = reportChecked;
    seam.prepareGateCheckpoint = realPrepare;
    if (!finished) { send(runId, row("cancel")); await done.catch(() => undefined); }
    await memory.stop();
    await monitoring.catch(() => undefined);
  }
  const states = api.states.filter(s => s.runId === runId).map(s => s.body);
  return { claim, runId, turns, captureErrors, stops, admitted, waitTrace, acks, states, revise, beforeRevise, reservations, witnessCalls: () => witnessCalls };
}
type GateRun = Awaited<ReturnType<typeof gateDrive>>;

const gateReports = (r: GateRun) => r.acks.filter(a => a.body.status === "awaiting_approval");
// A scenario that ends at the revised gate cancels the run itself; that cancel is not a failure of the flow.
const failedWith = (r: GateRun): string[] => r.states.filter(s => (s.status === "failed" && s.failure_reason !== "run cancelled") || s.status === "recovery_wait")
  .map(s => `${s.status}:${s.failure_reason ?? s.recovery_cause ?? ""}`);

/** The invariants every gate case shares. Every violated predicate is named (not only the first). */
function assertContinuedAtTheSameGate(r: GateRun, opts: { completes?: boolean } = {}): void {
  const bad: string[] = [];
  const check = (ok: unknown, what: string): void => { if (!ok) bad.push(what); };
  check(JSON.stringify(r.waitTrace.slice(0, 3)) === JSON.stringify(["enter", "reject:switch", "enter"]),
    `the original wait rejected before the re-join: ${r.waitTrace.join(",")}`);
  check(api.applyRequests.length === 1, "the switch applied once");
  const gates = gateReports(r);
  const before = r.beforeRevise;
  check(r.revise && before, "the revise was sent after the apply");
  if (r.revise && before) {
    check(before.gates === 1, `no gate was published again between the apply and the revise: ${before.gates} reports`);
    check(r.revise.gate_binding === "bound" && r.revise.gate_revision === before.revision,
      `the revise is bound to the pre-apply gate revision ${String(before.revision)}, row says ${String(r.revise.gate_revision)}`);
    check(gates[0]?.body.presentation_id === before.presentationIds[0], "the presentation id is the pre-apply one");
  }
  check(r.captureErrors.length === 0, `no guarded turn capture was refused: ${r.captureErrors.join(",")}`);
  check(failedWith(r).length === 0, `no failure or recovery park: ${failedWith(r).join(",")}`);
  // Every turn after the apply runs on the NEW credential; the one before it on the old one.
  check(r.turns[0]?.token === OLD_TOKEN, "the first turn ran on the old token");
  check(r.turns.length >= 2, `a revision turn was admitted: ${r.turns.length} turn(s)`);
  for (const turn of r.turns.slice(1)) {
    check(turn.token === NEW_TOKEN, `turn ${turn.index} ran on the new token`);
    check(turn.admits === true, `turn ${turn.index} is admitted by the phase`);
  }
  // The usage epoch ordinal: init legs after the server's switch frame carry ordinal 1.
  const frames = [...api.messages(r.runId)].sort((a, b) => a.seq - b.seq);
  const server = frames.find(m => (m.kind as string) === "credential_switch");
  check(server, "the server's credential_switch frame");
  const inits = frames.filter(m => server && m.seq > server.seq && m.kind === "status" && (m.payload as { event?: string }).event === "init");
  check(inits.length >= 1 && inits.every(m => m.credential_epoch_ordinal === 1), `post-apply init legs carry epoch ordinal 1 (${inits.length})`);
  // The revised plan is persisted at a new actionable human gate and the revise's receipt is settled there.
  check(gates.length >= 2 && gates.at(-1)!.body.plan_md?.includes("PLAN v2") && gates.at(-1)!.ack.applied === true,
    `the revised plan was presented at a new gate (${gates.length} gate reports)`);
  check(r.revise && api.isApplied(r.runId, r.revise.id), "the revise's receipt is settled");
  if (opts.completes !== false) check(r.states.some(s => s.status === "completed"), `the run completed: ${r.states.map(s => s.status).join(",")}`);
  assert.deepEqual(bad, [], "violated predicates");
}

describe("issue #4806: a human revise after an applied in-place switch at a guarded human gate", { timeout: 90_000 }, () => {
  it("(i) checked human gate: the revise is admitted on the new token and persisted at a new gate", async () => {
    assertContinuedAtTheSameGate(await gateDrive({ lane: "checked" }));
  });
  it("(ii) ordinary human gate", async () => {
    assertContinuedAtTheSameGate(await gateDrive({ lane: "ordinary" }));
  });
  it("(iii) witnessed gate: its settle sends running on revise", async () => {
    const r = await gateDrive({ lane: "ordinary", witnessed: true, approve: false });
    assertContinuedAtTheSameGate(r, { completes: false });
    assert.equal(r.witnessCalls() >= 1, true, "the first gate carried a checkpoint witness");
    assert.ok(r.states.some(s => s.status === "running"), "the witnessed settle sent running");
  });
  it("(iv) a clarification asked and answered in a revision after an applied switch settles only its question", async () => {
    const r = await gateDrive({ lane: "checked", clarify: true });
    assertContinuedAtTheSameGate(r);
    const settled = r.states.filter(s => s.status === "clarification_settled");
    assert.equal(settled.length, 1, "exactly one clarification settled");
    const asked = r.states.filter(s => s.status === "awaiting_input");
    assert.equal(asked.length, 1);
    assert.equal(settled[0]!.clarification_settlement?.revision_input_id, r.revise!.id, "bound to the pending revise");
    assert.equal(settled[0]!.clarification_settlement?.question_id, asked[0]!.open_question_id, "only the exact question settled");
    assert.equal(r.turns.length >= 4, true, `planning was re-admitted after the answer: ${r.turns.length}`);
    assert.equal(r.turns[2]!.admits, true);
  });
});

describe("issue #4806: a receipt refusal inside the deferred revision window fails closed", { timeout: 90_000 }, () => {
  it("an answer refused under a pending switch ends the flight as a retained stop: no release, no apply, no admission", async () => {
    const r = await gateDrive({ lane: "checked", clarify: true, switchAtQuestion: true, approve: false });
    const asked = r.states.filter(s => s.status === "awaiting_input");
    assert.equal(asked.length, 1, "the revision's clarification was asked");
    assert.deepEqual(r.stops, [{ error: "CredentialSwitchRetainedStop", failClosed: true }], "the deferred receipt signal was classified fail-closed");
    assert.equal(api.applyRequests.length, 0, "nothing was applied");
    for (const status of ["credential_switch", "clarification_settled"])
      assert.equal(r.states.some(s => s.status === status), false, `no ${status} report`);
    assert.equal(r.states.some(s => s.status === "failed" && s.failure_reason !== "run cancelled"), false, "never a failure");
    assert.ok(r.admitted.length > 0, "the context was sampled from the stop decision to the end of the run");
    assert.equal(r.admitted.some(Boolean), false, "no guarded command was admitted between the stop decision and the end of teardown");
  });
});

describe("issue #4806: control", { timeout: 90_000 }, () => {
  it("control: with no switch a guarded human revise is admitted and persisted (the fixture is not vacuous)", async () => {
    const r = await gateDrive({ lane: "checked", control: true });
    assert.deepEqual(r.captureErrors, []);
    assert.deepEqual(r.turns.map(t => t.token), [OLD_TOKEN, OLD_TOKEN, OLD_TOKEN]);
    assert.ok(api.isApplied(r.runId, r.revise!.id));
    assert.ok(r.states.some(s => s.status === "completed"), r.states.map(s => s.status).join(","));
  });
});

describe("issue #4806: renewal keeps the memory allowance, the pressure path and the ci_fix approval", { timeout: 90_000 }, () => {
  it("a claim with spent interventions renews with exactly the remaining allowance", async () => {
    const r = await gateDrive({ lane: "checked", count: 1 });
    assertContinuedAtTheSameGate(r);
    assert.equal(r.turns[0]!.remaining, 1, "the original context had max - spent");
    assert.equal(r.turns[1]!.remaining, 1, "the renewed context has max - spent, not a fresh allowance");
  });

  it("a pressure intervention on the renewed context reaches the real feedback and preservation callbacks", async () => {
    const r = await gateDrive({ lane: "checked", approve: false, count: 1, duringRevisionTurn: async x => {
      // A live guarded command on the RENEWED context: the intervention needs a victim to act on.
      const cap = x.captured()!;
      cap.commands.register(cap.context, {
        source: "claude-model-command", toolId: "renewed-tool", attemptId: "renewed-attempt",
        command: "inert fixture", root: { pid: 10, startTime: "10" },
        observe: async () => {
          const time = performance.now();
          return { available: true, complete: true, startedAtMs: time, completedAtMs: time,
            rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
        },
        cancel: async () => undefined,
        drain: async () => ({ confirmed: true }),
      });
      x.pressure(950);
      await until(() => x.reservations.length >= 1, 10_000);
      // The allowance is spent, so the intervention preserves and parks the run (the real preservation callback).
      await until(() => api.states.some(st => st.runId === x.runId && st.body.status === "recovery_wait"), 10_000);
    } });
    assert.equal(r.reservations.length >= 1, true, "the renewed context asked the server to admit an intervention");
    assert.equal(r.turns[1]?.token, NEW_TOKEN, "the pressured turn ran on the new token");
    const feed = api.messages(r.runId).filter(m => m.kind === "status" && (m.payload as { event?: string }).event === "worker_memory_pressure");
    assert.equal(feed.length >= 1, true, "the feedback callback posted the pressure result to the feed");
    assert.ok(r.states.some(s => s.status === "recovery_wait" && s.recovery_cause === "worker_memory_pressure"),
      `the preservation callback parked the run: ${r.states.map(s => `${s.status}:${s.recovery_cause ?? ""}`).join(",")}`);
  });

  for (const lane of ["checked", "ordinary"] as const) {
    it(`ci_fix (${lane}): a human approve of the revised plan applies and the pre-push guard sees the human approval`, async () => {
      const r = await gateDrive({ lane, kind: "ci_fix" });
      assertContinuedAtTheSameGate(r);
      const approves = api.inputRows(r.runId).filter(row => row.kind === "approve_plan");
      assert.equal(approves.length, 1);
      assert.ok(api.isApplied(r.runId, approves[0]!.id), "the exact human approve applied");
      const implement = r.turns.at(-1)!;
      assert.equal(implement.ciFixHumanApproved, true, "ciFixHumanApproved was true when implementation started");
    });
  }
});
