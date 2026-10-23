import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { watch } from "node:fs";
import os from "node:os";
import path from "node:path";
import http from "node:http";
import type { AddressInfo } from "node:net";
import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { createHash, createHmac } from "node:crypto";
import { RecoveryCoordinator, canonicalJson, type RecoveryRecord } from "../src/recovery.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { nullLogger } from "./helpers.js";

const linux = { skip: process.platform !== "linux" };
const token = "pass-join-fixture";
const captureId = "00000000-0000-4000-8000-000000003031";
function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(r => { resolve = r; });
  return { promise, resolve };
}
async function within<T>(pending: Promise<T>, ms = 5000): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([pending, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new assert.AssertionError({ message: "readiness/settlement exceeded " + ms })), ms);
    })]);
  } finally { clearTimeout(timer); }
}
async function fixture(t: TestContext) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-pass-join-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const recoveryRoot = path.join(root, "recovery");
  const record: RecoveryRecord = { version: 1, runId: "background", generation: 1, captureId,
    sourceSha: "a".repeat(40), kind: "issue", branch: "task", createdAt: 1, state: "bundled",
    reserveIdempotencyKey: "00000000-0000-4000-8000-000000003032" };
  const file = (r: RecoveryRecord) => path.join(recoveryRoot, r.runId, r.captureId + ".json");
  const seal = async (r: RecoveryRecord) => {
    await fs.mkdir(path.dirname(file(r)), { recursive: true });
    const key = createHmac("sha256", token).update("uzi-recovery-journal-v1").digest();
    const mac = createHmac("sha256", key).update(canonicalJson(r)).digest("hex");
    await fs.writeFile(file(r), JSON.stringify({ ...r, mac }));
    return r;
  };
  const bundle = async (r = record) => {
    const body = Buffer.from("# v2 git bundle\n" + "x".repeat(65536));
    const bundlePath = path.join(recoveryRoot, r.runId, r.captureId + ".bundle");
    await fs.mkdir(path.dirname(bundlePath), { recursive: true });
    await fs.writeFile(bundlePath, body);
    return seal({ ...r, bundlePath, byteSize: body.length,
      checksum: createHash("sha256").update(body).digest("hex"), chunkCount: 1,
      prerequisiteShas: [], selfContained: true });
  };
  return { root, recoveryRoot, record, file, seal, bundle };
}
async function server(t: TestContext, handle: http.RequestListener) {
  const instance = http.createServer(handle);
  await new Promise<void>(r => instance.listen(0, "127.0.0.1", r));
  t.after(async () => {
    instance.closeAllConnections();
    await new Promise<void>(r => instance.close(() => r()));
  });
  return new WorkerClient("http://127.0.0.1:" + (instance.address() as AddressInfo).port,
    token, "fixture", nullLogger());
}
function coordinator(f: Awaited<ReturnType<typeof fixture>>, client: WorkerClient, git = {} as GitCache) {
  return new RecoveryCoordinator({ recoveryRoot: f.recoveryRoot, workerToken: token,
    log: nullLogger(), client, git });
}

