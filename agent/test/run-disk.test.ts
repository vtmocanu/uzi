import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { RunDiskSampler, type RunDiskSamplerOptions } from "../src/run-disk.js";
import type { RunHomeBytes } from "../src/rmtree.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { WorkerClient } from "../src/client.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse, RunDiskEntry, WorkerStats } from "../src/protocol.js";
import { nullLogger, recordingLogger } from "./helpers.js";

// PRD #1809 D8: the per-run HOME size on the heartbeat. The sampler measures in the background
// (never on the heartbeat path), only worker-owned run-id dirs whose run is live here or
// non-terminal at the api, and reports the largest first, bounded.

const id = (n: number): string => `18090000-0000-4000-8000-${String(n).padStart(12, "0")}`;
const GIB = 1024 ** 3;

/** The entries without their `sampled_at` (asserted by its own tests). */
const sizesOf = (entries: RunDiskEntry[] | undefined): Array<Omit<RunDiskEntry, "sampled_at">> | undefined =>
  entries?.map(({ sampled_at: _at, ...rest }) => rest);

function deferred<T = void>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => (resolve = r));
  return { promise, resolve };
}

async function until(cond: () => boolean, what: string): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 2));
  }
}

async function withRoot(fn: (root: string) => Promise<void>): Promise<void> {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-rundisk-"));
  try {
    await fn(root);
  } finally {
    fs.rmSync(root, { recursive: true, force: true });
  }
}

/** Options with a fake measure that reports `sizes` (GiB) by run id and records each call. */
function opts(root: string, sizes: Map<string, number>, extra: Partial<RunDiskSamplerOptions> = {}) {
  const measured: string[] = [];
  const o: RunDiskSamplerOptions = {
    homeRoot: root,
    intervalMs: 5 * 60_000,
    isRunLive: () => false,
    statusOf: async () => "limit_wait",
    log: nullLogger(),
    measure: async (home): Promise<RunHomeBytes> => {
      const runId = path.basename(home);
      measured.push(runId);
      const g = sizes.get(runId) ?? 0;
      return { homeBytes: g * GIB, cacheBytes: (g * GIB) / 2, entries: 1, truncated: false };
    },
    ...extra,
  };
  return { o, measured };
}

