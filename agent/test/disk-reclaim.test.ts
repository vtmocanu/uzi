import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";

import {
  CachesDroppedMemo,
  DEFAULT_DISK_PRESSURE_THRESHOLD,
  DiskPressureController,
  modelPassMinAgeMs,
  runDiskReclaimPass,
  type DiskReclaimDeps,
} from "../src/disk-reclaim.js";
import { dropRunCaches, type DropRunCachesOptions } from "../src/run-caches.js";
import { noProcFd, RACED_FILES, seedRacedTree, startSwapRacer } from "./swap-racer.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { runReadOnlyModelPass } from "../src/model-pass.js";
import { restoreTreeWritability } from "../src/rmtree.js";
import type { WorkerClient } from "../src/client.js";
import type { GitCache } from "../src/git.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { makeClaim, nullLogger, recordingLogger } from "./helpers.js";

// PRD #1809 M3 (D5, D7): the running disk reclaim, its per-run lock and the soft-threshold
// controller. The reclaim tests drive the real pass against a real data-dir fixture (the real
// rmTreePinned and dropRunCaches, a read-only `0555` Go module cache in every HOME), with the
// api status lookup and the runner's live-run check injected.

const SKIP_ROOT = process.getuid?.() === 0 ? "running as uid 0 — the 0555 part of the fixture is inert for root" : false;

const roots: string[] = [];
after(async () => {
  for (const r of roots) {
    await restoreTreeWritability(r);
    fs.rmSync(r, { recursive: true, force: true });
  }
});

function dataDir(): { home: string; provision: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-disk-reclaim-"));
  roots.push(root);
  const home = path.join(root, "agent-home");
  const provision = path.join(root, "provision");
  fs.mkdirSync(home);
  fs.mkdirSync(provision);
  return { home, provision };
}

const CACHES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"];
const RESUME = [path.join(".claude", "projects", "-data-runner-clone", "sess-1.jsonl"), ".claude.json", "unknown.txt"];

/** A run HOME shaped like a Go + npm run's: the three caches (with a read-only module cache)
 *  beside the files a resume needs. */
