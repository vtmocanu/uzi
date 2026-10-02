import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { type CacheCapConfig, DiskGovernor, type DiskGovernorOptions, hardStopThreshold, runCacheCapBytes } from "../src/cache-cap.js";
import type { RunCacheDropResult, TrimResult, TrimTarget } from "../src/run-caches.js";
import { nullLogger } from "./helpers.js";
import type { Logger } from "../src/log.js";

// PRD #1809 D4: the cache governor's two layers with every I/O faked: an injected volume size,
// injected cache sizes (the measurement), a recording trim, and a controllable clock.

const GIB = 1024 ** 3;
const VOLUME = 25 * GIB;

const CONFIG: CacheCapConfig = {
  capEnabled: true,
  capFraction: 0.5,
  lowWater: 0.6,
  maxConcurrentRuns: 2,
  hardStopEnabled: true,
  hardMargin: 0.03,
};
/** 0.5 x 25 GiB / 2 runs. */
const CAP = Math.floor((VOLUME * 0.5) / 2);

interface Harness {
  gov: DiskGovernor;
  sizes: Map<string, number>;
  trims: { home: string; target: TrimTarget }[];
  stops: string[];
  /** Issue #1830: the runs whose caches were dropped in place, in order. */
  inPlace: string[];
  /** What an in-place drop does to the run's measured bytes (default: all gone). */
  inPlaceLeaves: { bytes: number | undefined; throws: boolean };
  reclaims: () => number;
  clock: { now: number };
  /** The governor's warn messages, in order. */
  warns: string[];
  /** The governor's warn messages with their structured fields, in order. */
  warnFields: { msg: string; fields?: Record<string, unknown> }[];
  /** While set, every in-place drop waits for it before it does anything (a PENDING drop). */
  inPlaceHold: { until?: Promise<void> };
}

function harness(opts: { config?: Partial<CacheCapConfig>; trimTo?: (target: TrimTarget) => number; threshold?: number } = {}): Harness {
  const sizes = new Map<string, number>();
  const trims: { home: string; target: TrimTarget }[] = [];
  const stops: string[] = [];
  const inPlace: string[] = [];
  const inPlaceLeaves: Harness["inPlaceLeaves"] = { bytes: 0, throws: false };
  let reclaims = 0;
  const clock = { now: 1_000_000 };
  const warns: string[] = [];
  const warnFields: Harness["warnFields"] = [];
  const inPlaceHold: Harness["inPlaceHold"] = {};
  const log: Logger = { ...nullLogger(), warn: (m: string, fields?: Record<string, unknown>) => {
      warns.push(m);
      warnFields.push({ msg: m, fields });
    },
  };
  const options: DiskGovernorOptions = {
    config: { ...CONFIG, ...opts.config },
    log,
    volumeTotalBytes: () => VOLUME,
    thresholdOf: () => opts.threshold,
    reclaim: async () => {
      reclaims++;
    },
    measure: async (home) => {
      const b = sizes.get(home);
      if (b === undefined) throw new Error("no such HOME");
      return { cacheBytes: b, entries: 1, truncated: false };
    },
    trim: async (home, target): Promise<TrimResult> => {
      trims.push({ home, target });
      const after = opts.trimTo ? opts.trimTo(target) : target.lowWaterBytes;
      sizes.set(home, after);
      return {
        goBuildBytes: target.totalBytes - after,
        goBuildUnits: 1,
        npmBytes: 0,
        npmBuckets: 0,
        npmContents: 0,
        kept: 0,
        moduleCacheRemoved: false,
        outOfTime: false,
        estimatedBytesAfter: after,
      };
    },
    now: () => clock.now,
  };
  const gov = new DiskGovernor(options);
  return { gov, sizes, trims, stops, inPlace, inPlaceLeaves, reclaims: () => reclaims, clock, warns, warnFields, inPlaceHold };
}

/** Register a run with no executor yet (cloning / setup): no status sent, not in the executor. */
function watchSetup(h: Harness, runId: string, bytes: number, requestStop: () => void = () => h.stops.push(runId)): void {
  const home = `/data/agent-home/${runId}`;
  h.sizes.set(home, bytes);
  h.gov.register(runId, {
    home,
    requestStop,
    reclaimInPlace: async (): Promise<RunCacheDropResult | undefined> => {
      h.inPlace.push(runId);
      if (h.inPlaceHold.until) await h.inPlaceHold.until;
      if (h.inPlaceLeaves.throws) throw new Error("drop failed");
      if (h.inPlaceLeaves.bytes !== undefined) h.sizes.set(home, h.inPlaceLeaves.bytes);
      return { dropped: ["go/pkg/mod"], absent: [], failed: [], skipped: [] };
    },
  });
}

