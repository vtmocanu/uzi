import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHash } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import { PassThrough, Readable, Writable } from "node:stream";
import { RecoveryMaintenance, MaintenanceResources, type MaintenanceEligibility } from "../src/recovery-maintenance.js";
import { verifyClosureFrames } from "../src/recovery-closure.js";
import { GitCache } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";
import type { BoundaryProcessSpawner } from "../src/git.js";

const sha = (s: string) => createHash("sha256").update(s).digest("hex");
const metadataCap = 256 * 1024 ** 2;
async function fixture(t: test.TestContext, decodedCap = 4096, clocks: { now?: () => number; monotonicNow?: () => number } = {}) {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/boundaries-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const ledger = new RecoveryMaintenance({ root: path.join(root, "recovery-maintenance"), workerToken: "boundary-test", ...clocks });
  const input: MaintenanceEligibility = {
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: sha("origin"), originalSource: sha("source"), sourceConfig: sha("config") },
    config: RecoveryMaintenance.configuration(decodedCap), exactCustody: true, terminalNonStopped: true,
    quiescent: true, sourceOutputContinuity: true, source: sha("source"),
  };
  const admitted = await ledger.admit(input);
  assert.ok(admitted.ok);
  const token = admitted.value;
  t.after(() => ledger.finish(token, "nonmutating"));
  const cache = new GitCache(root, nullLogger());
  const bare = path.join(root, "repo.git");
  execFileSync("git", ["init", "--bare", bare], { stdio: "pipe" });
  const git = (args: string[], stdin?: string) => execFileSync("git", ["--git-dir", bare, ...args], {
    encoding: "utf8", input: stdin, stdio: "pipe",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
      GIT_AUTHOR_NAME: "test", GIT_AUTHOR_EMAIL: "test@example.com",
      GIT_COMMITTER_NAME: "test", GIT_COMMITTER_EMAIL: "test@example.com" },
  }).trim();
  const blob = git(["hash-object", "-w", "--stdin"], "hello");
  const tree = git(["mktree"], `100644 blob ${blob}\tfile\n`);
  const tip = git(["commit-tree", tree, "-m", "fixture"]);
  return { root, ledger, input, token, cache, bare, tip, git };
}

function realSpawner(observed: { launches: number; closed: number; cancelled: number }): BoundaryProcessSpawner {
  return async request => {
    observed.launches++;
    const child = spawn(request.argv[0]!, request.argv.slice(1), { cwd: request.cwd, env: request.env, stdio: "pipe" });
    const completed = new Promise<{ code: number }>(resolve => {
      child.once("error", () => undefined);
      child.once("close", code => { observed.closed++; resolve({ code: code ?? -1 }); });
    });
    return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
      cancel: async () => { observed.cancelled++; child.kill("SIGKILL"); await completed; } };
  };
}

test("joined outcome recording survives a revoked active resource scope", async t => {
  const f = await fixture(t);
  await MaintenanceResources.run(f.token, async () => {
    assert.throws(() => MaintenanceResources.consume("metadataBytes", metadataCap + 1), /maintenance_resource_exhausted/);
    const result = await f.ledger.finish(f.token, "permanent");
    assert.deepEqual(result, { ok: true, value: undefined });
  });
  const value = JSON.parse(await fs.readFile(path.join(f.root, "recovery-maintenance", "complete.json"), "utf8")).value;
  assert.equal(value.cycles[0].exhausted, true);
  assert.equal(value.cycles[0].slots.length, 1);
});

test("slot abort timer uses the original monotonic remainder despite a wall-clock blip", async t => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let duringAdmission = true, reads = 0;
  const f = await fixture(t, 4096, {
    now: () => duringAdmission && ++reads === 3 ? 0 : 1_000_000,
    monotonicNow: () => 0,
  });
  duringAdmission = false;
  MaintenanceResources.run(f.token, () => {
    const signal = MaintenanceResources.signalIfActive()!;
    assert.equal(signal.aborted, false);
    t.mock.timers.tick(120_000);
    assert.equal(signal.aborted, true);
  });
});

