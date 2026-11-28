// Issue #4806: the shared driver of the revision-switch suites: a real runner over a real SdkExecutor, a scripted
// SdkQueryFn, a real WorkerMemoryRuntime and the in-place apply route of the api fake. Used by
// runner-worker-memory-claude.test.ts and runner-credential-switch-revision-memory.test.ts.
import assert from "node:assert/strict";
import path from "node:path";
import { performance } from "node:perf_hooks";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import type { RunContext } from "../src/executor.js";
import type { StateAck, StateRequest } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { recordingLogger } from "./helpers.js";
import { CredentialSwitchSignal, SteeringChannel } from "../src/steering.js";
import { CredentialSwitchOpLedger } from "../src/credential-switch-op-ledger.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { RunRunner } from "../src/runner.js";
import type { QuiesceRunOutcome } from "../src/run-quiescence.js";
import { assistant, resultOk, api, client, fakeGitlab, homeDir, runnerWith } from "./runner-harness.js";
import { freshClaim, newApi, row, send, until } from "./gate-revision-harness.js";

export const NONCE = "n".repeat(43);
export const GEN = 7;
export const SESSION = "sess-4806";
export const CLARIFICATION_FEATURE = "clarification_revision_settlement_v1";
// Assembled at runtime so no token-shaped literal sits in the source.
export const OLD_TOKEN = ["sk-ant-oat01-", "OLDTOKEN4806-0123456789"].join("");
export const NEW_TOKEN = ["sk-ant-oat01-", "NEWTOKEN4806-0123456789"].join("");
export const PLAN_V1 = "# PLAN v1\n- do it";
export const PLAN_V2 = "# PLAN v2\n- do it better";
export const SHA40 = "cafef00dcafef00dcafef00dcafef00dcafef00d";
const NO_SUMMARIES = {
  async generateIntentSummary(): Promise<string | null> { return null; },
  async generatePlanSummary(): Promise<null> { return null; },
} as never;
const CLEAN: QuiesceRunOutcome = {
  process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" },
};

const initFrame = (): SDKMessage => ({ type: "system", subtype: "init", session_id: SESSION, model: "m" }) as unknown as SDKMessage;
export const planFrames = (plan: string) => async function* (): AsyncIterable<SDKMessage> {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: { plan_md: plan } }], SESSION);
  yield resultOk(SESSION);
};
export const askFrames = async function* (): AsyncIterable<SDKMessage> {
  yield initFrame();
  yield assistant([{ type: "tool_use", id: "q", name: "mcp__uzi__ask_user", input: { questions: [{ question: "Which db?" }] } }], SESSION);
  yield resultOk(SESSION);
};
const doneFrames = async function* (): AsyncIterable<SDKMessage> {
  yield initFrame();
  yield assistant([{ type: "text", text: "done implementing" }, { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} }], SESSION);
  yield resultOk(SESSION);
};

export interface TurnRecord { index: number; resume: string | undefined; token: string | undefined; admits: boolean | undefined;
  remaining: number | undefined; ciFixHumanApproved: boolean | undefined; captureError?: string }