/** The runner's report choke point: a status is sent, then the server ACKs it. */
function report(h: Harness, runId: string, sent: string): void {
  h.gov.statusAcked(runId, h.gov.statusRequested(runId, sent), sent);
}

/** A run in `executor.run` whose status is `running` on both sides: the parkable shape. */
function watch(h: Harness, runId: string, bytes: number, requestStop?: () => void): void {
  watchSetup(h, runId, bytes, requestStop);
  h.gov.enterRun(runId);
  report(h, runId, "running");
}

const quiet = async (): Promise<boolean> => false;
const busy = async (): Promise<boolean> => true;
const settle = (): Promise<void> => new Promise((r) => setImmediate(r));

describe("cache cap math (PRD #1809 D4)", () => {
  it("the cap is the fraction of the volume divided by the max concurrent runs", () => {
    assert.strictEqual(runCacheCapBytes(VOLUME, 0.5, 2), 6.25 * GIB);
    assert.strictEqual(runCacheCapBytes(VOLUME, 0.5, 1), 12.5 * GIB);
    assert.strictEqual(runCacheCapBytes(undefined, 0.5, 2), undefined, "an unknown volume enforces no cap");
    assert.strictEqual(runCacheCapBytes(0, 0.5, 2), undefined);
  });

  it("the hard threshold is the api threshold minus the margin (0.87 by default)", () => {
    assert.ok(Math.abs(hardStopThreshold(undefined, 0.03) - 0.87) < 1e-9);
    assert.ok(Math.abs(hardStopThreshold(0.8, 0.03) - 0.77) < 1e-9);
    assert.strictEqual(hardStopThreshold(0.02, 0.03), 0.02, "a margin past the threshold falls back to it");
  });
});

describe("soft layer: the per-run cache cap at turn boundaries", () => {
  it("a run under the cap is neither trimmed nor parked", async () => {
    const h = harness();
    watch(h, "r1", CAP - 1);
    assert.strictEqual(await h.gov.boundary("r1", quiet), "continue");
    assert.strictEqual(h.trims.length, 0);
  });

  it("a cache over the cap is trimmed between turns with no live process, down to the low-water mark", async () => {
    const h = harness();
    watch(h, "r1", CAP + GIB);
    let probed = 0;
    assert.strictEqual(
      await h.gov.boundary("r1", async () => {
        probed++;
        return false;
      }),
      "continue",
    );
    assert.strictEqual(probed, 1, "the quiet-point probe is asked only because the run is over the cap");
    assert.deepStrictEqual(h.trims, [
      { home: "/data/agent-home/r1", target: { totalBytes: CAP + GIB, capBytes: CAP, lowWaterBytes: Math.floor(CAP * 0.6) } },
    ]);
  });

  it("a live background process blocks the trim", async () => {
    const h = harness();
    watch(h, "r1", CAP + 1);
    assert.strictEqual(await h.gov.boundary("r1", busy), "continue");
    assert.strictEqual(h.trims.length, 0, "never trimmed under a live process");
  });

  it("a probe that fails is not a quiet point", async () => {
    const h = harness();
    watch(h, "r1", CAP + 1);
    await h.gov.boundary("r1", async () => {
      throw new Error("probe failed");
    });
    assert.strictEqual(h.trims.length, 0);
  });

  it("a run that stays over the cap with a live process parks at its third boundary", async () => {
    const h = harness();
    watch(h, "r1", CAP + 1);
    assert.strictEqual(await h.gov.boundary("r1", busy), "continue");
    assert.strictEqual(await h.gov.boundary("r1", busy), "continue");
    assert.strictEqual(await h.gov.boundary("r1", busy), "park");
  });

  it("a boundary back under the cap resets the count", async () => {
    const h = harness();
    watch(h, "r1", CAP + 1);
    await h.gov.boundary("r1", busy);
    await h.gov.boundary("r1", busy);
    await h.gov.boundary("r1", quiet); // trimmed under the cap
    h.sizes.set("/data/agent-home/r1", CAP + 1);
    assert.strictEqual(await h.gov.boundary("r1", busy), "continue");
    assert.strictEqual(await h.gov.boundary("r1", busy), "continue");
  });

  it("runaway growth (1.25x the cap) with a live process parks at once", async () => {
    const h = harness();
    watch(h, "r1", Math.ceil(CAP * 1.25));
    assert.strictEqual(await h.gov.boundary("r1", busy), "park");
  });

  it("a trim that cannot get under the cap counts toward the park too", async () => {
    const h = harness({ trimTo: (t) => t.capBytes + 1 });
    watch(h, "r1", CAP * 2);
    assert.strictEqual(await h.gov.boundary("r1", quiet), "continue");
    assert.strictEqual(await h.gov.boundary("r1", quiet), "continue");
    assert.strictEqual(await h.gov.boundary("r1", quiet), "park");
    assert.strictEqual(h.trims.length, 3);
  });

  it("the soft layer can be disabled: nothing is measured, trimmed or parked", async () => {
    const h = harness({ config: { capEnabled: false } });
    watch(h, "r1", CAP * 3);
    for (let i = 0; i < 5; i++) assert.strictEqual(await h.gov.boundary("r1", busy), "continue");
    assert.strictEqual(h.trims.length, 0);
  });

  it("an unwatched run (a Codex or unfenced run) is never capped", async () => {
    const h = harness();
    assert.strictEqual(await h.gov.boundary("nobody", busy), "continue");
  });
});

