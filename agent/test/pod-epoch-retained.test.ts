
import { afterEach, beforeEach, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync, spawn } from "node:child_process";
import { MaintenanceResources } from "../src/recovery-maintenance.js";
import { GitCache, type EpochRetainedLimits } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
let fixture: Fixture;
let cache: GitCache;
let bare: string;
function git(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env, encoding: "utf8", stdio: "pipe" }).trim();
}
beforeEach(async () => {
  fixture = makeFixture();
  cache = new GitCache(fixture.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fixture.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
});
afterEach(() => { mock.restoreAll(); fixture.cleanup(); });

/** Real bare metadata and production attempt identities; absent paths still consume capacity. */
function attempts(count: number, target = bare, offset = 0,
  state: "abandoned" | "retired" | "live" = "abandoned", key = "issue-3430", branch = "agent/issue-3430") {
  const entries = [];
  for (let i = offset; i < offset + count; i++) {
    const attemptId = formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, i)), 1, (i + 1).toString(16).padStart(16, "0"));
    const clonePath = cache.runnerClonePath(target, key) + ".attempt-" + attemptId;
    const entry = { attemptId, runId: "epoch-run", clonePath, state };
    git(target, ["config", "--local", "--add", `uzi-attempts.${branch}.entry`, JSON.stringify(entry)]);
    entries.push(entry);
  }
  return entries;
}

it("completed retired pairs do not exhaust the epoch path cap before one live attempt", async () => {
  // The epoch proof requires canonical ancestors; macOS os.tmpdir() may spell /var through a symlink.
  const dataDir = fs.realpathSync(fixture.dataDir);
  bare = fs.realpathSync(bare);
  cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
  const completed = attempts(201, bare, 0, "retired");
  fs.mkdirSync(path.dirname(completed[0]!.clonePath), { recursive: true });
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone", ""]);
  const [live] = attempts(1, bare, 201, "live", "issue-live", "agent/issue-live");
  fs.mkdirSync(live!.clonePath, { recursive: true });
  fs.writeFileSync(path.join(live!.clonePath, "payload"), "retained");
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-live.clone",
    JSON.stringify({ runId: live!.runId, clonePath: live!.clonePath, attemptId: live!.attemptId })]);
  assert.deepEqual(await cache.readEpochJournalVector(bare, "agent/issue-3430"), [""]);
  const repair = await cache.inspectRetainedSources({ epoch: true, barePath: bare,
    selector: { runId: "epoch-run", generation: 1, attempt: completed[0]!.attemptId },
    holds: { state: "read", holds: [] }, lifecycle: { state: "terminal", status: "completed" },
    isLivePath: () => false });
  assert.equal(repair.sources.length, 1, "the completed pair still appears as repair evidence");
  const retired = new Set(completed.map(e => e.clonePath));
  const authority = {
    admitSourceUse: async () => {},
    beginBareMutation: async () => ({ settle: async () => {} }),
    assertBareClear: async () => {},
    beginDestructive: async () => ({ settle: async () => {} }),
    isRetiredSource: async (clonePath: string) => retired.has(clonePath),
  };
  cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions({ authority }));
  const census = await cache.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
  assert.equal(census.inventoryComplete, true);
  assert.equal(census.physicalPaths, 1, "only the tree that still needs work consumes the cap");
  assert.equal(census.overflow, 0);
  assert.deepEqual(census.groups.map(g => g.paths), [[live!.clonePath]]);

  retired.delete(completed[0]!.clonePath);
  const uncertain = await cache.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
  assert.equal(uncertain.physicalPaths, 2, "an unproven retired pair still consumes capacity");
  assert.equal(uncertain.groups.length, 2);
});
function inspect(over: Partial<Parameters<GitCache["inspectRetainedSources"]>[0]> = {}) {
  return cache.inspectRetainedSources({ isLivePath: () => false, barePath: bare, budgetMs: 90_000, ...over });
}

