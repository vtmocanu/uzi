// Issue #4043 M3: the runner-scratch phase of runPodEpochShadow, driven through the pass with a verified
// fixture epoch, a fake daemon guard and a temp scratch root. The census and delete helpers are the real ones
// where a case needs real bytes; settlement and interference cases use fake census text and a fake discard.
import assert from "node:assert/strict";
import { test } from "node:test";
import fs from "node:fs";
import { chmod, mkdir, mkdtemp, readFile, readdir, realpath, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { runPodEpochShadow, PodEpochSettlementError, EPOCH_SCRATCH_DELETION_ENABLED } from "../src/pod-epoch-reclaim.js";
import { PodEpochDaemonError, type PodEpochDaemonGuard } from "../src/pod-epoch-daemon.js";
import { GitCache, type EpochScratchDiscardInput, type RetainedSourceEvidence } from "../src/git.js";
import { censusScratchRoot } from "../src/rmtree.js";
import { scanScratchProcesses, type Settlement } from "../src/pod-epoch-scratch.js";
import { WORKER_UID } from "../src/runner-uid.js";
import { buildPodEpochScratchDeps } from "../src/main.js";
import { nullLogger } from "./helpers.js";
import { ENABLE_DELETION_FOR_TEST, ENABLE_SCRATCH_FOR_TEST } from "./pod-epoch-reclaim-support.js";
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
      inspectRetainedSources: async () => { throw new Error("unexpected inspection"); },
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

test("the production scratch gate is closed, separate from the attempt gate, and nothing under agent/src opens it", async () => {
  assert.equal(EPOCH_SCRATCH_DELETION_ENABLED, false);
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
  assert.equal(definition.match(/export const EPOCH_SCRATCH_DELETION_ENABLED: boolean = false;/g)?.length, 1);
  assert.ok(!definition.includes("EPOCH_RECLAIM_DELETION_ENABLED"), "the attempt phase uses the #4134 startup activation, not a constant");
  const built = buildPodEpochScratchDeps({ git: {} as never, dockerHost: undefined });
  assert.deepEqual(Object.keys(built).sort(), ["git", "quiesce"]);
});

test("gate closed: a pass with scratch deps does no census, no child, no new path read and reports not_run", async () => {
  await withWorld(["a", "b"], async w => {
    w.gateOpen = false;
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
      incomplete: 0, current_pod: 0, protected_record: 1, bind_referenced: 1, held_open: 1, cap: 0, changed: 0 });
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
      mount: 1, incomplete: 0, current_pod: 1, protected_record: 0, bind_referenced: 0, held_open: 1, cap: 0, changed: 0 });
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
      incomplete: 0, current_pod: 1, protected_record: 0, bind_referenced: 1, held_open: 0, cap: 0, changed: 0 });
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
  w.options.git.inspectRetainedSources = async opts => {
    for (const p of group.paths) opts.beforeTree!(p);
    return { sources: [source()], partial: false, unreadable: 0, sampledAt: "" };
  };
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