describe("hard layer: the per-tick pressure stop", () => {
  it("at or over the hard threshold it stops the parkable run with the largest caches, once", async () => {
    const h = harness();
    watch(h, "small", GIB);
    watch(h, "large", 4 * GIB);
    await h.gov.boundary("small", quiet);
    await h.gov.boundary("large", quiet);
    h.sizes.set("/data/agent-home/large", 5 * GIB); // it grew mid-turn: the stop measures afresh
    h.gov.observe(0.86);
    await settle();
    assert.deepStrictEqual(h.stops, [], "under 0.87: nothing");
    h.gov.observe(0.87);
    await settle();
    assert.deepStrictEqual(h.stops, ["large"]);
    // Rate-limited: another over-threshold tick does not stop a second run while the first has
    // not parked.
    h.clock.now += 15_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.stops, ["large"]);
  });

  it("the next stop waits for the first run's park AND a sample taken after it", async () => {
    const h = harness();
    watch(h, "a", 3 * GIB);
    watch(h, "b", 2 * GIB);
    await h.gov.boundary("a", quiet);
    await h.gov.boundary("b", quiet);
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"]);
    const sampledBeforePark = h.clock.now;
    h.clock.now += 5_000;
    h.gov.unregister("a"); // its flight ended: parked, caches dropped
    assert.strictEqual(h.reclaims(), 1, "the D7 reclaim runs after the stopped run's park");
    h.gov.observe(0.9, sampledBeforePark);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"], "a sample from before the park is not fresh");
    h.clock.now += 15_000;
    h.gov.observe(0.8);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"], "a fresh sample back under: nothing more");
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a", "b"], "a fresh sample still over: the next largest is stopped");
  });

  it("a run whose caches hold nothing is not stopped for the volume", async () => {
    const h = harness();
    watch(h, "r1", 0);
    await h.gov.boundary("r1", quiet);
    h.gov.observe(0.99);
    await settle();
    assert.deepStrictEqual(h.stops, []);
  });

  it("the hard layer can be disabled", async () => {
    const h = harness({ config: { hardStopEnabled: false } });
    watch(h, "r1", 5 * GIB);
    await h.gov.boundary("r1", quiet);
    h.gov.observe(1);
    await settle();
    assert.deepStrictEqual(h.stops, []);
  });

  it("follows the api's threshold", async () => {
    const h = harness({ threshold: 0.7 });
    watch(h, "r1", 5 * GIB);
    await h.gov.boundary("r1", quiet);
    h.gov.observe(0.68);
    await settle();
    assert.deepStrictEqual(h.stops, ["r1"], "0.70 - 0.03 = 0.67");
  });

  it("an unknown sample never stops a run", async () => {
    const h = harness();
    watch(h, "r1", 5 * GIB);
    await h.gov.boundary("r1", quiet);
    h.gov.observe(undefined);
    await settle();
    assert.deepStrictEqual(h.stops, []);
  });

  it("with both layers off the governor is disabled (the runner then watches nothing)", () => {
    assert.strictEqual(harness({ config: { capEnabled: false, hardStopEnabled: false } }).gov.enabled, false);
    assert.strictEqual(harness({ config: { capEnabled: false } }).gov.enabled, true);
  });
});

