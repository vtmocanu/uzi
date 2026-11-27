import assert from "node:assert/strict";
import { AsyncLocalStorage } from "node:async_hooks";
import { test, type TestContext } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { RecoveryDeadlineExhaustedError } from "../src/git.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { captureFailureOf, recordCaptureFailure, type CredentialFreeCaptureContext } from "../src/harness.js";
import { performance } from "node:perf_hooks";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimResponse, MemoryReservationRequest, StateAck } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { PauseNowSignal } from "../src/steering.js";
import { RunRunner, StaleClaimError } from "../src/runner.js";
import { MessageBatcher } from "../src/batcher.js";
import type { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import { commitInTree } from "./codex-reap-fixture.js";
import { api, client, git, homeDir, fakeGitlab, gitlabClaim, input, installHarness,
  runnerWith, worktreeDirFor } from "./runner-harness.js";

import { codexRig } from "./runner-codex-sinks-fixture.js";

installHarness();

function mockUnenrolledSource(t: TestContext, claim: ClaimResponse): () => void {
  const expected = [git.barePathFor(claim.repo.clone_url), "agent/issue-2396", claim.run_id];
  const calls: string[][] = [];
  t.mock.method(git, "readRemediationRestrictionFacts", async (bare: string, branch: string, runId: string) => {
    calls.push([bare, branch, runId]);
    return { enrolled: false, blocked: false, roots: [], flaggedOIDs: [], fingerprint: "memory-unenrolled-fixture" };
  });
  return () => {
    assert.ok(calls.length > 0, "the unenrolled source fixture must observe a restriction read");
    for (const call of calls) assert.deepEqual(call, expected, "restriction reads use the exact source identity");
  };
}

type LaunchRoundTag = { round: "authority" | "clone"; seq: number };
const launchRoundStore = new AsyncLocalStorage<LaunchRoundTag>();
/** Per fixture: the original claim's first remediation round, pinned by observation and asserted by the control. */
const LAUNCH_TARGET_ROUND = {
  exhausted: "clone", lateAck: "clone", delayedFeedback: "clone", handoff: "clone",
} as const satisfies Record<string, LaunchRoundTag["round"]>;
const LAUNCH_GATE_CAP_MS = 1000;
const LAUNCH_EXPIRY_WARNING = "remediation restriction read unavailable";

/**
 * The "slow launch" control: the launch-time restriction read returns, then its result is held until the
 * inner bootstrap operation aborts (or a 1000 ms cap), modelling a loaded host. With the 200 ms launch budget
 * that abort lands after the read and the launch round fails; with the 5000 ms launch budget the signal is
 * still live at release and the executor is entered. Only the declared target round's first read with the exact
 * source identity is gated; untagged calls are never gated.
 */
function launchControl(claim: ClaimResponse, target: LaunchRoundTag["round"]) {
  const s = {
    firstTagged: undefined as LaunchRoundTag | undefined, gated: undefined as LaunchRoundTag | undefined,
    readReturned: false, releasedBy: undefined as "abort" | "cap" | undefined,
    liveAtRelease: undefined as boolean | undefined, remainingMs: undefined as number | undefined,
    abortReason: "none", entered: false, frameMissing: false, heldMs: undefined as number | undefined,
    warnings: [] as { message: string; class: unknown; tag: LaunchRoundTag | undefined }[],
    liveAfterRead: undefined as boolean | undefined, frameDeadline: undefined as number | undefined,
    abortAtDate: undefined as number | undefined,
  };
  const logger = nullLogger();
  logger.child = () => logger;
  logger.warn = (message, fields) => {
    if (message === LAUNCH_EXPIRY_WARNING)
      s.warnings.push({ message, class: (fields as { class?: unknown } | undefined)?.class, tag: launchRoundStore.getStore() });
  };
  const facts = () => " [launch control facts: " + JSON.stringify({
    target, firstRound: s.firstTagged, gated: s.gated, readReturned: s.readReturned, releasedBy: s.releasedBy,
    signalLiveAtRelease: s.liveAtRelease, frameMissing: s.frameMissing, heldMs: s.heldMs, remainingMs: s.remainingMs, abortReason: s.abortReason,
    warnings: s.warnings, executorEntered: s.entered,
    liveAfterRead: s.liveAfterRead, frameDeadline: s.frameDeadline, abortAtDate: s.abortAtDate }) + "]";
  const attach = (runner: RunRunner) => {
    let seq = 0;
    const priv = runner as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;
    for (const [round, name] of [["authority", "requireLaunchAuthority"], ["clone", "cloneRemediationRound"]] as const) {
      const real = priv[name]!.bind(runner);
      priv[name] = (...args) => launchRoundStore.run({ round, seq: ++seq }, () => real(...args));
    }
    const target$ = git as unknown as {
      readRemediationRestrictionFacts: (bare: string, branch: string, runId: string) => Promise<unknown>;
      recoveryOperations: AsyncLocalStorage<{ signal: AbortSignal; deadline: number }>;
    };
    const previous = target$.readRemediationRestrictionFacts.bind(git);
    const expected = [git.barePathFor(claim.repo.clone_url), "agent/issue-2396", claim.run_id];
    target$.readRemediationRestrictionFacts = async (bare, branch, runId) => {
      const tag = launchRoundStore.getStore();
      const result = await previous(bare, branch, runId);
      const exact = bare === expected[0] && branch === expected[1] && runId === expected[2];
      if (!tag || !exact) return result;
      s.firstTagged ??= tag;
      if (tag.round !== target || s.gated) return result;
      s.gated = tag;
      s.readReturned = true;
      const frame = target$.recoveryOperations.getStore();
      // Recorded, not asserted here: the runner's bootstrap catch would swallow an assertion thrown in this wrapper.
      if (!frame) { s.frameMissing = true; return result; }
      // Provenance, read straight after the wrapped read returned: the bootstrap signal was still live and its deadline.
      s.liveAfterRead = !frame.signal.aborted;
      s.frameDeadline = frame.deadline;
      const heldFrom = performance.now();
      await new Promise<void>(resolve => {
        const done = (by: "abort" | "cap") => {
          clearTimeout(timer); s.releasedBy ??= by; if (by === "abort") s.abortAtDate ??= REAL_DATE_NOW(); resolve();
        };
        const timer = setTimeout(() => done("cap"), LAUNCH_GATE_CAP_MS);
        if (frame.signal.aborted) done("abort");
        else frame.signal.addEventListener("abort", () => done("abort"), { once: true });
      });
      s.heldMs = performance.now() - heldFrom;
      s.liveAtRelease = !frame.signal.aborted;
      s.remainingMs = frame.deadline - Date.now();
      const reason: unknown = frame.signal.reason;
      s.abortReason = !frame.signal.aborted ? "none"
        : reason instanceof RecoveryDeadlineExhaustedError ? "RecoveryDeadlineExhaustedError"
        : (reason as { constructor?: { name?: string } } | undefined)?.constructor?.name ?? typeof reason;
      return result;
    };
  };
  return {
    logger, attach, facts, markEntered: () => { s.entered = true; },
    assertControl: () => {
      assert.ok(LAUNCH_GATE_CAP_MS > 200, "the hold cap exceeds the 200 ms base launch budget" + facts());
      assert.equal(s.firstTagged?.round, target, "the declared target is the first remediation round" + facts());
      assert.ok(s.gated, "the target round was gated" + facts());
      assert.ok(!s.frameMissing, "the gate runs inside the bootstrap recovery operation" + facts());
      assert.equal(s.liveAfterRead, true, "the launch signal was live when the wrapped read returned" + facts());
      assert.equal(s.releasedBy, "cap", "the gate released by cap, not by abort" + facts());
      // The hold must outlast the 200 ms base launch budget; a wall-clock comparison to the cap itself would flake because
      // the cap timer can read a little short on performance.now(). A cap timer of 0 ms holds ~1-2 ms and fails here.
      assert.ok(s.heldMs! > 200, "the hold outlasted the 200 ms base launch budget" + facts());
      assert.equal(s.liveAtRelease, true, "the launch signal was live at release" + facts());
      assert.ok(s.remainingMs! > 0, "the launch allowance remained at release" + facts());
      assert.equal(s.warnings.length, 0, "no remediation-unavailable warning at all" + facts());
      assert.equal(s.entered, true, "the executor was entered" + facts());
    },
  };
}

function realCapture(prepare: () => void | Promise<void> = () => {}): NonNullable<Executor["withCredentialFreeCapture"]> {
  const rig = codexRig();
  return (deadlineMs, action, purpose, signal) => rig.capture(deadlineMs, async context => {
    await prepare();
    return action(context);
  }, purpose, signal);
}

const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function until(predicate: () => boolean, timeoutMs = 3000, facts?: () => string) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) assert.fail("bounded runner memory fixture wait" + (facts?.() ?? ""));
    await new Promise(resolve => setTimeout(resolve, 2));
  }
}

/** Fixture teardown: awaits each started promise in order (thunks, so a later read sees state the earlier join settled),
 *  recording rejections instead of throwing, so a rejected join cannot skip the joins and monitor stop after it. */
async function joinSettled(steps: (() => unknown)[]): Promise<unknown[]> {
  const errors: unknown[] = [];
  for (const step of steps) { try { await step(); } catch (error) { errors.push(error); } }
  return errors;
}

