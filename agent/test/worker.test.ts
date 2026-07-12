import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { ClaimResponse, ChatClaimResponse } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

// The worker runs the RUN lane and the CHAT lane as two independent, concurrent
// claim loops (PRD #39 Decision 4). This proves they actually run at the same time
// (not one-after-the-other) while sharing the Logger and WorkerClient — the
// concurrency the audit signs off on. Scrubbing correctness itself is covered by
// the log/redact suites; here the point is safe concurrent use of the collaborators.

const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));

function fakeConfig(over: Partial<Config> = {}): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    ...over,
  } as unknown as Config;
}

/** A runRunner whose execute parks each run until `release` aborts (runs carry no
 *  shutdown signal, so a test controls completion out-of-band). Records peak
 *  concurrency and per-run start/end so the slot semaphore is observable. */
function parkingRunRunner(release: AbortSignal, hooks: {
  onStart?: (claim: ClaimResponse) => void;
  onEnd?: (claim: ClaimResponse) => void;
} = {}): { runner: RunRunner; peak: () => number; active: () => number } {
  let active = 0;
  let peak = 0;
  const runner = {
    execute: async (claim: ClaimResponse) => {
      active++;
      peak = Math.max(peak, active);
      hooks.onStart?.(claim);
      await new Promise<void>((r) => {
        if (release.aborted) return r();
        release.addEventListener("abort", () => r(), { once: true });
      });
      active--;
      hooks.onEnd?.(claim);
    },
  } as unknown as RunRunner;
  return { runner, peak: () => peak, active: () => active };
}

/** A client whose run lane always has another queued run (unique ids) and whose
 *  chat lane is idle. Counts claimRun calls so a test can prove no claim is made
 *  while at capacity. */
function endlessRunClient(): { client: WorkerClient; claimCalls: () => number } {
  let claimCalls = 0;
  const client = {
    register: async () => ({}),
    heartbeat: async () => {},
    claimRun: async (): Promise<ClaimResponse | null> => {
      claimCalls++;
      return { run_id: `run-${claimCalls}` } as unknown as ClaimResponse;
    },
    claimChat: async (): Promise<ChatClaimResponse | null> => null,
  } as unknown as WorkerClient;
  return { client, claimCalls: () => claimCalls };
}

describe("Worker — concurrent run + chat lanes (Decision 4)", () => {
  it("executes a run and a chat session concurrently, sharing logger + client", async () => {
    const controller = new AbortController();
    const events: string[] = [];
    let ranRun = false;
    let ranChat = false;

    // The run's execute parks on a gate the chat's execute releases: if the two lanes
    // were sequential (the run fully before the chat), the run would deadlock here.
    let releaseRun!: () => void;
    const runGate = new Promise<void>((r) => (releaseRun = r));

    let gaveRun = false;
    let gaveChat = false;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        if (gaveRun) return null;
        gaveRun = true;
        return { run_id: "run-1" } as unknown as ClaimResponse;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => {
        if (gaveChat) return null;
        gaveChat = true;
        return { run_id: "chat-1" } as unknown as ChatClaimResponse;
      },
    } as unknown as WorkerClient;

    const { logger } = recordingLogger(); // shared across both lanes
    const runRunner = {
      execute: async () => {
        events.push("run:start");
        logger.addSecret("run-secret-000000"); // concurrent shared-registry mutation
        await runGate; // wait for the chat lane to prove it is running
        ranRun = true;
        events.push("run:end");
      },
    } as unknown as RunRunner;
    const chatRunner = {
      execute: async () => {
        events.push("chat:start");
        logger.addSecret("chat-secret-000000");
        ranChat = true;
        releaseRun();
        events.push("chat:end");
      },
    } as unknown as ChatRunner;

    const worker = new Worker(fakeConfig(), client, runRunner, chatRunner, logger);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && !(ranRun && ranChat); i++) await tick();
    controller.abort();
    await done;

    assert.ok(ranRun && ranChat, "both lanes executed");
    assert.ok(
      events.indexOf("chat:start") < events.indexOf("run:end"),
      "the chat session started before the run finished — the lanes ran concurrently",
    );
  });

  it("honors WORKER_CHAT_SESSIONS as the concurrent chat ceiling", async () => {
    const controller = new AbortController();
    let peak = 0;
    let active = 0;
    let claimed = 0;

    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async () => null, // run lane idle
      claimChat: async (): Promise<ChatClaimResponse | null> => {
        claimed++;
        return { run_id: `chat-${claimed}` } as unknown as ChatClaimResponse; // always more available
      },
    } as unknown as WorkerClient;

    // Each chat blocks until the worker shuts down (as a real parked chat would),
    // so the slots stay full and the ceiling is observable.
    const chatRunner = {
      execute: async (_claim: ChatClaimResponse, signal?: AbortSignal) => {
        active++;
        peak = Math.max(peak, active);
        await new Promise<void>((r) => {
          if (signal?.aborted) return r();
          signal?.addEventListener("abort", () => r(), { once: true });
        });
        active--;
      },
    } as unknown as ChatRunner;
    const runRunner = { execute: async () => {} } as unknown as RunRunner;

    const worker = new Worker(fakeConfig({ chatSessions: 2 }), client, runRunner, chatRunner, recordingLogger().logger);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 300 && active < 2; i++) await tick();
    // Give the loop extra ticks to (wrongly) over-fill if the ceiling were not honored.
    for (let i = 0; i < 20; i++) await tick();
    assert.strictEqual(peak, 2, "never more than WORKER_CHAT_SESSIONS chats run at once");
    controller.abort(); // shutdown aborts the parked chats → clean drain
    await done;
  });
});

