import { describe, it } from "node:test";
import assert from "node:assert/strict";
import os from "node:os";

import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse } from "../src/protocol.js";
import { DiskPressureController } from "../src/disk-reclaim.js";
import { nullLogger } from "./helpers.js";

// PRD #1809 D5: the admission stop, driven through the REAL Worker heartbeat and claim loops
// with the real StatsCollector sampling a real directory (os.tmpdir()). The volume's actual
// used fraction is whatever it is, so the test moves the THRESHOLD instead: a soft threshold
// of 0 puts any known sample at or over it, and one of 1 puts any not-completely-full volume
// under it.

const tick = (ms = 2): Promise<void> => new Promise((r) => setTimeout(r, ms));

async function until(cond: () => boolean, what: string, max = 2000): Promise<void> {
  for (let i = 0; i < max && !cond(); i++) await tick();
  assert.ok(cond(), `timed out waiting for: ${what}`);
}

describe("Worker × disk admission (PRD #1809 D5)", () => {
  it("claims no new run at or over the soft threshold, reclaims, and resumes claiming under it", async () => {
    let threshold = 0; // soft = max(0, 0 - 0) = 0: any known sample is over it
    let claims = 0;
    let passes = 0;
    const observed: Array<number | undefined> = [];
    const pressure = new DiskPressureController({
      softMargin: 0,
      thresholdOf: () => threshold,
      intervalMs: 60_000,
      log: nullLogger(),
      reclaim: async () => {
        passes++;
      },
    });
    const observe = pressure.observe.bind(pressure);
    pressure.observe = (f) => {
      observed.push(f);
      observe(f);
    };
    const client = {
      register: async () => ({ worker_id: "wid-1" }),
      heartbeat: async () => false,
      hasFeature: () => false,
      claimRun: async (): Promise<ClaimResponse | null> => {
        claims++;
        return null;
      },
      claimChat: async (): Promise<ChatClaimResponse | null> => null,
    } as unknown as WorkerClient;
    const worker = new Worker(
      {
        workerName: "w1",
        workerTemplate: "base",
        dataDir: os.tmpdir(),
        pollIntervalMs: 1,
        heartbeatIntervalMs: 5,
        chatPollMs: 1,
        chatSessions: 1,
        maxConcurrentRuns: 1,
      } as unknown as Config,
      client,
      { resumePendingRecoveries: async () => {}, execute: async () => {} } as unknown as RunRunner,
      { execute: async () => {} } as unknown as ChatRunner,
      { execute: async () => {} } as unknown as JudgeRunner,
      { execute: async () => {} } as unknown as ReviewRunner,
      nullLogger(),
      () => ({ ok: true, missing: [] as string[] }),
      undefined,
      undefined,
      undefined,
      undefined,
      undefined,
      pressure,
    );
    const ac = new AbortController();
    const running = worker.run(ac.signal);
    try {
      await until(() => observed.length >= 3, "three heartbeat samples");
      const sample = observed.find((f) => f !== undefined);
      assert.ok(sample !== undefined && sample >= 0 && sample <= 1, "the heartbeat feeds the data volume's used fraction");
      assert.equal(pressure.claimsBlocked(), true);
      const blockedAt = claims;
      await tick(40);
      assert.ok(claims <= blockedAt + 1, `no claims while over the soft threshold (claims ${blockedAt} -> ${claims})`);
      assert.ok(passes >= 1, "crossing the soft threshold triggered a reclaim pass");

      threshold = 1; // soft = 1: back under
      await until(() => !pressure.claimsBlocked(), "back under the soft threshold");
      const resumedAt = claims;
      await until(() => claims > resumedAt + 3, "claims resume under the soft threshold");
    } finally {
      ac.abort();
      await running;
    }
  });
});
