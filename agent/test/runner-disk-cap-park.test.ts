import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { WorkerClient } from "../src/client.js";
import type { StateAck, StateRequest } from "../src/protocol.js";
import { type CacheCapConfig, DiskGovernor, DiskParkSignal } from "../src/cache-cap.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";
import { TOKEN, api, baseUrl, client, fakeGitlab, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// PRD #1809 D4 through the runner: the cache cap's preventive park and the hard pressure stop
// both end the executor and take the MID-RUN data_volume_full recovery park (a clone exists, so
// the custody hold is kept), and the park's cache drop (M1) empties the preserved HOME. Seams
// only: the volume size and the cache sizes are injected, never a real full disk.

const FEATURE = "recovery_cause_data_volume_full";
const FENCE = "claim_generation_fence";
const GIB = 1024 ** 3;
const VOLUME = 10 * GIB;
/** 0.5 x 10 GiB / 1 run. */
const CAP = 5 * GIB;
const CACHES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"];
const HAS_PROC_FD = fs.existsSync("/proc/self/fd");

const CONFIG: CacheCapConfig = {
  capEnabled: true,
  capFraction: 0.5,
  lowWater: 0.6,
  maxConcurrentRuns: 1,
  hardStopEnabled: true,
  hardMargin: 0.03,
};

function seedHome(home: string): void {
  fs.mkdirSync(path.join(home, ".cache", "go-build", "0a"), { recursive: true });
  fs.writeFileSync(path.join(home, ".cache", "go-build", "0a", "0a1b-d"), "x".repeat(4096));
  fs.mkdirSync(path.join(home, "go", "pkg", "mod", "example.com"), { recursive: true });
  fs.mkdirSync(path.join(home, "go", "bin"), { recursive: true });
  fs.writeFileSync(path.join(home, "go", "bin", "gopls"), "bin");
  fs.mkdirSync(path.join(home, ".npm", "_cacache", "index-v5"), { recursive: true });
  fs.mkdirSync(path.join(home, ".claude", "projects", "p"), { recursive: true });
  fs.writeFileSync(path.join(home, ".claude", "projects", "p", "sess.jsonl"), "transcript");
}

interface GovernorProbe {
  gov: DiskGovernor;
  /** Cache bytes the fake measurement reports, per HOME. */
  sizes: Map<string, number>;
  reclaims: () => number;
  trims: () => number;
}

function governor(config: Partial<CacheCapConfig> = {}): GovernorProbe {
  const sizes = new Map<string, number>();
  let reclaims = 0;
  let trims = 0;
  const gov = new DiskGovernor({
    config: { ...CONFIG, ...config },
    log: nullLogger(),
    volumeTotalBytes: () => VOLUME,
    thresholdOf: () => undefined,
    reclaim: async () => {
      reclaims++;
    },
    measure: async (home) => ({ cacheBytes: sizes.get(home) ?? 0, entries: 1, truncated: false }),
    trim: async (_home, target) => {
      trims++;
      return {
        goBuildBytes: 0,
        goBuildUnits: 0,
        npmBytes: 0,
        npmBuckets: 0,
        npmContents: 0,
        kept: 0,
        moduleCacheRemoved: false,
        outOfTime: false,
        estimatedBytesAfter: target.totalBytes,
      };
    },
  });
  return { gov, sizes, reclaims: () => reclaims, trims: () => trims };
}

async function withHomeRoot(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-m4-"));
  try {
    await fn(homeRoot);
  } finally {
    fs.rmSync(homeRoot, { recursive: true, force: true });
  }
}

const parks = (runId: string): StateRequest[] =>
  api.states.filter((s) => s.runId === runId && s.body.status === "recovery_wait").map((s) => s.body);

/** An executor that keeps building between turns while a background process of the run stays
 *  alive: its caches stay over the cap at every boundary until the cap parks it. */
function staysOverFactory(homeRoot: string, probe: GovernorProbe): { factory: ExecutorFactory; home: () => string; boundaries: () => number } {
  let home = "";
  let boundaries = 0;
  const factory: ExecutorFactory = (id) => {
    home = path.join(homeRoot, id);
    probe.sizes.set(home, CAP + GIB);
    return {
      homeDir: home,
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          seedHome(home);
          for (let turn = 0; turn < 10; turn++) {
            boundaries++;
            if ((await ctx.cacheCapBoundary?.(async () => true)) === "park") throw new DiskParkSignal(true);
          }
          throw new Error("the cap never parked the run");
        },
      },
    };
  };
  return { factory, home: () => home, boundaries: () => boundaries };
}

