import { it } from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import { Worker } from "../src/worker.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { PendingTerminal, Outbox } from "../src/outbox.js";
import type { ActiveSnapshot } from "../src/protocol.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { nullLogger } from "./helpers.js";

const id = (n: number) => `${n.toString(16).padStart(8, "0")}-aaaa-4aaa-8aaa-aaaaaaaaaaaa`;
const config = { dockerWiring: {}, workerName: "registration", workerTemplate: "base", dataDir: os.tmpdir(),
  heartbeatIntervalMs: 5, pollIntervalMs: 1, chatPollMs: 1, maxConcurrentRuns: 1, crossCheckSlots: 0,
  chatSessions: 0, gapFillMax: 1000, outboxTerminalMaxBytes: 1 << 20 } as Config;
const runner = { resumePendingRecoveries: async () => {} } as unknown as RunRunner;
function outbox(entries: PendingTerminal[]): Outbox {
  return { listPendingTerminals: () => entries, listPendingOutcomeSources: () => entries,
    listPendingFinalizes: () => [], listPublicationRegisterEntries: () => [], uncleanRuns: () => [], depths: () => [],
    runsWithPending: () => [], unclassifiedPolicyRunIds: () => [], depthFor: () => undefined,
    drain: async () => {}, hasUndrainedMessages: () => false, isDisabled: () => false } as unknown as Outbox;
}
const pending = (n: number): PendingTerminal => ({ run_id: id(n), claim_generation: 1,
  outcome_kind: "unknown", phase: "policy_pending", since: 1, blocked: false });
function event() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}

for (const count of [1, 100]) for (const complete of [false, true])
it(`registration and heartbeat precede ${count} slow ownership reads (coverage=${complete})`, async () => {
  const entries = Array.from({ length: count }, (_, n) => pending(n + 1));
  const registry = new ActiveRunRegistry(() => entries, () => 2, () => complete);
  const controller = new AbortController();
  const registered = event(), ownership = event(), heartbeats = event();
  let release!: () => void;
  const held = new Promise<void>(resolve => { release = resolve; });
  let requests = 0, beats = 0, claims = 0;
  let initial: ActiveSnapshot | undefined;
  const client = {
    hasFeature: (feature: string) => feature === "active_run_snapshot",
    supportsScopedPendingClaim: false,
    getRunOwnership: async () => {
      requests++; ownership.resolve();
      await held;
      return { status: "running", claim_generation: 1, claim_released: false };
    },
    register: async (...args: unknown[]) => {
      initial = args.find(arg => arg && typeof arg === "object" && "snapshot_epoch" in arg) as ActiveSnapshot | undefined;
      registered.resolve(); return { worker_id: id(9999) };
    },
    heartbeat: async () => { if (++beats === 2) heartbeats.resolve(); },
    claimRun: async () => { claims++; return null; },
    claimChat: async () => null,
  } as unknown as WorkerClient;
  const worker = new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), () => ({ ok: true, missing: [] }), outbox(entries), undefined, registry);
  const running = worker.run(controller.signal);
  let timer: ReturnType<typeof setTimeout> | undefined;
  const timeout = new Promise<never>((_resolve, reject) => {
    timer = setTimeout(() => reject(new Error("startup did not register/heartbeat while ownership remained unreadable")), 5000);
  });
  try {
    const first = await Promise.race([registered.promise.then(() => "registered"), ownership.promise.then(() => "ownership"), timeout]);
    assert.equal(first, "registered", "ownership GET must not precede registration");
    await Promise.race([Promise.all([ownership.promise, heartbeats.promise]), timeout]);
    assert.ok(requests > 0, "post-registration reconciliation is still performed");
    assert.ok(beats >= 2, "heartbeat remains independent of slow ownership reads");
    assert.equal(claims, 0, "unresolved boot outcomes retain the existing claim gate");
    assert.ok(initial);
    assert.deepEqual(initial.active, []);
    assert.equal(initial.pending_overflow, true);
    assert.equal(initial.pending_all?.length, complete ? count : undefined,
      "registration never asserts complete coverage it does not have");
  } finally { clearTimeout(timer); controller.abort(); release(); await running; }
});