it("operator preserves 64 descriptors, and the 65th makes the view partial", async () => {
  attempts(64);
  const exact = await inspect();
  assert.equal(exact.sources.length, 64);
  assert.equal(exact.partial, false);
  attempts(1, bare, 64);
  const extra = await inspect();
  assert.equal(extra.sources.length, 64);
  assert.equal(extra.partial, true);
});
for (const count of [199, 200, 201]) {
  it("epoch census counts " + count + " physical paths across two real bares without measurement", async () => {
    attempts(count - 1);
    const second = path.join(path.dirname(bare), "second.git");
    git(path.dirname(bare), ["clone", "--bare", "--no-hardlinks", fixture.originPath, second]);
    fs.chmodSync(second, 0o700);
    attempts(1, second, count);
    const before = fs.readFileSync(path.join(bare, "config"));
    const census = await cache.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
    assert.equal(census.inventoryComplete, true);
    assert.equal(census.physicalPaths, count);
    assert.equal(census.overflow, Math.max(0, count - 200));
    assert.equal(census.overflowExact, true);
    assert.equal(new Set(census.groups.flatMap(g => g.paths)).size, Math.min(count, 200));
    assert.ok(census.groups.every(g => !("allocatedBytes" in g)));
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  });
}
it("epoch inspection admits 200 instead of silently returning the operator 64", async () => {
  attempts(201);
  const result = await inspect({ epoch: true });
  assert.equal(result.sources.length, 200);
  assert.equal(result.partial, true);
  assert.equal(result.unreadable, 0);
});
it("multipart paths are deduplicated and the whole group is omitted when it exceeds remaining capacity", async () => {
  const entries = attempts(3);
  const journal = { runId: "epoch-run", clonePath: entries[0]!.clonePath, attemptId: entries[0]!.attemptId,
    retainedSources: entries.map(e => ({ runId: e.runId, clonePath: e.clonePath, attemptId: e.attemptId })) };
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone", JSON.stringify(journal)]);
  const census = await cache.discoverEpochRetainedSources({ maxPhysicalPaths: 2, deadline: Date.now() + 90_000 });
  assert.equal(census.inventoryComplete, true);
  assert.equal(census.physicalPaths, 3);
  assert.equal(census.groups.length, 0);
  assert.equal(census.overflow, 3);
  assert.equal(census.overflowExact, true);
  const included = await cache.discoverEpochRetainedSources({ maxPhysicalPaths: 3, deadline: Date.now() + 90_000 });
  assert.equal(included.groups.length, 1);
  assert.equal(included.groups[0]!.paths.length, 3);
  const refused = await inspect({ epoch: true, maxPhysicalPaths: 2, selector: included.groups[0]!.selector });
  assert.equal(refused.partial, true);
  assert.equal(refused.sources.length, 0);
});
it("seen-path cap reports a lower bound without retaining beyond the bound", async () => {
  attempts(4);
  const result = await cache.discoverEpochRetainedSources({ limits: { maxSeenPaths: 3 }, deadline: Date.now() + 90_000 });
  assert.equal(result.inventoryComplete, false);
  assert.equal(result.overflowExact, false);
  assert.equal(result.physicalPaths, 3);
  assert.equal(result.groups.length, 3);
});
for (const limits of [{ maxDirectoryEntries: 1 }, { maxBares: 1 }] satisfies EpochRetainedLimits[]) {
  it("streaming census refuses entry/bare overflow " + JSON.stringify(limits), async () => {
    attempts(1);
    const second = path.join(path.dirname(bare), "second.git");
    git(path.dirname(bare), ["clone", "--bare", fixture.originPath, second]);
    fs.chmodSync(second, 0o700);
    const result = await cache.discoverEpochRetainedSources({ limits, deadline: Date.now() + 90_000 });
    assert.equal(result.inventoryComplete, false);
    assert.equal(result.overflowExact, false);
    assert.ok(result.physicalPaths <= 1);
  });
}
for (const limits of [{ maxMetadataBytes: 32 }, { maxMetadataRecords: 1 }] satisfies EpochRetainedLimits[]) {
  it("epoch metadata cap invalidates the bare before returning candidates " + JSON.stringify(limits), async () => {
    attempts(2);
    const result = await cache.discoverEpochRetainedSources({ limits, deadline: Date.now() + 90_000 });
    assert.equal(result.inventoryComplete, false);
    assert.equal(result.overflowExact, false);
    assert.equal(result.groups.length, 0);
    assert.equal(result.unreadable, 1);
    assert.equal((await inspect()).sources.length, 2, "epoch limits do not leak into operator reads");
  });
}
it("a missing repos root is an empty, complete census; any other open failure is not", async () => {
  const fresh = fs.mkdtempSync(path.join(fixture.dataDir, "..", "epoch-fresh-"));
  try {
    const empty = new GitCache(fresh, nullLogger(), undefined, testGitCacheOptions());
    const result = await empty.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
    assert.equal(result.inventoryComplete, true);
    assert.equal(result.overflowExact, true);
    assert.equal(result.unreadable, 0);
    assert.equal(result.groups.length, 0);
    fs.writeFileSync(path.join(fresh, "repos"), "not a directory");
    const blocked = await empty.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
    assert.equal(blocked.inventoryComplete, false);
    assert.equal(blocked.unreadable, 1);
  } finally { fs.rmSync(fresh, { recursive: true, force: true }); }
});
it("invalid config is unreadable and cannot produce a complete census", async () => {
  fs.appendFileSync(path.join(bare, "config"), "\n[unterminated\n");
  const result = await cache.discoverEpochRetainedSources();
  assert.equal(result.inventoryComplete, false);
  assert.equal(result.groups.length, 0);
  assert.equal(result.unreadable, 1);
});
it("queued epoch cancellation forfeits its bare slot and never gathers after release", async () => {
  attempts(1);
  let entered!: () => void;
  const started = new Promise<void>(r => { entered = r; });
  let release!: () => void;
  const held = new Promise<void>(r => { release = r; });
  const blocker = new GitCache(fixture.dataDir, nullLogger(), undefined, {
    ...testGitCacheOptions(), terminalRecordProtection: async () => { entered(); await held; return false; },
  });
  const active = blocker.inspectRetainedSources({ barePath: bare, isLivePath: () => false, budgetMs: 90_000 });
  await started;
  let calls = 0;
  const original = fsp.lstat;
  mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => { calls++; return original(...args); });
  const stop = new AbortController();
  const waiting = blocker.inspectRetainedSources({ barePath: bare, isLivePath: () => false, epoch: true, signal: stop.signal });
  await new Promise<void>(r => setImmediate(r));
  stop.abort();
  assert.equal((await waiting).partial, true);
  const beforeRelease = calls;
  release();
  await active;
  await new Promise<void>(r => setImmediate(r));
  assert.equal(calls, beforeRelease, "cancelled waiter must never execute its gather");
});
it("discovery abort waits for an uncancellable directory read and closes its handle", async () => {
  const original = fsp.opendir;
  const stop = new AbortController();
  let entered!: () => void;
  const started = new Promise<void>(r => { entered = r; });
  let release!: () => void;
  const held = new Promise<void>(r => { release = r; });
  let closed = false;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await original(...args);
    const read = handle.read.bind(handle);
    const close = handle.close.bind(handle);
    mock.method(handle, "read", async () => { entered(); await held; return read(); });
    mock.method(handle, "close", async () => { await close(); closed = true; });
    return handle;
  });
  let settled = false;
  const pending = cache.discoverEpochRetainedSources({ signal: stop.signal }).then(r => { settled = true; return r; });
  await started;
  stop.abort();
  await new Promise<void>(r => setImmediate(r));
  assert.equal(settled, false);
  assert.equal(closed, false);
  release();
  assert.equal((await pending).inventoryComplete, false);
  assert.equal(closed, true);
});
it("measurement abort joins an uncancellable lstat before returning partial", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  fs.writeFileSync(path.join(entry!.clonePath, "payload"), "retained");
  const original = fsp.lstat;
  const stop = new AbortController();
  let entered!: () => void;
  const started = new Promise<void>(r => { entered = r; });
  let release!: () => void;
  const held = new Promise<void>(r => { release = r; });
  let probes = 0;
  mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => {
    if (args[0] === entry!.clonePath && ++probes === 2) { entered(); await held; }
    return original(...args);
  });
  let settled = false;
  const pending = inspect({ epoch: true, signal: stop.signal }).then(r => { settled = true; return r; });
  await started;
  stop.abort();
  await new Promise<void>(r => setImmediate(r));
  assert.equal(settled, false);
  release();
  const result = await pending;
  assert.equal(result.partial, true);
  assert.equal(result.sources[0]!.allocatedBytes, null);
});