for (const stage of ["reserve", "upload", "late-budget-upload", "existing-budget-upload"] as const) {
  it("pass shutdown joins real silent " + stage + " and retains uncertain identity", linux, async t => {
    const f = await fixture(t);
    const entered = deferred(), disconnected = deferred();
    const requests: string[] = [];
    let reservation: unknown;
    let disconnect = () => {};
    const client = await server(t, (req, res) => {
      requests.push(req.url!);
      if (req.url!.endsWith("/" + (stage === "reserve" ? "reserve" : "upload"))) {
        disconnect = () => res.destroy();
        res.on("close", disconnected.resolve);
        let body = "";
        req.on("data", chunk => { body += String(chunk); });
        req.on("end", () => {
          if (stage === "reserve") reservation = JSON.parse(body);
          entered.resolve();
        });
        req.resume();
      } else res.writeHead(404).end("{}");
    });
    let uploadSettled = false;
    let observedBudget: MaintenanceBudget | undefined;
    const existingBudget = stage === "existing-budget-upload" ? new MaintenanceBudget() : undefined;
    t.after(() => existingBudget?.dispose());
    if (stage === "late-budget-upload") {
      t.mock.method(client, "getRunOwnership", async () => ({
        status: "cancelled", claim_generation: 1, inventory_guarded: false,
      }));
    }
    const upload = client.uploadRecoveryBundle.bind(client);
    t.mock.method(client, "uploadRecoveryBundle", async (...args: Parameters<typeof upload>) => {
      observedBudget = MaintenanceBudget.current();
      try { return await upload(...args); }
      finally {
        // The coordinator must join this continuation, including a budget attached by the probe.
        await new Promise<void>(resolve => setImmediate(resolve));
        uploadSettled = true;
      }
    });
    const record = await f.bundle({ ...f.record, ...(stage !== "reserve" ? { serverCaptureId: "server-capture" } : {}) });
    const before = await fs.readFile(f.file(record), "utf8");
    const bundleBefore = await fs.readFile(record.bundlePath!);
    const c = coordinator(f, client);
    const pass = existingBudget ? MaintenanceBudget.run(existingBudget, () => c.resumePending()) : c.resumePending();
    await within(entered.promise);
    if (stage === "late-budget-upload" || existingBudget) assert.ok(observedBudget, "upload has the observed/existing budget");
    const start = performance.now();
    let error: unknown;
    let settledAtShutdown = false;
    try { await within(c.shutdown()); settledAtShutdown = uploadSettled; } catch (e) { error = e; }
    // Always settle owned requests, including on the unfixed tree.
    if (error) disconnect();
    await within(disconnected.promise);
    await pass;
    if (error) throw error;
    if (stage !== "reserve") assert.equal(settledAtShutdown, true, "upload promise settled before shutdown returned");
    if (observedBudget) assert.equal(observedBudget.signal.aborted, true, "owned budget follows pass shutdown");
    t.diagnostic("shutdown join ms: " + (performance.now() - start).toFixed(1));
    assert.ok(performance.now() - start < 5000);
    assert.equal(await fs.readFile(f.file(record), "utf8"), before, "no ACK, cleanup or rewritten identity");
    assert.deepEqual(await fs.readFile(record.bundlePath!), bundleBefore, "uncertain upload retains local bytes");
    assert.equal(requests.some(p => /\/(?:release|ack)(?:\/|$)/.test(p)), false);
    if (stage === "reserve") assert.deepEqual(reservation, {
      run_id: record.runId, idempotency_key: record.reserveIdempotencyKey, source_sha: record.sourceSha, generation: 1,
    });
  });
}

it("pass shutdown reaps real bundle child before releasing the bare lock", linux, async t => {
  const f = await fixture(t);
  const bare = path.join(f.root, "repo.git");
  childProcess.execFileSync("/usr/bin/git", ["init", "--bare", "-b", "main", bare], { stdio: "pipe" });
  const tree = childProcess.execFileSync("/usr/bin/git", ["--git-dir", bare, "mktree"], { input: "", encoding: "utf8" }).trim();
  const sha = childProcess.execFileSync("/usr/bin/git", ["-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
    "--git-dir", bare, "commit-tree", tree], { input: "fixture\n", encoding: "utf8" }).trim();
  const bin = path.join(f.root, "bin"), artifact = path.join(f.root, "child.pid");
  await fs.mkdir(bin);
  await fs.writeFile(path.join(bin, "git"), "#!/bin/sh\ncase \"$*\" in *'bundle create'*) echo $$ > '" + artifact +
    "'; exec /bin/sleep 60;; esac\nexec /usr/bin/git \"$@\"\n", { mode: 0o700 });
  const started = deferred(), closed = deferred();
  const watcher = watch(f.root, (_event, name) => { if (name === "child.pid") started.resolve(); });
  t.after(() => watcher.close());
  const spawn = childProcess.spawn;
  let owned: ReturnType<typeof childProcess.spawn> | undefined;
  t.after(async () => {
    if (owned && owned.exitCode === null && owned.signalCode === null) owned.kill("SIGKILL");
    if (owned) await closed.promise;
  });
  t.mock.method(childProcess, "spawn", (command: string, args: string[], opts: childProcess.SpawnOptions) => {
    const child = spawn(command, args, { ...opts, env: { ...opts.env, PATH: bin + ":/usr/bin:/bin" } });
    if (args.includes("bundle") && args.includes("create")) {
      owned = child;
      child.once("close", closed.resolve);
    }
    return child;
  });
  syncBuiltinESMExports();
  t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
  const git = new GitCache(f.root, nullLogger());
  t.mock.method(git, "resolveRestartSource", async () => ({ status: "unpublished", barePath: bare }));
  const client = await server(t, (_req, res) => res.writeHead(404).end("{}"));
  const record = await f.seal({ ...f.record, state: "pinned", sourceSha: sha,
    finalizationPin: true, bareDir: "repo.git", defaultBranch: "main" });
  const c = coordinator(f, client, git);
  const pass = c.resumePending();
  await within(started.promise);
  assert.equal(Number(await fs.readFile(artifact, "utf8")), owned!.pid);
  let reaped = false;
  void closed.promise.then(() => { reaped = true; });
  const lock = git.withBareLock(bare, async () => { assert.equal(reaped, true, "child close precedes bare lock release"); });
  const start = performance.now();
  let failure: unknown;
  try { await within(c.shutdown()); } catch (error) { failure = error; }
  finally { if (!reaped) owned!.kill("SIGKILL"); await closed.promise; await pass; await lock; }
  if (failure) throw failure;
  assert.ok(performance.now() - start < 5000);
  assert.equal((await c.inspect(record.runId))[0]?.state, "pinned");
});

