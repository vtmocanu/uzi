// Standalone composition launched only by TestStaleWorkerCancelE2E.
// All normal worker protocol traffic uses authenticated Go handlers and PostgreSQL.
import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { Worker } from "../../src/worker.js";
import { WorkerClient } from "../../src/client.js";
import { RunRunner } from "../../src/runner.js";
import { GitCache } from "../../src/git.js";
import { GitLabClient } from "../../src/forge.js";
import { Outbox } from "../../src/outbox.js";
import { ActiveRunRegistry } from "../../src/active-run-registry.js";
import { StatsCollector } from "../../src/stats.js";
import { quiesceRunAttempt, setQuiescenceViewForTests } from "../../src/run-quiescence.js";
import type { Config } from "../../src/config.js";
import type { Executor, RunContext } from "../../src/executor.js";
import type { ChatRunner } from "../../src/chat-runner.js";
import type { JudgeRunner } from "../../src/judge-runner.js";
import type { ReviewRunner } from "../../src/review-runner.js";
import type { Logger } from "../../src/log.js";
import { makeFixture } from "../fixture-repo.js";
import { scopedRealView } from "../fake-proc.js";

const cfg = JSON.parse(process.env.UZI_STALE_CANCEL_FIXTURE ?? "") as {
  url: string; token: string; first: string; second: string; worker: string; variant: string; scratch: string;
};
type Snapshot = {
  status: string; worker: string; generation: number; inputs: number; holds: number;
  custody_unchanged: boolean; polls: number; input_polls: number; blocked: number; beats: number; receipts: number; forbidden: number;
};
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => { resolve = r; });
  let settled = false;
  return { promise, resolve: () => { settled = true; resolve(); }, settled: () => settled };
}
// Every observation loop is sequential, bounded by ten seconds; any failure stops
// this scenario. Other scenarios are isolated by owner, token and worker directory.
async function until(label: string, ready: () => boolean | Promise<boolean>) {
  const deadline = performance.now() + 10_000;
  while (performance.now() < deadline) {
    if (await ready()) return;
    await new Promise((r) => setTimeout(r, 5));
  }
  assert.fail(label + " within 10s");
}
async function control(action: string): Promise<Snapshot> {
  const response = await fetch(cfg.url + "/fixture/" + action, {
    method: "POST", headers: { Authorization: "Bearer " + cfg.token }, signal: AbortSignal.timeout(2000),
  });
  assert.equal(response.status, 200, "fixture " + action);
  return await response.json() as Snapshot;
}

async function cancelOwner() {
  const response = await fetch(cfg.url + "/api/runs/" + cfg.first + "/inputs", {
    method: "POST", headers: { Authorization: "Bearer " + cfg.token, "Content-Type": "application/json" },
    body: JSON.stringify({ kind: "cancel" }), signal: AbortSignal.timeout(2000),
  });
  assert.equal(response.status, 202);
  assert.equal((await response.json() as { server_side: boolean }).server_side, true);
}