it("#3775 epoch measurement walks a physical path once when multiple descriptors name it", async () => {
  const clonePath = cache.runnerClonePath(bare, "issue-3430");
  fs.mkdirSync(clonePath, { recursive: true });
  fs.writeFileSync(path.join(clonePath, "payload"), "retained");
  for (const generation of [1, 2]) {
    const marker = { version: 1, runId: "epoch-run", branch: "agent/issue-3430", key: "issue-3430",
      clonePath, generation, journalDigest: "a".repeat(64), reason: "stale_claim", at: new Date().toISOString() };
    git(bare, ["config", "--local", "--add", "uzi-stopped.epoch-run.entry", JSON.stringify(marker)]);
  }
  const original = fsp.opendir;
  let walks = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    if (args[0] === clonePath) walks++;
    return original(...args);
  });
  const result = await inspect({ epoch: true, maxPhysicalPaths: 1 });
  assert.equal(result.sources.length, 2);
  assert.equal(result.partial, false);
  assert.equal(walks, 1);
  assert.equal(result.sources[0]!.allocatedBytes, result.sources[1]!.allocatedBytes);
  assert.deepEqual(result.partialCauses, {});
  const capped = await inspect({ epoch: true, maxPhysicalPaths: 1, measureCaps: { maxEntries: 0 } });
  assert.deepEqual(capped.partialCauses, { measurement_entry_cap: 1 });
  assert.equal(capped.partial, false, "a cap confined to one tree no longer makes the pass partial");
  assert.deepEqual(capped.sources.map(s => s.sources[0]!.measurementShortfall), ["entry_cap", "entry_cap"]);
  assert.equal(capped.unreadable, 0);
  assert.equal(walks, 2);
});

for (const abort of [false, true]) {
  it("multipart binding failure stops the second physical measurement (abort=" + abort + ")", async () => {
    const entries = attempts(2);
    const source = (i: number) => ({ runId: "epoch-run", clonePath: entries[i]!.clonePath, attemptId: entries[i]!.attemptId });
    git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone", JSON.stringify({ ...source(0), retainedSources: [source(1)] })]);
    for (const entry of entries) {
      fs.mkdirSync(entry.clonePath, { recursive: true });
      fs.writeFileSync(path.join(entry.clonePath, "payload"), "retained");
    }
    const census = await cache.discoverEpochRetainedSources();
    const group = census.groups.find(g => g.paths.length === 2)!;
    assert.ok(group);
    const original = fsp.opendir;
    const walks: string[] = [];
    mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      if (entries.some(e => e.clonePath === args[0])) walks.push(String(args[0]));
      return original(...args);
    });
    const stop = new AbortController();
    const checked: string[] = [];
    const bindingError = new Error("pod_entry_changed");
    const result = await inspect({ epoch: true, selector: group.selector, signal: stop.signal,
      beforeTree: clonePath => {
        checked.push(clonePath);
        if (checked.length === 2) {
          if (abort) stop.abort(bindingError);
          throw bindingError;
        }
      } });
    assert.equal(checked.length, 2);
    assert.deepEqual(new Set(checked), new Set(entries.map(e => e.clonePath)));
    assert.deepEqual(walks, [checked[0]]);
    assert.equal(result.partial, true);
    assert.equal(result.sources.length, 1);
    assert.equal(result.sources[0]!.bytesState, "partial");
    assert.equal(result.sources[0]!.sources[1]!.allocatedBytes, null);
    assert.equal(result.sources[0]!.disposition, undefined);
    if (abort) assert.equal(stop.signal.reason, bindingError);
  });
}

it("a measurement directory-close failure is partial rather than a complete byte total", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  const original = fsp.opendir;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await original(...args);
    if (args[0] === entry!.clonePath) {
      const close = handle.close.bind(handle);
      mock.method(handle, "close", async () => { await close(); throw new Error("injected close failure"); });
    }
    return handle;
  });
  const result = await inspect({ epoch: true });
  assert.equal(result.partial, true);
  assert.equal(result.unreadable, 1);
  assert.equal(result.sources[0]!.allocatedBytes, null);
});

it("the production 16 MiB epoch output cap (R9) refuses a bare while operator inspection stays compatible", async () => {
  attempts(1);
  fs.appendFileSync(path.join(bare, "config"), "\n[epoch]\n\tpayload = " + "x".repeat(16 * 1024 * 1024 + 1) + "\n");
  const result = await cache.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
  assert.equal(result.inventoryComplete, false);
  assert.equal(result.groups.length, 0);
  assert.equal(result.unreadable, 1);
  const ordinary = await inspect();
  assert.equal(ordinary.sources.length, 1);
  assert.equal(ordinary.unreadable, 0);
});