async function withTimeout<T>(operation: Promise<T>, timeoutMs: number, label: string, facts?: () => string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      operation,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(`timed out waiting for ${label}${facts?.() ?? ""}`)), timeoutMs);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
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
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0, inactive_file: 0 },
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
  claimGeneration?: number;
  workerMemory?: {
    operation?: Promise<void>; settled?: boolean; closing?: Promise<void>; executorRun?: Promise<void>;
    closingDeadline?: number;
    hold?: { deadline: number; context: { claim_generation: number; flight: unknown } };
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

// ---------------------------------------------------------------------------------------------------------------------
// Custody deadline observer (test-only). One helper, installed in the four fixtures whose 200 ms owner budget was split
// from the 5000 ms launch budget. It never changes behaviour: every wrapper runs the real method and only records.
// Violations are collected (a throw inside a wrapped runner path would be swallowed by the runner's own catch) and
// asserted once, after all original work is joined.
// ---------------------------------------------------------------------------------------------------------------------
type ObservedFixture = "exhausted" | "lateAck" | "delayedFeedback" | "handoff";
type InvocationRole = "original" | "successor" | "duplicate";
type AbortSource = "lifecycle" | "parent" | "budget" | "maintenance" | "own" | "unknown";
type CustodyOutcome =
  | { kind: "rejected-before-action" }
  | { kind: "settled-without-abort" }
  | { kind: "abort"; atDate: number; reasonClass: string; source: AbortSource; deadlineClass: boolean };
interface CustodyRecord {
  kind: "hold" | "close"; invocation: InvocationRole | undefined; requested: number;
  parentDeadline: number | undefined; hasBudget: boolean; hasPending: boolean; effectiveDeadline: number;
  entered: boolean; outcome?: CustodyOutcome;
  /** The recoveryOperations frame the hold's action runs in (read inside the wrapped action), by identity. */
  frame?: unknown;
}
/** One RecoveryDeadlineExhaustedError thrown by GitCache.recoveryRemaining, keyed by the error object. */
interface DeadlineThrow { frame: unknown; deadline: number; at: number; budgetLimited: boolean }
type DeadlineThrowLookup = (reason: object) => DeadlineThrow | undefined;
/** Who aborted a capture's signal, classified synchronously during abort dispatch. */
type CaptureOrigin = "caller" | "maintenance" | "lane" | "unknown";
interface CaptureRecord {
  signal: AbortSignal; deadlineAt: number; caller: AbortSignal | undefined; seq0?: number;
  /** The maintenance-budget signal visible where the capture was requested. */
  budgetSignal?: AbortSignal;
  /** The capture was opened under the original flight's preserve call. */
  originalFlight?: boolean;
  /** The hold operation active where the capture was requested (wrapper scope, inside that operation's action). */
  custody?: CustodyRecord;
  /** The capture action's return value settled (or it threw synchronously). */
  settled?: boolean;
  /** The typed provenance of the lane's rejection, when it recorded one. */
  failure?: ReturnType<typeof captureFailureOf>;
  abortedAt?: number; reason?: unknown;
  /** The action had not yet returned when the abort was dispatched. */
  pendingAtAbort?: boolean; origin?: CaptureOrigin;
  /** What the capture action rejected with (or threw synchronously), by identity. */
  actionReason?: unknown;
}
interface HoldOpEvidence { outcome?: CustodyOutcome; effectiveDeadline: number }
interface CaptureEvidence {
  originalFlight?: boolean; custody?: { kind: string; frame?: unknown; effectiveDeadline?: number };
  failure?: { deadlineExpired: boolean };
  pendingAtAbort?: boolean; abortedAt?: number; deadlineAt: number; origin?: CaptureOrigin;
  actionReason?: unknown;
}
/** A preserveWorkerMemory call refused before any custody operation started. */
interface PreOpRefusal { entryAt: number; deadline: number; clean: boolean; dirty: string[] }
type RefusalClass = "expired" | "straddle" | "dirty";
interface PortRecord { aborted: boolean; reason: unknown; before?: { aborted: boolean; reason: unknown } }
interface HeldLike { context?: unknown; signal?: AbortSignal; deadlineAt: number }
interface SiteTag { site: string; held: HeldLike | undefined; flight: unknown }

/**
 * Held fetch-wait sites each fixture is expected to bracket, pinned from candidate runs; the listed sites were confirmed
 * against a base-budget run (the four budget sites reverted). A listed site that ends up uncaptured fails.
 */
const EXPECTED_HELD_SITES: Record<string, readonly string[]> = {
  "exhausted:accepted": ["captureRecoveryRestorePointBody"],
  "exhausted:ambiguous": ["captureRecoveryRestorePointBody"],
  "exhausted:deadline": ["captureRecoveryRestorePointBody"],
  // No held fetch wait is created on these paths: the generation verdict fails before capture, late-ACK's capture stops at
  // the WIP marker, delayed feedback has no capture lane and the handoff capture never reaches a fetch.
  "exhausted:generation": [], "lateAck:lateAck": [], "delayedFeedback:delayedFeedback": [],
  "handoff:unsettled": [], "handoff:expired": [],
};

const REAL_DATE_NOW = Date.now;
const MIXED_CUSTODY_MSG = "delayed feedback saw both a hold custody operation and a pre-operation refusal";
const PRESSURE_PARK_NONCE = "original-memory-registration";
const isThenable = (value: unknown): value is PromiseLike<unknown> =>
  (typeof value === "object" || typeof value === "function") && value !== null && typeof (value as { then?: unknown }).then === "function";
const isDeadlineReason =(reason: unknown) => reason instanceof RecoveryDeadlineExhaustedError;
const isAbortErrorReason = (reason: unknown) => reason instanceof DOMException && reason.name === "AbortError";
const abortOriginOf = (reason: unknown, caller: AbortSignal | undefined, budgetSignal: AbortSignal | undefined): CaptureOrigin =>
  caller?.aborted && reason === caller.reason ? "caller"
  : budgetSignal?.aborted && reason === budgetSignal.reason ? "maintenance"
  : isAbortErrorReason(reason) ? "lane" : "unknown";
const isDeadlineAbort = (op: HoldOpEvidence) => op.outcome?.kind === "abort" && op.outcome.deadlineClass &&
  op.outcome.atDate >= op.effectiveDeadline - 1;
/** A typed lane capture-deadline abort: the lane's own expiry, while the action was pending, inside a hold operation. */
const isCaptureAbort = (c: CaptureEvidence) => c.originalFlight === true && c.custody?.kind === "hold" &&
  c.failure?.deadlineExpired === true && c.pendingAtAbort === true &&
  c.abortedAt !== undefined && c.abortedAt >= c.deadlineAt - 1 && c.origin === "lane" &&
  // The lane's own deadline can sit before the hold's (a maintenance budget pulls it earlier): bind to the hold's too.
  c.custody.effectiveDeadline !== undefined && c.abortedAt >= c.custody.effectiveDeadline - 1;
/**
 * The hold's own deadline check: the capture action rejected with the very error object GitCache.recoveryRemaining threw
 * inside that hold's recoveryOperations frame, on the hold's effective deadline, not budget-limited, at or past that
 * deadline. Bound by object identity (the throw record is looked up by the error), not by any settle timestamp.
 */
const isHoldDeadlineCheck = (c: CaptureEvidence, lookup: DeadlineThrowLookup) => {
  if (c.originalFlight !== true || c.custody?.kind !== "hold") return false;
  const reason = c.actionReason;
  if (typeof reason !== "object" || reason === null) return false;
  const rec = lookup(reason);
  return rec !== undefined && rec.frame !== undefined && rec.frame === c.custody.frame &&
    c.custody.effectiveDeadline !== undefined && rec.deadline === c.custody.effectiveDeadline &&
    rec.budgetLimited === false && rec.at >= c.custody.effectiveDeadline;
};
/** The accepted evidence for an exhausted-deadline run, naming the disjunct that carried it. */
function exhaustedDeadlineEvidence(holdOps: readonly HoldOpEvidence[], captures: readonly CaptureEvidence[],
  lookup: DeadlineThrowLookup = () => undefined): "deadlineAbort" | "captureAbort" | "holdDeadlineCheck" | undefined {
  if (holdOps.some(isDeadlineAbort)) return "deadlineAbort";
  if (captures.some(isCaptureAbort)) return "captureAbort";
  if (captures.some(c => isHoldDeadlineCheck(c, lookup))) return "holdDeadlineCheck";
  return undefined;
}
/** Accepted only when no non-deadline predicate was already true at entry and the entry itself was at or past the deadline. */
function classifyPreOpRefusal(r: { entryAt: number; deadline: number; clean: boolean }): RefusalClass {
  return !r.clean ? "dirty" : r.entryAt >= r.deadline ? "expired" : "straddle";
}
const reasonClassOf = (reason: unknown) => isDeadlineReason(reason) ? "RecoveryDeadlineExhaustedError"
  : (reason as { constructor?: { name?: string } } | undefined)?.constructor?.name ?? typeof reason;

function observeCustody(f: Awaited<ReturnType<typeof setup>>, fixture: ObservedFixture, scenario: string) {
  const invocationStore = new AsyncLocalStorage<InvocationRole>();
  const flightStore = new AsyncLocalStorage<{ flight: MemoryFlightProof; custody?: CustodyRecord }>();
  const callerStore = new AsyncLocalStorage<"sink" | "reconcile">();
  const siteStore = new AsyncLocalStorage<SiteTag>();
  const violations: string[] = [];
  const fail = (message: string) => { if (violations.length < 40) violations.push(message); };
  let original: MemoryFlightProof | undefined;
  let boundaryMs = 5000;
  const begins: { flight: unknown; generation: unknown }[] = [];
  const starts: { flight: unknown; caller: string; firstCall: boolean; operationSet: boolean }[] = [];
  const preserves: { flight: unknown; promise: Promise<unknown>; store: { custody?: CustodyRecord } }[] = [];
  const custody: CustodyRecord[] = [];
  const preOpRefusals: PreOpRefusal[] = [];
  const captures: CaptureRecord[] = [];
  const capturePromises: Promise<unknown>[] = [];
  const t0BySignal = new Map<AbortSignal, number>();
  const seq0BySignal = new Map<AbortSignal, number>();
  /** Exact f(v) witnesses are searched in assertAll so observeWait does no work proportional to the clock log. */
  const pendingWitnesses: { site: string; timeoutMs: number; t0: number; t1: number; seq0: number; seq1: number; deadlineAt: number }[] = [];
  const waits = new WeakMap<object, { timeoutMs: unknown; signal: unknown; site: string }>();
  const reached = new Map<string, number>();
  const captured = new Map<string, number>();
  const uncaptured: string[] = [];
  const ownerCalls: { name: string; flight: unknown }[] = [];
  const ports: { park?: PortRecord; send?: PortRecord } = {};
  let foreignWaits = 0;
  const clock = { log: [] as { value: number; seq: number }[], logging: false };
  const patchedNow = () => {
    const value = REAL_DATE_NOW();
    if (clock.logging) clock.log.push({ value, seq: clock.log.length });
    return value;
  };

  // Mark the caller of startWorkerMemoryPreservation: the preserve sink is frozen at begin, so it is replaced there.
  // Every patch this observer installs on a live object records a restore closure here; join() runs them in reverse order.
  // A property that was inherited from the prototype is restored by deleting the own property, else by reassigning.
  const restores: (() => void)[] = [];
  const patchProp = (target: object, key: string, value: unknown) => {
    const t = target as Record<string, unknown>;
    const hadOwn = Object.prototype.hasOwnProperty.call(target, key);
    const previous = t[key];
    t[key] = value;
    restores.push(() => { if (hadOwn) t[key] = previous; else delete t[key]; });
  };
  const begin = f.memory.begin.bind(f.memory);
  patchProp(f.memory, "begin", ((input: Parameters<typeof begin>[0], sinks: Parameters<typeof begin>[1]) => {
    const flight = (input as { flight?: unknown }).flight;
    begins.push({ flight, generation: input.claim_generation });
    original ??= flight as MemoryFlightProof;
    return begin(input, { ...sinks,
      preserve: (context, signal, remainingMs) => callerStore.run("sink", () => sinks.preserve(context, signal, remainingMs)) });
  }) as typeof f.memory.begin);

  const g = git as unknown as {
    withRecoveryOperation: (...args: any[]) => Promise<unknown>;
    recoveryOperations: AsyncLocalStorage<{ signal: AbortSignal; deadline: number; budget?: MaintenanceBudget }>;
    recoveryRemaining: (operation: { signal: AbortSignal; deadline: number; budget?: MaintenanceBudget }) => number;
  };
  // Records every RecoveryDeadlineExhaustedError that GitCache.recoveryRemaining throws, keyed by the error object. The
  // wrapper is synchronous, rethrows the same error and returns the real value unchanged; callers use this.recoveryRemaining,
  // so the instance patch sees them.
  const deadlineThrows = new WeakMap<object, DeadlineThrow>();
  const deadlineThrowOf: DeadlineThrowLookup = reason => deadlineThrows.get(reason);
  // Options objects returned by an attached runner's owedOptions, keyed by identity to the flight they were built for.
  const optsFlight = new WeakMap<object, unknown>();
  type GitOwed = Record<"retainCurrentOwedCandidate" | "owedTipRetained", ((...args: any[]) => unknown) | undefined>;
  const owedNames = ["retainCurrentOwedCandidate", "owedTipRetained"] as const;
  const owedPatched: Record<string, "patched" | "absent"> = {};
  // The shared-git patches install on the first attach(), inside the fixture's try, so join() always restores them.
  let sharedInstalled = false;
  const installShared = () => {
  if (sharedInstalled) return;
  sharedInstalled = true;
  const originalRemaining = g.recoveryRemaining;
  patchProp(g, "recoveryRemaining", function (this: unknown, operation: Parameters<typeof g.recoveryRemaining>[0]) {
    try { return originalRemaining.call(this, operation); }
    catch (error) {
      if (error instanceof RecoveryDeadlineExhaustedError) {
        // One "now" and one budget read for both fields. Only the explicit remaining <= 0 branch counts: an error that is the
        // aborted signal's own reason was rethrown by throwIfAborted, and the first record per error object is never replaced.
        const now = REAL_DATE_NOW();
        const budgetRemaining = operation.budget?.remainingMs() ?? Infinity;
        const rethrown = operation.signal.aborted && error === operation.signal.reason;
        if (!rethrown && !deadlineThrows.has(error))
          // recoveryRemaining floors min(deadline - now, budget), so a sub-millisecond budget remainder is budget-limited.
          deadlineThrows.set(error, { frame: g.recoveryOperations.getStore(), deadline: operation.deadline, at: now,
            budgetLimited: Math.floor(budgetRemaining) <= 0 || budgetRemaining < operation.deadline - now });
      }
      throw error;
    }
  });
  const realRecovery = g.withRecoveryOperation.bind(git);
  patchProp(g, "withRecoveryOperation", (signal: AbortSignal, deadline: number, action: (signal: AbortSignal) => Promise<unknown>,
    ...rest: unknown[]) => {
    const memory = original?.workerMemory;
    const store = flightStore.getStore();
    let kind: CustodyRecord["kind"] | undefined;
    if (original && store) {
      if (store.flight === original && !store.custody) {
        kind = "hold";
        if (!memory?.hold) fail("a custody operation started without a hold");
        else if (deadline !== memory.hold.deadline) fail(`custody requested deadline ${deadline} !== hold.deadline ${memory.hold.deadline}`);
      }
    } else if (original && memory && invocationStore.getStore() === "original" && custody.some(c => c.kind === "hold")) {
      // Tag-based: after the flight-tagged hold started, original-invocation work without a flight tag is close work.
      if (memory.closingDeadline !== undefined && deadline === memory.closingDeadline) kind = "close";
      else fail(`custody work reached withRecoveryOperation with the original invocation tag but no flight tag, requested deadline ${deadline}`);
    }
    if (!kind) return realRecovery(signal, deadline, action, ...rest);
    const parent = g.recoveryOperations.getStore();
    const budget = parent?.budget ?? MaintenanceBudget.current() ?? (rest[0] as MaintenanceBudget | undefined);
    const pending = MaintenanceBudget.signal();
    const record: CustodyRecord = { kind, invocation: invocationStore.getStore(), requested: deadline,
      parentDeadline: parent?.deadline, hasBudget: MaintenanceBudget.current() !== undefined, hasPending: pending !== undefined,
      effectiveDeadline: Math.min(deadline, parent?.deadline ?? Infinity), entered: false };
    custody.push(record);
    if (store) store.custody = record;
    const settle = () => {
      record.outcome ??= record.entered ? { kind: "settled-without-abort" } : { kind: "rejected-before-action" };
    };
    // Non-perturbing: handlers attached below only mark the production promises as handled; production awaits them all,
    // and the exact production promise/value is returned.
    const wrapped = (combined: AbortSignal) => {
      record.entered = true;
      // withRecoveryOperation runs this action inside recoveryOperations.run(frame, ...), so this is the hold's own frame.
      record.frame = g.recoveryOperations.getStore();
      const onAbort = () => {
        if (record.outcome) return;
        const reason = combined.reason;
        const source: AbortSource = signal.aborted && reason === signal.reason ? "lifecycle"
          : parent?.signal.aborted && reason === parent.signal.reason ? "parent"
          : budget?.ownsExpiry(reason) === true ? "budget"
          : pending?.aborted && reason === pending.reason ? "maintenance"
          : isDeadlineReason(reason) ? "own" : "unknown";
        record.outcome = { kind: "abort", atDate: REAL_DATE_NOW(), reasonClass: reasonClassOf(reason), source,
          deadlineClass: source === "own" || (source === "parent" && isDeadlineReason(reason)) };
      };
      if (combined.aborted) onAbort(); else combined.addEventListener("abort", onAbort, { once: true });
      const cleanup = () => combined.removeEventListener("abort", onAbort);
      let result: unknown;
      try { result = action(combined); } catch (error) { cleanup(); throw error; }
      if (isThenable(result)) result.then(cleanup, cleanup); else cleanup();
      return result;
    };
    const p = realRecovery(signal, deadline, wrapped as never, ...rest);
    if (!isThenable(p)) { settle(); return p; }
    p.then(settle, settle);
    return p;
  });
  // Direct owner-retention seams on the shared git: synchronous, attributed through the options object the attached
  // runner's owedOptions produced, and returning the real value unchanged.
  const gOwed = git as unknown as GitOwed;
  for (const name of owedNames) {
    const real = gOwed[name];
    // A renamed seam would make the zero-call check vacuous: fail loudly, as the runner seams do.
    if (typeof real !== "function") { owedPatched[name] = "absent"; fail(`shared Git seam ${name} is absent`); continue; }
    owedPatched[name] = "patched";
    patchProp(git, name, function (this: unknown, ...args: any[]) {
      const key = args[1];
      const known = typeof key === "object" && key !== null && optsFlight.has(key);
      if (!known) fail(`unattributed owed call ${name}`);
      else ownerCalls.push({ name, flight: optsFlight.get(key) });
      return real.apply(this, args);
    });
  }
  };

  const portRecord = (signal: AbortSignal): PortRecord => {
    const record: PortRecord = { aborted: signal.aborted, reason: signal.aborted ? signal.reason : undefined };
    if (!signal.aborted) signal.addEventListener("abort", () => { record.aborted = true; record.reason = signal.reason; }, { once: true });
    return record;
  };

  const observeWait = (wait: { timeoutMs?: unknown; signal?: AbortSignal }) => {
    const t1 = REAL_DATE_NOW();
    const prior = waits.get(wait);
    if (prior) {
      if (prior.timeoutMs !== wait.timeoutMs || prior.signal !== wait.signal || prior.site !== siteStore.getStore()?.site)
        fail("a forwarded wait object changed its timeoutMs, signal or site");
      return;
    }
    const tag = siteStore.getStore();
    if (!tag || tag.flight !== original || !tag.held) return;
    // Only tagged original-flight held waits are remembered, so an untagged first sighting cannot suppress a later check.
    waits.set(wait, { timeoutMs: wait.timeoutMs, signal: wait.signal, site: tag.site });
    if (wait.signal !== tag.held.signal) { foreignWaits++; return; }
    const timeoutMs = wait.timeoutMs;
    if (typeof timeoutMs !== "number") { fail(`${tag.site}: held wait has no numeric timeoutMs`); return; }
    const model = (t: number) => Math.max(1, Math.min(boundaryMs, tag.held!.deadlineAt - t));
    if (!Number.isFinite(tag.held.deadlineAt)) {
      if (timeoutMs !== boundaryMs) fail(`${tag.site}: unbounded held capture must wait exactly ${boundaryMs}, got ${timeoutMs}`);
      else captured.set(tag.site, (captured.get(tag.site) ?? 0) + 1);
      return;
    }
    // Resolution limit: the bracket and the exact-f(v) witness cannot distinguish a timeoutMs deviation smaller than
    // this fixture's [t0, t1] width (about 30-50 ms observed), since some recorded Date.now in the window may match.
    const t0 = t0BySignal.get(wait.signal!);
    if (t0 === undefined) { uncaptured.push(`${tag.site}: no capture-entry t0 for the held signal`); return; }
    if (!(model(t1) <= timeoutMs && timeoutMs <= model(t0)))
      fail(`${tag.site}: timeoutMs ${timeoutMs} outside [f(t1)=${model(t1)}, f(t0)=${model(t0)}]`);
    if (!(timeoutMs >= 1 && timeoutMs <= boundaryMs)) fail(`${tag.site}: timeoutMs ${timeoutMs} violates floor 1 / clamp ${boundaryMs}`);
    pendingWitnesses.push({ site: tag.site, timeoutMs, t0, t1, seq0: seq0BySignal.get(wait.signal!) ?? 0, seq1: clock.log.length,
      deadlineAt: tag.held.deadlineAt });
    captured.set(tag.site, (captured.get(tag.site) ?? 0) + 1);
  };

  const disjunctOf = () => fixture === "exhausted" && scenario === "deadline"
    ? exhaustedDeadlineEvidence(custody.filter(c => c.kind === "hold"), captures, deadlineThrowOf) : undefined;
  /** The recorded recoveryRemaining throw behind a capture's action rejection, with frame equality as a boolean. */
  const throwFacts = (c: CaptureRecord) => {
    const rec = typeof c.actionReason === "object" && c.actionReason !== null ? deadlineThrowOf(c.actionReason) : undefined;
    return rec && { frameMatchesHold: rec.frame !== undefined && rec.frame === c.custody?.frame, deadline: rec.deadline, at: rec.at,
      budgetLimited: rec.budgetLimited };
  };
  const facts = () => " [custody observer facts: " + JSON.stringify({
    fixture, scenario, violations,
    starts: starts.map(s => ({ original: s.flight === original, caller: s.caller, firstCall: s.firstCall, operationSet: s.operationSet })),
    custody: custody.map(({ frame, ...c }) => ({ ...c, hasFrame: frame !== undefined, outcome: c.outcome ?? "none" })),
    captures: captures.map(c => ({ deadlineAt: c.deadlineAt, abortedAt: c.abortedAt, aborted: c.signal.aborted,
      origin: c.origin, pendingAtAbort: c.pendingAtAbort, reason: c.reason === undefined ? undefined : reasonClassOf(c.reason),
      failure: c.failure, custodyKind: c.custody?.kind, originalFlight: c.originalFlight,
      actionReason: c.actionReason === undefined ? undefined : reasonClassOf(c.actionReason),
      actionReasonIsRecordedThrow: typeof c.actionReason === "object" && c.actionReason !== null && deadlineThrowOf(c.actionReason) !== undefined,
      deadlineThrow: throwFacts(c) })),
    preOpRefusals: preOpRefusals.map(r => ({ ...r, class: classifyPreOpRefusal(r) })),
    disjunct: disjunctOf(),
    ports: { park: ports.park && { ...ports.park, reason: reasonClassOf(ports.park.reason),
        before: ports.park.before && { ...ports.park.before, reason: reasonClassOf(ports.park.before.reason) } },
      send: ports.send && { ...ports.send, reason: reasonClassOf(ports.send.reason),
        before: ports.send.before && { ...ports.send.before, reason: reasonClassOf(ports.send.before.reason) } } },
    reachedSites: [...reached], capturedSites: [...captured], uncaptured, foreignWaits,
    ownerCalls: ownerCalls.map(c => c.name), owedPatched }) + "]";

  return {
    facts,
    invoke<T>(role: InvocationRole, run: () => Promise<T>): Promise<T> { return invocationStore.run(role, run); },
    /** Attributes the runner's owedOptions results to their flight; the synchronous wrapper returns the identical promise. */
    attachOwed(runner: RunRunner) {
      const priv = runner as unknown as Record<string, (...args: any[]) => any>;
      const real = priv.owedOptions;
      assert.equal(typeof real, "function", "runner private seam owedOptions exists");
      const bound = real!.bind(runner);
      patchProp(runner, "owedOptions", (flight: unknown, ...rest: unknown[]) => {
        const result = bound(flight, ...rest);
        if (isThenable(result)) result.then((opts: unknown) => {
          if (typeof opts === "object" && opts !== null) optsFlight.set(opts, flight);
        }, () => {});
        return result;
      });
    },
    /** Wraps one runner instance. Must run before that runner's execute. */
    attach(runner: RunRunner, executor: Executor) {
      installShared();
      this.attachOwed(runner);
      if ((executor as { safety?: unknown }).safety !== undefined) fail("the fixture executor carries .safety, so a held capture could be authenticated");
      boundaryMs = (runner as unknown as { codexBoundaryDeadlineMs: number }).codexBoundaryDeadlineMs;
      const priv = runner as unknown as Record<string, (...args: any[]) => any>;
      const wrap = (name: string, make: (real: (...args: any[]) => any) => (...args: any[]) => any) => {
        const real = priv[name];
        assert.equal(typeof real, "function", `runner private seam ${name} exists`);
        patchProp(runner, name, make(real!.bind(runner)));
      };
      wrap("startWorkerMemoryPreservation", real => (flight: MemoryFlightProof) => {
        const firstCall = flight.workerMemory?.operation === undefined;
        const caller = callerStore.getStore() ?? "other";
        const result = real(flight);
        starts.push({ flight, caller, firstCall, operationSet: flight.workerMemory?.operation !== undefined });
        return result;
      });
      wrap("reconcileWorkerMemory", real => (flight: unknown) => callerStore.run("reconcile", () => real(flight)));
      wrap("preserveWorkerMemory", real => (flight: MemoryFlightProof, hold: unknown) => {
        const store: { flight: MemoryFlightProof; custody?: CustodyRecord } = { flight };
        // Same synchronous turn as the call: the entry time and every non-deadline predicate of the runner's pre-operation
        // identity() / retainedTerminalGuard, all side-effect-free reads.
        const entryAt = REAL_DATE_NOW();
        const dirty: string[] = [];
        const deadline = (hold as { deadline?: number } | undefined)?.deadline;
        if (flight === original) {
          const f2 = flight as any, r2 = runner as any, mem = f2.workerMemory, ctx = (hold as any)?.context;
          const read = (name: string, test: () => boolean) => { try { if (test()) dirty.push(name); } catch (error) { dirty.push(`${name} threw ${String(error)}`); } };
          // Fail closed: a field a predicate below reads that is missing or of the wrong type (a renamed field) is dirty, not clean.
          const need = (name: string, ok: () => boolean) => { try { if (!ok()) dirty.push(`unreadable:${name}`); } catch { dirty.push(`unreadable:${name}`); } };
          const isFn = (value: unknown) => typeof value === "function";
          need("runner.shuttingDownGlobal", () => typeof r2.shuttingDownGlobal === "boolean");
          need("runner.shutdownSignal", () => typeof r2.shutdownSignal.signal.aborted === "boolean");
          need("runner.client", () => typeof r2.client === "object" && r2.client !== null && "memoryIncarnation" in r2.client);
          for (const method of ["claimFence", "isCancelled", "terminalLifecycleSignal", "pendingCredentialSwitch", "getPauseMode"])
            need(`flight.steering.${method}`, () => isFn(f2.steering[method]));
          need("flight.claimGeneration", () => typeof f2.claimGeneration === "number");
          need("flight.workerMemory", () => typeof mem === "object" && mem !== null && typeof mem.ended === "boolean");
          if (typeof deadline !== "number") dirty.push("hold carries no numeric deadline");
          read("flight shuttingDown", () => f2.active?.shuttingDown === true);
          read("global shutdown", () => r2.shuttingDownGlobal === true);
          read("shutdown signal", () => r2.shutdownSignal.signal.aborted === true);
          read("claim fence", () => f2.steering.claimFence() !== undefined);
          read("cancelled", () => f2.steering.isCancelled() === true);
          read("terminal lifecycle", () => f2.steering.terminalLifecycleSignal().aborted === true);
          read("memory ended", () => mem?.ended === true);
          read("memory incarnation", () => r2.client.memoryIncarnation !== mem?.incarnation);
          read("context flight", () => ctx?.flight !== flight);
          read("claim generation", () => ctx?.claim_generation !== f2.claimGeneration);
          read("pending credential switch", () => f2.steering.pendingCredentialSwitch() !== undefined);
          read("pause mode", () => f2.steering.getPauseMode() !== null);
        }
        const promise = flightStore.run(store, () => real(flight, hold)) as Promise<unknown>;
        preserves.push({ flight, promise, store });
        // The async function ran synchronously through identity(): no custody operation means a pre-operation refusal.
        if (flight === original && !store.custody)
          preOpRefusals.push({ entryAt, deadline: typeof deadline === "number" ? deadline : NaN, clean: dirty.length === 0, dirty });
        return promise;
      });
      const sites: [string, (args: any[]) => { flight: unknown; held: HeldLike | undefined; authenticated: unknown }][] = [
        ["transferRestorePointToTrustedBare", a => ({ flight: a[0], held: a[4], authenticated: undefined })],
        ["captureRecoveryRestorePointBody", a => ({ flight: a[1], held: a[4]?.heldCapture, authenticated: a[4]?.heldAuthenticated })],
        ["captureHoldContext", a => ({ flight: a[1], held: a[3]?.heldCapture, authenticated: a[3]?.heldAuthenticated })],
      ];
      for (const [site, read] of sites) wrap(site, real => (...args: any[]) => {
        const { flight, held, authenticated } = read(args);
        if (flight === original) {
          if (authenticated !== undefined) fail(`${site}: a heldAuthenticated capture reached a held-fetch site`);
          if (held) reached.set(site, (reached.get(site) ?? 0) + 1);
          const store = flightStore.getStore();
          if (store && store.flight !== flight) fail(`${site}: flight argument differs from the flight-tagged original`);
        }
        return siteStore.run({ site, held, flight }, () => real(...args));
      });
      wrap("fetchTracking", real => (...args: any[]) => {
        const wait = args[6];
        if (typeof wait === "object" && wait !== null) {
          observeWait(wait);
          const tag = siteStore.getStore();
          if (tag && tag.flight === original && args[0] !== original) fail("fetchTracking flight argument is not the original flight");
        }
        return real(...args);
      });
      wrap("refusalGateInAuthority", real => (...args: any[]) => {
        const wait = args[5];
        if (typeof wait === "object" && wait !== null) observeWait(wait);
        return real(...args);
      });
      for (const [name, flightIndex] of [["stopForOwedLimit", 0], ["owedStopReleaseWait", 0], ["retainAcceptedOwnerPause", 1],
        ["handlePausePark", 1]] as const)
        wrap(name, real => (...args: any[]) => { ownerCalls.push({ name, flight: args[flightIndex] }); return real(...args); });
    },
    /** Wrap the original executor's capture lane: t0 per held signal, plus the capture-deadline abort record. */
    wrapCapture(inner: NonNullable<Executor["withCredentialFreeCapture"]>): NonNullable<Executor["withCredentialFreeCapture"]> {
      const wrapped = (deadlineMs: number, action: (context: CredentialFreeCaptureContext) => Promise<unknown>,
        purpose?: "capture" | "cancel", signal?: AbortSignal, attempt?: unknown) => {
        // The record exists in wrapper scope: the run is created before the callback runs (queue wait). The capture is requested
        // synchronously from inside the hold operation's action, so the flight store's custody read here is that operation
        // (a callback-time read could see a later operation on the same flight).
        const record = { caller: signal, budgetSignal: MaintenanceBudget.signal(),
          custody: flightStore.getStore()?.custody } as CaptureRecord;
        // Non-perturbing: the callback is synchronous and returns the exact production value; handlers attached to it only
        // mark it handled, and production awaits it.
        const run = inner(deadlineMs, context => {
          t0BySignal.set(context.signal, REAL_DATE_NOW());
          record.signal = context.signal;
          record.deadlineAt = context.deadlineAt;
          record.originalFlight = original !== undefined && flightStore.getStore()?.flight === original;
          record.seq0 = clock.log.length;
          seq0BySignal.set(context.signal, record.seq0);
          captures.push(record);
          // Synchronous during abort dispatch: nothing here awaits.
          const note = () => {
            record.abortedAt ??= REAL_DATE_NOW();
            record.reason = context.signal.reason;
            record.pendingAtAbort = !record.settled;
            record.origin = abortOriginOf(record.reason, record.caller, record.budgetSignal);
          };
          if (context.signal.aborted) note();
          else context.signal.addEventListener("abort", note, { once: true });
          const mark = () => { record.settled = true; };
          const markRejected = (error: unknown) => { record.actionReason = error; mark(); };
          let result: Promise<unknown>;
          try { result = action(context); } catch (error) { record.actionReason = error; record.settled = true; throw error; }
          if (isThenable(result)) result.then(mark, markRejected); else mark();
          return result;
        }, purpose, signal, attempt as never);
        // Records the typed failure without changing what is returned.
        if (isThenable(run)) void run.then(undefined, error => { record.failure = captureFailureOf(error); });
        capturePromises.push(run);
        return run;
      };
      return wrapped as unknown as NonNullable<Executor["withCredentialFreeCapture"]>;
    },
    captureRecords: (): readonly CaptureRecord[] => captures,
    custodyRecords: (): readonly CustodyRecord[] => custody,
    preOpRefusalRecords: (): readonly PreOpRefusal[] => preOpRefusals,
    deadlineThrowOf,
    startClock() {
      clock.log.length = 0; clock.logging = true;
      if (Date.now !== patchedNow) {
        const previous = Date.now;
        Date.now = patchedNow;
        restores.push(() => { if (Date.now === patchedNow) Date.now = previous; });
      }
    },
    /** Fixture-port abort records: plain listeners, attached on the first correlated call only, no clock reads. */
    observePark(args: Parameters<WorkerClient["reportState"]>) {
      const body = args[1] as unknown as Record<string, unknown>;
      if (args[0] !== f.claim.run_id || body.recovery_cause !== "worker_memory_pressure" || body.claim_generation !== 7 ||
          body.register_nonce !== PRESSURE_PARK_NONCE || body.memory_episode !== 3) return;
      if (ports.park) { fail("a second correlated park call reached the client"); return; }
      if (args[2] === undefined) { fail("the correlated park call carried no signal"); return; }
      ports.park = portRecord(args[2]);
    },
    observeSend(args: Parameters<WorkerClient["postMessages"]>) {
      if (args[0] !== f.claim.run_id || args[2] !== 7) return;
      if (ports.send) { fail("a second correlated pressure post reached the client"); return; }
      if (args[3] === undefined) { fail("the correlated pressure post carried no signal"); return; }
      ports.send = portRecord(args[3]);
    },
    /** Called straight after the fixture's existing race / await of the original work; only synchronous code follows. */
    snapshotPort(which: "park" | "send") {
      const record = ports[which];
      assert.ok(record, `${which} port was observed` + facts());
      record.before = { aborted: record.aborted, reason: record.reason };
      assert.equal(record.before.aborted, true, `${which} signal aborted before the original work resolved` + facts());
    },
    /** Late-ACK: the original flight keeps generation 7 while the successor (generation 8) is admitted. */
    assertOverlap(successor: MemoryFlightProof) {
      assert.ok(original, "original flight observed" + facts());
      assert.notEqual(successor, original, "successor is a different flight" + facts());
      assert.equal(original.claimGeneration, 7, "original flight stays generation 7 during the overlap" + facts());
      assert.equal(successor.claimGeneration, 8, "successor flight is generation 8" + facts());
      assert.equal(original.workerMemory?.hold?.context.claim_generation, 7, "original hold context stays generation 7" + facts());
      assert.notEqual(original.workerMemory?.settled, true, "original capture is still blocked during the overlap" + facts());
    },
    /** Join every piece of original work, then stop the clock recorder. Call at the end of the fixture's outer finally. */
    async join() {
      try {
        const memory = original?.workerMemory;
        await Promise.allSettled([memory?.operation, memory?.closing, memory?.executorRun,
          ...preserves.map(entry => entry.promise), ...capturePromises,
          ...f.memory.sinkReceipts.filter(receipt => receipt.context.flight === original).map(receipt => receipt.completion)]);
      } finally {
        clock.logging = false;
        // Undo every patch (Date.now, the shared Git seams, each attached runner's instance seams including the late-ACK
        // successor's owedOptions, and f.memory.begin) in strict reverse installation order. No fixture calls a wrapped seam
        // after join(); assertAll only reads records.
        // Each closure is a plain assignment or delete, so none can throw and one cannot block the rest.
        for (let restore = restores.pop(); restore; restore = restores.pop()) restore();
      }
    },
    /** All assertions; call only after join(). */
    assertAll() {
      const failures = [...violations];
      for (const w of pendingWitnesses) {
        const model = (t: number) => Math.max(1, Math.min(boundaryMs, w.deadlineAt - t));
        let found = false;
        for (let i = w.seq0; i < w.seq1 && i < clock.log.length; i++) {
          const entry = clock.log[i]!;
          if (entry.value >= w.t0 && entry.value <= w.t1 && model(entry.value) === w.timeoutMs) { found = true; break; }
        }
        if (!found) failures.push(`${w.site}: timeoutMs ${w.timeoutMs} equals f(v) for no recorded Date.now value in [${w.t0}, ${w.t1}]`);
      }
      const memory = original?.workerMemory;
      if (!original) failures.push("no original flight was begun");
      const firsts = starts.filter(s => s.flight === original && s.firstCall);
      if (firsts.length !== 1) failures.push(`expected exactly one first preservation call for the original flight, saw ${firsts.length}`);
      else {
        if (!firsts[0]!.operationSet) failures.push("the first preservation call did not set the owner operation");
        const wantCaller = fixture === "handoff" ? "sink" : "reconcile";
        if (firsts[0]!.caller !== wantCaller)
          failures.push(`the ${fixture} fixture is first preserved by ${wantCaller}, saw ${firsts[0]!.caller}`);
      }
      if (foreignWaits !== 0) failures.push(`${foreignWaits} held-site wait(s) carried a signal other than the held capture signal`);
      if (original) {
        if (original.claimGeneration !== 7) failures.push(`original flight generation ${original.claimGeneration}`);
        if (memory?.hold?.context.claim_generation !== 7) failures.push("original hold context is not generation 7");
        if (memory?.hold && memory.hold.context.flight !== original) failures.push("original hold context is bound to another flight");
      }
      for (const state of api.states) {
        const body = state.body as unknown as Record<string, unknown>;
        if (body.recovery_cause === "worker_memory_pressure" && body.claim_generation !== 7)
          failures.push(`a memory park was reported under generation ${String(body.claim_generation)}`);
      }
      for (const call of ownerCalls) if (call.flight === original) failures.push(`${call.name} ran for the original flight`);
      const holdOps = custody.filter(c => c.kind === "hold");
      // Close work is classified only by its closingDeadline, so its deadline is not separately asserted here; no
      // fixture observed a close operation reach withRecoveryOperation (the close path runs through the batcher).
      for (const op of custody) if (op.outcome === undefined) failures.push("a custody operation recorded no outcome");
      const heldCase = fixture === "lateAck" || fixture === "delayedFeedback" || (fixture === "exhausted" && scenario === "deadline");
      if (heldCase) for (const op of holdOps) {
        if (op.outcome?.kind === "abort" && !op.outcome.deadlineClass)
          failures.push(`a custody operation aborted from ${op.outcome.source} (${op.outcome.reasonClass}), not its deadline`);
      }
      // Late-ACK requires the record on every hold operation. Delayed feedback has no capture lane (runFreeCapture reports
      // "incomplete" at once), so its custody operation settles without an abort: an abort is checked when one occurs.
      // No upper bound is placed on a custody operation.
      for (const op of holdOps) {
        const required = fixture === "lateAck" || (fixture === "delayedFeedback" && op.outcome?.kind === "abort");
        if (required && !isDeadlineAbort(op))
          failures.push(`a custody operation has no deadline-class abort at or after its effective deadline (${JSON.stringify(op.outcome)})`);
      }
      if (fixture === "lateAck" && holdOps.length === 0) failures.push("late-ACK recorded no hold custody operation");
      const refusalClasses = preOpRefusals.map(classifyPreOpRefusal);
      if (fixture === "delayedFeedback") {
        // Either exactly one started hold operation, or exactly one pre-operation refusal that was clean and entered at or
        // after the hold deadline (reconcile reached preserveWorkerMemory after the hold expired).
        preOpRefusals.forEach((r, i) => {
          if (refusalClasses[i] === "straddle")
            failures.push(`a pre-operation refusal was entered before the hold deadline (entry ${r.entryAt} < deadline ${r.deadline})`);
          if (refusalClasses[i] === "dirty")
            failures.push(`a pre-operation refusal had a non-deadline predicate set at entry: ${JSON.stringify(r.dirty)}`);
        });
        if (holdOps.length > 0 && preOpRefusals.length > 0) failures.push(MIXED_CUSTODY_MSG);
        const expiredRefusal = holdOps.length === 0 && preOpRefusals.length === 1 && refusalClasses[0] === "expired";
        if (expiredRefusal) { /* accepted: no custody operation started */ }
        else if (holdOps.length !== 1)
          failures.push(`delayed feedback expected exactly one hold custody operation or one expired pre-operation refusal, saw ${holdOps.length} operation(s) and ${JSON.stringify(preOpRefusals)}`);
        else for (const op of holdOps) {
          if (!op.entered) failures.push("the delayed-feedback hold custody operation never entered its action");
          if (op.outcome?.kind === "rejected-before-action") failures.push("the delayed-feedback hold custody operation was rejected before action");
        }
      }
      if (heldCase) for (const c of captures) {
        if (c.originalFlight === true && (c.origin === "maintenance" || c.origin === "unknown"))
          failures.push(`an original-flight capture was aborted by ${c.origin} (${reasonClassOf(c.reason)}, pendingAtAbort ${String(c.pendingAtAbort)}), not by its lane deadline`);
      }
      // Settle-before-teardown through the real adapter: an abort of a capture with no typed failure is the lane's teardown
      // after the action settled, so it can never be a pending abort.
      if (fixture === "exhausted" && (scenario === "accepted" || scenario === "ambiguous")) for (const c of captures) {
        if (c.failure === undefined && c.signal.aborted && c.pendingAtAbort !== false)
          failures.push(`a capture with no typed failure was aborted while its action was pending (${c.origin}, ${reasonClassOf(c.reason)})`);
      }
      const disjunct = disjunctOf();
      if (fixture === "exhausted" && scenario === "deadline" && disjunct === undefined)
        failures.push("no deadline-class custody abort, typed lane capture-deadline abort or hold deadline check was recorded");
      const accepted = (reason: unknown) => isDeadlineReason(reason) || isAbortErrorReason(reason);
      if (ports.park && !accepted(ports.park.before?.reason)) failures.push(`park signal aborted with ${reasonClassOf(ports.park.before?.reason)}`);
      if (ports.send && !accepted(ports.send.before?.reason)) failures.push(`send signal aborted with ${reasonClassOf(ports.send.before?.reason)}`);
      const expected = EXPECTED_HELD_SITES[`${fixture}:${scenario}`] ?? [];
      for (const site of expected) if (!captured.has(site)) failures.push(`expected held fetch site ${site} was not captured`);
      assert.deepEqual(failures, [], "custody observer" + facts());
      console.log("custody-witness " + JSON.stringify({ fixture, scenario, disjunct: disjunct ?? null,
        captures: captures.map(c => ({ origin: c.origin ?? null, pendingAtAbort: c.pendingAtAbort ?? null,
          deadlineExpired: c.failure?.deadlineExpired ?? null, originalFlight: c.originalFlight === true,
          actionReasonIsRecordedThrow: typeof c.actionReason === "object" && c.actionReason !== null && deadlineThrowOf(c.actionReason) !== undefined,
          deadlineThrow: throwFacts(c) ?? null })),
        refusal: refusalClasses[0] ?? null, holdOps: holdOps.length }));
    },
  };
}

// ---------------------------------------------------------------------------------------------------------------------
// Probes of the custody oracles. Small synthetic inputs: each pins one accept or reject decision.
// ---------------------------------------------------------------------------------------------------------------------
const probeHold = (outcome: CustodyOutcome, extra: object = {}) => ({ kind: "hold" as const, effectiveDeadline: 100, outcome, ...extra });
const probeCapture = (extra: object = {}) => ({ originalFlight: true, custody: { kind: "hold", effectiveDeadline: 100 }, failure: { deadlineExpired: true },
  pendingAtAbort: true, abortedAt: 100, deadlineAt: 100, origin: "lane" as CaptureOrigin, ...extra });

test("custody oracle rejects an action that rejected early with a late continuation and no abort", () => {
  // Owner repro: the action rejected at t=0 and its continuation ran at t=200; nothing aborted, no typed failure.
  // The extra fields (settleAtDate, returnedAt) are exactly the facts the removed form (c) accepted: a hold operation that
  // settled without abort at or after its effective deadline, and an original-flight hold-custody capture whose action
  // returned at or after its deadline. The current oracle reads neither, so it must still reject.
  const hold = probeHold({ kind: "settled-without-abort" }, { settleAtDate: 100 + 5 });
  const capture = probeCapture({ failure: undefined, pendingAtAbort: false, abortedAt: undefined, origin: undefined,
    settled: true, returnedAt: 100 + 5 });
  assert.equal(exhaustedDeadlineEvidence([hold], [capture]), undefined);
});

test("custody oracle accepts a typed lane capture-deadline abort alone", () => {
  assert.equal(exhaustedDeadlineEvidence([probeHold({ kind: "settled-without-abort" })], [probeCapture()]), "captureAbort");
});

test("custody oracle accepts a deadline-class hold abort alone", () => {
  const hold = probeHold({ kind: "abort", atDate: 100, reasonClass: "RecoveryDeadlineExhaustedError", source: "own", deadlineClass: true });
  assert.equal(exhaustedDeadlineEvidence([hold], []), "deadlineAbort");
});

test("custody oracle rejects every near-miss capture abort", () => {
  const hold = probeHold({ kind: "settled-without-abort" });
  const cases: [string, object][] = [
    ["maintenance origin", { origin: "maintenance" }],
    ["caller abort with a typed failure", { origin: "caller" }],
    ["unknown origin", { origin: "unknown" }],
    ["deadlineExpired with pendingAtAbort false", { pendingAtAbort: false }],
    ["non-hold custody record", { custody: { kind: "close", effectiveDeadline: 100 } }],
    // Lane abort exactly at the lane's own deadline, but before the hold's effective deadline (a budget pulled the lane earlier).
    ["lane abort at the lane deadline but before the hold's effective deadline", { custody: { kind: "hold", effectiveDeadline: 150 } }],
    ["hold custody without an effective deadline", { custody: { kind: "hold" } }],
    ["no custody record", { custody: undefined }],
    ["failure not deadline-expired", { failure: { deadlineExpired: false } }],
    ["no typed failure", { failure: undefined }],
    ["abort before the capture deadline", { abortedAt: 98 }],
    ["not the original flight", { originalFlight: false }],
  ];
  for (const [label, extra] of cases)
    assert.equal(exhaustedDeadlineEvidence([hold], [probeCapture(extra)]), undefined, label);
});

// holdDeadlineCheck: the hold's own recoveryRemaining throw, bound to the capture's rejection by object identity.
const HOLD_FRAME = { frame: "hold" };
const holdCheckRig = (throwExtra: Partial<DeadlineThrow> = {}, captureExtra: object = {}, record = true) => {
  const reason = new RecoveryDeadlineExhaustedError();
  const rec: DeadlineThrow = { frame: HOLD_FRAME, deadline: 100, at: 100, budgetLimited: false, ...throwExtra };
  const lookup: DeadlineThrowLookup = r => (record && r === reason ? rec : undefined);
  const capture = { originalFlight: true, custody: { kind: "hold", frame: HOLD_FRAME, effectiveDeadline: 100 }, deadlineAt: 100,
    actionReason: reason, ...captureExtra };
  return { hold: probeHold({ kind: "settled-without-abort" }), capture, lookup };
};

test("custody oracle accepts the hold's own deadline check bound by identity", () => {
  const { hold, capture, lookup } = holdCheckRig();
  assert.equal(exhaustedDeadlineEvidence([hold], [capture], lookup), "holdDeadlineCheck");
  const later = holdCheckRig({ at: 250 });
  assert.equal(exhaustedDeadlineEvidence([later.hold], [later.capture], later.lookup), "holdDeadlineCheck");
});

test("custody oracle rejects every near-miss hold deadline check", () => {
  const cases: [string, Partial<DeadlineThrow>, object, boolean][] = [
    ["RDEE from a foreign frame", { frame: { frame: "other" } }, {}, true],
    ["throw without a frame", { frame: undefined }, { custody: { kind: "hold", frame: undefined, effectiveDeadline: 100 } }, true],
    ["budget-limited throw", { budgetLimited: true }, {}, true],
    ["RDEE never seen by recoveryRemaining", {}, {}, false],
    ["correct identity but thrown before the effective deadline", { at: 99 }, {}, true],
    ["throw deadline differs from the effective deadline", { deadline: 90 }, {}, true],
    ["not the original flight", {}, { originalFlight: false }, true],
    ["non-hold custody", {}, { custody: { kind: "close", frame: HOLD_FRAME, effectiveDeadline: 100 } }, true],
    ["a non-object rejection", {}, { actionReason: "x" }, true],
  ];
  for (const [label, throwExtra, captureExtra, record] of cases) {
    const { hold, capture, lookup } = holdCheckRig(throwExtra, captureExtra, record);
    assert.equal(exhaustedDeadlineEvidence([hold], [capture], lookup), undefined, label);
  }
});

test("pre-operation refusal: an entry before the deadline is a straddle, not expiry", () => {
  assert.equal(classifyPreOpRefusal({ entryAt: 950, deadline: 1000, clean: true }), "straddle");
});

test("pre-operation refusal: a straddle is never expiry whichever later timestamp is available", () => {
  for (const entryAt of [999, 990, 500])
    assert.equal(classifyPreOpRefusal({ entryAt, deadline: 1000, clean: true }), "straddle", `entry ${entryAt}`);
});

test("pre-operation refusal: a clean entry at or after the deadline is expiry", () => {
  for (const entryAt of [1000, 1001, 5000]) assert.equal(classifyPreOpRefusal({ entryAt, deadline: 1000, clean: true }), "expired");
});

test("pre-operation refusal: a non-deadline predicate set at entry is dirty even after the deadline", () => {
  // e.g. pause mode set before the entry, with the deadline already passed: the deadline did not cause the refusal.
  assert.equal(classifyPreOpRefusal({ entryAt: 1500, deadline: 1000, clean: false }), "dirty");
});

// The real wrapCapture, fed a scripted lane: the lane owns its own controller, calls the callback and aborts as scripted.
type ProbeAction = (context: CredentialFreeCaptureContext) => Promise<unknown>;
const pendingUntilAbort: ProbeAction = context => new Promise<never>((_, reject) => {
  context.signal.addEventListener("abort", () => reject(context.signal.reason), { once: true });
});
/** `script` runs after the lane called the callback and may abort the lane's controller (or an outer signal). */
async function probeWrapCapture(action: ProbeAction, script: (ctl: AbortController) => void,
  scope: (call: () => Promise<unknown>) => Promise<unknown> = call => call(), caller?: AbortSignal) {
  const f = await setup();
  const obs = observeCustody(f, "exhausted", "probe");
  try {
    const lane = async (deadlineMs: number, cb: ProbeAction, _purpose?: string, signal?: AbortSignal) => {
      const ctl = new AbortController();
      const outer = [signal, MaintenanceBudget.signal()].filter((x): x is AbortSignal => x !== undefined);
      if (outer.length) {
        const any = AbortSignal.any(outer);
        const forward = () => ctl.abort(any.reason);
        if (any.aborted) forward(); else any.addEventListener("abort", forward, { once: true });
      }
      try {
        const running = cb({ signal: ctl.signal, deadlineAt: Date.now() + deadlineMs } as CredentialFreeCaptureContext);
        script(ctl);
        return await running;
      } finally { ctl.abort(); }
    };
    const wrapped = obs.wrapCapture(lane as never);
    await scope(() => wrapped(100, action, "capture", caller).then(() => undefined, () => undefined));
    return obs.captureRecords();
  } finally { try { await f.memory.stop(); } finally { await obs.join(); } }
}

test("wrapCapture records pendingAtAbort false when the action threw synchronously before the teardown abort", async () => {
  const [record, ...rest] = await probeWrapCapture(() => { throw new Error("sync"); }, () => {});
  assert.equal(rest.length, 0);
  assert.equal(record!.pendingAtAbort, false);
  assert.equal(record!.origin, "lane");
});

test("wrapCapture records pendingAtAbort false when the action rejected early before the teardown abort", async () => {
  const [record] = await probeWrapCapture(async () => { throw new Error("early"); }, () => {});
  assert.equal(record!.pendingAtAbort, false);
});

test("wrapCapture classifies a pending action aborted by the lane's own controller as lane origin", async () => {
  const [record] = await probeWrapCapture(pendingUntilAbort, ctl => { ctl.abort(); });
  assert.equal(record!.pendingAtAbort, true);
  assert.equal(record!.origin, "lane");
});

test("wrapCapture classifies an abort carrying the caller signal's reason as caller origin", async () => {
  const caller = new AbortController();
  const [record] = await probeWrapCapture(pendingUntilAbort, () => { caller.abort(new Error("caller reason")); },
    call => call(), caller.signal);
  assert.equal(record!.pendingAtAbort, true);
  assert.equal(record!.origin, "caller");
});

test("wrapCapture classifies an abort carrying the maintenance budget's reason as maintenance origin", async () => {
  const budget = new MaintenanceBudget(5000);
  try {
    const [record] = await probeWrapCapture(pendingUntilAbort,
      () => { (budget as unknown as { expire(): void }).expire(); },
      call => MaintenanceBudget.run(budget, call));
    assert.equal(record!.pendingAtAbort, true);
    assert.equal(record!.origin, "maintenance");
  } finally { budget.dispose(); }
});

test("wrapCapture classifies an abort with a foreign reason as unknown origin", async () => {
  const caller = new AbortController();
  const [record] = await probeWrapCapture(pendingUntilAbort, ctl => { ctl.abort(new Error("foreign")); },
    call => call(), caller.signal);
  assert.equal(record!.origin, "unknown");
});

test("wrapCapture records no typed failure for a plain rejection and no custody outside a hold operation", async () => {
  const [record] = await probeWrapCapture(async () => { throw new Error("plain"); }, () => {});
  assert.equal(record!.failure, undefined, "an untyped rejection records no failure");
  assert.equal(record!.custody, undefined, "no hold operation was active");
});

/**
 * Runs one capture inside a real hold operation through the patched recoveryRemaining seam. `makeOperation` runs when the
 * capture starts (before the hold's deadline) and returns the operation handed to recoveryRemaining once the deadline has
 * passed, inside the hold's frame; the capture rejects with whatever recoveryRemaining throws.
 */
async function probeHoldThrow(makeOperation: (holdDeadline: number) => { signal: AbortSignal; budget?: object }) {
  const f = await setup();
  const obs = observeCustody(f, "exhausted", "probe");
  try {
    // The wall-clock allowance starts only after all setup (flight, observer, runner double, attach): it is assigned
    // immediately before preserveWorkerMemory, so a paused process during setup cannot expire it.
    let holdDeadline = Number.NaN;
    const HOLD_ALLOWANCE_MS = 100, WAIT_CAP_MS = 10_000;
    const flight = { workerMemory: { hold: { deadline: holdDeadline } } };
    // The observer tags the original flight at begin; the real begin rejects this synthetic input before doing any work.
    try { f.memory.begin({ flight, claim_generation: 7 } as never, {} as never); } catch { /* expected: invalid sinks */ }
    const lane = (_deadlineMs: number, cb: ProbeAction) =>
      cb({ signal: new AbortController().signal, deadlineAt: holdDeadline } as CredentialFreeCaptureContext);
    const capture = obs.wrapCapture(lane as never);
    // A runner double carrying only the seams attach wraps; preserveWorkerMemory opens the real hold operation.
    const none = () => undefined;
    const runner = { codexBoundaryDeadlineMs: 5000, startWorkerMemoryPreservation: none, reconcileWorkerMemory: none,
      transferRestorePointToTrustedBare: none, captureRecoveryRestorePointBody: none, captureHoldContext: none, fetchTracking: none,
      refusalGateInAuthority: none, stopForOwedLimit: none, owedStopReleaseWait: none, retainAcceptedOwnerPause: none,
      handlePausePark: none, owedOptions: none,
      preserveWorkerMemory: (_flight: unknown, hold: { deadline: number }) =>
        git.withRecoveryOperation(new AbortController().signal, hold.deadline, async () =>
          capture(5000, async () => {
            const operation = makeOperation(holdDeadline);
            // Wait until the real clock has passed the deadline (short timers, bounded by a generous cap).
            const giveUp = Date.now() + WAIT_CAP_MS;
            while (Date.now() <= holdDeadline && Date.now() < giveUp) await new Promise(resolve => setTimeout(resolve, 5));
            assert.ok(Date.now() > holdDeadline, "the real clock passed the hold deadline");
            // The hold's own deadline check, run inside the hold's frame.
            (git as unknown as { recoveryRemaining(op: object): number }).recoveryRemaining({ ...operation, deadline: holdDeadline });
            return "unreachable";
          }, "capture")) } as unknown as RunRunner;
    obs.attach(runner, {} as Executor);
    // Setup is done: start the allowance now, immediately before the hold operation opens.
    holdDeadline = Date.now() + HOLD_ALLOWANCE_MS;
    flight.workerMemory.hold.deadline = holdDeadline;
    await assert.rejects((runner as unknown as { preserveWorkerMemory(...a: unknown[]): Promise<unknown> })
      .preserveWorkerMemory(flight, { deadline: holdDeadline }), RecoveryDeadlineExhaustedError);
    const [record, ...rest] = obs.captureRecords();
    assert.equal(rest.length, 0);
    assert.equal(record!.originalFlight, true, obs.facts());
    assert.equal(record!.custody?.kind, "hold", obs.facts());
    assert.notEqual(record!.custody?.frame, undefined, "the wrapper recorded the hold's frame");
    return { record: record!, thrown: obs.deadlineThrowOf(record!.actionReason as object),
      evidence: exhaustedDeadlineEvidence([], obs.captureRecords(), obs.deadlineThrowOf), facts: obs.facts() };
  } finally { try { await f.memory.stop(); } finally { await obs.join(); } }
}

/**
 * Observer-level refusal probe, through the real attach()-installed preserveWorkerMemory wrapper and the delayedFeedback
 * assertAll. The runner double refuses before any custody operation (it never calls withRecoveryOperation) and settles only
 * after the hold deadline, logging a late "warn" timestamp: the classifier must see the entry sample, not that late one.
 * Returns the assertAll failure list (the double lacks the rest of the fixture, so assertAll always has unrelated failures).
 */
async function probeRefusal(entryBeforeDeadline: boolean, tweak: (flight: any) => void = () => {}) {
  const f = await setup();
  const obs = observeCustody(f, "delayedFeedback", "delayedFeedback");
  let failures: string[] = [], facts = "", warnAt = NaN, deadline = NaN;
  try {
    const signal = new AbortController().signal;
    const flight: any = { claimGeneration: 7, workerMemory: { ended: false, incarnation: 1, hold: undefined },
      steering: { claimFence: () => undefined, isCancelled: () => false, terminalLifecycleSignal: () => signal,
        pendingCredentialSwitch: () => undefined, getPauseMode: () => null } };
    try { f.memory.begin({ flight, claim_generation: 7 } as never, {} as never); } catch { /* expected: invalid sinks */ }
    const none = () => undefined;
    const runner = { codexBoundaryDeadlineMs: 5000, shuttingDownGlobal: false, shutdownSignal: { signal }, client: { memoryIncarnation: 1 },
      startWorkerMemoryPreservation: none, reconcileWorkerMemory: none, transferRestorePointToTrustedBare: none,
      captureRecoveryRestorePointBody: none, captureHoldContext: none, fetchTracking: none, refusalGateInAuthority: none,
      stopForOwedLimit: none, owedStopReleaseWait: none, retainAcceptedOwnerPause: none, handlePausePark: none, owedOptions: none,
      preserveWorkerMemory: async (_flight: unknown, hold: { deadline: number }) => {
        const giveUp = Date.now() + 10_000;
        while (Date.now() <= hold.deadline && Date.now() < giveUp) await new Promise(resolve => setTimeout(resolve, 5));
        warnAt = Date.now(); // the late "warn" observation
        return "refused";
      } } as unknown as RunRunner;
    obs.attach(runner, {} as Executor);
    tweak(flight);
    deadline = entryBeforeDeadline ? Date.now() + 60 : Date.now() - 5;
    const hold = { deadline, context: { flight, claim_generation: 7 } };
    flight.workerMemory.hold = hold;
    await (runner as unknown as { preserveWorkerMemory(...a: unknown[]): Promise<unknown> }).preserveWorkerMemory(flight, hold);
  } finally { try { await f.memory.stop(); } finally { await obs.join(); } }
  facts = obs.facts();
  try { obs.assertAll(); } catch (error) { failures = ((error as { actual?: unknown }).actual as string[] | undefined) ?? [String(error)]; }
  const recorded = obs.preOpRefusalRecords();
  assert.equal(recorded.length, 1, "exactly one pre-operation refusal was recorded");
  // The wrapper's own entry sample and deadline, not the probe's.
  return { failures, facts, warnAt, deadline: recorded[0]!.deadline, entryAt: recorded[0]!.entryAt };
}

test("refusal at the observer level: an entry before the deadline is a straddle even though the refusal settled after it", async () => {
  const entries: string[] = [];
  for (let attempt = 0; attempt < 5; attempt++) {
    const { failures, facts, warnAt, deadline, entryAt } = await probeRefusal(true);
    // Retry only when the recorded entry missed the 60 ms margin (a stalled process), never because of the class.
    if (entryAt >= deadline) { entries.push(`${entryAt}>=${deadline}`); continue; }
    assert.ok(warnAt > deadline, "late warn after the deadline");
    assert.ok(failures.some(m => /entered before the hold deadline/.test(m)), JSON.stringify(failures) + facts);
    assert.ok(facts.includes('"class":"straddle"'), facts);
    return;
  }
  assert.fail(`straddle precondition not reached in 5 attempts (recorded entries ${entries.join(", ")})`);
});

test("refusal at the observer level: a clean entry after the deadline is accepted as expiry", async () => {
  const { failures, facts, entryAt, deadline } = await probeRefusal(false);
  assert.ok(entryAt >= deadline);
  assert.ok(facts.includes('"class":"expired"'), facts);
  assert.ok(!failures.includes(MIXED_CUSTODY_MSG), JSON.stringify(failures));
  assert.deepEqual(failures.filter(m => /pre-operation refusal|delayed feedback expected/.test(m)), [], JSON.stringify(failures));
});

test("refusal at the observer level: a hold custody operation plus an expired refusal is rejected", async () => {
  const f = await setup();
  const obs = observeCustody(f, "delayedFeedback", "delayedFeedback");
  try {
    const signal = new AbortController().signal;
    const flight: any = { claimGeneration: 7, workerMemory: { ended: false, incarnation: 1, hold: undefined },
      steering: { claimFence: () => undefined, isCancelled: () => false, terminalLifecycleSignal: () => signal,
        pendingCredentialSwitch: () => undefined, getPauseMode: () => null } };
    try { f.memory.begin({ flight, claim_generation: 7 } as never, {} as never); } catch { /* expected: invalid sinks */ }
    const none = () => undefined;
    let calls = 0;
    const runner = { codexBoundaryDeadlineMs: 5000, shuttingDownGlobal: false, shutdownSignal: { signal }, client: { memoryIncarnation: 1 },
      startWorkerMemoryPreservation: none, reconcileWorkerMemory: none, transferRestorePointToTrustedBare: none,
      captureRecoveryRestorePointBody: none, captureHoldContext: none, fetchTracking: none, refusalGateInAuthority: none,
      stopForOwedLimit: none, owedStopReleaseWait: none, retainAcceptedOwnerPause: none, handlePausePark: none, owedOptions: none,
      preserveWorkerMemory: (_flight: unknown, hold: { deadline: number }) => {
        calls++;
        if (calls > 1) return Promise.resolve("refused");
        // Synchronously, before any await: the hold custody operation with exactly the hold's deadline.
        return git.withRecoveryOperation(new AbortController().signal, hold.deadline, async () => "ok");
      } } as unknown as RunRunner;
    obs.attach(runner, {} as Executor);
    const hold = { deadline: Date.now() + 150, context: { flight, claim_generation: 7 } };
    flight.workerMemory.hold = hold;
    const preserve = (runner as unknown as { preserveWorkerMemory(...a: unknown[]): Promise<unknown> }).preserveWorkerMemory.bind(runner);
    assert.equal(await preserve(flight, hold), "ok");
    const giveUp = Date.now() + 10_000;
    while (Date.now() <= hold.deadline && Date.now() < giveUp) await new Promise(resolve => setTimeout(resolve, 5));
    assert.ok(Date.now() > hold.deadline, "the real clock passed the hold deadline");
    assert.equal(await preserve(flight, hold), "refused");
  } finally { try { await f.memory.stop(); } finally { await obs.join(); } }
  const holds = obs.custodyRecords().filter(c => c.kind === "hold");
  assert.equal(holds.length, 1, "exactly one hold custody record");
  assert.equal(holds[0]!.entered, true);
  assert.equal(holds[0]!.outcome?.kind, "settled-without-abort", JSON.stringify(holds[0]!.outcome));
  const refusals = obs.preOpRefusalRecords();
  assert.equal(refusals.length, 1, "exactly one pre-operation refusal");
  assert.equal(refusals[0]!.clean, true, JSON.stringify(refusals[0]));
  assert.ok(obs.facts().includes('"class":"expired"'), obs.facts());
  let failures: string[] = [];
  try { obs.assertAll(); } catch (error) { failures = ((error as { actual?: unknown }).actual as string[] | undefined) ?? [String(error)]; }
  assert.ok(failures.includes(MIXED_CUSTODY_MSG), JSON.stringify(failures));
});

test("refusal at the observer level: a non-deadline predicate set at entry is dirty even after the deadline", async () => {
  const { failures, facts, entryAt, deadline } = await probeRefusal(false, flight => { flight.steering.getPauseMode = () => "pause"; });
  assert.ok(entryAt >= deadline, `recorded entry ${entryAt} is at or after the deadline ${deadline}`);
  assert.ok(facts.includes('"class":"dirty"'), facts);
  assert.ok(failures.some(m => /non-deadline predicate set at entry.*pause mode/.test(m)), JSON.stringify(failures));
});

test("wrapCapture records the lane's typed failure on the capture record", async () => {
  const f = await setup();
  const obs = observeCustody(f, "exhausted", "probe");
  try {
    const err = new Error("typed lane failure");
    const lane = async (_deadlineMs: number, cb: ProbeAction) => {
      void cb({ signal: new AbortController().signal, deadlineAt: Date.now() + 100 } as CredentialFreeCaptureContext);
      recordCaptureFailure(err, { deadlineExpired: true, childCleanupFailed: false, externallyAborted: false });
      throw err;
    };
    const wrapped = obs.wrapCapture(lane as never);
    await assert.rejects(wrapped(100, async () => "ok", "capture"), error => error === err);
    const [record, ...rest] = obs.captureRecords();
    assert.equal(rest.length, 0);
    assert.equal(record!.failure?.deadlineExpired, true, "the typed failure the lane recorded for the rejection");
  } finally { try { await f.memory.stop(); } finally { await obs.join(); } }
});

test("holdDeadlineCheck: a capture rejecting with the hold's own recoveryRemaining throw is bound by identity through the real seams", async () => {
  // A fresh signal keeps the abort-signal check out of recoveryRemaining.
  const { record, thrown, evidence, facts } = await probeHoldThrow(() => ({ signal: new AbortController().signal }));
  assert.ok(thrown, "recoveryRemaining recorded the very error object the action rejected with" + facts);
  assert.equal(thrown.frame, record.custody?.frame, "the throw happened in the hold's own frame");
  assert.equal(thrown.budgetLimited, false);
  assert.equal(evidence, "holdDeadlineCheck", facts);
});

test("holdDeadlineCheck: an RDEE created before the deadline and rethrown by an aborted signal is not the hold's own check", async () => {
  // The reviewer's case: the error object was minted ~37 ms before the deadline; recoveryRemaining only rethrows it through
  // operation.signal.throwIfAborted() after the deadline, so no deadline check of the hold's own produced it.
  const { thrown, evidence, facts } = await probeHoldThrow(() => {
    const ctl = new AbortController();
    ctl.abort(new RecoveryDeadlineExhaustedError());
    return { signal: ctl.signal };
  });
  assert.equal(thrown, undefined, "a rethrown signal reason is not recorded as a recoveryRemaining deadline throw" + facts);
  assert.equal(evidence, undefined, facts);
});

test("holdDeadlineCheck: a sub-millisecond budget remainder counts as budget-limited and is not accepted", async () => {
  const { thrown, evidence, facts } = await probeHoldThrow(() => ({ signal: new AbortController().signal,
    budget: { check() {}, remainingMs: () => 0.5 } }));
  assert.ok(thrown, "the explicit remaining <= 0 branch is still recorded" + facts);
  assert.equal(thrown.budgetLimited, true, facts);
  assert.equal(evidence, undefined, facts);
});

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

test("runner status feed carries memory_diagnostics only when the port supplies it; the stored result never does", async () => {
  const f = await setup();
  let executorError: unknown;
  const executor: Executor = { run: async ctx => {
    try {
      const captured = ctx.capturedMemory!()!;
      const sinks = (f.memory as unknown as { sinks: WeakMap<object, { prepareFeedback: (c: unknown, r: unknown, d?: unknown) => void; feedback: (r: unknown, c: unknown, s: AbortSignal, ms: number) => Promise<void> }> }).sinks;
      const sink = sinks.get(captured.context)!;
      const result = (id: string) => ({ code: "worker_memory_pressure", claim_generation: 7, intervention_id: id });
      sink.prepareFeedback(captured.context, result("with-diagnostics"), { version: 1, unavailable: "error" });
      sink.prepareFeedback(captured.context, result("without-diagnostics"));
      const stored = (captured.flight as unknown as { workerMemory: { feedback: Map<string, { result: object }> } }).workerMemory.feedback;
      assert.equal(stored.size, 2);
      for (const { result: kept } of stored.values()) assert.equal("memory_diagnostics" in kept, false, "stored result is unchanged");
      // Deliver through the flight's own flush so the status feed reaches the API.
      await sink.feedback(result("with-diagnostics"), captured.context, new AbortController().signal, 1000);
    } catch (error) { executorError = error; throw error; }
    throw new Error("fixture completed its proof");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    const feed = api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure");
    const byId = new Map(feed.map(m => [m.payload.intervention_id, m.payload]));
    assert.deepEqual(byId.get("with-diagnostics")?.memory_diagnostics, { version: 1, unavailable: "error" });
    assert.equal("memory_diagnostics" in byId.get("without-diagnostics")!, false);
    assert.equal(byId.size, 2);
  } finally { await f.memory.stop(); }
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

const exhaustedCases = [
  ...(["empty", "capture_failure", "ambiguous", "rotated", "deadline"] as const)
    .map(verdict => ({ verdict, slowRemediationMs: 0 })),
  { verdict: "deadline" as const, slowRemediationMs: 230 },
];
for (const { verdict, slowRemediationMs } of exhaustedCases) {
  test(`runner exhausted memory ${verdict}${slowRemediationMs ? " (slow remediation control)" : ""}: retained owner hold keeps clone/session and never reports model failure`, async t => {
    if (slowRemediationMs) {
      // A loaded host can make the pre-executor clone remediation read slow; it must not consume the narrow hold budget.
      const target = git as unknown as { remediationRestrictionFactsUnderLock: (...args: unknown[]) => Promise<unknown> };
      const real = target.remediationRestrictionFactsUnderLock.bind(target);
      t.mock.method(target, "remediationRestrictionFactsUnderLock", async (...args: unknown[]) => {
        await new Promise(resolve => setTimeout(resolve, slowRemediationMs));
        return real(...args);
      });
    }
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
    let teardownStartedAt: number | undefined;
    let rescueTimer: ReturnType<typeof setTimeout> | undefined;
    fs.writeFileSync(homeDir + "/session-proof", "original session");
    const executor: Executor = {
      withCredentialFreeCapture: realCapture(async () => {
        const operation = captureScope.getStore();
        assert.ok(operation, "owner settlement runs inside a bounded Git recovery operation");
        operation.ownerCapture = true;
        prepared = true;
        assert.equal(fs.existsSync(homeDir + "/session-proof"), true);
      }),
      run: async ctx => {
        // The 1500 ms oracle bounds the memory teardown under the narrow hold budget. Launch remediation before the
        // executor now runs under the generous boundary budget (and the slow-remediation control delays it), so the
        // teardown clock starts at executor entry, before the pressure trigger.
        teardownStartedAt = performance.now();
        // Rescue the unfixed transport so RED is an elapsed assertion, never the test timeout. It is armed on the
        // same clock as the 1500 ms oracle, so slow launch work cannot shrink the 2000 ms vs 1500 ms margin.
        if (verdict === "deadline") rescueTimer = setTimeout(() => transportRescue.abort(), transportRescueAfterMs);
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
      const started = performance.now();
      await runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory,
          // Only the memory hold is narrowed: launch remediation rounds keep the generous boundary budget.
          codexBoundaryDeadlineMs: positiveCaptureBudgetMs,
          ...(verdict === "deadline" ? { memoryHoldDeadlineMs: deadlineBudgetMs } : {}) }).execute(f.claim);
      if (verdict === "deadline") assert.ok(performance.now() - (teardownStartedAt ?? started) < deadlineElapsedBoundMs,
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

const initiallyExhaustedCases = [
  ...(["accepted", "ambiguous", "generation", "deadline"] as const).map(verdict => ({ verdict, slowLaunch: false })),
  { verdict: "deadline" as const, slowLaunch: true },
];
for (const { verdict, slowLaunch } of initiallyExhaustedCases) {
  test(`runner initially exhausted allowance ${verdict}: first accessor stops before model admission and retains owner custody${slowLaunch ? " (slow launch control)" : ""}`, async () => {
    const f = await setup(2);
    const obs = observeCustody(f, "exhausted", verdict);
    const launch = slowLaunch ? launchControl(f.claim, LAUNCH_TARGET_ROUND.exhausted) : undefined;
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
      withCredentialFreeCapture: obs.wrapCapture(realCapture()),
      run: async ctx => {
        launch?.markEntered();
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
      const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, launch?.logger,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000,
          ...(verdict === "deadline" ? { memoryHoldDeadlineMs: 200 } : {}) });
      launch?.attach(runner);
      obs.attach(runner, executor);
      obs.startClock();
      await obs.invoke("original", () => runner.execute(f.claim));
      if (executorError) throw executorError;
      assert.equal(proof, true, "actual RunRunner executor reached the first trusted accessor" + (launch?.facts() ?? ""));
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
    } finally { try { await f.memory.stop(); } finally { await obs.join(); } }
    obs.assertAll();
    launch?.assertControl();
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

function memoryOutbox(root: string, fail = false, gate?: (context: { kind: string }) => Promise<void>) {
  return new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 128 * 1024 * 1024, retentionMs: 86400000,
    rawWrite: async (write, context) => {
      if (fail && context.kind === "segment") throw new Error("fixture disk failure");
      await gate?.(context);
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

// Real fsync'd outbox spill can exceed a 200 ms cleanup budget under CI load (#3681).
const memoryCloseBudgetMs = 5000;

// Probe: expiry timers are captured instead of armed, and a held tail-segment write is released only after a
// SIMULATED elapsed point (not a real delay) so the budget outcome is deterministic.
// slowPreCloseMs injects a loaded-host interleaving: the executor reaches close() that long after the hold, and
// every non-segment outbox write is slowed by the same amount.
type TailProbe = { simulatedMs: number; expectDurable: boolean; slowPreCloseMs?: number };

async function runMemoryCloseTail(fail: boolean, budget: number, probe?: TailProbe) {
  const root = memoryOutboxRoot();
  const timers: { ms: number; controller: AbortController }[] = [];
  let gateHits = 0, reached!: () => void, release!: () => void;
  const gateReached = new Promise<void>(resolve => { reached = resolve; });
  const gateOpen = new Promise<void>(resolve => { release = resolve; });
  const outbox = memoryOutbox(root, fail, probe ? async context => {
    if (context.kind !== "segment") {
      if (probe.slowPreCloseMs) await new Promise(resolve => setTimeout(resolve, probe.slowPreCloseMs));
      return;
    }
    gateHits++;
    reached();
    await gateOpen;
  } : undefined);
  await outbox.init();
  const f = await setup(1);
  const monitoring = f.memory.run();
  let original: { workerMemory?: { feedback: Map<string, { operation?: Promise<boolean> }>;
    tailPreservation?: Promise<boolean>; tailDurable?: boolean; closing?: Promise<void> };
    retainedLocalCustody?: boolean; batcher: MessageBatcher } | undefined;
  let runner: ReturnType<typeof runnerWith> | undefined;
  const executor: Executor = { run: async ctx => {
    const command = enroll(ctx);
    original = command.captured.flight as typeof original;
    client.postMessages = async () => { throw new Error("offline before spill threshold"); };
    f.pressure(950);
    await command.captured.commands.watchOutcome(command.entry);
    if (probe?.slowPreCloseMs) await new Promise(resolve => setTimeout(resolve, probe.slowPreCloseMs));
    // The feedback/capture signal may already be expired: cleanup gets its own budget.
    const closeOriginal = () => original!.batcher.close(AbortSignal.abort(new Error("prior boundary expired")));
    let closed: Promise<void>;
    if (!probe) closed = closeOriginal();
    else {
      // The hold above ran under the generous fixture budget, so a loaded host cannot make the runner abandon this
      // executor and close the feed itself with a real timer. Only the close budget is narrowed to the probed
      // value. Production never splits the hold budget from the close budget; this test-only split reaches the
      // runner's private field; the assertion before execute() keeps it from silently drifting.
      (runner as unknown as { memoryHoldDeadlineMs: number }).memoryHoldDeadlineMs = budget;
      // The runner arms its expiry synchronously inside close(): capture it and freeze the clock for that window only.
      const realTimeout = AbortSignal.timeout, realNow = Date.now, fixed = realNow();
      (AbortSignal as { timeout: (ms: number) => AbortSignal }).timeout = ms => {
        const controller = new AbortController();
        timers.push({ ms, controller });
        return controller.signal;
      };
      Date.now = () => fixed;
      try { closed = closeOriginal(); } finally {
        AbortSignal.timeout = realTimeout;
        Date.now = realNow;
      }
    }
    await closed;
    throw new Error("quiet retained stop");
  } };
  const bounded = <T,>(operation: Promise<T>, label: string) => probe ? withTimeout(operation, 6000, label) : operation;
  const driver = probe ? withTimeout(gateReached, 6000, "tail segment write gate").then(() => {
    for (const timer of timers) if (timer.ms <= probe.simulatedMs) timer.controller.abort();
    release();
  }) : undefined;
  try {
    runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, outbox, codexBoundaryDeadlineMs: probe ? memoryCloseBudgetMs : budget });
    if (probe) assert.equal((runner as unknown as { memoryHoldDeadlineMs: number }).memoryHoldDeadlineMs,
      memoryCloseBudgetMs, "private close-budget field exists");
    await runner.execute(f.claim);
    await driver;
    const memory = original!.workerMemory!;
    await bounded(memory.closing as Promise<void>, "memory closing");
    assert.ok(memory.tailPreservation, "original operation stays explicitly owned");
    if (probe) {
      assert.equal(timers.length, 1, "exactly one expiry timer is armed");
      assert.equal(timers[0]!.ms, budget);
      assert.equal(gateHits, 1, "exactly one tail segment write");
    }
    if (probe && !probe.expectDurable) {
      assert.equal(await bounded(memory.tailPreservation, "tail preservation"), false);
      assert.equal(memory.tailDurable, false);
      assert.equal(original!.retainedLocalCustody, true);
      return;
    }
    assert.equal(await bounded(memory.tailPreservation, "tail preservation"), !fail);
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
    release();
    await f.memory.stop(); await monitoring;
    fs.rmSync(root, { recursive: true, force: true });
  }
}

for (const fail of [false, true]) {
  test(`runner memory close preserves original feedback intent/custody with durable tail=${!fail}`, async () => {
    await runMemoryCloseTail(fail, memoryCloseBudgetMs);
  });
}

for (const { budget, expectDurable } of [{ budget: memoryCloseBudgetMs, expectDurable: true }, { budget: 200, expectDurable: false }]) {
  test(`runner memory close tail probe: budget=${budget} at simulated 250 ms`, async () => {
    // Simulated point between the old 200 ms budget and the 5000 ms fixture budget; only the 200 ms control expires.
    await runMemoryCloseTail(false, budget, { simulatedMs: 250, expectDurable });
  });
}

test("runner memory close tail probe: budget=200 at simulated 250 ms (slow pre-close control)", async () => {
  // Same probe with the executor reaching close() 250 ms late and non-segment outbox writes slowed by 250 ms.
  await runMemoryCloseTail(false, 200, { simulatedMs: 250, expectDurable: false, slowPreCloseMs: 250 });
});

for (const primaryError of [new Error("exact owner failure"), undefined]) {
  test(`execution owner forwards exact primary rejection with pending completion error=${primaryError !== undefined}`, async () => {
    const claim = gitlabClaim(2396);
    const runner = runnerWith(() => ({ executor: { run: async () => {
      throw new Error("owner-only executor must not run");
    } } }), fakeGitlab().gitlab);
    const owner = runner as unknown as {
      executeClaim: (claim: ClaimResponse, inherited: unknown, own: (operation: Promise<void>) => void) => Promise<void>;
      executionTails: Map<string, Promise<void>>;
    };
    const cleanup = releaseGate();
    owner.executeClaim = async (_claim, _inherited, own) => {
      own(cleanup.promise);
      throw primaryError;
    };
    const observation = runner.execute(claim);
    const tail = owner.executionTails.get(claim.run_id)!;
    assert.ok(tail, "pending completion is strongly owned by existing map");
    try {
      await observation.then(() => assert.fail("expected exact rejection"), error => {
        assert.equal(error, primaryError);
        assert.equal(runner.isExecuting(claim.run_id), true);
        assert.equal(owner.executionTails.get(claim.run_id), tail);
      });
      assert.equal(runner.isExecuting(claim.run_id), true);
    } finally {
      cleanup.resolve();
      await tail;
    }
    assert.equal(runner.isExecuting(claim.run_id), false);
  });
}

for (const rejects of [false, true]) {
  test(`non-memory executor observation preserves settlement and exact error rejected=${rejects}`, async () => {
    const runner = runnerWith(() => ({ executor: { run: async () => assert.fail("unused executor") } }), fakeGitlab().gitlab);
    const owner = runner as unknown as {
      awaitExecutorWithMemoryDeadline(flight: unknown, execution: Promise<unknown>): Promise<unknown>;
    };
    const gate = releaseGate(), cancel = new AbortController();
    const memory: { executorRun?: Promise<void> } = {};
    const result = {}, primary = new Error("exact non-memory executor failure");
    const execution = gate.promise.then(() => { if (rejects) throw primary; return result; });
    const observing = owner.awaitExecutorWithMemoryDeadline({ cancel, workerMemory: memory }, execution);
    let settled = false;
    void observing.then(() => { settled = true; }, () => { settled = true; });
    try {
      cancel.abort(new Error("ordinary cancellation"));
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(settled, false, "non-memory cancellation cannot detach executor cleanup");
      assert.equal(memory.executorRun, undefined, "no memory ownership is invented without a hold");
      gate.resolve();
      if (rejects) await assert.rejects(observing, error => error === primary);
      else assert.equal(await observing, result);
    } finally {
      gate.resolve();
      await observing.catch(() => {});
    }
  });
}

test("memory hold after an earlier pause abort still bounds the executor observer", async t => {
  const runner = runnerWith(() => ({ executor: { run: async () => assert.fail("unused executor") } }), fakeGitlab().gitlab);
  const owner = runner as any;
  const execution = releaseGate(), cleanup = releaseGate();
  const cancel = new AbortController();
  const priorPause = new PauseNowSignal();
  cancel.abort(priorPause);
  const memory: any = { ended: false };
  const flight = { cancel, workerMemory: memory };
  t.mock.method(owner, "preserveWorkerMemory", async () => cleanup.promise);
  let returned = false, failure: unknown;
  const observation = owner.awaitExecutorWithMemoryDeadline(flight, execution.promise).catch((error: unknown) => {
    returned = true; failure = error;
  });
  memory.hold = { deadline: Date.now() - 1 };
  const operation = owner.startWorkerMemoryPreservation(flight);
  try {
    await new Promise(resolve => setTimeout(resolve, 0));
    assert.equal(returned, true, "the original spent hold deadline stops observation despite an already-aborted signal");
    assert.equal((failure as Error).constructor.name, "RetainedRecoveryStop");
    assert.equal(cancel.signal.reason, priorPause);
    assert.notEqual(memory.executorSettled, true, "the original executor remains owned");
    assert.notEqual(memory.settled, true, "the original capture remains owned");
  } finally {
    execution.resolve(); cleanup.resolve();
    await observation; await operation;
  }
});

test("execution owner ordinary observation follows tail deletion", async () => {
  const claim = gitlabClaim(2396);
  const runner = runnerWith(() => ({ executor: { run: async () => {
    throw new Error("owner-only executor must not run");
  } } }), fakeGitlab().gitlab);
  const owner = runner as unknown as {
    executeClaim: () => Promise<void>;
    executionTails: Map<string, Promise<void>>;
  };
  owner.executeClaim = async () => {};
  const observation = runner.execute(claim);
  const tail = owner.executionTails.get(claim.run_id)!;
  assert.ok(tail);
  await observation.then(() => {
    assert.equal(owner.executionTails.has(claim.run_id), false);
    assert.equal(runner.isExecuting(claim.run_id), false);
  });
  await tail;
});

// Slow-transport control: wraps globalThis.fetch and, for the FIRST request whose URL ends with `suffix` and whose
// string body contains `marker`, delays the real fetch by 400 ms (beyond the 200 ms owner budget); an abort of that
// request's own signal during the hold rejects with the signal's reason. Install it after constructing the runner,
// immediately before runner.execute, and call restore() in the finally that follows.
function holdFirstPost(suffix: string, marker: string): { restore: () => void; held: () => number } {
  const realFetch = globalThis.fetch;
  let held = 0;
  globalThis.fetch = (async (input: any, init?: any) => {
    const url = String(input?.url ?? input);
    const body = typeof init?.body === "string" ? init.body : "";
    if (held === 0 && url.endsWith(suffix) && body.includes(marker)) {
      held++;
      await new Promise<void>((resolve, reject) => {
        const s: AbortSignal | undefined = init?.signal;
        const t = setTimeout(resolve, 400);
        const fail = () => { clearTimeout(t); reject(s?.reason ?? new DOMException("held response aborted", "AbortError")); };
        if (s?.aborted) fail(); else s?.addEventListener("abort", fail, { once: true });
      });
    }
    return realFetch(input, init);
  }) as typeof fetch;
  return { restore: () => { globalThis.fetch = realFetch; }, held: () => held };
}

const lateAckCases = [
  ...[false, true].flatMap(slow => [true, false].map(claimCredential => ({ slow, claimCredential, slowLaunch: false }))),
  { slow: false, claimCredential: true, slowLaunch: true },
];
for (const { slow, claimCredential, slowLaunch } of lateAckCases) {
test(`M3 late native park ACK stays owned after deadline and cannot mutate a reclaimed successor credential=${claimCredential}${slow ? " (slow transport control)" : ""}${slowLaunch ? " (slow launch control)" : ""}`, async t => {
  const f = await setup(1);
  const obs = observeCustody(f, "lateAck", "lateAck");
  // This source is unenrolled. Keep unrelated remediation I/O out of the 200 ms
  // late-ACK budget; the drained capture scope, native ACK and stale-owner fences remain real.
  const verifyUnenrolledReads = mockUnenrolledSource(t, f.claim);
  const launch = slowLaunch ? launchControl(f.claim, LAUNCH_TARGET_ROUND.lateAck) : undefined;
  const successorCredential = f.claim.secrets.codex;
  const monitoring = f.memory.run();
  const release = releaseGate();
  const successorRelease = releaseGate();
  const cleanupRelease = releaseGate();
  const duplicateRelease = releaseGate();
  const captureReturnedGate = releaseGate();
  const duplicateEnteredGate = releaseGate();
  const rig = codexRig();
  let fullReapCompleted = false;
  const provider = rig.registry.reserveLaunch("provider");
  assert.equal(provider.kind, "reserved");
  if (provider.kind !== "reserved") throw new Error("provider reservation refused");
  rig.registry.registerRoot(provider.reservation, {
    kind: "provider",
    reap: async () => {
      fullReapCompleted = false;
      await Promise.resolve();
      fullReapCompleted = true;
      return { ok: true };
    },
    dispose: async () => {},
  });
  let captureContext: Parameters<Parameters<typeof rig.capture>[1]>[0] | undefined;
  let captureReturned = false, protectedScope: boolean | undefined, duplicateEntered = false;
  let queuedWork: Promise<void> | undefined;
  const originalReadiness = git.normalizeRetainedSourceObjects;
  let readiness = 0;
  let original: MemoryFlightProof | undefined;
  let successor: ReturnType<NonNullable<RunContext["capturedMemory"]>>;
  let nativeAck: StateAck | undefined, parkSignal: AbortSignal | undefined, liveAtSend: boolean | undefined;
  let parkAtWorkSettle: boolean | string | undefined;
  let successorWork: Promise<void> | undefined;
  let started: number | undefined, rescue: ReturnType<typeof setTimeout> | undefined;
  const report = client.reportState.bind(client);
  client.reportState = async (...args) => {
    if (args[1].recovery_cause !== "worker_memory_pressure") return report(...args);
    parkSignal = args[2];
    obs.observePark(args);
    liveAtSend = !args[2]?.aborted;
    // The uncooperative adapter ignores the owner abort, so the REAL client-validated receipt
    // is obtained even when the post outlives the deadline; abort is observed on parkSignal above.
    const ack = await report(args[0], args[1], undefined, args[3]);
    nativeAck = ack;
    // Start rescue at the blocked port, leaving room for the original 200ms stop,
    // successor's 5000ms local capture and intervening fixture work. Finally releases and joins it.
    rescue = setTimeout(() => release.resolve(), 10000);
    await release.promise;
    return ack;
  };
  fs.writeFileSync(homeDir + "/session-proof", "original session");
  git.worktreeStatus = async () => {
    protectedScope ??= captureContext !== undefined && !captureContext.signal.aborted &&
      rig.registry.state() === "poisoned" && rig.registry.pendingLaunchCount() === 0 &&
      rig.registry.inFlightCallbackCount() === 0 && fullReapCompleted;
    return ["M fixture"];
  };
  git.commitWipMarker = async () => false;
  const executor: Executor = {
    withCredentialFreeCapture: obs.wrapCapture(async (deadline, action, purpose, signal) => {
      try {
        return await rig.capture(deadline, async context => {
          captureContext = context;
          return action(context);
        }, purpose, signal);
      } finally {
        captureReturned = true;
        captureReturnedGate.resolve();
        await cleanupRelease.promise;
      }
    }),
    run: async ctx => {
      launch?.markEntered();
      const command = enroll(ctx);
      original = command.captured.flight as MemoryFlightProof;
      if (!claimCredential) delete f.claim.secrets.codex;
      started = performance.now();
      f.pressure(950);
      const result = await command.captured.commands.watchOutcome(command.entry);
      ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
      throw new Error("quiet retained stop");
    },
  };
  const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, launch?.logger,
    { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, memoryHoldDeadlineMs: 200 });
  let hold: ReturnType<typeof holdFirstPost> | undefined;
  let work!: Promise<void>;
  let deadlineTimer: ReturnType<typeof setTimeout> | undefined;
  // Attach runs inside the try so a throwing attach still reaches the finally that stops the monitor and joins the observer.
  let teardownErrors: unknown[] = [];
  try {
    launch?.attach(runner);
    obs.attach(runner, executor);
    hold = slow ? holdFirstPost("/state", "worker_memory_pressure") : undefined;
    obs.startClock();
    work = obs.invoke("original", () => runner.execute(f.claim));
    // Pins the park signal state at the first reaction registered on work.
    const rec = () => { parkAtWorkSettle = parkSignal === undefined ? "no-park-signal" : parkSignal.aborted; };
    work.then(rec, rec);
    await until(() => nativeAck !== undefined, 3000 + (launch ? LAUNCH_GATE_CAP_MS : 0), launch?.facts);
    assert.equal(liveAtSend, true, "original park send starts before its owner deadline");
    if (hold) assert.equal(hold.held(), 1, "control: first park POST response was held");
    assert.ok(nativeAck!.memoryParkAccepted, "receipt derives from the native public memory-hold DTO");
    // Race the join against the same 1500 ms perf-clock allowance the assertion below enforces, so a
    // runner that waits for the uncooperative park return fails by name instead of by the test timeout.
    try {
      await Promise.race([work, new Promise<never>((_, reject) => {
        // Node truncates fractional delays, so the callback rechecks the perf clock and reschedules the remainder.
        const arm = () => {
          const remaining = 1500 - (performance.now() - (started ?? performance.now()));
          if (remaining > 0) { deadlineTimer = setTimeout(arm, Math.ceil(remaining)); return; }
          reject(new Error("runner deadline does not wait for uncooperative park return"));
        };
        arm();
      })]);
    } finally { clearTimeout(deadlineTimer); }
    obs.snapshotPort("park");
    assert.ok(started !== undefined, "elapsed clock anchored at the pressure trigger");
    assert.ok(performance.now() - started < 1500, "runner deadline does not wait for uncooperative park return");
    assert.equal(protectedScope, true, "fake protected source runs inside real drained capture scope");
    assert.equal(captureReturned, false, "ACK still holds actual capture callback");
    const operation = original!.workerMemory!.operation!;
    assert.ok(operation, "original preservation promise remains on its flight");
    assert.notEqual(original!.workerMemory!.settled, true, "original work has not settled");
    assert.equal(runner.isExecuting(f.claim.run_id), true, "execution tail owns the pending original capture");
    assert.equal(parkSignal!.aborted, true);
    assert.equal(parkAtWorkSettle, true, "park signal aborted when original work settled");
    assert.notEqual(original!.parked, true, "native acceptance alone cannot mark an expired flight parked");
    assert.equal(original!.retainedLocalCustody, true);
    assert.equal(fs.readFileSync(homeDir + "/session-proof", "utf8"), "original session");
    assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
    assert.equal(api.states.filter(s => s.body.recovery_cause === "worker_memory_pressure").length, 1);
    assert.equal(api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure").length, 1);

    assert.deepEqual(await runner.withRetainedSourceBoundary(f.claim.run_id, [], async () => {
      return "unexpected maintenance entry";
    }), { entered: false }, "maintenance refuses before lock/capture acquisition");
    const duplicateGate = runner as unknown as { gateQueuedDuplicate: () => Promise<boolean> };
    duplicateGate.gateQueuedDuplicate = async () => {
      duplicateEntered = true;
      duplicateEnteredGate.resolve();
      await duplicateRelease.promise;
      return false;
    };
    queuedWork = obs.invoke("duplicate", () => runner.execute(f.claim));
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(duplicateEntered, false, "same-run duplicate cannot reach its gate while ACK is pending");
    f.pressure(100);
    git.worktreeStatus = async () => [];
    api.memoryRegisterNonce = "successor-memory-registration";
    await client.register("memory-fixture", "base");
    const next = { ...f.claim, secrets: { ...f.claim.secrets, codex: successorCredential },
      claim_generation: 8, memory_episode: 4, memory_intervention_count: 1 };
    api.setOwnershipStatus(next.run_id, "running", 8);
    // Host-only successor models the Codex sandbox profile with readiness already satisfied.
    git.normalizeRetainedSourceObjects = async () => { readiness++; };
    const nextExecutor: Executor = { sandboxesCommands: true, withCredentialFreeCapture: realCapture(), run: async ctx => {
      assert.equal(readiness, 1, "retained readiness completed before successor admission");
      successor = ctx.capturedMemory!();
      await successorRelease.promise;
      throw new Error("successor proof complete");
    } };
    const successorLog: string[] = [];
    const log = nullLogger();
    log.child = () => log;
    log.warn = log.error = (message, fields) => { successorLog.push(message + JSON.stringify(fields)); };
    successorWork = obs.invoke("successor", () => {
      const successorRunner = runnerWith(() => ({ executor: nextExecutor }), fakeGitlab().gitlab, undefined, log,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000 });
      obs.attachOwed(successorRunner);
      return successorRunner.execute(next);
    });
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
    obs.assertOverlap(nextFlight);
    assert.notEqual(original!.workerMemory!.settled, true, "controlled release, not rescue, settles old park");
    const nextParked = nextFlight.parked;
    release.resolve();
    await captureReturnedGate.promise;
    assert.equal(captureReturned, true, "actual capture has returned before wrapper cleanup");
    assert.equal(captureContext!.signal.aborted, true);
    assert.notEqual(original!.workerMemory!.settled, true, "ACK return is not full wrapper cleanup");
    assert.equal(duplicateEntered, false, "tail also owns delayed scope wrapper cleanup");
    assert.equal(runner.isExecuting(f.claim.run_id), true);
    cleanupRelease.resolve();
    await operation;
    await duplicateEnteredGate.promise;
    assert.equal(runner.isExecuting(f.claim.run_id), true, "old owner cannot delete replacement tail");
    duplicateRelease.resolve();
    await queuedWork;
    assert.equal(runner.isExecuting(f.claim.run_id), false, "replacement releases only after owned join");
    assert.equal(original!.workerMemory!.operation, operation, "late completion settles the same original promise");
    assert.equal(original!.workerMemory!.settled, true);
    assert.notEqual(original!.parked, true);
    assert.equal(nextFlight.parked, nextParked, "old acceptance cannot mark successor parked");
    assert.equal(nextFlight.workerMemory!.operation, undefined);
    assert.equal(successor!.commands.admitsCommands(successor!.context), true);
    assert.equal(successor!.commands.remaining(successor!.context), 1);
  } finally {
    try {
      hold?.restore();
      clearTimeout(rescue);
      release.resolve();
      cleanupRelease.resolve();
      duplicateRelease.resolve();
      successorRelease.resolve();
      teardownErrors = await joinSettled([() => work, () => original?.workerMemory?.operation,
        () => original?.workerMemory?.closing, () => successorWork, () => queuedWork]);
      git.normalizeRetainedSourceObjects = originalReadiness;
      await f.memory.stop(); await monitoring;
      await Promise.allSettled(f.memory.sinkReceipts.map(receipt => receipt.completion));
    } finally {
      await obs.join();
    }
  }
  // A rejected teardown join surfaces only after the monitor stopped, and only when the test body itself passed.
  if (teardownErrors.length > 0) throw teardownErrors[0];
  obs.assertAll();
  verifyUnenrolledReads();
  launch?.assertControl();
});
}

for (const interruption of ["registration", "generation", "shutdown", "cancel"] as const) {
  test(`M3 capture interruption ${interruption} keeps original custody and prevents stale mutations`, async () => {
    const f = await setup(1);
    const monitoring = f.memory.run();
    const captureScope = new AsyncLocalStorage<boolean>();
    let original: MemoryFlightProof | undefined;
    let reads = 0, writes = 0, fetches = 0;
    const executor: Executor = {
      withCredentialFreeCapture: realCapture(),
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

const delayedFeedbackCases = [
  { slow: false, slowLaunch: false }, { slow: true, slowLaunch: false }, { slow: false, slowLaunch: true },
];
for (const { slow, slowLaunch } of delayedFeedbackCases) {
test(`M3 delayed feedback beyond deadline retains original send and sink receipt without parking${slow ? " (slow transport control)" : ""}${slowLaunch ? " (slow launch control)" : ""}`, async () => {
  const f = await setup(1);
  const obs = observeCustody(f, "delayedFeedback", "delayedFeedback");
  const launch = slowLaunch ? launchControl(f.claim, LAUNCH_TARGET_ROUND.delayedFeedback) : undefined;
  const monitoring = f.memory.run();
  const release = releaseGate();
  let original: MemoryFlightProof | undefined, sending = false, sendSignal: AbortSignal | undefined;
  let liveAtSend: boolean | undefined;
  let started: number | undefined, rescue: ReturnType<typeof setTimeout> | undefined;
  const post = client.postMessages.bind(client);
  client.postMessages = async (...args) => {
    if (args[1].some(message => message.payload.event === "worker_memory_pressure")) {
      // Observe the owned send before the real post can return: its latency is not part of the contract.
      sending = true;
      sendSignal = args[3];
      obs.observeSend(args);
      liveAtSend = !args[3]?.aborted;
      // The uncooperative adapter ignores the owner abort, so the real post lands even when it
      // outlives the deadline; its native feed ACK is then withheld.
      await post(args[0], args[1], args[2]);
      rescue = setTimeout(() => release.resolve(), 2000);
      await release.promise;
      return;
    }
    await post(...args);
  };
  fs.writeFileSync(homeDir + "/session-proof", "original session");
  const executor: Executor = { run: async ctx => {
    launch?.markEntered();
    const command = enroll(ctx);
    original = command.captured.flight as MemoryFlightProof;
    started = performance.now();
    f.pressure(950);
    await command.captured.commands.watchOutcome(command.entry);
    throw new Error("quiet retained stop");
  } };
  const runner = runnerWith(() => ({ executor, homeDir }), fakeGitlab().gitlab, undefined, launch?.logger,
    { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000, memoryHoldDeadlineMs: 200 });
  let hold: ReturnType<typeof holdFirstPost> | undefined;
  let work!: Promise<void>;
  // Attach runs inside the try so a throwing attach still reaches the finally that stops the monitor and joins the observer.
  let teardownErrors: unknown[] = [];
  try {
    launch?.attach(runner);
    obs.attach(runner, executor);
    hold = slow ? holdFirstPost("/messages", "worker_memory_pressure") : undefined;
    obs.startClock();
    work = obs.invoke("original", () => runner.execute(f.claim));
    await until(() => sending, 3000 + (launch ? LAUNCH_GATE_CAP_MS : 0), launch?.facts);
    assert.equal(liveAtSend, true, "original feedback send starts before its owner deadline");
    const receipt = f.memory.sinkReceipts.find(r => r.kind === "feedback" && r.context.flight === original);
    assert.ok(receipt, "original feedback sink owns its completion receipt");
    const pending = [...original!.workerMemory!.feedback.values()][0]!;
    const operation = pending.operation!;
    assert.ok(operation);
    let settled = false;
    void operation.then(() => { settled = true; });
    await work;
    obs.snapshotPort("send");
    assert.ok(started !== undefined, "elapsed clock anchored at the pressure trigger");
    assert.ok(performance.now() - started < 1500, "uncooperative feedback does not defeat owner deadline");
    assert.equal(settled, false);
    if (hold) assert.equal(hold.held(), 1, "control: first feedback POST response was held");
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
    try {
      hold?.restore();
      clearTimeout(rescue); release.resolve();
      teardownErrors = await joinSettled([() => work, () => original?.workerMemory?.operation,
        () => original?.workerMemory?.closing]);
      await f.memory.stop(); await monitoring;
      await Promise.allSettled(f.memory.sinkReceipts.map(receipt => receipt.completion));
    } finally {
      await obs.join();
    }
  }
  // A rejected teardown join surfaces only after the monitor stopped, and only when the test body itself passed.
  if (teardownErrors.length > 0) throw teardownErrors[0];
  obs.assertAll();
  launch?.assertControl();
});
}

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

for (const [label, answers, guidance] of [
  ["a plain continue", ["continue"], undefined],
  ["a continue with guidance", ["finish M2"], "finish M2"],
] as const) {
  test(`completion question answered with ${label}: the next Running ACK releases its input phase hold`, async () => {
    const f = await setup();
    const monitoring = f.memory.run();
    let executorError: unknown, proof = false;
    let questionWork: Promise<unknown> | undefined;
    const rescue = setTimeout(() => api.setInputs(f.claim.run_id, [input("cancel")]), 15000);
    const executor: Executor = { run: async ctx => {
      try {
        await ctx.reportIteration!(1);
        const before = ctx.capturedMemory!()!;
        assert.equal(before.commands.admitsCommands(before.context), true);
        questionWork = ctx.askCompletionQuestion!(["m1"], { kind: "lead_request", reason: "fixture" });
        await until(() => api.states.at(-1)?.body.status === "awaiting_input");
        const waiting = ctx.capturedMemory!()!;
        assert.equal(waiting.context, before.context);
        assert.equal(waiting.commands.phaseAdmits(waiting.context), false, "the completion question holds the phase");
        const question = api.states.findLast(s => s.body.status === "awaiting_input")!.body.open_question_id;
        api.setInputs(f.claim.run_id, [input("answer", JSON.stringify({ question_id: question, answers }))]);
        assert.deepEqual(await questionWork, { outcome: "continue", guidance });
        const resumed = ctx.capturedMemory!()!;
        assert.equal(resumed.context, waiting.context, "the continue keeps the execution's context");
        assert.equal(resumed.commands.phaseAdmits(resumed.context), false, "the hold stays until the Running ACK");
        await ctx.reportIteration!(2);
        assert.equal(resumed.commands.isCurrent(resumed.context), true);
        assert.equal(resumed.commands.phaseAdmits(resumed.context), true, "the Running ACK releases the completion hold");
        assert.equal(resumed.commands.admitsCommands(resumed.context), true, "shell commands are admitted again");
        assert.equal(resumed.commands.remaining(resumed.context), 2, "the intervention allowance is untouched");
        assert.equal(f.reservations.length, 0);
        proof = true;
      } catch (error) { executorError = error; throw error; }
      finally { await questionWork?.catch(() => {}); }
      throw new Error("completion continue proof complete");
    } };
    try {
      await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
      if (executorError) throw executorError;
      assert.equal(proof, true);
    } finally {
      clearTimeout(rescue);
      await f.memory.stop(); await monitoring;
    }
  });
}

test("two consecutive completion questions, each continued, each release their own hold after their Running ACK", async () => {
  const f = await setup();
  const monitoring = f.memory.run();
  let executorError: unknown, proof = false;
  let questionWork: Promise<unknown> | undefined;
  const rescue = setTimeout(() => api.setInputs(f.claim.run_id, [input("cancel")]), 15000);
  const executor: Executor = { run: async ctx => {
    try {
      await ctx.reportIteration!(1);
      const captured = ctx.capturedMemory!()!;
      for (const round of [2, 3]) {
        questionWork = ctx.askCompletionQuestion!(["m1"], { kind: "lead_request", reason: `round ${round}` });
        await until(() => api.states.at(-1)?.body.status === "awaiting_input");
        assert.equal(captured.commands.phaseAdmits(captured.context), false, `round ${round} holds the phase`);
        const question = api.states.findLast(s => s.body.status === "awaiting_input")!.body.open_question_id;
        api.setInputs(f.claim.run_id, [input("answer", JSON.stringify({ question_id: question, answers: ["continue"] }))]);
        assert.deepEqual(await questionWork, { outcome: "continue", guidance: undefined });
        assert.equal(captured.commands.phaseAdmits(captured.context), false, `round ${round} holds until its ACK`);
        await ctx.reportIteration!(round);
        assert.equal(ctx.capturedMemory!()!.context, captured.context);
        assert.equal(captured.commands.admitsCommands(captured.context), true, `round ${round} releases its own hold`);
      }
      proof = true;
    } catch (error) { executorError = error; throw error; }
    finally { await questionWork?.catch(() => {}); }
    throw new Error("repeated completion continue proof complete");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    assert.equal(proof, true);
  } finally {
    clearTimeout(rescue);
    await f.memory.stop(); await monitoring;
  }
});

for (const [label, tamper] of [
  ["a rejected (applied: false)", (ack: StateAck) => ({ ...ack, applied: false })],
  ["a stale-claim", (ack: StateAck) => ({ ...ack, staleClaim: true })],
] as const) {
  test(`after a completion continue, ${label} Running ACK releases nothing`, async () => {
    const f = await setup();
    const monitoring = f.memory.run();
    let executorError: unknown, proof = false;
    let questionWork: Promise<unknown> | undefined;
    const rescue = setTimeout(() => api.setInputs(f.claim.run_id, [input("cancel")]), 15000);
    const report = client.reportState.bind(client);
    const executor: Executor = { run: async ctx => {
      try {
        await ctx.reportIteration!(1);
        const captured = ctx.capturedMemory!()!;
        questionWork = ctx.askCompletionQuestion!(["m1"], { kind: "lead_request", reason: "fixture" });
        await until(() => api.states.at(-1)?.body.status === "awaiting_input");
        const question = api.states.findLast(s => s.body.status === "awaiting_input")!.body.open_question_id;
        api.setInputs(f.claim.run_id, [input("answer", JSON.stringify({ question_id: question, answers: ["continue"] }))]);
        assert.deepEqual(await questionWork, { outcome: "continue", guidance: undefined });
        let tampered = false;
        client.reportState = async (...args) => {
          const ack = await report(...args);
          if (!tampered && args[1].status === "running") { tampered = true; return tamper(ack); }
          return ack;
        };
        await Promise.resolve(ctx.reportIteration!(2)).catch(() => {});
        assert.equal(tampered, true);
        assert.equal(captured.commands.phaseAdmits(captured.context), false, "the completion hold remains");
        assert.equal(captured.commands.admitsCommands(captured.context), false);
        proof = true;
      } catch (error) { executorError = error; throw error; }
      finally { client.reportState = report; await questionWork?.catch(() => {}); }
      throw new Error("completion ACK control proof complete");
    } };
    try {
      await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
        { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim).catch(() => {});
      if (executorError) throw executorError;
      assert.equal(proof, true);
    } finally {
      clearTimeout(rescue);
      await f.memory.stop(); await monitoring;
    }
  });
}

test("an expired completion question releases nothing on a later Running ACK", async () => {
  const f = await setup();
  f.claim.config = { ...f.claim.config, completion_hold_window_seconds: 1 } as typeof f.claim.config;
  const monitoring = f.memory.run();
  let executorError: unknown, proof = false;
  const rescue = setTimeout(() => api.setInputs(f.claim.run_id, [input("cancel")]), 15000);
  const executor: Executor = { run: async ctx => {
    try {
      await ctx.reportIteration!(1);
      const waiting = ctx.capturedMemory!()!;
      const questionWork = ctx.askCompletionQuestion!(["m1"], { kind: "lead_request", reason: "fixture" });
      await until(() => api.states.at(-1)?.body.status === "awaiting_input");
      assert.equal(waiting.commands.phaseAdmits(waiting.context), false);
      assert.deepEqual(await questionWork, { outcome: "expired" });
      assert.equal(waiting.commands.phaseAdmits(waiting.context), false, "an expired question releases nothing");
      assert.equal(waiting.commands.admitsCommands(waiting.context), false);
      await ctx.reportIteration!(2);
      assert.equal(waiting.commands.phaseAdmits(waiting.context), false, "a later Running ACK releases nothing either");
      assert.equal(waiting.commands.admitsCommands(waiting.context), false);
      proof = true;
    } catch (error) { executorError = error; throw error; }
    throw new Error("completion expiry proof complete");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 1000 }).execute(f.claim);
    if (executorError) throw executorError;
    assert.equal(proof, true);
  } finally {
    clearTimeout(rescue);
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
    const r = runnerWith(() => {
      // Each execution owns a fresh Codex epoch. A real closed/drained boundary
      // permits normal teardown before this worker accepts the persisted claim.
      const rig = codexRig();
      return { executor: { ...executor, safety: rig.safety,
        settleForCredentialFreeCapture: rig.settle, withCredentialFreeCapture: rig.capture,
        run: async ctx => {
          try { return await executor.run(ctx); }
          finally { await rig.safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000 }, async () => {}); }
        },
      } };
    }, fakeGitlab().gitlab, undefined, undefined, { memoryRuntime: memory });
    await r.execute({ ...f.claim, memory_policy: null });
    if (executorError) throw executorError;
    assert.equal(calls, 1, "first null-policy claim enters the actual runner");
    await r.execute({ ...f.claim, memory_intervention_count: 1 });
    if (executorError) throw executorError;
    assert.equal(calls, 2, "persisted allowance remains authoritative");
  } finally { await memory.stop(); }
});

test("runner fails closed on incomplete enabled Codex claims; a complete Claude claim gets the accessor (#2471)", async () => {
  const f = await setup();
  let calls = 0;
  let accessor: unknown;
  const executor: Executor = { run: async ctx => {
    calls++;
    accessor = ctx.capturedMemory;
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
    assert.equal(typeof accessor, "function", "a guard-bound runner enrols a Claude claim too");
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
      withCredentialFreeCapture: realCapture(),
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
      withCredentialFreeCapture: realCapture(async () => {
        if (!pauseSent) {
          pauseSent = true;
          api.appendInputs(f.claim.run_id, [input("pause", mode)]);
          await until(() => context?.pauseModeRequested?.() === mode);
        }
      }),
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

// The two controls inject latency the plain scenarios do not, in places the original fixtures were sensitive to.
// already-parked "slow transport": the first pressure batch is held 300 ms at the fake API's HTTP route, longer than the
// 200 ms feedback budget. The scenario accepts every batch in-process, so the hold must never fire (delayedMessages 0)
// and the park must still be accepted with the real pressure message recorded exactly once.
// withdrawn "slow capture": the first checkpointPackCaptured inside the capture finishes its real work and is then held
// until 3050 ms after the executor starts its settlement wait, so the capture outlasts the former 3000 ms poll whatever
// the host's git speed, and only the short capture tail remains inside its 5000 ms budget; the scenario joins the
// owned operation instead.
const handoffCases = [
  ...(["wall", "disk", "withdrawn", "cancel", "shutdown", "claim-fence", "credential-switch", "registration", "unsettled", "expired", "already-parked"] as const)
    .map(scenario => ({ scenario, control: undefined as string | undefined })),
  { scenario: "already-parked" as const, control: "slow transport control" },
  { scenario: "withdrawn" as const, control: "slow capture control" },
  { scenario: "unsettled" as const, control: "slow launch control" },
  { scenario: "expired" as const, control: "slow launch control" },
];
for (const { scenario, control } of handoffCases) {
  test(`memory pause handoff retains custody without an owner-pause disposition (${control ? `${scenario}, ${control}` : scenario})`, async t => {
    const f = await setup(1);
    const verifyUnenrolledReads = scenario === "unsettled" || scenario === "expired"
      ? mockUnenrolledSource(t, f.claim) : undefined;
    const launch = control === "slow launch control" ? launchControl(f.claim, LAUNCH_TARGET_ROUND.handoff) : undefined;
    const obs = scenario === "unsettled" || scenario === "expired" ? observeCustody(f, "handoff", scenario) : undefined;
    const monitoring = f.memory.run();
    const release = releaseGate();
    let context: RunContext | undefined, captured: MemoryFlightProof | undefined;
    let executorError: unknown, entered = false, parkAssertionsRan = false, heldCapture = 0;
    // Set by the executor immediately before it starts waiting on the capture's settlement.
    let waitStarted: number | undefined;
    const releaseAfterWaitMs = 3050; // just past the former 3000 ms poll
    const captureHoldCapMs = 4000;   // safety bound so a missing waitStarted cannot hang the capture
    const executorDone = releaseGate();
    let runner: ReturnType<typeof runnerWith>;
    if (scenario === "already-parked") {
      // Production's postMessages refuses an already-aborted send; the in-process transport records the real batch
      // through the fake API's own recording, so a loopback request cannot defeat the 200 ms feedback budget.
      client.postMessages = async (runId, messages, generation, signal) => {
        signal?.throwIfAborted();
        api.acceptMessages(runId, { messages, ...(generation !== undefined ? { claim_generation: generation } : {}) });
      };
    }
    if (control === "slow transport control") {
      let first = true;
      api.delayMessagesMs = (_runId, body) => {
        if (!first || !JSON.stringify(body).includes("worker_memory_pressure")) return 0;
        first = false;
        return 300;
      };
    }
    if (control === "slow capture control") {
      // The method is private to GitCache; the control holds the real call and then runs it unchanged.
      const target = git as unknown as { checkpointPackCaptured(...args: unknown[]): Promise<unknown> };
      const pack = target.checkpointPackCaptured.bind(git);
      t.mock.method(target, "checkpointPackCaptured", async (...args: unknown[]) => {
        // Run the real call first, then hold its result until a fixed time after the test began its settlement
        // wait: the capture then outlasts the former 3000 ms poll by a margin that does not depend on host speed,
        // and only the short capture tail remains once released. Bounded by captureHoldCapMs.
        const result = await pack(...args);
        if (heldCapture++ === 0) {
          const gaveUp = Date.now() + captureHoldCapMs;
          while ((waitStarted === undefined || Date.now() < waitStarted + releaseAfterWaitMs) && Date.now() < gaveUp) {
            await new Promise(resolve => setTimeout(resolve, 10));
          }
        }
        return result;
      });
    }
    fs.writeFileSync(homeDir + "/pause-session-proof", "retained session");
    const executor: Executor = {
      withCredentialFreeCapture: realCapture(async () => {
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
      }),
      run: async ctx => {
        launch?.markEntered();
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
          } else if (scenario === "already-parked" || scenario === "withdrawn") {
            // The owned capture has a five-second budget; a three-second poll can expire first.
            const operation = captured.workerMemory?.operation;
            assert.ok(operation, `${scenario} capture keeps its owned operation`);
            waitStarted = Date.now();
            await withTimeout(operation, 6000, `${scenario} memory capture settlement`);
            assert.equal(captured.workerMemory?.settled, true);
          } else {
            await until(() => captured?.workerMemory?.settled === true);
          }
          if (scenario === "already-parked") {
            assert.equal(captured.parked, true, "the original memory park must be positively accepted first");
            assert.equal(f.parkReceipts.length, 1);
            const pressure = api.messages(f.claim.run_id).filter(m => m.payload.event === "worker_memory_pressure");
            assert.equal(pressure.length, 1, "exactly one pressure message was recorded");
            assert.equal(pressure[0]!.payload.intervention_id, f.reservations[0]!.intervention_id);
            captured.steering.seedPauseRequested("now");
            parkAssertionsRan = true;
          }
        } catch (error) { executorError = error; throw error; } finally { executorDone.resolve(); }
        throw ctx.signal?.reason ?? new PauseNowSignal();
      },
    };
    if (obs) executor.withCredentialFreeCapture = obs.wrapCapture(executor.withCredentialFreeCapture!);
    runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, launch?.logger,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: 5000,
        ...(scenario === "unsettled" || scenario === "expired" ? { memoryHoldDeadlineMs: 200 } : {}) });
    let teardownErrors: unknown[] = [];
    try {
      launch?.attach(runner);
      obs?.attach(runner, executor);
      obs?.startClock();
      await (obs ? obs.invoke("original", () => runner.execute(f.claim)) : runner.execute(f.claim));
      // execute() can return while the executor is still joining a long capture; wait for its assertions to finish.
      await withTimeout(executorDone.promise, 7000 + (launch ? LAUNCH_GATE_CAP_MS : 0), "executor assertions", launch?.facts);
      if (executorError) throw executorError;
      if (scenario === "already-parked") assert.equal(parkAssertionsRan, true, "already-parked park assertions ran");
      if (control === "slow transport control") assert.equal(api.delayedMessages, 0, "the in-process transport never rides the delayed HTTP route");
      if (control === "slow capture control") assert.ok(heldCapture >= 1, "the capture delay was applied");
      const disposition = api.states.filter(s => s.body.status === "paused" || s.body.status === "pause_failed");
      assert.equal(disposition.length, 0, "unsettled work or a different lifecycle outcome cannot authorize owner pause");
      assert.equal(fs.existsSync(worktreeDirFor(2396)), true);
      assert.equal(fs.readFileSync(homeDir + "/pause-session-proof", "utf8"), "retained session");
      assert.equal(captured?.retainedLocalCustody, true);
    } finally {
      try {
        release.resolve();
        teardownErrors = await joinSettled([() => captured?.workerMemory?.operation]);
        await f.memory.stop(); await monitoring;
      } finally {
        await obs?.join();
      }
    }
    // A rejected teardown join surfaces only after the monitor stopped, and only when the test body itself passed.
    if (teardownErrors.length > 0) throw teardownErrors[0];
    obs?.assertAll();
    verifyUnenrolledReads?.();
    launch?.assertControl();
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
      withCredentialFreeCapture: realCapture(),
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


test("terminal observation stays on the exact tail through owned memory and queued duplicates", async () => {
  const claim = gitlabClaim(2396);
  const runner = runnerWith(() => ({ executor: { run: async () => { throw new Error("unused"); } } }), fakeGitlab().gitlab);
  const owner = runner as any;
  const cleanup = releaseGate();
  const observation = { terminal: "normal_failed", family: { original: "source" } };
  const inherited: unknown[] = [];
  let first = true, registered = 0, released = false;
  let registrationState: unknown;
  owner.gateQueuedDuplicate = async () => true;
  owner.refreshQueuedClaimCursor = async (value: ClaimResponse) => value;
  owner.executeClaim = async (_claim: ClaimResponse, previous: unknown, own: (operation: Promise<void>) => void) => {
    inherited.push(previous);
    if (!first) return;
    first = false;
    own(cleanup.promise);
    return observation;
  };
  const publicFirst = runner.execute(claim);
  const firstTail = owner.executionTails.get(claim.run_id);
  void firstTail.then(() => { released = true; });
  owner.recovery.resumeTerminalSource = async (_id: string, exact: Promise<unknown>) => {
    registered++;
    assert.equal(exact, firstTail);
    assert.equal(released, false, "recovery is registered before tail resolution");
    assert.notEqual(owner.executionTails.get(claim.run_id), firstTail, "a queued successor owns the map entry");
    registrationState = await Promise.race([exact, Promise.resolve("pending at registration")]);
    assert.equal(await exact, observation);
  };
  const publicSecond = runner.execute(claim);
  const secondTail = owner.executionTails.get(claim.run_id);
  const publicThird = runner.execute(claim);
  const thirdTail = owner.executionTails.get(claim.run_id);
  try {
    await publicFirst;
    assert.deepEqual(inherited, [undefined]);
    assert.equal(registered, 0, "memory cleanup precedes recovery registration");
    assert.equal(released, false);
    assert.equal(owner.executionTails.get(claim.run_id), thirdTail);
  } finally { cleanup.resolve(); }
  assert.equal(await firstTail, observation);
  await Promise.all([publicSecond, publicThird, secondTail, thirdTail]);
  assert.deepEqual(inherited, [undefined, observation, undefined]);
  assert.equal(registered, 1);
  assert.equal(registrationState, "pending at registration", "recovery registration precedes promise resolution");
  assert.equal(runner.isExecuting(claim.run_id), false);
});

// Issue #3240: a pause that cannot publish (nothing durable yet) reports pause_failed and the run
// KEEPS RUNNING (Decision 8). pause_failed is a diagnostic, not a lifecycle change: the context the
// Codex broker captured before the pause must stay exactly as it was, immediately after the failed
// pause and after the next Running ACK, with monitoring, latches and accounting untouched.
type MemorySnapshot = { current: boolean; admits: boolean; held: boolean; remaining: number | undefined };
function snapshot(c: NonNullable<ReturnType<NonNullable<RunContext["capturedMemory"]>>>): MemorySnapshot {
  return {
    current: c.commands.isCurrent(c.context, c.flight, c.lifecycleRevision),
    admits: c.commands.admitsCommands(c.context),
    held: c.commands.pressureHeld(c.context),
    remaining: c.commands.remaining(c.context),
  };
}
async function memoryRun(f: Awaited<ReturnType<typeof setup>>, body: (ctx: RunContext) => Promise<void>,
  capture?: NonNullable<Executor["withCredentialFreeCapture"]>) {
  const monitoring = f.memory.run();
  let executorError: unknown, proof = false;
  const executor: Executor = { withCredentialFreeCapture: capture, run: async ctx => {
    try { await body(ctx); proof = true; } catch (error) { executorError = error; throw error; }
    throw new Error("fixture proof complete");
  } };
  try {
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined,
      { memoryRuntime: f.memory, codexBoundaryDeadlineMs: capture ? 30_000 : 1000 }).execute(f.claim);
  } catch (error) {
    // A body assertion failure wins over the runner's own rejection (e.g. a tolerated StaleClaimError).
    throw executorError ?? error;
  } finally { api.stateAckWire = undefined; await f.memory.stop(); await monitoring; }
  if (executorError) throw executorError;
  assert.equal(proof, true);
}
const failedPause = (ctx: RunContext) => ctx.parkForPause!({ completedCount: 0, total: 1 });

for (const variant of ["admitting", "pressure-latched", "intervention-spent"] as const) {
  test(`a failed pause leaves the captured memory context unchanged (${variant})`, async () => {
    const f = await setup();
    let before: NonNullable<ReturnType<NonNullable<RunContext["capturedMemory"]>>> | undefined;
    await memoryRun(f, async ctx => {
      await ctx.reportIteration!(1);
      if (variant !== "admitting") {
        // One intervention: the latch holds (pressure-latched). For intervention-spent, sustained low
        // pressure then rearms it with one of two left. (An exhausted allowance is an owner hold that
        // stops the flight before any pause, so it has no pause_failed to survive.)
        const first = enroll(ctx);
        f.pressure(950);
        const result = await first.captured.commands.watchOutcome(first.entry);
        ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
        assert.equal(first.captured.commands.admitsCommands(first.captured.context), false, "the latch holds");
        if (variant === "intervention-spent") {
          f.pressure(100);
          await until(() => first.captured.commands.admitsCommands(first.captured.context));
          await until(() => f.memory.sinkReceipts.length === 0);
          assert.equal(first.captured.commands.remaining(first.captured.context), 1, "one intervention spent");
        }
      }
      before = ctx.capturedMemory!()!;
      const was = snapshot(before);
      const spent = f.reservations.length;
      assert.equal(await failedPause(ctx), false, "an unpublishable pause does not park");
      assert.ok(api.states.some(s => s.runId === f.claim.run_id && s.body.status === "pause_failed"));
      assert.deepEqual(snapshot(before), was, "immediately after pause_failed, before any running report");
      assert.equal(ctx.capturedMemory!()!.context, before.context, "the continuing run keeps its context");
      await ctx.reportIteration!(2);
      assert.deepEqual(snapshot(before), was, "after the next Running ACK");
      assert.equal(ctx.capturedMemory!()!.context, before.context);
      assert.equal(f.reservations.length, spent, "accounting unchanged");
      if (variant === "admitting") {
        // Monitoring is still live on the same context: a new pressure episode latches it.
        const next = enroll(ctx, "after-pause");
        f.pressure(950);
        const result = await next.captured.commands.watchOutcome(next.entry);
        ctx.emit({ kind: "tool_result", payload: { tool_id: result.tool_id, result } });
        assert.equal(next.captured.context, before.context);
        assert.equal(before.commands.admitsCommands(before.context), false, "the guard still latches after pause_failed");
        assert.equal(f.reservations.length, spent + 1);
      }
    });
    assert.ok(before);
    assert.equal(before.commands.isCurrent(before.context, before.flight, before.lifecycleRevision), false,
      "the terminal exit still retires the context");
  });
}

test("a pause that parks still retires the captured memory context", async () => {
  const f = await setup();
  const publish = client.publishCheckpoint.bind(client);
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: NodeJS.ReadableStream) => {
    for await (const _chunk of pack) { /* drain */ }
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-2396" } };
  };
  try {
    await memoryRun(f, async ctx => {
      await ctx.reportIteration!(1);
      const before = ctx.capturedMemory!()!;
      commitInTree(ctx.worktreePath, "WORK.txt", "work before the pause\n");
      await ctx.checkpoint!({ reap: false });
      assert.equal(await ctx.parkForPause!({ completedCount: 0, total: 1 }), true, "a publishable pause parks");
      assert.equal(before.commands.isCurrent(before.context, before.flight, before.lifecycleRevision), false,
        "a real park retires the context");
    }, realCapture());
  } finally {
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = publish;
  }
});

for (const ackKind of ["declined", "stale_claim"] as const) {
  test(`a ${ackKind} pause_failed ACK does not keep the captured context's authority`, async () => {
    const f = await setup();
    let before: NonNullable<ReturnType<NonNullable<RunContext["capturedMemory"]>>> | undefined;
    api.stateAckWire = (body, run) => {
      if (body.status !== "pause_failed") return undefined;
      return ackKind === "declined"
        // A state ACK is applied only on a 200 (client reportStateOnce); a plain 409 is a declined report.
        ? { status: 409, body: { run: { ...run, status: "running" } } }
        : { status: 409, body: { run: { ...run, status: "running" }, disposition: "stale_claim" } };
    };
    await memoryRun(f, async ctx => {
      await ctx.reportIteration!(1);
      before = ctx.capturedMemory!()!;
      assert.equal(before.commands.admitsCommands(before.context), true);
      let pauseError: unknown;
      const parked = await failedPause(ctx).catch((error: unknown) => { pauseError = error; return false; });
      assert.equal(parked, false);
      assert.equal(api.states.at(-1)?.body.status, "pause_failed");
      // handlePausePark keeps a park best-effort: a refused report surfaces as a declined pause, not a throw.
      assert.equal(pauseError, undefined);
      // Inside the still-running flight, right after the ACK: no shell authority survives.
      assert.equal(before.commands.isCurrent(before.context, before.flight, before.lifecycleRevision), false,
        `a ${ackKind} pause_failed ACK retires the context`);
      assert.equal(before.commands.admitsCommands(before.context), false, "the captured context admits no command");
    }).catch((error: unknown) => {
      // Only the expected end of a superseded claim is tolerated; assertion failures propagate.
      if (!(ackKind === "stale_claim" && error instanceof StaleClaimError)) throw error;
    });
    assert.ok(before);
    assert.equal(before.commands.isCurrent(before.context, before.flight, before.lifecycleRevision), false,
      "no ACK path leaves the flight's authority alive past its end");
  });
}

// memoryRun must not let a runner rejection hide a body assertion failure: the stale_claim variant
// above tolerates StaleClaimError, so a lost body failure would read as a pass there. Today
// execute() handles a stale claim and resolves, so the probe injects the rejection after it.
test("memoryRun keeps a body assertion failure when execute() rejects with StaleClaimError", async () => {
  const f = await setup();
  const bodyFailure = new assert.AssertionError({ message: "probe: body assertion failed" });
  const execute = RunRunner.prototype.execute;
  RunRunner.prototype.execute = async function (this: RunRunner, claim) {
    await execute.call(this, claim);
    throw new StaleClaimError();
  };
  try {
    const outcome = await memoryRun(f, async ctx => {
      await ctx.reportIteration!(1);
      throw bodyFailure;
    }).catch((error: unknown) => {
      if (!(error instanceof StaleClaimError)) throw error;
    }).then(() => "passed", (error: unknown) => error);
    assert.equal(outcome, bodyFailure, "the body's assertion failure survives the stale-claim tolerance");
  } finally { RunRunner.prototype.execute = execute; }
});

test("after a failed pause, an answered completion question still releases its hold on the Running ACK", async () => {
  const f = await setup();
  let questionWork: Promise<unknown> | undefined;
  const rescue = setTimeout(() => api.setInputs(f.claim.run_id, [input("cancel")]), 15000);
  try {
    await memoryRun(f, async ctx => {
      try {
        await ctx.reportIteration!(1);
        const before = ctx.capturedMemory!()!;
        assert.equal(await failedPause(ctx), false);
        questionWork = ctx.askCompletionQuestion!(["m1"], { kind: "lead_request", reason: "fixture" });
        await until(() => api.states.at(-1)?.body.status === "awaiting_input");
        assert.equal(before.commands.phaseAdmits(before.context), false, "the question holds the phase");
        const question = api.states.findLast(s => s.body.status === "awaiting_input")!.body.open_question_id;
        api.setInputs(f.claim.run_id, [input("answer", JSON.stringify({ question_id: question, answers: ["continue"] }))]);
        assert.deepEqual(await questionWork, { outcome: "continue", guidance: undefined });
        await ctx.reportIteration!(2);
        assert.equal(ctx.capturedMemory!()!.context, before.context);
        assert.deepEqual(snapshot(before), { current: true, admits: true, held: false, remaining: 2 },
          "the broker's capture admits shell commands again, as in 81c950c7's answered question");
      } finally { await questionWork?.catch(() => {}); }
    });
  } finally { clearTimeout(rescue); }
});

test("a delayed declined pause_failed ACK cannot retire the context a newer Running report kept", async () => {
  const f = await setup();
  const hold = releaseGate();
  api.afterPersistState(f.claim.run_id, body => body.status === "pause_failed", hold.promise);
  // A plain 409 is a declined state report (client reportStateOnce: applied only on a 200).
  api.stateAckWire = (body, run) =>
    body.status === "pause_failed" ? { status: 409, body: { run: { ...run, status: "running" } } } : undefined;
  await memoryRun(f, async ctx => {
    await ctx.reportIteration!(1);
    const before = ctx.capturedMemory!()!;
    const was = snapshot(before);
    const pausing = failedPause(ctx);
    await until(() => api.states.some(s => s.runId === f.claim.run_id && s.body.status === "pause_failed"));
    // A newer Running report lands while the pause_failed answer is still held.
    await ctx.reportIteration!(2);
    assert.deepEqual(snapshot(before), was, "the newer Running ACK keeps the context");
    hold.resolve();
    assert.equal(await pausing, false);
    assert.deepEqual(snapshot(before), was, "the late declined ACK must not retire the current context");
    assert.equal(ctx.capturedMemory!()!.context, before.context);
    assert.equal(f.reservations.length, 0, "accounting unchanged");
  });
});