describe("RunRunner — PRD #1809 D4 soft layer: a run that stays over the cap parks", () => {
  it("parks PREVENTIVELY (recovery_wait, data_volume_full, disk_park_preventive: true) and its caches are dropped", async (t) => {
    if (!HAS_PROC_FD) return t.skip("no /proc/self/fd on this host: the cache drop refuses here by design");
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const probe = governor();
      const { factory, home, boundaries } = staysOverFactory(homeRoot, probe);
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d401";
      await runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov }).execute(
        gitlabClaim(1809, { run_id: runId, claim_generation: 3 }),
      );
      assert.strictEqual(boundaries(), 3, "parked at the third boundary over the cap with a live process");
      assert.strictEqual(probe.trims(), 0, "a live background process blocked every trim");
      const [park, ...more] = parks(runId);
      assert.deepStrictEqual(more, []);
      assert.strictEqual(park?.recovery_cause, "data_volume_full");
      assert.strictEqual(park?.disk_park_preventive, true, "the soft park is uncounted");
      assert.strictEqual(park?.claim_generation, 3, "fenced on the claim generation");
      assert.ok(!api.states.some((s) => s.runId === runId && s.body.status === "failed"), "never a failure");
      for (const rel of CACHES) assert.strictEqual(fs.existsSync(path.join(home(), rel)), false, `${rel} dropped on the park`);
      assert.ok(fs.existsSync(path.join(home(), "go", "bin", "gopls")), "go/bin survives");
      assert.ok(fs.existsSync(path.join(home(), ".claude", "projects", "p", "sess.jsonl")), "the session survives for the resume");
      assert.ok(
        api.messages(runId).some((m) => m.kind === "status" && String(m.payload.text).includes("size cap")),
        "the feed says why the run paused",
      );
    });
  });

  it("repeated soft parks never produce a failure, even past the api's counted-park cap", async () => {
    await withHomeRoot(async (homeRoot) => {
      // A fake api with the real cap rule: past 3 COUNTED parks a disk park fails the run; a
      // preventive park is never counted.
      class CappedApiClient extends WorkerClient {
        counted = 0;
        readonly acks: string[] = [];
        constructor() {
          super(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
          this.protocolFeatures = [FEATURE, FENCE];
        }
        override async reportState(runId: string, body: StateRequest): Promise<StateAck> {
          if (body.status !== "recovery_wait") return await super.reportState(runId, body);
          if (body.recovery_cause === "data_volume_full" && body.disk_park_preventive !== true) this.counted++;
          const status = this.counted > 3 ? "failed" : "recovery_wait";
          this.acks.push(status);
          return { applied: true, status };
        }
      }
      const capped = new CappedApiClient();
      const probe = governor();
      const { gitlab } = fakeGitlab();
      for (let leg = 0; leg < 5; leg++) {
        const { factory } = staysOverFactory(homeRoot, probe);
        const runner = new RunRunner(capped, git, factory, nullLogger(), 20, undefined, {
          pollMs: 5,
          planApprovalTimeoutMs: 0,
          gitlab,
          recoveryRetryMs: 5,
          diskGovernor: probe.gov,
        });
        await runner.execute(gitlabClaim(1810 + leg, { run_id: `18090000-0000-4000-8000-00000000d41${leg}`, claim_generation: 2 }));
      }
      assert.deepStrictEqual(capped.acks, ["recovery_wait", "recovery_wait", "recovery_wait", "recovery_wait", "recovery_wait"]);
      assert.strictEqual(capped.counted, 0);
    });
  });

  it("an api without the typed cause gets the untyped recovery_wait park", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FENCE];
      const probe = governor();
      const { factory } = staysOverFactory(homeRoot, probe);
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d402";
      await runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov }).execute(
        gitlabClaim(1811, { run_id: runId, claim_generation: 3 }),
      );
      const [park] = parks(runId);
      assert.ok(park, "parked");
      assert.strictEqual(park.recovery_cause, undefined);
      assert.strictEqual(park.disk_park_preventive, undefined);
    });
  });
});