describe("RunDiskSampler (PRD #1809 D8)", () => {
  it("measures only worker-owned run dirs of live or non-terminal runs, largest first", async () => {
    await withRoot(async (root) => {
      const foreignUidDir = id(6);
      for (const n of [1, 2, 3, 4, 5, 6]) fs.mkdirSync(path.join(root, id(n)));
      fs.mkdirSync(path.join(root, "uzi-summary-abc"));
      fs.mkdirSync(path.join(root, "not-a-run"));
      const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-outside-"));
      fs.symlinkSync(outside, path.join(root, id(7)), "dir");
      try {
        const status = new Map<string, string | undefined>([
          [id(2), "paused"],
          [id(3), "completed"], // terminal: left to the reclaims
          [id(4), undefined], // 404: the api does not know it
        ]);
        const sizes = new Map([
          [id(1), 3],
          [id(2), 9],
          [id(3), 20],
          [id(4), 20],
          [id(5), 1],
          [id(6), 30],
          [id(7), 40],
        ]);
        const { o, measured } = opts(root, sizes, {
          isRunLive: (runId) => runId === id(1),
          statusOf: async (runId) => {
            if (runId === id(5)) return "recovery_wait";
            return status.get(runId);
          },
          workerUid: process.getuid?.() ?? 0,
          // A planted dir owned by another uid is never measured.
          lstat: async (p) => {
            const st = await fs.promises.lstat(p);
            return path.basename(p) === foreignUidDir ? { uid: st.uid + 1, isDirectory: () => st.isDirectory() } : st;
          },
        });
        const entries = await new RunDiskSampler(o).sample();
        assert.deepStrictEqual(
          entries.map((e) => e.run_id),
          [id(2), id(1), id(5)],
        );
        assert.deepStrictEqual(sizesOf(entries)?.[0], { run_id: id(2), home_bytes: 9 * GIB, cache_bytes: 4.5 * GIB });
        assert.deepStrictEqual(measured.sort(), [id(1), id(2), id(5)].sort(), "nothing else was walked");
      } finally {
        fs.rmSync(outside, { recursive: true, force: true });
      }
    });
  });

  it("keeps at most maxEntries runs (the largest), flags a truncated reading, and bounds the runs it measures", async () => {
    await withRoot(async (root) => {
      const sizes = new Map<string, number>();
      for (let n = 1; n <= 8; n++) {
        fs.mkdirSync(path.join(root, id(n)));
        sizes.set(id(n), n);
      }
      const { o } = opts(root, sizes, { maxEntries: 3 });
      const top = await new RunDiskSampler(o).sample();
      assert.deepStrictEqual(
        top.map((e) => e.home_bytes / GIB),
        [8, 7, 6],
      );
      const bounded = opts(root, sizes, { maxMeasured: 2 });
      assert.strictEqual((await new RunDiskSampler(bounded.o).sample()).length, 2);
      assert.strictEqual(bounded.measured.length, 2, "at most maxMeasured HOMEs are walked per sample");
      const trunc = opts(root, sizes, {
        measure: async () => ({ homeBytes: 5, cacheBytes: 1, entries: 9, truncated: true }),
        maxEntries: 1,
      });
      const [one, ...rest] = await new RunDiskSampler(trunc.o).sample();
      assert.deepStrictEqual(rest, []);
      assert.ok(one);
      assert.deepStrictEqual(sizesOf([{ ...one, run_id: "" }]), [{ run_id: "", home_bytes: 5, cache_bytes: 1, truncated: true }]);
    });
  });

  it("skips a HOME whose measure fails, and stops status lookups after consecutive api failures", async () => {
    await withRoot(async (root) => {
      for (let n = 1; n <= 6; n++) fs.mkdirSync(path.join(root, id(n)));
      let lookups = 0;
      const { o } = opts(root, new Map([[id(1), 1]]), {
        isRunLive: (runId) => runId === id(1) || runId === id(2),
        statusOf: async () => {
          lookups++;
          throw new Error("api down");
        },
        measure: async (home) => {
          if (path.basename(home) === id(2)) throw new Error("agent-uid helper exited 7");
          return { homeBytes: 1, cacheBytes: 0, entries: 1, truncated: false };
        },
      });
      const entries = await new RunDiskSampler(o).sample();
      assert.deepStrictEqual(sizesOf(entries), [{ run_id: id(1), home_bytes: 1, cache_bytes: 0 }], "the live run is still measured");
      assert.strictEqual(lookups, 3, "the bail stops asking an unreachable api");
    });
  });

  it("current() never waits on a measure: it returns the last finished sample and starts the next one when due", async () => {
    await withRoot(async (root) => {
      fs.mkdirSync(path.join(root, id(1)));
      let now = 1_000_000;
      let gate = deferred();
      let calls = 0;
      let size = 1;
      const { o } = opts(root, new Map(), {
        now: () => now,
        measure: async () => {
          calls++;
          await gate.promise;
          return { homeBytes: size, cacheBytes: 0, entries: 1, truncated: false };
        },
      });
      const sampler = new RunDiskSampler(o);
      assert.strictEqual(sampler.current(), undefined, "nothing before the first sample finishes");
      // The measure is blocked; current() keeps returning at once.
      for (let i = 0; i < 5; i++) assert.strictEqual(sampler.current(), undefined);
      await until(() => calls === 1, "the first measure to start");
      for (let i = 0; i < 5; i++) assert.strictEqual(sampler.current(), undefined);
      await new Promise((r) => setTimeout(r, 10));
      assert.strictEqual(calls, 1, "one sample in flight, never a second");
      gate.resolve();
      await sampler.settled();
      assert.deepStrictEqual(sizesOf(sampler.current()), [{ run_id: id(1), home_bytes: 1, cache_bytes: 0 }]);
      // Within the interval: no new sample.
      now += 60_000;
      sampler.current();
      await sampler.settled();
      assert.strictEqual(calls, 1);
      // Past it: the next sample starts, and the previous reading is served meanwhile.
      now += 5 * 60_000;
      gate = deferred();
      size = 2;
      assert.deepStrictEqual(sizesOf(sampler.current()), [{ run_id: id(1), home_bytes: 1, cache_bytes: 0 }]);
      await until(() => calls === 2, "the next measure to start");
      gate.resolve();
      await sampler.settled();
      assert.deepStrictEqual(sizesOf(sampler.current()), [{ run_id: id(1), home_bytes: 2, cache_bytes: 0 }]);
    });
  });

  it("a failed sample keeps the previous reading; interval 0 never samples", async () => {
    await withRoot(async (root) => {
      fs.mkdirSync(path.join(root, id(1)));
      let now = 0;
      const { o } = opts(root, new Map([[id(1), 1]]), { now: () => now });
      const sampler = new RunDiskSampler(o);
      sampler.current();
      await sampler.settled();
      const first = sampler.current();
      assert.ok(first && first.length === 1);
      fs.rmSync(root, { recursive: true, force: true });
      fs.mkdirSync(root);
      fs.chmodSync(root, 0o000); // listing now fails (EACCES) for a non-root user
      now += 10 * 60_000;
      sampler.current();
      await sampler.settled();
      fs.chmodSync(root, 0o700);
      if (process.getuid?.() !== 0) assert.deepStrictEqual(sampler.current(), first, "the old reading stands");

      const off = new RunDiskSampler({ ...opts(root, new Map()).o, intervalMs: 0 });
      assert.strictEqual(off.current(), undefined);
      await off.settled();
      assert.strictEqual(off.current(), undefined);
    });
  });
});