describe("hard layer: a stop is never wedged, and never lost to the soft park (PRD #1809 D4 rework)", () => {
  it("a hard stop requested while the soft boundary measures resolves the boundary to continue, never a (preventive) park (N1)", async () => {
    const h = harness();
    let stopAsked: () => void = () => {};
    const asked = new Promise<void>((r) => (stopAsked = r));
    watch(h, "r1", Math.ceil(CAP * 1.5), () => {
      h.stops.push("r1");
      stopAsked();
    });
    await h.gov.boundary("r1", quiet); // trimmed to the low-water mark
    h.sizes.set("/data/agent-home/r1", Math.ceil(CAP * 1.5));
    const verdict = h.gov.boundary("r1", async () => {
      // The volume crosses the hard threshold while this boundary is between measure and trim.
      h.gov.observe(0.95);
      await asked;
      return true;
    });
    assert.strictEqual(await verdict, "continue", "the pending stop parks it, COUNTED");
    assert.deepStrictEqual(h.stops, ["r1"]);
  });

  it("a stopped run that leaves its executor without a disk park releases the stop (N4)", async () => {
    const h = harness();
    watch(h, "a", 3 * GIB);
    watch(h, "b", 2 * GIB);
    await h.gov.boundary("a", quiet);
    await h.gov.boundary("b", quiet);
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"]);
    h.clock.now += 1_000;
    h.gov.leftLoop("a", false); // it finished its last turn: finalizing, not parking
    h.clock.now += 1_000;
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a", "b"], "the hard layer is not wedged on a stop that will never park");
  });

  it("a run that left for its disk park keeps the stop until its flight ends", async () => {
    const h = harness();
    watch(h, "a", 3 * GIB);
    watch(h, "b", 2 * GIB);
    await h.gov.boundary("a", quiet);
    await h.gov.boundary("b", quiet);
    h.gov.observe(0.9);
    await settle();
    h.gov.leftLoop("a", true);
    h.clock.now += 120_000;
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"], "still parking: one stop at a time");
  });

  it("a stop that has not produced a park within 10 minutes is given up (BLOCKING 2)", async () => {
    const h = harness();
    watch(h, "a", 3 * GIB);
    watch(h, "b", 2 * GIB);
    await h.gov.boundary("a", quiet);
    await h.gov.boundary("b", quiet);
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"]);
    h.clock.now += 9 * 60_000;
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a"], "still inside the timeout");
    h.clock.now += 60_000;
    h.gov.observe(0.9); // gives the stop up; this sample predates the release
    await settle();
    h.clock.now += 1_000;
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["a", "b"], "the stuck run is not asked again; the next largest is stopped");
  });

  it("while over the threshold with nothing to stop, the candidates are measured at most once a minute (N5)", async () => {
    const h = harness();
    let measures = 0;
    watch(h, "empty", 0);
    await h.gov.boundary("empty", quiet);
    const gov = h.gov as unknown as { measure: (home: string, o: { deadline: number }) => Promise<unknown> };
    const inner = gov.measure;
    gov.measure = async (home, o) => {
      measures++;
      return await inner(home, o);
    };
    for (let i = 0; i < 6; i++) {
      h.gov.observe(0.95);
      await settle();
      h.clock.now += 5_000;
    }
    assert.strictEqual(measures, 1, "six ticks in 30 s: one selection");
    h.clock.now += 60_000;
    h.gov.observe(0.95);
    await settle();
    assert.strictEqual(measures, 2);
    assert.deepStrictEqual(h.stops, []);
  });
});