describe("RunRunner — PRD #1809 D4 hard layer: the mid-turn pressure stop", () => {
  it("a volume crossing the hard threshold mid-turn stops the largest run, reaps its tree and parks it COUNTED with the data_volume_full cause", async (t) => {
    if (!HAS_PROC_FD) return t.skip("no /proc/self/fd on this host: the cache drop refuses here by design");
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const probe = governor();
      // Another running Claude run on the worker, with smaller caches: it must not be the one stopped.
      let otherStopped = 0;
      probe.sizes.set("/other/home", GIB);
      probe.gov.register("other-run", { home: "/other/home", requestStop: () => otherStopped++ });
      await probe.gov.boundary("other-run", async () => false);

      let home = "";
      let reaped = 0;
      let inTurn: () => void = () => {};
      const turnStarted = new Promise<void>((r) => (inTurn = r));
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        probe.sizes.set(home, 3 * GIB);
        return {
          homeDir: home,
          executor: {
            killAgentTree: () => {
              reaped++;
            },
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              // The boundary before the turn: under the cap, and the run becomes stoppable.
              assert.strictEqual(await ctx.cacheCapBoundary?.(async () => false), "continue");
              // A long build mid-turn, until the turn is dropped.
              await new Promise<void>((resolve) => {
                if (ctx.signal?.aborted) return resolve();
                ctx.signal?.addEventListener("abort", () => resolve(), { once: true });
                inTurn();
              });
              // What the SdkExecutor's turn catch does with a `disk` stop.
              if (ctx.pauseModeRequested?.() === "disk") throw new DiskParkSignal(false);
              throw new Error("the turn was dropped for another reason");
            },
          },
        };
      };
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d403";
      const done = runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov }).execute(
        gitlabClaim(1812, { run_id: runId, claim_generation: 4 }),
      );
      await turnStarted;
      probe.gov.observe(0.86);
      probe.gov.observe(0.9); // the stats tick: over 0.90 - 0.03
      await done;

      assert.strictEqual(otherStopped, 0, "the smaller run keeps running");
      assert.ok(reaped >= 1, "the stopped run's process tree was reaped");
      const [park, ...more] = parks(runId);
      assert.deepStrictEqual(more, []);
      assert.strictEqual(park?.recovery_cause, "data_volume_full");
      assert.strictEqual(park?.claim_generation, 4);
      assert.strictEqual("disk_park_preventive" in (park ?? {}), false, "the hard stop is a COUNTED park: the flag is absent");
      assert.ok(!api.states.some((s) => s.runId === runId && (s.body.status === "paused" || s.body.status === "failed")), "not an owner pause, not a failure");
      for (const rel of CACHES) assert.strictEqual(fs.existsSync(path.join(home, rel)), false, `${rel} dropped on the park`);
      assert.strictEqual(probe.reclaims(), 1, "the D7 reclaim ran after the park");
    });
  });
});

describe("RunRunner — PRD #1809 D4 both layers disabled", () => {
  it("a runner whose governor has both layers off offers no cap and cannot be stopped", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const probe = governor({ capEnabled: false, hardStopEnabled: false });
      let offered: unknown = "unset";
      const factory: ExecutorFactory = (id) => {
        const home = path.join(homeRoot, id);
        probe.sizes.set(home, 50 * GIB);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              offered = ctx.cacheCapBoundary;
              probe.gov.observe(1);
              await new Promise((r) => setTimeout(r, 20));
              // End the flight without finalizing anything (the generic failed path).
              throw new Error("end of the test run");
            },
          },
        };
      };
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d404";
      await runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov }).execute(
        gitlabClaim(1813, { run_id: runId, claim_generation: 3 }),
      );
      assert.strictEqual(offered, undefined, "no cap boundary is offered to the executor");
      assert.deepStrictEqual(parks(runId), [], "and nothing parked the run");
      assert.ok(!api.states.some((s) => s.runId === runId && s.body.recovery_cause !== undefined));
    });
  });
});

/** Count the runner's custody settles, and snapshot the HOME's caches when the capture runs. */
function instrument(runner: RunRunner, home: () => string): { settles: () => number; cachesAtCapture: () => boolean[] | undefined } {
  const r = runner as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  let settles = 0;
  let atCapture: boolean[] | undefined;
  const settle = r.reapThenSettleRecoveryGeneration!.bind(runner);
  r.reapThenSettleRecoveryGeneration = async (...a) => {
    settles++;
    return await settle(...a);
  };
  const capture = r.captureRecoveryRestorePoint!.bind(runner);
  r.captureRecoveryRestorePoint = async (...a) => {
    atCapture ??= CACHES.map((rel) => fs.existsSync(path.join(home(), rel)));
    return await capture(...a);
  };
  return { settles: () => settles, cachesAtCapture: () => atCapture };
}