test("metered native execution preserves spawn-error and overflow classifications", async t => {
  const f = await fixture(t);
  await MaintenanceResources.run(f.token, async () => {
    let missing: unknown;
    try { await f.cache["execScoped"](path.join(f.root, "absent-helper"), [], { env: {}, timeout: 1000 }); }
    catch (error) { missing = error; }
    assert.equal((missing as NodeJS.ErrnoException)?.code, "ENOENT");
    let overflow: unknown;
    try { await f.cache["execScoped"](process.execPath, ["-e", "process.stdout.write('xxxxxxxx')"],
      { env: {}, timeout: 1000, maxBuffer: 4 }); }
    catch (error) { overflow = error; }
    assert.equal((overflow as { outputExceeded?: boolean })?.outputExceeded, true);
  });
});

test("metered native execution keeps execFile's default one-MiB output bound", async t => {
  const f = await fixture(t);
  await MaintenanceResources.run(f.token, async () => {
    let refused = false;
    try { await f.cache["execScoped"](process.execPath,
      ["-e", "process.stdout.write(Buffer.alloc(1024 * 1024 + 1))"], { env: {}, timeout: 1000 }); }
    catch { refused = true; }
    assert.equal(refused, true);
  });
});

for (const mode of ["production", "discovery"] as const) {
  test(`${mode} metadata refuses at a tighter parent clock before timer dispatch`, async t => {
    const f = await fixture(t);
    let now = 0, delivered = 0;
    const parent = new MaintenanceBudget(10, () => now);
    t.after(() => parent.dispose());
    const work = async () => {
      now = 10;
      MaintenanceResources.consumeIfActive("metadataBytes", 1);
      delivered++;
    };
    await assert.rejects(MaintenanceBudget.run(parent, () => mode === "production"
      ? MaintenanceResources.run(f.token, work)
      : MaintenanceResources.runDiscovery(work)), /maintenance deadline/);
    assert.equal(delivered, 0);
  });
}

test("metadata discovery cannot initialize or charge a maintenance ledger", async t => {
  const f = await fixture(t);
  const parent = path.join(f.root, "uninitialized-data");
  await fs.mkdir(parent);
  const root = path.join(parent, "recovery-maintenance");
  const ledger = new RecoveryMaintenance({ root, workerToken: "discovery-fixture" });
  const result = await MaintenanceResources.runDiscovery(() => ledger.admit(f.input));
  assert.deepEqual(result, { ok: false, reason: "maintenance_discovery_only" });
  assert.deepEqual(await fs.readdir(parent), []);
});

for (const operation of ["admit", "revalidate"] as const) {
  test(`${operation} peer revocation aborts its signal while retaining the reservation`, async t => {
    let wall = 1_000_000;
    const f = await fixture(t, 4096, { now: () => wall, monotonicNow: () => 0 });
    const signal = MaintenanceResources.run(f.token, () => MaintenanceResources.signalIfActive()!);
    let cancelled = 0;
    signal.addEventListener("abort", () => cancelled++, { once: true });
    wall = f.token.deadline;
    const result = operation === "admit" ? await f.ledger.admit(f.input) : await f.ledger.revalidate(f.token, f.input);
    assert.deepEqual(result, { ok: false, reason: operation === "admit" ? "maintenance_slot_active" : "maintenance_token_invalid" });
    assert.equal(signal.aborted, true);
    assert.equal(cancelled, 1);
    assert.throws(() => MaintenanceResources.run(f.token, () => {}), /maintenance_token_invalid/);
    const stored = JSON.parse(await fs.readFile(path.join(f.root, "recovery-maintenance", "complete.json"), "utf8")).value;
    assert.equal(stored.cycles[0].slots.length, 1);
  });
}

test("decoded slices are charged before hashing, framing alone uses metadata", async t => {
  const f = await fixture(t, 5);
  const oid = createHash("sha1").update("blob 5\0hello").digest("hex");
  const header = `${oid} blob 5\n`;
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - Buffer.byteLength(header) - 1);
    await verifyClosureFrames(Readable.from([Buffer.from(header + "hello\n")]),
      [{ oid, type: "blob", size: 5 }], { delivered: 0, limit: 100 });
    MaintenanceResources.consume("metadataBytes", 0);
    assert.throws(() => MaintenanceResources.consume("decoded", 1), /maintenance_resource_exhausted/);
  });
});

