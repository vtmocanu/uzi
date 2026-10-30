import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ActiveSnapshot, ClaimResponse, ChatClaimResponse, StateAck, StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { sleep } from "../src/util.js";

// PRD #1391 Run B M4 — the worker BOOT claim gate (D7/D9/SC3): the heartbeat loop starts FIRST, every
// pending terminal journal is resolved BEFORE the claim loops start, and the claim loops stay closed
// while the pending set exceeds the cap — keyed on pending-vs-cap, NEVER on which wire features are
// negotiated (so a strict-decode rollback cannot reopen them while a journal is unresolved).

const RUN = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const RUN2 = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb";

const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
const noReview = { execute: async () => {} } as unknown as ReviewRunner;
const idleRunner = { recoveryInventoryPending: async () => false, resumePendingRecoveries: async () => {}, execute: async () => {} } as unknown as RunRunner;
const idleChat = { execute: async () => {} } as unknown as ChatRunner;
const okPreflight = (): { ok: boolean; missing: string[] } => ({ ok: true, missing: [] });

function fakeConfig(over: Partial<Config> = {}): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dockerWiring: {},
    dataDir: "/tmp/does-not-matter",
    gapFillMax: 100,
    outboxTerminalMaxBytes: 1 << 20,
    ...over,
  } as unknown as Config;
}

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fsp.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(): Promise<Outbox> {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "worker-boot-gate-"));
  tmpRoots.push(dir);
  const outbox = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await outbox.init();
  return outbox;
}

interface FakeClientHooks {
  heartbeat?: () => Promise<void>;
  reportState?: (runId: string, body: StateRequest) => Promise<StateAck>;
  claimRun?: () => Promise<ClaimResponse | null>;
  hasFeature?: (f: string) => boolean;
}

/** A programmable client: register/heartbeat/claim idle by default; reportState (the boot terminal
 *  resolve send) and hasFeature (the rollback signal) are overridable. */
function fakeClient(hooks: FakeClientHooks = {}): WorkerClient {
  return {
    register: async () => ({}),
    getRunOwnership: async () => ({ status: "completed", claim_generation: Number.MAX_SAFE_INTEGER }),
    heartbeat: hooks.heartbeat ?? (async () => {}),
    reportState: hooks.reportState ?? (async () => ({ applied: true, status: "completed" }) as StateAck),
    claimRun: hooks.claimRun ?? (async (): Promise<ClaimResponse | null> => null),
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
    hasFeature: hooks.hasFeature ?? (() => false),
    getMessageGaps: async () => ({ gaps: [] }),
    postMessages: async () => {},
  } as unknown as WorkerClient;
}

async function pollUntil(pred: () => boolean, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (pred()) return;
    await sleep(5);
  }
  assert.fail(`timed out waiting for: ${label}`);
}