describe("RunRunner — PRD #1809 D4 the mid-run disk park's reap, cache drop and custody", () => {
  it("reaps the attributed processes right after the group reap, drops the caches BEFORE the capture (N3), and keeps custody (N2)", async (t) => {
    if (!HAS_PROC_FD) return t.skip("no descriptor-pinned walk on this host: the cache drop refuses here by design");
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const probe = governor();
      const order: string[] = [];
      let home = "";
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        probe.sizes.set(home, CAP + GIB);
        return {
          homeDir: home,
          executor: {
            killAgentTree: () => {
              order.push("group-reap");
            },
            reapAttributedProcesses: async () => {
              order.push("attributed-reap");
            },
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              for (let turn = 0; turn < 10; turn++) {
                if ((await ctx.cacheCapBoundary?.(async () => true)) === "park") throw new DiskParkSignal(true);
              }
              throw new Error("the cap never parked the run");
            },
          },
        };
      };
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d405";
      const runner = runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov });
      const spy = instrument(runner, () => home);
      await runner.execute(gitlabClaim(1814, { run_id: runId, claim_generation: 3 }));
      assert.strictEqual(parks(runId).length, 1, "parked");
      const first = order.indexOf("attributed-reap");
      assert.ok(first > 0 && order[first - 1] === "group-reap", `the attributed reap follows the group reap: ${JSON.stringify(order)}`);
      assert.deepStrictEqual(spy.cachesAtCapture(), [false, false, false], "the caches were gone before the capture ran");
      assert.strictEqual(spy.settles(), 0, "the disk park keeps the custody hold: no settle");
    });
  });

  it("control: a transient recovery park DOES settle, so the custody spy above is live", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const factory: ExecutorFactory = (id) => ({
        homeDir: path.join(homeRoot, id),
        executor: {
          run: async (): Promise<ExecutorResult> => {
            throw new TransientRecoveryError();
          },
        },
      });
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d406";
      const runner = runnerWith(factory, gitlab, undefined, nullLogger());
      const spy = instrument(runner, () => homeRoot);
      await runner.execute(gitlabClaim(1815, { run_id: runId, claim_generation: 3 }));
      assert.strictEqual(parks(runId).length, 1, "parked");
      assert.ok(spy.settles() >= 1, "a transient park settles its generation");
    });
  });

  it("a soft park that races a hard stop is COUNTED (N1)", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const probe = governor();
      const factory: ExecutorFactory = (id) => {
        const home = path.join(homeRoot, id);
        probe.sizes.set(home, 3 * GIB);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              assert.strictEqual(await ctx.cacheCapBoundary?.(async () => false), "continue");
              // The volume crosses the hard threshold; the stop lands on this (now stoppable) run.
              const stopped = new Promise<void>((resolve) => ctx.signal?.addEventListener("abort", () => resolve(), { once: true }));
              probe.gov.observe(0.95);
              await stopped;
              // ...but this executor's soft layer had concluded "park" in the same window.
              throw new DiskParkSignal(true);
            },
          },
        };
      };
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d407";
      await runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov }).execute(
        gitlabClaim(1816, { run_id: runId, claim_generation: 3 }),
      );
      const [park] = parks(runId);
      assert.strictEqual(park?.recovery_cause, "data_volume_full");
      assert.strictEqual("disk_park_preventive" in (park ?? {}), false, "the stop makes it a COUNTED park");
    });
  });

  it("a stopped run that ends another way releases the hard layer for the next run before its flight ends (N4)", async () => {
    await withHomeRoot(async (homeRoot) => {
      client.protocolFeatures = [FEATURE, FENCE];
      const probe = governor();
      let otherStopped = 0;
      probe.sizes.set("/other/home", GIB);
      probe.gov.register("other-run", { home: "/other/home", requestStop: () => otherStopped++ });
      await probe.gov.boundary("other-run", async () => false);
      let stoppedWhileFinalizing = -1;
      const factory: ExecutorFactory = (id) => {
        const home = path.join(homeRoot, id);
        probe.sizes.set(home, 3 * GIB);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              await ctx.cacheCapBoundary?.(async () => false);
              probe.gov.observe(0.95); // stops THIS run (the largest)
              await new Promise((r) => setTimeout(r, 10));
              // It leaves its loop another way (here a failure), and a fresh sample still over the
              // threshold arrives while the flight's catch reports it, before the flight ends.
              setTimeout(() => {
                probe.gov.observe(0.95);
                setTimeout(() => (stoppedWhileFinalizing = otherStopped), 5);
              }, 0);
              throw new Error("the run ended another way");
            },
          },
        };
      };
      const { gitlab } = fakeGitlab();
      const runId = "18090000-0000-4000-8000-00000000d408";
      await runnerWith(factory, gitlab, undefined, nullLogger(), { diskGovernor: probe.gov }).execute(
        gitlabClaim(1817, { run_id: runId, claim_generation: 3 }),
      );
      assert.strictEqual(stoppedWhileFinalizing, 1, "the other run was stopped while the first was still finalizing");
    });
  });
});
