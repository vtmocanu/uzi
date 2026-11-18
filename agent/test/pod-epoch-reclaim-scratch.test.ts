// Issue #4043 M3: the runner-scratch phase of runPodEpochShadow, driven through the pass with a verified
// fixture epoch, a fake daemon guard and a temp scratch root. The census and delete helpers are the real ones
// where a case needs real bytes; settlement and interference cases use fake census text and a fake discard.
import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { spawn, ChildProcess } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { basename, dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { runPodEpochShadow, PodEpochSettlementError, RUN_LEAF_READ_MAX_MS } from "../src/pod-epoch-reclaim.js";
import { PodEpochDaemonError, type PodEpochDaemonGuard } from "../src/pod-epoch-daemon.js";
import { GitCache, type EpochScratchDiscardInput, type RetainedSourceEvidence, type RetainedInspectResult } from "../src/git.js";
import { censusScratchRoot, runAgentHelper, SCRATCH_CENSUS_SCRIPT } from "../src/rmtree.js";
import { scanScratchProcesses, type Settlement } from "../src/pod-epoch-scratch.js";
import { WORKER_UID, runnerCommand } from "../src/runner-uid.js";
import { buildPodEpochScratchDeps } from "../src/main.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { acquireWorkerLifetime, withAuthorityTransaction } from "../src/authority-lock.js";
import { openAuthorityStore, peekAllAuthoritySlots } from "../src/completion-authority.js";
import { testLockOptions } from "./authority-test-support.js";
import { WorkerClient } from "../src/client.js";
import { execFileSync } from "node:child_process";
import { ENABLE_DELETION_FOR_TEST, ENABLE_SCRATCH_FOR_TEST, perGroupInspect } from "./pod-epoch-reclaim-support.js";
import type { PodEpochOwnership } from "../src/pod-epoch-lock.js";

const bytes = await readFile(new URL("../../fixtures/pod-epoch/v2/positive.json", import.meta.url));
const evidence = JSON.parse(bytes.toString());
const fixture = (name: string) => readFile(new URL("../../fixtures/pod-epoch/" + name, import.meta.url));
const DELETE_SEAM = Symbol.for("uzi.test.pod-epoch-scratch.delete-seam");
const SPLIT_ENV = { UZI_UID_SPLIT: "1", UZI_RUNNER_TMPDIR: "/data/runner/.tmp/uzi-runner", TMPDIR: "/data/runner/.tmp/uzi-worker" };
const HAS_PROC_FD = fs.existsSync("/proc/self/fd");
const FUTURE_FENCE = () => ({ t0Ms: Date.now() + 3_600_000 });

type Options = Parameters<typeof runPodEpochShadow>[0];
type Census = typeof censusScratchRoot;
type CensusResult = Awaited<ReturnType<Census>>;

function fakeScope() {
  let pending = 0, stopped = false;
  const scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() { pending++; let done = false; return { settle() { if (!done) { done = true; pending--; } } }; },
    inheritStdio: (stdio: unknown[] = ["pipe", "pipe", "pipe"]) => [...stdio],
    stop() { stopped = true; }, stopped: () => stopped,
    assertSettled() { if (pending !== 0 || stopped) throw new Error("unsettled"); },
    finish() { world().events.push("finish"); this.assertSettled(); },
  };
  return { scope, pending: () => pending, stopped: () => stopped };
}
let current: World | undefined;
const world = (): World => current!;

/** Census text for fake leaves; `get` is read on every call so a test can change a leaf between calls. */
function fakeCensus(get: () => Array<Record<string, unknown>>, overflow = () => false): Census {
  return async (_root, rootExpect, leaf) => {
    const list = get().filter(e => leaf === undefined || e.name === leaf);
    const lines = [{ root: rootExpect, uid: process.getuid!() }, ...list, { end: true, entries: list.length, overflow: overflow() }];
    return { code: 0, stdout: lines.map(l => JSON.stringify(l)).join("\n") + "\n" } satisfies CensusResult;
  };
}
const dirEntry = (name: string, i: number, over: Record<string, unknown> = {}) => ({
  name, type: "dir", dev: "7", ino: String(100 + i), uid: process.getuid!(), fsdiff: false, fuid: false,
  bytes: 4096, shared: 0, ctime: 1, inc: false, ...over });
const removed = (bytes = 4096): Settlement => ({ outcome: "removed", bytes });

interface World {
  base: string; root: string; events: string[];
  parent: AbortController;
  options: Options;
  scope: ReturnType<typeof fakeScope>;
  guardAbort: AbortController;
  guard: PodEpochDaemonGuard;
  git: GitCache;
  unhealthy: boolean; unsettled: boolean;
  calls: { census: Array<string | undefined>; quiesce: number; protected: number; discards: string[]; refresh: number; binds: number;
    mounts: number; scans: number; begins: number; handovers: number; fence: number };
  gateOpen: boolean;
  env: NodeJS.ProcessEnv;
  quiesce: () => Promise<boolean>;
  binds: () => ReturnType<NonNullable<PodEpochDaemonGuard["bindSources"]>>;
  protectedRead: () => Awaited<ReturnType<GitCache["collectEpochScratchProtectedPaths"]>>;
  mounts: () => ReturnType<NonNullable<NonNullable<NonNullable<Options["scratch"]>["seams"]>["readMounts"]>>;
  fence: () => ReturnType<NonNullable<NonNullable<NonNullable<Options["scratch"]>["seams"]>["readFence"]>>;
  scan: (names: readonly string[]) => ReturnType<typeof scanScratchProcesses>;
  statfs: () => "ok" | "fs_unsupported";
  free: () => bigint | null;
  census: Census;
  discard: (input: EpochScratchDiscardInput) => Promise<Settlement>;
  refresh: () => Promise<void>;
  seamRoot: string | undefined;
  cleanup: () => Promise<void>;
}

