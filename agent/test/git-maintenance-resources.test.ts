import test from "node:test";
import assert from "node:assert/strict";
import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import { EventEmitter } from "node:events";
import { PassThrough, Readable } from "node:stream";
import fs from "node:fs/promises";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { MaintenanceResources, RecoveryMaintenance } from "../src/recovery-maintenance.js";
import { nullLogger } from "./helpers.js";

const tick = () => new Promise<void>(resolve => setImmediate(resolve));
const cap = 256 * 1024 ** 2;
async function fixture(t: test.TestContext, monotonicNow?: () => number) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/git-resources-"));
  const ledger = new RecoveryMaintenance({ root: path.join(dir, "recovery-maintenance"), workerToken: "fixture", monotonicNow });
  const admitted = await ledger.admit({
    identity: { run: "run", generation: 1, hold: "hold", repo: "repo", custodian: "worker",
      bareBasename: "repo.git", origin: "a".repeat(64), originalSource: "b".repeat(64), sourceConfig: "c".repeat(64) },
    config: RecoveryMaintenance.configuration(), exactCustody: true, terminalNonStopped: true, source: "b".repeat(64),
  });
  assert.ok(admitted.ok);
  t.after(async () => { await ledger.finish(admitted.value, "transient"); await fs.rm(dir, { recursive: true, force: true }); });
  return <T>(action: () => T) => MaintenanceResources.run(admitted.value, action);
}

type Mode = "buffered" | "streaming" | "runner stdin";
function producer(t: test.TestContext, native: boolean, mode: Mode, category?: "artifactBytes" | "closure") {
  const git = new GitCache("/unused", nullLogger());
  let cancelled = 0, complete!: (result: { code: number }) => void;
  const completed = new Promise<{ code: number }>(resolve => { complete = resolve; });
  const c = Object.assign(new EventEmitter(), {
    stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
    exitCode: null, signalCode: null, kill: () => { cancelled++; return true; },
  });
  if (native) {
    t.mock.method(childProcess, "spawn", () => c);
    syncBuiltinESMExports();
    t.after(() => { t.mock.restoreAll(); syncBuiltinESMExports(); });
  }
  const output: Buffer[] = [];
  const start = async () => {
    const action = async () => {
      if (mode === "buffered") {
        const result = await git["execScoped"]("git", ["version"], { env: {}, maxBuffer: 100 });
        output.push(Buffer.from(result.stdout));
        return 0;
      }
      if (mode === "runner stdin") {
        const result = await git["spawnGitAsRunnerWithStdin"]("/unused", ["version"]);
        result.stdin.end();
        return (await result.exited).code;
      }
      const result = await git["spawnGit"]("/unused", ["version"], undefined, { stdoutCategory: category, stderrMaxBytes: 1 });
      const drained = (async () => { for await (const chunk of result.stdout) output.push(Buffer.from(chunk)); })();
      const settled = await Promise.allSettled([drained, result.exited]);
      if (settled[0].status === "rejected") throw settled[0].reason;
      assert.equal(settled[1].status, "fulfilled");
      return settled[1].value;
    };
    return native ? action() : git.withBoundaryProcessSpawner(async () => ({
      stdin: c.stdin, stdout: c.stdout, stderr: c.stderr, completed,
      cancel: async () => { cancelled++; await completed; },
    }), new AbortController().signal, action);
  };
  const close = () => {
    c.stdout.end(); c.stderr.end();
    complete({ code: 0 }); c.emit("close", 0);
  };
  return { c, start, close, output, cancelled: () => cancelled };
}