function seedHome(home: string): void {
  const mod = path.join(home, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
  fs.mkdirSync(mod, { recursive: true });
  fs.writeFileSync(path.join(mod, "dec.go"), "package inf\n");
  fs.mkdirSync(path.join(home, ".cache", "go-build", "0a"), { recursive: true });
  fs.writeFileSync(path.join(home, ".cache", "go-build", "0a", "obj"), "x".repeat(4096));
  fs.mkdirSync(path.join(home, ".npm", "_cacache", "index-v5"), { recursive: true });
  fs.writeFileSync(path.join(home, ".npm", "_cacache", "index-v5", "entry"), "{}");
  for (const rel of RESUME) {
    fs.mkdirSync(path.dirname(path.join(home, rel)), { recursive: true });
    fs.writeFileSync(path.join(home, rel), "keep\n");
  }
  fs.chmodSync(mod, 0o555);
}

/** A provision dir a finished install leaves under the uid split: a read-only subtree. */
function seedProvision(dir: string): void {
  const ro = path.join(dir, ".devbox", "nix", "profile");
  fs.mkdirSync(ro, { recursive: true });
  fs.writeFileSync(path.join(ro, "manifest.json"), "{}");
  fs.chmodSync(ro, 0o555);
}

/** A run with a HOME and a provision dir, both seeded. */
function seedRun(d: { home: string; provision: string }): { id: string; home: string; provision: string } {
  const id = randomUUID();
  const home = path.join(d.home, id);
  const provision = path.join(d.provision, id);
  seedHome(home);
  seedProvision(provision);
  return { id, home, provision };
}

const exists = (p: string) => fs.existsSync(p);

function assertUntouched(run: { home: string; provision: string }, what: string): void {
  for (const rel of [...CACHES, ...RESUME]) assert.ok(exists(path.join(run.home, rel)), `${what}: ${rel} must be kept`);
  assert.ok(exists(run.provision), `${what}: its provision dir must be kept`);
}

function deps(d: { home: string; provision: string }, over: Partial<DiskReclaimDeps>): DiskReclaimDeps {
  return {
    homeRoot: d.home,
    provisionRoot: d.provision,
    statusOf: async () => undefined,
    isRunLive: () => false,
    locks: new RunDiskLocks(),
    log: nullLogger(),
    ...over,
  };
}

describe("runDiskReclaimPass (PRD #1809 D7)", () => {
  it("frees a process-ended park's caches and a terminal run's leftovers, keeping resume artifacts", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const parked = seedRun(d);
    const terminal = seedRun(d);
    const status = new Map([
      [parked.id, "limit_wait"],
      [terminal.id, "completed"],
    ]);

    const s = await runDiskReclaimPass(deps(d, { statusOf: async (id) => status.get(id) }));

    // The parked run: caches gone, every resume artifact and its provision dir kept.
    for (const rel of CACHES) assert.equal(exists(path.join(parked.home, rel)), false, `${rel} dropped`);
    for (const rel of RESUME) assert.ok(exists(path.join(parked.home, rel)), `${rel} kept for the resume`);
    // The terminal run: HOME and the read-only provision dir both removed.
    assert.equal(exists(terminal.home), false, "terminal HOME removed");
    assert.equal(exists(terminal.provision), false, "terminal provision dir removed despite its 0555 subtree");
    assert.equal(s.cachesDropped, 1);
    assert.equal(s.terminalHomesRemoved, 1);
    assert.equal(s.provisionDirsRemoved, 1);
    assert.equal(s.failed, 0);
  });

  it("removes a terminal run's provision dir that has no HOME beside it", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const id = randomUUID();
    seedProvision(path.join(d.provision, id));
    const s = await runDiskReclaimPass(deps(d, { statusOf: async () => "failed" }));
    assert.equal(exists(path.join(d.provision, id)), false);
    assert.equal(s.provisionDirsRemoved, 1);
  });

  it("drops caches for every process-ended park status (limit_wait, recovery_wait, paused)", { skip: SKIP_ROOT }, async () => {
    for (const st of ["limit_wait", "recovery_wait", "paused"]) {
      const d = dataDir();
      const run = seedRun(d);
      await runDiskReclaimPass(deps(d, { statusOf: async () => st }));
      for (const rel of CACHES) assert.equal(exists(path.join(run.home, rel)), false, `${st}: ${rel} dropped`);
      for (const rel of RESUME) assert.ok(exists(path.join(run.home, rel)), `${st}: ${rel} kept`);
      assert.ok(exists(run.provision), `${st}: a parked run keeps its provision dir`);
    }
  });

  it("never touches a live run, whatever the api says, and never asks for its status", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const liveTerminal = seedRun(d); // e.g. failed server-side by the stale-worker sweep while still running here
    const liveParked = seedRun(d);
    const live = new Set([liveTerminal.id, liveParked.id]);
    const asked: string[] = [];
    const status = new Map([
      [liveTerminal.id, "failed"],
      [liveParked.id, "limit_wait"],
    ]);
    const s = await runDiskReclaimPass(
      deps(d, {
        isRunLive: (id) => live.has(id),
        statusOf: async (id) => {
          asked.push(id);
          return status.get(id);
        },
      }),
    );
    assertUntouched(liveTerminal, "live run read as failed");
    assertUntouched(liveParked, "live run read as parked");
    assert.deepEqual(asked, [], "a live run's status is never looked up");
    assert.equal(s.skippedLive, 2);
  });

  it("re-checks liveness under the lock: a claim that started after the status read is left alone", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const run = seedRun(d);
    let live = false;
    const s = await runDiskReclaimPass(
      deps(d, {
        isRunLive: () => live,
        // The run is claimed on this worker between the status read and the deletion.
        statusOf: async () => {
          live = true;
          return "completed";
        },
      }),
    );
    assertUntouched(run, "run claimed mid-pass");
    assert.equal(s.skippedLive, 1);
  });

  it("never touches a gate-parked run, a running one, or a queued one", { skip: SKIP_ROOT }, async () => {
    const statuses = ["awaiting_approval", "awaiting_input", "awaiting_followup", "running", "claimed", "queued"];
    const d = dataDir();
    const runs = statuses.map(() => seedRun(d));
    const byId = new Map(runs.map((r, i) => [r.id, statuses[i]]));
    const s = await runDiskReclaimPass(deps(d, { statusOf: async (id) => byId.get(id) }));
    runs.forEach((r, i) => assertUntouched(r, statuses[i] as string));
    assert.equal(s.skippedNotEligible, statuses.length);
  });

  it("skips on every kind of not-knowing and bails on a streak of api failures", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const notFound = seedRun(d);
    const s404 = await runDiskReclaimPass(deps(d, { statusOf: async () => undefined }));
    assertUntouched(notFound, "404");
    assert.equal(s404.skippedStatusUnknown, 1);

    for (let i = 0; i < 4; i++) seedRun(d);
    let calls = 0;
    const down = await runDiskReclaimPass(
      deps(d, {
        statusOf: async () => {
          calls++;
          throw new Error("api down");
        },
      }),
    );
    assert.equal(down.stoppedEarly, "api_unreachable");
    assert.equal(calls, 3, "stops after three consecutive could-not-asks");
    assert.equal(down.unexamined, 2);
  });

  it("remembers a parked run whose caches are gone and skips it until it is seen executing again", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const run = seedRun(d);
    const memo = new CachesDroppedMemo();
    let live = false;
    let status = "limit_wait";
    const calls: Array<{ home: string; opts: DropRunCachesOptions }> = [];
    const { logger, lines } = recordingLogger();
    const base = deps(d, {
      cachesDropped: memo,
      isRunLive: () => live,
      statusOf: async () => status,
      log: logger,
      dropCaches: async (home, log, opts) => {
        calls.push({ home, opts });
        return dropRunCaches(home, log, opts);
      },
    });

    const first = await runDiskReclaimPass(base);
    assert.equal(first.cachesDropped, 1);
    assert.equal(calls.length, 1);
    assert.equal(calls[0]?.opts.message, "disk reclaim dropped a parked run's caches", "the reclaim's own log line");
    assert.ok((calls[0]?.opts.deadlineMs ?? Infinity) <= 60_000, "the reclaim-side drop is capped at 60 s");
    assert.ok(lines.some((l) => (l as { msg: string }).msg === "disk reclaim dropped a parked run's caches"));
    assert.ok(!lines.some((l) => (l as { msg: string }).msg === "run caches dropped on park"), "not the park's line");
    assert.ok(memo.has(run.id));

    const second = await runDiskReclaimPass(base);
    assert.equal(calls.length, 1, "a remembered run is not dropped again");
    assert.equal(second.cachesDropped, 0, "a skipped run is not counted as a drop");
    assert.equal(second.cachesAlreadyClear, 1);

    live = true; // resumed here: seen executing
    await runDiskReclaimPass(base);
    assert.equal(memo.has(run.id), false, "seeing the run executing forgets the drop");
    live = false;
    seedHome(run.home); // the resumed run refilled its caches, then parked again
    const third = await runDiskReclaimPass(base);
    assert.equal(calls.length, 2, "dropped again after the run executed");
    assert.equal(third.cachesDropped, 1);
    for (const rel of CACHES) assert.equal(exists(path.join(run.home, rel)), false, `${rel} dropped again`);

    status = "running"; // resumed elsewhere
    await runDiskReclaimPass(base);
    assert.equal(memo.has(run.id), false, "a non-park status forgets the drop too");
  });

  it("a run that resumes here, rebuilds its caches and parks again between two passes is dropped again", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const locks = new RunDiskLocks();
    const memo = new CachesDroppedMemo();
    const claim = makeClaim({ run_id: randomUUID() });
    const home = path.join(d.home, claim.run_id);
    seedHome(home);
    // The resume's executor factory stands in for the whole execution: it rebuilds the
    // caches, and the run then ends (parks) before the next pass looks.
    const factory: ExecutorFactory = () => {
      seedHome(home);
      throw new Error("stop after the factory");
    };
    const runner = new RunRunner({} as WorkerClient, {} as GitCache, factory, nullLogger(), 500, "tok", {
      diskLocks: locks,
      cachesDropped: memo,
    });
    let drops = 0;
    const pass = () =>
      runDiskReclaimPass(
        deps(d, {
          locks,
          cachesDropped: memo,
          isRunLive: (id) => runner.isExecuting(id),
          statusOf: async () => "limit_wait",
          dropCaches: async (h, log, opts) => {
            drops++;
            return dropRunCaches(h, log, opts);
          },
        }),
      );

    const first = await pass();
    assert.equal(first.cachesDropped, 1);
    assert.ok(memo.has(claim.run_id), "remembered as dropped");

    // Resumed on this worker and parked again, all between two passes: no pass saw it live.
    await runner.execute(claim).catch(() => undefined);
    assert.equal(runner.isExecuting(claim.run_id), false, "the execution is over before the next pass");
    assert.equal(memo.has(claim.run_id), false, "the runner forgot the drop when it started executing the run");
    for (const rel of CACHES) assert.ok(exists(path.join(home, rel)), `${rel} rebuilt by the resume`);

    const second = await pass();
    assert.equal(drops, 2, "the next pass drops the rebuilt caches instead of trusting the stale memo");
    assert.equal(second.cachesDropped, 1);
    for (const rel of CACHES) assert.equal(exists(path.join(home, rel)), false, `${rel} dropped again`);
  });

  it("stops at its per-root read cap on a flooded root, reading a bounded number of names", async () => {
    const d = dataDir();
    // A live run's agent plants names in agent-home and provision: files and dirs, none a run.
    for (let i = 0; i < 300; i++) {
      fs.writeFileSync(path.join(d.home, `junk-${i}`), "");
      fs.mkdirSync(path.join(d.provision, `junk-${i}`));
    }
    const { logger, lines } = recordingLogger();
    let lookups = 0;
    const s = await runDiskReclaimPass(
      deps(d, {
        maxDirReads: 50,
        log: logger,
        statusOf: async () => {
          lookups++;
          return undefined;
        },
      }),
    );
    assert.equal(s.stoppedEarly, "budget");
    assert.equal(s.dirEntriesRead, 100, "exactly the cap per root, not the 600 names on disk");
    assert.equal(lookups, 0);
    const capLines = lines.filter((l) => (l as { msg: string }).msg === "disk reclaim stopped listing a directory at its read cap");
    assert.equal(capLines.length, 2, "one warning per capped root");
    const done = lines.find((l) => (l as { msg: string }).msg === "disk reclaim pass complete") as Record<string, unknown>;
    assert.equal(done.stopped_early, "budget");
    assert.equal(done.dir_entries_read, 100);

    // Under the cap, the same pass reads everything and does not stop early.
    const full = await runDiskReclaimPass(deps(d, { maxDirReads: 1_000 }));
    assert.equal(full.stoppedEarly, undefined);
    assert.equal(full.dirEntriesRead, 600);
  });

  it("a capped listing still examines what it read, and keeps the memo of runs it did not reach", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const run = seedRun(d);
    const memo = new CachesDroppedMemo();
    const unseen = randomUUID();
    memo.add(unseen);
    const s = await runDiskReclaimPass(
      deps(d, { maxDirReads: 1, cachesDropped: memo, statusOf: async () => "completed" }),
    );
    assert.equal(s.stoppedEarly, "budget");
    assert.equal(s.terminalHomesRemoved, 1, "the one run the capped listing read was examined");
    assert.equal(exists(run.home), false);
    assert.ok(memo.has(unseen), "a run past the cap is not known to be gone, so it is not forgotten");
    // The capped window's one entry was the HOME this pass removed, so the cursor, less that
    // removal, is back at 0: the next listing is whole.
    await runDiskReclaimPass(deps(d, { cachesDropped: memo }));
    assert.equal(memo.has(unseen), false, "a whole listing without its HOME forgets it");
  });

  /** An lstat that presents every path in `foreign` as owned by another uid (the runner's). */
  function lstatAs(foreign: Set<string>, workerUid: number) {
    return async (p: string) => {
      const st = await fs.promises.lstat(p);
      return { uid: foreign.has(p) ? workerUid + 1 : workerUid, mtimeMs: st.mtimeMs, isDirectory: () => st.isDirectory() };
    };
  }

  /** `dir`'s names in the order the pass's streamed listing reads them (`readdirSync` may sort). */
  function listingOrder(dir: string): string[] {
    const handle = fs.opendirSync(dir);
    const names: string[] = [];
    try {
      for (let e = handle.readSync(); e !== null; e = handle.readSync()) names.push(e.name);
    } finally {
      handle.closeSync();
    }
    return names;
  }

  /** A minimal worker-owned run HOME (no read-only subtree, so root runs it too). */
  function plainHome(root: string): { id: string; home: string } {
    const id = randomUUID();
    const home = path.join(root, id);
    fs.mkdirSync(path.join(home, ".claude"), { recursive: true });
    fs.writeFileSync(path.join(home, ".claude.json"), "{}");
    return { id, home };
  }

  it("flood A: planted run-shaped dirs not owned by the worker take no lookup and no budget slot", async () => {
    const d = dataDir();
    const workerUid = 4242;
    const foreign = new Set<string>();
    for (let i = 0; i < 2_000; i++) {
      const dir = path.join(i % 2 ? d.home : d.provision, randomUUID());
      fs.mkdirSync(dir);
      foreign.add(dir);
    }
    const real = Array.from({ length: 20 }, () => plainHome(d.home));
    let lookups = 0;
    const s = await runDiskReclaimPass(
      deps(d, {
        workerUid,
        lstat: lstatAs(foreign, workerUid),
        maxEntries: 500,
        statusOf: async () => {
          lookups++;
          return "completed";
        },
      }),
    );
    for (const r of real) assert.equal(exists(r.home), false, "every terminal worker-owned HOME is removed in pass 1");
    for (const dir of foreign) assert.ok(exists(dir), "a planted dir is never touched");
    assert.equal(lookups, 20, "one status lookup per real run, none for the 2000 planted dirs");
    assert.equal(s.runsExamined, 20);
    assert.equal(s.skippedNotOwned, 2_000);
    assert.equal(s.terminalHomesRemoved, 20);
    assert.equal(s.unexamined, 0);
    assert.equal(s.stoppedEarly, undefined);
  });

  it("a provision dir the worker does not own is skipped and counted, and its run's HOME is still removed", async () => {
    const d = dataDir();
    const workerUid = 4242;
    // A legacy pre-split provision dir the entrypoint re-owned to runner: rmTreePinned would
    // refuse it (EPERM), so the pass never tries.
    const run = plainHome(d.home);
    const legacyProvision = path.join(d.provision, run.id);
    fs.mkdirSync(legacyProvision);
    // A planted HOME beside a worker-owned provision dir: the run is examined through its
    // provision dir only, and the foreign HOME is left alone.
    const other = randomUUID();
    const plantedHome = path.join(d.home, other);
    const ownedProvision = path.join(d.provision, other);
    fs.mkdirSync(plantedHome);
    fs.mkdirSync(ownedProvision);
    const removed: string[] = [];
    const { logger, lines } = recordingLogger();
    const s = await runDiskReclaimPass(
      deps(d, {
        workerUid,
        log: logger,
        lstat: lstatAs(new Set([legacyProvision, plantedHome]), workerUid),
        statusOf: async () => "completed",
        removeTree: async (parent, name) => {
          removed.push(path.join(parent, name));
          fs.rmSync(path.join(parent, name), { recursive: true, force: true });
        },
      }),
    );
    assert.equal(exists(run.home), false, "the worker-owned HOME is removed");
    assert.ok(exists(legacyProvision), "the runner-owned provision dir is left alone");
    assert.ok(!removed.includes(legacyProvision), "and its removal is never attempted");
    assert.ok(exists(plantedHome), "a HOME the worker does not own is never removed");
    assert.equal(exists(ownedProvision), false, "the worker-owned provision dir beside it is");
    assert.equal(s.runsExamined, 2);
    assert.equal(s.terminalHomesRemoved, 1);
    assert.equal(s.provisionDirsRemoved, 1);
    assert.equal(s.provisionDirsNotOwned, 1);
    assert.equal(s.failed, 0);
    const warns = lines.filter((l) => (l as { level?: string; msg: string }).msg === "disk reclaim could not remove a terminal run's directory");
    assert.equal(warns.length, 0, "no warning for a dir it never tried");
  });

  it("never touches a model-pass-named dir the worker does not own, whatever its age", async () => {
    const d = dataDir();
    const workerUid = 4242;
    const planted = path.join(d.home, "uzi-judge-planted");
    fs.mkdirSync(planted);
    const old = (Date.now() - 2 * 60 * 60_000) / 1000;
    fs.utimesSync(planted, old, old);
    const s = await runDiskReclaimPass(deps(d, { workerUid, lstat: lstatAs(new Set([planted]), workerUid) }));
    assert.ok(exists(planted));
    assert.equal(s.modelPassHomesNotOwned, 1);
    assert.equal(s.modelPassHomesRemoved, 0);
  });

  it("flood B: a capped listing rotates, so real HOMEs behind a flood of names (within the read budget) are reached within ceil(N/cap)+1 passes", async () => {
    const d = dataDir();
    // Real HOMEs on both sides of the flood in creation order, so some sit past the first
    // window whichever order this filesystem lists in.
    const real = Array.from({ length: 10 }, () => plainHome(d.home));
    for (let i = 0; i < 200; i++) {
      if (i % 2) fs.writeFileSync(path.join(d.home, `junk-${i}`), "");
      else fs.mkdirSync(path.join(d.home, `junk-${i}`));
    }
    real.push(...Array.from({ length: 10 }, () => plainHome(d.home)));
    const cap = 60; // read budget 4 x 60 = 240 covers the root's 220 entries
    const bound = Math.ceil(220 / cap) + 1;
    const cursors = new Map<string, number>();
    const removedPerPass: number[] = [];
    for (let pass = 0; pass < bound && real.some((r) => exists(r.home)); pass++) {
      const s = await runDiskReclaimPass(deps(d, { maxDirReads: cap, dirCursors: cursors, statusOf: async () => "completed" }));
      assert.ok(s.dirEntriesRead <= 220, "a pass never reads more than the directory holds");
      removedPerPass.push(s.terminalHomesRemoved);
    }
    for (const r of real) assert.equal(exists(r.home), false, `every real HOME is reached within ${bound} passes`);
    assert.ok((removedPerPass[0] ?? 0) < 20, "the first window alone does not hold every real HOME");
    assert.equal(
      removedPerPass.reduce((a, b) => a + b, 0),
      20,
      "each real HOME is removed exactly once",
    );
  });

  it("a capped listing resumes where it stopped and wraps to 0 at the directory's end", async () => {
    const d = dataDir();
    for (let i = 0; i < 30; i++) fs.writeFileSync(path.join(d.home, `junk-${i}`), "");
    const cursors = new Map<string, number>();
    const pass = () => runDiskReclaimPass(deps(d, { maxDirReads: 20, dirCursors: cursors }));
    await pass();
    assert.equal(cursors.get(d.home), 20, "stopped at the cap: resume after the window");
    assert.equal(cursors.has(d.provision), false, "an empty root reached its end: starts from 0");
    const second = await pass();
    assert.equal(second.dirEntriesRead, 30, "20 skipped then the last 10 read");
    assert.equal(second.stoppedEarly, undefined);
    assert.equal(cursors.has(d.home), false, "reached the end: the next listing starts from 0");
  });

  it("a resumed listing charges its skipped prefix to the read budget and starts over past it", async () => {
    const d = dataDir();
    for (let i = 0; i < 200; i++) fs.writeFileSync(path.join(d.home, `junk-${i}`), "");
    // A cursor far past what one pass may read (budget 4 x 10 = 40 per root).
    const cursors = new Map<string, number>([[d.home, 150]]);
    const s = await runDiskReclaimPass(deps(d, { maxDirReads: 10, dirCursors: cursors }));
    assert.equal(s.dirEntriesRead, 40, "the skip stopped at the budget, not at the 150-entry cursor");
    assert.equal(s.stoppedEarly, "budget");
    assert.equal(cursors.has(d.home), false, "the cursor is back at 0");
    const again = await runDiskReclaimPass(deps(d, { maxDirReads: 10, dirCursors: cursors }));
    assert.equal(again.dirEntriesRead, 10, "the next pass reads its first window");
    assert.equal(cursors.get(d.home), 10);

    // Rotation wraps at the budget, so no pass skips past it.
    const seen: number[] = [];
    for (let i = 0; i < 4; i++) {
      await runDiskReclaimPass(deps(d, { maxDirReads: 10, dirCursors: cursors }));
      seen.push(cursors.get(d.home) ?? 0);
    }
    assert.deepEqual(seen, [20, 30, 0, 10], "a window ending at the 40-entry budget wraps to 0");
  });

  it("stops a listing at the pass deadline, in the skipped prefix and in the kept window", async () => {
    const d = dataDir();
    for (let i = 0; i < 500; i++) fs.writeFileSync(path.join(d.home, `junk-${i}`), "");
    for (const skip of [300, 0]) {
      let t = 0;
      const cursors = new Map<string, number>(skip ? [[d.home, skip]] : []);
      const s = await runDiskReclaimPass(
        deps(d, { maxDirReads: 1_000, dirCursors: cursors, deadlineMs: 100, now: () => t++ }),
      );
      assert.equal(s.stoppedEarly, "deadline", `skip ${skip}: reported as the deadline, not the budget`);
      assert.ok(s.dirEntriesRead < 110, `skip ${skip}: stopped near the deadline, not after ${s.dirEntriesRead} reads`);
      if (skip) assert.equal(cursors.get(d.home), skip, "a deadline mid-skip retries the same window");
      else assert.ok((cursors.get(d.home) ?? 0) > 0, "a deadline mid-window resumes where it stopped");
    }
  });

  it("the run budget resumes after the last examined run, so never-removed runs cannot starve terminal HOMEs", async () => {
    const d = dataDir();
    const gone = Array.from({ length: 60 }, () => plainHome(d.home)); // HOMEs of runs the api no longer knows
    const terminal = Array.from({ length: 20 }, () => plainHome(d.home));
    const status = new Map(terminal.map((r) => [r.id, "completed"]));
    const dirCursors = new Map<string, number>();
    const runCursors: NonNullable<DiskReclaimDeps["runCursors"]> = new Map();
    const asked = new Map<string, number>();
    const pass = () =>
      runDiskReclaimPass(
        deps(d, {
          maxEntries: 50,
          dirCursors,
          runCursors,
          statusOf: async (id) => {
            asked.set(id, (asked.get(id) ?? 0) + 1);
            return status.get(id);
          },
        }),
      );
    const first = await pass();
    assert.equal(first.stoppedEarly, "budget");
    assert.equal(first.runsExamined, 50);
    const second = await pass();
    assert.equal(second.runsExamined, 50);
    for (const r of terminal) assert.equal(exists(r.home), false, "every terminal HOME is removed within two passes");
    for (const r of gone) assert.ok(exists(r.home), "a 404 HOME is never removed");
    for (const r of [...gone, ...terminal]) assert.ok((asked.get(r.id) ?? 0) >= 1, "every run was examined");
    assert.equal(first.terminalHomesRemoved + second.terminalHomesRemoved, 20);
  });

  it("a capped window with more never-removed runs than the run budget still lets the listing move on to the HOMEs after it", async () => {
    const d = dataDir();
    const workerUid = 4242;
    // 70 HOMEs, given their roles by the order this filesystem lists them in, so the first
    // window (20 dirents) is all never-removed runs, whatever the readdir order is.
    for (let i = 0; i < 70; i++) plainHome(d.home);
    const order = listingOrder(d.home);
    const persistent = order.slice(0, 30); // 404 HOMEs: examined every rotation, never removed
    const junk = new Set(order.slice(30, 50).map((id) => path.join(d.home, id))); // planted, not the worker's
    const terminal = order.slice(50);
    const status = new Map(terminal.map((id) => [id, "completed"]));
    const dirCursors = new Map<string, number>();
    const runCursors: NonNullable<DiskReclaimDeps["runCursors"]> = new Map();
    const asked = new Map<string, number>();
    const pass = () =>
      runDiskReclaimPass(
        deps(d, {
          workerUid,
          lstat: lstatAs(junk, workerUid),
          maxDirReads: 20,
          maxEntries: 5,
          dirCursors,
          runCursors,
          statusOf: async (id) => {
            asked.set(id, (asked.get(id) ?? 0) + 1);
            return status.get(id);
          },
        }),
      );
    // (ceil(70 / 20) + 1) windows x ceil(20 / 5) passes per window.
    const bound = (Math.ceil(70 / 20) + 1) * Math.ceil(20 / 5);
    let passes = 0;
    while (passes < bound && terminal.some((id) => exists(path.join(d.home, id)))) {
      await pass();
      passes += 1;
    }
    for (const id of terminal) assert.equal(exists(path.join(d.home, id)), false, `every terminal HOME is removed within ${bound} passes`);
    for (const id of persistent) assert.ok(exists(path.join(d.home, id)), "a 404 HOME is never removed");
    for (const p of junk) assert.ok(exists(p), "a planted dir is never removed");
    for (const id of persistent) assert.ok((asked.get(id) ?? 0) >= 1, "every persistent run was examined");
    for (const p of junk) assert.equal(asked.has(path.basename(p)), false, "a planted dir takes no lookup");
  });

  it("a window held for the run rotation keeps its end entry, so entries past it wait for their own window", async () => {
    const d = dataDir();
    for (let i = 0; i < 30; i++) plainHome(d.home);
    const order = listingOrder(d.home);
    // The first window (10 dirents): 6 terminal HOMEs and 4 never-removed runs.
    const terminal = new Set(order.slice(0, 6));
    const dirCursors = new Map<string, number>();
    const runCursors: NonNullable<DiskReclaimDeps["runCursors"]> = new Map();
    const examined: string[][] = [];
    const pass = async () => {
      const seen: string[] = [];
      await runDiskReclaimPass(
        deps(d, {
          maxDirReads: 10,
          maxEntries: 5,
          dirCursors,
          runCursors,
          statusOf: async (id) => {
            seen.push(id);
            return terminal.has(id) ? "completed" : undefined;
          },
        }),
      );
      examined.push(seen);
    };
    await pass();
    assert.equal(examined[0]?.length, 5);
    assert.equal(dirCursors.has(d.home), false, "the window is held");
    await pass();
    const firstWindow = new Set(order.slice(0, 10));
    for (const id of examined[1] ?? []) assert.ok(firstWindow.has(id), "the held window reads none of the entries past its end");
    assert.equal(dirCursors.get(d.home), 10 - 6, "the rotation wrapped: the listing moves on past the window, less its removals");
    assert.equal(runCursors.has(d.home), false);
  });

  it("a drop that found nothing is not counted as a drop and logs nothing of its own", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const id = randomUUID();
    fs.mkdirSync(path.join(d.home, id, ".claude"), { recursive: true });
    const { logger, lines } = recordingLogger();
    const s = await runDiskReclaimPass(deps(d, { statusOf: async () => "paused", log: logger }));
    assert.equal(s.cachesDropped, 0);
    assert.equal(s.cachesAlreadyClear, 1);
    const msgs = lines.map((l) => (l as { msg: string }).msg);
    assert.ok(!msgs.some((m) => /caches/.test(m)), `no drop line for a no-op: ${JSON.stringify(msgs)}`);
  });

  it("a failed drop is counted as failed and not remembered", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const run = seedRun(d);
    const memo = new CachesDroppedMemo();
    const s = await runDiskReclaimPass(
      deps(d, {
        cachesDropped: memo,
        statusOf: async () => "paused",
        dropCaches: async () => ({ dropped: [".npm/_cacache"], absent: [], failed: ["go/pkg/mod"], skipped: [] }),
      }),
    );
    assert.equal(s.failed, 1);
    assert.equal(memo.has(run.id), false);
  });

  /**
   * The audit's attack against the reclaim itself: a terminal run's HOME is removed while a
   * same-uid racer swaps intermediate dirs inside it for symlinks to a victim (swap-racer.ts).
   * With the reclaim's removal on rmHomeTree (`fs.rm`), hundreds of victim files went.
   */
  it("removes a terminal HOME without following an intermediate dir swapped for a symlink mid-walk", async (t) => {
    if (noProcFd) return t.skip("no descriptor-pinned walk on this host: it refuses here by design");
    const d = dataDir();
    const victim = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-disk-reclaim-victim-"));
    roots.push(victim);
    const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-disk-reclaim-racer-"));
    roots.push(scratch);
    const id = randomUUID();
    const home = path.join(d.home, id);
    await seedRacedTree(home, victim);
    const racer = await startSwapRacer(home, victim, scratch);
    let s;
    let swaps: number;
    try {
      s = await runDiskReclaimPass(deps(d, { statusOf: async () => "completed" }));
    } finally {
      swaps = await racer.stop();
    }
    assert.ok(swaps > 0, "the racer swapped at least one directory mid-walk (the race happened)");
    assert.equal(fs.readdirSync(victim).length, RACED_FILES, "no victim file outside agent-home was deleted");
    assert.equal(s.terminalHomesRemoved, 1, "the terminal HOME was removed despite the racer");
    assert.equal(exists(home), false);
  });

  it("logs one summary line naming what it freed and what it skipped", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    seedRun(d);
    const { logger, lines } = recordingLogger();
    await runDiskReclaimPass(deps(d, { statusOf: async () => "cancelled", log: logger }));
    const summary = lines.find((l) => (l as { msg: string }).msg === "disk reclaim pass complete") as Record<string, unknown>;
    assert.ok(summary, "summary logged");
    assert.equal(summary.terminal_homes_removed, 1);
    assert.equal(summary.provision_dirs_removed, 1);
    assert.equal(summary.skipped_live, 0);
  });
});

