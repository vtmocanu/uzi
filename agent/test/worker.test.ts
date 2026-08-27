import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import { RequestError } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ClaimResponse, ChatClaimResponse, WorkerStats } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";

// These run-lane / chat-lane tests never claim a judge run, so a no-op JudgeRunner
// stub satisfies the Worker constructor (the judge lane has its own test file).
const noJudge = { execute: async () => {} } as unknown as JudgeRunner;

// Likewise a no-op ReviewRunner: these tests never claim a diff-review run (a `task`
// claim with review_target_run_id set); the review lane has its own test file.
const noReview = { execute: async () => {} } as unknown as ReviewRunner;

// PRD #92 M3: the real boot toolchain preflight would fail on this (non-image) test host
// (no /opt/uzi-toolchain, no baked go/gcc on PATH), so the concurrency/semaphore tests
// inject a passing preflight; the dedicated preflight-gate test below drives a failing one.
const okPreflight = () => ({ ok: true, missing: [] as string[] });

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
    dockerWiring: {},
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

    const worker = new Worker(fakeConfig(), client, runRunner, chatRunner, noJudge,
      noReview, logger, okPreflight);
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

    const worker = new Worker(fakeConfig({ chatSessions: 2 }), client, runRunner, chatRunner, noJudge,
      noReview, recordingLogger().logger, okPreflight);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 300 && active < 2; i++) await tick();
    // Give the loop extra ticks to (wrongly) over-fill if the ceiling were not honored.
    for (let i = 0; i < 20; i++) await tick();
    assert.strictEqual(peak, 2, "never more than WORKER_CHAT_SESSIONS chats run at once");
    controller.abort(); // shutdown aborts the parked chats → clean drain
    await done;
  });
});

// The heartbeat loop self-reports container stats (PRD #49 M1): each tick collects a
// sample and passes it to client.heartbeat. On this (non-Linux) test host the cgroup
// files are absent, so the collector uses its process fallback — enough to prove the
// wiring: the heartbeat receives a well-formed WorkerStats.
describe("Worker — heartbeat carries a resource sample (PRD #49 M1)", () => {
  it("passes a collected WorkerStats to client.heartbeat", async () => {
    const controller = new AbortController();
    const seen: (WorkerStats | undefined)[] = [];
    const client = {
      register: async () => ({}),
      heartbeat: async (stats?: WorkerStats) => {
        seen.push(stats);
      },
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;

    const worker = new Worker(fakeConfig(), client, { execute: async () => {} } as unknown as RunRunner, {} as unknown as ChatRunner, noJudge,
      noReview, recordingLogger().logger, okPreflight);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && seen.length === 0; i++) await tick();
    controller.abort();
    await done;

    const stats = seen.find((s) => s !== undefined);
    assert.ok(stats, "the heartbeat received a stats sample");
    assert.ok(stats.source === "cgroup" || stats.source === "process", "source is a valid enum");
    assert.ok(Number.isFinite(stats.mem_bytes) && stats.mem_bytes >= 0, "mem_bytes is a non-negative number");
  });
});

