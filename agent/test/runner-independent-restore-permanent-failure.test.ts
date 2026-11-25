// Issue #4644: a permanent message failure (the batcher breaker) that trips while the independent-restore route's predecessor P is
// still guarded - from route selection until the seed hands the flight over to S - must neither report `failed` nor run the
// terminal reap: P's process, clone, HOME and provision directory belong to another incarnation (#3644 amendment 15). The
// unsupported-predecessor owner hold (holdUnsupportedRestore) keeps its own park. A trip after the seed's durable success stops the
// flight with S kept and runs no model. Each row is driven through the production RunRunner.execute() with real Git, the real
// batcher, a real planted process carrying P's HOME and, per harness, a Claude-shaped or a Codex-shaped (real facade) executor.
// What is proven: no terminal install, no terminal reap (reapRecoveryProviderForSettle / reapForSink), no Codex boundary, refresh,
// release or spawn, no model start, P's bytes and process untouched, and the route does not continue past the trip.
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import type * as http from "node:http";
import type { Executor } from "../src/executor.js";
import { PauseNowSignal } from "../src/steering.js";
import { CredentialSwitchOpLedger } from "../src/credential-switch-op-ledger.js";
import { quiesceRunAttempt, setQuiescenceViewForTests, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { reapRunProcesses } from "../src/run-procs.js";
import { api, client, git, installHarness } from "./runner-harness.js";
import { scopedRealView } from "./fake-proc.js";
import { codexRig, type CodexRig } from "./runner-codex-sinks-fixture.js";
import { alive, cleanupHold, plantChild, procfs } from "./runner-bare-identity-hold-fixture.js";
import {
  FEATURES, PARK_FEATURE, assertStoppedUntouched, buildRig, cleanupRig, drive, failed, logged, resetRigRoots, rigDefaults, snapshot, type Rig,
  type RigOptions, type Snapshot,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = true;

afterEach(() => { cleanupRig(); cleanupHold(); });

const PARK_FEATURES = [...FEATURES, PARK_FEATURE];
const SWITCH_DEFERRED = "retained credential switch deferred: the restore route's predecessor is untouchable until the successor is seeded";
const CODEX_SWITCH_REFUSED = "Codex retained credential switch refused; source and session retained";
const STOPPED_LOG = "retained recovery stopped; source and accounting kept";
const HARNESSES = ["claude", "codex"] as const;
type Harness = (typeof HARNESSES)[number];

interface FlightView {
  batcher: {
    emit(msg: unknown): void; flushOwned(signal: AbortSignal): Promise<boolean>; awaitPermanentFailureSettled(): Promise<void>;
    close(...a: unknown[]): Promise<void>;
  };
  cancel: AbortController;
  steering: {
    lifecycleSignal(): AbortSignal; terminalLifecycleSignal(): AbortSignal; pendingCredentialSwitch(): number | undefined;
    credentialSwitchHoldOpen(): boolean; isCancelled(): boolean; claimFence(): unknown;
  };
  prologueSwitchHold?: { latched(): boolean };
  retainedSwitchSettlement?: string;
  runId: string;
  restorePredecessor?: unknown;
  restoreUnsupportedHold?: boolean;
  restoreTransportStopped?: boolean;
  preserveRecoveryClone?: boolean;
  retainedLocalCustody?: boolean;
  preserveSession?: boolean;
  keepGuardedInventoryOpen?: boolean;
}

interface Ctx {
  r: Rig;
  harness: Harness;
  child: number;
  pCodex?: CodexRig;
  /** P's facade counters after its priming, the baseline the zero assertions compare with. */
  primed?: { refresh: number; release: number; spawns: number };
  /** Row-owned counters: every executor either side of the swap increments `models` from its run(). */
  counts: { models: number; reapsP: number; reapsS: number };
  quiesce: QuiesceRunRequest[];
  flight?: FlightView;
  handler: { entries: Array<{ predecessorSet: boolean; hold: boolean | undefined }>; promises: Promise<unknown>[] };
  terminalInstalls: number;
  providerReaps: number;
  sinkReaps: number;
  seeds: number;
  swaps: number;
  /** Hooks a row sets before drive(); each runs before the original method continues. */
  onHold?: () => void;
  onStop?: () => void;
  onRoute?: (flight: FlightView) => void;
  onAttribution?: () => Promise<void>;
  afterSeed?: (out: { path: string }) => Promise<void>;
}

function executorFor(c: { counts: Ctx["counts"] }, codex: CodexRig | undefined, home: () => string, side: "reapsP" | "reapsS"): Executor {
  return {
    ...(codex ? { sandboxesCommands: true, safety: codex.safety, settleForCredentialFreeCapture: codex.settle, withCredentialFreeCapture: codex.capture } : {}),
    run: async () => { c.counts.models++; throw new Error("no model runs in the restore window"); },
    reclaimCachesInPlace: async () => ({ dropped: [], absent: [], failed: [], skipped: [] }),
    killAgentTree: () => {},
    reapAttributedProcesses: async opts => { c.counts[side]++; return reapRunProcesses(home(), undefined, [], opts); },
  } as Executor;
}

/** The restore window of the neighbouring window test, with row-owned counters on every executor and the spies this issue needs. */
async function arrange(harness: Harness, o: RigOptions = {}): Promise<Ctx> {
  const holder: { r?: Rig } = {};
  const counts: Ctx["counts"] = { models: 0, reapsP: 0, reapsS: 0 };
  const quiesce: QuiesceRunRequest[] = [];
  const pCodex = harness === "codex" ? codexRig() : undefined;
  // A facade that never ran a boundary cannot be disposed (the swap's disposal of P's executor would refuse), as in production
  // where P's executor has already run: prime it once, then count from a clean slate.
  const primed = pCodex ? await pCodex.safety.withBoundary({ boundary: "park", deadlineMs: 2000 }, async () => undefined).then(() => {
    pCodex.boundaries.length = 0;
    return { refresh: pCodex.refreshCalls(), release: pCodex.releaseCalls(), spawns: pCodex.processSpawns() };
  }) : undefined;
  const sCodex = harness === "codex" ? codexRig() : undefined;
  const r = await buildRig({
    addrAware: true,
    executor: executorFor({ counts }, pCodex, () => holder.r!.runHome, "reapsP"),
    successorExecutor: executorFor({ counts }, sCodex, () => path.join(holder.r!.homeRoot, `${holder.r!.runId}.g${holder.r!.claim.claim_generation}`), "reapsS"),
    ...o,
    runnerExtra: { quiesceRun: async req => { quiesce.push(req); return quiesceRunAttempt(req); }, ...o.runnerExtra },
  });
  holder.r = r;
  const c: Ctx = { r, harness, child: 0, counts, quiesce, handler: { entries: [], promises: [] }, terminalInstalls: 0, providerReaps: 0, sinkReaps: 0,
    seeds: 0, swaps: 0, ...(pCodex && primed ? { pCodex, primed } : {}) };

  const runner = r.runner as unknown as Record<string, (...a: unknown[]) => unknown>;
  const wrap = (name: string, before: (...a: unknown[]) => unknown): void => {
    const original = runner[name]!.bind(r.runner);
    mock.method(runner, name, (...args: unknown[]) => {
      const pre = before(...args);
      return pre instanceof Promise ? pre.then(() => original(...args)) : original(...args);
    });
  };
  const handlerOriginal = runner.handlePermanentFailure!.bind(r.runner);
  mock.method(runner, "handlePermanentFailure", (...args: unknown[]) => {
    const f = args[1] as FlightView;
    c.handler.entries.push({ predecessorSet: f.restorePredecessor !== undefined, hold: f.restoreUnsupportedHold });
    const p = handlerOriginal(...args) as Promise<unknown>;
    c.handler.promises.push(p);
    return p;
  });
  wrap("journalAndSendTerminal", () => { c.terminalInstalls++; });
  wrap("reapRecoveryProviderForSettle", () => { c.providerReaps++; });
  wrap("reapForSink", () => { c.sinkReaps++; });
  wrap("swapRestoreExecutor", () => { c.swaps++; });
  wrap("holdUnsupportedRestore", () => { c.onHold?.(); });
  wrap("stopRestoreRoute", () => { c.onStop?.(); });
  wrap("runRestoreRoute", (...a) => { c.flight = a[1] as FlightView; c.onRoute?.(c.flight); });

  const seed = git.seedIndependentAttempt.bind(git);
  mock.method(git, "seedIndependentAttempt", (async (...args: Parameters<typeof seed>) => {
    c.seeds++;
    const out = await seed(...args);
    await c.afterSeed?.(out);
    return out;
  }) as never);
  const read = git.readRestoreAttribution.bind(git);
  mock.method(git, "readRestoreAttribution", (async (...args: Parameters<typeof read>) => {
    const hook = c.onAttribution;
    c.onAttribution = undefined;
    await hook?.();
    return read(...args);
  }) as never);

  c.child = await plantChild(r.runHome, r.pPath);
  setQuiescenceViewForTests(scopedRealView({ pids: [c.child] }));
  return c;
}

/** Parks are confirmed as the real api's ACK confirms them (it carries the typed cause). */
function confirmParks(): void {
  const real = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
  mock.method(client, "reportState", (async (...args: unknown[]) => {
    const body = args[1] as Record<string, unknown>;
    const ack = await real(...args);
    return body.status === "recovery_wait" && typeof body.recovery_cause === "string" ? { ...ack, recoveryCause: body.recovery_cause } : ack;
  }) as never);
}

/** Arms a permanent 403 for the next message send, queues a feed line and flushes it: the batcher must trip (positive witness). */
async function trip(c: Ctx, expectPredecessor: boolean): Promise<void> {
  const flight = c.flight!;
  const entered = c.handler.entries.length;
  api.failMessagesNext(1, 403);
  flight.batcher.emit({ kind: "status", agent: "worker", payload: { text: "transport trip witness" } });
  assert.equal(await flight.batcher.flushOwned(new AbortController().signal), false, "the permanent failure refused the flush");
  await flight.batcher.awaitPermanentFailureSettled();
  assert.ok(c.handler.entries.length > entered, "the breaker entered handlePermanentFailure");
  assert.equal(c.handler.entries[c.handler.entries.length - 1]!.predecessorSet, expectPredecessor, "restorePredecessor at the handler's entry");
}

async function until(label: string, cond: () => boolean, ms = 10_000): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${label}`);
    await new Promise(resolve => setTimeout(resolve, 5));
  }
}

/** Asks the api for a credential switch at the claim's generation and waits until the flight's steering has tripped on it. */
async function tripSwitch(flight: FlightView): Promise<void> {
  api.requestCredentialSwitch(flight.runId, 2);
  await until("the credential switch to trip", () => flight.steering.pendingCredentialSwitch() === 2);
}

const touchesP = (r: Rig, req: QuiesceRunRequest): boolean =>
  req.targetPaths.some(t => t === r.pPath || t.startsWith(r.pPath + path.sep) || r.pPath.startsWith(t + path.sep));

const parks = (r: Rig) => api.states.filter(s => s.runId === r.runId && s.body.status === "recovery_wait");
const installs = (r: Rig) => api.states.filter(s => s.runId === r.runId && ["failed", "completed", "cancelled"].includes(String(s.body.status)));

/** Settles every handler call and the batcher's own capture of it before anything is asserted. */
async function settle(c: Ctx): Promise<void> {
  await Promise.allSettled(c.handler.promises);
  await c.flight?.batcher.awaitPermanentFailureSettled();
}

/** The terminal side of the flight did nothing, the model never started and P's process is alive. */
function assertNoTerminalEffects(c: Ctx): void {
  assert.ok(c.handler.entries.length >= 1, "the handler was entered");
  assert.equal(failed(c.r).length, 0, "no failed report");
  assert.equal(installs(c.r).length, 0, "no terminal report");
  assert.equal(c.terminalInstalls, 0, "no terminal install");
  assert.equal(c.providerReaps, 0, "no terminal provider reap");
  assert.equal(c.sinkReaps, 0, "no sink reap");
  assert.equal(c.counts.reapsP, 0, "P's executor was never asked to reap by HOME");
  assert.equal(c.counts.models, 0, "no model start on any executor");
  assert.equal(alive(c.child), true, "P's process survived");
  assert.deepEqual(c.quiesce.filter(req => touchesP(c.r, req)).map(req => req.site), [], "no quiescence request named P's path");
}

function assertCodexZeros(c: Ctx): void {
  if (!c.pCodex || !c.primed) return;
  assert.deepEqual(c.pCodex.boundaries, [], "no Codex withBoundary on P's facade");
  assert.equal(c.pCodex.refreshCalls(), c.primed.refresh, "no credential refresh");
  assert.equal(c.pCodex.releaseCalls(), c.primed.release, "no credential release");
  assert.equal(c.pCodex.processSpawns(), c.primed.spawns, "no Codex process spawn");
}

function assertCustodyKept(c: Ctx): void {
  const f = c.flight!;
  assert.equal(f.preserveRecoveryClone, true);
  assert.equal(f.retainedLocalCustody, true);
  assert.equal(f.preserveSession, true);
  assert.equal(f.keepGuardedInventoryOpen, true);
}

const unseeded = (c: Ctx): void => {
  assert.equal(c.seeds, 0, "no seed");
  assert.equal(c.swaps, 0, "no executor swap");
  assert.deepEqual(c.quiesce.filter(req => req.site === "attempt_seed"), [], "no attempt_seed quiescence request");
  assert.equal(logged(c.r, "independent restore seeded").length, 0, "no seeded log");
};

for (const harness of HARNESSES) {
  describe(`a permanent message failure in the restore window holds instead of failing: ${harness}`, procfs, () => {
    beforeEach(() => { resetRigRoots(); });

    it("W1: the unsupported-predecessor hold keeps its park when its guidance flush trips the breaker", async () => {
      const c = await arrange(harness, { pShape: "canonical", features: PARK_FEATURES });
      confirmParks();
      c.onHold = () => { api.failMessagesNext(1, 403); };
      const { before, traps } = await drive(c.r);
      await settle(c);
      assert.equal(c.handler.entries.some(e => e.hold === true), true, "the handler ran for the held flight");
      assertNoTerminalEffects(c);
      assert.equal(parks(c.r).length, 1, "exactly one park");
      assert.equal(parks(c.r)[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
      assert.equal(parks(c.r)[0]!.body.claim_generation, c.r.claim.claim_generation);
      assert.equal(logged(c.r, "restore unsupported predecessor held").length, 1);
      assert.ok(logged(c.r, "guidance_unconfirmed").length >= 1, "the guidance flush was unconfirmed");
      unseeded(c);
      assertCodexZeros(c);
      await assertStoppedUntouched(c.r, before, traps);
      assert.equal(c.r.counters.reclaims, 0);
    });

    it("W2a: the item-7 refusal line's send trips the breaker; the flight stops without failing or reaping", async () => {
      const c = await arrange(harness, { pBoundary: "invalid" });
      c.onStop = () => { api.failMessagesNext(1, 403); };
      const { before, traps } = await drive(c.r);
      await settle(c);
      assertNoTerminalEffects(c);
      assert.equal(logged(c.r, "independent restore refused; predecessor retained").length, 1);
      assert.equal(parks(c.r).length, 0, "no park");
      assertCustodyKept(c);
      unseeded(c);
      assertCodexZeros(c);
      await assertStoppedUntouched(c.r, before, traps);
    });

    it("W2b: a trip during classification stops the eligible route before any seed", async () => {
      const c = await arrange(harness);
      c.onAttribution = () => trip(c, true);
      const { before, traps } = await drive(c.r);
      await settle(c);
      assertNoTerminalEffects(c);
      assertCustodyKept(c);
      unseeded(c);
      assertCodexZeros(c);
      await assertStoppedUntouched(c.r, before, traps);
    });

    it("W2b-pause: the same trip with a spent cancel (PauseNowSignal) still stops before any seed", async () => {
      const c = await arrange(harness);
      c.onRoute = flight => { flight.cancel.abort(new PauseNowSignal()); };
      c.onAttribution = () => trip(c, true);
      const { before, traps } = await drive(c.r);
      await settle(c);
      assertNoTerminalEffects(c);
      assert.equal(c.flight!.restoreTransportStopped, true, "the sticky flag carries the stop the spent cancel could not");
      unseeded(c);
      assertCodexZeros(c);
      assert.equal(failed(c.r).length, 0);
      assert.deepEqual((await snapshot(c.r)).clone, before.clone);
      assert.deepEqual((await snapshot(c.r)).home, before.home);
      assert.deepEqual((await snapshot(c.r)).provision, before.provision);
      void traps;
    });

    it("switch race: an active credential switch keeps precedence over the breaker's stop", async () => {
      const c = await arrange(harness);
      c.onAttribution = async () => { await tripSwitch(c.flight!); await trip(c, true); };
      const { before, traps } = await drive(c.r);
      await settle(c);
      assertNoTerminalEffects(c);
      // The Codex executor takes its own retained-switch arm (its credential-free capture boundary is unavailable in the window).
      assert.equal(logged(c.r, harness === "codex" ? CODEX_SWITCH_REFUSED : SWITCH_DEFERRED).length, 1, "the harness's switch arm ran once");
      assert.equal(logged(c.r, STOPPED_LOG).length, 0, "the breaker's stop did not preempt the switch");
      unseeded(c);
      assertCodexZeros(c);
      await assertStoppedUntouched(c.r, before, traps);
    });

    it("post-seed: a trip while the seed returns stops with S kept and starts no model", async () => {
      const c = await arrange(harness);
      c.onRoute = flight => { flight.cancel.abort(new PauseNowSignal()); };
      let sPath: string | undefined;
      c.afterSeed = async out => {
        sPath = out.path;
        const f = c.flight!;
        assert.equal(f.cancel.signal.aborted, true, "the cancel was already spent");
        assert.equal(f.steering.lifecycleSignal().aborted, false, "steering's lifecycle is unaborted");
        assert.equal(f.steering.terminalLifecycleSignal().aborted, false, "steering's terminal lifecycle is unaborted");
        await trip(c, true);
      };
      const before: Snapshot = await snapshot(c.r);
      await drive(c.r).catch(() => undefined);
      await settle(c);
      assert.equal(c.seeds, 1, "the seed ran once");
      assert.ok(sPath && fs.existsSync(sPath), "S's seeded clone exists");
      assertNoTerminalEffects(c);
      assertCustodyKept(c);
      assert.equal(c.counts.models, 0);
      assertCodexZeros(c);
      const after = await snapshot(c.r);
      assert.equal(after.clone, before.clone, "P's clone is byte-identical");
      assert.equal(after.home, before.home, "P's HOME is byte-identical");
      assert.equal(after.provision, before.provision, "P's provision directory is byte-identical");
    });

    it("control: without a trip the eligible route seeds and swaps", async () => {
      const c = await arrange(harness);
      await drive(c.r);
      await settle(c);
      assert.ok(c.seeds >= 1, "seeded");
      assert.ok(c.swaps >= 1, "swapped");
      assert.equal(logged(c.r, "independent restore seeded").length, 1);
      assert.equal(c.handler.entries.length, 0, "no breaker trip");
      assert.equal(alive(c.child), true);
    });
  });
}

// Issue #4652: an op-fenced credential switch that arrives after the restore transport stopped (Claude harness only; Codex keeps its
// own retained-switch arm, covered by the "switch race" row above). The restore-route predecessor fence must still be reached.
const OP = "op-1";
const NONCE = "n".repeat(43);
const HELD_LOG = "retained credential switch held; run processes unproven, work and custody retained";
const HOOK_LOG = "restore transport stopped; a pending credential switch is held through the op-fenced fence";
const HOOK_FAILED_LOG = "late credential switch hold after the restore transport stopped did not complete; run left non-terminal, custody kept";
const BUDGET_MS = 300;
/** Slack over BUDGET_MS for the late hold itself (one in-flight request and its retry sleep). */
const HOLD_SLACK_MS = 2000;
/** Separate slack for the final hold-settled ack, which runs on the shutdown signal with the client terminal retry schedule. */
const SETTLED_ACK_SLACK_MS = 5000;
/** Allowance for the snapshot and restore setup that runs inside drive() before the hold starts. */
const DRIVE_SETUP_SLACK_MS = 3000;
/** Loopback latency between a hold report passing its deadline guard and the fake api receiving it. */
const WIRE_TOLERANCE_MS = 250;
/** Outer watchdog for a drive whose hold is budgeted: the hold, the settled acknowledgement and the setup, each named. */
const BUDGETED_DRIVE_MS = BUDGET_MS + HOLD_SLACK_MS + SETTLED_ACK_SLACK_MS + DRIVE_SETUP_SLACK_MS;

/** Races a budgeted drive against BUDGETED_DRIVE_MS; the watchdog timer is cleared either way so it never outlives the test. */
async function driveWithinBudget(run: Promise<unknown>): Promise<"done" | "timeout"> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([
      run.then(() => "done" as const),
      new Promise<"timeout">(resolve => { timer = setTimeout(() => resolve("timeout"), BUDGETED_DRIVE_MS); }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

describe("an op-fenced credential switch after the restore transport stopped reaches the predecessor hold: claude", procfs, () => {
  const ledgerDirs: string[] = [];
  let restoreClient: (() => void) | undefined;
  beforeEach(() => { resetRigRoots(); });
  afterEach(() => {
    restoreClient?.();
    restoreClient = undefined;
    for (const dir of ledgerDirs.splice(0)) fs.rmSync(dir, { recursive: true, force: true });
  });

  /** buildRig resets client.protocolFeatures, so the in-place apply capability is negotiated after the rig exists. */
  function negotiate(): void {
    const c = client as unknown as { registerNonce?: string; hasInPlaceCredentialSwitchCapability: boolean };
    const prior = { nonce: c.registerNonce, capability: c.hasInPlaceCredentialSwitchCapability };
    client.protocolFeatures = [...client.protocolFeatures, "credential_switch_hold", "credential_switch_apply_v1"];
    c.registerNonce = NONCE;
    c.hasInPlaceCredentialSwitchCapability = true;
    restoreClient = () => { c.registerNonce = prior.nonce; c.hasInPlaceCredentialSwitchCapability = prior.capability; };
  }

  async function arrangeSwitch(o: { ledger: boolean; extra?: RigOptions["runnerExtra"] }): Promise<Ctx> {
    const extra: NonNullable<RigOptions["runnerExtra"]> = { ...o.extra };
    if (o.ledger) {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "restore-switch-ledger-"));
      ledgerDirs.push(dir);
      extra.credentialSwitchLedger = new CredentialSwitchOpLedger(dir);
    }
    const c = await arrange("claude", { runnerExtra: extra });
    negotiate();
    return c;
  }

  const gen = (c: Ctx): number => c.r.claim.claim_generation!;
  const holds = (c: Ctx, status: string) => api.holdRequests.filter(h => h.runId === c.r.runId && h.status === status);
  const heldReports = (c: Ctx) => holds(c, "credential_switch_held");

  /** Order 1 with the ledger: the switch is requested while the prologue hold is open, so it is latched (not pending). */
  async function latchSwitch(c: Ctx, ms = 10_000): Promise<void> {
    const f = c.flight!;
    assert.equal(f.steering.credentialSwitchHoldOpen(), true, "the prologue hold is open");
    assert.ok(f.prologueSwitchHold, "the flight carries the prologue hold");
    api.requestCredentialSwitch(f.runId, gen(c), OP);
    await until("the switch to be latched", () => f.prologueSwitchHold!.latched(), ms);
    assert.equal(f.steering.pendingCredentialSwitch(), undefined, "latched, not pending");
  }

  function assertHeldOnce(c: Ctx): void {
    const held = heldReports(c);
    assert.equal(held.length, 1, "exactly one credential_switch_held");
    assert.equal(held[0]!.op_id, OP);
    assert.equal(held[0]!.claim_generation, gen(c));
    assert.equal(logged(c.r, HOOK_LOG).length + logged(c.r, "in-place credential switch deferred to a hold: the restore route's predecessor is untouchable until the successor is seeded").length >= 1, true,
      "the predecessor branch ran");
    assert.equal(logged(c.r, HELD_LOG).length, 1, "the hold was confirmed");
    assert.equal(holds(c, "credential_switch_hold_settled").length, 1, "the hold was settled after the join");
    assertNoTerminalEffects(c);
    assertCustodyKept(c);
  }

  const reason = (c: Ctx): string | undefined => (c.flight!.cancel.signal.reason as { constructor: { name: string } } | undefined)?.constructor.name;

  it("Order 1, ledger: a switch latched by the prologue hold is served and held through the op-fenced fence", async () => {
    const c = await arrangeSwitch({ ledger: true });
    c.onAttribution = async () => { await trip(c, true); await latchSwitch(c); };
    const { before, traps } = await drive(c.r);
    await settle(c);
    assertHeldOnce(c);
    assert.equal(logged(c.r, HOOK_LOG).length, 1, "the late hook held the latched switch");
    assert.equal(logged(c.r, STOPPED_LOG).length, 1);
    assert.equal(reason(c), "RestoreTransportStop");
    unseeded(c);
    await assertStoppedUntouched(c.r, before, traps);
  });

  it("Order 1, no ledger (Path A): the CredentialSwitchSignal arm reaches the predecessor hold", async () => {
    const c = await arrangeSwitch({ ledger: false });
    c.onAttribution = async () => {
      assert.equal(c.flight!.prologueSwitchHold, undefined, "no ledger, so no prologue hold");
      await trip(c, true);
      api.requestCredentialSwitch(c.flight!.runId, gen(c), OP);
      await until("the credential switch to trip", () => c.flight!.steering.pendingCredentialSwitch() === gen(c));
    };
    const { before, traps } = await drive(c.r);
    await settle(c);
    assertHeldOnce(c);
    assert.equal(reason(c), "RestoreTransportStop");
    assert.equal(logged(c.r, HOOK_LOG).length, 0, "the late hook was not needed on Path A");
    unseeded(c);
    await assertStoppedUntouched(c.r, before, traps);
  });

  it("Path A budget control: an unreachable api ends the CredentialSwitchSignal arm's hold within the verification allowance", async () => {
    const c = await arrangeSwitch({ ledger: false, extra: { retainedSwitchVerificationMs: BUDGET_MS, recoveryRetryMs: 5 } });
    c.onAttribution = async () => {
      await trip(c, true);
      api.requestCredentialSwitch(c.flight!.runId, gen(c), OP);
      await until("the credential switch to trip", () => c.flight!.steering.pendingCredentialSwitch() === gen(c));
      api.failStateNext(100_000, 503);
    };
    const outcome = await driveWithinBudget(drive(c.r));
    assert.equal(outcome, "done", "the Path A hold ended within its budget instead of retrying without end");
    await settle(c);
    assert.ok(api.stateWire.some(s => s.runId === c.r.runId && s.body.status === "credential_switch_held"), "the hold report was attempted");
    assert.equal(logged(c.r, HELD_LOG).length, 0, "no hold was confirmed");
    assert.equal(logged(c.r, HOOK_LOG).length, 0, "the late hook was not needed on Path A");
    assert.equal(reason(c), "RestoreTransportStop");
    assertNoTerminalEffects(c);
    assertCustodyKept(c);
    unseeded(c);
  });

  /** Requests the switch from inside the batcher's first close after the stop line, i.e. after the RetainedRecoveryStop arm began. */
  function switchAtStopClose(c: Ctx, before?: () => void): void {
    c.onRoute = flight => {
      const original = flight.batcher.close.bind(flight.batcher);
      let armed = true;
      flight.batcher.close = async (...a: unknown[]) => {
        if (armed && logged(c.r, STOPPED_LOG).length > 0) {
          armed = false;
          before?.();
          api.requestCredentialSwitch(flight.runId, gen(c), OP);
          await until("the late switch to trip", () => flight.steering.pendingCredentialSwitch() === gen(c));
        }
        return original(...a);
      };
    };
  }

  it("Order 2a, ledger: a switch that arrives during the stop arm is held by the late hook", async () => {
    const c = await arrangeSwitch({ ledger: true });
    c.onAttribution = () => trip(c, true);
    switchAtStopClose(c);
    const { before, traps } = await drive(c.r);
    await settle(c);
    assert.equal(logged(c.r, HOOK_LOG).length, 1, "the late hook attempted the hold");
    assertHeldOnce(c);
    assert.equal(logged(c.r, STOPPED_LOG).length, 1);
    assert.equal(reason(c), "RestoreTransportStop");
    unseeded(c);
    await assertStoppedUntouched(c.r, before, traps);
  });

  it("Order 2a budget control: an unreachable api ends the late hold within the verification allowance, leaving the settle owed", async () => {
    const c = await arrangeSwitch({ ledger: true, extra: { retainedSwitchVerificationMs: BUDGET_MS, recoveryRetryMs: 5 } });
    const runner = c.r.runner as unknown as Record<string, (...a: unknown[]) => unknown> & { pendingHoldSettled: Map<string, unknown> };
    const hook: { start?: number; end?: number; wireAtEnd?: number } = {};
    const hookOriginal = runner.holdSwitchAfterTransportStop!.bind(runner);
    mock.method(runner, "holdSwitchAfterTransportStop", async (...args: unknown[]) => {
      hook.start = Date.now();
      try { return await hookOriginal(...args); } finally { hook.end = Date.now(); hook.wireAtEnd = api.stateWire.length; }
    });
    const joinOriginal = runner.joinRetainedSwitchOperations!.bind(runner);
    const atJoin: Array<string | undefined> = [];
    mock.method(runner, "joinRetainedSwitchOperations", async (...args: unknown[]) => {
      atJoin.push((args[0] as FlightView).retainedSwitchSettlement);
      return joinOriginal(...args);
    });
    c.onAttribution = () => trip(c, true);
    let armedAt = -1;
    // Arrival time of every hold report, so each send can be checked against the hook's verification deadline.
    const heldAt: number[] = [];
    const fake = api as unknown as { handleState: (res: http.ServerResponse, runId: string, json: Record<string, unknown>) => Promise<void> };
    const handleState = fake.handleState.bind(api);
    const heldClock = mock.method(fake, "handleState", async (res: http.ServerResponse, runId: string, json: Record<string, unknown>) => {
      if (runId === c.r.runId && json.status === "credential_switch_held") heldAt.push(Date.now());
      return handleState(res, runId, json);
    });
    switchAtStopClose(c, () => { armedAt = api.stateWire.length; api.failStateNext(100_000, 503); });
    let outcome: string;
    try {
      outcome = await driveWithinBudget(drive(c.r));
    } finally {
      heldClock.mock.restore();
    }
    // The final credential_switch_hold_settled ack is not part of the hold budget: it runs on the shutdown signal with the
    // client's terminal retry schedule, so it gets its own slack (SETTLED_ACK_SLACK_MS) on top of the hold allowance.
    assert.equal(outcome, "done", "drive settled within the hold budget plus the bounded settled acknowledgement");
    await settle(c);
    assert.ok(armedAt >= 0, "the api failure was armed inside the stop arm");
    assert.ok(hook.start !== undefined && hook.end !== undefined && hook.wireAtEnd !== undefined, "the late hook ran");
    assert.ok(hook.end - hook.start < BUDGET_MS + HOLD_SLACK_MS, `the late hold ended within its allowance (took ${hook.end - hook.start} ms)`);
    const sent = api.stateWire.slice(armedAt).filter(s => s.runId === c.r.runId).map(s => String(s.body.status));
    const own = (from: number, to?: number) => api.stateWire.slice(from, to).filter(s => s.runId === c.r.runId);
    const heldAttempts = own(armedAt, hook.wireAtEnd).filter(s => s.body.status === "credential_switch_held");
    assert.ok(heldAttempts.length >= 1, "the hold report was attempted");
    assert.equal(heldAttempts.every(s => (s.body as { op_id?: string }).op_id === OP), true);
    const lateHolds = heldAt.filter(t => t >= hook.start! + BUDGET_MS + WIRE_TOLERANCE_MS);
    assert.deepEqual(lateHolds.map(t => t - hook.start!), [], "every hold report was sent before the verification deadline");
    assert.equal(own(hook.wireAtEnd).some(s => s.body.status === "credential_switch_held"), false, "no hold report after the hook ended");
    assert.equal(own(armedAt, hook.wireAtEnd).some(s => s.body.status === "credential_switch_hold_settled"), false,
      "the settled acknowledgement is not sent inside the hold budget");
    assert.ok(own(hook.wireAtEnd).some(s => s.body.status === "credential_switch_hold_settled"), "the settled acknowledgement was attempted after the hook ended");
    assert.deepEqual([...new Set(sent)].filter(s => s !== "credential_switch_held" && s !== "credential_switch_hold_settled"), [],
      "only hold reports were sent after the failure was armed");
    assert.equal(logged(c.r, HELD_LOG).length, 0, "no hold was confirmed");
    assert.equal(api.holdState(c.r.runId), undefined);
    assertNoTerminalEffects(c);
    assertCustodyKept(c);
    assert.deepEqual(atJoin, ["hold_pending"], "the owed settle was preserved up to the join");
    assert.equal(runner.pendingHoldSettled.has(`${c.r.runId}:${gen(c)}`), true, "the failed settled acknowledgement is retried later");
    unseeded(c);
  });

  // recoveryRetryMs sets the in-place report backoff: 5 ms lands the cancel during a report, 3 s during the backoff wait.
  for (const [when, recoveryRetryMs] of [["during a report", 5], ["during the backoff", 3000]] as const) it(`Order 2a cancel ${when}: an owner cancel observed while the late hold retries is reported once, custody kept`, async () => {
    const c = await arrangeSwitch({ ledger: true, extra: { retainedSwitchVerificationMs: 20_000, recoveryRetryMs } });
    // The in-place server drains inputs (a cancel included) while the switch is pending.
    api.inPlaceInputDrain = true;
    let holdFailure: { mock: { restore(): void } } | undefined;
    try {
      c.onAttribution = () => trip(c, true);
      // Only the hold reports fail (a transient 503), so the cancellation report itself can land.
      let failHolds = false;
      let holdAttempts = 0;
      const fake = api as unknown as { handleState: (res: http.ServerResponse, runId: string, json: Record<string, unknown>) => Promise<void> };
      const handleState = fake.handleState.bind(api);
      holdFailure = mock.method(fake, "handleState", async (res: http.ServerResponse, runId: string, json: Record<string, unknown>) => {
        if (failHolds && runId === c.r.runId && json.status === "credential_switch_held") {
          holdAttempts++;
          res.writeHead(503, { "content-type": "application/json" });
          res.end(JSON.stringify({ error: "injected failure" }));
          return;
        }
        return handleState(res, runId, json);
      });
      let cancelling: Promise<void> | undefined;
      switchAtStopClose(c, () => {
        failHolds = true;
        cancelling = (async () => {
          await until("a hold report attempt", () => holdAttempts > 0);
          api.appendInputs(c.r.runId, [{ id: 1, kind: "cancel", body: null }]);
          await until("the cancel", () => c.flight!.steering.isCancelled());
        })();
      });
      await drive(c.r);
      await cancelling;
      await settle(c);
      assert.equal(logged(c.r, HOOK_LOG).length, 1, "the late hook attempted the hold");
      assert.equal(logged(c.r, HELD_LOG).length, 0, "no hold was confirmed");
      const cancelled = failed(c.r);
      assert.equal(cancelled.length, 1, "exactly one cancellation report");
      assert.equal((cancelled[0]!.body as { failure_reason?: string }).failure_reason, "run cancelled", "it reports the owner cancel");
      assert.equal(logged(c.r, HOOK_FAILED_LOG).length, 0, "the cancel is not logged as an incomplete hold");
      assert.equal(c.counts.models, 0, "no model start");
      assert.equal(alive(c.child), true, "P's process survived");
      assertCustodyKept(c);
      assert.ok(holds(c, "credential_switch_hold_settled").length >= 1 || (c.r.runner as unknown as { pendingHoldSettled: Map<string, unknown> }).pendingHoldSettled.has(`${c.r.runId}:${gen(c)}`),
        "the owed hold_pending settle was sent or queued by the join");
    } finally {
      holdFailure?.mock.restore();
      api.inPlaceInputDrain = false;
    }
  });

  it("Order 2b: a switch that arrives only after the flight ended holds nothing", async () => {
    const c = await arrangeSwitch({ ledger: true });
    c.onAttribution = () => trip(c, true);
    const { before, traps } = await drive(c.r);
    await settle(c);
    api.requestCredentialSwitch(c.r.runId, gen(c), OP);
    await new Promise(resolve => setTimeout(resolve, 50));
    assert.equal(heldReports(c).length, 0, "no credential_switch_held");
    assert.equal(logged(c.r, HOOK_LOG).length, 0);
    assert.equal(logged(c.r, HOOK_FAILED_LOG).length, 0);
    assert.equal(logged(c.r, STOPPED_LOG).length, 1);
    assertNoTerminalEffects(c);
    assertCustodyKept(c);
    unseeded(c);
    await assertStoppedUntouched(c.r, before, traps);
  });

  describe("controls: a lifecycle that already decided the exit is not overridden by the switch", () => {
    /** stopArm: how often the RetainedRecoveryStop arm logs (default 1; a fence is dispatched as a stale claim, never through it). */
    interface Expect { failed: number; terminalInstalls: number; stopArm?: number }
    /** The outcome the base (before issue #4652) produces. A cancel or fence that arrives after the switch was latched is withheld
     *  by steering (the switch keeps precedence, as in the "switch race" row), so only shutdown is also driven in that order. */
    interface Outcomes { control_first: Expect; switch_first?: Expect }
    const control = (name: string, act: (c: Ctx) => Promise<void>, outcomes: Outcomes): void => {
      const orders = outcomes.switch_first ? (["the control, then the switch", "the switch latched, then the control"] as const) : (["the control, then the switch"] as const);
      for (const order of orders) {
        it(`${name}: ${order}`, async () => {
          const c = await arrangeSwitch({ ledger: true });
          let hookError: unknown;
          c.onAttribution = async () => {
            try {
              await trip(c, true);
              if (order === "the control, then the switch") {
                await act(c);
                // Steering stops polling once the lifecycle ended, so the switch can only be requested, never observed.
                api.requestCredentialSwitch(c.flight!.runId, gen(c), OP);
              } else {
                await latchSwitch(c);
                await act(c);
              }
            } catch (error) {
              hookError = error;
              throw error;
            }
          };
          await drive(c.r);
          await settle(c);
          api.failInputReceipts("ack", undefined);
          assert.equal(hookError, undefined, "the arrangement completed (a rejected control or latch must not pass silently)");
          assert.equal(heldReports(c).length, 0, "no credential_switch_held");
          assert.equal(holds(c, "credential_switch_hold_settled").length, 0);
          const expectOutcome = order === "the control, then the switch" ? outcomes.control_first : outcomes.switch_first!;
          assert.equal(logged(c.r, STOPPED_LOG).length, expectOutcome.stopArm ?? 1, "the stop arm count");
          // A decided exit never logs the late hold as held; only a switch pending at the hook (switch first) warns that it did not run.
          assert.equal(logged(c.r, HOOK_LOG).length, 0, "no late hold logged as held");
          assert.equal(logged(c.r, HOOK_FAILED_LOG).length, order === "the switch latched, then the control" ? 1 : 0, "late hold refusal warn");
          assert.equal(failed(c.r).length, expectOutcome.failed, "failed reports");
          assert.equal(c.terminalInstalls, expectOutcome.terminalInstalls, "terminal installs");
          assert.equal(c.counts.models, 0, "no model start");
          assert.equal(alive(c.child), true, "P's process survived");
          assertCustodyKept(c);
        });
      }
    };
    control("cancel", async c => {
      api.appendInputs(c.r.runId, [{ id: 1, kind: "cancel", body: null }]);
      await until("the cancel", () => c.flight!.steering.isCancelled());
    }, { control_first: { failed: 1, terminalInstalls: 1 } });
    control("shutdown", async c => { c.r.runner.shutdown(); }, { control_first: { failed: 0, terminalInstalls: 0 }, switch_first: { failed: 0, terminalInstalls: 0 } });
    control("stale fence", async c => {
      // A fence is only ever observed through an input receipt: an operator row is polled, then its ACK is refused as stale.
      api.appendInputs(c.r.runId, [{ id: 1, kind: "follow_up", body: "x" }]);
      api.failInputReceipts("ack", 409, "stale");
      await until("the fence", () => c.flight!.steering.claimFence() !== undefined, 3000);
    }, { control_first: { failed: 0, terminalInstalls: 0, stopArm: 0 } });
  });
});