export interface GateOpts {
  lane: "checked" | "ordinary";
  witnessed?: boolean;
  /** Every gate (not only the first) is prepared with a checkpoint witness. */
  witnessAll?: boolean;
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
  /** Runs inside the real prepareGateCheckpoint seam of every gate (its result is the witness of the first gate only
   *  when `witnessed`). */
  duringPrepare?: (x: GateX, call: number) => Promise<void>;
  /** Memory runtime budgets (defaults are the m1 fixture's). */
  memory?: { responseBudgetMs?: number; sampleMs?: number };
  /** Set up spies before the run starts; the returned functions restore them after it. */
  install?: (x: GateX) => Array<() => void>;
  /** Replaces the default flow (wait for the gate, switch/revise, wait for the end). `finish` runs after it. */
  drive?: (x: GateX) => Promise<void>;
  /** Replaces the first-gate default of requesting the switch there (the default requests it unless `control`). */
  noSwitchAtGate?: boolean;
  /** The first gate's APPLY is withheld from the fake, and the hook of the scenario scripts its own outcomes. */
  noScriptApply?: boolean;
  /** Provider frames of the turn at `index` replace the scripted ones. */
  scriptOverride?: Record<number, () => AsyncIterable<SDKMessage>>;
  /** The checker REVISEs round 1 (an automatic revision turn follows) and approves round 2. */
  autoRevise?: boolean;
  /** The apply outcomes the first gate's switch is scripted with (default: one applied with NEW_TOKEN). */
  applyScript?: Array<Parameters<typeof api.scriptApply>[1]>;
  /** Runs after the apply POST was answered (the server applied) and before the worker sees the answer. */
  duringApply?: (x: GateX) => Promise<void>;
  /** Runs at EVERY entry of the switch hook (tagged or not) before it decides; `tagged` says which. */
  beforeHook?: (x: GateX, tagged: boolean) => Promise<void>;
  /** The first gate's switch is requested without an op id (the v1 contract). */
  opless?: boolean;
  /** Runs, with the real tagged signal in hand, at the hook route (ctx.attemptCredentialSwitch) before it is classified. */
  atHook?: (x: GateX) => Promise<void>;
  /** Runs, with the real tagged signal in hand, at the outer route (the executor's rejection reaching executeClaim). */
  atOuter?: (x: GateX) => Promise<void>;
  /** Also negotiate plan_revision_retry_v1 (the revision recovery lane); the default fixture does not. */
  retry?: boolean;
}
export interface GateX {
  runId: string;
  pressure(bytes: number): void;
  reservations: unknown[];
  captured(): ReturnType<NonNullable<RunContext["capturedMemory"]>>;
  runner: RunRunner;
  /** The flight's steering channel (set once the first guarded turn captured the memory context). */
  steering(): SteeringChannel;
  flight(): object;
  /** Resolves when a /state report of this run satisfying `pred` has been recorded (immediately if one already was). */
  stateSeen(pred: (b: StateRequest) => boolean): Promise<void>;
  /** Resolves once `n` more GET /inputs of this run have been answered. */
  afterInputGets(n: number): Promise<void>;
  /** Resolves when the first memory intervention reservation has been asked of the server. */
  reserved: Promise<void>;
  /** Request the switch now (op `op`) and script its apply with `token`; optionally ride the next report ACK. */
  requestSwitch(op: string, token: string, opts?: { onNextGateAck?: boolean }): void;
  /** Race `promise` against the end of the run: false when the run finished first. */
  waitFor(pred: () => boolean, ms?: number): Promise<boolean>;
  finished(): boolean;
  /** Send the bound human revise for the gate currently presented. */
  sendRevise(text?: string): ReturnType<typeof send>[number];
  cancel(): void;
  /** Wrap `owner[name]`, restoring it after the run. */
  spy<T extends object, K extends string>(owner: T, name: K, wrap: (orig: (...a: unknown[]) => unknown, self: unknown, args: unknown[]) => unknown): void;
  /** Every credential-switch defer window opened and closed (steering begin/endCredentialSwitchDefer). */
  defer: { begins: number; ends: number; depthSamples: number[] };
  turns: TurnRecord[];
  acks: Array<{ body: StateRequest; ack: StateAck }>;
  waitTrace: string[];
  events: string[];
  memory: WorkerMemoryRuntime;
  logs: unknown[];
}