it("ownership refresh skips authenticated server-settled sources", async () => {
  const active = pending(1001);
  const entries = [...Array.from({ length: 100 }, (_, n) => ({ ...pending(n + 1), server_settled: true })), active];
  const requested: string[] = [];
  const client = { getRunOwnership: async (run: string) => {
    requested.push(run); return { status: "running", claim_generation: 1, claim_released: false };
  } } as unknown as WorkerClient;
  const registry = new ActiveRunRegistry(() => entries, () => 2);
  const worker = new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), undefined, outbox(entries), undefined, registry);
  await (worker as unknown as { observePendingPolicyOwnership(): Promise<void> }).observePendingPolicyOwnership();
  assert.deepEqual(requested, [active.run_id]);
});

for (const lane of ["boot", "drain", "sweep"] as const)
it(`${lane} ownership pass shares one deadline across 100 slow reads and still delivers an ordinary terminal`, async t => {
  const entries = Array.from({ length: 100 }, (_, n) => pending(n + 1));
  const target = entries[0]!.run_id;
  const terminal = { ...pending(1), claim_generation: 2, outcome_kind: "terminal" as const };
  const registry = new ActiveRunRegistry(() => entries, () => 200);
  let now = 10000;
  t.mock.method(Date, "now", () => now);
  const reads: Array<{ signal?: AbortSignal; deadline: number } | undefined> = [];
  const delivered: string[] = [];
  const signal = new AbortController().signal;
  const client = {
    hasFeature: () => false,
    getRunOwnership: async (_run: string, budget?: { signal?: AbortSignal; deadline: number }) => {
      reads.push(budget);
      // Deterministically consume the whole pass, rather than waiting a real second.
      now += 1000;
      return { status: "running", claim_generation: 1, claim_released: false };
    },
    reportState: async (run: string) => { delivered.push(run); return { applied: false, status: "running" }; },
  } as unknown as WorkerClient;
  const store = outbox(entries);
  Object.assign(store, {
    listPendingTerminals: () => [terminal],
    isTerminalResolveHeld: () => false,
    readTerminalJournal: async () => ({ body: { status: "failed", error: "ordinary" }, messagesThroughSeq: 0, blocked: false }),
  });
  const replayRunner = { observeSettlementTerminalAck: async () => {} } as unknown as RunRunner;
  const worker = new Worker(config, client, replayRunner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), undefined, store, undefined, registry);
  const seam = worker as unknown as {
    resolveBootTerminals(signal: AbortSignal): Promise<void>;
    resolveRunTerminal(run: string, signal: AbortSignal): Promise<void>;
    sweepPendingTerminals(signal: AbortSignal): Promise<void>;
  };
  if (lane === "boot") await seam.resolveBootTerminals(signal);
  else if (lane === "drain") await seam.resolveRunTerminal(target, signal);
  else await seam.sweepPendingTerminals(signal);
  assert.equal(reads.length, 1, "expiry must stop the pass instead of spending a timeout per source");
  assert.equal(reads[0]?.deadline, 11000, "the existing HTTP transport receives the absolute shared deadline");
  assert.equal(reads[0]?.signal, signal, "worker shutdown cancels the actual request");
  assert.deepEqual(delivered, [target], "ordinary terminal delivery proceeds after ownership expiry");
  assert.deepEqual(registry.build(true).pending_policy_protection, [],
    "a response arriving at the deadline cannot establish ownership");
});

it("ownership passes cap fast reads and rotate beyond old candidates", async t => {
  const entries = Array.from({ length: 100 }, (_, n) => pending(n + 1));
  const requested: string[] = [], deadlines: number[] = [];
  t.mock.method(Date, "now", () => 10000);
  const client = { getRunOwnership: async (run: string, budget: { deadline: number }) => {
    requested.push(run); deadlines.push(budget?.deadline);
    return { status: "running", claim_generation: 1, claim_released: false };
  } } as unknown as WorkerClient;
  const registry = new ActiveRunRegistry(() => entries, () => 200);
  const worker = new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    nullLogger(), undefined, outbox(entries), undefined, registry);
  const seam = worker as unknown as { observePendingPolicyOwnership(): Promise<void> };
  await seam.observePendingPolicyOwnership();
  assert.equal(requested.length, 16);
  await seam.observePendingPolicyOwnership();
  assert.deepEqual(requested, entries.slice(0, 32).map(entry => entry.run_id));
  assert.deepEqual(deadlines, Array(32).fill(11000));
});