it("epoch ref-record overflow refuses authority rather than truncating the owed pin snapshot", async () => {
  const [entry] = attempts(1);
  const sha = git(bare, ["rev-parse", "HEAD"]);
  for (let i = 0; i < 33; i++) git(bare, ["update-ref", "refs/uzi-owed/run-" + i + "/" + sha, sha]);
  const result = await inspect({ epoch: true, epochLimits: { maxMetadataRecords: 32 },
    selector: { runId: entry!.runId, generation: 1, attempt: entry!.attemptId }, holds: { state: "read", holds: [] } });
  assert.equal(result.sources.length, 1);
  assert.equal(result.sources[0]!.disposition?.kind, "refused");
  assert.ok(result.sources[0]!.consumers.includes("blocker:owed_pins_unreadable"));
  assert.deepEqual(result.partialCauses, {});
  assert.equal(result.partial, false);
  assert.equal(result.unreadable, 0);
  attempts(1, bare, 1);
  const sibling = await inspect({ epoch: true, maxDescriptors: 1, epochLimits: { maxMetadataRecords: 32 } });
  assert.deepEqual(sibling.partialCauses, { inspection_descriptor_cap: 1 });
  assert.equal(sibling.partial, true);
});

for (const key of ["uzi-attempts.agent/issue-3430.entry", "uzi-recovery.agent/issue-3430.clone", "uzi-retained.epoch-run.journal", "uzi-stopped.epoch-run.entry"]) {
  it("epoch malformed retained metadata invalidates the whole bare: " + key, async () => {
    attempts(1);
    git(bare, ["config", "--local", "--add", key, "{malformed"]);
    const result = await cache.discoverEpochRetainedSources({ deadline: Date.now() + 90_000 });
    assert.equal(result.inventoryComplete, false);
    assert.equal(result.groups.length, 0);
    assert.equal(result.unreadable, 1);
  });
}

it("a selected multipart group that grows after the census fails before any measurement", async () => {
  const entries = attempts(3);
  const source = (i: number) => ({ runId: "epoch-run", clonePath: entries[i]!.clonePath, attemptId: entries[i]!.attemptId });
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone", JSON.stringify({ ...source(0), retainedSources: [source(1)] })]);
  const census = await cache.discoverEpochRetainedSources({ maxPhysicalPaths: 2, deadline: Date.now() + 90_000 });
  const group = census.groups.find(g => g.paths.length === 2)!;
  assert.ok(group);
  fs.mkdirSync(entries[0]!.clonePath, { recursive: true });
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone", JSON.stringify({ ...source(0), retainedSources: [source(1), source(2)] })]);
  const original = fsp.opendir;
  let measurementOpens = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    if (args[0] === entries[0]!.clonePath) measurementOpens++;
    return original(...args);
  });
  const result = await inspect({ epoch: true, maxPhysicalPaths: 2, selector: group.selector });
  assert.equal(result.partial, true);
  assert.equal(result.sources.length, 0);
  assert.equal(measurementOpens, 0);
});

for (const failingFirst of [true, false]) for (const abort of [false, true]) {
  it("epoch discovery joins failed realpath and delayed sibling (failureFirst=" + failingFirst + ", abort=" + abort + ")", async () => {
    let entered!: () => void, release!: () => void;
    const ready = new Promise<void>(r => { entered = r; });
    const held = new Promise<void>(r => { release = r; });
    let calls = 0;
    mock.method(fsp, "realpath", async () => {
      const first = ++calls === 1;
      if (first === failingFirst) throw new Error("lookup failure");
      entered(); await held; return path.dirname(bare);
    });
    const stop = new AbortController();
    let settled = false;
    const pending = cache.discoverEpochRetainedSources({ signal: stop.signal }).finally(() => { settled = true; });
    try {
      await ready;
      if (abort) stop.abort();
      // Allow unrelated directory cleanup to finish without releasing either lookup.
      for (let i = 0; i < 1000 && !settled; i++) await new Promise<void>(r => setImmediate(r));
      assert.equal(calls, 2);
      assert.equal(settled, false, "discovery cannot hand ownership back before both lookups settle");
    } finally { release(); }
    const result = await pending;
    assert.equal(result.inventoryComplete, false);
    assert.equal(result.groups.length, 0);
  });
}

for (const failingFirst of [true, false]) {
  it("public bare resolution joins failed realpath and delayed sibling (failureFirst=" + failingFirst + ")", async () => {
    let entered!: () => void, release!: () => void;
    const ready = new Promise<void>(r => { entered = r; });
    const held = new Promise<void>(r => { release = r; });
    let calls = 0, settled = false;
    mock.method(fsp, "realpath", async () => {
      if ((++calls === 1) === failingFirst) throw new Error("lookup failure");
      entered(); await held; return path.dirname(bare);
    });
    const pending = cache.resolveRecoveryBareDir(path.basename(bare)).finally(() => { settled = true; });
    try {
      await ready;
      await new Promise<void>(r => setImmediate(r));
      assert.equal(calls, 2);
      assert.equal(settled, false, "public resolution must join both filesystem outcomes");
    } finally { release(); }
    assert.equal(await pending, undefined);
  });
}

it("#3775 a child ENOENT stays a complete measurement without a partial cause", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  const payload = path.join(entry!.clonePath, "payload");
  fs.writeFileSync(payload, "retained");
  const lstat = fsp.lstat;
  let hits = 0;
  mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => {
    if (args[0] === payload) { hits++; throw Object.assign(new Error("vanished"), { code: "ENOENT" }); }
    return lstat(...args);
  });
  const result = await inspect({ epoch: true });
  assert.equal(hits, 1);
  assert.equal(result.partial, false);
  assert.equal(result.unreadable, 0);
  assert.equal(result.sources[0]!.bytesState, "measured");
  assert.deepEqual(result.partialCauses, {});
});

type MeasureMode = "entry" | "pending" | "depth" | "deadline" | "root" | "open" | "read" | "child" | "close" | "cap-close" | "read-close" |
  "open-eacces" | "sub-eperm" | "child-eperm" | "open-eio" | "open-emfile" | "read-eacces" | "open-string" | "open-getter" | "child-getter";