test("decoded quota refuses actual body delivery with a larger legacy closure limit", async t => {
  const f = await fixture(t, 4);
  const oid = createHash("sha1").update("blob 5\0hello").digest("hex");
  await MaintenanceResources.run(f.token, async () => {
    const budget = { delivered: 0, limit: 100 };
    await assert.rejects(verifyClosureFrames(Readable.from([Buffer.from(`${oid} blob 5\nhello\n`)]),
      [{ oid, type: "blob", size: 5 }], budget), /maintenance_resource_exhausted/);
    assert.equal(budget.delivered, 0);
    assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
  });
});

for (const supervised of [false, true]) {
  test(`${supervised ? "supervised" : "native"} buffered raw stdout refuses before retaining a returned string`, async t => {
    const f = await fixture(t);
    const observed = { launches: 0, closed: 0, cancelled: 0 };
    await MaintenanceResources.run(f.token, async () => {
      MaintenanceResources.consume("metadataBytes", metadataCap - 1);
      const action = () => f.cache.verifyRecoveryClosure(f.bare, f.tip);
      assert.equal(await (supervised
        ? f.cache.withBoundaryProcessSpawner(realSpawner(observed), new AbortController().signal, action)
        : action()), false);
      assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
    });
    if (supervised) assert.equal(observed.closed, observed.launches);
  });

  test(`${supervised ? "supervised" : "native"} buffered closure charges actual launches and shares cache entry visits`, async t => {
    const f = await fixture(t);
    const observed = { launches: 0, closed: 0, cancelled: 0 };
    const action = () => f.cache.withRecoveryOperation(new AbortController().signal, Date.now() + 5000, async () => {
      assert.equal(await f.cache.verifyRecoveryClosure(f.bare, f.tip), true);
      // Three objects, one listing and one metadata visit each, even on cache hits.
      MaintenanceResources.consume("closureEntries", 65536 - 12);
      assert.equal(await f.cache.verifyRecoveryClosure(f.bare, f.tip), true);
      MaintenanceResources.consume("launches", 512 - 7);
      assert.throws(() => MaintenanceResources.consume("closureEntries", 1), /maintenance_resource_exhausted/);
    });
    await MaintenanceResources.run(f.token, () => supervised
      ? f.cache.withBoundaryProcessSpawner(realSpawner(observed), new AbortController().signal, action)
      : action()).catch(error => assert.match(String(error), /maintenance_token_invalid|maintenance_resource_exhausted|abort/i));
    if (supervised) assert.equal(observed.closed, observed.launches);
  });

  test(`${supervised ? "supervised" : "native"} streaming counts raw stdout before a zero-byte consumer cap`, async t => {
    const f = await fixture(t);
    const observed = { launches: 0, closed: 0, cancelled: 0 };
    const action = () => f.cache.readBare(f.bare, ["version"], { maxBytes: 0, timeoutMs: 5000 });
    await MaintenanceResources.run(f.token, async () => {
      MaintenanceResources.consume("metadataBytes", metadataCap - 1);
      await assert.rejects(supervised
        ? f.cache.withBoundaryProcessSpawner(realSpawner(observed), new AbortController().signal, action)
        : action());
      assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
    });
    if (supervised) assert.equal(observed.closed, observed.launches);
  });

  test(`${supervised ? "supervised" : "native"} launch cap refuses before creating a buffered child`, async t => {
    const f = await fixture(t);
    const observed = { launches: 0, closed: 0, cancelled: 0 };
    const action = () => f.cache.verifyRecoveryClosure(f.bare, f.tip);
    await MaintenanceResources.run(f.token, async () => {
      MaintenanceResources.consume("launches", 512);
      const result = await (supervised
        ? f.cache.withBoundaryProcessSpawner(realSpawner(observed), new AbortController().signal, action)
        : action()).catch(() => false);
      assert.equal(result, false);
      assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
    });
    assert.equal(observed.launches, 0);
  });
}