describe("RunDiskSampler (PRD #1809 D8) — sampled_at, partial samples and rotation", () => {
  /** Run one due sample through the production path (current → settled) and return the reading. */
  async function tick(sampler: RunDiskSampler, clock: { now: number }): Promise<RunDiskEntry[] | undefined> {
    clock.now += 10 * 60_000;
    sampler.current();
    await sampler.settled();
    return sampler.current();
  }

  it("stamps each entry with when its sample FINISHED measuring (RFC 3339 UTC)", async () => {
    await withRoot(async (root) => {
      fs.mkdirSync(path.join(root, id(1)));
      fs.mkdirSync(path.join(root, id(2)));
      const clock = { now: Date.UTC(2026, 8, 28, 12, 0, 0) };
      const { o } = opts(root, new Map([[id(1), 2], [id(2), 1]]), {
        now: () => clock.now,
        measure: async (home) => {
          clock.now += 90_000; // each measure takes 90s of wall time
          return { homeBytes: path.basename(home) === id(1) ? 2 : 1, cacheBytes: 0, entries: 1, truncated: false };
        },
      });
      const entries = await new RunDiskSampler(o).sample();
      const finished = new Date(clock.now).toISOString();
      assert.strictEqual(finished, "2026-09-28T12:03:00.000Z", "two 90s measures after the start");
      assert.deepStrictEqual(
        entries.map((e) => e.sampled_at),
        [finished, finished],
        "the finish time, not the start time",
      );
    });
  });

  it("a partial sample keeps the previous entries of the runs it did not reach, while their HOME is still worker-owned", async () => {
    await withRoot(async (root) => {
      for (let n = 1; n <= 5; n++) fs.mkdirSync(path.join(root, id(n)));
      const clock = { now: Date.UTC(2026, 8, 28, 12, 0, 0) };
      const sizes = new Map([[id(1), 1], [id(2), 2], [id(3), 3], [id(4), 4], [id(5), 5]]);
      let apiDownFor = new Set<string>();
      let foreign = new Set<string>();
      const rec = recordingLogger();
      const { o } = opts(root, sizes, {
        now: () => clock.now,
        log: rec.logger,
        workerUid: process.getuid?.() ?? 0,
        statusOf: async (runId) => {
          if (apiDownFor.has(runId)) throw new Error("api 503");
          return "limit_wait";
        },
        lstat: async (p) => {
          const st = await fs.promises.lstat(p);
          return foreign.has(path.basename(p)) ? { uid: st.uid + 1, isDirectory: () => st.isDirectory() } : st;
        },
      });
      const sampler = new RunDiskSampler(o);
      const first = await tick(sampler, clock);
      assert.strictEqual(first?.length, 5);
      const firstAt = first[0]!.sampled_at;
      assert.ok(!rec.lines.some((l) => (l as { partial?: boolean }).partial), "a complete sample is not partial");

      // Second sample: the api cannot answer for id(2) and id(4); id(3)'s HOME is gone; id(4)'s is
      // now owned by another uid; id(5) grew.
      apiDownFor = new Set([id(2), id(4)]);
      fs.rmSync(path.join(root, id(3)), { recursive: true });
      foreign = new Set([id(4)]);
      sizes.set(id(5), 50);
      const second = await tick(sampler, clock);
      const secondAt = new Date(clock.now).toISOString();
      assert.deepStrictEqual(
        second?.map((e) => [e.run_id, e.home_bytes / GIB, e.sampled_at]),
        [
          [id(5), 50, secondAt], // re-measured
          [id(2), 2, firstAt], // not reached (lookup failed): the previous entry, with its own time
          [id(1), 1, secondAt], // re-measured
        ],
        "id(3) (HOME gone) and id(4) (no longer worker-owned) are dropped, not carried",
      );
      const partial = rec.lines.filter((l) => (l as { partial?: boolean }).partial === true);
      assert.strictEqual(partial.length, 1, "the partial sample is logged as such");
      assert.deepStrictEqual(
        { failures: (partial[0] as Record<string, unknown>).status_lookup_failures, carried: (partial[0] as Record<string, unknown>).carried },
        { failures: 1, carried: 1 },
      );

      // Third sample: complete again, id(2) is re-measured with a fresh time.
      apiDownFor = new Set();
      const third = await tick(sampler, clock);
      const thirdAt = new Date(clock.now).toISOString();
      assert.deepStrictEqual(
        third?.map((e) => [e.run_id, e.sampled_at]),
        [
          [id(5), thirdAt],
          [id(2), thirdAt],
          [id(1), thirdAt],
        ],
      );
    });
  });

  it("a sample that hits its deadline keeps the unreached runs' previous entries", async () => {
    await withRoot(async (root) => {
      for (let n = 1; n <= 3; n++) fs.mkdirSync(path.join(root, id(n)));
      const clock = { now: Date.UTC(2026, 8, 28, 12, 0, 0) };
      let slow = false;
      const rec = recordingLogger();
      const { o } = opts(root, new Map([[id(1), 1], [id(2), 2], [id(3), 3]]), {
        now: () => clock.now,
        log: rec.logger,
        sampleDeadlineMs: 60_000,
        measure: async (home) => {
          if (slow) clock.now += 61_000; // the first measure uses up the whole sample deadline
          const g = Number(path.basename(home).slice(-1));
          return { homeBytes: g * GIB, cacheBytes: 0, entries: 1, truncated: false };
        },
      });
      const sampler = new RunDiskSampler(o);
      const first = await tick(sampler, clock);
      assert.strictEqual(first?.length, 3);
      slow = true;
      const second = await tick(sampler, clock);
      assert.deepStrictEqual(
        second?.map((e) => e.run_id),
        [id(3), id(2), id(1)],
        "a deadline-cut sample does not blank the runs it did not reach",
      );
      assert.strictEqual(second?.filter((e) => e.sampled_at !== first?.[0]?.sampled_at).length, 1, "one run re-measured");
      assert.ok(
        rec.lines.some((l) => (l as { partial?: boolean; stopped?: string }).partial && (l as { stopped?: string }).stopped === "deadline"),
        "logged as a deadline-cut partial sample",
      );
    });
  });

  it("rotates the start across samples, so HOMEs past the per-sample budget are measured next time", async () => {
    await withRoot(async (root) => {
      for (let n = 1; n <= 5; n++) fs.mkdirSync(path.join(root, id(n)));
      const clock = { now: Date.UTC(2026, 8, 28, 12, 0, 0) };
      const sizes = new Map([[id(1), 1], [id(2), 2], [id(3), 3], [id(4), 4], [id(5), 5]]);
      const { o, measured } = opts(root, sizes, { now: () => clock.now, maxMeasured: 2 });
      const sampler = new RunDiskSampler(o);
      const rounds: string[][] = [];
      let reading: RunDiskEntry[] | undefined;
      for (let i = 0; i < 4; i++) {
        const before = measured.length;
        reading = await tick(sampler, clock);
        rounds.push(measured.slice(before));
      }
      assert.deepStrictEqual(rounds, [
        [id(1), id(2)],
        [id(3), id(4)],
        [id(5), id(1)],
        [id(2), id(3)],
      ]);
      assert.deepStrictEqual(
        reading?.map((e) => e.run_id),
        [id(5), id(4), id(3), id(2), id(1)],
        "every HOME is in the reading, the trailing ones included",
      );
    });
  });
});