/** Faults confined to one tree: in epoch inspection they leave the pass complete and name a shortfall. */
const localShortfall = {
  entry: "entry_cap", pending: "pending_cap", depth: "depth_cap", child: "eacces", "child-eperm": "eperm",
  "open-eacces": "eacces", "sub-eperm": "eperm",
} as const;
const codedError = (code: string) => Object.assign(new Error("private"), { code });
const throwingCode = () => Object.defineProperty({}, "code", { get() { throw new Error("hostile getter"); } });
for (const epoch of [true, false]) for (const mode of ["entry", "pending", "depth", "deadline", "root", "open", "read", "child", "close", "cap-close", "read-close",
  "open-eacces", "sub-eperm", "child-eperm", "open-eio", "open-emfile", "read-eacces", "open-string", "open-getter", "child-getter"] as const satisfies readonly MeasureMode[]) {
  it("#3775 measurement original-site causes " + mode + (epoch ? "" : " (operator twin)"), async () => {
    const [entry] = attempts(1);
    const root = entry!.clonePath;
    fs.mkdirSync(root, { recursive: true });
    fs.writeFileSync(path.join(root, "payload"), "retained");
    fs.mkdirSync(path.join(root, "dir"));
    fs.mkdirSync(path.join(root, "dir2"));
    const open = fsp.opendir, lstat = fsp.lstat;
    let rootStats = 0, hits = 0, clock = 0;
    const childFault = (): Error | object | undefined =>
      mode === "child" ? codedError("EACCES") : mode === "child-eperm" ? codedError("EPERM") :
      mode === "child-getter" ? throwingCode() : undefined;
    mock.method(fsp, "lstat", async (...args: Parameters<typeof fsp.lstat>) => {
      if (mode === "root" && args[0] === root && ++rootStats === 2) { hits++; throw codedError("EACCES"); }
      const fault = childFault();
      if (fault && String(args[0]).startsWith(root + path.sep)) { hits++; throw fault; }
      return lstat(...args);
    });
    mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      const atRoot = args[0] === root;
      const below = String(args[0]).startsWith(root + path.sep);
      if (atRoot && mode === "open") { hits++; throw new Error("private"); }
      if (atRoot && mode === "open-eacces") { hits++; throw codedError("EACCES"); }
      if (atRoot && mode === "open-eio") { hits++; throw codedError("EIO"); }
      if (atRoot && mode === "open-emfile") { hits++; throw codedError("EMFILE"); }
      if (atRoot && mode === "open-string") { hits++; throw "private"; }
      if (atRoot && mode === "open-getter") { hits++; throw throwingCode(); }
      if (below && mode === "sub-eperm") { hits++; throw codedError("EPERM"); }
      const handle = await open(...args);
      if (atRoot) {
        if (mode === "deadline") clock = 100;
        if (mode === "read" || mode === "read-close") mock.method(handle, "read", async () => { hits++; throw new Error("private"); });
        if (mode === "read-eacces") mock.method(handle, "read", async () => { hits++; throw codedError("EACCES"); });
        if (mode === "close" || mode === "cap-close" || mode === "read-close") {
          const close = handle.close.bind(handle);
          mock.method(handle, "close", async () => { await close(); hits++; throw new Error("private"); });
        }
      }
      return handle;
    });
    const caps = mode === "entry" || mode === "cap-close" ? { maxEntries: 1 } :
      mode === "pending" ? { maxPending: 0 } : mode === "depth" ? { maxDepth: 0 } : {};
    const result = await inspect({ epoch, measureCaps: caps, now: () => clock, budgetMs: 100 });
    const expected = mode === "entry" ? { measurement_entry_cap: 1 } :
      mode === "cap-close" ? { measurement_entry_cap: 1, measurement_filesystem_error: 1 } :
      mode === "pending" ? { measurement_pending_cap: 1 } : mode === "depth" ? { measurement_depth_cap: 1 } :
      mode === "deadline" ? { measurement_deadline: 1 } :
      { measurement_filesystem_error: mode === "read-close" ? 2 : 1 };
    assert.deepEqual(result.partialCauses, expected);
    const shortfall = mode in localShortfall ? localShortfall[mode as keyof typeof localShortfall] : undefined;
    assert.equal(result.partial, !(epoch && shortfall));
    assert.equal(result.sources.length, 1);
    const part = result.sources[0]!.sources[0]!;
    assert.equal(part.allocatedBytes, null);
    assert.equal(result.sources[0]!.bytesState, "unknown");
    if (epoch && shortfall) assert.equal(part.measurementShortfall, shortfall);
    else assert.ok(!("measurementShortfall" in part), "only a tree-local epoch stop carries the key");
    assert.equal(result.unreadable, ["read", "close", "cap-close", "read-close", "read-eacces"].includes(mode) ? 1 : 0);
    if (!["entry", "pending", "depth", "deadline"].includes(mode)) assert.ok(hits > 0);
  });
}