describe("Worker boot claim gate (PRD #1391 Run B M4)", () => {
  it("boot terminal G2 settlement checks G1 custody separately before exact finalize retirement", async () => {
    const outbox = await mkOutbox();
    await outbox.journalTerminal(RUN, 2, "running", 0, { status: "completed" });
    await outbox.journalFinalize(RUN, 1);
    await outbox.journalFinalize(RUN, 2);
    const checked: number[] = [];
    const worker = new Worker(fakeConfig(), fakeClient(), {
      recoveryInventoryPending: async (_runId: string, generation: number) => {
        checked.push(generation);
        return generation === 1;
      },
    } as unknown as RunRunner, idleChat, noJudge, noReview, nullLogger(), okPreflight, outbox);
    await (worker as unknown as { resolveBootTerminals(signal: AbortSignal): Promise<void> })
      .resolveBootTerminals(new AbortController().signal);
    assert.equal(outbox.hasPendingTerminal(RUN, 2), false);
    assert.deepEqual(checked.sort(), [1, 2]);
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN, claim_generation: 1 }]);
  });

  it("heartbeat starts FIRST; the claim loop does not start until the pending terminal is resolved", async () => {
    const outbox = await mkOutbox();
    await outbox.journalTerminal(RUN, 4, "running", 0, { status: "completed" });
    assert.equal(outbox.hasPendingTerminal(RUN, 4), true, "precondition: one pending terminal journal");

    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);

    let heartbeats = 0;
    let claims = 0;
    let reportStates = 0;
    let releaseReport!: () => void;
    const reportGate = new Promise<void>((r) => (releaseReport = r));

    const client = fakeClient({
      heartbeat: async () => {
        heartbeats += 1;
      },
      claimRun: async () => {
        claims += 1;
        return null;
      },
      // The boot terminal resolve send: BLOCK it so the boot gate is held open, then apply (200) so
      // the journal retires and the claim loop is released.
      reportState: async () => {
        reportStates += 1;
        await reportGate;
        return { applied: true, status: "completed" } as StateAck;
      },
    });

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      client,
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
      registry,
    );
    const done = worker.run(controller.signal);

    // While the boot terminal resolve is held open, the heartbeat loop is ticking but the claim loop
    // has NOT started (resolveBootTerminals is awaited before the claim loops).
    await pollUntil(() => heartbeats >= 1 && reportStates >= 1, 2000, "heartbeat + boot resolve both started");
    await sleep(40); // give the claim loop every chance to (wrongly) fire
    assert.equal(claims, 0, "the claim loop must not claim before the pending terminal is resolved");
    assert.ok(heartbeats >= 1, "the heartbeat loop started first, keeping the pending lease refreshed");

    // Resolve the terminal (200 → retire). The pending set empties, so the boot gate clears and the
    // claim loop starts.
    releaseReport();
    await pollUntil(() => claims >= 1, 2000, "the claim loop starts once the terminal is resolved");
    assert.equal(outbox.hasPendingTerminal(RUN, 4), false, "the resolved terminal was retired");

    controller.abort();
    await done;
  });

  it("issue #2950: a boot with only a run_not_owned blocked journal does not pause the claim loop", async () => {
    const outbox = await mkOutbox();
    await outbox.journalTerminal(RUN, 4, "running", 0, { status: "completed" });
    await outbox.markTerminalBlocked(RUN, 4, "run_not_owned");
    // Cap 0 (the cap-independent floor): counted, this entry would pause claims forever.
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);
    assert.equal(registry.claimsPausedByPendingOverflow(), false);
    let claims = 0;
    let reportStates = 0;
    const client = fakeClient({
      claimRun: async () => { claims += 1; return null; },
      reportState: async () => { reportStates += 1; return { applied: true, status: "completed" } as StateAck; },
    });
    const controller = new AbortController();
    const worker = new Worker(fakeConfig(), client, idleRunner, idleChat, noJudge, noReview, nullLogger(),
      okPreflight, outbox, new Map(), registry);
    const done = worker.run(controller.signal);
    await pollUntil(() => claims >= 1, 2000, "the claim loop starts despite the blocked journal");
    assert.equal(reportStates, 0, "a blocked journal is never sent");
    assert.equal(outbox.hasPendingTerminal(RUN, 4), true, "the journal is kept");
    controller.abort();
    await done;
  });

  it("issue #1742: a boot-resolved terminal also retires the run's finalize records at generation <= G", async () => {
    // A crash between installing G's terminal journal and retiring G's finalize record leaves both
    // files. The lister excludes the run while the journal is pending; once the boot resolve settles
    // the journal the finalize record must go too, or it blocks the retention sweep.
    const outbox = await mkOutbox();
    await outbox.journalFinalize(RUN, 3); // older generation, superseded by G
    await outbox.journalFinalize(RUN, 4);
    await outbox.journalFinalize(RUN, 6); // a higher generation is not G's to retire
    await outbox.journalFinalize(RUN2, 1); // another run is untouched
    await outbox.journalTerminal(RUN, 4, "running", 0, { status: "completed" });
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN2, claim_generation: 1 }], "precondition: RUN is excluded");

    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(), Object.assign(fakeClient(), { getRunOwnership: async () => ({ status: "completed", claim_generation: 4 }) }),
      idleRunner, idleChat, noJudge, noReview, nullLogger(), okPreflight,
      outbox, new Map(), registry,
    );
    const done = worker.run(controller.signal);
    try {
      await pollUntil(() => !outbox.hasPendingTerminal(RUN, 4), 2000, "the boot resolve retired the terminal");
      const runDir = path.join(tmpRoots[tmpRoots.length - 1]!, "outbox", RUN);
      const finalizeNames = async (): Promise<string[]> =>
        (await fsp.readdir(runDir)).filter((n) => n.startsWith("finalize-")).sort();
      // Wait on the files themselves: the per-run deduped list shows gen 6 before gens 3 and 4 are unlinked.
      let names: string[] = await finalizeNames();
      for (let i = 0; i < 200 && names.join() !== "finalize-6.json"; i++) {
        await sleep(10);
        names = await finalizeNames();
      }
      // RUN2's record was offered on the accepted register and retired there; RUN keeps only gen 6.
      assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN, claim_generation: 6 }]);
      // The list dedupes by run (highest generation), so assert the files themselves.
      const files = (await fsp.readdir(path.join(tmpRoots[tmpRoots.length - 1]!, "outbox", RUN)))
        .filter((n) => n.startsWith("finalize-"))
        .sort();
      assert.deepEqual(files, ["finalize-6.json"], "gens 3 and 4 were retired, the higher gen 6 kept");
    } finally {
      controller.abort();
      await done;
    }
  });

  it("issue #1742: a terminal the boot resolve leaves listed keeps its finalize record", async () => {
    const outbox = await mkOutbox();
    await outbox.journalFinalize(RUN, 4);
    await outbox.journalTerminal(RUN, 4, "running", 0, { status: "completed" });
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    let reports = 0;
    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      // A benign non-terminal 409: the boot resolve KEEPS the journal pending.
      fakeClient({ reportState: async () => { reports += 1; return { applied: false, status: "running" } as StateAck; } }),
      idleRunner, idleChat, noJudge, noReview, nullLogger(), okPreflight, outbox, new Map(), registry,
    );
    const done = worker.run(controller.signal);
    await pollUntil(() => reports >= 1, 2000, "the boot resolve ran");
    await sleep(40);
    assert.equal(outbox.hasPendingTerminal(RUN, 4), true, "the journal stays pending");
    controller.abort();
    await done;
    // Retire the journal by hand: the finalize record must still be there to list.
    await outbox.retireTerminal(RUN, 4);
    assert.deepEqual(outbox.listPendingFinalizes(), [{ run_id: RUN, claim_generation: 4 }]);
  });

  it("a strict-decode rollback does NOT reopen the claim loops while a journal is still unresolved (D9)", async () => {
    // Two pending terminals with a cap of 0 (undefined getter) ⇒ pending_overflow, so the claim loop
    // gate stays CLOSED. The boot resolve keeps the journals (the api answers a benign non-terminal
    // 409), so they remain pending. A strict-decode rollback (clearFeatures) must NOT reopen the claim
    // loop — the gate keys on pending-vs-cap, never on the negotiated feature set.
    const outbox = await mkOutbox();
    await outbox.journalTerminal(RUN, 1, "running", 0, { status: "completed" });
    await outbox.journalTerminal(RUN2, 1, "running", 0, { status: "completed" });

    let cap: number | undefined = undefined; // the cap-independent floor: any pending ⇒ overflow
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => cap);

    let claims = 0;
    let featuresCleared = false;
    const client = fakeClient({
      claimRun: async () => {
        claims += 1;
        return null;
      },
      // A benign non-terminal 409: the boot resolve KEEPS each journal (they stay pending).
      reportState: async () => ({ applied: false, status: "running" }) as StateAck,
      // The negotiated feature toggles OFF mid-run (a strict-decode rollback cleared the set). The
      // claim gate must ignore this entirely.
      hasFeature: () => (featuresCleared ? false : true),
    });

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      client,
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
      registry,
    );
    const done = worker.run(controller.signal);

    // The claim loop is running but gated by pending_overflow.
    await sleep(60);
    assert.equal(claims, 0, "pending_overflow keeps the claim loop closed");

    // Simulate the strict-decode rollback: clear the negotiated feature set.
    featuresCleared = true;
    await sleep(60);
    assert.equal(claims, 0, "a feature rollback does NOT reopen the claim loop while a journal is unresolved");

    // Only clearing the pending set (cap rises above the count) reopens it — proving the gate keys on
    // pending-vs-cap, not on features.
    cap = 32;
    await pollUntil(() => claims >= 1, 2000, "the claim loop reopens once the pending set fits the cap");

    controller.abort();
    await done;
  });

  it("cap 0: register carries an EMPTY pending subset + pending_overflow (cap-independent, no snapshot rejection)", async () => {
    const outbox = await mkOutbox();
    await outbox.journalTerminal(RUN, 2, "running", 0, { status: "completed" });
    await outbox.journalTerminal(RUN2, 3, "running", 0, { status: "completed" });

    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 0);

    let registeredSnapshot: ActiveSnapshot | undefined;
    let sawRegister = false;
    const client = {
      register: async (
        _name: string,
        _template?: string,
        _max?: number,
        _caps?: string[],
        _proto?: string[],
        initialSnapshot?: ActiveSnapshot,
      ) => {
        sawRegister = true;
        registeredSnapshot = initialSnapshot;
        return {};
      },
      heartbeat: async () => {},
      reportState: async () => ({ applied: true, status: "completed" }) as StateAck,
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
      hasFeature: () => false,
      getMessageGaps: async () => ({ gaps: [] }),
      postMessages: async () => {},
    } as unknown as WorkerClient;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      client,
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
      registry,
    );
    const done = worker.run(controller.signal);
    await pollUntil(() => sawRegister, 2000, "the worker registered");
    controller.abort();
    await done;

    assert.ok(registeredSnapshot, "a worker holding pending terminals carries an initial snapshot on register");
    assert.deepEqual(registeredSnapshot!.active, [], "the register snapshot lists NO pending subset (cap-independent)");
    assert.equal(registeredSnapshot!.pending_overflow, true, "with pending outcomes it sets pending_overflow so the api leases them all");
  });

  it("an ordinary worker with NO pending terminal registers WITHOUT an initial snapshot", async () => {
    const outbox = await mkOutbox(); // empty: no journals
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32);
    let registeredSnapshot: ActiveSnapshot | undefined = { snapshot_epoch: 9, active: [], pending_overflow: false };
    let sawRegister = false;
    const client = {
      register: async (
        _name: string,
        _template?: string,
        _max?: number,
        _caps?: string[],
        _proto?: string[],
        initialSnapshot?: ActiveSnapshot,
      ) => {
        sawRegister = true;
        registeredSnapshot = initialSnapshot;
        return {};
      },
      heartbeat: async () => {},
      reportState: async () => ({ applied: true }) as StateAck,
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
      hasFeature: () => false,
      getMessageGaps: async () => ({ gaps: [] }),
      postMessages: async () => {},
    } as unknown as WorkerClient;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      client,
      idleRunner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
      registry,
    );
    const done = worker.run(controller.signal);
    await pollUntil(() => sawRegister, 2000, "the worker registered");
    controller.abort();
    await done;
    assert.equal(registeredSnapshot, undefined, "no pending terminal ⇒ no register snapshot (byte-identical register wire)");
  });
});

