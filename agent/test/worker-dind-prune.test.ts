import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse } from "../src/protocol.js";
import { DindPruneController, DindPruneGate } from "../src/dind-prune.js";
import type { DindMeterSample } from "../src/dind-meter.js";
import { sleep } from "../src/util.js";
import { nullLogger } from "./helpers.js";

// issue #1759 M3: the claim-vs-prune race, driven through the REAL Worker claim loops.
// The prune controller runs its real loop with short sleeps; the fake client counts
// claims and can hold a claim in flight; the exec spy records, for every docker command,
// whether the worker was idle at that moment. No prune argv may ever run while a run or
// chat is active, and while the gate is held neither loop may call the claim endpoints.

const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
const noReview = { execute: async () => {} } as unknown as ReviewRunner;
const okPreflight = () => ({ ok: true, missing: [] as string[] });
const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));

function fakeConfig(): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dockerWiring: { dockerHost: "unix:///run/dind/docker.sock" },
  } as unknown as Config;
}

/** A promise plus its resolver. */
function deferred<T = void>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

interface Harness {
  worker: Worker;
  gate: DindPruneGate;
  pressure: { on: boolean };
  execs: Array<{ argv: string[]; idle: boolean }>;
}

function build(client: Partial<WorkerClient>, runner: RunRunner, chatRunner: ChatRunner): Harness {
  const gate = new DindPruneGate();
  const pressure = { on: false };
  const execs: Harness["execs"] = [];
  let n = 0;
  let worker!: Worker;
  const ctl = new DindPruneController({
    dockerHost: "unix:///run/dind/docker.sock",
    heartbeatIntervalMs: 60_000,
    workerName: "w1",
    gate,
    isIdle: () => worker.isIdle(),
    log: nullLogger(),
    // Every read is a new, fresh, over-threshold sample (distinct epochs, newer than now).
    readSample: (): DindMeterSample | null =>
      pressure.on
        ? { epochS: Math.floor(Date.now() / 1000) + 1 + n++, bytesUsed: 95, bytesTotal: 100, inodesUsed: 1, inodesTotal: 100 }
        : null,
    exec: async (argv) => {
      execs.push({ argv: [...argv], idle: worker.isIdle() });
      return "";
    },
    sleep: (ms, signal) => sleep(Math.min(ms, 2), signal),
  });
  const fullClient = {
    register: async () => ({ worker_id: "wid-1" }),
    heartbeat: async () => false, // custody clear on every heartbeat
    hasFeature: () => false,
    claimRun: async (): Promise<ClaimResponse | null> => null,
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
    ...client,
  } as unknown as WorkerClient;
  worker = new Worker(
    fakeConfig(),
    fullClient,
    runner,
    chatRunner,
    noJudge,
    noReview,
    nullLogger(),
    okPreflight,
    undefined,
    undefined,
    undefined,
    undefined,
    ctl,
  );
  return { worker, gate, pressure, execs };
}

const idleRunner = { resumePendingRecoveries: async () => {}, execute: async () => {} } as unknown as RunRunner;
const idleChat = { execute: async () => {} } as unknown as ChatRunner;
const isPrune = (argv: string[]) => argv[0] !== "ps";

async function until(cond: () => boolean, what: string, max = 2000): Promise<void> {
  for (let i = 0; i < max && !cond(); i++) await tick();
  assert.ok(cond(), `timed out waiting for: ${what}`);
}

describe("Worker × DinD prune claim gate (issue #1759)", () => {
  it("while the gate is held, neither claim loop calls claimRun/claimChat; release resumes both", async () => {
    const counts = { run: 0, chat: 0 };
    const h = build(
      {
        claimRun: async () => {
          counts.run++;
          return null;
        },
        claimChat: async () => {
          counts.chat++;
          return null;
        },
      },
      idleRunner,
      idleChat,
    );
    h.gate.close(); // the prune holds the gate from before the loops start
    const controller = new AbortController();
    const done = h.worker.run(controller.signal);
    for (let i = 0; i < 60; i++) await tick();
    assert.strictEqual(counts.run, 0, "no run-lane claim while the gate is held");
    assert.strictEqual(counts.chat, 0, "no chat-lane claim while the gate is held");
    h.gate.open();
    await until(() => counts.run > 0 && counts.chat > 0, "both lanes claim after release");
    controller.abort();
    await done;
    assert.strictEqual(h.gate.inFlightClaims(), 0, "every entered claim exited");
  });

  it("a claim in flight when the gate closes returns a run: the prune aborts and nothing runs while the run is active", async () => {
    const claimGate = deferred<void>();
    const runGate = deferred<void>();
    let claims = 0;
    let runDone = false;
    const runner = {
      resumePendingRecoveries: async () => {},
      execute: async () => {
        await runGate.promise;
        runDone = true;
      },
    } as unknown as RunRunner;
    const h = build(
      {
        claimRun: async (): Promise<ClaimResponse | null> => {
          claims++;
          if (claims > 1) return null;
          await claimGate.promise; // held in flight until the test releases it
          return { run_id: "run-1" } as unknown as ClaimResponse;
        },
      },
      runner,
      idleChat,
    );
    const controller = new AbortController();
    const done = h.worker.run(controller.signal);
    await until(() => claims === 1, "the first claim is in flight");
    h.pressure.on = true;
    await until(() => h.gate.claimsClosed(), "the prune closed the gate");
    assert.strictEqual(h.gate.inFlightClaims(), 1, "the prune is waiting on the in-flight claim");
    claimGate.resolve(); // the claim returns a run
    await until(() => !h.gate.claimsClosed(), "the prune released the gate");
    for (let i = 0; i < 50; i++) await tick();
    assert.strictEqual(h.execs.length, 0, "no docker command at all while the claimed run is active");
    runGate.resolve();
    await until(() => h.execs.some((e) => isPrune(e.argv)), "the prune runs once the worker is idle");
    controller.abort();
    await done;
    assert.ok(runDone);
    for (const e of h.execs) assert.ok(e.idle, `${e.argv.join(" ")} ran while the worker was busy`);
  });

  it("an active chat session blocks the prune", async () => {
    const chatGate = deferred<void>();
    let chatStarted = false;
    let chatClaims = 0;
    const chatRunner = {
      execute: async () => {
        chatStarted = true;
        await chatGate.promise;
      },
    } as unknown as ChatRunner;
    const h = build(
      {
        claimChat: async (): Promise<ChatClaimResponse | null> =>
          chatClaims++ === 0 ? ({ run_id: "chat-1" } as unknown as ChatClaimResponse) : null,
      },
      idleRunner,
      chatRunner,
    );
    const controller = new AbortController();
    const done = h.worker.run(controller.signal);
    await until(() => chatStarted, "the chat is active");
    h.pressure.on = true;
    for (let i = 0; i < 80; i++) await tick();
    assert.strictEqual(h.execs.length, 0, "no docker command while a chat is active");
    chatGate.resolve();
    await until(() => h.execs.some((e) => isPrune(e.argv)), "the prune runs once the chat ends");
    controller.abort();
    await done;
    for (const e of h.execs) assert.ok(e.idle, `${e.argv.join(" ")} ran while the worker was busy`);
  });
});
