import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type * as http from "node:http";
import { Worker } from "../src/worker.js";
import { Outbox } from "../src/outbox.js";
import { StatsCollector } from "../src/stats.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { PhaseSwitchUnverifiedError } from "../src/phase-switch-unverified.js";
import { PHASE_SWITCH_HOLD_REASON, type PhaseSwitchHoldOutcome } from "../src/protocol.js";
import { api, client, fakeGitlab, fx, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";
import { pollUntil } from "./worker-sweep-rig.js";

installHarness();

// PRD #3252 D8 (m4), integrated: a real Worker claim loop (max concurrency 1), the real RunRunner and the
// real WorkerClient over the fake api; only the executor (the SDK query) is a scripted stand-in. The hold
// POST is applied server-side but its ACK is lost; the owner then Resumes (queued with the phase-switch
// hold reason at the same generation). The old flight must end quietly, free its slot, drop out of the
// heartbeat snapshot, and the next claim (generation 2) must be admitted and run the barrier again.

function workerConfig(): Config {
  return {
    workerName: "phase-switch-requeue", workerTemplate: "base", pollIntervalMs: 1, heartbeatIntervalMs: 5,
    chatPollMs: 1, chatSessions: 0, maxConcurrentRuns: 1, crossCheckSlots: 0, dockerWiring: {}, dataDir: fx.dataDir,
    gapFillMax: 100, outboxTerminalMaxBytes: 1 << 20,
  } as unknown as Config;
}

const TERMINAL = ["completed", "failed", "cancelled"];

describe("phase-switch hold across a real Worker claim loop (issue #3252 m4)", () => {
  it("a lost hold ACK then an owner Resume frees the slot and the next claim re-runs the barrier", async () => {
    api.setRegisterProtocolFeatures(["active_run_snapshot", "heartbeat_outbox", "claim_generation_fence"]);
    api.registerNonce = "33333333-0000-4000-8000-000000000004";
    const { gitlab } = fakeGitlab();
    const iid = 32590;
    const gen1 = gitlabClaim(iid, {
      plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it", session_id: null, claim_generation: 1,
      config: { plan_phase: { model: "opus", effort: "high" } },
    });
    const runId = gen1.run_id;
    const gen2 = { ...gen1, claim_generation: 2 };

    const events: string[] = [];
    let heartbeatsAtSecondRun = -1;
    let finished = false;
    const executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        events.push(`run:${ctx.claimGeneration}`);
        if (ctx.claimGeneration === 2) heartbeatsAtSecondRun = api.heartbeatBodies.length;
        // The settings barrier cannot prove the planning pair was applied.
        throw new PhaseSwitchUnverifiedError("model_mismatch", { model: "opus", effort: "high" }, { model: "claude-sonnet-5-5", effort: "high" });
      },
      closeAndSettlePlanSummary: async () => {},
      killAgentTree: () => {},
    };

    const original = client.requestPhaseSwitchHold.bind(client);
    (client as unknown as { requestPhaseSwitchHold: unknown }).requestPhaseSwitchHold = async (
      _id: string, args: { claimGeneration: number },
    ): Promise<PhaseSwitchHoldOutcome> => {
      events.push(`post:${args.claimGeneration}`);
      // Generation 1: the request is applied server-side but its reply is lost. Generation 2 is answered.
      return args.claimGeneration === 1
        ? { outcome: "unknown", detail: "socket hang up" }
        : { outcome: "held", status: "paused" };
    };

    let reads = 0;
    type Snap = { active_snapshot?: { active?: Array<{ run_id: string; claim_generation: number }> } };
    const listsGeneration1 = (): boolean => (api.heartbeatBodies as Snap[]).some((b) =>
      (b.active_snapshot?.active ?? []).some((e) => e.run_id === runId && e.claim_generation === 1));
    api.ownershipWire.set(runId, async (res: http.ServerResponse) => {
      reads++;
      // Positive control for the snapshot assertions below: the live generation 1 flight is listed.
      if (reads === 1) await pollUntil(listsGeneration1, 10_000, "a heartbeat snapshot listing the generation 1 flight");
      // Every read made before the generation 2 claim is served belongs to the old flight (the single
      // slot is still held); the new claim's own admission reads come after and are not under test.
      if (api.claimsServed < 2) events.push(`own:${reads}`);
      // Read 1: the owner already Resumed: queued, the phase-switch hold reason, still at generation 1.
      // Later reads (the new claim's own admission checks): the run is running at generation 2.
      const row = reads === 1
        ? { status: "queued", claim_generation: 1, claim_released: false, hold_reason: PHASE_SWITCH_HOLD_REASON, cancellation_requested: false }
        : { status: "running", claim_generation: 2, claim_released: false, hold_reason: null, cancellation_requested: false };
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify(row));
      // The server re-offers the run on a new claim generation.
      if (reads === 1) api.enqueueClaim(gen2);
    });

    const outbox = new Outbox({
      root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    const activeRuns = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => client.workerOutboxMaxPending);
    const runner = runnerWith(() => ({ executor }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5, outbox, activeRuns });
    const chat = { execute: async () => {} };
    const worker = new Worker(workerConfig(), client, runner, chat as unknown as ChatRunner,
      chat as unknown as JudgeRunner, chat as unknown as ReviewRunner, nullLogger(),
      () => ({ ok: true, missing: [] }), outbox, undefined, activeRuns,
      undefined, undefined, undefined, undefined,
      (dataDir) => new StatsCollector({ dataDir, processRss: () => 64 * 1024 * 1024 }));

    const controller = new AbortController();
    api.enqueueClaim(gen1);
    const done = worker.run(controller.signal).finally(() => { finished = true; });
    try {
      await pollUntil(() => events.includes("post:2"), 20_000, `the generation 2 claim ran the barrier and requested its hold (events so far: ${events.join(",")})`).catch((e: Error) => {
        throw new Error(`${e.message}; events now: ${events.join(",")}; states: ${api.states.map((s) => s.body.status).join(",")}; claimsServed=${api.claimsServed}; registryHas=${activeRuns.has(runId)}; executing=${runner.isExecuting(runId)}; hb=${JSON.stringify(api.heartbeatBodies.slice(-1)[0]?.active_snapshot)}`);
      });
      // Let the heartbeat loop report the now-idle worker a few times.
      const baseline = api.heartbeatBodies.length;
      await pollUntil(() => api.heartbeatBodies.length >= baseline + 3, 5_000, "idle heartbeats");
    } finally {
      controller.abort();
      await done.catch(() => undefined);
      (client as unknown as { requestPhaseSwitchHold: unknown }).requestPhaseSwitchHold = original;
    }
    assert.ok(finished);

    // The old flight observed the queued row once, never posted again, and the new claim ran after it.
    assert.deepEqual(events, ["run:1", "post:1", "own:1", "run:2", "post:2"], events.join(","));
    // No finalize, failure or other terminal report for either generation.
    const states = api.states.filter((s) => s.runId === runId).map((s) => s.body.status as string);
    assert.deepEqual(states.filter((s) => TERMINAL.includes(s)), [], `states: ${states.join(",")}`);
    // The retained clone survived both flights.
    assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
    // The heartbeat snapshot dropped the generation 1 flight once it ended: from the second run's start on,
    // no snapshot names generation 1, and the last one is empty.
    const bodies = api.heartbeatBodies.slice(Math.max(0, heartbeatsAtSecondRun)) as Snap[];
    const entries = bodies.flatMap((b) => b.active_snapshot?.active ?? []).filter((e) => e.run_id === runId);
    assert.ok(!entries.some((e) => e.claim_generation === 1), "the generation 1 flight is not in any later snapshot");
    const last = (api.heartbeatBodies[api.heartbeatBodies.length - 1] as Snap).active_snapshot;
    assert.ok(last !== undefined, "snapshots were negotiated and sent");
    assert.deepEqual((last.active ?? []).filter((e) => e.run_id === runId), [], "the slot is free and the run is not listed");
  });
});
