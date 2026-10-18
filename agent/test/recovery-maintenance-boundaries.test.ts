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
