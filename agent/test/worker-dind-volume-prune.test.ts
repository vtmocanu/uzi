import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { rmSync } from "node:fs";

import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse } from "../src/protocol.js";
import { DindPruneGate, createDindPrune, type DockerExec } from "../src/dind-prune.js";
import { createDindVolumePrune } from "../src/dind-volume-prune.js";
import type { DindMeterSample } from "../src/dind-meter.js";
import { sleep } from "../src/util.js";
import { nullLogger } from "./helpers.js";

// issue #3891: the pressure-only anonymous-volume leg, driven through the REAL Worker claim
// loops. The leg is built with createDindVolumePrune and handed to createDindPrune exactly as
// main.ts does. A run is active (so the worker is NOT idle) and the api reports custody held
// (true) or unknown (undefined) on every heartbeat; sustained pressure must still run the leg's
// frozen argv, never any image/builder/ps argv, never touch the gate, and never stop claims.

const noJudge = { execute: async () => {} } as unknown as JudgeRunner;
const noReview = { execute: async () => {} } as unknown as ReviewRunner;
const okPreflight = () => ({ ok: true, missing: [] as string[] });
const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));

const DOCKER_HOST = "unix:///run/dind/docker.sock";

function fakeConfig(): Config {
  return {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 5,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dindPruneEnabled: true,
    dockerWiring: { dockerHost: DOCKER_HOST },
  } as unknown as Config;
}

function deferred<T = void>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

const configDirs = new Set<string>();
after(() => {
  for (const d of configDirs) rmSync(d, { recursive: true, force: true });
});

async function until(cond: () => boolean, what: string, max = 3000): Promise<void> {
  for (let i = 0; i < max && !cond(); i++) await tick();
  assert.ok(cond(), `timed out waiting for: ${what}`);
}

for (const custody of ["held (true)", "unknown (undefined)"] as const) {
  describe(`Worker x DinD volume leg with custody ${custody} (issue #3891)`, () => {
    it("runs the leg's frozen argv while a run is active, without gate changes, and claims keep flowing", async () => {
      const gate = new DindPruneGate();
      const execs: Array<{ argv: string[]; idle: boolean; claimsClosed: boolean; env: NodeJS.ProcessEnv }> = [];
      let n = 0;
      let worker!: Worker;
      const readSample = (): DindMeterSample => ({
        epochS: Math.floor(Date.now() / 1000) + 1 + n++,
        bytesUsed: 95,
        bytesTotal: 100,
        inodesUsed: 1,
        inodesTotal: 100,
      });
      const exec: DockerExec = async (argv, opts) => {
        if (opts.env.DOCKER_CONFIG) configDirs.add(opts.env.DOCKER_CONFIG);
        execs.push({ argv: [...argv], idle: worker.isIdle(), claimsClosed: gate.claimsClosed(), env: opts.env });
        return argv[0] === "version" ? "1.43\n" : "Total reclaimed space: 1.0GB\n";
      };
      const config = fakeConfig();
      const fastSleep = (ms: number, signal?: AbortSignal) => sleep(Math.min(ms, 2), signal);
      const volumeLeg = createDindVolumePrune(config, { gate, log: nullLogger(), readSample, exec, sleep: fastSleep });
      assert.ok(volumeLeg, "the leg is built under the cache prune's enablement predicate");
      const ctl = createDindPrune(config, {
        gate,
        isIdle: () => worker.isIdle(),
        log: nullLogger(),
        readSample,
        exec,
        sleep: fastSleep,
        volumeLeg,
      });
      assert.ok(ctl);

      const runGate = deferred<void>();
      let runStarted = false;
      let runClaims = 0;
      let heartbeats = 0;
      let chatClaims = 0;
      const runner = {
        resumePendingRecoveries: async () => {},
        execute: async () => {
          runStarted = true;
          await runGate.promise;
        },
      } as unknown as RunRunner;
      const client = {
        register: async () => ({ worker_id: "wid-1" }),
        heartbeat: async () => {
          heartbeats++;
          return custody === "held (true)" ? true : undefined;
        },
        hasFeature: () => false,
        claimRun: async (): Promise<ClaimResponse | null> => {
          runClaims++;
          return runClaims === 1 ? ({ run_id: "run-1" } as unknown as ClaimResponse) : null;
        },
        claimChat: async (): Promise<ChatClaimResponse | null> => {
          chatClaims++;
          return null;
        },
      } as unknown as WorkerClient;
      worker = new Worker(
        config, client, runner, { execute: async () => {} } as unknown as ChatRunner, noJudge, noReview,
        nullLogger(), okPreflight, undefined, undefined, undefined, undefined, ctl,
      );

      const controller = new AbortController();
      const done = worker.run(controller.signal);
      try {
        await until(() => runStarted, "the run is active");
        await until(() => execs.some((e) => e.argv[0] === "volume"), "the leg prunes while the run is active");
        // The run lane is at capacity with the active run, so the chat lane shows claims flowing.
        const claimsAtPrune = chatClaims;
        await until(() => chatClaims > claimsAtPrune + 3, "chat claim polling keeps flowing after the leg ran");
        assert.ok(heartbeats > 0, "heartbeats carried the custody flag throughout");
        assert.equal(worker.isIdle(), false, "the run is still active");

        assert.deepEqual(
          execs.map((e) => e.argv),
          [["version", "--format", "{{.Server.APIVersion}}"], ["volume", "prune", "-f"]],
          "exactly the leg's frozen pair: no ps, image or builder argv",
        );
        for (const e of execs) {
          assert.equal(e.idle, false, `${e.argv.join(" ")} ran while the worker was busy`);
          assert.equal(e.claimsClosed, false, "the leg never closes the claim gate");
          assert.equal(e.env.DOCKER_API_VERSION, "1.42");
        }
        assert.equal(gate.generation(), 0, "no close/open call by any owner");
        assert.equal(gate.claimsClosed(), false);
      } finally {
        runGate.resolve();
        controller.abort();
        await done;
      }
      assert.equal(gate.inFlightClaims(), 0);
    });
  });
}