async function makeWorld(leaves: string[] = ["a", "b"]): Promise<World> {
  const base = await realpath(await mkdtemp(join(tmpdir(), "uzi-scratch-pass-")));
  const root = join(base, "root");
  await mkdir(root, { mode: 0o700 });
  await chmod(root, 0o700);
  for (const name of leaves) {
    await mkdir(join(root, name, "sub"), { recursive: true });
    await writeFile(join(root, name, "sub", "f.txt"), "x".repeat(5000));
  }
  const parent = new AbortController();
  const guardAbort = new AbortController();
  const real = new GitCache(join(tmpdir(), "uzi-scratch-pass-cache"), nullLogger());
  const f = fakeScope();
  const w: World = {
    base, root, events: [], parent, scope: f, guardAbort, git: real, unhealthy: false, unsettled: false,
    calls: { census: [], quiesce: 0, protected: 0, discards: [], refresh: 0, binds: 0, mounts: 0, scans: 0, begins: 0, handovers: 0, fence: 0 },
    gateOpen: true, env: { ...SPLIT_ENV },
    quiesce: async () => true,
    binds: () => ({ state: "read", sources: [] }),
    protectedRead: () => ({ state: "read", paths: [] }),
    mounts: () => ({ mountedLeaves: new Set<string>() }),
    fence: FUTURE_FENCE,
    scan: () => ({ held: new Set<string>() }),
    statfs: () => "ok",
    free: () => null,
    census: censusScratchRoot,
    discard: input => real.discardEpochScratchLeaf(input),
    refresh: async () => undefined,
    seamRoot: root,
    cleanup: async () => { await rm(base, { recursive: true, force: true }); },
    guard: undefined as never, options: undefined as never,
  };
  w.guard = {
    signal: guardAbort.signal,
    assertHealthy() { if (w.unhealthy) throw new PodEpochDaemonError("daemon_changed"); },
    close: async () => undefined,
    bindSources: () => { w.calls.binds++; return w.binds(); },
    refreshInventory: async () => { w.calls.refresh++; await w.refresh(); },
  };
  const ownership: PodEpochOwnership = {
    mode: "exclusive", consume: async () => { throw new Error("consume already performed"); },
    matchesEntry: () => true,
    reacquireShared: async () => { w.events.push("handover"); w.calls.handovers++; },
    close: () => { throw new Error("must retain ownership"); },
    beginReclaim: () => { w.events.push("begin"); w.calls.begins++; return f.scope as never; },
  };
  w.options = {
    bootstrap: { podUid: evidence.successor, runtime: evidence.runtime, evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
    ownership, consumption: { result: "fresh", mayEvaluate: true }, signal: parent.signal,
    readEvidence: async () => bytes, kernelRelease: () => "6.1.0-test",
    client: { listRecoveryHolds: async run_id => ({ run_id, holds: [] }), getRunOwnership: async () => ({ status: "completed" }) },
    git: {
      discoverEpochRetainedSources: async () => ({ groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 }),
      inspectEpochRetainedGroups: async () => { throw new Error("unexpected inspection"); },
    },
    openDaemonGuard: async () => w.guard,
    scratch: {
      get scratchTestGate() { return w.gateOpen ? ENABLE_SCRATCH_FOR_TEST : undefined; },
      git: {
        assertNoUnsettledDeletes: () => { if (w.unsettled) throw new Error("unsettled runner delete outstanding"); },
        collectEpochScratchProtectedPaths: async () => { w.calls.protected++; return w.protectedRead(); },
        discardEpochScratchLeaf: async input => { w.calls.discards.push(input.name); return w.discard(input); },
      },
      quiesce: async () => { w.calls.quiesce++; return w.quiesce(); },
      seams: {
        get env() { return w.env; },
        layout: { mode: "split", root, ownerUid: process.getuid!(), modes: [0o700] },
        readFence: () => { w.calls.fence++; return w.fence(); },
        readMounts: () => { w.calls.mounts++; return w.mounts(); },
        statfs: () => w.statfs(),
        freeBytes: () => w.free(),
        scanProcesses: names => { w.calls.scans++; return w.scan(names); },
        bindLstat: p => fs.lstatSync(p),
        bindShared: base,
        census: (r, e, leaf, t) => { w.calls.census.push(leaf); return w.census(r, e, leaf, t); },
        get discardSeam() { return w.seamRoot ? { token: DELETE_SEAM, root: w.seamRoot } : undefined; },
      },
    },
  };
  current = w;
  return w;
}

async function withWorld(leaves: string[] | undefined, body: (w: World) => Promise<void>): Promise<void> {
  const w = await makeWorld(leaves);
  try { await body(w); } finally { await w.cleanup(); current = undefined; }
}
const remaining = async (w: World) => (await readdir(w.root)).sort();
const untouched = async (w: World, names: string[]) => assert.deepEqual(await remaining(w), [...names].sort());
const noScratchWork = (w: World) => {
  assert.deepEqual(w.calls.census, []); assert.equal(w.calls.quiesce, 0); assert.equal(w.calls.protected, 0);
  assert.deepEqual(w.calls.discards, []); assert.equal(w.calls.refresh, 0); assert.equal(w.calls.binds, 0);
  assert.equal(w.calls.mounts, 0); assert.equal(w.calls.scans, 0); assert.equal(w.calls.fence, 0);
};
const run = async (w: World) => (await runPodEpochShadow(w.options))!;

test("production scratch activation is separate and nothing under agent/src supplies the sentinel", async () => {
  const src = fileURLToPath(new URL("../src/", import.meta.url));
  const files = (await readdir(src, { recursive: true })).filter(f => /\.(ts|mts|cts|js|mjs|cjs)$/.test(f));
  assert.ok(files.length > 50);
  const referencing: string[] = [];
  for (const f of files) {
    const text = await readFile(join(src, f), "utf8");
    if (/scratchTestGate|scratch-gate|ENABLE_SCRATCH_FOR_TEST/.test(text)) referencing.push(f);
  }
  assert.deepEqual(referencing, ["pod-epoch-reclaim.ts"]);
  const definition = await readFile(join(src, "pod-epoch-reclaim.ts"), "utf8");
  assert.equal(definition.match(/scratchTestGate\s*===\s*TEST_SCRATCH_GATE\b/g)?.length, 2, "the gate and the seams are tied to the sentinel");
  assert.equal(definition.match(/TEST_SCRATCH_GATE\b/g)?.length, 3, "the sentinel is defined once and compared twice");
  assert.match(definition, /scratchDeps\.activation === true \|\| scratchDeps\.scratchTestGate === TEST_SCRATCH_GATE/);
  assert.ok(!definition.includes("EPOCH_RECLAIM_DELETION_ENABLED"), "the attempt phase uses the #4134 startup activation, not a constant");
  const built = buildPodEpochScratchDeps({ git: {} as never, dockerHost: undefined });
  assert.deepEqual(Object.keys(built).sort(), ["activation", "git", "quiesce"]);
});

test("production scratch activation without sentinel enters phase, refuses unsafe layout and cannot read test seams", async () => {
  await withWorld(["a"], async w => {
    const scratch = w.options.scratch!;
    delete scratch.scratchTestGate;
    assert.equal(scratch.scratchTestGate, undefined);
    scratch.activation = true;
    Object.defineProperty(scratch, "seams", { get() { throw new Error("production accessed test seams"); } });
    // An unsupported fixed root refuses before pinning, census or deletion, regardless of host layout.
    const saved = process.env.UZI_RUNNER_TMPDIR;
    process.env.UZI_RUNNER_TMPDIR = "/unsupported-epoch-scratch-root";
    try {
      const result = await run(w);
      assert.equal(result.verdict, "eligible");
      assert.equal(result.scratch.state, "refused");
      assert.equal(result.scratch.reason, "layout_refused");
      assert.equal(result.scratch.deleted, 0);
      noScratchWork(w);
      await untouched(w, ["a"]);
    } finally {
      if (saved === undefined) delete process.env.UZI_RUNNER_TMPDIR;
      else process.env.UZI_RUNNER_TMPDIR = saved;
    }
  });
});

test("scratch activation absent or false without sentinel stays not_run even with attempt activation", async () => {
  for (const activation of [undefined, false]) {
    await withWorld(["a"], async w => {
      delete w.options.scratch!.scratchTestGate;
      w.options.scratch!.activation = activation;
      w.options.deletion = { activation: true } as NonNullable<Options["deletion"]>;
      // No attempt candidates: it may settle its deps, but must never enter scratch.
      w.options.deletion.git = { assertNoUnsettledDeletes() {} } as never;
      const result = await run(w);
      assert.equal(result.scratch.state, "not_run");
      assert.equal(result.scratch.reason, "not_run");
      noScratchWork(w);
      await untouched(w, ["a"]);
    });
  }
});

test("gate closed: a pass with scratch deps does no census, no child, no new path read and reports not_run", async () => {
  await withWorld(["a", "b"], async w => {
    w.gateOpen = false;
    w.options.scratch!.activation = false;
    const result = await run(w);
    assert.equal(result.verdict, "eligible");
    assert.equal(result.scratch.state, "not_run"); assert.equal(result.scratch.reason, "not_run");
    assert.equal(result.scratch.layout, null); assert.equal(result.scratch.deleted, 0);
    assert.equal(result.scratch.reclaim_bytes_state, "unknown");
    noScratchWork(w);
    assert.equal(w.calls.begins, 0);
    await untouched(w, ["a", "b"]);
    assert.deepEqual(w.events, ["handover"]);
  });
});

test("baseline: only admitted leaves are removed; protected, bound, held, helper, file and container names stay", {
  skip: HAS_PROC_FD ? false : "no /proc/self/fd on this host: the pinned walk refuses here by design",
}, async () => {
  const names = ["ok-1", "ok-2", "pr-leaf", "bd-leaf", "hd-leaf", "uzi-quiesce-x", "uzi-worker"];
  await withWorld(names, async w => {
    await writeFile(join(w.root, "plain-file"), "f");
    w.protectedRead = () => ({ state: "read", paths: [join(w.root, "pr-leaf", "sub")] });
    w.binds = () => ({ state: "read", sources: [join(w.root, "bd-leaf", "sub")] });
    // A real scan over a fake /proc: a worker-uid process whose cwd is inside the held leaf.
    const proc = join(w.base, "proc");
    await fakeProc(proc, [{ pid: 7, cwd: join(w.root, "hd-leaf", "sub") }]);
    w.scan = n => scanScratchProcesses(w.root, n, undefined, proc);
    let free = 10_000n;
    w.free = () => (free -= 2000n);
    const result = await run(w);
    const s = result.scratch;
    assert.equal(s.state, "ran"); assert.equal(s.reason, "complete"); assert.equal(s.layout, "split");
    assert.equal(s.examined, 8);
    assert.deepEqual(s.kept, { protected_name: 1, not_directory: 1, name_refused: 0, active_helper: 1, foreign_owner: 0, mount: 0,
      incomplete: 0, current_pod: 0, protected_record: 1, bind_referenced: 1, held_open: 1, cap: 0, changed: 0,
      run_active: 0, run_retained: 0, run_unknown: 0, run_unreadable: 0, run_local_active: 0, run_local_unknown: 0 });
    assert.equal(s.eligible, 2); assert.equal(s.deleted, 2); assert.equal(s.residue, 0); assert.equal(s.helper_failed, 0);
    assert.ok(s.eligible_allocated_bytes_estimate > 0);
    assert.equal(s.deleted_allocated_bytes_estimate, s.eligible_allocated_bytes_estimate);
    assert.equal(s.reclaim_bytes_state, "estimated"); assert.equal(s.estimate_partial, false);
    assert.equal(s.statfs_free_delta_unattributed, -2000);
    assert.equal(s.not_idle_after, false);
    await untouched(w, ["bd-leaf", "hd-leaf", "plain-file", "pr-leaf", "uzi-quiesce-x", "uzi-worker"]);
    // One shared scope, finished before the handover; no attempt phase (no deletion deps) opened another.
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
    assert.equal(w.scope.pending(), 0);
    // Verdicts carry only fixed tokens and counts: no path or leaf name.
    const text = JSON.stringify(result.scratch);
    for (const needle of [w.base, "ok-1", "ok-2", "-leaf", "plain-file"]) assert.ok(!text.includes(needle), needle);
  });
});

/** A fake /proc: `<pid>/status`, a `cwd` link and an `fd` directory of links. */
async function fakeProc(dir: string, procs: Array<{ pid: number; uid?: number; cwd?: string; fds?: string[]; fdIsFile?: boolean }>) {
  await mkdir(dir, { recursive: true });
  for (const p of procs) {
    const u = p.uid ?? WORKER_UID;
    const pdir = join(dir, String(p.pid));
    await mkdir(pdir);
    await writeFile(join(pdir, "status"), `Name:\tx\nUid:\t${u}\t${u}\t${u}\t${u}\nGid:\t${u}\t${u}\t${u}\t${u}\n`);
    await symlink(p.cwd ?? "/", join(pdir, "cwd"));
    if (p.fdIsFile) await writeFile(join(pdir, "fd"), "");
    else {
      await mkdir(join(pdir, "fd"));
      for (const [i, target] of (p.fds ?? []).entries()) await symlink(target, join(pdir, "fd", String(i + 3)));
    }
  }
}

test("kept with a fixed reason: an open descriptor, a mount, an active quiesce helper and a leaf newer than the pod entry", async () => {
  const future = Date.now() + 7_200_000;
  await withWorld(["held", "mounted", "uzi-quiesce-1", "fresh", "old"], async w => {
    const proc = join(w.base, "proc");
    await fakeProc(proc, [{ pid: 9, fds: [join(w.root, "held", "sub", "f.txt") + " (deleted)"] }]);
    w.scan = n => scanScratchProcesses(w.root, n, undefined, proc);
    w.mounts = () => ({ mountedLeaves: new Set(["mounted"]) });
    w.discard = async () => removed();
    w.census = fakeCensus(() => [dirEntry("held", 1), dirEntry("mounted", 2), dirEntry("uzi-quiesce-1", 3),
      dirEntry("fresh", 4, { ctime: future }), dirEntry("old", 5)]);
    const result = await run(w);
    assert.deepEqual(result.scratch.kept, { protected_name: 0, not_directory: 0, name_refused: 0, active_helper: 1, foreign_owner: 0,
      mount: 1, incomplete: 0, current_pod: 1, protected_record: 0, bind_referenced: 0, held_open: 1, cap: 0, changed: 0,
      run_active: 0, run_retained: 0, run_unknown: 0, run_unreadable: 0, run_local_active: 0, run_local_unknown: 0 });
    assert.deepEqual(w.calls.discards, ["old"]);
  });
});

test("single-UID layout: one pass over a mixed root deletes only the safe orphan", async () => {
  await withWorld(["orphan", "uzi-worker", "uzi-runner", "uzi-quiesce-1", "cur", "dind"], async w => {
    // The exact single-UID environment: no split variables, TMPDIR at the shared pool root.
    w.env = { TMPDIR: "/data/runner/.tmp" };
    w.options.scratch!.seams!.layout = { mode: "single", root: w.root, ownerUid: process.getuid!(), modes: [0o700] };
    w.fence = () => ({ t0Ms: 1000 });
    w.binds = () => ({ state: "read", sources: [join(w.root, "dind", "sub")] });
    w.census = fakeCensus(() => [dirEntry("orphan", 1), dirEntry("uzi-worker", 2), dirEntry("uzi-runner", 3), dirEntry("uzi-quiesce-1", 4),
      dirEntry("cur", 5, { ctime: 5000 }), dirEntry("dind", 6)]);
    w.discard = async input => { await rm(join(w.root, input.name), { recursive: true }); return removed(); };
    const result = await run(w);
    const s = result.scratch;
    assert.equal(s.state, "ran"); assert.equal(s.layout, "single");
    assert.equal(s.examined, 6); assert.equal(s.eligible, 1); assert.equal(s.deleted, 1); assert.equal(s.residue, 0);
    assert.deepEqual(s.kept, { protected_name: 2, not_directory: 0, name_refused: 0, active_helper: 1, foreign_owner: 0, mount: 0,
      incomplete: 0, current_pod: 1, protected_record: 0, bind_referenced: 1, held_open: 0, cap: 0, changed: 0,
      run_active: 0, run_retained: 0, run_unknown: 0, run_unreadable: 0, run_local_active: 0, run_local_unknown: 0 });
    assert.deepEqual(w.calls.discards, ["orphan"]);
    await untouched(w, ["cur", "dind", "uzi-quiesce-1", "uzi-runner", "uzi-worker"]);
  });
});

// A clock stepped back and forward leaves every leaf's ctime below T0, so the fence admits everything
// (FUTURE_FENCE is exactly that). Only the live-writer checks remain between a leaf and its delete.
test("weakened fence with a live writer: a held-open leaf is kept, whether held at the census or only before the delete", async () => {
  for (const heldAtCensus of [true, false]) {
    await withWorld(["a"], async w => {
      w.fence = FUTURE_FENCE;
      w.census = fakeCensus(() => [dirEntry("a", 1)]);
      w.discard = async () => removed();
      // Scan 1 is the pre-census probe, scan 2 the census-time scan, scan 3 the pre-delete scan of the leaf.
      let scans = 0;
      w.scan = () => ({ held: new Set(++scans >= (heldAtCensus ? 2 : 3) ? ["a"] : []) });
      const result = await run(w);
      assert.equal(result.scratch.kept.held_open, 1, `heldAtCensus=${heldAtCensus}`);
      assert.equal(result.scratch.deleted, 0);
      assert.deepEqual(w.calls.discards, []);
      await untouched(w, ["a"]);
    });
  }
});

test("weakened fence with a live writer: a leaf that changes between the census and the re-census is kept changed", async () => {
  await withWorld(["a"], async w => {
    w.fence = FUTURE_FENCE;
    let calls = 0;
    w.census = fakeCensus(() => [dirEntry("a", 1, { bytes: ++calls === 1 ? 4096 : 8192 })]);
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(result.scratch.kept.changed, 1); assert.equal(result.scratch.deleted, 0);
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, ["a"]);
  });
});

test("weakened fence with a live writer: a failed idle proof before the delete stops the phase and keeps the leaf", async () => {
  await withWorld(["a"], async w => {
    w.fence = FUTURE_FENCE;
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => removed();
    let q = 0;
    w.quiesce = async () => ++q < 2;
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "not_idle");
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, ["a"]);
  });
});

test("a leaf whose census is incomplete, foreign-owned or on another filesystem is never admitted", async () => {
  await withWorld(["inc", "foreign", "fs", "good"], async w => {
    w.discard = async () => removed();
    w.census = fakeCensus(() => [dirEntry("inc", 1, { inc: true }), dirEntry("foreign", 2, { fuid: true }),
      dirEntry("fs", 3, { fsdiff: true }), dirEntry("good", 4)]);
    const result = await run(w);
    assert.equal(result.scratch.kept.incomplete, 1); assert.equal(result.scratch.kept.foreign_owner, 1);
    assert.equal(result.scratch.kept.mount, 1); assert.equal(result.scratch.eligible, 1);
    assert.deepEqual(w.calls.discards, ["good"]);
  });
});

/** Every refusal leaves every leaf in place, spawns no delete and lets the attempt phase's pass continue. */
const refusals: Array<[string, string, (w: World) => void | Promise<void>]> = [
  ["layout_refused", "layout_refused (TMPDIR at the data root)", w => { w.env = { ...SPLIT_ENV, TMPDIR: "/data/runner" }; }],
  ["layout_refused", "layout_refused (single layout with a split variable left)", w => {
    w.env = { UZI_RUNNER_TMPDIR: "/data/runner/.tmp/uzi-runner", TMPDIR: "/data/runner/.tmp" }; }],
  ["layout_refused", "layout_refused (unset)", w => { w.env = {}; }],
  ["root_unpinned", "root_unpinned (the root is missing)", async w => { await rm(w.root, { recursive: true }); }],
  ["root_identity", "root_identity (the root has the wrong mode)", async w => { await chmod(w.root, 0o755); }],
  ["pod_entry_invalid", "pod_entry_invalid (real reader, no record)", w => {
    const options = w.options.scratch!.seams!;
    options.readFence = undefined; options.podEntryFile = join(w.base, "no-such-pod-entry"); }],
  ["pod_entry_invalid", "pod_entry_invalid (real reader, another pod's record)", async w => {
    const options = w.options.scratch!.seams!;
    const other = "11111111-2222-3333-4444-555555555555";
    await writeFile(join(w.base, "entry"), JSON.stringify({ current: other, previous: null }));
    options.readFence = undefined; options.podEntryFile = join(w.base, "entry"); }],
  ["mount_unexpected_ancestor", "mount_unexpected_ancestor", w => { w.mounts = () => ({ refused: "mount_unexpected_ancestor" }); }],
  ["mount_unknown", "mount_unknown", w => { w.mounts = () => ({ refused: "mount_unknown" }); }],
  ["fs_unsupported", "fs_unsupported", w => { w.statfs = () => "fs_unsupported"; }],
  ["bind_unknown", "bind_unknown (unknown inventory)", w => { w.binds = () => ({ state: "unknown" }); }],
  ["bind_unknown", "bind_unknown (a guard without bindSources)", w => { delete (w.guard as { bindSources?: unknown }).bindSources; }],
  ["bind_unknown", "bind_unknown (bindSources throws)", w => { w.binds = () => { throw new Error("boom"); }; }],
  ["bind_unknown", "bind_unknown (a relative source)", w => { w.binds = () => ({ state: "read", sources: ["relative/path"] }); }],
  ["bind_ancestor", "bind_ancestor", w => { w.binds = () => ({ state: "read", sources: [w.base] }); }],
  ["protected_unknown", "protected_unknown", w => { w.protectedRead = () => ({ state: "unknown", reason: "store_entry_cap" }); }],
  ["protected_unknown", "protected_unknown (the reader throws)", w => { w.protectedRead = () => { throw new Error("boom"); }; }],
  ["protected_malformed", "protected_malformed", w => { w.protectedRead = () => ({ state: "malformed", reason: "store_file_malformed" }); }],
  ["not_idle", "not_idle (the idle proof is false)", w => { w.quiesce = async () => false; }],
  ["not_idle", "not_idle (the idle proof throws)", w => { w.quiesce = async () => { throw new Error("boom"); }; }],
  ["not_idle", "not_idle (a process of another uid)", async w => {
    const proc = join(w.base, "proc");
    await fakeProc(proc, [{ pid: 3, uid: 10003 }]);
    w.scan = n => scanScratchProcesses(w.root, n, undefined, proc);
  }],
  ["self_descriptor_unreadable", "self_descriptor_unreadable (unreadable descriptors)", async w => {
    const proc = join(w.base, "proc");
    await fakeProc(proc, [{ pid: 3, fdIsFile: true }]);
    w.scan = n => scanScratchProcesses(w.root, n, undefined, proc);
  }],
  ["census_identity", "census_identity (the census reports another root)", w => {
    w.census = async () => ({ code: 3, stdout: "" }); }],
  ["census_failed", "census_failed", w => { w.census = async () => ({ code: 1, stdout: "" }); }],
  ["census_malformed", "census_malformed", w => { w.census = async () => ({ code: 0, stdout: "not json\n" }); }],
];
for (const [reason, label, tune] of refusals) {
  test("delete-nothing: " + label, async () => {
    await withWorld(["a", "b"], async w => {
      w.discard = async () => assert.fail("must not delete");
      await tune(w);
      const result = await run(w);
      assert.equal(result.scratch.state, "refused", label); assert.equal(result.scratch.reason, reason, label);
      assert.equal(result.scratch.deleted, 0);
      assert.deepEqual(w.calls.discards, []);
      assert.equal(w.calls.refresh, 0);
      if (fs.existsSync(w.root)) await untouched(w, ["a", "b"]);
      // The refusal leaves the pass intact: the attempt phase and the handover are not affected.
      assert.equal(result.verdict, "eligible"); assert.equal(result.authority.result, "attested");
      assert.equal(w.events.at(-1), "handover");
      assert.equal(w.calls.begins, 0);
    });
  });
}

