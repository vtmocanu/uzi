import test from "node:test";
import assert from "node:assert/strict";
import childProcess, { execFileSync } from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import fs from "node:fs/promises";
import path from "node:path";
import { GitCache, runnerGitCarriesWorkerMark } from "../src/git.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

const tick = () => new Promise<void>(resolve => setImmediate(resolve));
const cap = 256 * 1024 ** 2;
const packArgs = ["pack-objects", "--revs", "--stdout", "-q"];

async function fixture(t: test.TestContext) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/runner-stream-"));
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture" });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok);
  t.after(async () => {
    await ledger.finish(admitted.value, "transient");
    await fs.rm(dir, { recursive: true, force: true });
  });
  return { dir, run: <T>(action: () => T) => MaintenanceResources.run(admitted.value, action) };
}

function producer(t: test.TestContext, native: boolean) {
  const git = new GitCache("/unused", nullLogger());
  let complete!: (result: { code: number }) => void;
  const completed = new Promise<{ code: number }>(resolve => { complete = resolve; });
  let release!: () => void;
  const cancellation = new Promise<void>(resolve => { release = resolve; });
  let cancelled = 0;
  const cancelContexts: boolean[] = [];
  const requests: BoundaryProcessRequest[] = [];
  const cancel = () => { cancelled++; cancelContexts.push(MaintenanceResources.active()); };
  const child = Object.assign(new EventEmitter(), {
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: null, signalCode: null, kill: () => { cancel(); return true; },
  });
  if (native) {
    t.mock.method(childProcess, "spawn", () => child);
    syncBuiltinESMExports();
    t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
  }
  const start = (streamStdout = true) => {
    const action = () => git["spawnGitAsRunnerWithStdin"]("/unused", packArgs, { streamStdout });
    return native ? action() : git.withBoundaryProcessSpawner(async request => {
      requests.push(request);
      return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
        cancel: async () => { cancel(); await cancellation; } };
    }, new AbortController().signal, action);
  };
  const settleOwner = (code = 0) => { complete({ code }); child.emit("close", code); };
  const close = (code = 0) => {
    child.stdout.end(); child.stderr.end(); child.stdin.end();
    settleOwner(code);
  };
  return { child, start, close, settleOwner, release, requests, cancelContexts, cancelled: () => cancelled };
}