it("foreground terminal preservation keeps caller grace while the background pass aborts", linux, async t => {
  const f = await fixture(t);
  const backgroundEntered = deferred(), foregroundEntered = deferred(), backgroundClosed = deferred();
  let foregroundResponse: http.ServerResponse | undefined;
  let foregroundDisconnected = false;
  let stopBackground = () => {};
  const client = await server(t, (req, res) => {
    if (req.url!.includes("/background/") && req.url!.endsWith("/reserve")) {
      stopBackground = () => res.destroy();
      res.on("close", backgroundClosed.resolve);
      req.resume(); req.on("end", backgroundEntered.resolve);
    } else if (req.url!.includes("/foreground/") && req.url!.endsWith("/upload")) {
      foregroundResponse = res;
      res.on("close", () => { foregroundDisconnected = true; });
      req.resume(); req.on("end", foregroundEntered.resolve);
    } else res.writeHead(404).end("{}");
  });
  t.mock.method(client, "hasFeature", () => true);
  t.mock.method(client, "hasHeldPublicationV2", () => false);
  t.mock.method(client, "getRunOwnership", async () => ({ status: "cancelled", claim_generation: 1, inventory_guarded: true }));
  t.mock.method(client, "listRecoveryHolds", async (runId: string) => ({
    run_id: runId, holds: [{ generation: 1, hold_id: "hold", inventory_guarded: true }],
  }));
  const background = await f.bundle();
  const context = { runId: "foreground", generation: 1, kind: "issue" as const, branch: "task",
    barePath: path.join(f.root, "repo.git"),
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: f.record.sourceSha } };
  const foreground = await f.bundle({ ...f.record, runId: "foreground", inventoryGuarded: true,
    originalSourceSha: f.record.sourceSha, inventoryCurrentSha: f.record.sourceSha,
    originalRoots: [{ sha: f.record.sourceSha, contexts: [context] }], coverageContext: context,
    coverageDigest: "b".repeat(64), bareDir: "repo.git", serverCaptureId: "foreground-capture" });
  const c = coordinator(f, client);
  const callerBudget = new MaintenanceBudget();
  t.after(() => callerBudget.dispose());
  let foregroundSettled = false;
  const preservation = MaintenanceBudget.run(callerBudget, () => c.operateFrozenInventory({
    record: foreground, barePath: context.barePath, defaultBranch: "main",
  }, { foreground: true, terminal: { status: "failed" } })).finally(() => { foregroundSettled = true; });
  // Attach a handler before shutdown can reject either operation.
  const observed = preservation.then(value => ({ value }), error => ({ error }));
  await within(foregroundEntered.promise);
  const pass = MaintenanceBudget.run(callerBudget, () => c.resumePending(undefined, [background]));
  await within(backgroundEntered.promise);
  const started = performance.now();
  let failure: unknown;
  try {
    await within(c.shutdown());
    await within(backgroundClosed.promise);
    assert.ok(performance.now() - started < 5000);
    assert.equal(callerBudget.signal.aborted, false);
    assert.equal(foregroundDisconnected, false);
    assert.equal(foregroundSettled, false, "foreground still owns its caller grace");
  } catch (error) { failure = error; }
  finally {
    stopBackground();
    foregroundResponse!.setHeader("content-type", "application/json");
    foregroundResponse!.end(JSON.stringify({ state: "available", capture_id: "foreground-capture" }));
    await pass;
  }
  const result = await within(observed);
  if (failure) throw failure;
  assert.ok("value" in result, "foreground preservation must finish under its original caller budget");
  assert.equal(result.value.state, "uploaded");
  assert.equal((await c.inspect("foreground"))[0]?.state, "uploaded");
});