test("delete-nothing: the census is never started when an earlier proof refused", async () => {
  for (const tune of [(w: World) => { w.quiesce = async () => false; }, (w: World) => { w.binds = () => ({ state: "unknown" }); },
    (w: World) => { w.statfs = () => "fs_unsupported"; }, (w: World) => { w.env = {}; }]) {
    await withWorld(["a"], async w => { tune(w); await run(w); assert.deepEqual(w.calls.census, []); });
  }
});

test("delete-nothing: a pass that is not a fresh, attested, eligible one never reaches the scratch phase", async () => {
  const cases: Array<[string, (w: World) => void | Promise<void>]> = [
    ["same-pod restart (consumption existing)", w => { w.options.consumption = { result: "existing", mayEvaluate: false }; }],
    ["consumption full", w => { w.options.consumption = { result: "full", mayEvaluate: false }; }],
    ["no attestation (a handoff document)", w => { w.options.readEvidence = async () => JSON.stringify({ handoff: "none", reason: "x" }); }],
    ["contradicted attestation", async w => { const raw = await fixture("v2/valid/contradicted.json"); w.options.readEvidence = async () => raw; }],
    ["v1 evidence", async w => { const raw = await fixture("valid/positive.json"); w.options.readEvidence = async () => raw; }],
    ["undeclared runtime", w => { w.options.bootstrap = { ...w.options.bootstrap, firstMode: "shared", reason: "runtime_not_declared" }; }],
    ["a partial inventory", w => {
      const discover = w.options.git.discoverEpochRetainedSources;
      w.options.git.discoverEpochRetainedSources = async o => ({ ...await discover(o), inventoryComplete: false, unreadable: 1 });
    }],
    ["a missing native scope", w => { delete (w.options.ownership as { beginReclaim?: unknown }).beginReclaim; }],
    ["a shared lock", w => { w.options.ownership = { ...w.options.ownership, mode: "shared" }; }],
    ["an aborted shutdown", w => { w.parent.abort(); }],
  ];
  for (const [label, tune] of cases) {
    await withWorld(["a", "b"], async w => {
      await tune(w);
      w.discard = async () => assert.fail("must not delete");
      const result = await runPodEpochShadow(w.options);
      noScratchWork(w);
      if (result) assert.equal(result.scratch.state, "not_run", label);
      assert.equal(w.calls.begins, 0, label);
      await untouched(w, ["a", "b"]);
    });
  }
});

test("a daemon veto before the scratch phase never starts it", async () => {
  await withWorld(["a"], async w => {
    w.guardAbort.abort();
    const result = await run(w);
    noScratchWork(w);
    assert.equal(result.scratch.state, "not_run");
  });
});

test("interference: refreshInventory throwing vetoes the pass between leaves; later leaves stay", async () => {
  await withWorld(["a", "b", "c"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2), dirEntry("c", 3)]);
    w.discard = async () => removed();
    let n = 0;
    w.refresh = async () => { if (++n === 2) throw new PodEpochDaemonError("daemon_changed"); };
    const result = await run(w);
    assert.deepEqual(w.calls.discards, ["a"]);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "daemon_changed");
    assert.equal(result.scratch.deleted, 1);
    assert.equal(result.reason, "daemon_changed");
    assert.notEqual(result.verdict, "eligible");
    assert.equal(result.authority.reason, "daemon_veto");
    // The first leaf was joined and settled; the scope finished and the handover ran.
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
});

test("interference: a guard without refreshInventory stops the phase before the first delete", async () => {
  await withWorld(["a"], async w => {
    delete (w.guard as { refreshInventory?: unknown }).refreshInventory;
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => assert.fail("must not delete");
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "daemon_changed");
    assert.equal(result.verdict, "eligible");
    assert.equal(w.calls.begins, 0);
  });
});

test("interference: a leaf changed between the census and its re-census is kept as changed, then the phase continues", async () => {
  await withWorld(["a", "b"], async w => {
    let bump = false;
    w.census = fakeCensus(() => [dirEntry("a", 1, bump ? { bytes: 8192 } : {}), dirEntry("b", 2)]);
    const inner = w.census;
    w.census = async (r, e, leaf, t) => { if (leaf === "a") bump = true; return inner(r, e, leaf, t); };
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(result.scratch.kept.changed, 1);
    assert.deepEqual(w.calls.discards, ["b"]);
    assert.equal(result.scratch.deleted, 1); assert.equal(result.scratch.state, "ran");
  });
});

test("interference (real census): a write between the census and the re-census keeps the leaf", {
  skip: HAS_PROC_FD ? false : "no /proc/self/fd on this host",
}, async () => {
  await withWorld(["a", "b"], async w => {
    const real = censusScratchRoot;
    w.census = async (r, e, leaf, t) => {
      if (leaf === "a") await writeFile(join(w.root, "a", "late.txt"), "late".repeat(2000));
      return real(r, e, leaf, t);
    };
    const result = await run(w);
    assert.equal(result.scratch.kept.changed, 1); assert.equal(result.scratch.deleted, 1);
    await untouched(w, ["a"]);
  });
});

test("interference: an idle proof that fails before a leaf's delete stops the phase; a held leaf at re-scan is kept", async () => {
  await withWorld(["a", "b"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    w.discard = async () => removed();
    let q = 0;
    w.quiesce = async () => ++q < 2;
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "not_idle");
    assert.deepEqual(w.calls.discards, []);
  });
  await withWorld(["a", "b"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    w.discard = async () => removed();
    let scans = 0;
    // The first two scans (pre-census, classification) see nothing; the per-leaf scan of "a" sees it held.
    w.scan = names => ++scans === 3 ? { held: new Set(names) } : { held: new Set<string>() };
    const result = await run(w);
    assert.equal(result.scratch.kept.held_open, 1);
    assert.deepEqual(w.calls.discards, ["b"]);
  });
});

test("interference: the first leaf's mount appearing before its delete keeps it; a refused mount read stops the phase", async () => {
  await withWorld(["a", "b"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    w.discard = async () => removed();
    let reads = 0;
    // Reads: the early topology read, the post-census classification, then one per leaf.
    w.mounts = () => ++reads === 3 ? { mountedLeaves: new Set(["a"]) } : { mountedLeaves: new Set<string>() };
    const result = await run(w);
    assert.equal(result.scratch.kept.mount, 1); assert.deepEqual(w.calls.discards, ["b"]);
  });
  await withWorld(["a", "b"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    w.discard = async () => removed();
    let reads = 0;
    w.mounts = () => ++reads === 3 ? { refused: "mount_unknown" } : { mountedLeaves: new Set<string>() };
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "mount_unknown");
    assert.deepEqual(w.calls.discards, []);
  });
});

test("interference: a soft stop with a leaf in flight joins it and starts no new leaf", async () => {
  await withWorld(["a", "b", "c"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2), dirEntry("c", 3)]);
    let joined = false;
    w.discard = async () => {
      w.parent.abort();
      await new Promise(resolve => setTimeout(resolve, 20));
      joined = true;
      return removed();
    };
    const result = await run(w);
    assert.equal(joined, true);
    assert.deepEqual(w.calls.discards, ["a"]);
    assert.equal(result.scratch.deleted, 1);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "shutdown");
    assert.equal(result.verdict, "partial");
    // The in-flight leaf settled before the scope finished; a shutdown never hands over.
    assert.deepEqual(w.events, ["begin", "finish"]);
  });
});

test("interference: a pass that reaches the cap deletes at most 512 leaves and reports the rest as cap", async () => {
  const names = Array.from({ length: 520 }, (_, i) => "leaf-" + String(i).padStart(4, "0"));
  await withWorld([], async w => {
    w.census = fakeCensus(() => names.map((n, i) => dirEntry(n, i)));
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(result.scratch.eligible, 512); assert.equal(result.scratch.kept.cap, 8);
    assert.equal(w.calls.discards.length, 512); assert.equal(result.scratch.deleted, 512);
    assert.equal(result.scratch.eligible_allocated_bytes_estimate, 512 * 4096);
  });
});

test("the census budget: too little time left refuses with census_budget before any census", async () => {
  await withWorld(["a"], async w => {
    let t = 0;
    w.options.now = () => t;
    // Reads of the clock before the census: the pass start, then the budget. Jump near the soft stop in between.
    w.binds = () => { t = 590_000; return { state: "read", sources: [] }; };
    const result = await run(w);
    assert.equal(result.scratch.state, "refused"); assert.equal(result.scratch.reason, "census_budget");
    assert.deepEqual(w.calls.census, []);
  });
});

test("list overflow of the root listing is reported", async () => {
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)], () => true);
    w.discard = async () => removed();
    assert.equal((await run(w)).scratch.list_overflow, true);
  });
});

// ---- settlement ----------------------------------------------------------------------------------

test("settlement: residue that retained the leaf continues, and the third retained residue stops the phase", async () => {
  await withWorld(["a", "b", "c", "d"], async w => {
    w.census = fakeCensus(() => ["a", "b", "c", "d"].map((n, i) => dirEntry(n, i)));
    w.discard = async () => ({ outcome: "residue", bytesLowerBound: 1000, kind: "retained", stopPhase: false });
    const result = await run(w);
    assert.deepEqual(w.calls.discards, ["a", "b", "c"]);
    assert.equal(result.scratch.residue, 3); assert.equal(result.scratch.deleted, 0);
    assert.equal(result.scratch.deleted_allocated_bytes_estimate, 3000); assert.equal(result.scratch.estimate_partial, true);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "retained_cap");
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
});

for (const mode of ["readable", "unreadable", "missingclose"] as const) {
  test("async no-child reclaim real discard: " + mode, async () => {
    await withWorld(["a", "b"], async w => {
      const census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
      w.census = census;
      const child = Object.assign(new EventEmitter(), {
        pid: undefined, stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
        exitCode: null, signalCode: null,
      });
      const internal = w.git as unknown as { epochNativeTestDependencies: {
        spawn: typeof spawn; alive: () => boolean; kill: () => void; joinMs: number;
      } };
      internal.epochNativeTestDependencies = {
        spawn: (() => {
          setImmediate(() => {
            child.emit("error", Object.assign(new Error("exec refused"), { code: "EAGAIN" }));
            child.stdout.end(); child.stderr.end();
            if (mode !== "missingclose") child.emit("close", -11, null);
          });
          return child as unknown as ChildProcess;
        }) as typeof spawn,
        alive: () => { throw new Error("undefined pid has no group proof"); },
        kill: () => assert.fail("no group to kill"), joinMs: 20,
      };
      let classified = false;
      w.discard = input => w.git.discardEpochScratchLeaf({
        ...input,
        testSeam: { token: DELETE_SEAM, root: w.root, census: async (...args) => {
          classified = true;
          assert.equal(w.scope.pending(), 1);
          assert.throws(() => w.git.assertNoUnsettledDeletes());
          return mode === "unreadable" ? { code: 1, stdout: "" } : census(...args);
        } },
      });
      if (mode === "readable") {
        const result = await run(w);
        assert.equal(result.scratch.state, "stopped");
        assert.equal(result.scratch.reason, "helper_failed");
        assert.equal(result.scratch.helper_failed, 1);
        assert.equal(result.scratch.residue, 1);
        assert.equal(w.scope.pending(), 0);
        assert.equal(w.scope.stopped(), false);
        w.git.assertNoUnsettledDeletes();
        assert.ok(w.events.indexOf("finish") >= 0);
        assert.ok(w.events.indexOf("handover") > w.events.indexOf("finish"));
        assert.equal(w.calls.handovers, 1);
      } else {
        await assert.rejects(run(w), error => error instanceof PodEpochSettlementError);
        assert.equal(w.scope.pending(), 1);
        assert.equal(w.scope.stopped(), true);
        assert.throws(() => w.git.assertNoUnsettledDeletes());
        assert.equal(w.calls.handovers, 0);
        assert.ok(!w.events.includes("finish"));
      }
      assert.equal(classified, mode !== "missingclose");
      assert.deepEqual(w.calls.discards, ["a"], "the sibling never starts");
      await untouched(w, ["a", "b"]);
    });
  });
}

test("settlement: a helper failure or a changed leaf stops the phase after that leaf", async () => {
  for (const kind of ["helper_failed", "changed"] as const) {
    await withWorld(["a", "b"], async w => {
      w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
      w.discard = async () => ({ outcome: "residue", bytesLowerBound: 0, kind, stopPhase: true });
      const result = await run(w);
      assert.deepEqual(w.calls.discards, ["a"]);
      assert.equal(result.scratch.residue, 1); assert.equal(result.scratch.helper_failed, kind === "helper_failed" ? 1 : 0);
      assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, kind);
    });
  }
});