describe("issue #1830: the hard layer covers every phase of a run", () => {
  it("the heaviest cache on a PLANNING run (never at a boundary) is stopped once the hard threshold is crossed", async () => {
    const h = harness();
    watch(h, "planning", 5 * GIB); // in the executor, status running both sides; no cacheCapBoundary ever
    watch(h, "implement", GIB);
    await h.gov.boundary("implement", quiet);
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["planning"]);
    assert.deepStrictEqual(h.inPlace, []);
  });

  it("a run at its plan gate (ACKed awaiting_approval) is reclaimed in place, not stopped, and only again from a fresh sample", async () => {
    const h = harness();
    watch(h, "gated", 5 * GIB);
    report(h, "gated", "awaiting_approval");
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, []);
    assert.deepStrictEqual(h.inPlace, ["gated"]);
    // It regrew; a sample taken before the drop ended is not fresh.
    h.sizes.set("/data/agent-home/gated", 2 * GIB);
    h.gov.observe(0.9, h.clock.now - 1);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"], "a stale sample does nothing");
    h.clock.now += 1_000;
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated", "gated"]);
    assert.deepStrictEqual(h.stops, []);
    // The drop used its own slot: a later parkable stop is not blocked by it.
    watch(h, "impl", 9 * GIB);
    h.clock.now += 1_000;
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["impl"]);
  });

  it("a revision turn (in the executor, ACKed awaiting_approval) is reclaimed, not stopped", async () => {
    const h = harness();
    watch(h, "r", 3 * GIB);
    report(h, "r", "awaiting_approval");
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, []);
    assert.deepStrictEqual(h.inPlace, ["r"]);
  });

  it("a non-running report in flight and an unreadable first ACK are not parkable", async () => {
    const h = harness();
    watch(h, "inflight", 3 * GIB);
    h.gov.statusRequested("inflight", "awaiting_approval"); // sent, ACK not back; ACKed is still running
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["inflight"], "sent awaiting_approval, ACKed running for the older send");
    assert.deepStrictEqual(h.stops, []);

    const h2 = harness();
    watchSetup(h2, "unknown", 3 * GIB); // no earlier running ACK to lean on
    h2.gov.enterRun("unknown");
    h2.gov.statusAcked("unknown", h2.gov.statusRequested("unknown", "running"), undefined);
    h2.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h2.inPlace, ["unknown"]);
    assert.deepStrictEqual(h2.stops, []);
  });

  it("a late running ACK of a send older than the last non-running send never makes the run parkable", async () => {
    // running(1), awaiting_approval(2), running(3): the LAST send is running again, so only the
    // ACK ordering keeps this honest (the old latest-send-only guard is not what is pinned here).
    const h = harness();
    watchSetup(h, "stale", 3 * GIB);
    h.gov.enterRun("stale");
    const first = h.gov.statusRequested("stale", "running");
    h.gov.statusRequested("stale", "awaiting_approval");
    h.gov.statusRequested("stale", "running");
    h.gov.statusAcked("stale", first, "running"); // the late ACK of send 1
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["stale"], "reclaimed in place, not stopped");
    assert.deepStrictEqual(h.stops, []);
  });

  it("running(3) ACKed running after a non-running send is parkable", async () => {
    const h = harness();
    watchSetup(h, "back", 3 * GIB);
    h.gov.enterRun("back");
    const first = h.gov.statusRequested("back", "running");
    const second = h.gov.statusRequested("back", "awaiting_approval");
    const third = h.gov.statusRequested("back", "running");
    h.gov.statusAcked("back", first, "running");
    h.gov.statusAcked("back", second, "awaiting_approval");
    h.gov.statusAcked("back", third, "running");
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["back"]);
    assert.deepStrictEqual(h.inPlace, []);
  });

  it("a late declined ACK of an older send does not cancel a newer running ACK (lastNonRunningAckedSeq is monotone)", async () => {
    const h = harness();
    watchSetup(h, "ooo", 3 * GIB);
    h.gov.enterRun("ooo");
    h.gov.statusRequested("ooo", "running"); // 1
    const second = h.gov.statusRequested("ooo", "running");
    const third = h.gov.statusRequested("ooo", "running");
    h.gov.statusAcked("ooo", third, "running");
    h.gov.statusAcked("ooo", second, "awaiting_approval"); // late, older than the running ACK
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, ["ooo"]);
    assert.deepStrictEqual(h.inPlace, []);
  });

  it("a late declined ACK of an older send does not undo an earlier declined ACK's cancel", async () => {
    const h = harness();
    watchSetup(h, "ooo2", 3 * GIB);
    h.gov.enterRun("ooo2");
    const first = h.gov.statusRequested("ooo2", "running");
    const second = h.gov.statusRequested("ooo2", "running");
    const third = h.gov.statusRequested("ooo2", "running");
    h.gov.statusAcked("ooo2", second, "running");
    h.gov.statusAcked("ooo2", third, "awaiting_approval"); // cancels the running ACK of 2
    h.gov.statusAcked("ooo2", first, "awaiting_approval"); // late: must not lower the cancel mark
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.stops, []);
    assert.deepStrictEqual(h.inPlace, ["ooo2"]);
  });

  it("the logged acked_status is the newest send's, not a late ACK of an older one", async () => {
    const h = harness();
    watch(h, "log", 3 * GIB);
    h.gov.statusRequested("log", "awaiting_approval"); // 2
    const second = 2;
    const third = h.gov.statusRequested("log", "awaiting_input"); // 3
    h.gov.statusAcked("log", third, "awaiting_input");
    h.gov.statusAcked("log", second, "awaiting_approval"); // late ACK of the older send
    h.gov.observe(0.9);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["log"]);
    const drop = h.warnFields.find((w) => w.fields?.in_place === true);
    assert.ok(drop, "the in-place drop was logged");
    assert.strictEqual(drop.fields?.acked_status, "awaiting_input");
  });

  for (const declined of ["awaiting_approval", undefined] as const) {
    it(`a declined or unreadable ACK (${String(declined)}) of a LATER running send cancels the earlier running ACK`, async () => {
      const h = harness();
      watch(h, "later", 5 * GIB); // running sent (1) and ACKed running
      const second = h.gov.statusRequested("later", "running");
      h.gov.statusAcked("later", second, declined); // the server declined it (409) or the ACK was unreadable
      h.gov.observe(0.99);
      await settle();
      assert.deepStrictEqual(h.stops, [], "no stop: the server may not be at running");
      assert.deepStrictEqual(h.inPlace, ["later"], "dropped in place instead");
    });
  }

  it("a LATER running ACK (a higher seq) after a declined one restores parkable", async () => {
    const h = harness();
    watch(h, "restored", 5 * GIB);
    const second = h.gov.statusRequested("restored", "running");
    h.gov.statusAcked("restored", second, undefined);
    const third = h.gov.statusRequested("restored", "running");
    h.gov.statusAcked("restored", third, "running");
    h.gov.observe(0.99);
    await settle();
    assert.deepStrictEqual(h.stops, ["restored"]);
    assert.deepStrictEqual(h.inPlace, []);
  });

  it("a same-status running report in flight (un-ACKed) does not unmake a parkable run", async () => {
    const h = harness();
    watch(h, "impl", 5 * GIB); // running sent and ACKed
    h.gov.statusRequested("impl", "running"); // the next running report is sent, ACK not back yet
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, [], "no in-place drop under the live turn");
    assert.deepStrictEqual(h.stops, ["impl"]);
  });

  it("a run skipped in place while its first running report was un-ACKed is stopped once that ACK lands", async () => {
    const h = harness();
    h.inPlaceLeaves.bytes = 4 * GIB; // the drop leaves bytes, so the run is skipped for the stretch
    watchSetup(h, "impl", 5 * GIB);
    h.gov.enterRun("impl");
    const seq = h.gov.statusRequested("impl", "running"); // sent, no running ACK yet
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["impl"]);
    assert.deepStrictEqual(h.stops, []);
    h.gov.statusAcked("impl", seq, "running");
    h.clock.now += 1_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.stops, ["impl"], "the skip did not outlive the run becoming parkable");
  });

  it("a setup run and a finalizing run are reclaimed in place, never stopped", async () => {
    const h = harness();
    watchSetup(h, "setup", 3 * GIB);
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["setup"]);

    const f = harness();
    watch(f, "fin", 3 * GIB);
    f.gov.leftLoop("fin", false); // executor returned: finalizing
    f.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(f.inPlace, ["fin"]);
    assert.deepStrictEqual(f.stops, []);
  });

  it("the action follows the run's state AFTER the measurement", async () => {
    const h = harness();
    watch(h, "a", 3 * GIB);
    const gov = h.gov as unknown as { measure: (home: string, o: { deadline: number }) => Promise<unknown> };
    const inner = gov.measure;
    gov.measure = async (home, o) => {
      const r = await inner(home, o);
      report(h, "a", "awaiting_input"); // it reached a question wait while being measured
      return r;
    };
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.stops, []);
    assert.deepStrictEqual(h.inPlace, ["a"]);
  });

  it("an in-place drop that throws is logged and releases the slot", async () => {
    const h = harness();
    h.inPlaceLeaves.throws = true;
    watch(h, "gated", 3 * GIB);
    report(h, "gated", "awaiting_approval");
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"]);
    // The slot is free: the parkable run is acted on at the next fresh sample.
    watch(h, "impl", GIB);
    h.clock.now += 1_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.stops, ["impl"], "the failed run is skipped for the stretch; the smaller parkable run is stopped");
  });

  it("a non-parkable run whose drop left bytes behind is skipped for the stretch so the next-heaviest run is stopped", async () => {
    const h = harness();
    h.inPlaceLeaves.bytes = 4 * GIB; // the drop leaves bytes (e.g. a kept subtree)
    watch(h, "gated", 5 * GIB);
    report(h, "gated", "awaiting_approval");
    watch(h, "impl", 2 * GIB);
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"]);
    assert.deepStrictEqual(h.stops, []);
    h.clock.now += 1_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"], "not reclaimed again this stretch");
    assert.deepStrictEqual(h.stops, ["impl"]);
  });

  it("a PENDING in-place drop holds its own slot: leftLoop and unregister neither release a stop nor fire the D7 reclaim, and the give-up timeout skips it", async () => {
    const h = harness();
    let release: () => void = () => {};
    h.inPlaceHold.until = new Promise<void>((r) => (release = r));
    watch(h, "gated", 3 * GIB);
    report(h, "gated", "awaiting_approval");
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"], "the drop started and is still pending");
    h.gov.leftLoop("gated", false);
    assert.ok(!h.warns.some((m) => m.includes("without a disk park")), "no stop is released for a drop");
    h.clock.now += 11 * 60_000;
    h.gov.observe(0.95);
    assert.ok(!h.warns.some((m) => m.includes("giving it up")), "the give-up timeout does not touch a drop");
    h.gov.unregister("gated");
    await settle();
    assert.strictEqual(h.reclaims(), 0, "no D7 reclaim for a run that was only reclaimed in place");
    release();
    await settle();
  });

  it("the in-place skip resets when usage falls under the threshold", async () => {
    const h = harness();
    h.inPlaceLeaves.bytes = 4 * GIB;
    watch(h, "gated", 5 * GIB);
    report(h, "gated", "awaiting_approval");
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"]);
    h.clock.now += 1_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"], "skipped for the rest of the stretch");
    h.gov.observe(0.5); // the volume recovered: the stretch ends
    h.clock.now += 1_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated", "gated"], "a new stretch acts on it again");
  });

  it("the in-place skip resets when the run's status changes", async () => {
    const h = harness();
    h.inPlaceLeaves.bytes = 4 * GIB;
    watch(h, "gated", 5 * GIB);
    report(h, "gated", "awaiting_approval");
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"]);
    report(h, "gated", "awaiting_input"); // a different wait
    h.clock.now += 1_000;
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated", "gated"]);
  });

  it("a heavier gate-waiting run next to a lighter parkable run is dropped in place and nothing is stopped in that selection", async () => {
    const h = harness();
    watch(h, "gated", 6 * GIB);
    report(h, "gated", "awaiting_approval");
    watch(h, "impl", 2 * GIB);
    h.gov.observe(0.95);
    await settle();
    assert.deepStrictEqual(h.inPlace, ["gated"]);
    assert.deepStrictEqual(h.stops, [], "the lighter parkable run is not stopped in the same selection");
  });

  it("an in-place drop does not trigger the D7 reclaim and leaves leftLoop/unregister untouched", async () => {
    const h = harness();
    watch(h, "gated", 3 * GIB);
    report(h, "gated", "awaiting_approval");
    h.gov.observe(0.95);
    await settle();
    h.gov.unregister("gated");
    assert.strictEqual(h.reclaims(), 0, "only a stopped run's flight end runs the D7 reclaim");
  });
});