describe("Worker boot: the ancestry-settlement sweep starts only after the boot terminal gate (issue #1582 M2)", () => {
  it("no settlement sweep until resolveBootTerminals resolves; then an immediate sweep and timed re-sweeps", async () => {
    const outbox = await mkOutbox();
    await outbox.journalTerminal(RUN, 4, "running", 0, { status: "completed" });

    const events: string[] = [];
    let releaseReport!: () => void;
    const reportGate = new Promise<void>((r) => (releaseReport = r));
    const client = fakeClient({
      reportState: async () => {
        events.push("boot-resolve:start");
        await reportGate;
        events.push("boot-resolve:done");
        return { applied: true, status: "completed" } as StateAck;
      },
    });
    let sweeps = 0;
    const runner = {
      resumePendingRecoveries: async () => {},
      execute: async () => {},
      settlePendingPredecessors: async () => {
        sweeps += 1;
        events.push("settle-sweep");
      },
    } as unknown as RunRunner;

    const controller = new AbortController();
    const worker = new Worker(
      fakeConfig(),
      client,
      runner,
      idleChat,
      noJudge,
      noReview,
      nullLogger(),
      okPreflight,
      outbox,
      new Map(),
      undefined,
      20, // settlement re-sweep interval (ms)
    );
    const done = worker.run(controller.signal);
    try {
      await pollUntil(() => events.includes("boot-resolve:start"), 2000, "the boot terminal resolve started");
      await sleep(60); // every chance for the sweep to (wrongly) start early
      assert.equal(sweeps, 0, "the settlement sweep must not run before the pending terminal is resolved");

      releaseReport();
      await pollUntil(() => sweeps >= 3, 2000, "an immediate sweep, then timed re-sweeps");
      assert.ok(
        events.indexOf("boot-resolve:done") < events.indexOf("settle-sweep"),
        "the first sweep follows the boot terminal resolve",
      );
    } finally {
      // Stop the worker even when an assertion above failed, so a regression reads as a failure
      // rather than a hung test file.
      releaseReport();
      controller.abort();
      await done;
    }
    const after = sweeps;
    await sleep(60);
    assert.equal(sweeps, after, "the sweep loop stops on abort");
  });
});

