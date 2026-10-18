import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { StatsCollector } from "../src/stats.js";
import { quiesceRunAttempt } from "../src/run-quiescence.js";
import type { Config } from "../src/config.js";
import type { Executor, RunContext } from "../src/executor.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { api, client, fx, git, gitlabClaim, fakeGitlab, runnerWith, installHarness, deferred } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

async function until(ready: () => boolean) {
  for (let i = 0; i < 1000 && !ready(); i++) await new Promise((r) => setTimeout(r, 2));
  assert.ok(ready(), "condition reached within two seconds");
}

// Timing contract: production polling defaults to 3s and WorkerClient HTTP to 30s,
// including input response bodies. Reconnect detection waits for outstanding input/
// receipt work and inclusion flush, then the next successful poll or fenced receipt.
// Active receipt failure is bounded by 30 failures / 60s elapsed; stop drains at most
// three inclusion requests. Detection is distinct from teardown: executor settlement,
// source quiescence, message-batcher close and steering stop all precede slot release.
// Receipt give-up fails server-visibly; only confirmed terminal controls stop without a report.
// These short-poll fixtures hold quiescence behind an event barrier to prove that
// ordering. They do not assert a universal 33-second total or spend a gate deadline.
describe("M2 terminal cancellation real worker custody", () => {
  installHarness();
  for (const gate of ["plan", "question", "receipt deadline"] as const) {
    it(`${gate}: retains source and keeps capacity one until execute settles`, async (t) => {
      const first = gitlabClaim(29071, { claim_generation: 7 });
      const second = gitlabClaim(29072, { claim_generation: 8 });
      const unrelated = "44444444-4444-4444-8444-444444444444";
      const outbox = new Outbox({
        root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
      });
      await outbox.init();
      await outbox.journalFinalize(unrelated, 2);
      const unrelatedPath = path.join(fx.dataDir, "outbox", unrelated, "finalize-2.json");
      const unrelatedBytes = fs.readFileSync(unrelatedPath);
      const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 256);
      const gateEntered = deferred();
      const stopEntered = deferred();
      const releaseStop = deferred();
      const secondStarted = deferred();
      const releaseSecond = deferred();
      let source = "";
      let firstSettled = false;
      let gateSettled = false;
      let gateError: unknown;
      let deadlineAdvanced = false;
      const realNow = Date.now.bind(Date);
      // Only Date advances; Node's timers and both gate deadlines remain unchanged/disabled.
      if (gate === "receipt deadline") t.mock.method(Date, "now", () => realNow() + (deadlineAdvanced ? 60_001 : 0));
      const factory = (runId: string) => ({
        executor: {
          run: async (ctx: RunContext) => {
            if (runId !== first.run_id) {
              secondStarted.resolve();
              await releaseSecond.promise;
              return { branch: ctx.branch, pausedAt: { completedCount: 0, total: 1 } };
            }
            source = ctx.worktreePath;
            fs.writeFileSync(path.join(source, "README.md"), "dirty retained bytes\n");
            fs.writeFileSync(path.join(source, "untracked.txt"), "untracked retained bytes\n");
            try {
              const waiting = gate === "question"
                ? ctx.askUser!([{ question: "Which target?", header: "Target" }])
                : ctx.gatePlan!("# Retained plan\n- preserve source");
              gateEntered.resolve();
              await waiting;
              assert.fail("the stopped flight must reject the real gate");
            } catch (err) {
              gateError = err;
              throw err;
            } finally { gateSettled = true; }
          },
        } as Executor,
      });
      const { gitlab, calls } = fakeGitlab();
      const run = runnerWith(factory, gitlab, undefined, nullLogger(), {
        outbox, activeRuns: registry, planApprovalTimeoutMs: 0, questionTimeoutMs: 0,
        quiesceRun: async (request) => {
          if (request.site === "stopped_source") {
            stopEntered.resolve();
            await releaseStop.promise;
          }
          return quiesceRunAttempt(request);
        },
      });
      const execute = run.execute.bind(run);
      t.mock.method(run, "execute", async (...args: Parameters<typeof execute>) => {
        try { return await execute(...args); }
        finally { if (args[0].run_id === first.run_id) firstSettled = true; }
      });
      const controller = new AbortController();
      const config = {
        workerName: "terminal-test", workerTemplate: "base", dataDir: fx.dataDir,
        pollIntervalMs: 2, heartbeatIntervalMs: 10, chatPollMs: 2, chatSessions: 0,
        maxConcurrentRuns: 1, dockerWiring: {},
      } as Config;
      const worker = new Worker(config, client, run, {} as ChatRunner, {} as JudgeRunner,
        {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }), outbox,
        undefined, registry, undefined, undefined, undefined, undefined,
        (dataDir) => new StatsCollector({ dataDir, processRss: () => 64 * 1024 * 1024 }));
      let claimCalls = 0;
      const claimRun = client.claimRun.bind(client);
      t.mock.method(client, "claimRun", async (...args: Parameters<typeof claimRun>) => {
        claimCalls++;
        return claimRun(...args);
      });
      api.enqueueClaim(first);
      api.enqueueClaim(second);
      const heldGet = api.holdNextInputGet(first.run_id);
      const done = worker.run(controller.signal);
      t.after(async () => {
        heldGet.release(); releaseStop.resolve(); releaseSecond.resolve();
        controller.abort(); run.shutdown();
        await done;
      });
      try {
        await gateEntered.promise;
        await heldGet.entered;
        await until(() => api.states.some((s) => s.runId === first.run_id &&
          s.body.status === (gate === "question" ? "awaiting_input" : "awaiting_approval")));
        if (gate === "receipt deadline") {
          api.setInputs(first.run_id, [{ id: 71, kind: "follow_up", body: "keep this input" }]);
          api.failInputReceipts("applied", 503);
          const apply = client.applyInputs.bind(client);
          t.mock.method(client, "applyInputs", async (...args: Parameters<typeof apply>) => {
            try { return await apply(...args); }
            finally { deadlineAdvanced = true; }
          });
        } else {
          api.rawInputGets(first.run_id, { inputs: [], terminal_cancelled: { run_id: first.run_id, generation: 7 } });
        }
        heldGet.release();
        await stopEntered.promise;
        assert.equal(gateSettled, true, "the actual plan/question wait rejected");
        assert.ok(gateError instanceof Error);
        assert.equal(gateError.name, gate === "receipt deadline" ? "InputReceiptError" : "ClaimFencedSignal");
        assert.equal(firstSettled, false, "retention quiescence still owns execute");
        assert.equal(run.isExecuting(first.run_id), true);
        assert.equal(registry.has(first.run_id), true);
        assert.equal(claimCalls, 1, "control receipt has not freed capacity");
        const heartbeats = api.heartbeatBodies.length;
        await until(() => api.heartbeatBodies.length >= heartbeats + 2);
        assert.equal(claimCalls, 1, "independent worker heartbeats cannot admit a second claim during teardown");
        releaseStop.resolve();
        await Promise.race([secondStarted.promise, done.then(() => { throw new Error("worker exited before second claim"); })]);
        assert.equal(firstSettled, true);
        assert.equal(run.isExecuting(first.run_id), false);
        assert.equal(registry.has(first.run_id), false);
        assert.equal(registry.has(second.run_id), true);
        assert.equal(fs.readFileSync(path.join(source, "README.md"), "utf8"), "dirty retained bytes\n");
        assert.equal(fs.readFileSync(path.join(source, "untracked.txt"), "utf8"), "untracked retained bytes\n");
        const bareConfig = fs.readFileSync(path.join(git.barePathFor(fx.originPath), "config"), "utf8");
        assert.match(bareConfig, gate === "receipt deadline" ? /\[uzi-recovery "agent\/issue-29071"\]/ : /uzi-stopped/);
        assert.ok(bareConfig.includes(first.run_id), "retention journal names this run");
        assert.deepEqual(fs.readFileSync(unrelatedPath), unrelatedBytes, "unrelated finalize evidence is unchanged");
        assert.deepEqual(outbox.listPendingTerminals().map(({ run_id, claim_generation, server_settled }) =>
          ({ run_id, claim_generation, server_settled })), gate === "receipt deadline"
          ? [{ run_id: first.run_id, claim_generation: 7, server_settled: true }]
          : [], "an acknowledged failure retains its custody-pending journal without consuming a slot");
        const terminals = api.states.filter((s) => s.runId === first.run_id && ["completed", "failed", "cancelled"].includes(s.body.status));
        if (gate === "receipt deadline") {
          assert.ok(terminals.length > 0, "receipt give-up reports a server-visible failure");
          assert.match(terminals[0]!.body.failure_reason ?? "", /could not confirm 1 applied operator input/);
          for (const terminal of terminals) {
            assert.equal(terminal.body.status, "failed");
            assert.equal(terminal.body.claim_generation, 7);
            assert.deepEqual(terminal.body, terminals[0]!.body, "custody-pending retries replay the exact failed journal");
          }
        } else {
          assert.deepEqual(terminals, [], "confirmed terminal controls do not produce a competing report");
        }
        assert.equal(calls.length, 0, "no forge publication");
        if (gate === "receipt deadline") {
          assert.equal(deadlineAdvanced, true);
          assert.ok(api.inputReceiptCalls.some((c) => c.kind === "applied" && c.runId === first.run_id));
        }
      } finally {
        heldGet.release(); releaseStop.resolve(); releaseSecond.resolve();
        controller.abort(); run.shutdown();
        await done;
      }
    });
  }
});