test("settlement: an unresolved delete, or an untagged throw, is fatal: EX kept, no handover", async () => {
  const failures: Array<[string, () => Error]> = [
    ["epochScratchUnresolved", () => Object.assign(new Error("epoch scratch delete unresolved"), { epochScratchUnresolved: true })],
    ["any other throw", () => new Error("boom")],
  ];
  for (const [label, make] of failures) {
    await withWorld(["a", "b"], async w => {
      w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
      // Neither failure stops the scope here, so only the phase's own handling of the throw can make this fatal.
      w.discard = async () => { throw make(); };
      await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError, label);
      assert.deepEqual(w.calls.discards, ["a"], label);
      assert.equal(w.calls.handovers, 0, label);
      assert.ok(!w.events.includes("finish"), label);
    });
  }
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => ({ outcome: "unresolved" } as Settlement);
    await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError);
    assert.equal(w.calls.handovers, 0);
  });
});

test("settlement (real helper): a post-delete census that cannot be read is unresolved and keeps EX", {
  skip: HAS_PROC_FD ? false : "no /proc/self/fd on this host",
}, async () => {
  await withWorld(["a"], async w => {
    w.seamRoot = w.root;
    const options = w.options.scratch!.seams!;
    const seam = { token: DELETE_SEAM, root: w.root, census: async () => ({ code: 1, stdout: "" }) };
    Object.defineProperty(options, "discardSeam", { get: () => seam });
    await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError);
    assert.equal(w.calls.handovers, 0);
    assert.equal(w.scope.stopped(), true);
    assert.equal(w.scope.pending(), 1, "the ticket stays pending");
    assert.throws(() => w.git.assertNoUnsettledDeletes(), /unsettled|timed out|may still/i, "the entry stays");
  });
});

test("settlement: a refusal of the pre-launch observation is the pass's own invalidation, not an unresolved delete", async () => {
  await withWorld(["a", "b"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    w.discard = async input => { w.unhealthy = true; await input.beforeLaunch(); return removed(); };
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "observation_refused");
    assert.equal(result.reason, "daemon_changed");
    assert.equal(result.scratch.deleted, 0);
    assert.deepEqual(w.calls.discards, ["a"]);
  });
});

test("settlement: a throw tagged as never launched is not fatal; the same throw untagged is", async () => {
  // The real delete refuses on a past deadline before any child or unsettled entry exists.
  await withWorld(["a", "b"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    w.discard = async input => w.git.discardEpochScratchLeaf({ ...input, deadline: 0 });
    let outcome: unknown;
    try { outcome = await run(w); } catch (error) { outcome = error; }
    assert.ok(!(outcome instanceof PodEpochSettlementError), "a pre-launch refusal is not an unresolved delete");
    assert.deepEqual(w.calls.discards, ["a"]);
    assert.equal(w.scope.stopped(), false);
    assert.doesNotThrow(() => w.git.assertNoUnsettledDeletes());
    await untouched(w, ["a", "b"]);
  });
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => { throw new Error("epoch scratch delete deadline exhausted"); };
    await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError);
  });
});

test("census: a rejecting initial census refuses the scratch phase and the attempt phase still runs", async () => {
  await withWorld(["a"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    w.options.deletion = attemptDeletion(w, attempt);
    withAttemptInventory(w, () => attemptSource());
    w.census = async () => { throw new Error("census helper timed out"); };
    const result = await run(w);
    assert.equal(result.scratch.state, "refused"); assert.equal(result.scratch.reason, "census_failed");
    assert.deepEqual(w.calls.discards, []);
    assert.equal(result.deleted, 1); assert.equal(attempt.discards, 1);
    await untouched(w, ["a"]);
  });
});

test("census: a rejecting re-census stops the scratch phase and the attempt phase still runs", async () => {
  await withWorld(["a"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    w.options.deletion = attemptDeletion(w, attempt);
    withAttemptInventory(w, () => attemptSource());
    const first = fakeCensus(() => [dirEntry("a", 1)]);
    w.census = async (r, e, leaf, t) => { if (leaf !== undefined) throw new Error("census helper killed"); return first(r, e, leaf, t); };
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "census_failed");
    assert.equal(result.scratch.eligible, 1);
    assert.deepEqual(w.calls.discards, []);
    assert.equal(result.deleted, 1); assert.equal(attempt.discards, 1);
    await untouched(w, ["a"]);
  });
});

/** A fake clock for the pass: `fire` jumps to the hard deadline and runs the due timers (no real-time wait). */
function fakeClock(w: World) {
  let t = 0;
  const timers: Array<{ at: number; cb: () => void; live: boolean }> = [];
  w.options.now = () => t;
  w.options.schedule = (cb, ms) => {
    const entry = { at: t + ms, cb, live: true };
    timers.push(entry);
    return () => { entry.live = false; };
  };
  /** `from` selects only timers due at or after that time (600_000 = the owner's hard deadline, not the soft stop). */
  const fire = (from = 0) => {
    t = 600_000;
    for (;;) {
      const due = timers.find(x => x.live && x.at <= t && x.at >= from);
      if (!due) break;
      due.live = false; due.cb();
    }
  };
  return { fire };
}

test("census: a census that never resolves at the hard deadline is fatal: no attempt delete, no handover", async () => {
  for (const which of ["initial", "re-census"] as const) {
    await withWorld(["a"], async w => {
      const attempt = { discards: 0, unsettled: 0 };
      w.options.deletion = attemptDeletion(w, attempt);
      withAttemptInventory(w, () => attemptSource());
      const clock = fakeClock(w);
      const first = fakeCensus(() => [dirEntry("a", 1)]);
      w.census = (r, e, leaf, t) => {
        if (which === "initial" || leaf !== undefined) {
          setImmediate(() => clock.fire());
          return new Promise(() => undefined);
        }
        return first(r, e, leaf, t);
      };
      w.discard = async () => assert.fail("must not delete");
      await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError, which);
      assert.deepEqual(w.calls.discards, [], which);
      assert.equal(attempt.discards, 0, which);
      assert.equal(w.calls.handovers, 0, which);
      assert.ok(!w.events.includes("handover"), which);
      await untouched(w, ["a"]);
    });
  }
});

test("pre-launch: a refusal tagged as never launched ends the pass as launch_refused; the attempt phase deletes nothing", async () => {
  await withWorld(["a", "b"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    w.options.deletion = attemptDeletion(w, attempt);
    withAttemptInventory(w, () => attemptSource());
    w.census = fakeCensus(() => [dirEntry("a", 1), dirEntry("b", 2)]);
    // The real delete refuses on a past deadline before any child or unsettled entry exists (tagged).
    w.discard = async input => w.git.discardEpochScratchLeaf({ ...input, deadline: 0 });
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "launch_refused");
    assert.equal(result.verdict, "deferred"); assert.equal(result.reason, "read_failed");
    assert.deepEqual(w.calls.discards, ["a"]);
    assert.equal(result.scratch.deleted, 0);
    assert.equal(attempt.discards, 0); assert.equal(result.deleted, 0);
    await untouched(w, ["a", "b"]);
  });
});

test("pre-launch: a slow observation that finishes after the pass went fatal cannot start a delete", async () => {
  await withWorld(["a"], async w => {
    const clock = fakeClock(w);
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    let release!: () => void;
    const gate = new Promise<void>(resolve => { release = resolve; });
    let late: Promise<unknown> | undefined;
    w.discard = input => {
      late = gate.then(() => input.beforeLaunch()).then(() => "launched", (error: unknown) => error);
      // Only the owner's hard deadline passes: the pass goes fatal without the soft stop having aborted it.
      setImmediate(() => clock.fire(600_000));
      return late as Promise<never>;
    };
    await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError);
    release();
    const outcome = await late;
    assert.ok(outcome instanceof Error, "the observation refused instead of letting the delete launch");
    assert.match((outcome as Error).message, /interrupted/);
    assert.equal(w.calls.handovers, 0);
  });
});

test("the scratch phase runs before the attempt deletion phase", async () => {
  await withWorld(["a"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    const deps = attemptDeletion(w, attempt);
    const git = deps.git as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>;
    const attemptDiscard = git.discardStoppedSource!;
    git.discardStoppedSource = async (...args) => { w.events.push("attempt-discard"); return attemptDiscard(...args); };
    w.options.deletion = deps;
    withAttemptInventory(w, () => attemptSource());
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => { w.events.push("scratch-discard"); return removed(); };
    const result = await run(w);
    assert.equal(result.scratch.deleted, 1); assert.equal(result.deleted, 1);
    const order = w.events.filter(e => e.endsWith("-discard"));
    assert.deepEqual(order, ["scratch-discard", "attempt-discard"]);
  });
});

test("settlement: an unsettled runner delete already recorded is fatal before any census", async () => {
  await withWorld(["a"], async w => {
    w.unsettled = true;
    await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError);
    noScratchWork(w);
    assert.equal(w.calls.handovers, 0);
  });
});

test("settlement: the shared scope's finish consults the scratch git for an unsettled delete", async () => {
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => { w.unsettled = true; return removed(); };
    await assert.rejects(runPodEpochShadow(w.options), error => error instanceof PodEpochSettlementError);
    assert.equal(w.calls.handovers, 0);
    assert.ok(!w.events.includes("finish"));
  });
});

test("settlement: the idle proof is run again after the last join; a failure there stops the phase not_idle, not fatal", async () => {
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => removed();
    let q = 0;
    w.quiesce = async () => ++q < 3;
    const result = await run(w);
    assert.equal(q, 3);
    assert.equal(result.scratch.not_idle_after, true); assert.equal(result.scratch.deleted, 1);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "not_idle");
    assert.equal(w.calls.handovers, 1);
  });
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(w.calls.quiesce, 3); assert.equal(result.scratch.not_idle_after, false);
    assert.equal(result.scratch.state, "ran");
  });
});

test("settlement: a failing final idle proof records not_idle_after but keeps an earlier stop reason", async () => {
  const cases: Array<[string, { outcome: "residue"; bytesLowerBound: number; kind: string; stopPhase: boolean }]> = [
    ["helper_failed", { outcome: "residue", bytesLowerBound: 0, kind: "helper_failed", stopPhase: true }],
    ["retained_cap", { outcome: "residue", bytesLowerBound: 1000, kind: "retained", stopPhase: false }],
  ];
  for (const [reason, settlement] of cases) {
    await withWorld(["a", "b", "c", "d"], async w => {
      w.census = fakeCensus(() => ["a", "b", "c", "d"].map((n, i) => dirEntry(n, i + 1)));
      w.discard = async () => settlement as never;
      // Proofs: one initial, one before each delete (1 leaf for helper_failed, 3 for retained_cap), one final.
      // Only the final proof fails.
      const finalProof = reason === "helper_failed" ? 3 : 5;
      let q = 0;
      w.quiesce = async () => ++q < finalProof;
      const result = await run(w);
      assert.equal(result.scratch.not_idle_after, true, reason);
      assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, reason);
    });
  }
});

test("the pool free-bytes delta is reported as an unattributed number, null when unreadable", async () => {
  for (const [free, expected] of [[[900n, 100n], -800], [[100n, 5_000_000_000_000_000_000n], null], [[null, 1n], null]] as const) {
    await withWorld(["a"], async w => {
      w.census = fakeCensus(() => [dirEntry("a", 1)]);
      w.discard = async () => removed();
      const seq = [...free];
      w.free = () => seq.shift() ?? null;
      assert.equal((await run(w)).scratch.statfs_free_delta_unattributed, expected);
    });
  }
});

// ---- gate independence, the shared scope and #3851 ------------------------------------------------

function attemptDeletion(w: World, calls: { discards: number; unsettled: number }): NonNullable<Options["deletion"]> {
  const tx = {};
  let active: object | undefined;
  return {
    enableForTest: ENABLE_DELETION_FOR_TEST,
    authority: {
      withTransaction: async (fn: (t: unknown) => Promise<unknown>) => { active = tx; try { return await fn(tx); } finally { active = undefined; } },
      active: () => active,
      endSourceUse: async (paths: Iterable<string>) => ({ ended: [...paths], kept: [], endedSlots: [] }),
    } as never,
    reserve: (async (_runId: string, action: (eligible: () => boolean) => Promise<unknown>) => action(() => true)) as never,
    quiesce: async () => true,
    git: {
      assertNoUnsettledDeletes: () => { calls.unsettled++; if (w.unsettled) throw new Error("unsettled"); },
      readEpochJournalVector: async () => [JSON.stringify({ attemptId: "attempt", clonePath: "/tree/0", runId: "run0" })],
      createEpochDiscardProof: (input: unknown) => ({ input }) as never,
      discardStoppedSource: async () => { calls.discards++; return { kind: "done", sources: [{ clonePath: "/tree/0", outcome: "removed" }], metadata: "retired", tookOver: true }; },
    },
  };
}
function attemptSource(over: Partial<RetainedSourceEvidence> = {}, partOver: Record<string, unknown> = {}): RetainedSourceEvidence {
  return {
    barePath: "/bare", runId: "run0", branch: "b0", key: "key", kind: "attempt", generation: 1,
    attempt: "attempt", ledgerState: "live", journalState: "none", markerState: "none",
    presence: "present", descriptor: "0".repeat(64), allocatedBytes: 4096, bytesState: "measured",
    sources: [{ clonePath: "/tree/0", attempt: "attempt", generation: 1, presence: "present", ledgerState: "live", live: false,
      allocatedBytes: 4096, ...partOver } as never],
    consumers: [], localRecords: [], holds: { state: "read", ids: [] }, disposition: { kind: "local_discard" }, ...over,
  };
}
function withAttemptInventory(w: World, source: () => RetainedSourceEvidence) {
  const group = { barePath: "/bare", selector: { runId: "run0", generation: 1, attempt: "attempt" }, paths: ["/tree/0"] };
  w.options.git.discoverEpochRetainedSources = async () => ({ groups: [group], inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0 });
  w.options.git.inspectEpochRetainedGroups = perGroupInspect(async opts => {
    for (const p of group.paths) opts.beforeTree!(p);
    return { sources: [source()], partial: false, unreadable: 0, sampledAt: "" };
  });
  w.options.client.listRecoveryHolds = async run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: 0 } });
}