// A spent budget outranks a tree-local stop: the stop is then a pass-wide deadline, never a shortfall.
for (const stop of ["opendir-eacces", "entry-cap"] as const) {
  it("#3851 a measurement deadline that passed during a local " + stop + " stop is pass-wide", async () => {
    const [entry] = attempts(1);
    const root = entry!.clonePath;
    fs.mkdirSync(root, { recursive: true });
    for (const name of ["a", "b", "c"]) fs.writeFileSync(path.join(root, name), "retained");
    const open = fsp.opendir;
    let clock = 0, reads = 0;
    mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      if (args[0] === root && stop === "opendir-eacces") { clock = 100; throw codedError("EACCES"); }
      const handle = await open(...args);
      if (args[0] === root) {
        const read = handle.read.bind(handle);
        mock.method(handle, "read", async () => { if (++reads === 2) clock = 100; return read(); });
      }
      return handle;
    });
    const result = await inspect({ epoch: true, now: () => clock, budgetMs: 100, measureCaps: stop === "entry-cap" ? { maxEntries: 1 } : {} });
    assert.equal(result.partial, true);
    assert.deepEqual(result.partialCauses, stop === "entry-cap"
      ? { measurement_entry_cap: 1, measurement_deadline: 1 }
      : { measurement_filesystem_error: 1, measurement_deadline: 1 });
    assert.ok(!("measurementShortfall" in result.sources[0]!.sources[0]!));
  });
}
// The walk's budget check precedes its awaited directory close; a budget spent during that close still
// makes the local stop pass-wide when the inspection accepts it.
it("#3851 an inspection budget spent during the directory close after a local entry-cap stop is pass-wide", async () => {
  const [entry] = attempts(1);
  const root = entry!.clonePath;
  fs.mkdirSync(root, { recursive: true });
  for (const name of ["a", "b", "c"]) fs.writeFileSync(path.join(root, name), "retained");
  const open = fsp.opendir;
  let clock = 0, closes = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await open(...args);
    if (args[0] === root) {
      const close = handle.close.bind(handle);
      mock.method(handle, "close", async () => { await close(); closes++; clock = 100; });
    }
    return handle;
  });
  const result = await inspect({ epoch: true, now: () => clock, budgetMs: 100, measureCaps: { maxEntries: 1 } });
  assert.equal(closes, 1);
  assert.equal(result.partial, true);
  assert.deepEqual(result.partialCauses, { measurement_entry_cap: 1, inspection_budget: 1 });
  assert.ok(!("measurementShortfall" in result.sources[0]!.sources[0]!));
  assert.equal(result.sources[0]!.sources[0]!.allocatedBytes, null);
});
// The shortfall guard in inspectRetainedSourcesScoped only decides on a measurement cache hit: a second
// descriptor naming an already-measured path skips withRecoveryOperation, so no interruption check runs
// for it. These pin each half of the guard there, acting only after the first measurement has settled.
for (const half of ["signal", "deadline", "budget"] as const) {
  it("#3851 a cached local stop is not a shortfall once the " + half + " has expired after the first measurement", async () => {
    const clonePath = cache.runnerClonePath(bare, "issue-3430");
    fs.mkdirSync(clonePath, { recursive: true });
    fs.writeFileSync(path.join(clonePath, "payload"), "retained");
    for (const generation of [1, 2]) {
      const marker = { version: 1, runId: "epoch-run", branch: "agent/issue-3430", key: "issue-3430",
        clonePath, generation, journalDigest: "a".repeat(64), reason: "stale_claim", at: new Date().toISOString() };
      git(bare, ["config", "--local", "--add", "uzi-stopped.epoch-run.entry", JSON.stringify(marker)]);
    }
    const open = fsp.opendir;
    let measured = false;
    mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      if (args[0] === clonePath) measured = true;
      return open(...args);
    });
    // No await separates the first descriptor's guard from the second descriptor's cache hit, so the
    // expiry is injected synchronously: the first read of the guarded value (made by the first
    // descriptor's guard, after its measurement settled) is still live, every later read is expired.
    const stop = new AbortController();
    const deadline = Date.now() + 600_000;
    const realNow = Date.now.bind(Date);
    let settled = false;
    let guardReads = 0;
    const live = () => settled && ++guardReads > 1 ? false : true;
    if (half === "signal") Object.defineProperty(stop.signal, "aborted", { get: () => !live() });
    else if (half === "deadline") mock.method(Date, "now", () => (live() ? realNow() : deadline + 1));
    const operation = GitCache.prototype.withRecoveryOperation;
    mock.method(cache, "withRecoveryOperation", async (...args: Parameters<typeof operation>) => {
      const value = await operation.apply(cache, args);
      if (measured) settled = true;
      return value;
    });
    // The budget half drives the injected inspection clock: it stays at 0 until the first post-settlement
    // read (the first descriptor's guard), then reads past the 100 ms budget.
    const result = await inspect({ epoch: true, signal: stop.signal, deadline, measureCaps: { maxEntries: 0 },
      ...(half === "budget" ? { now: () => (live() ? 0 : 100), budgetMs: 100 } :
        // The deadline half mocks Date.now, so the budget runs on its own injected clock.
        half === "deadline" ? { now: () => 0, budgetMs: 600_000 } : {}) });
    assert.ok(guardReads >= 2, "the second descriptor's guard ran after the first measurement settled");
    if (half === "budget") assert.equal(result.partialCauses?.inspection_budget, 1);
    assert.equal(result.sources.length, 2);
    const parts = result.sources.map(s => s.sources[0]!);
    assert.equal(parts[0]!.measurementShortfall, "entry_cap");
    assert.ok(!("measurementShortfall" in parts[1]!), "the cached part is a pass-wide stop, not a shortfall");
    assert.equal(result.partial, true);
  });
}

// These two stop through checkRecoveryInterruption() in the opendir catch, not the cache-hit guard.
it("#3851 an absolute deadline option that expires during a local stop is partial without a shortfall (interruption path)", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  const deadline = Date.now() + 600_000;
  const realNow = Date.now.bind(Date);
  let targetOpens = 0;
  mock.method(Date, "now", () => targetOpens ? deadline + 1 : realNow());
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    if (args[0] === entry!.clonePath) { targetOpens++; throw codedError("EACCES"); }
    throw new Error("unexpected opendir");
  });
  const result = await inspect({ epoch: true, deadline, now: () => 0, budgetMs: 600_000 });
  assert.equal(targetOpens, 1, "the target opendir reached the interruption catch");
  assert.equal(result.partial, true);
  const parts = result.sources.flatMap(source => source.sources);
  assert.equal(parts.length, 1);
  assert.ok(!("measurementShortfall" in parts[0]!));
});
it("#3851 an aborted signal during a local stop is partial without a shortfall (interruption path)", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  const stop = new AbortController();
  let targetOpens = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    if (args[0] === entry!.clonePath) { targetOpens++; stop.abort(); throw codedError("EACCES"); }
    throw new Error("unexpected opendir");
  });
  const result = await inspect({ epoch: true, signal: stop.signal });
  assert.equal(targetOpens, 1, "the target opendir reached the interruption catch");
  assert.equal(result.partial, true);
  const parts = result.sources.flatMap(source => source.sources);
  assert.equal(parts.length, 1);
  assert.ok(!("measurementShortfall" in parts[0]!));
});