export async function gateDrive(o: GateOpts) {
  newApi();
  api.memoryRegisterNonce = NONCE;
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence", "gate_revision_v1",
    "credential_switch_hold", "credential_switch_apply_v1", CLARIFICATION_FEATURE, "milestone_reconciliation_v1",
    ...(o.retry ? ["plan_revision_retry_v1"] : [])]);
  await client.register("memory-gate-4806", "base");
  client.protocolFeatures = [...client.protocolFeatures, "gate_revision_v1", "credential_switch_hold", "credential_switch_apply_v1",
    ...(o.clarify ? [CLARIFICATION_FEATURE] : []), ...(o.witnessed ? ["milestone_reconciliation_v1"] : []),
    ...(o.retry ? ["plan_revision_retry_v1"] : [])];
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
    const revising = o.autoRevise === true && round === 1;
    const proof = { lead_last_seq: Math.max(0, ...api.messages(id).map((m) => m.seq)), claim_generation: GEN,
      plan_cross_check_settled: true, gate_revision: 0, current_plan_sha256: "0".repeat(64) };
    if (method === "GET") return { status: 200, body: undefined };
    return { status: 200, body: { result: "candidate", round, checker_run_id: "11111111-1111-4111-8111-111111111111",
      candidate_digest: digest, candidate_generation: GEN, verdict: revising ? "revise" : "approve",
      reason_class: revising ? "revise" : "approve", findings: revising ? { summary: "fix round 1", items: [] } : null,
      deadline_at: new Date(Date.now() + 60_000).toISOString(), ...proof,
      candidate: { ...candidate, plan_md: o.autoRevise ? String((candidate as { plan_md?: unknown }).plan_md ?? PLAN_V1) : PLAN_V1,
        milestones: [], required_capabilities: [], required_tools: [], size_class: "s" },
      automatic_revision_limit: 3, automatic_rounds_enabled: o.autoRevise === true } };
  };

  let xRef: GateX | undefined;
  const { logger, lines: logs } = recordingLogger();
  const ledger = new CredentialSwitchOpLedger(path.join(homeDir, "..", `ledger-4806-${runId}`));
  const flights: Array<Record<string, unknown>> = [];
  // The ordered event log every ordering assertion reads (worker-side events only).
  const events: string[] = [];
  // ---- observation seams (every one is restored in the finally below) -------------------------
  const restores: Array<() => void> = [];
  const spy = (owner: object, name: string, wrap: (orig: (...a: unknown[]) => unknown, self: unknown, args: unknown[]) => unknown): void => {
    const target = owner as Record<string, (...a: unknown[]) => unknown>;
    const orig = target[name]!;
    target[name] = function (this: unknown, ...args: unknown[]) { return wrap(orig.bind(this), this, args); };
    restores.push(() => { target[name] = orig; });
  };
  const waitTrace: string[] = [];
  spy(SteeringChannel.prototype, "awaitGateEvent", (orig, _self, args) => {
    waitTrace.push("enter");
    const p = orig(...args) as Promise<unknown>;
    p.then(() => waitTrace.push("settle"), (e: unknown) => waitTrace.push(e instanceof CredentialSwitchSignal ? "reject:switch" : "reject:other"));
    return p;
  });
  spy(RunRunner.prototype, "inPlaceCredentialSwitch", (orig, _self, args) => { events.push("inplace"); return orig(...args); });
  spy(RunRunner.prototype, "beginMemoryContext", (orig, _self, args) => { events.push("begin-context"); return orig(...args); });
  const defer = { begins: 0, ends: 0, depthSamples: [] as number[] };
  spy(SteeringChannel.prototype, "beginCredentialSwitchDefer", (orig, _self, args) => { defer.begins++; events.push("defer:begin"); return orig(...args); });
  spy(SteeringChannel.prototype, "endCredentialSwitchDefer", (orig, _self, args) => { defer.ends++; events.push("defer:end"); return orig(...args); });
  // A trip that took effect (not deferred) rejects the parked waiters; a deferred stamp never reaches it.
  spy(SteeringChannel.prototype, "rejectParkedWaiters", (orig, _self, args) => { events.push("trip"); return orig(...args); });
  spy(client, "applyCredentialSwitch", async (orig, _self, args) => {
    events.push("apply");
    const result = await orig(...args);
    if (o.duringApply && xRef) await o.duringApply(xRef);
    events.push("apply:answered");
    return result;
  });
  const acks: Array<{ body: StateRequest; ack: StateAck }> = [];
  const stateWaiters: Array<{ pred: (b: StateRequest) => boolean; resolve: () => void }> = [];
  const noteState = (b: StateRequest): void => {
    for (const w of stateWaiters.splice(0)) { if (w.pred(b)) w.resolve(); else stateWaiters.push(w); }
  };
  const reportState = client.reportState.bind(client);
  client.reportState = async (...args: Parameters<typeof reportState>) => {
    if (args[0] === runId) events.push(`report:${args[1].status}`);
    const ack = await reportState(...args);
    if (args[0] === runId) {
      acks.push({ body: args[1], ack }); noteState(args[1]);
      events.push(`ack:${args[1].status}${ack.applied === true ? ":applied" : ""}`);
    }
    return ack;
  };
  restores.push(() => { client.reportState = reportState; });
  const reportChecked = client.reportPlanCrossCheckGateState.bind(client);
  client.reportPlanCrossCheckGateState = async (...args: Parameters<typeof reportChecked>) => {
    if (args[0] === runId) events.push(`report:${args[1].status}`);
    const ack = await reportChecked(...args);
    if (args[0] === runId) {
      acks.push({ body: args[1], ack }); noteState(args[1]);
      events.push(`ack:${args[1].status}${ack.applied === true ? ":applied" : ""}`);
    }
    return ack;
  };
  restores.push(() => { client.reportPlanCrossCheckGateState = reportChecked; });
  const getInputs = client.getInputs.bind(client);
  const inputWaiters: Array<{ left: number; resolve: () => void }> = [];
  client.getInputs = async (...args: Parameters<typeof getInputs>) => {
    const result = await getInputs(...args);
    if (args[0] === runId) for (const w of inputWaiters.splice(0)) { if (--w.left <= 0) w.resolve(); else inputWaiters.push(w); }
    return result;
  };
  restores.push(() => { client.getInputs = getInputs; });
  let witnessCalls = 0, prepareCalls = 0;
  spy(RunRunner.prototype, "prepareGateCheckpoint", async (orig, _self, args) => {
    const call = prepareCalls++;
    events.push(`prepare:begin:${call}`);
    if (o.duringPrepare && xRef) await o.duringPrepare(xRef, call);
    const result = !o.witnessed ? await orig(...args) : witnessCalls++ === 0 || o.witnessAll ? { source_head: SHA40, published_checkpoint: SHA40 } : undefined;
    events.push(`prepare:end:${call}`);
    return result;
  });

  // ---- memory -----------------------------------------------------------------------------
  let usage = 0, sampleId = 0, used = o.count ?? 0;
  const reservations: unknown[] = [];
  const accepted = new Map<string, number>();
  let markReserved!: () => void;
  const reserved = new Promise<void>((resolve) => { markReserved = resolve; });
  client.reserveMemoryIntervention = async request => {
    reservations.push(request);
    markReserved();
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true, allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  const memory = new WorkerMemoryRuntime({ enabled: true, reserveBytes: 100, hysteresisBytes: 100, sampleMs: o.memory?.sampleMs ?? 2,
    responseBudgetMs: o.memory?.responseBudgetMs ?? 200, rearmMs: 10, maxInterventions: 2 }, client, logger, {
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
  let flightRef: object | undefined;
  let finished = false;
  const x: GateX = {
    runId, pressure: (bytes) => { usage = bytes; }, reservations, reserved,
    captured: () => rctx?.capturedMemory?.(),
    runner: undefined as unknown as RunRunner,
    steering: () => (flightRef as { steering: SteeringChannel }).steering,
    flight: () => flightRef as object,
    stateSeen: (pred) => new Promise<void>((resolve) => {
      if (api.states.some((st) => st.runId === runId && pred(st.body))) return resolve();
      stateWaiters.push({ pred, resolve });
    }),
    afterInputGets: (n) => new Promise<void>((resolve) => { inputWaiters.push({ left: n, resolve }); }),
    requestSwitch: (op, token, opts) => {
      api.requestCredentialSwitch(runId, GEN, op);
      api.scriptApply(runId, { outcome: "applied", token });
      if (opts?.onNextGateAck) api.armStateAckCredentialSwitchOnce(runId, GEN, "awaiting_approval");
    },
    waitFor: (pred, ms = 20_000) => until(() => finished || pred(), ms).then(() => pred()),
    finished: () => finished,
    sendRevise: (text = "please revise") => send(runId, row("revise_plan", text))[0]!,
    cancel: () => { send(runId, row("cancel")); },
    spy, defer, turns, acks, waitTrace, events, memory, logs,
  };
  xRef = x;
  o.install?.(x);
  const scripts: Array<() => AsyncIterable<SDKMessage>> = [planFrames(PLAN_V1),
    ...(o.clarify ? [askFrames] : []), planFrames(PLAN_V2), doneFrames];
  let turnNo = 0;
  const queryFn: SdkQueryFn = (params) => {
    const index = turnNo++;
    const env = params.options.env as Record<string, string | undefined>;
    const record: TurnRecord = { index, resume: params.options.resume, token: env.CLAUDE_CODE_OAUTH_TOKEN,
      admits: undefined, remaining: undefined, ciFixHumanApproved: undefined };
    turns.push(record);
    events.push(`turn:${index}`);
    const script = o.scriptOverride?.[index] ?? scripts[Math.min(index, scripts.length - 1)]!;
    return (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      try {
        const captured = rctx?.capturedMemory?.();
        if (captured) {
          flightRef = captured.flight as object;
          record.admits = captured.commands.phaseAdmits(captured.context);
          record.remaining = captured.commands.remaining(captured.context);
          record.ciFixHumanApproved = (captured.flight as { ciFixHumanApproved?: boolean }).ciFixHumanApproved;
        }
      } catch (error) { record.captureError = error instanceof Error ? error.message : String(error); }
      if (index === 1) await o.duringRevisionTurn?.(x);
      yield* script();
    })();
  };
  const entries: Array<{ worktree: string; branch: string }> = [];
  const sdk = new SdkExecutor(logger, homeDir, { queryFn, client, summaryRunner: NO_SUMMARIES });
  const run = sdk.run.bind(sdk);
  sdk.run = async (ctx) => {
    rctx = ctx;
    entries.push({ worktree: ctx.worktreePath, branch: ctx.branch });
    const capture = ctx.capturedMemory;
    // Name what the guard refused with: a refused capture is how a turn is stopped before the provider.
    if (capture) ctx.capturedMemory = () => {
      try { return capture(); } catch (error) { captureErrors.push(error instanceof Error ? error.constructor.name : String(error)); throw error; }
    };
    const hook = ctx.attemptCredentialSwitch;
    if (hook) ctx.attemptCredentialSwitch = async (opts) => {
      const tagged = opts?.signal !== undefined && x.steering().isDeferredReceiptSignal(opts.signal);
      await o.beforeHook?.(x, tagged);
      if (o.atHook && tagged) await o.atHook(x);
      return hook(opts);
    };
    try { return await run(ctx); } catch (error) {
      if (o.atOuter && flightRef && x.steering().isDeferredReceiptSignal(error)) await o.atOuter(x);
      throw error;
    }
  };

  let requested = o.control === true || o.switchAtQuestion === true || o.noSwitchAtGate === true;
  api.onState(runId, (b) => {
    noteState(b);
    if (b.status === "awaiting_approval") {
      if (b.plan_md?.includes("PLAN v2")) { if (o.approve !== false) send(runId, row("approve_plan")); }
      else if (!requested) {
        requested = true;
        api.requestCredentialSwitch(runId, GEN, o.opless ? undefined : "op-1");
        if (!o.opless) api.scriptApply(runId, ...(o.applyScript ?? [{ outcome: "applied" as const, token: NEW_TOKEN }]));
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
  const stops: Array<{ error: string; failClosed: boolean; early: unknown }> = [];
  const admitted: boolean[] = [];
  let sampler: NodeJS.Timeout | undefined;
  spy(RunRunner.prototype, "classifyDeferredReceiptSignal", (orig, _self, args) => {
    const stop = orig(...args) as { error: unknown; failClosed: boolean };
    flights.push(args[0] as Record<string, unknown>);
    stops.push({ error: stop.error instanceof Error ? stop.error.constructor.name : String(stop.error), failClosed: stop.failClosed, early: args[1] });
    const cap = (() => { try { return rctx?.capturedMemory?.(); } catch { return undefined; } })();
    if (cap) {
      const sample = (): void => { admitted.push(cap.commands.isCurrent(cap.context, cap.flight, cap.lifecycleRevision) && cap.commands.admissionReason(cap.context) === undefined); };
      sample();
      sampler = setInterval(sample, 1);
    }
    return stop;
  });

  const runner = runnerWith(() => ({ executor: sdk }), fakeGitlab().gitlab, undefined, logger, {
    memoryRuntime: memory, recoveryRetryMs: 1, planApprovalTimeoutMs: 20_000, switchSupersedeWaitMs: 2_000,
    questionTimeoutMs: 20_000, codexBoundaryDeadlineMs: 1_000, planCrossCheckTiming: { preparationMs: 3000, backoffMs: [1, 2], requestMs: 100, pollMs: 5 },
    credentialSwitchLedger: ledger,
    quiesceRun: async () => CLEAN,
  });
  x.runner = runner;
  const done = runner.execute(claim).finally(() => { finished = true; });
  const waitFor = x.waitFor;
  const gateAcks = () => acks.filter(a => a.body.status === "awaiting_approval");
  let revise: ReturnType<typeof send>[number] | undefined;
  let beforeRevise: { gates: number; presentationIds: string[]; revision: number | undefined } | undefined;
  try {
    if (o.drive) await o.drive(x);
    else {
      const applied = o.control || o.switchAtQuestion || o.noSwitchAtGate ? await waitFor(() => gateAcks().length >= 1 && waitTrace.length >= 1) : await waitFor(() => api.applyRequests.length >= 1);
      const rejoined = applied && (o.control || o.switchAtQuestion || o.noSwitchAtGate || await waitFor(() => waitTrace.filter(e => e === "enter").length >= 2));
      if (rejoined) {
        const presented = acks.filter(a => a.body.status === "awaiting_approval");
        beforeRevise = { gates: presented.length, presentationIds: presented.map(a => String(a.body.presentation_id)),
          revision: presented.at(-1)?.ack.gateRevision };
        revise = x.sendRevise();
      }
      if (o.approve === false) {
        if (revise && await waitFor(() => acks.filter(a => a.body.status === "awaiting_approval").length >= 2 || api.states.some(s => s.runId === runId && s.body.status === "recovery_wait")))
          await waitFor(() => revise !== undefined && api.isApplied(runId, revise.id), 5_000);
        send(runId, row("cancel"));
      }
    }
    await done;
  } finally {
    for (const restore of restores.reverse()) restore();
    clearInterval(sampler);
    if (!finished) { send(runId, row("cancel")); await done.catch(() => undefined); }
    await memory.stop();
    await monitoring.catch(() => undefined);
  }
  const states = api.states.filter(s => s.runId === runId).map(s => s.body);
  return { claim, runId, turns, captureErrors, stops, admitted, waitTrace, acks, states, revise, beforeRevise, reservations, defer,
    runner, events, logs, entries, ledger, flights, witnessCalls: () => witnessCalls, x };
}
export type GateRun = Awaited<ReturnType<typeof gateDrive>>;

export const gateReports = (r: GateRun) => r.acks.filter(a => a.body.status === "awaiting_approval");
// A scenario that ends at the revised gate cancels the run itself; that cancel is not a failure of the flow.
export const failedWith = (r: GateRun): string[] => r.states.filter(s => (s.status === "failed" && s.failure_reason !== "run cancelled") || s.status === "recovery_wait")
  .map(s => `${s.status}:${s.failure_reason ?? s.recovery_cause ?? ""}`);

/** The invariants every gate case shares. Every violated predicate is named (not only the first). */
export function assertContinuedAtTheSameGate(r: GateRun, opts: { completes?: boolean } = {}): void {
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