// The RUN lane's slot semaphore (PRD #42 M2, Decision 1): the loop executes up to
// WORKER_MAX_CONCURRENT_RUNS runs as tracked promises, slot-before-claim, releasing
// a slot only when its run settles. These are worker-level unit tests with a faked
// RunRunner — the real plan-gate/executor path is covered in runner.test.ts; here
// the subject is purely the semaphore (cap, backoff, release, drain).
describe("Worker — RUN lane slot semaphore (PRD #42 M2)", () => {
  it("runs up to WORKER_MAX_CONCURRENT_RUNS concurrently, logs at capacity, and does not claim while full", async () => {
    const controller = new AbortController();
    const release = new AbortController();
    const { client, claimCalls } = endlessRunClient();
    const { runner, peak, active } = parkingRunRunner(release.signal);
    const { logger, lines } = recordingLogger();

    // chatRunner unused: the chat lane always claims null here, so execute is never called.
    const worker = new Worker(fakeConfig({ maxConcurrentRuns: 2 }), client, runner, {} as unknown as ChatRunner, logger);
    const done = worker.run(controller.signal);

    for (let i = 0; i < 500 && active() < 2; i++) await tick();
    // Extra polls: if the cap were not honored the loop would keep claiming and
    // over-fill; slot-before-claim stops claimRun at exactly the cap.
    for (let i = 0; i < 40; i++) await tick();

    assert.strictEqual(peak(), 2, "never more than WORKER_MAX_CONCURRENT_RUNS runs at once");
    assert.strictEqual(claimCalls(), 2, "no claim is made while every slot is full (slot-before-claim)");
    assert.ok(
      lines.some((l) => (l as { msg?: string }).msg === "run lane at capacity, deferring claim"),
      "an at-capacity line is logged for saturation observability",
    );

    release.abort(); // let both parked runs finish
    controller.abort();
    await done;
  });

  it("at the default cap of 1 runs the lane serially with no at-capacity log (identical to pre-#42)", async () => {
    const controller = new AbortController();
    const release = new AbortController();
    const order: string[] = [];
    let served = 0;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        if (served >= 2) return null;
        served++;
        return { run_id: `run-${served}` } as unknown as ClaimResponse;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;
    const { runner, peak } = parkingRunRunner(release.signal, {
      onStart: (c) => order.push(`start:${c.run_id}`),
      onEnd: (c) => order.push(`end:${c.run_id}`),
    });
    const { logger, lines } = recordingLogger();

    const worker = new Worker(fakeConfig({ maxConcurrentRuns: 1 }), client, runner, {} as unknown as ChatRunner, logger);
    const done = worker.run(controller.signal);

    // run-1 parks holding the only slot; run-2 must NOT start until run-1 frees it.
    for (let i = 0; i < 200 && order.length < 1; i++) await tick();
    for (let i = 0; i < 40; i++) await tick();
    assert.deepStrictEqual(order, ["start:run-1"], "run-2 does not start while the single slot is held");
    assert.strictEqual(peak(), 1, "cap 1 never runs two runs at once");

    release.abort(); // run-1 finishes → run-2 takes the freed slot
    for (let i = 0; i < 300 && order.length < 4; i++) await tick();
    assert.deepStrictEqual(
      order,
      ["start:run-1", "end:run-1", "start:run-2", "end:run-2"],
      "runs execute strictly serially at cap 1",
    );
    assert.ok(
      !lines.some((l) => (l as { msg?: string }).msg === "run lane at capacity, deferring claim"),
      "the default cap logs no at-capacity line (output identical to pre-#42)",
    );

    controller.abort();
    await done;
  });

  it("releases the slot after a claim error and after an execute throw", async () => {
    const controller = new AbortController();
    const executed: string[] = [];
    // Claim sequence: a claim error (no slot taken), then a run whose execute throws
    // (its slot must release), then a run that must still find a free slot.
    const steps: Array<() => ClaimResponse | null> = [
      () => { throw new Error("claim boom"); },
      () => ({ run_id: "throws" }) as unknown as ClaimResponse,
      () => ({ run_id: "ok" }) as unknown as ClaimResponse,
    ];
    let i = 0;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        const step = steps[i++];
        return step ? step() : null;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;
    const runner = {
      execute: async (claim: ClaimResponse) => {
        executed.push(claim.run_id);
        if (claim.run_id === "throws") throw new Error("execute boom");
      },
    } as unknown as RunRunner;

    const worker = new Worker(
      fakeConfig({ maxConcurrentRuns: 1 }),
      client,
      runner,
      {} as unknown as ChatRunner,
      recordingLogger().logger,
    );
    const done = worker.run(controller.signal);
    for (let n = 0; n < 300 && !executed.includes("ok"); n++) await tick();
    controller.abort();
    await done;

    assert.deepStrictEqual(
      executed,
      ["throws", "ok"],
      "the slot frees after a claim error and after an execute throw, so the next run executes",
    );
  });

  it("holds a slot across a parked (plan-gated) run while a sibling run completes on another slot", async () => {
    const controller = new AbortController();
    const gate = new AbortController(); // releases the plan-gated run-1
    const order: string[] = [];
    let served = 0;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        if (served >= 2) return null;
        served++;
        return { run_id: `run-${served}` } as unknown as ClaimResponse;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;
    // run-1 parks inside execute (as it would at awaiting_approval, Decision 2);
    // run-2 runs to completion on the second slot alongside it.
    let run2Done = false;
    const runner = {
      execute: async (claim: ClaimResponse) => {
        order.push(`start:${claim.run_id}`);
        if (claim.run_id === "run-1") {
          await new Promise<void>((r) => {
            if (gate.signal.aborted) return r();
            gate.signal.addEventListener("abort", () => r(), { once: true });
          });
        } else {
          run2Done = true;
        }
        order.push(`end:${claim.run_id}`);
      },
    } as unknown as RunRunner;

    const worker = new Worker(
      fakeConfig({ maxConcurrentRuns: 2 }),
      client,
      runner,
      {} as unknown as ChatRunner,
      recordingLogger().logger,
    );
    const done = worker.run(controller.signal);

    for (let i = 0; i < 300 && !run2Done; i++) await tick();
    assert.ok(run2Done, "the second slot executed a run while run-1 was parked at the gate");
    assert.ok(
      order.includes("end:run-2") && !order.includes("end:run-1"),
      "run-1 still holds its slot (parked); run-2 finished alongside it",
    );

    gate.abort(); // approve/finish run-1
    controller.abort();
    await done;
  });

  it("drains all in-flight runs on shutdown before run() resolves", async () => {
    const controller = new AbortController();
    const release = new AbortController();
    const { client } = endlessRunClient();
    const ended: string[] = [];
    const { runner, active } = parkingRunRunner(release.signal, { onEnd: (c) => ended.push(c.run_id) });

    const worker = new Worker(
      fakeConfig({ maxConcurrentRuns: 3 }),
      client,
      runner,
      {} as unknown as ChatRunner,
      recordingLogger().logger,
    );
    const done = worker.run(controller.signal);
    let resolved = false;
    void done.then(() => {
      resolved = true;
    });

    for (let i = 0; i < 500 && active() < 3; i++) await tick();
    assert.strictEqual(active(), 3, "three runs are in flight");

    controller.abort(); // begin shutdown; the three runs are still parked
    for (let i = 0; i < 40; i++) await tick();
    assert.strictEqual(resolved, false, "run() does not resolve while in-flight runs are still draining");

    release.abort(); // let every in-flight run finish
    await done;
    assert.strictEqual(resolved, true);
    assert.strictEqual(ended.length, 3, "every in-flight run drained");
    assert.strictEqual(active(), 0);
  });
});