for (const [limits, key] of [
  [{ maxMetadataBytes: 32 }, "census_metadata_byte_cap"],
  [{ maxMetadataRecords: 1 }, "census_metadata_record_cap"],
  [{ maxSeenPaths: 1 }, "census_seen_path_cap"],
] as const) {
  it("#3775 census original-site " + key, async () => {
    attempts(2);
    const result = await cache.discoverEpochRetainedSources({ limits });
    assert.equal(result.inventoryComplete, false);
    assert.deepEqual(result.partialCauses, { [key]: 1 });
    assert.deepEqual((await inspect()).partialCauses, {});
  });
}
it("#3775 inspection budget deadline interruption and descriptor/path limits", async () => {
  attempts(2);
  for (const [options, expected] of [
    [{ budgetMs: 0 }, { inspection_budget: 1 }],
    [{ deadline: 0 }, { inspection_deadline: 1 }],
    [{ signal: AbortSignal.abort() }, { inspection_interrupted: 1 }],
    [{ maxDescriptors: 1 }, { inspection_descriptor_cap: 1 }],
    [{ maxPhysicalPaths: 1 }, { inspection_physical_path_cap: 1 }],
  ] as const) {
    const result = await inspect({ epoch: true, ...options });
    assert.equal(result.partial, true);
    assert.deepEqual(result.partialCauses, expected);
  }
});
it("#3775 concurrent epoch census inspection and operator calls on the SAME GitCache are isolated", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  fs.writeFileSync(path.join(entry!.clonePath, "a"), "a");
  fs.writeFileSync(path.join(entry!.clonePath, "b"), "b");
  let ready!: () => void, release!: () => void;
  const started = new Promise<void>(r => { ready = r; });
  const held = new Promise<void>(r => { release = r; });
  const open = fsp.opendir;
  let hits = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await open(...args);
    if (args[0] === entry!.clonePath && ++hits === 1) { ready(); await held; }
    return handle;
  });
  const pending = inspect({ epoch: true, measureCaps: { maxEntries: 1 } });
  await started;
  try {
    const census = await cache.discoverEpochRetainedSources({ limits: { maxMetadataBytes: 32 } });
    assert.deepEqual(census.partialCauses, { census_metadata_byte_cap: 1 });
    const sibling = await inspect({ epoch: true });
    assert.deepEqual(sibling.partialCauses, {});
    assert.equal(sibling.partial, false);
    const operator = await inspect();
    assert.deepEqual(operator.partialCauses, {});
    assert.equal(operator.partial, false);
  } finally { release(); }
  assert.deepEqual((await pending).partialCauses, { measurement_entry_cap: 1 });
  assert.equal(hits, 3);
});

for (const mode of ["native", "metered", "boundary"] as const) {
  it("#3775 metadata byte cap survives " + mode + " subprocess wrapping", async () => {
    attempts(1);
    const discover = () => cache.discoverEpochRetainedSources({ limits: { maxMetadataBytes: 32 } });
    let launches = 0;
    const result = mode === "metered" ? await MaintenanceResources.runDiscovery(discover) :
      mode === "boundary" ? await cache.withBoundaryProcessSpawner(async request => {
        launches++;
        const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env, stdio: "pipe" });
        const completed = new Promise<{ code: number }>((resolve, reject) => {
          child.once("error", reject);
          child.once("close", code => resolve({ code: code ?? 128 }));
        });
        return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
          cancel: async () => { child.kill(); await completed; } };
      }, new AbortController().signal, discover) : await discover();
    assert.deepEqual(result.partialCauses, { census_metadata_byte_cap: 1 });
    assert.equal(result.inventoryComplete, false);
    assert.equal(result.unreadable, 1);
    assert.equal(result.groups.length, 0);
    if (mode === "boundary") assert.ok(launches > 0);
  });
}
it("#3775 source record expansion has its own cap", async () => {
  const [entry] = attempts(1);
  const source = { runId: entry!.runId, clonePath: entry!.clonePath, attemptId: entry!.attemptId };
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone",
    JSON.stringify({ ...source, retainedSources: Array.from({ length: 33 }, () => source) })]);
  const result = await cache.discoverEpochRetainedSources({ limits: { maxMetadataRecords: 32 } });
  assert.equal(result.inventoryComplete, false);
  assert.deepEqual(result.partialCauses, { census_source_record_cap: 1 });
  const inspected = await inspect({ epoch: true, epochLimits: { maxMetadataRecords: 32 } });
  assert.deepEqual(inspected.partialCauses, { inspection_source_record_cap: 1 });
});

