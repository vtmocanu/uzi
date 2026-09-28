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
import { StatsCollector } from "../src/stats.js";
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

interface Harness {
  pressure: DiskPressureController;
  observed: Array<number | undefined>;
  claims: () => number;
  passes: () => number;
  setThreshold: (t: number) => void;
  stop: () => Promise<void>;
}

/** The real Worker loops over a stub client, with a controller built from `over`. */
function harness(over: { admission?: boolean; admissionMaxWaitMs?: number; reclaim?: boolean } = {}): Harness {
  let threshold = 0; // soft = 0: any known sample is over it
  let claims = 0;
  let passes = 0;
  const observed: Array<number | undefined> = [];
  const pressure = new DiskPressureController({
    softMargin: 0,
    thresholdOf: () => threshold,
    intervalMs: 60_000,
    admission: over.admission ?? true,
    admissionMaxWaitMs: over.admissionMaxWaitMs ?? 60 * 60_000,
    log: nullLogger(),
    ...(over.reclaim === false
      ? {}
      : {
          reclaim: async () => {
            passes++;
          },
        }),
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
    undefined,
    // issue #1863: stub only the RSS read (the default reads the proc filesystem, which the
    // Landlock test sandbox denies); the disk sample stays the real statfs of os.tmpdir().
    (dataDir) => new StatsCollector({ dataDir, processRss: () => 64 * 1024 * 1024 }),
  );
  const ac = new AbortController();
  const running = worker.run(ac.signal);
  return {
    pressure,
    observed,
    claims: () => claims,
    passes: () => passes,
    setThreshold: (t) => {
      threshold = t;
    },
    stop: async () => {
      ac.abort();
      await running;
    },
  };
}

describe("Worker × disk admission (PRD #1809 D5)", () => {
  it("claims no new run at or over the soft threshold, reclaims, and resumes claiming under it", async () => {
    const h = harness();
    try {
      await until(() => h.observed.length >= 3, "three heartbeat samples");
      const sample = h.observed.find((f) => f !== undefined);
      assert.ok(sample !== undefined && sample >= 0 && sample <= 1, "the heartbeat feeds the data volume's used fraction");
      assert.equal(h.pressure.claimsBlocked(), true);
      const blockedAt = h.claims();
      await tick(40);
      assert.ok(h.claims() <= blockedAt + 1, `no claims while over the soft threshold (claims ${blockedAt} -> ${h.claims()})`);
      assert.ok(h.passes() >= 1, "crossing the soft threshold triggered a reclaim pass");

      h.setThreshold(1); // soft = 1: back under
      await until(() => !h.pressure.claimsBlocked(), "back under the soft threshold");
      const resumedAt = h.claims();
      await until(() => h.claims() > resumedAt + 3, "claims resume under the soft threshold");
    } finally {
      await h.stop();
    }
  });

  it("UZI_DISK_ADMISSION off: keeps claiming over the soft threshold while the reclaim still runs", async () => {
    const h = harness({ admission: false });
    try {
      await until(() => h.observed.length >= 3, "three heartbeat samples");
      assert.equal(h.pressure.claimsBlocked(), false);
      const at = h.claims();
      await until(() => h.claims() > at + 3, "claims continue over the soft threshold");
      assert.ok(h.passes() >= 1, "the pressure reclaim still ran");
    } finally {
      await h.stop();
    }
  });

  it("the stop is bounded: still over after a reclaim and the admission wait, claiming resumes", async () => {
    const h = harness({ admissionMaxWaitMs: 30 });
    try {
      await until(() => h.observed.length >= 2 && h.pressure.claimsBlocked(), "blocked over the soft threshold");
      await until(() => h.passes() >= 1, "a pressure reclaim ran");
      await until(() => !h.pressure.claimsBlocked(), "the admission wait elapsed");
      const at = h.claims();
      await until(() => h.claims() > at + 3, "claims resume while still over the soft threshold");
    } finally {
      await h.stop();
    }
  });
});