it("ordinary pass without shutdown still uploads and settles normally", linux, async t => {
  const f = await fixture(t);
  const requests: string[] = [];
  const client = await server(t, (req, res) => {
    requests.push(req.url!); req.resume();
    req.on("end", () => res.setHeader("content-type", "application/json").end(JSON.stringify(
      req.url!.endsWith("/reserve") ? { capture_id: "server-capture", state: "reserved" } : { state: "available" })));
  });
  await f.bundle();
  const c = coordinator(f, client);
  const callerBudget = new MaintenanceBudget();
  t.after(() => callerBudget.dispose());
  await MaintenanceBudget.run(callerBudget, () => c.resumePending());
  assert.equal((await c.inspect(f.record.runId))[0]?.state, "uploaded");
  assert.equal(requests.filter(p => p.endsWith("/reserve")).length, 1);
  assert.equal(requests.filter(p => p.endsWith("/upload")).length, 1);
  await c.shutdown();
  assert.equal(callerBudget.signal.aborted, false, "completed pass relinquishes its caller budget relay");
});

it("pass shutdown joins the first response-body source cancellation", linux, async t => {
  const f = await fixture(t);
  const reading = deferred(), cancellationStarted = deferred(), release = deferred();
  let sourceSettled = false, shutdownSettled = false;
  const paths: string[] = [];
  const body = new ReadableStream<Uint8Array>({
    pull() { reading.resolve(); },
    async cancel() {
      cancellationStarted.resolve();
      await release.promise;
      sourceSettled = true;
    },
  }, { highWaterMark: 0 });
  const client = new WorkerClient("http://fixture.invalid", token, "fixture", nullLogger(), {
    fetch: async input => {
      paths.push(String(input));
      return String(input).endsWith("/reserve")
        ? new Response(body, { status: 200 })
        : Response.json({ error: "not_found" }, { status: 404 });
    },
  });
  const record = await f.bundle();
  const original = await fs.readFile(f.file(record), "utf8");
  const c = coordinator(f, client);
  const pass = c.resumePending();
  let shutdown: Promise<void> | undefined;
  try {
    await within(reading.promise);
    shutdown = c.shutdown().then(() => { shutdownSettled = true; });
    await within(cancellationStarted.promise);
    // Flush continuations after the reader closes while its source cleanup is held.
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(sourceSettled, false);
    assert.equal(shutdownSettled, false, "shutdown returned before response source cancellation settled");
  } finally {
    release.resolve();
    await (shutdown ?? c.shutdown());
    await pass;
  }
  assert.equal(sourceSettled, true);
  assert.equal(await fs.readFile(f.file(record), "utf8"), original, "uncertain reserve retains its journal and identity");
  assert.equal(paths.filter(url => url.endsWith("/reserve")).length, 1);
  assert.equal(paths.some(url => /\/(?:upload|release|ack)(?:\/|$)/.test(url)), false);
});