it("#3775 census and shared listing entry/bare caps retain exact cause families", async () => {
  attempts(1);
  const second = path.join(path.dirname(bare), "second.git");
  git(path.dirname(bare), ["clone", "--bare", fixture.originPath, second]);
  fs.chmodSync(second, 0o700);
  for (const [limits, suffix] of [[{ maxDirectoryEntries: 1 }, "entry_cap"], [{ maxBares: 1 }, "bare_cap"]] as const) {
    const census = await cache.discoverEpochRetainedSources({ limits });
    assert.equal(census.inventoryComplete, false);
    assert.deepEqual(census.partialCauses, { ["census_" + suffix]: 1 });
    const inspection = await cache.inspectRetainedSources({ epoch: true, epochLimits: limits, isLivePath: () => false });
    assert.equal(inspection.unreadable, 1);
    assert.deepEqual(inspection.partialCauses, { ["inspection_" + suffix]: 1 });
  }
});
it("#3775 physical census omission and inspection metadata caps are independently named", async () => {
  attempts(2);
  const census = await cache.discoverEpochRetainedSources({ maxPhysicalPaths: 1 });
  assert.equal(census.inventoryComplete, true);
  assert.equal(census.overflow, 1);
  assert.deepEqual(census.partialCauses, { census_physical_path_cap: 1 });
  for (const [epochLimits, suffix] of [[{ maxMetadataBytes: 32 }, "metadata_byte_cap"], [{ maxMetadataRecords: 1 }, "metadata_record_cap"]] as const) {
    const result = await inspect({ epoch: true, epochLimits });
    assert.equal(result.unreadable, 1);
    assert.deepEqual(result.partialCauses, { ["inspection_" + suffix]: 1 });
  }
});
it("#3775 unreadable and census deadline/interruption aggregation", async () => {
  const interrupted = await cache.discoverEpochRetainedSources({ signal: AbortSignal.abort() });
  assert.deepEqual(interrupted.partialCauses, { census_interrupted: 1 });
  const deadline = await cache.discoverEpochRetainedSources({ deadline: 0 });
  assert.deepEqual(deadline.partialCauses, { census_deadline: 1 });
  const open = fsp.opendir;
  let hits = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    if (args[0] === path.dirname(bare)) { hits++; throw Object.assign(new Error("private"), { code: "EACCES" }); }
    return open(...args);
  });
  const unreadable = await cache.discoverEpochRetainedSources();
  assert.deepEqual(unreadable.partialCauses, { census_unreadable: 1 });
  assert.equal(unreadable.unreadable, 1);
  assert.equal(hits, 1);
  fs.appendFileSync(path.join(bare, "config"), "\n[broken\n");
  const inspected = await inspect({ epoch: true });
  assert.deepEqual(inspected.partialCauses, { inspection_unreadable_bare: 1 });
  assert.equal(inspected.unreadable, 1);
});
it("#3775 one binding failure diagnoses once while repeated skips retain old unreadable counts", async () => {
  const entries = attempts(3);
  const sources = entries.map(e => ({ runId: e.runId, clonePath: e.clonePath, attemptId: e.attemptId }));
  git(bare, ["config", "--local", "uzi-recovery.agent/issue-3430.clone", JSON.stringify({ ...sources[0], retainedSources: sources.slice(1) })]);
  for (const entry of entries) fs.mkdirSync(entry.clonePath, { recursive: true });
  let checks = 0;
  const result = await inspect({ epoch: true, beforeTree: () => { checks++; throw "binding primitive"; } });
  assert.equal(result.partial, true);
  assert.equal(result.unreadable, 3);
  assert.equal(checks, 1);
  assert.deepEqual(result.partialCauses, { tree_binding_failed: 1 });
  assert.ok(result.sources.every(s => s.disposition === undefined && s.epochSettlement === undefined));
});
it("#3775 each executed shared-path measurement reports its failures without changing retries", async () => {
  const clonePath = cache.runnerClonePath(bare, "issue-3430");
  fs.mkdirSync(clonePath, { recursive: true });
  for (const generation of [1, 2]) {
    const marker = { version: 1, runId: "epoch-run", branch: "agent/issue-3430", key: "issue-3430",
      clonePath, generation, journalDigest: "a".repeat(64), reason: "stale_claim", at: new Date().toISOString() };
    git(bare, ["config", "--local", "--add", "uzi-stopped.epoch-run.entry", JSON.stringify(marker)]);
  }
  const open = fsp.opendir;
  let walks = 0;
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await open(...args);
    if (args[0] === clonePath) {
      walks++;
      mock.method(handle, "read", async () => { throw new Error("read"); });
      const close = handle.close.bind(handle);
      mock.method(handle, "close", async () => { await close(); throw new Error("close"); });
    }
    return handle;
  });
  const result = await inspect({ epoch: true });
  assert.equal(walks, 2);
  assert.equal(result.unreadable, 2);
  assert.equal(result.partial, true);
  assert.deepEqual(result.partialCauses, { measurement_filesystem_error: 4 });
});

for (const kind of ["bytes", "unterminated-record"] as const) {
  it("#3775 protected-descriptor wrapping preserves " + kind + " cause in each scope", async () => {
    attempts(1);
    const seam = cache as unknown as {
      protectedRecoveries: (bare: string) => Promise<unknown[]>;
      checkEpochMetadata: (args: string[], output: string) => void;
    };
    let hits = 0;
    mock.method(seam, "protectedRecoveries", async () => {
      hits++;
      // Produce a known original-site failure after the initial config gather succeeded.
      seam.checkEpochMetadata(["config", "--get-regexp", "^uzi-retained"],
        kind === "bytes" ? "x".repeat(4097) : "x\n".repeat(16) + "last");
      return [];
    });
    const limits = { maxMetadataBytes: 4096, maxMetadataRecords: 16 };
    const suffix = kind === "bytes" ? "metadata_byte_cap" : "metadata_record_cap";
    const census = await cache.discoverEpochRetainedSources({ limits });
    assert.equal(census.inventoryComplete, false);
    assert.equal(census.unreadable, 1);
    assert.deepEqual(census.partialCauses, { ["census_" + suffix]: 1 });
    const inspected = await inspect({ epoch: true, epochLimits: limits });
    assert.equal(inspected.unreadable, 1);
    assert.deepEqual(inspected.partialCauses, { ["inspection_" + suffix]: 1 });
    assert.equal(hits, 2);
  });
}

it("#3775 interruption during measurement close joins cleanup and reports interruption", async () => {
  const [entry] = attempts(1);
  fs.mkdirSync(entry!.clonePath, { recursive: true });
  const stop = new AbortController();
  const open = fsp.opendir;
  let entered!: () => void, release!: () => void;
  const started = new Promise<void>(resolve => { entered = resolve; });
  const held = new Promise<void>(resolve => { release = resolve; });
  mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await open(...args);
    if (args[0] === entry!.clonePath) {
      const close = handle.close.bind(handle);
      mock.method(handle, "close", async () => { await close(); entered(); await held; });
    }
    return handle;
  });
  let settled = false;
  const pending = inspect({ epoch: true, signal: stop.signal }).then(result => { settled = true; return result; });
  await started;
  try {
    stop.abort();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(settled, false);
  } finally { release(); }
  const result = await pending;
  assert.equal(result.partial, true);
  assert.equal(result.unreadable, 0);
  assert.equal(result.sources[0]!.allocatedBytes, null);
  assert.deepEqual(result.partialCauses, { inspection_interrupted: 1 });
});