describe("runDiskReclaimPass model-pass HOMEs (PRD #1809 D7)", () => {
  const OLD = Date.now() - 2 * 60 * 60_000;

  function seedPassHome(root: string, name: string, mtimeMs: number): string {
    const dir = path.join(root, name);
    fs.mkdirSync(path.join(dir, ".claude"), { recursive: true });
    fs.writeFileSync(path.join(dir, ".claude", "state"), "x");
    fs.utimesSync(dir, mtimeMs / 1000, mtimeMs / 1000);
    return dir;
  }

  it("derives the age bound from the longest configured model-pass timeout plus a margin", () => {
    assert.equal(modelPassMinAgeMs([5 * 60_000, 5 * 60_000, 60_000]), 20 * 60_000);
    assert.equal(modelPassMinAgeMs([5 * 60_000, 5 * 60_000, 2 * 60 * 60_000]), 2 * 60 * 60_000 + 15 * 60_000);
  });

  it("removes a stranded pass HOME older than the bound and keeps a recent one", async () => {
    const d = dataDir();
    const old = seedPassHome(d.home, "uzi-summary-old1", OLD);
    const recent = seedPassHome(d.home, "uzi-judge-new1", Date.now());
    const other = seedPassHome(d.home, "not-a-pass-home", OLD);
    const s = await runDiskReclaimPass(deps(d, {}));
    assert.equal(exists(old), false, "an old unowned pass HOME is removed");
    assert.ok(exists(recent), "a pass HOME younger than any pass lives is kept");
    assert.ok(exists(other), "a dir that is not a pass HOME is never touched");
    assert.equal(s.modelPassHomesRemoved, 1);
    assert.equal(s.modelPassHomesTooRecent, 1);
  });

  it("never removes the HOME of a model pass live in this process, whatever its age", async () => {
    const d = dataDir();
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    let started!: () => void;
    const running = new Promise<void>((r) => (started = r));
    const queryFn = (() =>
      (async function* () {
        started();
        await gate;
        yield { type: "result", subtype: "success", is_error: false };
      })()) as unknown as SdkQueryFn;
    const pass = runReadOnlyModelPass({
      token: "tok",
      systemPrompt: "sys",
      prompt: "hi",
      homeRoot: d.home,
      homePrefix: "uzi-review-",
      label: "review",
      timeoutMs: 30_000,
      queryFn,
      denyReason: "read-only",
      log: nullLogger(),
    });
    await running;
    const [name] = fs.readdirSync(d.home).filter((n) => n.startsWith("uzi-review-"));
    assert.ok(name, "the live pass created its HOME");
    const home = path.join(d.home, name);

    // Age bound 0: only the registry stands between the live pass and deletion.
    const s = await runDiskReclaimPass(deps(d, { modelPassMinAgeMs: 0 }));
    assert.ok(exists(home), "the live pass's HOME is kept");
    assert.equal(s.modelPassHomesLive, 1);
    assert.equal(s.modelPassHomesRemoved, 0);

    release();
    await pass;
    assert.equal(exists(home), false, "the pass removed its own HOME when it finished");
  });
});