test("gate independence: the attempt override open with the scratch gate closed does no scratch work", async () => {
  await withWorld(["a"], async w => {
    w.gateOpen = false;
    const attempt = { discards: 0, unsettled: 0 };
    w.options.deletion = attemptDeletion(w, attempt);
    withAttemptInventory(w, () => attemptSource());
    const result = await run(w);
    noScratchWork(w);
    assert.equal(result.deleted, 1); assert.equal(attempt.discards, 1);
    assert.equal(result.scratch.state, "not_run");
    await untouched(w, ["a"]);
  });
});

test("gate independence: the attempt deletion deps with the attempt gate closed and the scratch gate open delete no attempt", async () => {
  await withWorld(["a"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    const deps = attemptDeletion(w, attempt);
    delete (deps as { enableForTest?: unknown }).enableForTest;
    w.options.deletion = deps;
    withAttemptInventory(w, () => attemptSource());
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(result.scratch.deleted, 1);
    assert.equal(result.deleted, 0); assert.equal(attempt.discards, 0);
  });
});

test("gate independence: the scratch gate open without attempt deletion deps still reclaims scratch", async () => {
  await withWorld(["a"], async w => {
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => removed();
    assert.equal(w.options.deletion, undefined);
    const result = await run(w);
    assert.equal(result.scratch.deleted, 1); assert.equal(result.deleted, 0);
    assert.equal(result.reason, "shadow_complete");
  });
});

test("one reclaim scope is shared by the scratch and attempt phases and finished once before the handover", async () => {
  await withWorld(["a"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    w.options.deletion = attemptDeletion(w, attempt);
    withAttemptInventory(w, () => attemptSource());
    w.census = fakeCensus(() => [dirEntry("a", 1)]);
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(result.scratch.deleted, 1); assert.equal(result.deleted, 1);
    assert.equal(w.calls.begins, 1);
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
    // Scratch ran before the attempt phase: the attempt discard saw an already-open, settled scope.
    assert.equal(result.reason, "reclaim_complete");
  });
});

test("the attempt phase alone still opens its own scope lazily, and a scratch phase that deletes nothing opens none", async () => {
  await withWorld(["a"], async w => {
    const attempt = { discards: 0, unsettled: 0 };
    w.options.deletion = attemptDeletion(w, attempt);
    withAttemptInventory(w, () => attemptSource());
    w.census = fakeCensus(() => [dirEntry("a", 1, { inc: true })]);
    w.discard = async () => assert.fail("must not delete");
    const result = await run(w);
    assert.equal(result.scratch.kept.incomplete, 1);
    assert.equal(w.calls.begins, 1);
    assert.equal(result.deleted, 1);
  });
});

test("#3851: an unmeasured attempt sibling does not gate scratch, and a byte-incomplete scratch leaf is still kept", async () => {
  await withWorld(["proven", "short"], async w => {
    withAttemptInventory(w, () => attemptSource({ allocatedBytes: null, bytesState: "partial" }, { allocatedBytes: null }));
    w.census = fakeCensus(() => [dirEntry("proven", 1), dirEntry("short", 2, { inc: true })]);
    w.discard = async () => removed();
    const result = await run(w);
    assert.equal(result.verdict, "eligible");
    assert.notEqual(result.bytes_state, "measured", "the attempt sibling is unmeasured");
    assert.equal(result.scratch.state, "ran");
    assert.deepEqual(w.calls.discards, ["proven"]);
    assert.equal(result.scratch.kept.incomplete, 1);
  });
});

// Issue #4143 (observation only): a `.retire-<uuid>` parent is never a scratch leaf the pass may remove. Its name
// fails the leaf-name rule (name_refused), so it is never admitted, whatever the verdict.
const RETIRE_PARENT = ".retire-0b7e1c52-4a64-4f0e-9a43-0c5f0d8a7e11";
async function retireParentTree(w: World): Promise<string[]> {
  const parent = join(w.root, RETIRE_PARENT);
  await mkdir(join(parent, "clone", "sub"), { recursive: true });
  await writeFile(join(parent, "clone", "sub", "f.txt"), "precious".repeat(700));
  return retireParentNow(w);
}
function retireParentNow(w: World): string[] {
  const out: string[] = [];
  const walk = (p: string, rel: string): void => {
    const st = fs.lstatSync(p);
    out.push(`${rel}|${st.mode.toString(8)}|${st.size}|${st.mtimeMs}|${st.isFile() ? fs.readFileSync(p, "utf8").length : ""}`);
    if (st.isDirectory()) for (const n of fs.readdirSync(p).sort()) walk(join(p, n), `${rel}/${n}`);
  };
  walk(join(w.root, RETIRE_PARENT), ".");
  return out;
}

test("#4143: an eligible, attested pass keeps a .retire-<uuid> parent byte-identical and never admits it", {
  skip: HAS_PROC_FD ? false : "no /proc/self/fd on this host: the pinned walk refuses here by design",
}, async () => {
  await withWorld(["ok-1"], async w => {
    const before = await retireParentTree(w);
    const result = await run(w);
    assert.equal(result.verdict, "eligible");
    assert.equal(result.authority.result, "attested");
    assert.equal(result.scratch.state, "ran");
    assert.equal(result.scratch.kept.name_refused, 1, "the retire parent is refused by name");
    assert.equal(result.scratch.eligible, 1, "only the ordinary leaf is admitted");
    assert.ok(!w.calls.discards.some(n => n.startsWith(".retire-")), "never offered to the delete path");
    assert.deepEqual(retireParentNow(w), before);
    await untouched(w, [RETIRE_PARENT]);
  });
});

test("#4143: a partial or deferred pass never touches a .retire-<uuid> parent", async () => {
  const cases: Array<[string, (w: World) => void]> = [
    ["a partial inventory", w => {
      const discover = w.options.git.discoverEpochRetainedSources;
      w.options.git.discoverEpochRetainedSources = async o => ({ ...await discover(o), inventoryComplete: false, unreadable: 1 });
    }],
    ["same-pod restart (consumption existing)", w => { w.options.consumption = { result: "existing", mayEvaluate: false }; }],
  ];
  for (const [label, tune] of cases) {
    await withWorld(["ok-1"], async w => {
      const before = await retireParentTree(w);
      tune(w);
      w.discard = async () => assert.fail("must not delete");
      const result = await runPodEpochShadow(w.options);
      if (result) assert.equal(result.scratch.state, "not_run", label);
      noScratchWork(w);
      assert.deepEqual(retireParentNow(w), before, label);
      await untouched(w, ["ok-1", RETIRE_PARENT]);
    });
  }
});

// ---- Issue #4420 m2: per-run `run-<uuid>` TMPDIR leaves --------------------------------------------------------
// The leaf is deleted only when the server says the run is terminal with zero holds AND every local source of the run
// has positively ended. These cases drive the whole pass: the scratch phase is the real one, the leaf census and the
// delete are the real ones, the run's local state lives in a real GitCache over a real bare repository, and the
// server is a stub. Where a case needs a retained-group shape that the earlier sources would otherwise pre-empt, the
// group inspection (and only that) is stubbed; each such case says so.

const RUN_A = "0a0a0a0a-1111-4222-8333-444444444444";
const RUN_B = "0b0b0b0b-1111-4222-8333-444444444444";
const RUN_BRANCH = "agent/issue-4420";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const leafOf = (id: string) => `run-${id}`;
const FD_ONLY = { skip: HAS_PROC_FD ? false : "no fd directory under the process pseudo-filesystem on this host: the pinned walk refuses here by design" };

interface RunWorld {
  fx: Fixture; git: GitCache; bare: string; outboxRoot: string; outbox: Outbox;
  status: Map<string, string | Error>;
  holds: (runId: string) => Awaited<ReturnType<Options["client"]["listRecoveryHolds"]>>;
  peek: (clonePath: string) => Promise<unknown>;
  /** What the read-only slot enumeration returns. */
  slots: unknown[];
  calls: string[];
  classified: string[];
  dataPath: (...parts: string[]) => string;
  gitIn: (args: string[]) => string;
}

/** Wires a real GitCache (and a real, initialised Outbox) into the world's scratch deps and server stub. */
async function attachRunWorld(w: World): Promise<RunWorld> {
  const fx = makeFixture();
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(join(bare, ".."), 0o700); fs.chmodSync(bare, 0o700);
  const outboxRoot = join(fx.dataDir, "outbox");
  const outbox = new Outbox({ root: outboxRoot, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 4e6, retentionMs: 86_400_000 });
  await outbox.init();
  const rw: RunWorld = {
    fx, git, bare, outboxRoot, outbox, status: new Map(), calls: [], classified: [],
    holds: run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: 0 } }),
    peek: async () => null, slots: [],
    dataPath: (...parts) => join(fx.dataDir, ...parts),
    gitIn: args => execFileSync("git", ["-C", bare, ...args], { env: GIT_ENV, encoding: "utf8" }).trim(),
  };
  w.options.client = {
    getRunOwnership: async runId => {
      rw.calls.push("ownership"); const s = rw.status.get(runId) ?? "completed";
      if (s instanceof Error) throw s;
      return { status: s };
    },
    listRecoveryHolds: async runId => { rw.calls.push("holds"); return rw.holds(runId); },
  };
  const scratch = w.options.scratch!;
  scratch.git = { ...scratch.git, classifyEpochScratchRunRetention: (id, o) => { rw.classified.push(id); return git.classifyEpochScratchRunRetention(id, o); } };
  scratch.authority = {
    peekSlot: clonePath => rw.peek(clonePath) as never,
    peekAllSlots: async () => rw.slots as never,
    inspectMaintenance: runId => new RecoveryMaintenance({ root: join(fx.dataDir, "recovery-maintenance"), workerToken: "fixture" }).inspectRun(runId),
  };
  scratch.outbox = outbox;
  const prior = w.cleanup;
  w.cleanup = async () => { fx.cleanup(); await prior(); };
  return rw;
}

async function withRunWorld(leaves: string[], body: (w: World, rw: RunWorld) => Promise<void>): Promise<void> {
  await withWorld(leaves, async w => { await body(w, await attachRunWorld(w)); });
}

/** The real production path to a settled retired attempt: seed a clone, journal it, retire it with discard. */
async function seedRetiredAttempt(rw: RunWorld, runId: string, retire = true): Promise<string> {
  const attempt = formatAttemptId(new Date("2026-02-01T00:00:00Z"), 1, "000000000000000a");
  const clone = await rw.git.runnerCloneForBranch(rw.bare, RUN_BRANCH, "issue-4420", noProofReseed, runId, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  await rw.git.markRecoveryCapture(rw.bare, clone.path, RUN_BRANCH, runId, attempt);
  if (retire) await rw.git.retireRunnerClone(rw.bare, clone.path, RUN_BRANCH, runId, { discard: true, attemptId: attempt });
  return clone.path;
}

const keptRun = (r: Awaited<ReturnType<typeof run>>) => Object.fromEntries(Object.entries(r.scratch.kept).filter(([k, v]) => k.startsWith("run_") && v > 0));

test("#4420 reclaimed: a terminal zero-hold run with normal settled metadata (retired ledger, missing footprint, cleared journal, idle outbox) has its leaf deleted; a plain leaf follows today's rules", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A), "plain"], async (w, rw) => {
    const clonePath = await seedRetiredAttempt(rw, RUN_A);
    // The facts this fixture rests on, read back from the state the production retire left behind.
    assert.equal(JSON.parse(rw.gitIn(["config", "--get-all", `uzi-attempts.${RUN_BRANCH}.entry`]).split("\n").at(-1)!).state, "retired");
    assert.equal(rw.gitIn(["config", "--local", `uzi-recovery.${RUN_BRANCH}.clone`]), "");
    assert.equal(fs.existsSync(clonePath), false);
    // The run's outbox is driven through the real Outbox: messages appended, drained (acked), a terminal journaled and
    // retired, leaving the manifest-only directory a normally finished run leaves behind.
    await rw.outbox.appendSegment(RUN_A, 1, [{ seq: 1, kind: "text", payload: { text: "hi" } }]);
    assert.equal(rw.outbox.hasUndrainedMessages(RUN_A), true);
    assert.deepEqual(await rw.outbox.drainRun(RUN_A, async () => undefined), { retired: true, staleRetired: 0 });
    assert.deepEqual(await rw.outbox.journalTerminal(RUN_A, 1, "running", 1, { status: "completed" }), { journaled: true, adopted: false });
    assert.equal(rw.outbox.hasPendingTerminal(RUN_A, 1), true);
    assert.equal(await rw.outbox.retireTerminal(RUN_A, 1), true);
    const outboxFiles = fs.readdirSync(join(rw.outboxRoot, RUN_A));
    assert.deepEqual(outboxFiles, ["manifest.json"], "a manifest-only run directory: no segment, terminal-, publication- or finalize- file");
    assert.equal(await rw.outbox.hasPhysicalTerminalProtection(RUN_A), false);
    const result = await run(w);
    assert.equal(result.scratch.state, "ran");
    assert.deepEqual(keptRun(result), {});
    assert.deepEqual(result.scratch.run_local_sources, {});
    assert.deepEqual([...w.calls.discards].sort(), [leafOf(RUN_A), "plain"].sort());
    assert.equal(result.scratch.deleted, 2);
    await untouched(w, []);
    assert.deepEqual(rw.calls, ["ownership", "holds"], "one lifecycle read then one holds read, for the run leaf only");
    assert.deepEqual(rw.classified, [RUN_A]);
  });
});