for (const supervised of [false, true]) {
  test(`${supervised ? "supervised" : "native"} pack import meters artifact output and all launches including runner stdin`, async t => {
    const f = await fixture(t);
    const clone = path.join(f.root, "clone");
    execFileSync("git", ["init", clone], { stdio: "pipe" });
    const observed = { launches: 0, closed: 0, cancelled: 0 };
    await MaintenanceResources.run(f.token, async () => {
      const action = () => f.cache.ensureRunnerCloneObjects(f.bare, clone, f.tip, []);
      await (supervised ? f.cache.withBoundaryProcessSpawner(realSpawner(observed), new AbortController().signal, action) : action());
      MaintenanceResources.consume("launches", 512 - 4);
      assert.throws(() => MaintenanceResources.consume("artifactBytes", 256 * 1024 ** 2), /maintenance_resource_exhausted/);
    });
    if (supervised) {
      assert.equal(observed.launches, 4);
      assert.equal(observed.closed, 4);
    }
  });
}

test("stderr refusal cancels and joins the supervised owner before releasing its reservation", async t => {
  const f = await fixture(t);
  let close!: (value: { code: number }) => void;
  const completed = new Promise<{ code: number }>(resolve => { close = resolve; });
  let cancelled!: () => void;
  const cancellation = new Promise<void>(resolve => { cancelled = resolve; });
  const stdout = new PassThrough(), stderr = new PassThrough();
  let settled = false;
  const operation = MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - 1);
    return f.cache.withBoundaryProcessSpawner(async () => {
      setImmediate(() => stderr.end(Buffer.alloc(8192)));
      return { stdin: new Writable({ write(_c, _e, done) { done(); } }), stdout, stderr, completed,
        cancel: async () => { cancelled(); await completed; } };
    }, new AbortController().signal, () => f.cache.readBare(f.bare, ["version"], { maxBytes: 0, timeoutMs: 5000 }));
  });
  const outcome = operation.then(() => { settled = true; }, error => { settled = true; return error; });
    await cancellation;
    assert.equal(settled, false);
    assert.deepEqual(await f.ledger.admit(f.input), { ok: false, reason: "maintenance_slot_active" });
    stdout.end(); close({ code: -1 });
    assert.match(String(await outcome), /maintenance_token_invalid|maintenance_resource_exhausted/);
});

test("streaming quota refusal waits for deferred supervised owner settlement", async t => {
  const f = await fixture(t);
  const stdout = new PassThrough();
  let release!: (value: { code: number }) => void;
  const completed = new Promise<{ code: number }>(resolve => { release = resolve; });
  let cancellations = 0;
  const spawner: BoundaryProcessSpawner = async () => ({
    stdout, stderr: Readable.from([]), stdin: new Writable({ write(_chunk, _encoding, done) { done(); } }),
    completed, cancel: async () => { cancellations++; await completed; },
  });
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - 1);
    let settled = false;
    const pending = f.cache.withBoundaryProcessSpawner(spawner, new AbortController().signal,
      () => f.cache.readBare(f.bare, ["version"], { maxBytes: 0, timeoutMs: 5000 }));
    const outcome = pending.then(() => { settled = true; return undefined; },
      error => { settled = true; return error as unknown; });
    await new Promise(resolve => setImmediate(resolve));
    stdout.write(Buffer.from("xx"));
    await new Promise(resolve => setImmediate(resolve));
    const beforeRelease = { settled, cancellations };
    release({ code: -1 });
    const error = await outcome;
    assert.ok(beforeRelease.cancellations > 0);
    assert.equal(beforeRelease.settled, false, "quota refusal cannot release the owned reservation before settlement");
    assert.match(String(error), /maintenance_/);
  });
});