for (const native of [false, true]) for (const mode of ["buffered", "streaming", "runner stdin"] as const) {
  for (const side of ["stdout", "stderr"] as const) {
    test(`${native ? "native" : "supervised"} ${mode} ${side} reserves before raw read outside ALS and joins close`, async t => {
      const run = await fixture(t), p = producer(t, native, mode);
      let settled = false, reads = 0;
      const pending = run(() => {
        MaintenanceResources.consume("metadataBytes", cap - 3);
        return p.start();
      }).then(code => { settled = true; return code; }, error => { settled = true; return error; });
      await tick();
      const raw = p.c[side].read.bind(p.c[side]);
      t.mock.method(p.c[side], "read", (size?: number) => { if (size !== 0) reads++; return raw(size); });
      p.c[side].write(Buffer.from("four"));
      await tick();
      const before = { reads, cancelled: p.cancelled(), settled, buffered: p.c[side].readableLength };
      if (native) p.c.emit("error", new Error("controlled early error"));
      else p.c[side].emit("error", new Error("controlled early error"));
      await tick();
      const settledOnError = settled;
      p.close();
      const result = await pending;
      assert.equal(before.reads, 0, "quota three must refuse the entire four-byte request before raw read");
      assert.equal(before.buffered, 4);
      assert.ok(before.cancelled > 0, "refusal must cancel even without an explicit recovery wrapper");
      assert.equal(before.settled, false);
      assert.equal(settledOnError, false, "error is not owned completion");
      assert.notEqual(result, 0, "late successful close cannot restore output authority");
      assert.equal(Buffer.concat(p.output).length, 0);
    });
  }
}

for (const kind of ["stdout quota", "stderr quota", "object mode", "encoded", "clean EOF"] as const) {
  test(`supervised runner stdin joins discarded output after early completion: ${kind}`, async t => {
    const run = await fixture(t), git = new GitCache("/unused", nullLogger());
    const stdout = kind === "object mode" ? Readable.from(["bad"]) : new PassThrough();
    if (kind === "encoded") stdout.setEncoding("utf8");
    const stderr = new PassThrough(), stdin = new PassThrough();
    const source = kind === "stderr quota" ? stderr : stdout;
    let reads = 0, cancelled = 0, settled = false, reaped = false;
    let reap!: () => void;
    const cancellation = new Promise<void>(resolve => { reap = () => { reaped = true; resolve(); }; });
    const raw = source.read.bind(source);
    t.mock.method(source, "read", (size?: number) => { if (size !== 0) reads++; return raw(size); });
    const result = await run(() => {
      MaintenanceResources.consume("metadataBytes", cap - 3);
      return git.withBoundaryProcessSpawner(async () => ({
        stdin, stdout, stderr, completed: Promise.resolve({ code: 0 }),
        cancel: async () => { cancelled++; await cancellation; },
      }), new AbortController().signal, () => git["spawnGitAsRunnerWithStdin"]("/unused", ["version"]));
    });
    const pending = result.exited.then(value => { settled = true; return value; });
    await tick();
    const before = settled;
    if (kind.endsWith("quota")) (source as PassThrough).write(Buffer.from("four"));
    await tick();
    const beforeReap = settled;
    if (stdout instanceof PassThrough) stdout.end();
    stderr.end(); stdin.end();
    reap();
    const value = await pending;
    assert.equal(before, false, "owned completion cannot outrun open or rejected output");
    assert.equal(beforeReap, false, "output refusal must join owned cancellation");
    if (kind === "clean EOF") {
      assert.equal(value.code, 0);
      assert.equal(cancelled, 0);
    } else {
      assert.notEqual(value.code, 0);
      assert.ok(cancelled > 0);
    }
    assert.equal(reaped, true);
    assert.equal(reads, 0, "refused output must never reach a positive raw read");
  });
}

test("exact cap then later exhaustion cannot revive streaming success", async t => {
  const run = await fixture(t), p = producer(t, false, "streaming");
  let reads = 0;
  const raw = p.c.stdout.read.bind(p.c.stdout);
  t.mock.method(p.c.stdout, "read", (size?: number) => { if (size) reads += size; return raw(size); });
  const pending = run(() => { MaintenanceResources.consume("metadataBytes", cap - 3); return p.start(); })
    .then(code => code, error => error);
  await tick();
  p.c.stdout.write(Buffer.from("abc"));
  await tick();
  assert.equal(reads, 3);
  assert.equal(Buffer.concat(p.output).toString(), "abc");
  p.c.stdout.write(Buffer.from("d"));
  await tick();
  p.close();
  assert.notEqual(await pending, 0);
  assert.equal(reads, 3);
});