describe("RunDiskLocks × RunRunner (PRD #1809 D7)", () => {
  it("a resume of a run waits for an in-progress reclaim of that run, then proceeds", { skip: SKIP_ROOT }, async () => {
    const d = dataDir();
    const locks = new RunDiskLocks();
    const claim = makeClaim({ run_id: randomUUID() });
    const run = { home: path.join(d.home, claim.run_id), provision: path.join(d.provision, claim.run_id) };
    seedHome(run.home);
    seedProvision(run.provision);

    const events: string[] = [];
    let factoryCalled!: () => void;
    const factoryReached = new Promise<void>((r) => (factoryCalled = r));
    // The executor factory is the first thing an execution does with the run's HOME; stop there.
    const factory: ExecutorFactory = () => {
      events.push("factory");
      factoryCalled();
      throw new Error("stop after the factory");
    };
    const runner = new RunRunner({} as WorkerClient, {} as GitCache, factory, nullLogger(), 500, "tok", {
      diskLocks: locks,
    });

    let dropEntered!: () => void;
    const dropping = new Promise<void>((r) => (dropEntered = r));
    let finishDrop!: () => void;
    const dropGate = new Promise<void>((r) => (finishDrop = r));
    const reclaim = runDiskReclaimPass({
      homeRoot: d.home,
      provisionRoot: d.provision,
      statusOf: async () => "limit_wait",
      isRunLive: (id) => runner.isExecuting(id),
      locks,
      log: nullLogger(),
      dropCaches: async () => {
        events.push("drop:start");
        dropEntered();
        await dropGate;
        events.push("drop:end");
        return { dropped: [...CACHES], absent: [], failed: [], skipped: [] };
      },
    });
    await dropping;

    // The resume arrives while the reclaim holds the run's lock mid-deletion.
    const exec = runner.execute(claim).catch(() => undefined);
    assert.ok(runner.isExecuting(claim.run_id), "the run is marked executing at once");
    for (let i = 0; i < 20; i++) await new Promise((r) => setImmediate(r));
    assert.deepEqual(events, ["drop:start"], "the resume has not touched the run while the reclaim deletes");

    finishDrop();
    await reclaim;
    await factoryReached;
    await exec;
    assert.deepEqual(events, ["drop:start", "drop:end", "factory"]);
  });

  it("serialises holders of one run and never blocks another run", async () => {
    const locks = new RunDiskLocks();
    const order: string[] = [];
    const a1 = await locks.acquire("a");
    const a2 = locks.acquire("a").then((rel) => {
      order.push("a2");
      rel();
    });
    const b = await locks.acquire("b"); // not blocked by a1
    order.push("b");
    b();
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(order, ["b"], "the second holder of `a` waits for the first");
    a1();
    a1(); // releasing twice is harmless
    await a2;
    assert.deepEqual(order, ["b", "a2"]);
  });
});