test("RPC attempts are charged once per transport and oversized error chunks before truncation", async t => {
  const f = await fixture(t);
  let calls = 0;
  const client = new WorkerClient("http://boundary.test", "token", "test", nullLogger(), { fetch: async () => {
    calls++;
    return new Response("x".repeat(8192), { status: 503 });
  } });
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("rpcAttempts", 30);
    await assert.rejects(client.releaseRecoveryCustody("run"));
    await assert.rejects(client.releaseRecoveryCustody("run"));
    assert.equal(calls, 2);
    await assert.rejects(client.releaseRecoveryCustody("run"));
    assert.equal(calls, 2);
  });
});

test("oversized error body consumes the full returned chunk", async t => {
  const f = await fixture(t);
  const client = new WorkerClient("http://boundary.test", "token", "test", nullLogger(), { fetch: async () =>
    new Response("x".repeat(8192), { status: 503 }) });
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - 4096);
    await assert.rejects(client.releaseRecoveryCustody("run"));
    assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
  });
});

for (const status of [400, 403, 404, 409, 503]) {
  test(`global archive-upload transport counts every request and retains HTTP ${status}`, async t => {
    const f = await fixture(t);
    let calls = 0;
    t.mock.method(globalThis, "fetch", async () => { calls++; return new Response("refused", { status }); });
    const client = new WorkerClient("http://boundary.test", "token", "test", nullLogger());
    await MaintenanceResources.run(f.token, async () => {
      MaintenanceResources.consume("rpcAttempts", 31);
      const upload = () => client.uploadRecoveryBundle("run", "00000000-0000-4000-8000-000000000001",
        { byte_size: 0, checksum: sha(""), chunk_count: 0 }, Readable.from([]));
      await assert.rejects(upload(), error => (error as { status?: number }).status === status);
      assert.equal(calls, 1);
      await assert.rejects(upload());
      assert.equal(calls, 1);
    });
  });
}

for (const supervised of [false, true]) {
  test(`${supervised ? "supervised" : "native"} runner-stdin discarded stdout still consumes metadata`, async t => {
    const f = await fixture(t);
    const observed = { launches: 0, closed: 0, cancelled: 0 };
    await MaintenanceResources.run(f.token, async () => {
      MaintenanceResources.consume("metadataBytes", metadataCap - 1);
      const action = async () => {
        const importer = await f.cache["spawnGitAsRunnerWithStdin"](f.bare, ["version"]);
        importer.stdin.end();
        await importer.exited;
      };
      await (supervised ? f.cache.withBoundaryProcessSpawner(realSpawner(observed), new AbortController().signal, action) : action());
      assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
    });
    if (supervised) assert.equal(observed.closed, observed.launches);
  });
}

test("scoped discovery charges raw root entries before name validation or filtering", async t => {
  const f = await fixture(t);
  await fs.mkdir(path.join(f.root, "repos"), { recursive: true });
  await fs.writeFile(path.join(f.root, "repos", "untrusted"), "");
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("adminEntries", 4096);
    await assert.rejects(f.cache.discoverOwedCandidates(), /maintenance_resource_exhausted/);
  });
});

// Build the worker-owned on-disk public discovery input, including several logical
// generations sharing one physical pin. Expected page sizes are independent constants.
async function producerBare(f: Awaited<ReturnType<typeof fixture>>, name: string, generations: number, pinned = true) {
  const bare = path.join(f.root, "repos", name);
  await fs.mkdir(path.dirname(bare), { recursive: true });
  await fs.cp(f.bare, bare, { recursive: true });
  await fs.chmod(path.dirname(bare), 0o700);
  await fs.chmod(bare, 0o700);
  await fs.chmod(path.join(bare, "config"), 0o600);
  const owed = path.join(bare, "uzi-owed");
  await fs.mkdir(owed, { mode: 0o700 });
  for (let generation = 1; generation <= generations; generation++) {
    const context = { runId: "run", branch: "agent/test", barePath: bare, generation,
      kind: "issue", defaultIdentity: { ref: "refs/heads/main", sha: f.tip } };
    const key = sha(JSON.stringify(["run", "agent/test", bare, generation, "issue", "refs/heads/main", f.tip, "claim"]));
    const contextName = `context-${key}.json`;
    await fs.writeFile(path.join(owed, contextName), JSON.stringify({ version: 1, ...context }), { mode: 0o600 });
    if (pinned) await fs.writeFile(path.join(owed, `candidate-run-${f.tip}-${key}.json`),
      JSON.stringify({ version: 1, runId: "run", sha: f.tip, context: contextName }), { mode: 0o600 });
  }
  if (pinned) execFileSync("git", ["--git-dir", bare, "update-ref", `refs/uzi-owed/run/${f.tip}`, f.tip]);
  return bare;
}