for (const category of ["artifactBytes", "closure"] as const) {
  test(`streaming ${category} stdout is separate from metadata`, async t => {
    const run = await fixture(t), p = producer(t, false, "streaming", category);
    const pending = run(() => { MaintenanceResources.consume("metadataBytes", cap); return p.start(); });
    await tick();
    p.c.stdout.write(Buffer.from("four"));
    await tick();
    p.close();
    assert.equal(await pending, 0);
    assert.equal(Buffer.concat(p.output).toString(), "four");
    run(() => MaintenanceResources.consume(category === "closure" ? "decoded" : "artifactBytes",
      category === "closure" ? 8 * 1024 ** 3 : cap - 4));
    assert.throws(() => run(() => MaintenanceResources.consume(category === "closure" ? "decoded" : "artifactBytes", 1)),
      /maintenance_resource_exhausted/);
  });
}

test("artifact stdout rejects the whole requested chunk before raw read", async t => {
  const run = await fixture(t), p = producer(t, false, "streaming", "artifactBytes");
  let reads = 0;
  const raw = p.c.stdout.read.bind(p.c.stdout);
  const pending = run(() => { MaintenanceResources.consume("artifactBytes", cap - 3); return p.start(); })
    .then(code => code, error => error);
  await tick();
  t.mock.method(p.c.stdout, "read", (size?: number) => { if (size !== 0) reads++; return raw(size); });
  p.c.stdout.write(Buffer.from("four"));
  await tick();
  p.close();
  assert.notEqual(await pending, 0);
  assert.equal(reads, 0);
});

test("expiry inside raw read refuses delivery and later launches", async t => {
  let now = 0;
  const run = await fixture(t, () => now), p = producer(t, false, "buffered");
  const pending = run(() => p.start()).then(code => code, error => error);
  await tick();
  const raw = p.c.stdout.read.bind(p.c.stdout);
  t.mock.method(p.c.stdout, "read", (size?: number) => {
    const result = raw(size);
    if (size && result !== null) now = 120_000;
    return result;
  });
  p.c.stdout.write(Buffer.from("four"));
  await tick();
  assert.ok(p.cancelled() > 0);
  p.close();
  assert.notEqual(await pending, 0);
  assert.equal(Buffer.concat(p.output).length, 0);
  assert.throws(() => run(() => p.start()), /maintenance_/);
});

test("streaming stdout pulls at most 64 KiB and stops for downstream backpressure", async t => {
  const run = await fixture(t);
  const stdout = new PassThrough(), stderr = new PassThrough(), stdin = new PassThrough();
  const git = new GitCache("/unused", nullLogger());
  const sizes: number[] = [];
  const raw = stdout.read.bind(stdout);
  t.mock.method(stdout, "read", (size?: number) => { if (size) sizes.push(size); return raw(size); });
  const result = await run(() => git.withBoundaryProcessSpawner(async () => ({
    stdin, stdout, stderr, completed: Promise.resolve({ code: 0 }), cancel: async () => {},
  }), new AbortController().signal, () => git["spawnGit"]("/unused", ["version"])));
  const payload = Buffer.alloc(1024 * 1024, 97);
  stdout.end(payload); stderr.end();
  await tick(); await tick();
  const before = sizes.reduce((sum, size) => sum + size, 0);
  assert.ok(before < payload.length, "a paused downstream must stop raw consumption");
  await tick();
  assert.equal(sizes.reduce((sum, size) => sum + size, 0), before);
  const chunks: Buffer[] = [];
  for await (const chunk of result.stdout) chunks.push(Buffer.from(chunk));
  assert.deepEqual(Buffer.concat(chunks), payload);
  assert.ok(sizes.length > 0);
  assert.ok(sizes.every(size => size <= 64 * 1024));
  assert.equal(await result.exited, 0);
});

test("ordinary unscoped output keeps its existing semantics", async t => {
  const p = producer(t, false, "buffered");
  const pending = p.start();
  await tick();
  p.c.stdout.write(Buffer.from("four"));
  p.close();
  assert.equal(await pending, 0);
  assert.equal(Buffer.concat(p.output).toString(), "four");
});