test("#4420 kept: a broken loose recovery pin under the run's refs (one git lists nothing for) keeps the run leaf", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    await settleOutboxRun(rw, RUN_A);
    const pin = join(rw.bare, "refs", "uzi-recovery-pin", RUN_A, "1");
    fs.mkdirSync(join(pin, ".."), { recursive: true });
    fs.writeFileSync(pin, "garbage\n");
    const result = await run(w);
    assert.equal(result.scratch.state, "ran");
    assert.deepEqual([...w.calls.discards], [], "the leaf is not deleted");
    assert.deepEqual(rw.classified, [RUN_A]);
    assert.equal(result.scratch.deleted, 0);
  });
});

test("#4420 kept: a retired ledger row naming an unrelated missing path (not the bare's canonical attempt clone) keeps the run leaf", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    const clonePath = await seedRetiredAttempt(rw, RUN_A);
    await settleOutboxRun(rw, RUN_A);
    const row = JSON.parse(rw.gitIn(["config", "--get-all", `uzi-attempts.${RUN_BRANCH}.entry`]).split("\n").at(-1)!);
    rw.gitIn(["config", "--local", "--add", `uzi-attempts.${RUN_BRANCH}.entry`,
      JSON.stringify({ ...row, clonePath: join(dirname(dirname(clonePath)), "unrelated", basename(clonePath)) })]);
    const result = await run(w);
    assert.equal(result.scratch.state, "ran");
    assert.deepEqual([...w.calls.discards], [], "the leaf is not deleted");
    assert.equal(result.scratch.deleted, 0);
  });
});

/** Drives the real Outbox to the settled manifest-only run directory a normally finished run leaves behind. */
async function settleOutboxRun(rw: RunWorld, runId: string): Promise<string> {
  await rw.outbox.appendSegment(runId, 1, [{ seq: 1, kind: "text", payload: { text: "hi" } }]);
  assert.deepEqual(await rw.outbox.drainRun(runId, async () => undefined), { retired: true, staleRetired: 0 });
  assert.deepEqual(await rw.outbox.journalTerminal(runId, 1, "running", 1, { status: "completed" }), { journaled: true, adopted: false });
  assert.equal(await rw.outbox.retireTerminal(runId, 1), true);
  assert.deepEqual(fs.readdirSync(join(rw.outboxRoot, runId)), ["manifest.json"]);
  return join(rw.outboxRoot, runId);
}

/** The restart: a NEW Outbox over the same root (no cross-instance lock; the old instance is simply no longer consulted). */
async function restartOutbox(w: World, rw: RunWorld): Promise<Outbox> {
  const next = new Outbox({ root: rw.outboxRoot, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 4e6, retentionMs: 86_400_000 });
  await next.init();
  w.options.scratch!.outbox = next;
  return next;
}

const WRONG_MAC = "0".repeat(64);

async function assertRejectedRecordKeepsLeaf(prepare: (rw: RunWorld, dir: string) => void | Promise<void>, label: string): Promise<void> {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    const dir = await settleOutboxRun(rw, RUN_A);
    await prepare(rw, dir);
    const next = await restartOutbox(w, rw);
    // The behaviour first (so the unfixed code fails on the deletion, not on the accessor), then the accessor.
    const result = await run(w);
    assert.equal(result.scratch.state, "ran", label);
    assert.deepEqual(keptRun(result), { run_local_unknown: 1 }, label);
    assert.deepEqual(result.scratch.run_local_sources, { outbox: 1 }, label);
    assert.deepEqual(w.calls.discards, [], label);
    assert.equal(result.scratch.deleted, 0, label);
    await untouched(w, [leafOf(RUN_A)]);
    assert.equal(next.hasLoadRejectedRecords(RUN_A), true, label);
    assert.equal(next.hasLoadRejectedRecords(RUN_A.toUpperCase()), true, `${label} (case-insensitive)`);
  });
}

test("#4420 restart: a manifest or finalize record that init rejected keeps the run leaf as unknown(outbox), untouched", FD_ONLY, async () => {
  await assertRejectedRecordKeepsLeaf((_rw, dir) => { fs.writeFileSync(join(dir, "manifest.json"), "{not json"); }, "malformed manifest");
  await assertRejectedRecordKeepsLeaf((_rw, dir) => { fs.writeFileSync(join(dir, "finalize-1.json"), "{not json"); }, "malformed finalize");
  await assertRejectedRecordKeepsLeaf((_rw, dir) => { fs.writeFileSync(join(dir, "finalize-01.json"), "{}"); }, "non-canonical finalize name");
  await assertRejectedRecordKeepsLeaf((_rw, dir) => {
    const body = JSON.parse(fs.readFileSync(join(dir, "manifest.json"), "utf8")) as Record<string, unknown>;
    assert.ok("mac" in body, "the fixture's manifest carries a mac field");
    fs.writeFileSync(join(dir, "manifest.json"), JSON.stringify({ ...body, mac: WRONG_MAC }));
  }, "manifest with a wrong mac");
  await assertRejectedRecordKeepsLeaf((_rw, dir) => { fs.writeFileSync(join(dir, "finalize-1.json"), JSON.stringify({ version: 1, mac: WRONG_MAC })); }, "finalize with a wrong mac");
  await assertRejectedRecordKeepsLeaf((_rw, dir) => { fs.rmSync(join(dir, "manifest.json")); fs.mkdirSync(join(dir, "manifest.json")); }, "a directory in place of the manifest");
  // Authentic but cross-run: another run's valid manifest copied into this run's directory.
  await assertRejectedRecordKeepsLeaf(async (rw, dir) => {
    const other = await settleOutboxRun(rw, RUN_B);
    fs.copyFileSync(join(other, "manifest.json"), join(dir, "manifest.json"));
  }, "cross-run manifest");
});

test("#4420 restart: undrained messages kept under an upper-case alias of the run id keep the lowercase run leaf active", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    await rw.outbox.appendSegment(RUN_A.toUpperCase(), 1, [{ seq: 1, kind: "text", payload: { text: "pending" } }]);
    await restartOutbox(w, rw);
    const result = await run(w);
    assert.deepEqual(keptRun(result), { run_local_active: 1 });
    assert.deepEqual(result.scratch.run_local_sources, { outbox: 1 });
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 restart controls: a valid pending record stays active; an absent run directory and a valid settled run reclaim", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    await rw.outbox.appendSegment(RUN_A, 1, [{ seq: 1, kind: "text", payload: { text: "pending" } }]);
    const next = await restartOutbox(w, rw);
    assert.equal(next.hasLoadRejectedRecords(RUN_A), false);
    const result = await run(w);
    assert.deepEqual(keptRun(result), { run_local_active: 1 });
    assert.deepEqual(result.scratch.run_local_sources, { outbox: 1 });
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    assert.equal(fs.existsSync(join(rw.outboxRoot, RUN_A)), false);
    const next = await restartOutbox(w, rw);
    assert.equal(next.hasLoadRejectedRecords(RUN_A), false);
    const result = await run(w);
    assert.deepEqual(keptRun(result), {});
    assert.deepEqual(w.calls.discards, [leafOf(RUN_A)]);
    assert.equal(result.scratch.deleted, 1);
  });
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    await settleOutboxRun(rw, RUN_A);
    const next = await restartOutbox(w, rw);
    assert.equal(next.hasLoadRejectedRecords(RUN_A), false);
    const result = await run(w);
    assert.deepEqual(keptRun(result), {});
    assert.deepEqual(w.calls.discards, [leafOf(RUN_A)]);
    assert.equal(result.scratch.deleted, 1);
  });
});

// Issue #4420: the slot inventory is checked against the authenticated heads, driven through the REAL authority store
// enrolled in the fixture's data dir and the REAL read-only enumeration (only the world's slot stub is replaced).
async function enrollRealAuthority(w: World, rw: RunWorld, admit: boolean): Promise<string> {
  const dataDir = fs.realpathSync(rw.fx.dataDir);
  const workerToken = "unit-reclaim-" + "token-4420";
  const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
  const sourceDir = join(dataDir, "runner", "slug", "issue-4420");
  fs.mkdirSync(sourceDir, { recursive: true });
  const key = await withAuthorityTransaction(lifetime, { timeoutMs: 5000 }, async tx => {
    const store = await openAuthorityStore(tx, { workerToken });
    if (!admit) return "";
    await store.admitSourceUse(tx, sourceDir);
    return store.slotKey(tx, sourceDir);
  });
  w.options.scratch!.authority!.peekAllSlots = (max, bound) => peekAllAuthoritySlots({ dataDir, workerToken }, max, bound);
  return key;
}

test("#4420 authority inventory: an enrolled store that never admitted a slot reclaims the run leaf; an intact admitted (active, unpublished) slot keeps it as unknown", FD_ONLY, async () => {
  for (const admit of [false, true]) {
    await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
      await seedRetiredAttempt(rw, RUN_A);
      await enrollRealAuthority(w, rw, admit);
      const result = await run(w);
      if (!admit) {
        assert.deepEqual(keptRun(result), {}, `admit=${admit}`);
        assert.deepEqual(w.calls.discards, [leafOf(RUN_A)], `admit=${admit}`);
      } else {
        assert.deepEqual(keptRun(result), { run_local_unknown: 1 }, `admit=${admit}`);
        assert.deepEqual(result.scratch.run_local_sources, { authority_inventory: 1 }, `admit=${admit}`);
        assert.deepEqual(w.calls.discards, [], `admit=${admit}`);
      }
    });
  }
});

const SLOT_KEY = "a".repeat(64);
const tombstone = { state: "retired", key: SLOT_KEY, pending: null, retiring: null, publication: null };

test("#4420 authority inventory: an active unpublished slot with no ledger entry keeps the run leaf as unknown", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    rw.slots = [{ state: "active", key: SLOT_KEY, pending: null, retiring: null, publication: null }];
    const result = await run(w);
    assert.deepEqual(keptRun(result), { run_local_unknown: 1 });
    assert.deepEqual(result.scratch.run_local_sources, { authority_inventory: 1 });
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 authority inventory: a retired tombstone attributed through the run's ledger clone path still reclaims the run leaf", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    const clonePath = await seedRetiredAttempt(rw, RUN_A);
    rw.peek = async path => (path === clonePath ? tombstone : null);
    rw.slots = [tombstone];
    const result = await run(w);
    assert.deepEqual(keptRun(result), {});
    assert.deepEqual(w.calls.discards, [leafOf(RUN_A)]);
  });
});