it("pass shutdown keeps a caller budget inherited only through Git independent", linux, async t => {
  const f = await fixture(t);
  const git = new GitCache(f.root, nullLogger());
  const c = coordinator(f, new WorkerClient("http://unused.invalid", token, "fixture", nullLogger()), git);
  const entered = deferred(), release = deferred();
  let mono = 0, childBudget: MaintenanceBudget | undefined, settled = false;
  const callerBudget = new MaintenanceBudget(600_000, () => mono);
  let pass: Promise<void> | undefined, shutdown: Promise<void> | undefined;
  try {
    await MaintenanceBudget.runAttempt("foreground:1", () => callerBudget, async () => {
      MaintenanceBudget.observeCancellation();
      await git.withRecoveryOperation(callerBudget.signal, Infinity, () =>
        MaintenanceBudget.runAttempt("independent:1", () => {
          assert.fail("the independent caller must not create a new clock");
        }, async () => {
          assert.equal(MaintenanceBudget.current(), undefined, "the Git scope retains the caller clock across an inactive attempt");
          pass = c["trackPass"](undefined, async () => {
            childBudget = MaintenanceBudget.current();
            entered.resolve();
            await release.promise;
          });
          await within(entered.promise);
          mono = 100_000;
          shutdown = c.shutdown().then(() => { settled = true; });
          try {
            assert.equal(callerBudget.signal.aborted, false, "pass shutdown must not abort the Git-only caller budget");
            assert.ok(childBudget);
            assert.notEqual(childBudget, callerBudget);
            assert.equal(childBudget.signal.aborted, true, "the owned pass still receives shutdown");
            await new Promise<void>(resolve => setImmediate(resolve));
            assert.equal(settled, false, "shutdown joins the owned action");
          } finally {
            release.resolve();
            await pass;
            await shutdown;
          }
          callerBudget.check();
          assert.equal(callerBudget.remainingMs(), 500_000, "caller deadline and usability are unchanged");
        }));
    });
  } finally {
    release.resolve();
    await pass?.catch(() => undefined);
    await shutdown;
    callerBudget.dispose();
  }
});

for (const activeBudget of [false, true]) {
  it(`late headers after pass abort join source cleanup (active budget ${activeBudget})`, linux, async t => {
    const f = await fixture(t);
    const entered = deferred(), headers = deferred(), cancelling = deferred(), release = deferred();
    let cancelled = 0, shutdownReturned = false, rpcReturned = false;
    const paths: string[] = [];
    const body = new ReadableStream<Uint8Array>({
      async cancel() { cancelled++; cancelling.resolve(); await release.promise; },
    }, { highWaterMark: 0 });
    const client = new WorkerClient("http://fixture.invalid", token, "fixture", nullLogger(), {
      fetch: async input => {
        paths.push(String(input));
        entered.resolve();
        await headers.promise;
        return new Response(body);
      },
    });
    const record = await f.bundle();
    const before = await fs.readFile(f.file(record), "utf8");
    const c = coordinator(f, client);
    const budget = activeBudget ? new MaintenanceBudget() : undefined;
    const reserve = client.reserveRecoveryCapture.bind(client);
    t.mock.method(client, "reserveRecoveryCapture", async (...args: Parameters<typeof reserve>) => {
      try { return await reserve(...args); } finally { rpcReturned = true; }
    });
    const pass = budget ? MaintenanceBudget.run(budget, () => c.resumePending()) : c.resumePending();
    let shutdown: Promise<void> | undefined;
    try {
      await within(entered.promise);
      shutdown = c.shutdown().then(() => { shutdownReturned = true; });
      headers.resolve();
      await within(Promise.race([cancelling.promise, shutdown]));
      assert.equal(shutdownReturned, false, "premature shutdown settlement before source cancellation joined");
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(rpcReturned, false, "RPC must retain its caller locks while source cancellation is pending");
      assert.equal(shutdownReturned, false, "premature shutdown settlement during source cancellation");
    } finally {
      headers.resolve(); release.resolve();
      await (shutdown ?? c.shutdown()); await pass; budget?.dispose();
    }
    assert.equal(cancelled, 1);
    assert.equal(body.locked, false);
    assert.equal(await fs.readFile(f.file(record), "utf8"), before, "uncertain reservation identity stays unchanged");
    assert.equal(paths.length, 1, "no follow-on upload or ACK");
  });
}