it("real worker stale owner cancel: " + cfg.variant, async () => {
  process.umask(0o077);
  setQuiescenceViewForTests(scopedRealView());
  const lines: string[] = [];
  const record = (message: string, fields?: Record<string, unknown>) => { lines.push(JSON.stringify({ message, fields })); };
  const log: Logger = { debug: record, info: record, warn: record, error: record,
    addSecret: () => {}, removeSecret: () => {}, child: () => log };
  const fx = makeFixture({ "README.md": "original\n" });
  // Only the offline forge's Git URL is substituted. GitCache and clone custody
  // operations run normally against a real local repository.
  const gitConfig = path.join(cfg.scratch, "git-config");
  fs.writeFileSync(gitConfig, `[url "${fx.originPath}"]\n\tinsteadOf = https://forge.example/fixture.git\n`);
  process.env.GIT_CONFIG_GLOBAL = gitConfig;
  process.env.GIT_CONFIG_SYSTEM = "/dev/null";
  const client = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", log, { httpTimeoutMs: 1000 });
  const git = new GitCache(fx.dataDir, log);
  const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log,
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
  await outbox.init();
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 256);
  const entered = deferred(), stopEntered = deferred(), releaseStop = deferred();
  const secondEntered = deferred(), releaseSecond = deferred();
  let source = "", gateSettled = false, firstSettled = false, secondStarted = false;
  let claimCalls = 0, forgeCalls = 0, terminalStates = 0, exactDetectedAt = 0, interruptedAt = 0, teardownAt = 0, admittedAt = 0;
  let generation = 0, stalePayloadGeneration = 0;
  const getInputs = client.getInputs.bind(client);
  client.getInputs = async (...args: Parameters<typeof getInputs>) => {
    const result = await getInputs(...args);
    if (result.terminalCancelled?.generation === generation) exactDetectedAt ||= performance.now();
    else if (result.terminalCancelled) stalePayloadGeneration = result.terminalCancelled.generation;
    return result;
  };
  const claim = client.claimRun.bind(client);
  client.claimRun = async (...args: Parameters<typeof claim>) => { claimCalls++; return claim(...args); };
  const report = client.reportState.bind(client);
  client.reportState = async (...args: Parameters<typeof report>) => {
    if (args[0] === cfg.first && ["cancelled", "completed", "failed"].includes(args[1].status)) terminalStates++;
    return report(...args);
  };
  const forge = new GitLabClient({ fetchFn: async () => { forgeCalls++; throw new Error("unexpected forge publication"); } });
  const runner = new RunRunner(client, git, (runId) => ({
    executor: {
      run: async (ctx: RunContext) => {
        if (runId === cfg.second) {
          secondStarted = true; secondEntered.resolve();
          await releaseSecond.promise;
          return { branch: ctx.branch, pausedAt: { completedCount: 0, total: 1 } };
        }
        assert.equal(runId, cfg.first);
        source = ctx.worktreePath;
        fs.writeFileSync(path.join(source, "README.md"), "dirty retained bytes\n");
        fs.writeFileSync(path.join(source, "untracked.txt"), "untracked retained bytes\n");
        // A real model executor also exits when the runner aborts its SDK signal.
        // Keep that shutdown seam while the human gate itself remains production code.
        let onAbort: (() => void) | undefined;
        try {
          const waiting = cfg.variant === "question-empty" || cfg.variant === "ordinary-outage"
            ? ctx.askUser!([{ question: "Which target?", header: "Target" }])
            : ctx.gatePlan!("# Retained plan\n- preserve source");
          entered.resolve();
          const aborted = new Promise<never>((_, reject) => {
            onAbort = () => reject(ctx.signal?.reason ?? new Error("executor aborted"));
            if (ctx.signal?.aborted) onAbort();
            else ctx.signal?.addEventListener("abort", onAbort, { once: true });
          });
          await Promise.race([waiting, aborted]);
          assert.fail("the live gate must reject on terminal control");
        } finally {
          if (onAbort) ctx.signal?.removeEventListener("abort", onAbort);
          gateSettled = true; interruptedAt = performance.now();
        }
      },
    } as Executor,
  }), log, 10, cfg.token, {
    gitlab: forge, outbox, activeRuns: registry, pollMs: 2, planApprovalTimeoutMs: 0, questionTimeoutMs: 0,
    checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
    quiesceRun: async (request) => {
      if (request.site === "stopped_source") { stopEntered.resolve(); await releaseStop.promise; }
      return quiesceRunAttempt(request);
    },
  });
  const execute = runner.execute.bind(runner);
  runner.execute = async (...args: Parameters<typeof execute>) => {
    // Worker admission is entry to execute; clone preparation precedes model entry.
    if (args[0].run_id === cfg.second) admittedAt = performance.now();
    generation = args[0].run_id === cfg.first ? args[0].claim_generation! : generation;
    try { return await execute(...args); }
    finally { if (args[0].run_id === cfg.first) { firstSettled = true; teardownAt = performance.now(); } }
  };
  const controller = new AbortController();
  const config = { workerName: "stale-cancel", workerTemplate: "base", dataDir: fx.dataDir,
    pollIntervalMs: 2, heartbeatIntervalMs: 10, chatPollMs: 2, chatSessions: 0,
    maxConcurrentRuns: 1, dockerWiring: {} } as Config;
  const worker = new Worker(config, client, runner, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner,
    log, () => ({ ok: true, missing: [] }), outbox, undefined, registry,
    undefined, undefined, undefined, undefined,
    (dataDir) => new StatsCollector({ dataDir, processRss: () => 64 * 1024 * 1024 }));
  const done = worker.run(controller.signal);
  let passed = false;
  try {
    await until("original executor enters gate", () => source !== "");
    await until("gate entered", entered.settled);
    const expected = cfg.variant === "question-empty" || cfg.variant === "ordinary-outage" ? "awaiting_input" : "awaiting_approval";
    await until("real gate state persisted", async () => (await control("inspect")).status === expected);
    // Add unrelated evidence AFTER startup: this proof must not ask boot recovery
    // to retire an intentionally unrelated report.
    const unrelated = "44444444-4444-4444-8444-444444444444";
    await outbox.journalFinalize(unrelated, 2);
    const unrelatedPath = path.join(fx.dataDir, "outbox", unrelated, "finalize-2.json");
    const unrelatedBytes = fs.readFileSync(unrelatedPath);
    const before = await control("outage");
    assert.equal(before.worker, cfg.worker);
    assert.ok(generation > 1, "real claim observes a current generation above one");
    assert.equal(before.generation, generation);
    assert.equal(before.inputs, 0, "empty inputs: no held receipts can fence execution");
    assert.equal(before.holds, 1, "real claim opened source custody");
    assert.equal(before.receipts, 0);
    assert.equal(gateSettled, false);
    await until("at least two blocked worker requests", async () => (await control("inspect")).blocked >= before.blocked + 2);
    assert.equal((await control("inspect")).status, expected);
    assert.equal(gateSettled, false);
    assert.equal(firstSettled, false);
    assert.equal(runner.isExecuting(cfg.first), true);
    assert.equal(registry.has(cfg.first), true);
    assert.equal(secondStarted, false);
    assert.equal(claimCalls, 1);
    if (cfg.variant !== "ordinary-outage") {
      await cancelOwner();
      const cancelled = await control("inspect");
      assert.equal(cancelled.status, "cancelled");
      assert.equal(cancelled.inputs, 0, "owner route did not synthesize a cancel row");
      assert.equal(cancelled.custody_unchanged, true);
    }
    const disconnected = await control("inspect");
    const reconnectAt = performance.now();
    await control("reconnect");
    if (cfg.variant === "ordinary-outage") {
      await until("ordinary reconnect successful input polls and heartbeats", async () => {
        const state = await control("inspect");
        return state.beats >= disconnected.beats + 3 && state.input_polls >= disconnected.input_polls + 3;
      });
      assert.equal((await control("inspect")).status, expected);
      assert.equal(gateSettled, false, "original gate survives ordinary outage");
      assert.equal(firstSettled, false);
      assert.equal(secondStarted, false);
      assert.equal(claimCalls, 1);
      assert.equal(runner.isExecuting(cfg.first), true);
      assert.equal(registry.has(cfg.first), true);
    } else {
      if (cfg.variant === "stale-generation") {
        await until("three stale-generation successful terminal polls", async () => (await control("inspect")).polls >= 3);
        assert.ok(stalePayloadGeneration > 0, "stale control carries a positive older generation");
        assert.ok(stalePayloadGeneration < generation);
        assert.equal(gateSettled, false, "stale generation cannot interrupt this gate");
        assert.equal(firstSettled, false);
        assert.equal(claimCalls, 1);
        assert.equal(registry.has(cfg.first), true);
        await control("exact");
      }
      // Mutation removes only the new control: this exact empty-poll assertion
      // must fail within ten seconds, without a gate budget or receipt deadline.
      await until("exact empty poll interrupts gate and enters stopped-source quiescence", () => gateSettled);
      await until("stopped-source quiescence entered", stopEntered.settled);
      assert.ok(exactDetectedAt > 0);
      assert.equal(firstSettled, false);
      assert.equal(runner.isExecuting(cfg.first), true);
      assert.equal(registry.has(cfg.first), true);
      assert.equal(claimCalls, 1);
      const beats = (await control("inspect")).beats;
      await until("heartbeats during held source quiescence", async () => (await control("inspect")).beats >= beats + 2);
      assert.equal(secondStarted, false);
      assert.equal(claimCalls, 1, "teardown still occupies maxConcurrentRuns=1");
      assert.equal(admittedAt, 0, "no next execution admitted while teardown is held");
      releaseStop.resolve();
      await until("same worker second executor admission", () => secondStarted);
      await until("second executor entered", secondEntered.settled);
      assert.ok(admittedAt >= teardownAt && admittedAt - teardownAt <= 1000,
        "worker admits execute within one second after real teardown");
      assert.equal(firstSettled, true);
      assert.equal(runner.isExecuting(cfg.first), false);
      assert.equal(registry.has(cfg.first), false);
      assert.equal(registry.has(cfg.second), true);
      const bareConfig = fs.readFileSync(path.join(git.barePathFor("https://forge.example/fixture.git"), "config"), "utf8");
      assert.match(bareConfig, /uzi-stopped/);
      assert.ok(bareConfig.includes(cfg.first));
      assert.ok(bareConfig.includes(source), "retained source journal attributes the original clone");
      console.log(JSON.stringify({ variant: cfg.variant, detectionMs: exactDetectedAt - reconnectAt,
        interruptionMs: interruptedAt - exactDetectedAt, teardownMs: teardownAt - interruptedAt,
        admissionMs: admittedAt - teardownAt }));
    }
    assert.equal(fs.readFileSync(path.join(source, "README.md"), "utf8"), "dirty retained bytes\n");
    assert.equal(fs.readFileSync(path.join(source, "untracked.txt"), "utf8"), "untracked retained bytes\n");
    assert.deepEqual(fs.readFileSync(unrelatedPath), unrelatedBytes);
    assert.deepEqual(outbox.listPendingTerminals(), []);
    assert.equal(terminalStates, 0, "no competing terminal state report");
    assert.equal(forgeCalls, 0);
    const after = await control("inspect");
    assert.equal(after.custody_unchanged, true, "source custody rows unchanged");
    assert.equal(after.worker, before.worker);
    assert.equal(after.generation, before.generation);
    assert.equal(after.receipts, 0);
    assert.equal(after.forbidden, 0);
    passed = true;
  } catch (error) {
    console.error(lines.join("\n").replaceAll(cfg.token, "[redacted]").replaceAll("fixture-model-secret", "[redacted]").replaceAll("fixture-forge-secret", "[redacted]"));
    throw error;
  } finally {
    // The ordinary-outage proof ends with its original question still live. Only
    // AFTER all retention assertions, cancel via the real stale-owner route for
    // fixture cleanup, avoiding graceful shutdown checkpoint publication.
    if (passed && cfg.variant === "ordinary-outage") {
      await control("outage");
      await cancelOwner();
      controller.abort();
      releaseStop.resolve();
      await control("reconnect");
      await until("ordinary-outage owned cleanup settles execution", () => firstSettled);
    }
    await control("reconnect");
    releaseStop.resolve();
    controller.abort(); runner.shutdown();
    releaseSecond.resolve();
    await done;
    if (passed) fx.cleanup();
  }
});

// Timing scope: fixture poll=2ms, HTTP=1s, no held receipts and zero gate budgets.
// Detection includes any in-flight input HTTP, receipt handling and inclusion drain.
// Execute settlement additionally includes source quiescence, message-batcher close
// and steering stop; admission is measured separately AFTER execute returns.
// Production uses poll=3s, HTTP=30s, receipt failure limits=30 attempts/60s,
// and at most three stop-inclusion attempts. These are not a universal 33s bound.
