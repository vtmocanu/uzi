
import { afterEach, beforeEach, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
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
function attempts(count: number, target = bare, offset = 0) {
  const entries = [];
  for (let i = offset; i < offset + count; i++) {
    const attemptId = formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, i)), 1, (i + 1).toString(16).padStart(16, "0"));
    const clonePath = cache.runnerClonePath(target, "issue-3430") + ".attempt-" + attemptId;
    const entry = { attemptId, runId: "epoch-run", clonePath, state: "abandoned" as const };
    git(target, ["config", "--local", "--add", "uzi-attempts.agent/issue-3430.entry", JSON.stringify(entry)]);
    entries.push(entry);
  }
  return entries;
}
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

it("epoch measurement walks a physical path once when multiple descriptors name it", async () => {
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

it("the production 8 MiB epoch output cap refuses a bare while operator inspection stays compatible", async () => {
  attempts(1);
  fs.appendFileSync(path.join(bare, "config"), "\n[epoch]\n\tpayload = " + "x".repeat(8 * 1024 * 1024 + 1) + "\n");
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