// The RUN lane's slot semaphore (PRD #42 M2, Decision 1): the loop executes up to
// WORKER_MAX_CONCURRENT_RUNS runs as tracked promises, slot-before-claim, releasing
// a slot only when its run settles. These are worker-level unit tests with a faked
// RunRunner — the real plan-gate/executor path is covered in runner-plan-gate.test.ts
// and its sibling runner-*.test.ts files (split out of runner.test.ts 2026-08-03); here
// the subject is purely the semaphore (cap, backoff, release, drain).
describe("Worker — RUN lane slot semaphore (PRD #42 M2)", () => {
  it("runs up to WORKER_MAX_CONCURRENT_RUNS concurrently, logs at capacity, and does not claim while full", async () => {
    const controller = new AbortController();
    const release = new AbortController();
    const { client, claimCalls } = endlessRunClient();
    const { runner, peak, active } = parkingRunRunner(release.signal);
    const { logger, lines } = recordingLogger();

    // chatRunner unused: the chat lane always claims null here, so execute is never called.
    const worker = new Worker(fakeConfig({ maxConcurrentRuns: 2 }), client, runner, {} as unknown as ChatRunner, noJudge,
      noReview, logger, okPreflight);
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

    const worker = new Worker(fakeConfig({ maxConcurrentRuns: 1 }), client, runner, {} as unknown as ChatRunner, noJudge,
      noReview, logger, okPreflight);
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
      noJudge,
      noReview,
      recordingLogger().logger,
      okPreflight,
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
      noJudge,
      noReview,
      recordingLogger().logger,
      okPreflight,
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
      noJudge,
      noReview,
      recordingLogger().logger,
      okPreflight,
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

// PRD #92 M3: the boot toolchain preflight is a fail-loud REGISTRATION gate. A worker
// whose /nix store is missing the baked toolchain (a stale seed after an image roll)
// must THROW at run() start — before it ever calls client.register — so the pod surfaces
// the error to an operator instead of retrying forever or emitting silent 127s mid-run.
describe("Worker — boot toolchain preflight gate (PRD #92 M3)", () => {
  it("throws before register when the preflight fails, naming the missing tools", async () => {
    const controller = new AbortController();
    let registered = false;
    const client = {
      register: async () => {
        registered = true;
        return {};
      },
      heartbeat: async () => {},
      claimRun: async () => null,
      claimChat: async () => null,
    } as unknown as WorkerClient;

    const failing = () => ({ ok: false, missing: ["go", "gcc", "/opt/uzi-toolchain"] });
    const worker = new Worker(
      fakeConfig(),
      client,
      { execute: async () => {} } as unknown as RunRunner,
      {} as unknown as ChatRunner,
      noJudge,
      noReview,
      recordingLogger().logger,
      failing,
    );

    await assert.rejects(
      () => worker.run(controller.signal),
      /toolchain preflight failed: missing go, gcc, \/opt\/uzi-toolchain/,
      "run() rejects with a message naming the missing tools",
    );
    assert.strictEqual(registered, false, "register is NEVER called when the preflight fails");
  });

  it("registers normally when the preflight passes", async () => {
    const controller = new AbortController();
    let registered = false;
    const client = {
      register: async () => {
        registered = true;
        return {};
      },
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => null,
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;

    const worker = new Worker(
      fakeConfig(),
      client,
      { execute: async () => {} } as unknown as RunRunner,
      {} as unknown as ChatRunner,
      noJudge,
      noReview,
      recordingLogger().logger,
      okPreflight,
    );
    const done = worker.run(controller.signal);
    for (let i = 0; i < 300 && !registered; i++) await tick();
    assert.strictEqual(registered, true, "a passing preflight lets registration proceed");
    controller.abort();
    await done;
  });
});

// Issue #109: registerWithRetry classifies its failures. A 401/403 is a PERMANENT auth
// rejection of the worker join token (rotated or invalid) — retrying can never clear it,
// so it must FAIL LOUD (throw → main.ts fatal handler, exit 1). Every other error
// (network, timeout, 5xx, 408/429, any other status) keeps retrying with the capped backoff.
describe("Worker — register auth-rejection classification (issue #109)", () => {
  for (const status of [401, 403]) {
    it(`throws immediately on a ${status} auth rejection with no retry`, async () => {
      const controller = new AbortController();
      let calls = 0;
      const client = {
        register: async () => {
          calls++;
          throw new RequestError("POST", "/worker/register", status, "unauthorized");
        },
        heartbeat: async () => {},
        claimRun: async () => null,
        claimChat: async () => null,
      } as unknown as WorkerClient;

      const worker = new Worker(
        fakeConfig(),
        client,
        { execute: async () => {} } as unknown as RunRunner,
        {} as unknown as ChatRunner,
        noJudge,
      noReview,
        recordingLogger().logger,
        okPreflight,
      );

      await assert.rejects(
        () => worker.run(controller.signal),
        (err: unknown) => err instanceof RequestError && err.status === status,
        `run() rejects with the ${status} RequestError`,
      );
      assert.strictEqual(calls, 1, `register is called exactly once (no retry) on ${status}`);
    });
  }

  it("retries a transient 5xx with backoff, then succeeds", async () => {
    const controller = new AbortController();
    let calls = 0;
    const { logger, lines } = recordingLogger();
    const client = {
      register: async () => {
        calls++;
        if (calls <= 2) throw new RequestError("POST", "/worker/register", 503, "unavailable");
        return {};
      },
      heartbeat: async () => {},
      claimRun: async () => null,
      claimChat: async () => null,
    } as unknown as WorkerClient;

    const worker = new Worker(
      fakeConfig(),
      client,
      { execute: async () => {} } as unknown as RunRunner,
      {} as unknown as ChatRunner,
      noJudge,
      noReview,
      logger,
      okPreflight,
    );

    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && calls < 3; i++) await tick();
    assert.strictEqual(calls, 3, "register retried past the transient 5xx and succeeded on the 3rd call");
    assert.ok(
      lines.some((l) => (l as { msg?: string }).msg === "register failed, retrying"),
      "the transient retry path emitted a 'register failed, retrying' warn line",
    );
    controller.abort();
    await done;
  });

  it("retries a plain network error with backoff, then succeeds", async () => {
    const controller = new AbortController();
    let calls = 0;
    const { logger, lines } = recordingLogger();
    const client = {
      register: async () => {
        calls++;
        if (calls <= 2) throw new Error("ECONNREFUSED");
        return {};
      },
      heartbeat: async () => {},
      claimRun: async () => null,
      claimChat: async () => null,
    } as unknown as WorkerClient;

    const worker = new Worker(
      fakeConfig(),
      client,
      { execute: async () => {} } as unknown as RunRunner,
      {} as unknown as ChatRunner,
      noJudge,
      noReview,
      logger,
      okPreflight,
    );

    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && calls < 3; i++) await tick();
    assert.strictEqual(calls, 3, "register retried past the network error and succeeded on the 3rd call");
    assert.ok(
      lines.some((l) => (l as { msg?: string }).msg === "register failed, retrying"),
      "the network-error retry path emitted a 'register failed, retrying' warn line",
    );
    controller.abort();
    await done;
  });
});

// PRD #400 M4b — dispatch routing: a `task`-kind claim carrying review_target_run_id is
// a diff-review claim and must route to the ReviewRunner, NOT the normal task executor
// (RunRunner). A plain task claim (no review target) still routes to the RunRunner.
describe("Worker — diff-review dispatch (PRD #400 M4b)", () => {
  it("routes a review_target_run_id claim to the ReviewRunner, not the RunRunner", async () => {
    const controller = new AbortController();
    const routed: string[] = [];
    let gave = false;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        if (gave) return null;
        gave = true;
        return { run_id: "rev-1", kind: "task", review_target_run_id: "target-9" } as unknown as ClaimResponse;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;

    const runRunner = { execute: async () => { routed.push("runner"); } } as unknown as RunRunner;
    const judgeRunner = { execute: async () => { routed.push("judge"); } } as unknown as JudgeRunner;
    const reviewRunner = { execute: async (c: ClaimResponse) => { routed.push(`review:${c.review_target_run_id}`); } } as unknown as ReviewRunner;

    const worker = new Worker(fakeConfig(), client, runRunner, {} as unknown as ChatRunner, judgeRunner, reviewRunner, recordingLogger().logger, okPreflight);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && routed.length === 0; i++) await tick();
    controller.abort();
    await done;

    assert.deepStrictEqual(routed, ["review:target-9"], "the review claim went to the ReviewRunner only");
    assert.ok(!routed.includes("runner"), "the RunRunner was not called for a review claim");
    assert.ok(!routed.includes("judge"), "the JudgeRunner was not called for a review claim");
  });

  it("routes a plain task claim (no review target) to the RunRunner", async () => {
    const controller = new AbortController();
    const routed: string[] = [];
    let gave = false;
    const client = {
      register: async () => ({}),
      heartbeat: async () => {},
      claimRun: async (): Promise<ClaimResponse | null> => {
        if (gave) return null;
        gave = true;
        return { run_id: "task-1", kind: "task" } as unknown as ClaimResponse;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;

    const runRunner = { execute: async () => { routed.push("runner"); } } as unknown as RunRunner;
    const reviewRunner = { execute: async () => { routed.push("review"); } } as unknown as ReviewRunner;

    const worker = new Worker(fakeConfig(), client, runRunner, {} as unknown as ChatRunner, noJudge, reviewRunner, recordingLogger().logger, okPreflight);
    const done = worker.run(controller.signal);
    for (let i = 0; i < 500 && routed.length === 0; i++) await tick();
    controller.abort();
    await done;

    assert.deepStrictEqual(routed, ["runner"], "a plain task claim went to the RunRunner");
  });
});