describe("Worker heartbeat — PRD #1809 D8 run_disk", () => {
  it("carries the sampler's latest finished reading and keeps beating while a measure is stuck", async () => {
    await withRoot(async (root) => {
      fs.mkdirSync(path.join(root, id(1)));
      const release = deferred();
      let measuring = 0;
      const sampler = new RunDiskSampler({
        homeRoot: root,
        intervalMs: 1,
        isRunLive: () => true,
        statusOf: async () => undefined,
        log: nullLogger(),
        measure: async () => {
          measuring++;
          await release.promise;
          return { homeBytes: 7 * GIB, cacheBytes: 3 * GIB, entries: 1, truncated: false };
        },
      });
      const beats: Array<WorkerStats | undefined> = [];
      const client = {
        register: async () => ({ worker_id: "wid-1" }),
        heartbeat: async (stats: WorkerStats | undefined) => {
          beats.push(stats === undefined ? undefined : { ...stats });
          return false;
        },
        hasFeature: () => false,
        claimRun: async (): Promise<ClaimResponse | null> => null,
        claimChat: async (): Promise<ChatClaimResponse | null> => null,
      } as unknown as WorkerClient;
      const config = {
        workerName: "w1",
        workerTemplate: "base",
        pollIntervalMs: 1,
        heartbeatIntervalMs: 2,
        chatPollMs: 1,
        chatSessions: 1,
        maxConcurrentRuns: 1,
        dataDir: root,
      } as unknown as Config;
      const runner = { resumePendingRecoveries: async () => {}, execute: async () => {} } as unknown as RunRunner;
      const worker = new Worker(
        config,
        client,
        runner,
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
        undefined,
        sampler,
      );
      const ac = new AbortController();
      const done = worker.run(ac.signal);
      try {
        await until(() => beats.length >= 5, "heartbeats while the measure is stuck");
        assert.strictEqual(measuring, 1, "one measure in flight, the heartbeat never started another");
        assert.ok(beats.every((b) => b !== undefined && !("run_disk" in b)), "no run_disk before a sample finishes");
        release.resolve();
        await until(() => beats.some((b) => b?.run_disk !== undefined), "a heartbeat with run_disk");
        const withDisk = beats.find((b) => b?.run_disk !== undefined);
        assert.deepStrictEqual(sizesOf(withDisk?.run_disk), [{ run_id: id(1), home_bytes: 7 * GIB, cache_bytes: 3 * GIB }]);
        assert.match(withDisk?.run_disk?.[0]?.sampled_at ?? "", /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z$/);
      } finally {
        ac.abort();
        await done.catch(() => undefined);
      }
    });
  });
});
