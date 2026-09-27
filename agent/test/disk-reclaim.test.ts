import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";

import {
  DEFAULT_DISK_PRESSURE_THRESHOLD,
  DiskPressureController,
  runDiskReclaimPass,
  type DiskReclaimDeps,
} from "../src/disk-reclaim.js";
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
// rmHomeTree and dropRunCaches, a read-only `0555` Go module cache in every HOME), with the
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
  function controller(over: { threshold?: () => number | undefined; reclaim?: () => Promise<unknown>; now?: () => number } = {}) {
    const { logger, lines } = recordingLogger();
    let passes = 0;
    const c = new DiskPressureController({
      softMargin: 0.1,
      thresholdOf: over.threshold ?? (() => undefined),
      intervalMs: 60_000,
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

  it("fails open: an unknown sample never blocks claims", () => {
    const { c } = controller();
    c.observe(0.99);
    assert.equal(c.claimsBlocked(), true);
    c.observe(undefined);
    assert.equal(c.claimsBlocked(), false);
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
    finish();
    await first;
    now = 30_000;
    assert.equal(c.requestReclaim("pressure"), undefined, "a pressure pass within 60s of the last is skipped");
    now = 61_000;
    const third = c.requestReclaim("pressure");
    assert.ok(third);
    finish();
    await third;
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