describe("Worker policy ownership reconciliation", () => {
  const body: PolicyHoldRequest = {
    outcome_id: "11111111-1111-4111-8111-111111111111", source_generation: 1, claim_generation: 1,
    detail: "refused", continuation_mode: "same_conversation", published: false,
    tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "a".repeat(32) + "-1",
  };
  type Driver = {
    resolveBootTerminals(signal: AbortSignal): Promise<void>;
    resolveRunTerminal(runId: string): Promise<void>;
    sweepPendingTerminals(): Promise<void>;
  };
  for (const via of ["boot", "drain", "sweep"] as const) {
    for (const proof of [
      { status: "running", claim_generation: 3, claim_released: false },
      { status: "running", claim_generation: 3, claim_released: true },
      { status: "paused", claim_generation: 3, claim_released: false },
      { status: "running", claim_generation: 3 },
      { status: "surprise", claim_generation: 3, claim_released: false },
      { status: "running", claim_generation: 0, claim_released: false },
      null,
    ]) it(`${via} projects only positive unreleased ownership: ${JSON.stringify(proof)}`, async () => {
      const outbox = await mkOutbox();
      await outbox.journalPolicyHold(RUN, body);
      const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 10);
      registry.add(RUN, 1);
      registry.markPolicyPendingOnly(RUN, 1);
      let reports = 0;
      const client = fakeClient();
      client.getRunOwnership = async () => proof as never;
      client.reportPolicyHold = async (_runId, request) => {
        reports++;
        assert.deepEqual(request, body, "worker never guesses current mode or converts original");
        return { disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 };
      };
      const worker = new Worker(fakeConfig(), client, idleRunner, idleChat, noJudge, noReview,
        nullLogger(), okPreflight, outbox, new Map(), registry) as unknown as Driver;
      if (via === "boot") await worker.resolveBootTerminals(new AbortController().signal);
      else if (via === "drain") await worker.resolveRunTerminal(RUN);
      else await worker.sweepPendingTerminals();
      assert.equal(reports, 1, "pending-only execution must remain replayable");
      const snapshot = registry.build();
      const valid = proof?.status === "running" && proof.claim_generation === 3 && proof.claim_released === false;
      const negative = proof?.claim_generation === 3 &&
        (proof.claim_released === true || proof.status === "paused");
      assert.deepEqual(snapshot.active.map(e => e.claim_generation), valid ? [3] : []);
      assert.equal(snapshot.pending_overflow, !valid && !negative);
      assert.deepEqual(snapshot.pending_all, [{ run_id: RUN, claim_generation: 1 }]);
      assert.deepEqual((await outbox.readTerminalJournal(RUN, 1))?.body, body);
    });
  }

  it("heartbeat delivery facts clear after exact settlement without deleting custody evidence", async () => {
    const outbox = await mkOutbox();
    await outbox.journalPolicyHold(RUN, body);
    const worker = new Worker(fakeConfig(), fakeClient(), idleRunner, idleChat, noJudge, noReview,
      nullLogger(), okPreflight, outbox) as unknown as {
        outboxEntries(): { pending_terminal: number; policy_pending_outcome_id?: string; policy_pending_source_generation?: number }[];
      };
    const pending = worker.outboxEntries()[0]!;
    assert.equal(pending.pending_terminal, 1);
    assert.equal(pending.policy_pending_outcome_id, body.outcome_id);
    assert.equal(pending.policy_pending_source_generation, 1);
    const observed = await outbox.readTerminalJournalForRetirement(RUN, 1);
    assert.ok(observed);
    await outbox.markTerminalServerSettled(RUN, 1, observed.context);
    const settled = worker.outboxEntries()[0]!;
    assert.equal(settled.pending_terminal, 0);
    assert.equal(settled.policy_pending_outcome_id, undefined);
    assert.equal(settled.policy_pending_source_generation, undefined);
    assert.deepEqual((await outbox.readTerminalJournal(RUN, 1))?.body, body);
    assert.equal(outbox.listPendingTerminals().length, 1);
  });

  it("late G3 response cannot remove the G4 lease during a sweep", async () => {
    const outbox = await mkOutbox();
    await outbox.journalPolicyHold(RUN, body);
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 10);
    let entered!: () => void;
    let finish!: (proof: unknown) => void;
    const started = new Promise<void>(r => { entered = r; });
    const response = new Promise<unknown>(r => { finish = r; });
    const client = fakeClient();
    client.getRunOwnership = async () => { entered(); return await response as never; };
    client.reportPolicyHold = async () => ({ disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 });
    const worker = new Worker(fakeConfig(), client, idleRunner, idleChat, noJudge, noReview,
      nullLogger(), okPreflight, outbox, new Map(), registry) as unknown as Driver;
    const sweeping = worker.sweepPendingTerminals();
    await started;
    registry.add(RUN, 4);
    registry.markPolicyPendingOnly(RUN, 4);
    registry.notePolicyOwnership(RUN, 4);
    finish({ status: "paused", claim_generation: 3, claim_released: true });
    await sweeping;
    assert.deepEqual(registry.build().active.map(e => e.claim_generation), [4]);
  });
});