test("producer cursor counts logical fanout, continues pages, and preserves ordinary discovery", async t => {
  const f = await fixture(t);
  await producerBare(f, "fanout.git", 7);
  await producerBare(f, "sibling.git", 2);
  const ordinary = await f.cache.discoverOwedCandidates();
  assert.equal(ordinary.length, 9);
  const cursor = f.cache.openProducerOwedDiscovery();
  t.after(() => cursor.close());
  const pages = [];
  for (let pass = 0; pass < 4; pass++) {
    const page = await cursor.next();
    pages.push(page);
    assert.equal(page.coverage, "incomplete");
    assert.equal(page.protectedInventory, false, JSON.stringify(page.protectedBares));
    if (page.scanComplete) break;
  }
  assert.deepEqual(pages.map(page => page.selected.length), [4, 4, 1]);
  const observed = pages.flatMap(page => page.selected)
    .map(item => `${item.context.barePath}:${item.context.generation}`).sort();
  assert.deepEqual(observed, ordinary.map(item => `${item.context.barePath}:${item.context.generation}`).sort());
  assert.equal(new Set(observed).size, 9);
  for (const item of pages.flatMap(page => page.selected)) {
    const expected = ordinary.find(other => other.context.barePath === item.context.barePath &&
      other.context.generation === item.context.generation)!;
    assert.deepEqual(item.candidates, expected.candidates, "shared pin provenance must remain complete");
  }
  assert.deepEqual(await f.cache.discoverOwedCandidates(), ordinary);
});

test("producer cursor bounds pinless context fanout and matches ordinary discovery", async t => {
  const f = await fixture(t);
  await producerBare(f, "pinless.git", 9, false);
  const ordinary = await f.cache.discoverOwedCandidates();
  assert.equal(ordinary.length, 9);
  assert.ok(ordinary.every(item => item.candidates.length === 0));
  const cursor = f.cache.openProducerOwedDiscovery();
  t.after(() => cursor.close());
  const pages = [];
  for (let pass = 0; pass < 4; pass++) {
    const page = await cursor.next();
    pages.push(page);
    assert.equal(page.coverage, "incomplete");
    assert.equal(page.protectedInventory, false);
    if (page.scanComplete) break;
  }
  assert.deepEqual(pages.map(page => page.selected.length), [4, 4, 1]);
  assert.equal(pages.at(-1)!.scanComplete, true);
  const observed = pages.flatMap(page => page.selected).sort((a, b) => a.context.generation! - b.context.generation!);
  assert.deepEqual(observed, ordinary);
  assert.deepEqual(await f.cache.discoverOwedCandidates(), ordinary);
});