for (const native of [false, true]) {
  const lane = native ? "native" : "owned";
  test(`${lane} runner artifact stdout meters outside producer ALS and gates EOF on settlement`, async t => {
    const { run } = await fixture(t), p = producer(t, native);
    const result = await run(() => {
      MaintenanceResources.consume("metadataBytes", cap);
      return p.start();
    });
    assert.ok(result.stdout);
    result.stdin.end();
    const chunks: Buffer[] = [];
    let ended = false;
    const read = (async () => { for await (const chunk of result.stdout!) chunks.push(Buffer.from(chunk)); ended = true; })();
    p.child.stdout.end(Buffer.from("PACK"));
    await tick();
    assert.equal(Buffer.concat(chunks).toString(), "PACK");
    assert.equal(ended, false, "pipe EOF must wait for owned completion");
    p.close();
    await read;
    assert.equal((await result.exited).code, 0);
    assert.equal(p.cancelled(), 0);
    run(() => MaintenanceResources.consume("artifactBytes", cap - 4));
    assert.throws(() => run(() => MaintenanceResources.consume("artifactBytes", 1)), /maintenance_resource_exhausted/);
    assert.equal(runnerGitCarriesWorkerMark(packArgs), false);
    if (!native) {
      const request = p.requests[0];
      assert.ok(request);
      assert.equal(request.identity, "command");
      assert.equal(request.env.GIT_NO_LAZY_FETCH, "1");
      assert.equal(request.env.GIT_CONFIG_NOSYSTEM, "1");
      assert.equal(request.env.UZI_WORKER_TOKEN, undefined);
    }
  });

  test(`${lane} runner bytes followed by nonzero completion cannot end cleanly`, async t => {
    const p = producer(t, native), result = await p.start();
    assert.ok(result.stdout);
    const chunks: Buffer[] = [];
    const read = (async () => { for await (const chunk of result.stdout!) chunks.push(Buffer.from(chunk)); })();
    const failed = assert.rejects(read, /exited 7.*diagnostic/);
    p.child.stdout.write(Buffer.from("PACK"));
    await tick();
    assert.equal(Buffer.concat(chunks).toString(), "PACK");
    p.child.stderr.write("diagnostic");
    p.close(7);
    await failed;
    assert.deepEqual(await result.exited, { code: 7, stderr: "diagnostic" });
  });

  for (const explicit of [false, true]) {
    test(`${lane} runner ${explicit ? "abort" : "consumer abandonment"} cancels and joins actual settlement`, async t => {
      const { run } = await fixture(t), p = producer(t, native), result = await run(() => p.start());
      assert.ok(result.stdout);
      let settled = false;
      const exited = result.exited.then(value => { settled = true; return value; });
      if (explicit) result.abort();
      else result.stdout.destroy();
      await tick();
      assert.ok(p.cancelled() > 0);
      assert.ok(p.cancelContexts.every(Boolean), "cancellation must retain producer resource context");
      assert.equal(settled, false);
      p.close(-1);
      await tick();
      if (!native) assert.equal(settled, false, "completed alone must not outrun cancellation reap");
      p.release();
      assert.notEqual((await exited).code, 0);
    });
  }

  test(`${lane} runner refuses artifact chunk before raw read and joins cancellation`, async t => {
    const { run } = await fixture(t), p = producer(t, native);
    const result = await run(() => { MaintenanceResources.consume("artifactBytes", cap - 3); return p.start(); });
    assert.ok(result.stdout);
    let reads = 0, settled = false;
    const raw = p.child.stdout.read.bind(p.child.stdout);
    t.mock.method(p.child.stdout, "read", (size?: number) => { if (size) reads++; return raw(size); });
    const read = (async () => { for await (const _chunk of result.stdout!) assert.fail("refused bytes delivered"); })();
    const failed = assert.rejects(read, /maintenance_resource_exhausted/);
    const exited = result.exited.then(value => { settled = true; return value; });
    p.child.stdout.write(Buffer.from("four"));
    await failed;
    assert.equal(reads, 0);
    assert.ok(p.cancelled() > 0);
    assert.equal(settled, false);
    p.close();
    await tick();
    if (!native) assert.equal(settled, false);
    p.release();
    assert.notEqual((await exited).code, 0);
  });

  test(`${lane} early owner completion still joins late stdout refusal and cancellation`, async t => {
    const { run } = await fixture(t), p = producer(t, native);
    const result = await run(() => { MaintenanceResources.consume("artifactBytes", cap - 3); return p.start(); });
    assert.ok(result.stdout);
    let settled = false;
    const exited = result.exited.then(value => { settled = true; return value; });
    p.child.stderr.end();
    p.settleOwner();
    await tick();
    assert.equal(settled, false);
    const read = (async () => { for await (const _chunk of result.stdout!) assert.fail("refused bytes delivered"); })();
    const failed = assert.rejects(read, /maintenance_resource_exhausted/);
    p.child.stdout.write(Buffer.from("four"));
    await failed;
    await tick();
    assert.ok(p.cancelled() > 0);
    if (!native) assert.equal(settled, false);
    p.release();
    assert.notEqual((await exited).code, 0);
  });

  test(`${lane} runner stdout preserves backpressure`, async t => {
    const { run } = await fixture(t), p = producer(t, native), result = await run(() => p.start());
    assert.ok(result.stdout);
    const payload = Buffer.alloc(1024 * 1024, 97);
    p.child.stdout.end(payload);
    await tick(); await tick();
    assert.ok(p.child.stdout.readableLength > 0, "unread consumer must stop upstream draining");
    let bytes = 0;
    const read = (async () => { for await (const chunk of result.stdout!) bytes += chunk.length; })();
    p.close();
    await read;
    assert.equal(bytes, payload.length);
    assert.equal((await result.exited).code, 0);
  });

  test(`${lane} default runner stdout remains drained metadata`, async t => {
    const { run } = await fixture(t), p = producer(t, native), result = await run(() => p.start(false));
    assert.equal(result.stdout, undefined);
    p.child.stdout.write(Buffer.from("index-pack metadata"));
    p.close();
    assert.equal((await result.exited).code, 0);
    run(() => MaintenanceResources.consume("artifactBytes", cap));
    run(() => MaintenanceResources.consume("metadataBytes", cap - Buffer.byteLength("index-pack metadata")));
    assert.throws(() => run(() => MaintenanceResources.consume("metadataBytes", 1)), /maintenance_resource_exhausted/);
  });
}

test("real runner git produces a pack through the optional stdout seam", async t => {
  const { dir, run } = await fixture(t);
  execFileSync("git", ["init", "--bare", dir], { stdio: "pipe" });
  const oid = execFileSync("git", ["-C", dir, "hash-object", "-w", "--stdin"], { input: "fixture\n", encoding: "utf8" }).trim();
  const git = new GitCache(dir, nullLogger());
  const result = await run(() => git["spawnGitAsRunnerWithStdin"](dir, ["pack-objects", "--stdout"], { streamStdout: true }));
  assert.ok(result.stdout);
  result.stdin.end(oid + "\n");
  const chunks: Buffer[] = [];
  for await (const chunk of result.stdout) chunks.push(Buffer.from(chunk));
  assert.equal((await result.exited).code, 0);
  const pack = Buffer.concat(chunks);
  assert.equal(pack.subarray(0, 4).toString(), "PACK");
  execFileSync("git", ["-C", dir, "index-pack", "--stdin"], { input: pack, stdio: ["pipe", "pipe", "pipe"] });
});