test("#4420 authority inventory: a head-recorded slot whose file or whole slots directory was deleted keeps the run leaf as unknown", FD_ONLY, async () => {
  for (const mode of ["file", "directory"] as const) {
    await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
      await seedRetiredAttempt(rw, RUN_A);
      const key = await enrollRealAuthority(w, rw, true);
      const slots = join(fs.realpathSync(rw.fx.dataDir), ".uzi-authority", "slots");
      if (mode === "file") fs.rmSync(join(slots, `${key}.json`)); else fs.rmSync(slots, { recursive: true, force: true });
      const result = await run(w);
      assert.deepEqual(keptRun(result), { run_local_unknown: 1 }, mode);
      assert.deepEqual(result.scratch.run_local_sources, { authority_inventory: 1 }, mode);
      assert.deepEqual(w.calls.discards, [], mode);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
});

test("#4420 authority inventory: an abort of the pass mid-read interrupts the real inventory, keeps the leaf, and the pass resolves", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    await seedRetiredAttempt(rw, RUN_A);
    await enrollRealAuthority(w, rw, true);
    const real = w.options.scratch!.authority!.peekAllSlots!;
    const outcomes: string[] = [];
    w.options.scratch!.authority!.peekAllSlots = (max, bound) => {
      // Abort while the real enumeration is mid-read (it makes hundreds of separate fs calls).
      setImmediate(() => w.parent.abort());
      return real(max, bound).then(
        slots => { outcomes.push("returned"); return slots; },
        (error: unknown) => { outcomes.push((error as Error).message); throw error; },
      );
    };
    const result = (await runPodEpochShadow(w.options))!;
    assert.deepEqual(outcomes, ["authority read interrupted"], "the real inventory observed the pass's abort between its reads");
    assert.equal(result.scratch.state, "stopped");
    assert.deepEqual(keptRun(result), {});
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 kept run_local_active: the run's recovery record, a journal naming the clone, a live ledger, outbox protection and an in-flight credential switch", FD_ONLY, async () => {
  const cases: Array<[string, string, (rw: RunWorld) => Promise<void>]> = [
    ["custody", "recovery/<runId> record", async rw => {
      fs.mkdirSync(rw.dataPath("recovery", RUN_A), { recursive: true }); fs.writeFileSync(rw.dataPath("recovery", RUN_A, "r.json"), "{}");
    }],
    ["recovery_journal", "a journal still naming the clone (named_by_journal)", async rw => { await seedRetiredAttempt(rw, RUN_A, false); }],
    ["ledger_live", "a live ledger entry", async rw => {
      const attempt = formatAttemptId(new Date("2026-02-02T00:00:00Z"), 1, "000000000000000b");
      rw.gitIn(["config", "--local", "--add", `uzi-attempts.${RUN_BRANCH}.entry`,
        JSON.stringify({ attemptId: attempt, runId: RUN_A, clonePath: rw.dataPath("runner", "slug", `issue-4420.attempt-${attempt}`), state: "live" })]);
    }],
    ["outbox", "a terminal journal file in the run's outbox directory", async rw => {
      fs.mkdirSync(join(rw.outboxRoot, RUN_A), { recursive: true, mode: 0o700 }); fs.writeFileSync(join(rw.outboxRoot, RUN_A, "terminal-1.json"), "{}");
    }],
    ["outbox", "a publication record in the run's outbox directory", async rw => {
      fs.mkdirSync(join(rw.outboxRoot, RUN_A), { recursive: true, mode: 0o700 }); fs.writeFileSync(join(rw.outboxRoot, RUN_A, "publication-1.json"), "{}");
    }],
    ["credential_switch", "a credential-switch op still `sending`", async rw => {
      fs.mkdirSync(rw.dataPath("credential-switch-ops"), { recursive: true });
      fs.writeFileSync(rw.dataPath("credential-switch-ops", `${RUN_A}.json`), JSON.stringify({ ops: { op: { generation: 1, state: "sending" } } }));
    }],
  ];
  for (const [source, label, arrange] of cases) {
    await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
      await arrange(rw);
      const result = await run(w);
      assert.deepEqual(keptRun(result), { run_local_active: 1 }, label);
      assert.deepEqual(result.scratch.run_local_sources, { [source]: 1 }, label);
      assert.deepEqual(w.calls.discards, [], label);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
});

test("#4420 kept via the retained-group evidence (group inspection stubbed, everything else real): owner hold, local record, protected source, ordinary local_discard with a present footprint, refused", FD_ONLY, async () => {
  const evidence = (over: Partial<RetainedSourceEvidence>): RetainedSourceEvidence => ({
    barePath: "", runId: RUN_A, branch: RUN_BRANCH, key: "issue-4420", kind: "attempt", generation: 1, attempt: "a", ledgerState: "retired",
    journalState: "none", markerState: "none", presence: "missing", descriptor: "d", allocatedBytes: 0, bytesState: "measured",
    sources: [{ clonePath: "/x", attempt: "a", generation: 1, presence: "missing", ledgerState: "retired", live: false, allocatedBytes: 0 }],
    consumers: [], localRecords: [], holds: { state: "read", ids: [] }, disposition: undefined, ...over });
  const present = (ledgerState: "retired" | "reclaimed") =>
    [{ clonePath: "/x", attempt: "a", generation: 1, presence: "present" as const, ledgerState, live: false, allocatedBytes: 1 }];
  const cases: Array<[string, RetainedSourceEvidence]> = [
    ["owner_hold", evidence({ disposition: { kind: "owner_hold_discard", holdIds: ["h"] } })],
    ["local_record", evidence({ disposition: { kind: "resolve_local_record", records: [] } })],
    ["protected_source", evidence({ kind: "protected", disposition: { kind: "local_protected_discard" }, presence: "present", sources: present("retired") })],
    ["attempt_footprint", evidence({ presence: "present", disposition: { kind: "local_discard" }, sources: present("retired") })],
    ["retained_refused", evidence({ disposition: { kind: "refused", consumers: ["named_by_journal:active"] } })],
  ];
  for (const [source, ev] of cases) {
    await withRunWorld([leafOf(RUN_A), "plain"], async (w, rw) => {
      const group = { barePath: rw.bare, selector: { runId: RUN_A, generation: 1 as const, attempt: "a" }, paths: ["/x"] };
      rw.git.discoverEpochRetainedSources = async () => ({ groups: [group], inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0 });
      rw.git.inspectEpochRetainedGroups = async () => [{ sources: [ev], partial: false, unreadable: 0, sampledAt: "" } satisfies RetainedInspectResult];
      const result = await run(w);
      assert.deepEqual(keptRun(result), { run_local_active: 1 }, source);
      assert.deepEqual(result.scratch.run_local_sources, { [source]: 1 }, source);
      assert.deepEqual(w.calls.discards, ["plain"], source);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
});

test("#4420 a local retained source whose recorded path is outside TMPDIR still keeps the leaf", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
    // The footprint lives under the data dir's runner tree, nowhere near the scratch root; retention is by run, not path.
    const clonePath = await seedRetiredAttempt(rw, RUN_A);
    fs.mkdirSync(clonePath);
    assert.ok(!clonePath.startsWith(w.root));
    const result = await run(w);
    assert.deepEqual(keptRun(result), { run_local_active: 1 });
    assert.deepEqual(result.scratch.run_local_sources, { attempt_footprint: 1 });
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 an unreadable local store is run_local_unknown for run leaves while a plain leaf in the same pass is deleted as before", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A), leafOf(RUN_B), "plain"], async (w, rw) => {
    fs.writeFileSync(rw.dataPath("repos", "stray.git"), "a file where a bare repository belongs");
    const result = await run(w);
    assert.deepEqual(keptRun(result), { run_local_unknown: 2 });
    assert.deepEqual(result.scratch.run_local_sources, { classifier: 2 });
    assert.deepEqual(w.calls.discards, ["plain"]);
    assert.equal(result.scratch.deleted, 1);
    await untouched(w, [leafOf(RUN_A), leafOf(RUN_B)]);
  });
});

test("#4420 an outbox that is missing, disabled or throwing keeps the leaf as run_local_unknown(outbox); an outbox with a record of the run keeps it active", FD_ONLY, async () => {
  const stub = (over: Record<string, unknown>) => ({
    isDisabled: () => false, unusableRootReason: () => undefined, hasPhysicalTerminalProtection: async () => false, hasLoadRejectedRecords: () => false, runsWithPending: () => [],
    // listPendingFinalizes (the register's view, capped at 256 entries) is deliberately a truncated empty list here.
    listPendingFinalizes: () => [], listPendingFinalizeGenerations: (_includeTerminal?: boolean) => [], listPublicationRecords: () => [], depthFor: () => undefined, uncleanRuns: () => [], ...over }) as never;
  const unknownCases: Array<[string, unknown]> = [
    ["no outbox dependency", undefined],
    ["disabled", stub({ isDisabled: () => true })],
    ["unusable root", stub({ unusableRootReason: () => "symlink" })],
    ["a listing that throws", stub({ listPendingFinalizeGenerations: () => { throw new Error("boom"); } })],
  ];
  for (const [label, outbox] of unknownCases) {
    await withRunWorld([leafOf(RUN_A)], async (w, _rw) => {
      if (outbox === undefined) delete w.options.scratch!.outbox; else w.options.scratch!.outbox = outbox as never;
      const result = await run(w);
      assert.deepEqual(keptRun(result), { run_local_unknown: 1 }, label);
      assert.deepEqual(result.scratch.run_local_sources, { outbox: 1 }, label);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
  const depth = (pendingMessages: number, pendingTerminal: number) =>
    (id: string) => id === RUN_A ? { runId: id, pendingMessages, pendingTerminal, staleRetired: 0, since: 0 } : undefined;
  const activeCases: Array<[string, unknown]> = [
    ["a pending finalize (case-insensitive)", stub({ listPendingFinalizeGenerations: () => [{ run_id: RUN_A.toUpperCase(), claim_generation: 1 }] })],
    ["a pending finalize beyond the register's 256-entry truncation", stub({
      listPendingFinalizeGenerations: () => [
        ...Array.from({ length: 300 }, (_, n) => ({ run_id: `other-${n}`, claim_generation: 1 })),
        { run_id: RUN_A, claim_generation: 2 }],
    })],
    ["a pending finalize visible only in the complete (includeTerminal) listing", stub({
      listPendingFinalizeGenerations: (includeTerminal?: boolean) => includeTerminal ? [{ run_id: RUN_A, claim_generation: 1 }] : [],
    })],
    ["a publication record", stub({ listPublicationRecords: () => [{ run_id: RUN_A, claim_generation: 1 }] })],
    ["pending messages", stub({ depthFor: depth(2, 0) })],
    ["a pending terminal", stub({ depthFor: depth(0, 1) })],
    ["a spill-unclean flag", stub({ uncleanRuns: () => [RUN_A] })],
  ];
  for (const [label, outbox] of activeCases) {
    await withRunWorld([leafOf(RUN_A)], async (w, _rw) => {
      w.options.scratch!.outbox = outbox as never;
      const result = await run(w);
      assert.deepEqual(keptRun(result), { run_local_active: 1 }, label);
      assert.deepEqual(result.scratch.run_local_sources, { outbox: 1 }, label);
    });
  }
  // A tracked run whose manifest holds nothing pending is not a keep.
  await withRunWorld([leafOf(RUN_A)], async (w, _rw) => {
    w.options.scratch!.outbox = stub({ depthFor: depth(0, 0) });
    await run(w);
    assert.deepEqual(w.calls.discards, [leafOf(RUN_A)]);
  });
});

test("#4420 server lifecycle: a non-terminal status keeps run_active without any local read", FD_ONLY, async () => {
  for (const status of ["running", "paused", "awaiting_input", "queued"]) {
    await withRunWorld([leafOf(RUN_A), "plain"], async (w, rw) => {
      rw.status.set(RUN_A, status);
      const result = await run(w);
      assert.deepEqual(keptRun(result), { run_active: 1 }, status);
      assert.deepEqual(rw.calls, ["ownership"], "no holds read for a run that has not ended");
      assert.deepEqual(rw.classified, []);
      assert.deepEqual(w.calls.discards, ["plain"]);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
});

test("#4420 server lifecycle: an exact ownership 404 is run_unknown; any other failure or malformed answer is run_unreadable", FD_ONLY, async () => {
  const answers: Array<[string, () => Response, string]> = [
    ["exact 404", () => new Response(JSON.stringify({ error: "run not found for this worker" }), { status: 404 }), "run_unknown"],
    ["404 with another body", () => new Response(JSON.stringify({ error: "not found" }), { status: 404 }), "run_unreadable"],
    ["500", () => new Response("boom", { status: 500 }), "run_unreadable"],
    ["malformed body", () => new Response(JSON.stringify({ nope: true })), "run_unreadable"],
  ];
  for (const [label, answer, reason] of answers) {
    await withRunWorld([leafOf(RUN_A), "plain"], async (w, rw) => {
      w.options.client = new WorkerClient("http://example.invalid", "inert", "test", nullLogger(), {
        fetch: async url => {
          const p = new URL(String(url)).pathname;
          if (p.endsWith("/ownership")) return answer();
          const run_id = p.split("/").at(-2)!;
          return new Response(JSON.stringify({ run_id, holds: [], run_open_holds: { run_id, count: 0 } }));
        },
      });
      const result = await run(w);
      assert.deepEqual(keptRun(result), { [reason]: 1 }, label);
      assert.deepEqual(rw.classified, [], label);
      assert.deepEqual(w.calls.discards, ["plain"], label);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
});

test("#4420 server holds: an open hold, a run-wide open count, a missing run_open_holds and a failed read each keep the leaf", FD_ONLY, async () => {
  const cases: Array<[string, RunWorld["holds"], string]> = [
    ["a hold on this worker", run_id => ({ run_id, holds: [{ hold_id: "h", generation: 1 } as never], run_open_holds: { run_id, count: 1 } }), "run_retained"],
    ["a run-wide open count with no local hold", run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: 2 } }), "run_retained"],
    ["no run_open_holds (unknown, never zero)", run_id => ({ run_id, holds: [] }), "run_unreadable"],
    ["run_open_holds for another run", run_id => ({ run_id, holds: [], run_open_holds: { run_id: "other", count: 0 } }), "run_unreadable"],
    ["a response for another run", () => ({ run_id: "other", holds: [], run_open_holds: { run_id: "other", count: 0 } }), "run_unreadable"],
    ["a negative run-wide count", run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: -1 } }), "run_unreadable"],
    ["a fractional run-wide count", run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: 1.5 } }), "run_unreadable"],
    ["a non-number run-wide count", run_id => ({ run_id, holds: [], run_open_holds: { run_id, count: "0" as never } }), "run_unreadable"],
    ["a read that throws", () => { throw new Error("holds unreachable"); }, "run_unreadable"],
  ];
  for (const [label, holds, reason] of cases) {
    await withRunWorld([leafOf(RUN_A)], async (w, rw) => {
      rw.holds = holds;
      const result = await run(w);
      assert.deepEqual(keptRun(result), { [reason]: 1 }, label);
      assert.deepEqual(rw.classified, [], label);
      assert.deepEqual(w.calls.discards, [], label);
      await untouched(w, [leafOf(RUN_A)]);
    });
  }
});

test("#4420 an aborted pass is not swallowed as a keep: an abort during the lifecycle read stops the phase", FD_ONLY, async () => {
  // Run leaves are processed after the plain ones, so "zplain" is already deleted when the run leaf's read aborts.
  await withRunWorld([leafOf(RUN_A), "zplain"], async (w, rw) => {
    w.options.client.getRunOwnership = async () => { rw.calls.push("ownership"); w.parent.abort(); throw new Error("aborted mid-read"); };
    const result = (await runPodEpochShadow(w.options))!;
    assert.equal(result.scratch.state, "stopped");
    assert.deepEqual(w.calls.discards, ["zplain"]);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

/** A stepped fake clock: `advance` moves time and runs the timers that fell due (no real-time wait). */
function steppedClock(w: World) {
  let t = 0;
  const timers: Array<{ at: number; cb: () => void; live: boolean }> = [];
  w.options.now = () => t;
  w.options.schedule = (cb, ms) => {
    const entry = { at: t + ms, cb, live: true };
    timers.push(entry);
    return () => { entry.live = false; };
  };
  return {
    advance: (ms: number) => {
      t += ms;
      for (;;) {
        const due = timers.find(x => x.live && x.at <= t);
        if (!due) break;
        due.live = false; due.cb();
      }
    },
  };
}
const hangUntilAborted = (signal: AbortSignal | undefined) => new Promise<never>((_resolve, reject) => {
  signal?.addEventListener("abort", () => reject(signal.reason), { once: true });
});

test("#4420 hung ownership reads: plain leaves are processed first and each hung run leaf is run_unreadable after its own bound", FD_ONLY, async () => {
  const runs = [RUN_A, RUN_B, "0c0c0c0c-1111-4222-8333-444444444444"];
  await withRunWorld([...runs.map(leafOf), "tmp.aaa", "tmp.bbb"], async (w, rw) => {
    const clock = steppedClock(w);
    const seenAtFirstRead: string[][] = [];
    w.options.client.getRunOwnership = async (_id, options) => {
      rw.calls.push("ownership");
      seenAtFirstRead.push([...w.calls.discards]);
      setImmediate(() => clock.advance(RUN_LEAF_READ_MAX_MS));
      return hangUntilAborted(options.signal);
    };
    const result = await run(w);
    assert.equal(result.scratch.state, "ran");
    assert.deepEqual(seenAtFirstRead[0], ["tmp.aaa", "tmp.bbb"], "both plain leaves were deleted before any run leaf read began");
    assert.equal(rw.calls.filter(c => c === "ownership").length, runs.length, "every hung read ended on its own bound and the pass went on");
    assert.deepEqual(keptRun(result), { run_unreadable: runs.length });
    assert.deepEqual(w.calls.discards, ["tmp.aaa", "tmp.bbb"]);
    await untouched(w, runs.map(leafOf));
  });
});

test("#4420 a hung holds read is run_unreadable after its own bound and does not stop the pass", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A), "tmp.aaa"], async (w, rw) => {
    const clock = steppedClock(w);
    w.options.client.listRecoveryHolds = async (_id, signal) => {
      rw.calls.push("holds");
      setImmediate(() => clock.advance(RUN_LEAF_READ_MAX_MS));
      return hangUntilAborted(signal);
    };
    const result = await run(w);
    assert.equal(result.scratch.state, "ran");
    assert.deepEqual(keptRun(result), { run_unreadable: 1 });
    assert.deepEqual(w.calls.discards, ["tmp.aaa"]);
    assert.deepEqual(rw.classified, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 an abort of the pass during a hung run-leaf read still propagates as a stop, not a keep", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A), leafOf(RUN_B)], async (w, rw) => {
    w.options.client.getRunOwnership = async (_id, options) => {
      rw.calls.push("ownership");
      setImmediate(() => w.parent.abort());
      return hangUntilAborted(options.signal);
    };
    const result = (await runPodEpochShadow(w.options))!;
    assert.equal(result.scratch.state, "stopped");
    assert.equal(rw.calls.filter(c => c === "ownership").length, 1, "no further run leaf is read after the abort");
    assert.deepEqual(keptRun(result), {});
    await untouched(w, [leafOf(RUN_A), leafOf(RUN_B)]);
  });
});

// ---- Issue #4420 m3: admission, restart and cap coverage for run leaves, driven through the pass ---------------------
// These cases use the fake-census world (no real git), so each leaf's census facts are explicit. The server stub counts
// every read so "no server read" is an assertion, not an assumption.

const RUN_C = "0c0c0c0c-1111-4222-8333-444444444444";
const RUN_D = "0d0d0d0d-1111-4222-8333-444444444444";
const RUN_E = "0e0e0e0e-1111-4222-8333-444444444444";

function countingServer(w: World, status: (runId: string) => string = () => "completed", holdsOpen = 0) {
  const seen = { ownership: [] as string[], holds: [] as string[] };
  w.options.client = {
    getRunOwnership: async runId => { seen.ownership.push(runId); return { status: status(runId) }; },
    listRecoveryHolds: async runId => {
      seen.holds.push(runId);
      return { run_id: runId, holds: [], run_open_holds: { run_id: runId, count: holdsOpen } };
    },
  };
  return seen;
}

test("#4420 every non-terminal status, parked ones included, keeps the run leaf run_active with no holds read", async () => {
  const statuses = ["queued", "claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup",
    "limit_wait", "pool_wait", "paused", "recovery_wait"];
  for (const status of statuses) {
    await withWorld([leafOf(RUN_A), "plain"], async w => {
      w.census = fakeCensus(() => [dirEntry(leafOf(RUN_A), 1), dirEntry("plain", 2)]);
      w.discard = async () => removed();
      const seen = countingServer(w, () => status);
      const result = await run(w);
      assert.equal(result.scratch.kept.run_active, 1, status);
      assert.deepEqual(seen.ownership, [RUN_A], status);
      assert.deepEqual(seen.holds, [], status);
      assert.deepEqual(w.calls.discards, ["plain"], status);
      await untouched(w, [leafOf(RUN_A), "plain"]);
    });
  }
});

test("#4420 restart: a previous-pod run leaf (older than the pod-entry fence) of a paused run, or of a terminal run with an open hold, survives", async () => {
  const cases: Array<[string, string, number, "run_active" | "run_retained"]> = [
    ["paused", "paused", 0, "run_active"],
    ["recovery-retained (terminal, one open hold)", "failed", 1, "run_retained"],
  ];
  for (const [label, status, open, reason] of cases) {
    await withWorld([leafOf(RUN_A), "plain"], async w => {
      // ctime 1 against a fence far in the future: the leaf belongs to a previous pod, so current_pod does not shield it.
      w.fence = () => ({ t0Ms: 1_000_000 });
      w.census = fakeCensus(() => [dirEntry(leafOf(RUN_A), 1, { ctime: 1 }), dirEntry("plain", 2, { ctime: 1 })]);
      w.discard = async () => removed();
      countingServer(w, () => status, open);
      const result = await run(w);
      assert.equal(result.scratch.kept.current_pod, 0, label);
      assert.equal(result.scratch.kept[reason], 1, label);
      assert.deepEqual(w.calls.discards, ["plain"], label);
      await untouched(w, [leafOf(RUN_A), "plain"]);
    });
  }
});

test("#4420 restart (real worker-local state): a previous-pod run leaf of a terminal run with a local recovery record survives", FD_ONLY, async () => {
  await withRunWorld([leafOf(RUN_A), "plain"], async (w, rw) => {
    fs.mkdirSync(rw.dataPath("recovery", RUN_A), { recursive: true }); fs.writeFileSync(rw.dataPath("recovery", RUN_A, "r.json"), "{}");
    const result = await run(w);
    assert.equal(result.scratch.kept.current_pod, 0);
    assert.deepEqual(keptRun(result), { run_local_active: 1 });
    assert.deepEqual(w.calls.discards, ["plain"]);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 a run leaf that changed between the census and its re-census is kept changed without any server read", async () => {
  await withWorld([leafOf(RUN_A), "plain"], async w => {
    let bump = false;
    const inner = fakeCensus(() => [dirEntry(leafOf(RUN_A), 1, bump ? { bytes: 8192 } : {}), dirEntry("plain", 2)]);
    w.census = async (r, e, leaf, t) => { if (leaf === leafOf(RUN_A)) bump = true; return inner(r, e, leaf, t); };
    w.discard = async () => removed();
    const seen = countingServer(w);
    const result = await run(w);
    assert.equal(result.scratch.kept.changed, 1);
    assert.deepEqual(seen, { ownership: [], holds: [] });
    assert.deepEqual(w.calls.discards, ["plain"]);
    await untouched(w, [leafOf(RUN_A), "plain"]);
  });
});

test("#4420 one run's status flipping from terminal to paused after the census is honoured: the per-leaf read sees paused and the leaf is kept run_active", async () => {
  await withWorld([leafOf(RUN_A), "plain"], async w => {
    let status = "completed";
    const inner = fakeCensus(() => [dirEntry(leafOf(RUN_A), 1), dirEntry("plain", 2)]);
    let flipped = false;
    // The census of the run leaf is the last step before its server read; flip the status once the census has run.
    w.census = async (r, e, leaf, t) => {
      const out = await inner(r, e, leaf, t);
      if (leaf === leafOf(RUN_A) && !flipped) { flipped = true; status = "paused"; }
      return out;
    };
    w.discard = async () => removed();
    const seen = countingServer(w, () => status);
    const result = await run(w);
    assert.equal(flipped, true);
    assert.equal(result.scratch.kept.run_active, 1, "paused at the read: kept, not deleted");
    assert.deepEqual(seen.ownership, [RUN_A]);
    assert.deepEqual(w.calls.discards, ["plain"]);
    await untouched(w, [leafOf(RUN_A), "plain"]);
  });
});

test("#4420 documented window: a status read as terminal at the per-leaf read is read exactly once and not re-read before launch", async () => {
  await withWorld([leafOf(RUN_A)], async w => {
    // Terminal is absorbing (api run-status UPDATEs never leave completed/failed/cancelled, audited in m2), so one
    // terminal read cannot later become non-terminal and a second read before launch would add nothing.
    const seen = endedRunWorld(w);
    const result = await run(w);
    assert.deepEqual(seen.ownership, [RUN_A], "exactly one ownership read for the leaf");
    assert.equal(result.scratch.deleted, 1);
  });
});

/** A server stub, a classifier and an outbox that all say the run has ended, so only the delete-time checks remain. */
function endedRunWorld(w: World) {
  const seen = countingServer(w);
  w.options.scratch!.git = { ...w.options.scratch!.git, classifyEpochScratchRunRetention: async () => ({ state: "ended" }) } as never;
  w.options.scratch!.outbox = {
    isDisabled: () => false, unusableRootReason: () => undefined, hasPhysicalTerminalProtection: async () => false, hasLoadRejectedRecords: () => false, runsWithPending: () => [],
    listPendingFinalizes: () => [], listPendingFinalizeGenerations: () => [], listPublicationRecords: () => [],
    depthFor: (id: string) => ({ runId: id, pendingMessages: 0, pendingTerminal: 0, staleRetired: 0, since: 0 }), uncleanRuns: () => [],
  } as never;
  w.census = fakeCensus(() => [dirEntry(leafOf(RUN_A), 1)]);
  w.discard = async () => removed();
  return seen;
}

test("#4420 an ended run leaf is deleted, but a leaf that becomes held open after the run-leaf reads, or whose idle proof fails, is kept by the existing checks", async () => {
  await withWorld([leafOf(RUN_A)], async w => {
    const seen = endedRunWorld(w);
    const result = await run(w);
    assert.deepEqual(seen, { ownership: [RUN_A], holds: [RUN_A] });
    assert.equal(result.scratch.deleted, 1); assert.deepEqual(w.calls.discards, [leafOf(RUN_A)]);
  });
  await withWorld([leafOf(RUN_A)], async w => {
    const seen = endedRunWorld(w);
    // Scan 1 is the pre-census probe, scan 2 the census-time scan, scan 3 the per-leaf scan after the run-leaf reads.
    let scans = 0;
    w.scan = () => ({ held: new Set(++scans >= 3 ? [leafOf(RUN_A)] : []) });
    const result = await run(w);
    assert.deepEqual(seen, { ownership: [RUN_A], holds: [RUN_A] });
    assert.equal(result.scratch.kept.held_open, 1); assert.equal(result.scratch.deleted, 0);
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
  await withWorld([leafOf(RUN_A)], async w => {
    endedRunWorld(w);
    let q = 0;
    w.quiesce = async () => ++q < 2;
    const result = await run(w);
    assert.equal(result.scratch.state, "stopped"); assert.equal(result.scratch.reason, "not_idle");
    assert.deepEqual(w.calls.discards, []);
    await untouched(w, [leafOf(RUN_A)]);
  });
});

test("#4420 a run leaf over the per-leaf census cap is incomplete and never reaches a server read", FD_ONLY, async () => {
  await withWorld([leafOf(RUN_A), "plain"], async w => {
    // makeWorld seeds <leaf>/sub/f.txt (two entries); drop the plain leaf's file so only the run leaf is over a cap of 1.
    await rm(join(w.root, "plain", "sub", "f.txt"));
    w.census = (root, expect, leaf, budget) => runAgentHelper(runnerCommand, SCRATCH_CENSUS_SCRIPT,
      [root, expect, leaf ?? "-", "4096", "1", "2000000", "256", String(Math.max(0, Math.floor(budget) - 2000))], budget);
    w.discard = async () => removed();
    const seen = countingServer(w);
    const result = await run(w);
    assert.equal(result.scratch.kept.incomplete, 1);
    assert.deepEqual(seen, { ownership: [], holds: [] });
    assert.deepEqual(w.calls.discards, ["plain"]);
    await untouched(w, [leafOf(RUN_A), "plain"]);
  });
});

test("#4420 a non-directory (symlink-shaped census entry), mounted, foreign-owned or held-open run leaf is kept by the existing rules before any server read", async () => {
  const link = leafOf(RUN_A), mounted = leafOf(RUN_B), foreign = leafOf(RUN_C), held = leafOf(RUN_D), foreignFs = leafOf(RUN_E);
  await withWorld([link, mounted, foreign, held, foreignFs, "plain"], async w => {
    w.mounts = () => ({ mountedLeaves: new Set([mounted]) });
    w.scan = () => ({ held: new Set([held]) });
    w.discard = async () => removed();
    w.census = fakeCensus(() => [dirEntry(link, 1, { type: "other" }), dirEntry(mounted, 2), dirEntry(foreign, 3, { fuid: true }),
      dirEntry(held, 4), dirEntry(foreignFs, 5, { fsdiff: true }), dirEntry("plain", 6)]);
    const seen = countingServer(w);
    const result = await run(w);
    const k = result.scratch.kept;
    assert.equal(k.not_directory, 1); assert.equal(k.mount, 2); assert.equal(k.foreign_owner, 1); assert.equal(k.held_open, 1);
    assert.deepEqual(seen, { ownership: [], holds: [] }, "none of them is read from the server");
    assert.deepEqual(w.calls.discards, ["plain"]);
  });
});