test("producer close joins an active directory read and handle cleanup", async t => {
  const f = await fixture(t);
  await producerBare(f, "closing.git", 6);
  const deferred = () => {
    let resolve!: () => void;
    const promise = new Promise<void>(done => { resolve = done; });
    return { promise, resolve };
  };
  const readStarted = deferred();
  const releaseRead = deferred();
  const closeStarted = deferred();
  const releaseClose = deferred();
  t.after(() => { releaseRead.resolve(); releaseClose.resolve(); });
  const opendir = fs.opendir.bind(fs);
  let opened = 0, closed = 0;
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    const directory = await opendir(...args);
    if (args[0] !== path.join(f.root, "repos")) return directory;
    opened++;
    const read = directory.read.bind(directory);
    const close = directory.close.bind(directory);
    t.mock.method(directory, "read", async () => {
      readStarted.resolve();
      await releaseRead.promise;
      return read();
    });
    t.mock.method(directory, "close", async () => {
      closeStarted.resolve();
      await releaseClose.promise;
      await close();
      closed++;
    });
    return directory;
  });
  const cursor = f.cache.openProducerOwedDiscovery();
  t.after(() => cursor.close());
  const next = cursor.next();
  await readStarted.promise;
  let closeSettled = false;
  const closing = cursor.close().then(() => { closeSettled = true; });
  await Promise.resolve();
  assert.equal(closeSettled, false);
  assert.equal(closed, 0);
  releaseRead.resolve();
  await closeStarted.promise;
  assert.equal(closeSettled, false);
  assert.equal(closed, 0);
  releaseClose.resolve();
  await Promise.all([next, closing]);
  assert.equal(closeSettled, true);
  assert.equal(opened, 1);
  assert.equal(closed, 1);
  await assert.rejects(cursor.next(), /cursor closed/);
  await cursor.close();
  assert.equal(closed, 1);
});

test("producer cursor refuses an already aborted signal without opening a handle", async t => {
  const f = await fixture(t);
  const controller = new AbortController();
  controller.abort();
  let opened = 0;
  t.mock.method(fs, "opendir", async () => { opened++; throw new Error("unexpected open"); });
  const cursor = f.cache.openProducerOwedDiscovery({ signal: controller.signal });
  await assert.rejects(cursor.next(), /cursor closed/);
  await cursor.close();
  assert.equal(opened, 0);
});

test("producer cursor passes a root prefix larger than the per-pass entry budget without rescanning", async t => {
  const f = await fixture(t);
  const repos = path.join(f.root, "repos");
  await fs.mkdir(repos);
  for (let first = 0; first < 4100; first += 50) {
    await Promise.all(Array.from({ length: Math.min(50, 4100 - first) },
      (_, i) => fs.writeFile(path.join(repos, `unreadable-${first + i}`), "")));
  }
  await producerBare(f, "valid.git", 1);
  const cursor = f.cache.openProducerOwedDiscovery();
  t.after(() => cursor.close());
  const protectedPaths = new Set<string>();
  let selected = 0, complete = false, passes = 0;
  for (; passes < 140; passes++) {
    const page = await cursor.next();
    assert.ok(page.protectedBares.length <= 32);
    for (const item of page.protectedBares) {
      assert.equal(item.reason, "metadata_incomplete");
      assert.equal(protectedPaths.has(item.barePath), false);
      protectedPaths.add(item.barePath);
    }
    selected += page.selected.length;
    if (page.scanComplete) { complete = true; assert.equal(page.protectedInventory, true); break; }
  }
  assert.ok(complete);
  assert.ok(passes > 1);
  assert.equal(protectedPaths.size, 4100);
  assert.equal(selected, 1);
});

for (const corruption of ["malformed", "missing-context"] as const) {
  test(`producer refuses the entire selected bare with ${corruption} metadata`, async t => {
    const f = await fixture(t);
    const bare = await producerBare(f, "broken.git", 6);
    const owed = path.join(bare, "uzi-owed");
    const contexts = (await fs.readdir(owed)).filter(name => name.startsWith("context-")).sort();
    if (corruption === "malformed") await fs.writeFile(path.join(owed, contexts.at(-1)!), "{");
    else await fs.unlink(path.join(owed, contexts.at(-1)!));
    const cursor = f.cache.openProducerOwedDiscovery();
    t.after(() => cursor.close());
    const page = await cursor.next();
    assert.equal(page.selected.length, 0);
    assert.equal(page.protectedInventory, true);
    assert.deepEqual(page.protectedBares, [{ barePath: bare, reason: "metadata_incomplete" }]);
    assert.equal(page.coverage, "incomplete");
    await assert.rejects(f.cache.discoverOwedCandidates());
  });
}