import * as r1fs from "node:fs/promises";
import * as r1os from "node:os";
import * as r1path from "node:path";
import { makeScratchRoot } from "./fixture-repo.js";

const r1Modes = ["empty", "other-policy", "other-unknown", "same-policy", "same-unknown"] as const;
async function r1DisabledStore(mode: typeof r1Modes[number]) {
  const root = makeScratchRoot("r1-producer-", await r1fs.realpath(r1os.tmpdir()));
  const storeRoot = r1path.join(root, "outbox");
  const options = { root: storeRoot, log: nullLogger(), runMaxBytes: mode.endsWith("unknown") ? 1 : 1e6,
    maxBytes: mode.endsWith("unknown") ? 1 : 1e6, retentionMs: 86400000 };
  await new Outbox(options).init();
  await r1fs.writeFile(r1path.join(storeRoot, ".key"), "invalid");
  const outbox = new Outbox(options);
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  assert.deepEqual(outbox.listPendingTerminals(), [], "construct the runner with an empty disabled store");
  return { root, outbox };
}
async function r1LateEvidence(outbox: Outbox, mode: typeof r1Modes[number], current: string) {
  if (mode === "empty") return;
  const result = await outbox.journalPolicyHold(mode.startsWith("same") ? current : "cccccccc-cccc-4ccc-8ccc-cccccccccccc", {
    outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", source_generation: 1, claim_generation: 1,
    detail: "refused", continuation_mode: "same_conversation", published: false,
    tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "a".repeat(32) + "-1",
  });
  assert.equal(result.journaled, false);
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, mode.endsWith("unknown") ? "unknown" : "policy_hold");
}

