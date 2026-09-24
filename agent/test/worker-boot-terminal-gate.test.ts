import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
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
const idleRunner = { resumePendingRecoveries: async () => {}, execute: async () => {} } as unknown as RunRunner;
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
