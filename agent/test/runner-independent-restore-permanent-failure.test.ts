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
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import type { Executor } from "../src/executor.js";
import { PauseNowSignal } from "../src/steering.js";
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
  batcher: { emit(msg: unknown): void; flushOwned(signal: AbortSignal): Promise<boolean>; awaitPermanentFailureSettled(): Promise<void> };
  cancel: AbortController;
  steering: { lifecycleSignal(): AbortSignal; terminalLifecycleSignal(): AbortSignal; pendingCredentialSwitch(): number | undefined };
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
    runnerExtra: { quiesceRun: async req => { quiesce.push(req); return quiesceRunAttempt(req); } },
    ...o,
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