type R1ReplayDriver = {
  resolveBootTerminals(signal: AbortSignal): Promise<void>;
  resolveRunTerminal(runId: string, signal?: AbortSignal): Promise<void>;
  sweepPendingTerminals(signal?: AbortSignal): Promise<void>;
};
for (const method of ["boot", "run", "sweep"] as const) {
  for (const mode of r1Modes.filter(m => !m.startsWith("same"))) {
    it("R1 replay " + method + " disabled " + mode + " invents no ordinary outcome", async () => {
      const { root, outbox } = await r1DisabledStore(mode);
      try {
        let sends = 0;
        const client = fakeClient({ reportState: async () => { sends++; return { applied: true }; } });
        const driver = new Worker(fakeConfig(), client, idleRunner, idleChat, noJudge, noReview,
          nullLogger(), okPreflight, outbox) as unknown as R1ReplayDriver;
        await r1LateEvidence(outbox, mode, RUN2);
        if (method === "boot") await driver.resolveBootTerminals(new AbortController().signal);
        else if (method === "run") await driver.resolveRunTerminal(RUN2);
        else await driver.sweepPendingTerminals();
        assert.equal(sends, 0, "a replay needs an existing readable ordinary journal");
      } finally { await r1fs.rm(root, { recursive: true, force: true }); }
    });
  }

  for (const source of ["policy", "unknown", "unreadable-target"] as const) {
    it("R1 replay " + method + " healthy B with retained A " + source, async () => {
      const root = makeScratchRoot("r1-replay-", await r1fs.realpath(r1os.tmpdir()));
      try {
        const options = { root: r1path.join(root, "outbox"), log: nullLogger(), runMaxBytes: 4096,
          maxBytes: 16384, retentionMs: 86400000 };
        let outbox = new Outbox(options);
        await outbox.init();
        assert.equal((await outbox.journalTerminal(RUN2, 8, "running", 0, { status: "completed" })).journaled, true);
        if (source === "unreadable-target") {
          await r1fs.writeFile(r1path.join(options.root, ".key"), "invalid");
          outbox = new Outbox(options);
          await outbox.init();
          assert.equal(outbox.isDisabled(), true);
        } else {
          if (source === "unknown") {
            // A malformed physical source is unknown; B keeps its healthy body/read budget.
            await r1fs.mkdir(r1path.join(options.root, RUN), { recursive: true });
            await r1fs.writeFile(r1path.join(options.root, RUN, "terminal-1.json"), "{}");
            outbox = new Outbox(options);
            await outbox.init();
          } else {
            await outbox.journalPolicyHold(RUN, { outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
              source_generation: 1, claim_generation: 1, detail: "refused",
              continuation_mode: "same_conversation", published: false, tag: "cyberPolicy",
              phase: "implementation", correlation_id: "pr-" + "a".repeat(32) + "-1" });
          }
          assert.equal(outbox.listPendingTerminals().find(p => p.run_id === RUN)?.outcome_kind,
            source === "unknown" ? "unknown" : "policy_hold");
        }
        const bodies: StateRequest[] = [];
        const client = fakeClient({ reportState: async (id, body) => {
          assert.equal(id, RUN2);
          bodies.push(body);
          return { applied: true, status: "completed" };
        } });
        const driver = new Worker(fakeConfig(), client, idleRunner, idleChat, noJudge, noReview,
          nullLogger(), okPreflight, outbox) as unknown as R1ReplayDriver;
        for (let attempt = 0; attempt < 2; attempt++) {
          if (method === "boot") await driver.resolveBootTerminals(new AbortController().signal);
          else if (method === "run") await driver.resolveRunTerminal(RUN2);
          else await driver.sweepPendingTerminals();
        }
        assert.equal(bodies.length, source === "unreadable-target" ? 0 : 1);
        if (bodies.length) assert.equal(bodies[0]!.claim_generation, 8);
      } finally { await r1fs.rm(root, { recursive: true, force: true }); }
    });
  }
}