test("producer resource refusal retains protection and cancellation closes the cursor", async t => {
  const f = await fixture(t);
  await producerBare(f, "budget.git", 1);
  const controller = new AbortController();
  const cursor = f.cache.openProducerOwedDiscovery({ signal: controller.signal });
  t.after(() => cursor.close());
  const page = await MaintenanceResources.runDiscovery(async () => {
    MaintenanceResources.consumeIfActive("adminEntries", 4096);
    return cursor.next();
  }).catch(error => {
    assert.match(String(error), /maintenance_resource_exhausted/);
    return undefined;
  });
  // An outer exhausted discovery scope also refuses its final check; the cursor's
  // sticky protection must survive into the next independent page.
  assert.equal(page, undefined);
  const final = await cursor.next();
  assert.equal(final.protectedInventory, true);
  assert.equal(final.selected.length, 0);
  assert.equal(final.scanComplete, true);
  assert.equal(final.coverage, "incomplete");
  await producerBare(f, "cancelled.git", 6);
  const cancelled = f.cache.openProducerOwedDiscovery({ signal: controller.signal });
  assert.equal((await cancelled.next()).scanComplete, false);
  controller.abort();
  await assert.rejects(cancelled.next(), /cursor closed/);
  await cancelled.close();
});

test("ACK config charges raw comments before invoking its parser", async t => {
  const f = await fixture(t);
  const bare = await producerBare(f, "ack.git", 1);
  const padding = "# " + "x".repeat(8192) + "\n";
  await fs.appendFile(path.join(bare, "config"), padding);
  let parserCalls = 0;
  const exec = f.cache["execScoped"].bind(f.cache);
  t.mock.method(f.cache as unknown as { execScoped: typeof exec }, "execScoped", async (...args: Parameters<typeof exec>) => {
    if (args[1].includes("--no-includes")) parserCalls++;
    return exec(...args);
  });
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("metadataBytes", metadataCap - 4096);
    const result = await f.cache.withReportProofBudget({ signal: new AbortController().signal, deadline: Date.now() + 10000 },
      () => f.cache.readAcknowledgedReportSourcesUnderLock(bare, "run"));
    assert.equal(result.kind, "unknown");
    assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
  });
  assert.equal(parserCalls, 0);
});

test("ACK config entries are charged before interpreting a parsed snapshot", async t => {
  const f = await fixture(t);
  const bare = await producerBare(f, "ack-entries.git", 1);
  await MaintenanceResources.run(f.token, async () => {
    MaintenanceResources.consume("adminEntries", 4096);
    const result = await f.cache.withReportProofBudget({ signal: new AbortController().signal, deadline: Date.now() + 10000 },
      () => f.cache.readAcknowledgedReportSourcesUnderLock(bare, "run"));
    assert.equal(result.kind, "unknown");
    assert.equal(MaintenanceResources.signalIfActive()?.aborted, true);
  });
});

test("discovery shares nested counters, grants no token, and refuses decode/source/artifact production", async t => {
  const f = await fixture(t);
  await MaintenanceResources.runDiscovery(async () => {
    assert.throws(() => MaintenanceResources.check(), /maintenance_token_invalid/);
    assert.throws(() => MaintenanceResources.run(f.token, () => {}), /maintenance_token_invalid/);
    MaintenanceResources.consumeIfActive("rpcAttempts", 15);
    await MaintenanceResources.runDiscovery(async () => MaintenanceResources.consumeIfActive("rpcAttempts", 1));
    assert.throws(() => MaintenanceResources.consumeIfActive("rpcAttempts", 1), /maintenance_resource_exhausted/);
  }).catch(error => assert.match(String(error), /maintenance_resource_exhausted/));
  for (const resource of ["decoded", "sourceBytes", "artifactBytes"] as const) {
    await assert.rejects(MaintenanceResources.runDiscovery(async () =>
      MaintenanceResources.consumeIfActive(resource, 0)), /maintenance_resource_exhausted/);
  }
  await assert.rejects(MaintenanceResources.runDiscovery(() => f.cache.commitWipMarker(f.bare)), /maintenance_resource_exhausted/);
  // Discovery cannot revoke or mint a producer token.
  MaintenanceResources.run(f.token, () => MaintenanceResources.check());
});