describe("DiskPressureController (PRD #1809 D5)", () => {
  function controller(
    over: {
      threshold?: () => number | undefined;
      reclaim?: () => Promise<unknown>;
      now?: () => number;
      admissionMaxWaitMs?: number;
      pressureSpacingMs?: number;
    } = {},
  ) {
    const { logger, lines } = recordingLogger();
    let passes = 0;
    const c = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: over.threshold ?? (() => undefined),
      intervalMs: 60_000,
      admission: true,
      admissionMaxWaitMs: over.admissionMaxWaitMs ?? 15 * 60_000,
      pressureSpacingMs: over.pressureSpacingMs,
      log: logger,
      now: over.now,
      reclaim:
        over.reclaim ??
        (async () => {
          passes++;
        }),
    });
    return { c, lines, passes: () => passes };
  }

  it("derives the soft threshold from the api's, defaulting to 0.90 minus the margin", () => {
    let t: number | undefined;
    const { c } = controller({ threshold: () => t });
    assert.ok(Math.abs(c.softThreshold() - (DEFAULT_DISK_PRESSURE_THRESHOLD - 0.1)) < 1e-9);
    t = 0.95;
    assert.ok(Math.abs(c.softThreshold() - 0.85) < 1e-9);
  });

  it("blocks claims at or over the soft threshold, logs each transition once, and resumes under it", async () => {
    const { c, lines, passes } = controller();
    c.observe(0.5);
    assert.equal(c.claimsBlocked(), false);
    c.observe(0.8);
    assert.equal(c.claimsBlocked(), true, "exactly at the soft threshold blocks");
    c.observe(0.85);
    c.observe(0.79);
    assert.equal(c.claimsBlocked(), false);
    const transitions = lines.filter((l) => /soft threshold/.test((l as { msg: string }).msg));
    assert.equal(transitions.length, 2, "one line per transition, none per tick");
    await new Promise((r) => setImmediate(r));
    assert.equal(passes(), 1, "a pressure pass ran once (the second over-tick fell inside the spacing)");
  });

  it("never puts the soft threshold at or below zero: a threshold under the margin is used as is", () => {
    const { c } = controller({ threshold: () => 0.05 });
    assert.equal(c.softThreshold(), 0.05, "0.05 - 0.10 would block at 0%; the threshold itself is used");
    c.observe(0.02);
    assert.equal(c.claimsBlocked(), false, "2% used is under a 5% threshold");
    c.observe(0.06);
    assert.equal(c.claimsBlocked(), true);
    const exact = controller({ threshold: () => 0.1 });
    assert.equal(exact.c.softThreshold(), 0.1, "a margin equal to the threshold also falls back");
  });

  it("bounds the stop: reopens once a reclaim ran and the wait passed, and closes again on the next crossing", async () => {
    let now = 1_000;
    let finish!: () => void;
    let passes = 0;
    const { c, lines } = controller({
      now: () => now,
      admissionMaxWaitMs: 15 * 60_000,
      pressureSpacingMs: 0,
      reclaim: () => {
        passes++;
        return new Promise<void>((r) => (finish = r));
      },
    });
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), true);
    await new Promise((r) => setImmediate(r));
    assert.equal(passes, 1, "the crossing requested a pass");
    now += 20 * 60_000;
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), true, "past the wait but the pass has not finished: still blocked");
    finish();
    await new Promise((r) => setImmediate(r));
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), false, "a reclaim ran and the wait passed: claims reopen");
    assert.ok(lines.some((l) => /after a reclaim and the admission wait/.test((l as { msg: string }).msg)), "with a warning");
    now += 60 * 60_000;
    c.observe(0.95);
    assert.equal(c.claimsBlocked(), false, "stays open while the volume stays over");
    await new Promise((r) => setImmediate(r));
    finish();
    await new Promise((r) => setImmediate(r));

    c.observe(0.5); // under
    now += 1_000;
    c.observe(0.85); // a fresh crossing
    assert.equal(c.claimsBlocked(), true, "the next crossing closes it again");
    await new Promise((r) => setImmediate(r));
    now += 20 * 60_000;
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), true, "its pass has not finished yet");
    finish();
  });

  it("a pass that started before the crossing does not count toward the bounded wait", async () => {
    let now = 1_000;
    let finish!: () => void;
    const { c } = controller({
      now: () => now,
      admissionMaxWaitMs: 1_000,
      reclaim: () => new Promise<void>((r) => (finish = r)),
    });
    const periodic = c.requestReclaim("periodic");
    await new Promise((r) => setImmediate(r));
    now += 10;
    c.observe(0.85); // crosses while the periodic pass is in flight (the pressure request joins it)
    finish();
    await periodic;
    now += 5_000;
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), true, "only a pass started after the crossing proves a reclaim ran");
  });

  it("UZI_DISK_ADMISSION off: never blocks, and the reclaim still runs over the soft threshold", async () => {
    const { logger } = recordingLogger();
    let passes = 0;
    const c = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: () => undefined,
      intervalMs: 60_000,
      admission: false,
      admissionMaxWaitMs: 15 * 60_000,
      log: logger,
      reclaim: async () => {
        passes++;
      },
    });
    c.observe(0.99);
    assert.equal(c.claimsBlocked(), false);
    await new Promise((r) => setImmediate(r));
    assert.equal(passes, 1);
  });

  it("UZI_DISK_RECLAIM off: no pass ever runs, and the admission stop is still bounded", () => {
    let now = 0;
    const c = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: () => undefined,
      intervalMs: 60_000,
      admission: true,
      admissionMaxWaitMs: 1_000,
      log: nullLogger(),
      now: () => now,
    });
    assert.equal(c.requestReclaim("periodic"), undefined);
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), true);
    now += 1_000;
    c.observe(0.85);
    assert.equal(c.claimsBlocked(), false, "with nothing to reclaim, only the wait applies");
  });

  it("fails open: an unknown sample never blocks claims", () => {
    const { c } = controller();
    c.observe(0.99);
    assert.equal(c.claimsBlocked(), true);
    c.observe(undefined);
    assert.equal(c.claimsBlocked(), false);
  });

  it("an unknown sample is not 'back under': the stretch, its start and its reopening survive it", async () => {
    let now = 1_000;
    const { c, lines } = controller({ now: () => now, admissionMaxWaitMs: 10 * 60_000, pressureSpacingMs: 0 });
    c.observe(0.85); // crossing at t=1s
    await new Promise((r) => setImmediate(r)); // its pass finishes
    now += 6 * 60_000;
    c.observe(undefined); // a statfs blip mid-stretch
    assert.equal(c.claimsBlocked(), false, "fails open while the latest sample is unknown");
    now += 5 * 60_000;
    c.observe(0.85); // 11 min after the ORIGINAL crossing
    assert.equal(c.claimsBlocked(), false, "the bounded wait still counts from the original crossing, so claims reopen");
    const msgs = lines.map((l) => (l as { msg: string }).msg);
    assert.ok(!msgs.some((m) => /back under/.test(m)), "an unknown sample never logs 'back under'");
    assert.equal(msgs.filter((m) => /at or over the soft threshold/.test(m)).length, 1, "and the stretch is not re-entered");

    c.observe(undefined);
    c.observe(0.9);
    assert.equal(c.claimsBlocked(), false, "a reopened stretch stays reopened across an unknown sample");
  });

  it("warns once when an unknown sample lifts an active stop, and never when no stop was active", () => {
    const warned = (lines: unknown[]) =>
      lines.filter((l) => (l as { msg: string }).msg === "disk sample unknown; claims fail open").length;
    const { c, lines } = controller();
    c.observe(0.5);
    c.observe(undefined);
    assert.equal(warned(lines), 0, "no stop was active under the soft threshold");
    c.observe(0.95);
    assert.equal(c.claimsBlocked(), true);
    c.observe(undefined);
    c.observe(undefined);
    assert.equal(warned(lines), 1, "one warning on the known-to-unknown transition, none per unknown tick");
    c.observe(0.95);
    c.observe(undefined);
    assert.equal(warned(lines), 2, "a stop active again warns again on its next lift");

    const offLog = recordingLogger();
    const quiet = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: () => undefined,
      intervalMs: 60_000,
      admission: false,
      admissionMaxWaitMs: 60_000,
      log: offLog.logger,
    });
    quiet.observe(0.99);
    quiet.observe(undefined);
    assert.equal(warned(offLog.lines), 0, "with the admission stop off there is no stop to lift");
  });

  it("runs one pass at a time and spaces pressure passes", async () => {
    let now = 0;
    let finish!: () => void;
    let calls = 0;
    const { c } = controller({
      now: () => now,
      reclaim: () => {
        calls++;
        return new Promise<void>((r) => (finish = r));
      },
    });
    const first = c.requestReclaim("pressure");
    assert.equal(c.requestReclaim("periodic"), first, "a periodic request joins the pass in flight");
    await new Promise((r) => setImmediate(r)); // the pass starts on a later microtask
    finish();
    await first;
    now = 30_000;
    assert.equal(c.requestReclaim("pressure"), undefined, "a pressure pass within 60s of the last is skipped");
    now = 61_000;
    const third = c.requestReclaim("pressure");
    assert.ok(third);
    await new Promise((r) => setImmediate(r));
    finish();
    await third;
    assert.equal(calls, 2);
  });

  it("a reclaim that throws synchronously never wedges the controller", async () => {
    let calls = 0;
    const { c, lines } = controller({
      reclaim: () => {
        calls++;
        throw new Error("sync boom");
      },
    });
    const first = c.requestReclaim("periodic");
    assert.ok(first, "a pass was started");
    await first;
    assert.ok(lines.some((l) => (l as { msg: string }).msg === "disk reclaim pass failed"));
    const second = c.requestReclaim("periodic");
    assert.ok(second && second !== first, "the next request starts a new pass: inFlight was cleared");
    await second;
    assert.equal(calls, 2);
  });

  it("a failing pass is logged and never escapes", async () => {
    const { c, lines } = controller({
      reclaim: async () => {
        throw new Error("boom");
      },
    });
    await c.requestReclaim("periodic");
    assert.ok(lines.some((l) => (l as { msg: string }).msg === "disk reclaim pass failed"));
  });
});
