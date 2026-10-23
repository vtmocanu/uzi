import { after, describe, it, type TestContext } from "node:test";
import path from "node:path";
import nativeFs from "node:fs/promises";
import os from "node:os";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import assert from "node:assert/strict";
import { PassThrough, Readable } from "node:stream";
import { getEventListeners } from "node:events";
import { createHash, randomBytes } from "node:crypto";
import { crc32, deflateSync, inflateSync } from "node:zlib";
import { GitCache, PLANNING_REFUSAL_TOKENS, classifyPlanningCaptureError, gitEnv } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import { TickSpawner } from "../src/tick-spawner.js";
import { RUNNER_UID, runnerPath, runnerTmpdir, uidSplitActive } from "../src/runner-uid.js";
import { nullLogger } from "./helpers.js";
import { probeLandlockAvailability } from "../src/codex/codex-capability.js";

import { PlanningFixtureIO, planningExec, planningGitEnv } from "./planning-fixture-io.js";

const workerExec = promisify(execFile);
const defaultLimits = { sourceAllowance: 128 * 1024 * 1024, indexAllowance: 2 * 1024 * 1024,
  pathAllowance: 20000, entryAllowance: 30000, directoryAllowance: 2048,
  ignoreAllowance: 128 * 1024 * 1024, ignorePipeAllowance: 2 * 1024 * 1024 };

const cap = 512 * 1024;
const git = new GitCache(process.cwd(), nullLogger());
const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const tick = (): Promise<void> => new Promise((resolve) => setImmediate(resolve));
function deferred<T>(): { promise: Promise<T>; resolve: (value: T) => void } {
  let resolve!: (value: T) => void;
  return { promise: new Promise<T>((r) => { resolve = r; }), resolve: (v) => resolve(v) };
}
function pipe(data?: Buffer): PassThrough {
  const stream = new PassThrough();
  if (data) stream.end(data);
  return stream;
}
function fake(stdout: Readable, overrides: Partial<BoundaryProcessHandle> = {}): BoundaryProcessHandle {
  return {
    stdin: null, stdout, stderr: pipe(Buffer.alloc(0)),
    completed: Promise.resolve({ code: 0 }), cancel: async () => {},
    ...overrides,
  };
}
function run(handle: BoundaryProcessHandle, signal = new AbortController().signal): Promise<Buffer> {
  return git.withBoundaryProcessSpawner(async () => handle, signal,
    () => git.readBoundedPlanningOutput(process.cwd(), [process.execPath], 2000));
}

const fixtureIO = new PlanningFixtureIO();
const fs = fixtureIO.fs;
const exec = planningExec;
after(async () => { await fixtureIO.close(); });
// The classified outcome of a rejected capture; the raw message is never asserted for meaning.
async function refusalOf(pending: Promise<unknown>): Promise<{ refusal: string; diagnostic: string; message: string }> {
  try { await pending; } catch (error) {
    const message = (error as Error).message;
    return { ...classifyPlanningCaptureError(message), message };
  }
  assert.fail("expected the capture to be refused");
}
async function assertRefused(pending: Promise<unknown>, refusal: string, diagnostic: string): Promise<void> {
  const got = await refusalOf(pending);
  assert.deepEqual({ refusal: got.refusal, diagnostic: got.diagnostic }, { refusal, diagnostic });
}
// TickSpawner enters cwd before runnerCommand switches identity. Launch from the
// worker cwd, then enter the private clone inside the trusted Node capture helper.
function captureSpawnRequest(request: BoundaryProcessRequest): BoundaryProcessRequest {
  const argv = [...request.argv];
  assert.equal(argv[1], "-e");
  argv[2] = `process.chdir(${JSON.stringify(request.cwd)});\n` + argv[2];
  return { ...request, cwd: process.cwd(), argv };
}
async function fixture(options: {
  seed?: (seed: string) => Promise<void>; shared?: boolean; meter?: boolean; sandbox?: boolean;
  limits?: Partial<typeof defaultLimits>;
  // JS run first inside the trusted helper process; __DATA__ is the fixture directory literal.
  hook?: string;
  // Give the helper a private TMPDIR so leftover uzi-planning-* trees are observable.
  privateTmp?: boolean;
  // TickSpawner SIGTERM-to-SIGKILL grace (default 100 ms); slowed cancellation probes use 1s.
  killGraceMs?: number;
  // Add this many unreferenced tiny loose objects to the clone's object store.
  looseObjects?: number;
} = {}): Promise<{
  cache: GitCache; clone: string; bare: string; protectedRoot: string; base: string; data: string; tmp: string;
  git: (...args: string[]) => Promise<string>;
  bareGit: (...args: string[]) => Promise<string>;
  capture: (signal?: AbortSignal) => Promise<Buffer>;
  dispose: () => Promise<void>;
}> {
  const data = await fixtureIO.temporaryDirectory("planning-fixture-");
  const protectedRoot = await nativeFs.mkdtemp(path.join(await nativeFs.realpath(os.tmpdir()), "planning-protected-"));
  await nativeFs.chmod(protectedRoot, 0o755);
  let clone = "";
  try {
    const seed = path.join(data, "seed");
    clone = path.join(protectedRoot, "runner", "repo", "issue-1");
    const bare = path.join(protectedRoot, "repos", "repo.git");
    await fs.mkdir(seed);
    const run = async (cwd: string, ...args: string[]): Promise<string> =>
      (await exec("git", ["-C", cwd, ...args], { env: planningGitEnv(), timeout: 5000 })).stdout;
    await run(seed, "init", "--quiet", "--template=");
    await fs.writeFile(path.join(seed, "tracked"), "base\n");
    await fs.writeFile(path.join(seed, "deleted"), "delete me\n");
    await fs.writeFile(path.join(seed, ".gitignore"), "ignored\nignored-dir/\n");
    await options.seed?.(seed);
    await run(seed, "add", ".");
    await run(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "base");
    // Worker native IO owns the bare and protected parents. Runner IPC never
    // creates authority; temporary read access to the seed is removed afterwards.
    await fs.chmod(data, 0o755);
    await nativeFs.mkdir(path.dirname(bare), { recursive: true, mode: 0o755 });
    const bareGit = async (...args: string[]): Promise<string> => (await workerExec("git", ["-C", bare, ...args],
      { env: gitEnv(), timeout: 5000 })).stdout;
    await workerExec("git", ["clone", "--quiet", "--bare", "--no-local", seed, bare], { env: gitEnv(), timeout: 5000 });
    await fs.chmod(data, 0o700);
    await nativeFs.mkdir(path.dirname(clone), { recursive: true, mode: 0o755 });
    await nativeFs.chmod(path.dirname(clone), 0o777);
    await run(seed, "clone", "--quiet", options.shared ? "--shared" : "--no-local", bare, clone);
    await nativeFs.chmod(path.dirname(clone), 0o755);
    if (options.looseObjects) {
      // A clone keeps its fetched objects in a pack, so write the tiny loose objects directly.
      const hashing = exec("git", ["-C", clone, "hash-object", "-w", "--stdin"], { env: planningGitEnv(), timeout: 30000 });
      hashing.child.stdin?.end("loose\n");
      await hashing;
      const paths: string[] = [];
      for (let i = 0; i < options.looseObjects; i++) {
        const file = path.join(data, "loose-" + i);
        await fs.writeFile(file, "small " + i + "\n");
        paths.push(file);
      }
      const bulk = exec("git", ["-C", clone, "hash-object", "-w", "--stdin-paths"], { env: planningGitEnv(), timeout: 60000 });
      bulk.child.stdin?.end(paths.join("\n") + "\n");
      await bulk;
    }
    const base = (await run(clone, "rev-parse", "HEAD")).trim();
    const cache = new GitCache(protectedRoot, nullLogger());
    if (options.limits) cache.setPlanningLimitsForTest({ ...defaultLimits, ...options.limits });
    const tmp = path.join(data, "private-tmp");
    if (options.privateTmp) await fs.mkdir(tmp, { mode: 0o700 });
    return {
      data, tmp, clone, bare, protectedRoot, base, cache, bareGit, git: (...args) => run(clone, ...args),
      capture: async (signal = new AbortController().signal) => {
        const owner = new TickSpawner({ signal, killGraceMs: options.killGraceMs ?? 100 });
        const privateTmp = options.sandbox
          ? await fixtureIO.temporaryDirectory("planning-command-") : undefined;
        try {
          if (privateTmp) {
            await assertOwned(privateTmp, 0o700);
            // The outside probe is readable before confinement is installed.
            await fs.readFile(path.join(data, "seed", "tracked"));
          }
          return await cache.withBoundaryProcessSpawner((request) => {
            if (privateTmp) {
              const argv = [...request.argv];
              assert.equal(argv[1], "-e");
              // Prove enforced read confinement in the same process that captures the checkout.
              argv[2] = `
const denyFs = require("node:fs");
for (const denied of ["/", ${JSON.stringify(path.join(data, "seed", "tracked"))}]) {
  let deniedError;
  try { const fd = denyFs.openSync(denied, denyFs.constants.O_RDONLY); denyFs.closeSync(fd); }
  catch (error) { deniedError = error; }
  if (deniedError?.code !== "EACCES") throw Error("required sandbox did not deny " + denied);
}
for (const denied of [${JSON.stringify(path.join(bare, "config"))}, ${JSON.stringify(path.join(bare, "objects", "sandbox-write"))}]) {
  let deniedError;
  try { const fd = denyFs.openSync(denied, denyFs.constants.O_WRONLY | denyFs.constants.O_CREAT); denyFs.closeSync(fd); }
  catch (error) { deniedError = error; }
  if (deniedError?.code !== "EACCES") throw Error("required sandbox allowed bare write");
}
` + argv[2];
              return owner.spawn({
                ...request,
                // The sandbox enters its explicit clone cwd after the identity switch.
                cwd: process.cwd(),
                argv: ["/usr/local/bin/uzi-codex-command-sandbox",
                  "--cross-check", "--root", protectedRoot, "--state", privateTmp,
                  "--cwd", clone, "--", argv[0]!, ...argv.slice(1)],
                env: { ...request.env, HOME: privateTmp, TMPDIR: privateTmp },
              });
            }
            const spawnRequest = options.privateTmp ? { ...request, env: { ...request.env, TMPDIR: tmp } } : request;
            if (!options.meter && !options.hook) return owner.spawn(captureSpawnRequest(spawnRequest));
            // Instrument only the trusted inline capture program, preserving its argv and body.
            const argv = [...spawnRequest.argv];
            assert.equal(argv[1], "-e");
            const metricPath = path.join(data, "read-metrics.json");
            const hook = (options.hook ?? "").replaceAll("__DATA__", JSON.stringify(data));
            argv[2] = hook + (options.meter ? `
const meterFs = require("node:fs");
const meterCp = require("node:child_process");
const readMetrics = { source: 0, diff: 0, diffSpawned: 0, diffClosed: 0 };
const meterRead = meterFs.readSync;
meterFs.readSync = function(...args) {
  const bytes = meterRead.apply(this, args);
  readMetrics.source += bytes;
  return bytes;
};
const meterSpawn = meterCp.spawn;
meterCp.spawn = function(...args) {
  const p = meterSpawn.apply(this, args);
  if (args[1].includes("cat-file") && ["commit", "tree", "blob"].some(type => args[1].includes(type))) {
    const read = p.stdout.read;
    p.stdout.read = function(...readArgs) {
      const bytes = read.apply(this, readArgs);
      if (Buffer.isBuffer(bytes)) readMetrics.source += bytes.length;
      return bytes;
    };
  }
  if (args[1].includes("diff") && args[1].includes("--no-index")) {
    readMetrics.diffSpawned++;
    p.once("close", () => { readMetrics.diffClosed++; });
    const read = p.stdout.read;
    p.stdout.read = function(...readArgs) {
      const bytes = read.apply(this, readArgs);
      if (Buffer.isBuffer(bytes)) readMetrics.diff += bytes.length;
      return bytes;
    };
  }
  return p;
};
process.on("exit", () => meterFs.writeFileSync(${JSON.stringify(metricPath)}, JSON.stringify(readMetrics)));
` : "") + argv[2];
            return owner.spawn(captureSpawnRequest({ ...spawnRequest, argv }));
          }, signal, () => cache.capturePlanningDiff(clone, base, bare));
        } finally {
          await owner.settled();
          assert.deepEqual(owner.survivors(), []);
          if (privateTmp) await fs.rm(privateTmp, { recursive: true, force: true });
        }
      },
      dispose: async () => {
        await fs.rm(clone, { recursive: true, force: true });
        await fs.rm(data, { recursive: true, force: true });
        await nativeFs.rm(protectedRoot, { recursive: true, force: true });
      },
    };
  } catch (error) {
    if (clone) await fs.rm(clone, { recursive: true, force: true });
    await nativeFs.rm(protectedRoot, { recursive: true, force: true });
    await fs.rm(data, { recursive: true, force: true });
    throw error;
  }
}

// Assert from the runner's own IO channel; split fixture leaves stay private.
async function assertOwned(file: string, mode?: number): Promise<void> {
  const stat = await fs.stat(file);
  assert.equal(stat.uid, uidSplitActive() ? RUNNER_UID : process.getuid!());
  if (mode !== undefined) assert.equal(stat.mode & 0o777, mode);
}

// Native capture walks Linux /proc/<pid>/fd paths (src/git.ts); Linux CI runs this block.
// Snapshot bytes and ownership, excluding access times changed by legitimate reads.
async function bareInventory(root: string): Promise<unknown[]> {
  const records: unknown[] = [];
  async function visit(file: string): Promise<void> {
    const st = await nativeFs.lstat(file);
    records.push([path.relative(root, file), st.mode, st.uid, st.gid,
      st.isFile() ? createHash("sha256").update(await nativeFs.readFile(file)).digest("hex") : null]);
    if (st.isDirectory()) for (const name of (await nativeFs.readdir(file)).sort()) await visit(path.join(file, name));
  }
  await visit(root);
  return records;
}
async function proveBareReadOnly(t: TestContext, packed: boolean): Promise<void> {
  if (!uidSplitActive() || process.getuid?.() !== 10001) {
    t.skip("requires actual worker 10001 and runner 10002"); return;
  }
  const hook = [
    'const uidFs = require("node:fs"), uidPath = require("node:path");',
    'if (process.getuid() !== 10002) throw Error("not the real runner uid");',
    'const uidBare = process.argv[4];',
    'for (const target of [uidBare + "/config", uidBare + "/objects/probe", uidBare + "/refs/probe"]) {',
    '  let denied;',
    '  try { const fd = uidFs.openSync(target, uidFs.constants.O_WRONLY | uidFs.constants.O_CREAT); uidFs.closeSync(fd); }',
    '  catch (error) { denied = error; }',
    '  if (!["EACCES", "EPERM"].includes(denied?.code)) throw Error("bare write authority");',
    '}',
    'for (const target of [uidBare, uidPath.dirname(uidBare)]) {',
    '  let denied;',
    '  try { uidFs.renameSync(target, target + "-moved"); } catch (error) { denied = error; }',
    '  if (!["EACCES", "EPERM"].includes(denied?.code)) throw Error("protected rename authority");',
    '}',
    'uidFs.writeFileSync(__DATA__ + "/uid-proof", String(process.getuid()));',
  ].join("\n") + "\n";
  const f = await fixture({ hook });
  try {
    if (packed) await f.bareGit("repack", "-a", "-d");
    else {
      await nativeFs.rm(path.join(f.bare, "objects"), { recursive: true });
      await fs.chmod(f.data, 0o755);
      await nativeFs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.bare, "objects"), { recursive: true });
      await fs.chmod(f.data, 0o700);
    }
    const before = await bareInventory(path.dirname(f.bare));
    const protectedBefore = await nativeFs.stat(f.protectedRoot);
    await fs.writeFile(path.join(f.clone, "tracked"), "real runner change\n");
    assert.match((await f.capture()).toString(), /-base\n\+real runner change/);
    assert.equal(await fs.readFile(path.join(f.data, "uid-proof"), "utf8"), "10002");
    assert.deepEqual(await bareInventory(path.dirname(f.bare)), before);
    const protectedAfter = await nativeFs.stat(f.protectedRoot);
    assert.deepEqual([protectedAfter.ino, protectedAfter.mode, protectedAfter.uid, protectedAfter.gid],
      [protectedBefore.ino, protectedBefore.mode, protectedBefore.uid, protectedBefore.gid]);
  } finally { await f.dispose(); }
}

describe("Unit2 runner source capture", { skip: process.platform !== "linux" && "Linux /proc capture required" }, () => {
  it("reads loose trusted bare objects as the real runner uid without write authority", async t => {
    await proveBareReadOnly(t, false);
  });
  it("reads packed trusted bare objects as the real runner uid without write authority", async t => {
    await proveBareReadOnly(t, true);
  });
  it("cleans a fixture whose seed callback fails", async () => {
    let data = "";
    await assert.rejects(fixture({ seed: async seed => {
      data = path.dirname(seed);
      await fs.mkdir(path.join(data, "partial"), { mode: 0o700 });
      throw new Error("seed setup failed");
    } }), /seed setup failed/);
    assert.ok(data);
    await assert.rejects(fs.stat(data), { code: "ENOENT" });
  });

  it("owns fixture roots, private tmp, late state and capture hook outputs", async () => {
    const f = await fixture({ privateTmp: true, meter: true,
      hook: 'require("node:fs").writeFileSync(__DATA__ + "/hook.json", JSON.stringify({ cwd: process.cwd(), uid: process.getuid() }));' });
    try {
      await assertOwned(f.data, 0o700);
      await assertOwned(f.tmp, 0o700);
      await fs.mkdir(path.join(f.data, "late-state"), { mode: 0o700 });
      await fs.writeFile(path.join(f.clone, "tracked"), "owned change\\n");
      await f.git("add", "tracked");
      await f.capture();
      assert.deepEqual(JSON.parse(await fs.readFile(path.join(f.data, "hook.json"), "utf8")), {
        cwd: f.clone, uid: uidSplitActive() ? RUNNER_UID : process.getuid!(),
      });
      for (const file of [f.clone, path.join(f.clone, ".git/index"),
        path.join(f.data, "hook.json"), path.join(f.data, "read-metrics.json")]) await assertOwned(file);
      await assertOwned(path.join(f.data, "late-state"), 0o700);
    } finally { await f.dispose(); }
  });

  it("captures under the enforced required command sandbox", async (t) => {
    if (process.platform !== "linux") { t.skip("Linux command sandbox required"); return; }
    try { await fs.access("/usr/local/bin/uzi-codex-command-sandbox", fs.constants.X_OK); }
    catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") throw error;
      t.skip("fixed command sandbox binary absent"); return;
    }
    const probe = probeLandlockAvailability();
    if (probe === "unavailable") { t.skip("kernel Landlock unavailable"); return; }
    assert.equal(probe, "available");
    const f = await fixture({ sandbox: true });
    try {
      await fs.writeFile(path.join(f.clone, "tracked"), "sandbox change\n");
      assert.match((await f.capture()).toString(), /-base\n\+sandbox change/);
    } finally { await f.dispose(); }
  });

  for (const direction of ["file to directory", "directory to file"]) {
    for (const staged of [false, true]) {
      it(`captures ${direction} replacement (${staged ? "staged" : "unstaged"})`, async () => {
        const fileFirst = direction === "file to directory";
        const f = await fixture({ seed: async seed => {
          if (!fileFirst) await fs.mkdir(path.join(seed, "a"));
          await fs.writeFile(path.join(seed, fileFirst ? "a" : "a/b"), "old replacement\n");
        } });
        try {
          await fs.rm(path.join(f.clone, "a"), { recursive: true });
          if (fileFirst) await fs.mkdir(path.join(f.clone, "a"));
          await fs.writeFile(path.join(f.clone, fileFirst ? "a/b" : "a"), "new replacement\n");
          if (staged) await f.git("add", "-A");
          const patch = await f.capture();
          const text = patch.toString();
          assert.match(text, /deleted file mode 100644/);
          assert.match(text, /new file mode 100644/);
          assert.match(text, /-old replacement/);
          assert.match(text, /\+new replacement/);
          // Applying the public patch must reproduce the new path shape and bytes.
          const target = path.join(f.data, "apply");
          await exec("git", ["clone", "--quiet", "--no-local", path.join(f.data, "seed"), target],
            { env: planningGitEnv(), timeout: 5000 });
          const patchPath = path.join(f.data, "replacement.patch");
          await fs.writeFile(patchPath, patch);
          await exec("git", ["-C", target, "apply", patchPath], { env: planningGitEnv(), timeout: 5000 });
          assert.equal(await fs.readFile(path.join(target, fileFirst ? "a/b" : "a"), "utf8"), "new replacement\n");
          if (!fileFirst) await assert.rejects(fs.stat(path.join(target, "a/b")), { code: "ENOTDIR" });
        } finally { await f.dispose(); }
      });
    }
  }

  for (const kind of ["symlink to file", "symlink to directory", "dangling symlink", "fifo"]) {
    for (const nested of [false, true]) {
      it(`refuses ${kind} replacing a tracked ${nested ? "ancestor" : "leaf"}`, async () => {
        const f = await fixture({ seed: async seed => {
          if (nested) await fs.mkdir(path.join(seed, "a"));
          await fs.writeFile(path.join(seed, nested ? "a/b" : "a"), "old replacement\n");
        } });
        try {
          await fs.rm(path.join(f.clone, "a"), { recursive: true });
          const outside = path.join(f.data, "outside");
          if (kind === "symlink to directory") {
            await fs.mkdir(outside);
            await fs.writeFile(path.join(outside, "b"), "outside\n");
          } else if (kind === "symlink to file") await fs.writeFile(outside, "outside\n");
          if (kind === "fifo") await exec("mkfifo", [path.join(f.clone, "a")], { timeout: 5000 });
          else await fs.symlink(outside, path.join(f.clone, "a"));
          const got = await refusalOf(f.capture());
          assert.equal(got.refusal, "unsupported_entry");
          assert.equal(got.diagnostic, kind === "fifo" ? (nested ? "non_directory_ancestor" : "nonregular_source")
            : (nested ? "non_directory_ancestor" : "unsupported_symlink"));
          assert.doesNotMatch(got.message, /outside/);
        } finally { await f.dispose(); }
      });
    }
  }

  for (const kind of ["outside loose symlink", "outside loose directory symlink", "forged blob", "forged commit", "forged tree", "packed pack symlink", "packed idx symlink", "packed directory symlink"]) {
    it("authenticates trusted bare object integrity: " + kind, async () => {
      const f = await fixture();
      let forged: { type: string; oid: string } | undefined;
      try {
        if (!kind.startsWith("packed")) {
          // Force a loose-only baseline so Git cannot prefer an authentic packed duplicate.
          await nativeFs.rm(path.join(f.bare, "objects"), { recursive: true });
          await fs.chmod(f.data, 0o755);
          await nativeFs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.bare, "objects"), { recursive: true });
        }
        await fs.unlink(path.join(f.clone, "tracked"));
        assert.match((await f.capture()).toString(), /-base/);
        if (kind.startsWith("packed")) {
          await f.bareGit("repack", "-a", "-d");
          assert.match((await f.capture()).toString(), /-base/);
          const packDir = path.join(f.bare, "objects/pack");
          const suffix = kind.includes("idx") ? ".idx" : ".pack";
          const name = (await nativeFs.readdir(packDir)).find(name => name.endsWith(suffix))!;
          const object = path.join(packDir, name), outside = path.join(f.data, "outside-pack");
          if (kind === "packed directory symlink") {
            await nativeFs.cp(packDir, outside, { recursive: true });
            await nativeFs.rm(packDir, { recursive: true });
            await nativeFs.symlink(outside, packDir);
          } else {
            await nativeFs.copyFile(object, outside);
            await nativeFs.unlink(object);
            await nativeFs.symlink(outside, object);
          }
        } else {
          const type = kind.includes("commit") ? "commit" : kind.includes("tree") ? "tree" : "blob";
          const oid = (await f.bareGit("rev-parse", type === "commit" ? "HEAD" : type === "tree" ? "HEAD^{tree}" : "HEAD:tracked")).trim();
          if (kind.startsWith("forged")) forged = { type, oid };
          let body: Buffer;
          if (type === "commit") body = Buffer.from((await f.bareGit("cat-file", "commit", oid)) + "forged message\n");
          else if (type === "tree") {
            // An otherwise valid tree under the original OID points tracked at a different authentic blob.
            const other = (await f.bareGit("rev-parse", "HEAD:deleted")).trim();
            body = Buffer.concat([Buffer.from("100644 tracked\0"), Buffer.from(other, "hex")]);
          } else body = Buffer.from(kind.startsWith("outside") ? "base\n" : "evil\n"); // Same size; symlink controls use authentic bytes.
          assert.equal(createHash("sha1").update(type + " " + body.length + "\0").update(body).digest("hex") === oid,
            kind.startsWith("outside"));
          const bytes = deflateSync(Buffer.concat([Buffer.from(type + " " + body.length + "\0"), body]));
          const object = path.join(f.bare, "objects", oid.slice(0, 2), oid.slice(2));
          await nativeFs.mkdir(path.dirname(object), { recursive: true });
          await nativeFs.rm(object, { force: true });
          if (kind === "outside loose directory symlink") {
            // Redirect an authentic fanout directory, not just the final object file.
            await nativeFs.writeFile(object, bytes);
            const outside = path.join(f.data, "outside-fanout");
            await nativeFs.cp(path.dirname(object), outside, { recursive: true });
            await nativeFs.rm(path.dirname(object), { recursive: true });
            await nativeFs.symlink(outside, path.dirname(object));
          } else if (kind === "outside loose symlink") {
            const outside = path.join(f.data, "outside-object");
            await nativeFs.writeFile(outside, bytes);
            await nativeFs.symlink(outside, object);
          } else if (type === "blob") await nativeFs.writeFile(object, bytes);
          else {
            // A valid pack/idx checksum does not authenticate the idx's object identity.
            // Keep other baseline objects loose; substitute this commit/tree through a forged idx.
            const header = Buffer.alloc(12);
            header.write("PACK"); header.writeUInt32BE(2, 4); header.writeUInt32BE(1, 8);
            let size = body.length;
            const encoding = [(type === "commit" ? 1 : 2) * 16 + (size & 15)];
            size >>>= 4;
            while (size) {
              encoding[encoding.length - 1]! |= 128;
              encoding.push(size & 127); size >>>= 7;
            }
            const entry = Buffer.concat([Buffer.from(encoding), deflateSync(body)]);
            const payload = Buffer.concat([header, entry]), packHash = createHash("sha1").update(payload).digest();
            const pack = Buffer.concat([payload, packHash]);
            const idx = Buffer.alloc(8 + 256 * 4 + 20 + 4 + 4 + 20);
            idx.writeUInt32BE(0xff744f63, 0); idx.writeUInt32BE(2, 4);
            for (let i = parseInt(oid.slice(0, 2), 16); i < 256; i++) idx.writeUInt32BE(1, 8 + i * 4);
            Buffer.from(oid, "hex").copy(idx, 1032);
            idx.writeUInt32BE(crc32(entry), 1052); idx.writeUInt32BE(12, 1056);
            packHash.copy(idx, 1060);
            const packDir = path.join(f.bare, "objects/pack");
            await nativeFs.mkdir(packDir, { recursive: true });
            const stem = path.join(packDir, "pack-" + packHash.toString("hex"));
            await nativeFs.writeFile(stem + ".pack", pack);
            await nativeFs.writeFile(stem + ".idx", Buffer.concat([idx, createHash("sha1").update(idx).digest()]));
          }
        }
        if (forged) {
          // Prove Git serves the forged body; rejection must come from capture authentication.
          const { type, oid } = forged;
          const served = (await workerExec("git", ["-C", f.bare, "cat-file", type, oid],
            { env: planningGitEnv(), timeout: 5000, encoding: "buffer" })).stdout;
          assert.notEqual(createHash("sha1").update(type + " " + served.length + "\0").update(served).digest("hex"), oid);
        }
        if (kind.startsWith("forged")) await assertRefused(f.capture(), "base_unavailable", "base_object_integrity");
        else assert.match((await f.capture()).toString(), /-base/);
      } finally { await f.dispose(); }
    });
  }

  it("captures legitimate loose and repacked objects", async () => {
    const f = await fixture();
    try {
      await fs.rm(path.join(f.clone, ".git/objects"), { recursive: true });
      await fs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.clone, ".git/objects"), { recursive: true });
      // Materialize a loose blob and commit through Git's public object-writing interface.
      await fs.writeFile(path.join(f.clone, "tracked"), "loose change\n");
      await f.git("add", "tracked");
      await f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "loose");
      assert.match((await f.capture()).toString(), /-base\n\+loose change/);
      await f.git("repack", "-a", "-d");
      assert.match((await f.capture()).toString(), /-base\n\+loose change/);
    } finally { await f.dispose(); }
  });

  it("refuses non-UTF8 base metadata even after the source path is deleted", async () => {
    const f = await fixture({ seed: async seed => {
      const name = Buffer.concat([Buffer.from(seed + "/"), Buffer.from([0xff])]);
      await fs.writeFile(name, "base text\n");
    } });
    try {
      await fs.unlink(Buffer.concat([Buffer.from(f.clone + "/"), Buffer.from([0xff])]));
      await f.git("add", "-u");
      await assert.rejects(f.capture(), /UZI-PLANNING-REFUSAL non_utf8_path/);
    } finally { await f.dispose(); }
  });
  it("captures committed, staged, unstaged, deleted and nonignored untracked text against the immutable base", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.clone, "tracked"), "committed\n");
      await f.git("add", "tracked");
      await f.git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "change");
      await fs.writeFile(path.join(f.clone, "staged"), "staged\n");
      await f.git("add", "staged");
      await fs.writeFile(path.join(f.clone, "staged"), "unstaged\n");
      await fs.unlink(path.join(f.clone, "deleted"));
      await fs.writeFile(path.join(f.clone, "fresh"), "fresh text\n");
      await fs.writeFile(path.join(f.clone, "ignored"), "ignored text\n");
      await fs.mkdir(path.join(f.clone, "ignored-dir"));
      await fs.symlink("/does-not-exist", path.join(f.clone, "ignored-dir", "escape"));
      await fs.writeFile(path.join(f.clone, "binary"), Buffer.from([0, 1, 2, 3]));
      const patch = (await f.capture()).toString();
      assert.match(patch, /-base\n\+committed/);
      assert.match(patch, /\+unstaged/);
      assert.match(patch, /-delete me/);
      assert.match(patch, /\+fresh text/);
      assert.doesNotMatch(patch, /ignored text|binary|\+staged/);
      assert.match(patch, /diff --git a\/tracked b\/tracked/);
    } finally { await f.dispose(); }
  });

  it("reads changed bytes despite a forged matching index stat cache and valid checksum", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.clone, "tracked"), "evil\n");
      await fs.chmod(path.join(f.clone, "tracked"), 0o644);
      const st = await fs.stat(path.join(f.clone, "tracked"), { bigint: true });
      const indexPath = path.join(f.clone, ".git/index");
      const index = await fs.readFile(indexPath);
      let offset = 12;
      let forged = false;
      for (let i = 0; i < index.readUInt32BE(8); i++) {
        const end = index.indexOf(0, offset + 62);
        const name = index.toString("utf8", offset + 62, end);
        if (name === "tracked") {
          const mask = 0xffffffffn;
          const fields = [
            st.ctimeNs / 1000000000n, st.ctimeNs % 1000000000n,
            st.mtimeNs / 1000000000n, st.mtimeNs % 1000000000n,
            st.dev, st.ino, st.mode, st.uid, st.gid, st.size,
          ];
          fields.forEach((value, j) => index.writeUInt32BE(Number(value & mask), offset + j * 4));
          forged = true;
        }
        offset += Math.ceil((end + 1 - offset) / 8) * 8;
      }
      assert.equal(forged, true);
      createHash("sha1").update(index.subarray(0, -20)).digest().copy(index, index.length - 20);
      await fs.writeFile(indexPath, index);
      const later = new Date(Number((st.mtimeNs > st.ctimeNs ? st.mtimeNs : st.ctimeNs) / 1000000n) + 2000);
      await fs.utimes(indexPath, later, later);
      assert.match((await f.capture()).toString(), /-base\n\+evil/);
    } finally { await f.dispose(); }
  });

  it("captures text and .gitattributes itself when repository attributes disable diff", async () => {
    const f = await fixture({ seed: async seed => {
      await fs.writeFile(path.join(seed, ".gitattributes"), "* -diff\n");
    } });
    try {
      await fs.writeFile(path.join(f.clone, "tracked"), "visible change\n");
      await fs.writeFile(path.join(f.clone, ".gitattributes"), "* -diff\n# changed attributes\n");
      const patch = (await f.capture()).toString();
      assert.match(patch, /\+visible change/);
      assert.match(patch, /\+# changed attributes/);
    } finally { await f.dispose(); }
  });

  it("refuses more than 200 nonignored untracked paths before reading contents", async () => {
    const f = await fixture();
    try {
      for (let i = 0; i < 201; i++) await fs.writeFile(path.join(f.clone, "extra-" + i), "x");
      await assertRefused(f.capture(), "too_many_untracked", "untracked_path_cap");
    } finally { await f.dispose(); }
  });

  it("bounds source bytes even when huge binary content would produce no patch", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.clone, "huge"), Buffer.alloc(4 * 1024 * 1024 + 1));
      await assertRefused(f.capture(), "diff_too_large", "source_cap");
    } finally { await f.dispose(); }
  });

  it("captures 33 unchanged 4 MiB baseline files at default limits", async () => {
    const f = await fixture({ meter: true, seed: async seed => {
      const text = Buffer.alloc(4 * 1024 * 1024, 120);
      for (let i = 0; i < 33; i++) await fs.writeFile(path.join(seed, "unchanged-" + i), text);
    } });
    try {
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "tracked"), "small edit\n");
      const expected = await f.git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--", "tracked");
      assert.match(expected, /-base\n\+small edit/);
      assert.equal((await f.capture()).toString(), expected);
    } finally { await f.dispose(); }
  });

  it("captures unchanged baseline text across 1030 sibling one-file directories at default limits", async () => {
    const f = await fixture({ seed: async seed => {
      for (let i = 0; i < 1030; i++) {
        const directory = path.join(seed, "sibling-" + String(i).padStart(4, "0"));
        await fs.mkdir(directory);
        await fs.writeFile(path.join(directory, "file"), "unchanged\n");
      }
    } });
    try { assert.deepEqual(await f.capture(), Buffer.alloc(0)); }
    finally { await f.dispose(); }
  });

  it("refuses patch overflow without returning partial output", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.clone, "text"), "x\n".repeat(180000));
      await assert.rejects(f.capture(), /process failed|exceeded/);
    } finally { await f.dispose(); }
  });

  it("bounds actual inner diff reads across separate patches and reaps every diff child", async () => {
    const f = await fixture({ meter: true });
    try {
      // Each patch is about 200 KiB; only their combined output exceeds the cap.
      for (let i = 0; i < 3; i++)
        await fs.writeFile(path.join(f.clone, "patch-" + i), ("x".repeat(199) + "\n").repeat(1000));
      await assert.rejects(f.capture(), /UZI-PLANNING-REFUSAL (git_pipe_cap|patch_cap)/);
      const metrics = JSON.parse(await fs.readFile(path.join(f.data, "read-metrics.json"), "utf8"));
      console.log("aggregate inner diff actual reads:", metrics.diff);
      assert.equal(metrics.diff, cap + 1);
      assert.equal(metrics.diffSpawned, 3);
      assert.equal(metrics.diffClosed, metrics.diffSpawned);
    } finally { await f.dispose(); }
  });

  it("captures a baseline with 5461 tracked paths, an index over 512 KiB and 100 MiB of unchanged text", async () => {
    const f = await fixture({ seed: async seed => {
      const large = Buffer.alloc(2 * 1024 * 1024, 120);
      await fs.mkdir(path.join(seed, "many"));
      for (let i = 0; i < 50; i++) await fs.writeFile(path.join(seed, "large-" + i), large);
      for (let i = 0; i < 5408; i++)
        await fs.writeFile(path.join(seed, "many", "normal-repository-tracked-file-path-" + String(i).padStart(5, "0")), "unchanged\n");
    } });
    try {
      assert.ok((await fs.stat(path.join(f.clone, ".git/index"))).size > cap);
      assert.equal((await f.capture()).length, 0);
      await fs.writeFile(path.join(f.clone, "tracked"), "normal repository change\n");
      assert.match((await f.capture()).toString(), /\+normal repository change/);
    } finally { await f.dispose(); }
  });

  it("captures a textual change in a 2 MiB tracked file", async () => {
    const f = await fixture({ seed: async seed => {
      await fs.writeFile(path.join(seed, "large"), "baseline\n" + "unchanged\n".repeat(240000));
    } });
    try {
      await fs.writeFile(path.join(f.clone, "large"), "changed\n" + "unchanged\n".repeat(240000));
      assert.match((await f.capture()).toString(), /-baseline\n\+changed/);
    } finally { await f.dispose(); }
  });

  it("captures a direct shared clone and refuses after its trusted bare is removed", async () => {
    const f = await fixture({ shared: true });
    try {
      assert.ok((await fs.readFile(path.join(f.clone, ".git/objects/info/alternates"))).length > 0);
      assert.equal((await f.capture()).length, 0);
      await fs.writeFile(path.join(f.clone, "tracked"), "shared change\n");
      assert.match((await f.capture()).toString(), /-base\n\+shared change/);
      await nativeFs.rm(f.bare, { recursive: true });
      await assertRefused(f.capture(), "base_unavailable", "base_object_type_mismatch");
    } finally { await f.dispose(); }
  });

  it("ignores hostile local config includes even when config itself is a FIFO", async () => {
    const f = await fixture();
    try {
      const fifo = path.join(f.data, "hostile-config");
      await exec("mkfifo", [fifo], { env: planningGitEnv(), timeout: 5000 });
      await f.git("config", "include.path", fifo);
      await fs.writeFile(path.join(f.clone, "tracked"), "include-safe change\n");
      assert.match((await f.capture()).toString(), /\+include-safe change/);
      await fs.unlink(path.join(f.clone, ".git/config"));
      await exec("mkfifo", [path.join(f.clone, ".git/config")], { env: planningGitEnv(), timeout: 5000 });
      assert.match((await f.capture()).toString(), /\+include-safe change/);
    } finally { await f.dispose(); }
  });

  it("refuses leaf and parent symlink escapes, nonregular ignores and an unavailable base", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.data, "outside"), "outside content\n");
      await fs.symlink(path.join(f.data, "outside"), path.join(f.clone, "escape"));
      await assertRefused(f.capture(), "unsupported_entry", "unsupported_symlink");
      await fs.unlink(path.join(f.clone, "escape"));
      await fs.mkdir(path.join(f.clone, "parent"));
      await fs.writeFile(path.join(f.clone, "parent", "file"), "inside\n");
      await f.git("add", "parent/file");
      await fs.rm(path.join(f.clone, "parent"), { recursive: true });
      await fs.symlink(f.data, path.join(f.clone, "parent"));
      await assertRefused(f.capture(), "unsupported_entry", "non_directory_ancestor");
      await fs.unlink(path.join(f.clone, "parent"));
      await fs.mkdir(path.join(f.clone, ".gitignore-bad"));
      await fs.unlink(path.join(f.clone, ".gitignore"));
      await fs.symlink(path.join(f.data, "outside"), path.join(f.clone, ".gitignore"));
      // A tracked .gitignore replaced by a link is a changed symlink candidate, never read through.
      await assertRefused(f.capture(), "unsupported_entry", "unsupported_symlink");
      await fs.unlink(path.join(f.clone, ".gitignore"));
      await fs.writeFile(path.join(f.clone, ".gitignore"), "");
      const ac = new AbortController(), owner = new TickSpawner({ signal: ac.signal });
      await assert.rejects(f.cache.withBoundaryProcessSpawner(req => owner.spawn(captureSpawnRequest(req)), ac.signal,
        () => f.cache.capturePlanningDiff(f.clone, "0".repeat(40), f.bare)), /UZI-PLANNING-REFUSAL base_object_type_mismatch/);
      await owner.settled();
      assert.deepEqual(owner.survivors(), []);
    } finally { await f.dispose(); }
  });


  // Tracked symlinks (mode 120000): unchanged ones are skipped without ever following the target.
  const seedLinks = async (seed: string): Promise<void> => {
    await fs.mkdir(path.join(seed, "sub"));
    await fs.symlink("tracked", path.join(seed, "link"));
    await fs.symlink("/does/not/exist", path.join(seed, "dangling"));
    await fs.symlink("..", path.join(seed, "dir-link"));
    await fs.symlink("../tracked", path.join(seed, "sub/nested-link"));
  };

  it("captures an unchanged tracked symlink tree as an empty patch", async () => {
    const f = await fixture({ seed: seedLinks });
    try {
      assert.equal((await fs.lstat(path.join(f.clone, "link"))).isSymbolicLink(), true);
      assert.equal((await f.capture()).length, 0);
    } finally { await f.dispose(); }
  });

  it("captures edits elsewhere next to unchanged tracked symlinks", async () => {
    const f = await fixture({ seed: seedLinks });
    try {
      await fs.writeFile(path.join(f.clone, "tracked"), "edited\n");
      await fs.writeFile(path.join(f.clone, "fresh"), "fresh text\n");
      const patch = (await f.capture()).toString();
      assert.match(patch, /-base\n\+edited/);
      assert.match(patch, /\+fresh text/);
      assert.doesNotMatch(patch, /link|dangling/);
    } finally { await f.dispose(); }
  });

  it("treats a symlinked .gitignore as absent while the path stays a candidate", async () => {
    const f = await fixture({ seed: async seed => {
      await fs.rm(path.join(seed, ".gitignore"));
      await fs.writeFile(path.join(seed, "ignore-rules"), "ignored\n");
      await fs.symlink("ignore-rules", path.join(seed, ".gitignore"));
    } });
    try {
      assert.equal((await f.capture()).length, 0);
      await fs.writeFile(path.join(f.clone, "ignored"), "not ignored without rules\n");
      await fs.writeFile(path.join(f.clone, "tracked"), "edited\n");
      const patch = (await f.capture()).toString();
      assert.match(patch, /\+edited/);
      assert.match(patch, /\+not ignored without rules/);
      // Retargeting the link itself is a changed symlink and is refused.
      await fs.rm(path.join(f.clone, ".gitignore"));
      await fs.symlink("tracked", path.join(f.clone, ".gitignore"));
      await assertRefused(f.capture(), "unsupported_entry", "unsupported_symlink");
    } finally { await f.dispose(); }
  });

  for (const scenario of [
    "retargeted", "non-UTF8 retarget", "staged added", "untracked", "deleted", "staged deleted",
    "file to link", "link to file", "index mode differs from base", "nested retargeted",
  ]) {
    it(`refuses a changed symlink as unsupported_entry: ${scenario}`, async () => {
      const f = await fixture({ seed: seedLinks });
      try {
        const link = path.join(f.clone, "link");
        switch (scenario) {
          case "retargeted": await fs.rm(link); await fs.symlink("deleted", link); break;
          case "non-UTF8 retarget": await fs.rm(link); await fs.symlink(Buffer.from([0x74, 0xff, 0x6b]), link); break;
          case "nested retargeted":
            await fs.rm(path.join(f.clone, "sub/nested-link"));
            await fs.symlink("../deleted", path.join(f.clone, "sub/nested-link"));
            break;
          case "staged added":
            await fs.symlink("tracked", path.join(f.clone, "added-link"));
            await f.git("add", "added-link");
            break;
          case "untracked": await fs.symlink("tracked", path.join(f.clone, "added-link")); break;
          case "deleted": await fs.rm(link); break;
          case "staged deleted": await f.git("rm", "-q", "link"); break;
          case "file to link": {
            await fs.rm(path.join(f.clone, "tracked"));
            await fs.symlink("link", path.join(f.clone, "tracked"));
            break;
          }
          case "link to file": await fs.rm(link); await fs.writeFile(link, "tracked\n"); break;
          case "index mode differs from base": {
            const oid = (await f.git("rev-parse", "HEAD:tracked")).trim();
            await f.git("update-index", "--cacheinfo", `100644,${oid},link`);
            break;
          }
        }
        const got = await refusalOf(f.capture());
        assert.equal(got.refusal, "unsupported_entry");
        assert.equal(got.diagnostic, "unsupported_symlink");
      } finally { await f.dispose(); }
    });
  }

  it("never reads a symlink target's content", async () => {
    const f = await fixture({ meter: true });
    try {
      await fs.writeFile(path.join(f.data, "outside"), Buffer.alloc(3 * 1024 * 1024, 111));
      await fs.symlink(path.join(f.data, "outside"), path.join(f.clone, "escape"));
      const got = await refusalOf(f.capture());
      assert.equal(got.refusal, "unsupported_entry");
      const metrics = JSON.parse(await fs.readFile(path.join(f.data, "read-metrics.json"), "utf8"));
      assert.ok(metrics.source < 256 * 1024, "target bytes were not read: " + metrics.source);
    } finally { await f.dispose(); }
  });

  // Test-only instrumentation of the trusted helper process: record every readlink and whether
  // the descriptor named by its pinned proc-fd parent path was still open at that moment.
  const linkHook = (swap: boolean): string => `
const hookFs = require("node:fs");
const hookOpen = new Set(), hookLog = [];
const hookOpenSync = hookFs.openSync, hookCloseSync = hookFs.closeSync, hookReadlink = hookFs.readlinkSync;
const hookPinned = new RegExp("^/" + "proc/\\\\d+/fd/(\\\\d+)/");
hookFs.openSync = function(...args) { const fd = hookOpenSync.apply(this, args); hookOpen.add(fd); return fd; };
hookFs.closeSync = function(fd) { hookOpen.delete(fd); return hookCloseSync.call(this, fd); };
hookFs.readlinkSync = function(target, ...rest) {
  const match = hookPinned.exec(String(target));
  hookLog.push({ pinned: Boolean(match), parentOpen: match ? hookOpen.has(Number(match[1])) : false });
  ${swap ? 'hookFs.unlinkSync(target); hookFs.writeFileSync(target, "swapped to a regular file\\n");' : ""}
  return hookReadlink.call(this, target, ...rest);
};
process.on("exit", () => hookFs.writeFileSync(__DATA__ + "/hook.json", JSON.stringify(hookLog)));
`;

  it("reads each symlink only through its still-open pinned parent descriptor", async () => {
    const f = await fixture({ seed: seedLinks, hook: linkHook(false) });
    try {
      assert.equal((await f.capture()).length, 0);
      const log = JSON.parse(await fs.readFile(path.join(f.data, "hook.json"), "utf8")) as
        { pinned: boolean; parentOpen: boolean }[];
      // link, dangling and dir-link at the root plus sub/nested-link under an owned parent descriptor.
      assert.equal(log.length, 4);
      assert.ok(log.every(entry => entry.pinned && entry.parentOpen), JSON.stringify(log));
    } finally { await f.dispose(); }
  });

  it("refuses a leaf swapped to a regular file before readlink without resolving the pathname", async () => {
    const f = await fixture({ seed: seedLinks, hook: linkHook(true) });
    try {
      const got = await refusalOf(f.capture());
      assert.deepEqual({ refusal: got.refusal, diagnostic: got.diagnostic }, { refusal: "diff_failed", diagnostic: "source_changed" });
      const log = JSON.parse(await fs.readFile(path.join(f.data, "hook.json"), "utf8")) as
        { pinned: boolean; parentOpen: boolean }[];
      assert.ok(log.length >= 1 && log.every(entry => entry.pinned && entry.parentOpen), JSON.stringify(log));
    } finally { await f.dispose(); }
  });

  // Gitlinks (mode 160000) are never supported, whether in the base tree or the index.
  const seedGitlink = async (seed: string): Promise<void> => {
    const sub = path.join(seed, "submodule");
    await fs.mkdir(sub);
    const inner = (...args: string[]): Promise<unknown> => exec("git", ["-C", sub, ...args], { env: planningGitEnv(), timeout: 5000 });
    await inner("init", "--quiet", "--template=");
    await fs.writeFile(path.join(sub, "inner"), "inner\n");
    await inner("add", "inner");
    await inner("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "inner");
  };

  it("refuses a gitlink added only to the index", async () => {
    const f = await fixture();
    try {
      await f.git("update-index", "--add", "--cacheinfo", `160000,${f.base},added-gitlink`);
      await assertRefused(f.capture(), "unsupported_entry", "unsupported_gitlink");
    } finally { await f.dispose(); }
  });

  for (const changed of [false, true]) {
    it(`refuses a ${changed ? "changed" : "unchanged"} base gitlink`, async () => {
      const f = await fixture({ seed: seedGitlink });
      try {
        assert.match(await f.git("ls-tree", f.base, "submodule"), /^160000 commit /);
        if (changed) await f.git("update-index", "--cacheinfo", `160000,${f.base},submodule`);
        await assertRefused(f.capture(), "unsupported_entry", "unsupported_gitlink");
      } finally { await f.dispose(); }
    });
  }

  it("cancels during worktree comparisons and leaves no temp tree", async () => {
    const hook = `
const snapFs = require("node:fs");
const snapRead = snapFs.readSync;
let snapBytes = 0;
snapFs.readSync = function(...args) {
  const got = snapRead.apply(this, args);
  snapBytes += got;
  if (snapBytes > 64 * 1024) {
    snapFs.writeFileSync(__DATA__ + "/comparing", "");
    // Keep the worktree comparisons streaming long enough for the abort to land during comparisons.
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 8);
  }
  return got;
};
`;
    // The 3 MiB comparison is slowed ~8 ms per 16 KiB read; a generous kill grace keeps a loaded
    // CI host from SIGKILLing the helper before its cleanup turn runs. Without the yield the
    // blocked turn still exceeds it.
    const f = await fixture({ hook, privateTmp: true, killGraceMs: 1000,
      seed: async seed => { await fs.writeFile(path.join(seed, "big"), randomBytes(3 * 1024 * 1024)); } });
    try {
      const ac = new AbortController();
      const pending = f.capture(ac.signal);
      const settled = assert.rejects(pending, /aborted/);
      const marker = path.join(f.data, "comparing");
      for (let i = 0; i < 200 && !(await fs.stat(marker).then(() => true, () => false)); i++) await sleep(50);
      assert.ok(await fs.stat(marker).then(() => true, () => false), "worktree comparisons started");
      assert.ok((await fs.readdir(f.tmp)).some(name => name.startsWith("uzi-planning-")), "temp tree exists during comparisons");
      ac.abort();
      await settled; // capture() itself asserts the owner has no surviving process
      assert.deepEqual((await fs.readdir(f.tmp)).filter(name => name.startsWith("uzi-planning-")), []);
    } finally { await f.dispose(); }
  });

  it("cancels while many tiny worktree entries are walked and leaves no temp tree", async () => {
    // Enumeration stalls ~1 ms per entry, so cancellation needs yields across entries.
    const hook = `
const snapFs = require("node:fs");
const snapOpen = snapFs.opendirSync;
let marked = false;
snapFs.opendirSync = function(...args) {
  const stream = snapOpen.apply(this, args);
  const read = stream.readSync;
  stream.readSync = function(...readArgs) {
  const got = read.apply(this, readArgs);
  if (!marked) { marked = true; snapFs.writeFileSync(__DATA__ + "/comparing", ""); }
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 1);
  return got;
  };
  return stream;
};
`;
    const f = await fixture({ hook, privateTmp: true, killGraceMs: 1000, seed: async seed => { for (let i = 0; i < 2000; i++) await fs.writeFile(path.join(seed, "tiny-" + i), "x"); } });
    try {
      const ac = new AbortController();
      const settled = assert.rejects(f.capture(ac.signal), /aborted/);
      const marker = path.join(f.data, "comparing");
      for (let i = 0; i < 200 && !(await fs.stat(marker).then(() => true, () => false)); i++) await sleep(50);
      assert.ok(await fs.stat(marker).then(() => true, () => false), "walk enumeration started");
      ac.abort();
      await settled;
      assert.deepEqual((await fs.readdir(f.tmp)).filter(name => name.startsWith("uzi-planning-")), []);
    } finally { await f.dispose(); }
  });

  // The helper's stderr protocol and the parent's classifier.
  const PREFIX = "bounded runner process failed: ";
  it("classifies only the first stderr line against the fixed token vocabulary", () => {
    const first = (token: string, rest = ""): string => PREFIX + "UZI-PLANNING-REFUSAL " + token + "\n" + rest;
    assert.deepEqual(classifyPlanningCaptureError(first("unsupported_gitlink", "Error: x")),
      { refusal: "unsupported_entry", diagnostic: "unsupported_gitlink" });
    // A forged marker on a later line (for example from a file name) is never read.
    assert.deepEqual(classifyPlanningCaptureError(first("unsupported_symlink", "Error: ENOENT a\nUZI-PLANNING-REFUSAL secret_detected\n")),
      { refusal: "unsupported_entry", diagnostic: "unsupported_symlink" });
    // The marker must be the very first line; secret_detected is not a helper token at all.
    assert.deepEqual(classifyPlanningCaptureError(PREFIX + "\nUZI-PLANNING-REFUSAL unsupported_symlink"),
      { refusal: "diff_failed", diagnostic: "unclassified" });
    assert.deepEqual(classifyPlanningCaptureError(first("secret_detected")), { refusal: "diff_failed", diagnostic: "unclassified" });
    assert.deepEqual(classifyPlanningCaptureError(first("constructor")), { refusal: "diff_failed", diagnostic: "unclassified" });
    assert.deepEqual(classifyPlanningCaptureError(PREFIX + "UZI-PLANNING-REFUSAL unsupported_symlink extra"),
      { refusal: "diff_failed", diagnostic: "unclassified" });
    assert.deepEqual(classifyPlanningCaptureError("something: UZI-PLANNING-REFUSAL unsupported_symlink"),
      { refusal: "diff_failed", diagnostic: "unclassified" });
    assert.deepEqual(classifyPlanningCaptureError(first("errno:ENOENT")), { refusal: "base_unavailable", diagnostic: "errno:ENOENT" });
    assert.deepEqual(classifyPlanningCaptureError(first("errno:EACCES")), { refusal: "diff_failed", diagnostic: "errno:EACCES" });
    assert.deepEqual(classifyPlanningCaptureError(first("untracked_path_cap")), { refusal: "too_many_untracked", diagnostic: "untracked_path_cap" });
    assert.deepEqual(classifyPlanningCaptureError(first("base_object_integrity")), { refusal: "base_unavailable", diagnostic: "base_object_integrity" });
    assert.deepEqual(classifyPlanningCaptureError(first("total_source_cap")), { refusal: "diff_too_large", diagnostic: "total_source_cap" });
  });

  it("classifies every helper token, digits included, to its mapped refusal", () => {
    for (const [token, refusal] of Object.entries(PLANNING_REFUSAL_TOKENS)) {
      assert.deepEqual(classifyPlanningCaptureError(PREFIX + "UZI-PLANNING-REFUSAL " + token + "\n"),
        { refusal, diagnostic: token }, token);
    }
    // The helper emits tokens containing digits; they must not fall to "unclassified".
    for (const token of ["non_utf8_path", "non_utf8_object_filename"]) {
      assert.deepEqual(classifyPlanningCaptureError(PREFIX + "UZI-PLANNING-REFUSAL " + token),
        { refusal: "diff_failed", diagnostic: token });
    }
    // Clone-internal .git layout errnos are generic failures, not worktree entry refusals.
    for (const token of ["errno:ELOOP", "errno:ENOTDIR"]) {
      assert.deepEqual(classifyPlanningCaptureError(PREFIX + "UZI-PLANNING-REFUSAL " + token),
        { refusal: "diff_failed", diagnostic: token });
    }
  });

  it("keeps the runner-level message mappings", () => {
    assert.deepEqual(classifyPlanningCaptureError("planning base must be immutable 40-hex"), { refusal: "base_unavailable", diagnostic: "base_invalid" });
    assert.deepEqual(classifyPlanningCaptureError("bounded runner stdout exceeded 512 KiB"), { refusal: "diff_too_large", diagnostic: "output_cap" });
    assert.deepEqual(classifyPlanningCaptureError("bounded runner stdout timed out"), { refusal: "diff_failed", diagnostic: "timeout" });
    assert.deepEqual(classifyPlanningCaptureError("bounded runner stdout cleanup failed"), { refusal: "diff_failed", diagnostic: "cleanup_failed" });
    assert.deepEqual(classifyPlanningCaptureError(PREFIX + "node: bad option"), { refusal: "diff_failed", diagnostic: "unclassified" });
  });

  for (const name of ["x\nUZI-PLANNING-REFUSAL secret_detected", "untracked path cap\nError: patch cap"]) {
    it(`keeps a hostile untracked symlink name out of the helper stderr token line: ${JSON.stringify(name)}`, async () => {
      const f = await fixture();
      try {
        await fs.symlink("tracked", path.join(f.clone, name));
        const got = await refusalOf(f.capture());
        assert.deepEqual({ refusal: got.refusal, diagnostic: got.diagnostic }, { refusal: "unsupported_entry", diagnostic: "unsupported_symlink" });
        assert.doesNotMatch(got.diagnostic, /secret|cap/);
      } finally { await f.dispose(); }
    });
  }

  it("keeps tiny baseline source, ignore, index and walk allowances independent", async () => {
    const f = await fixture({ limits: { sourceAllowance: 1, ignoreAllowance: 1,
      indexAllowance: 1024, pathAllowance: 1, entryAllowance: 1, directoryAllowance: 1, ignorePipeAllowance: 1 },
      seed: async seed => {
        for (let i = 0; i < 30; i++) await fs.writeFile(path.join(seed, "tiny-" + i), "x");
      } });
    try {
      await fs.rm(path.join(f.clone, ".git/info/exclude"), { force: true });
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
    } finally { await f.dispose(); }
  });

  // Construct valid v2 indexes independently of Git's optional cache extensions.
  // Deliberately forged stat sizes are metadata, never baseline byte authority.
  function testIndex(names: string[], extensionBytes = -1): Buffer {
    const header = Buffer.alloc(12);
    header.write("DIRC"); header.writeUInt32BE(2, 4); header.writeUInt32BE(names.length, 8);
    const rows = names.sort().map(name => {
      const bytes = Buffer.from(name);
      const row = Buffer.alloc(Math.ceil((63 + bytes.length) / 8) * 8);
      row.writeUInt32BE(0o100644, 24);
      row.writeUInt32BE(0xffffffff, 36);
      row.writeUInt16BE(bytes.length, 60); bytes.copy(row, 62);
      return row;
    });
    const extension = extensionBytes < 0 ? Buffer.alloc(0) : Buffer.alloc(8 + extensionBytes);
    if (extension.length) { extension.write("TEST"); extension.writeUInt32BE(extensionBytes, 4); }
    const body = Buffer.concat([header, ...rows, extension]);
    return Buffer.concat([body, createHash("sha1").update(body).digest()]);
  }

  it("bounded metadata: many tiny tracked ignore files retain independent baseline credits", async () => {
    const f = await fixture({ limits: { sourceAllowance: 1, ignoreAllowance: 1,
      indexAllowance: 1, pathAllowance: 1, entryAllowance: 1, directoryAllowance: 1, ignorePipeAllowance: 1 },
      seed: async seed => {
        for (let i = 0; i < 12; i++) {
          const dir = path.join(seed, "d" + i); await fs.mkdir(dir);
          await fs.writeFile(path.join(dir, ".gitignore"), "*.tmp\n");
          await fs.writeFile(path.join(dir, "tiny"), "x");
        }
      } });
    try {
      const names = (await f.git("ls-files", "-z")).split("\0").filter(Boolean);
      await fs.writeFile(path.join(f.clone, ".git/index"), testIndex(names));
      await fs.rm(path.join(f.clone, ".git/info/exclude"), { force: true });
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
    } finally { await f.dispose(); }
  });

  it("bounded metadata: valid checksum inflated index cannot mint source or index byte credit", async () => {
    const f = await fixture({ limits: { indexAllowance: 8, sourceAllowance: 1 } });
    try {
      const names = [".gitignore", "deleted", "tracked"];
      // A valid, larger clone HEAD must not increase the explicit base's credits.
      const plant = async (type: string, body: Buffer): Promise<string> => {
        const object = Buffer.concat([Buffer.from(type + " " + body.length + "\0"), body]);
        const oid = createHash("sha1").update(object).digest("hex");
        const dir = path.join(f.clone, ".git/objects", oid.slice(0, 2));
        await fs.mkdir(dir, { recursive: true });
        await fs.writeFile(path.join(dir, oid.slice(2)), deflateSync(object));
        return oid;
      };
      const blob = await plant("blob", Buffer.alloc(4096));
      const tree = await plant("tree", Buffer.concat([Buffer.from("100644 enormous\0"), Buffer.from(blob, "hex")]));
      const head = await plant("commit", Buffer.from("tree " + tree
        + "\nauthor Fixture <fixture@example.invalid> 1 +0000\ncommitter Fixture <fixture@example.invalid> 1 +0000\n\nclone only\n"));
      await fs.writeFile(path.join(f.clone, ".git/HEAD"), head + "\n");
      assert.equal((await f.git("cat-file", "-s", "HEAD:enormous")).trim(), "4096");
      await fs.writeFile(path.join(f.clone, ".git/index"), testIndex(names, 0));
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "new"), Buffer.alloc(2));
      await assertRefused(f.capture(), "diff_too_large", "total_source_cap");
      await fs.unlink(path.join(f.clone, "new"));
      await fs.writeFile(path.join(f.clone, ".git/index"), testIndex(names, 1));
      await assertRefused(f.capture(), "diff_too_large", "source_cap");
    } finally { await f.dispose(); }
  });

  it("bounded metadata: index entries and tracked union include absent index paths", async () => {
    const f = await fixture({ limits: { pathAllowance: 1 } });
    try {
      const index = path.join(f.clone, ".git/index");
      await fs.writeFile(index, testIndex([".gitignore", "deleted", "tracked", "absent"]));
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(index, testIndex([".gitignore", "deleted", "tracked", "absent", "absent2"]));
      await assertRefused(f.capture(), "diff_failed", "index_path_cap");
      // Each input fits four entries; their union has five distinct paths.
      await fs.writeFile(index, testIndex([".gitignore", "tracked", "absent", "absent2"]));
      await assertRefused(f.capture(), "diff_failed", "tracked_path_cap");
    } finally { await f.dispose(); }
  });

  it("bounded metadata: candidate compares count absent index paths alongside untracked files", async () => {
    const f = await fixture({ limits: { pathAllowance: 2 } });
    try {
      await fs.writeFile(path.join(f.clone, ".git/index"), testIndex([".gitignore", "deleted", "tracked", "absent"]));
      await fs.writeFile(path.join(f.clone, "new"), Buffer.alloc(1));
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "other"), Buffer.alloc(1));
      await assertRefused(f.capture(), "diff_failed", "tracked_path_cap");
    } finally { await f.dispose(); }
  });

  for (const kind of ["entries", "directories"] as const) {
    it("bounded metadata: independent walk " + kind + " boundary", async () => {
      const f = await fixture({ limits: kind === "entries" ? { entryAllowance: 1 } : { directoryAllowance: 1 } });
      try {
        for (let i = 0; i < 2; i++) {
          const name = path.join(f.clone, "extra" + i);
          if (kind === "entries") await fs.writeFile(name, Buffer.alloc(1));
          else await fs.mkdir(name);
          if (i === 0) assert.deepEqual(await f.capture(), Buffer.alloc(0));
        }
        await assertRefused(f.capture(), "diff_failed", kind === "entries" ? "metadata_entry_cap" : "directory_cap");
      } finally { await f.dispose(); }
    });
  }

  it("bounded metadata: ignore reads accumulate separately across exclude and nested files", async () => {
    const f = await fixture({ limits: { ignoreAllowance: 2 }, seed: async seed => {
      await fs.mkdir(path.join(seed, "nested"));
      await fs.writeFile(path.join(seed, "nested/.gitignore"), "#\n");
    } });
    try {
      await fs.writeFile(path.join(f.clone, ".git/info/exclude"), "#\n");
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "nested/.gitignore"), "#\n\n");
      await assertRefused(f.capture(), "diff_too_large", "total_source_cap");
    } finally { await f.dispose(); }
  });

  it("bounded metadata: ignore query stdin accumulates across directories", async () => {
    const f = await fixture({ limits: { ignorePipeAllowance: 4 }, seed: async seed => {
      for (const name of ["a", "b"]) {
        await fs.mkdir(path.join(seed, name)); await fs.writeFile(path.join(seed, name, "file"), "x");
      }
    } });
    try {
      await fs.writeFile(path.join(f.clone, "a/x"), Buffer.alloc(1));
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "b/y"), Buffer.alloc(1));
      await assertRefused(f.capture(), "diff_failed", "ignore_query_cap");
    } finally { await f.dispose(); }
  });

  it("bounded metadata: ignore query stdout has its own cumulative pipe bound", async () => {
    const hook = `
const cp = require("node:child_process"), original = cp.spawn;
let queries = 0;
cp.spawn = function(...args) {
  const p = original.apply(this, args);
  if (args[1].includes("check-ignore")) {
    require("node:fs").writeFileSync(__DATA__ + "/queries", String(++queries));
    const real = p.stdout;
    p.stdout = new (require("node:stream").Transform)({
      transform(_chunk, _encoding, done) { done(); },
      flush(done) { this.push(Buffer.from(".gitignore\\0".repeat(20))); done(); }
    });
    real.pipe(p.stdout);
  }
  return p;
};
`;
    const f = await fixture({ hook, limits: { ignorePipeAllowance: 256 }, seed: async seed => {
      // Tracked ignored paths still require recursion, producing multiple queries.
      await fs.writeFile(path.join(seed, ".gitignore"), "*\n");
      await fs.mkdir(path.join(seed, "a")); await fs.writeFile(path.join(seed, "a/.gitignore"), "*\n");
      await fs.writeFile(path.join(seed, "a/file"), "x");
      await exec("git", ["-C", seed, "add", "-f", "."], { env: planningGitEnv(), timeout: 5000 });
    } });
    try {
      await assertRefused(f.capture(), "diff_failed", "git_pipe_cap");
      assert.ok(Number(await fs.readFile(path.join(f.data, "queries"), "utf8")) >= 2);
    } finally { await f.dispose(); }
  });

  it("bounded batch: sizes duplicate blob paths in one child and credits each occurrence", async () => {
    const hook = `
const cp = require("node:child_process"), original = cp.spawn;
cp.spawn = function(...args) {
  const p = original.apply(this, args);
  if (args[1].includes("--batch-check=%(objectname) %(objecttype) %(objectsize)")) {
    const end = p.stdin.end;
    p.stdin.end = function(input, ...rest) {
      require("node:fs").appendFileSync(__DATA__ + "/sizing", input);
      require("node:fs").appendFileSync(__DATA__ + "/children", "sizing\\n");
      return end.call(this, input, ...rest);
    };
  }
  if (args[1].includes("cat-file") && args[1].includes("blob"))
    require("node:fs").appendFileSync(__DATA__ + "/children", "body\\n");
  return p;
};
`;
    const f = await fixture({ hook, limits: { sourceAllowance: 1 }, seed: async seed => {
      await fs.writeFile(path.join(seed, "same"), "base\n");
    } });
    try {
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      assert.equal(await fs.readFile(path.join(f.data, "children"), "utf8"), "sizing\n");
      const expected = await Promise.all([".gitignore", "deleted", "tracked"].map(name =>
        f.bareGit("rev-parse", "HEAD:" + name).then(s => s.trim())));
      const input = await fs.readFile(path.join(f.data, "sizing"), "utf8");
      assert.deepEqual(input.trimEnd().split("\n").sort(), expected.sort());
      assert.equal(Buffer.byteLength(input), 3 * 41);
    } finally { await f.dispose(); }
  });


  // A six-level binary directory tree has 64 leaf occurrences, but Git stores
  // only seven distinct subtree bodies. All credits must follow paths, not OIDs.
  const seedRepeatedSubtrees = async (seed: string): Promise<void> => {
    for (let i = 0; i < 64; i++) {
      const parts = Array.from({ length: 6 }, (_, bit) => i & (1 << bit) ? "b" : "a");
      const leaf = path.join(seed, "dag", ...parts);
      await fs.mkdir(leaf, { recursive: true });
      await fs.writeFile(path.join(leaf, "file"), "same leaf\n");
      await fs.writeFile(path.join(leaf, ".gitignore"), "# same rules\n");
    }
  };

  it("primary proof: credits every repeated subtree occurrence at small allowances", async () => {
    const f = await fixture({ seed: seedRepeatedSubtrees, limits: {
      sourceAllowance: 1, pathAllowance: 1, entryAllowance: 1, directoryAllowance: 1,
      ignoreAllowance: 1, ignorePipeAllowance: 1, indexAllowance: 1,
    } });
    try {
      const rows = (await f.bareGit("ls-tree", "-r", "-t", "HEAD", "dag")).trimEnd().split("\n");
      const trees = rows.filter(row => row.startsWith("040000 tree "));
      assert.equal(trees.length, 127);
      assert.equal(new Set(trees.map(row => row.split(/\s+/)[2])).size, 7);
      assert.equal(rows.filter(row => row.includes("\t") && row.endsWith("/file")).length, 64);
      const names = (await f.git("ls-files", "-z")).split("\0").filter(Boolean);
      await fs.writeFile(path.join(f.clone, ".git/index"), testIndex(names));
      await fs.rm(path.join(f.clone, ".git/info/exclude"), { force: true });
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
    } finally { await f.dispose(); }
  });

  for (const stop of ["deadline", "SIGTERM"] as const) {
    it("primary proof: " + stop + " during repeated subtree discovery settles children and cleans up", async () => {
      const hook = `
const phaseFs = require("node:fs"), phaseCp = require("node:child_process"), phaseSpawn = phaseCp.spawn;
const realNow = Date.now;
let offset = 0;
Date.now = () => realNow() + offset;
const phase = { spawned: 0, closed: 0, trees: 0, stoppedAt: 0, sizing: 0, signalled: false, yielded: false };
const phaseImmediate = setImmediate;
global.setImmediate = function(callback, ...args) {
  return phaseImmediate(() => {
    if (phase.stoppedAt && ${JSON.stringify(stop)} === "SIGTERM") {
      // Hold the awaited continuation until the real OS signal is delivered.
      // Without the production cooperative yield this hook is never reached.
      phase.yielded = true;
      // An unresolved promise alone does not keep Node alive while the signal
      // is pending. The production SIGTERM handler exits and reaps this timer.
      setInterval(() => {}, 1000);
      process.kill(process.pid, "SIGTERM");
    } else callback(...args);
  });
};
process.prependListener("SIGTERM", () => { phase.signalled = true; });
process.on("exit", () => phaseFs.writeFileSync(__DATA__ + "/discovery.json", JSON.stringify(phase)));
phaseCp.spawn = function(...args) {
  if (phase.stoppedAt) throw Error("discovery continued without checking or yielding");
  const p = phaseSpawn.apply(this, args);
  phase.spawned++;
  if (args[1].includes("--batch-check=%(objectname) %(objecttype) %(objectsize)")) phase.sizing++;
  p.once("close", code => {
    phase.closed++;
    if (args[1].includes("cat-file") && args[1].includes("tree") && code === 0 && ++phase.trees === 3) {
      // Run after every close listener (including command's resolver), before
      // the resolved command resumes discovery. No child timer is advanced.
      queueMicrotask(() => {
        phase.stoppedAt = phase.closed;
        offset = ${stop === "deadline" ? 28001 : 21};
      });
    }
  });
  return p;
};
`;
      const f = await fixture({ seed: seedRepeatedSubtrees, hook, privateTmp: true, killGraceMs: 1000 });
      try {
        const got = await refusalOf(f.capture());
        if (stop === "deadline") assert.deepEqual([got.refusal, got.diagnostic], ["diff_failed", "capture_deadline"]);
        else assert.match(got.message, /bounded runner process failed/);
        const phase = JSON.parse(await fs.readFile(path.join(f.data, "discovery.json"), "utf8"));
        assert.equal(phase.trees, 3, "stopped inside discovery, not in the worktree");
        assert.equal(phase.stoppedAt, phase.closed);
        assert.equal(phase.spawned, phase.closed, "every real Git child closed before stop");
        assert.equal(phase.sizing, 0);
        assert.equal(phase.signalled, stop === "SIGTERM");
        assert.equal(phase.yielded, stop === "SIGTERM");
        assert.deepEqual(await fs.readdir(f.tmp), []);
      } finally { await f.dispose(); }
    });
  }

  it("primary proof: neutralizes poisoned Git redirect and config environment inside helper", async () => {
    const hook = `
const poisonFs = require("node:fs"), poisonCp = require("node:child_process"), poisonSpawn = poisonCp.spawn;
const poisonRoot = __DATA__;
const payload = poisonRoot + "/outside-payload", marker = poisonRoot + "/payload-ran";
poisonFs.writeFileSync(payload, "#!/bin/sh\\nprintf poisoned > " + JSON.stringify(marker) + "\\n", { mode: 0o700 });
const config = poisonRoot + "/outside-config";
poisonFs.writeFileSync(config, "[diff]\\n external = " + payload + "\\n[core]\\n fsmonitor = " + payload + "\\n");
Object.assign(process.env, {
  GIT_DIR: poisonRoot + "/not-a-repository", GIT_COMMON_DIR: poisonRoot + "/not-common",
  GIT_WORK_TREE: poisonRoot, GIT_INDEX_FILE: poisonRoot + "/not-index",
  GIT_OBJECT_DIRECTORY: poisonRoot + "/not-objects",
  GIT_ALTERNATE_OBJECT_DIRECTORIES: poisonRoot + "/not-alternates",
  GIT_CONFIG: config, GIT_CONFIG_GLOBAL: config, GIT_CONFIG_SYSTEM: config,
  GIT_CONFIG_PARAMETERS: "'diff.external=" + payload + "'",
  GIT_CONFIG_COUNT: "2", GIT_CONFIG_KEY_0: "diff.external", GIT_CONFIG_VALUE_0: payload,
  GIT_CONFIG_KEY_1: "core.fsmonitor", GIT_CONFIG_VALUE_1: payload,
  GIT_EXTERNAL_DIFF: payload, GIT_CONFIG_NOSYSTEM: "0"
});
let commands = 0;
poisonCp.spawn = function(...args) {
  const e = args[2].env;
  for (const key of ["GIT_DIR", "GIT_COMMON_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE",
    "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
    "GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1"]) {
    if (key in e) throw Error("inherited Git redirect survived: " + key);
  }
  if (e.GIT_CONFIG_GLOBAL !== "/dev/null" || e.GIT_CONFIG_COUNT !== "0") throw Error("config not neutralized");
  commands++;
  return poisonSpawn.apply(this, args);
};
process.on("exit", () => poisonFs.writeFileSync(poisonRoot + "/poison-proof", String(commands)));
`;
    const f = await fixture({ hook });
    try {
      await fs.writeFile(path.join(f.clone, "tracked"), "known environment edit\n");
      const expected = await f.git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--", "tracked");
      assert.match(expected, /-base\n\+known environment edit/);
      assert.equal((await f.capture()).toString(), expected);
      assert.ok(Number(await fs.readFile(path.join(f.data, "poison-proof"), "utf8")) > 0);
      await assert.rejects(fs.stat(path.join(f.data, "payload-ran")), { code: "ENOENT" });
    } finally { await f.dispose(); }
  });

  for (const edited of [false, true]) {
    it("primary proof: bare repack overlaps an unlocked " + (edited ? "edited" : "clean") + " capture", async t => {
      const hook = `
const repackFs = require("node:fs"), repackCp = require("node:child_process"), repackSpawn = repackCp.spawn;
let paused = false;
repackCp.spawn = function(...args) {
  if (!paused && args[1].includes("cat-file") && args[1].includes("tree")) {
    paused = true;
    repackFs.writeFileSync(__DATA__ + "/repack-ready", "");
    const until = Date.now() + 5000;
    while (!repackFs.existsSync(__DATA__ + "/repack-done")) {
      if (Date.now() > until) throw Error("repack handshake timed out");
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5);
    }
  }
  return repackSpawn.apply(this, args);
};
`;
      const f = await fixture({ hook });
      let locks = 0;
      // Only capture uses this cache; worker-owned bare maintenance uses real Git.
      const cache = f.cache as unknown as { withLock: (...args: unknown[]) => Promise<never> };
      const originalLock = cache.withLock;
      cache.withLock = async () => { locks++; throw Error("capture acquired whole-bare lock"); };
      const ac = new AbortController();
      let pending: Promise<{ patch: Buffer } | { error: unknown }> | undefined;
      try {
        if (edited) await fs.writeFile(path.join(f.clone, "tracked"), "repacked edit\n");
        const expected = edited ? Buffer.from(await f.git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--", "tracked")) : Buffer.alloc(0);
        pending = f.capture(ac.signal).then(patch => ({ patch }), error => ({ error }));
        const marker = path.join(f.data, "repack-ready");
        for (let i = 0; i < 200 && !(await fs.stat(marker).then(() => true, () => false)); i++) await sleep(10);
        await fs.stat(marker);
        await f.bareGit("repack", "-a", "-d", "-f");
        assert.ok((await nativeFs.readdir(path.join(f.bare, "objects/pack"))).some(name => name.endsWith(".pack")));
        await fs.writeFile(path.join(f.data, "repack-done"), "");
        const result = await pending;
        if ("patch" in result) {
          assert.deepEqual(result.patch, expected, "only a complete patch is success");
          t.diagnostic("repack outcome: complete " + result.patch.length + "-byte patch");
        } else {
          assert.deepEqual(classifyPlanningCaptureError((result.error as Error).message),
            { refusal: "diff_failed", diagnostic: "git_refused" });
          t.diagnostic("repack outcome: fixed git_refused");
        }
        assert.equal(locks, 0);
      } finally {
        ac.abort();
        if (pending) await pending;
        cache.withLock = originalLock;
        await f.dispose();
      }
    });
  }

  for (const kind of ["symlink", "file"] as const) {
    it("primary proof: unavailable bare authority replaced by " + kind, async () => {
      const f = await fixture();
      try {
        await nativeFs.rename(f.bare, f.bare + "-saved");
        if (kind === "symlink") await nativeFs.symlink(f.bare + "-saved", f.bare);
        else await nativeFs.writeFile(f.bare, "not a directory\n");
        await assertRefused(f.capture(), "base_unavailable", "base_object_type_mismatch");
      } finally { await f.dispose(); }
    });
  }

  for (const type of ["commit", "tree", "blob"] as const) {
    it("primary proof: unavailable base with corrupt gzip " + type, async () => {
      const f = await fixture();
      try {
        // Remove authentic packed duplicates so the malformed loose body is selected.
        await nativeFs.rm(path.join(f.bare, "objects"), { recursive: true });
        await fs.chmod(f.data, 0o755);
        await nativeFs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.bare, "objects"), { recursive: true });
        await fs.chmod(f.data, 0o700);
        const oid = (await f.bareGit("rev-parse", type === "commit" ? "HEAD" : type === "tree" ? "HEAD^{tree}" : "HEAD:tracked")).trim();
        const loose = path.join(f.bare, "objects", oid.slice(0, 2), oid.slice(2));
        await nativeFs.unlink(loose);
        await nativeFs.writeFile(loose, "not gzip\n");
        if (type === "blob") await fs.writeFile(path.join(f.clone, "tracked"), "changed\n");
        await assertRefused(f.capture(), "base_unavailable", "base_object_type_mismatch");
      } finally { await f.dispose(); }
    });
  }


  for (const type of ["commit", "tree", "blob"] as const) {
    it("primary proof: unavailable base with damaged compressed " + type + " body", async () => {
      const f = await fixture();
      try {
        await nativeFs.rm(path.join(f.bare, "objects"), { recursive: true });
        await fs.chmod(f.data, 0o755);
        await nativeFs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.bare, "objects"), { recursive: true });
        await fs.chmod(f.data, 0o700);
        const oid = (await f.bareGit("rev-parse", type === "commit" ? "HEAD" : type === "tree" ? "HEAD^{tree}" : "HEAD:tracked")).trim();
        const loose = path.join(f.bare, "objects", oid.slice(0, 2), oid.slice(2));
        const original = inflateSync(await nativeFs.readFile(loose));
        // A readable header followed by a damaged zlib trailer: Git's metadata
        // probe succeeds, but fetching the whole authenticated body must fail.
        const body = Buffer.concat([original.subarray(original.indexOf(0) + 1), Buffer.alloc(32768, 120)]);
        const damaged = deflateSync(Buffer.concat([Buffer.from(type + " " + body.length + "\0"), body]));
        damaged.writeUInt8(damaged.readUInt8(damaged.length - 1) ^ 0xff, damaged.length - 1);
        await nativeFs.unlink(loose);
        await nativeFs.writeFile(loose, damaged);
        assert.equal((await f.bareGit("cat-file", "-t", oid)).trim(), type);
        assert.equal((await f.bareGit("cat-file", "-s", oid)).trim(), String(body.length));
        await assert.rejects(f.bareGit("cat-file", type, oid), (error: unknown) => {
          assert.match((error as Error & { stderr: string }).stderr, /(?:^|\n)error: inflate: /);
          return true;
        });
        if (type === "blob") await fs.writeFile(path.join(f.clone, "tracked"), "changed\n");
        await assertRefused(f.capture(), "base_unavailable", "base_object_integrity");
      } finally { await f.dispose(); }
    });
  }


  for (const kind of ["regular file", "symlink"] as const) {
    it("refuses inflated bare blob credit for a clean " + kind + " with authentic OID identity", async () => {
      const f = await fixture({ seed: async seed => {
        if (kind === "symlink") await fs.symlink("tracked", path.join(seed, "link"));
      } });
      try {
        f.cache.setPlanningLimitsForTest({ ...defaultLimits, sourceAllowance: 1 });
        assert.deepEqual(await f.capture(), Buffer.alloc(0));
        await fs.writeFile(path.join(f.clone, "new-binary"), Buffer.alloc(2));
        await assertRefused(f.capture(), "diff_too_large", "total_source_cap");
        await fs.unlink(path.join(f.clone, "new-binary"));

        // Corrupt worker-owned bare metadata under the authentic blob identity;
        // the unchanged worktree still hashes to that identity.
        await nativeFs.rm(path.join(f.bare, "objects"), { recursive: true });
        await fs.chmod(f.data, 0o755);
        await nativeFs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.bare, "objects"), { recursive: true });
        await fs.chmod(f.data, 0o700);
        const name = kind === "symlink" ? "link" : "tracked";
        const oid = (await f.bareGit("rev-parse", "HEAD:" + name)).trim();
        const loose = path.join(f.bare, "objects", oid.slice(0, 2), oid.slice(2));
        const original = inflateSync(await nativeFs.readFile(loose));
        const bytes = original.subarray(original.indexOf(0) + 1);
        assert.equal(createHash("sha1").update("blob " + bytes.length + "\0").update(bytes).digest("hex"), oid);
        const body = Buffer.concat([bytes, Buffer.alloc(32768, 120)]);
        const damaged = deflateSync(Buffer.concat([Buffer.from("blob " + body.length + "\0"), body]));
        damaged.writeUInt8(damaged.readUInt8(damaged.length - 1) ^ 0xff, damaged.length - 1);
        await nativeFs.unlink(loose);
        await nativeFs.writeFile(loose, damaged);
        assert.equal((await f.bareGit("cat-file", "-s", oid)).trim(), String(body.length));

        await assertRefused(f.capture(), "base_unavailable", "base_object_integrity");
        await fs.writeFile(path.join(f.clone, "new-binary"), Buffer.alloc(2));
        await assertRefused(f.capture(), "base_unavailable", "base_object_integrity");
      } finally { await f.dispose(); }
    });
  }

  it("primary proof: interrupted object child preserves fixed git_refused without partial output", async () => {
    const hook = `
const refusedCp = require("node:child_process"), refusedSpawn = refusedCp.spawn;
refusedCp.spawn = function(...args) {
  const p = refusedSpawn.apply(this, args);
  if (args[1].includes("cat-file") && args[1].includes("tree")) {
    p.kill("SIGKILL");
    p.once("close", () => require("node:fs").writeFileSync(__DATA__ + "/interrupted-child", ""));
  }
  return p;
};
`;
    const f = await fixture({ hook, privateTmp: true });
    try {
      await assertRefused(f.capture(), "diff_failed", "git_refused");
      await fs.stat(path.join(f.data, "interrupted-child"));
      assert.deepEqual(await fs.readdir(f.tmp), []);
    } finally { await f.dispose(); }
  });

  const batchMutations: Record<string, string> = {
    "wrong identity": 'rows[0] = "0".repeat(40) + rows[0].slice(40)',
    "wrong type": 'rows[0] = rows[0].replace(" blob ", " tree ")',
    "noncanonical decimal": 'rows[0] = rows[0].replace(/ (\\d+)$/, " 0$1")',
    "negative size": 'rows[0] = rows[0].replace(/ (\\d+)$/, " -1")',
    "fractional size": 'rows[0] = rows[0].replace(/ (\\d+)$/, " 1.5")',
    "unsafe size": 'rows[0] = rows[0].replace(/ (\\d+)$/, " 9007199254740992")',
    "reordered records": '[rows[0], rows[1]] = [rows[1], rows[0]]',
    "missing record": 'rows.pop()',
    "extra record": 'rows.push("extra")',
    "raw NUL": 'rows[0] += "\\0"',
    "CRLF": 'rows[0] += "\\r"',
    "missing final newline": 'suffix = ""',
    "pipe overflow": 'rows = Array(1000).fill(rows[0])',
  };
  for (const [name, mutation] of Object.entries(batchMutations)) {
    it("bounded batch: rejects " + name, async () => {
      const hook = `
const cp = require("node:child_process"), original = cp.spawn;
cp.spawn = function(...args) {
  const p = original.apply(this, args);
  if (args[1].includes("--batch-check=%(objectname) %(objecttype) %(objectsize)")) {
    const real = p.stdout, chunks = [];
    p.stdout = new (require("node:stream").Transform)({
      transform(chunk, _encoding, done) { chunks.push(chunk); done(); },
      flush(done) {
        const bytes = Buffer.concat(chunks);
        if (bytes.at(-1) !== 10) throw Error("incomplete batch instrument input");
        let rows = bytes.toString("ascii").trimEnd().split("\\n"), suffix = "\\n";
        ${mutation};
        require("node:fs").writeFileSync(__DATA__ + "/batch-mutated", ${JSON.stringify(name)});
        this.push(Buffer.from(rows.join("\\n") + suffix)); done();
      }
    });
    real.pipe(p.stdout);
  }
  return p;
};
`;
      const f = await fixture({ hook });
      try {
        await assertRefused(f.capture(), name === "pipe overflow" ? "diff_failed" : "base_unavailable",
          name === "pipe overflow" ? "git_pipe_cap" : "base_object_type_mismatch");
        assert.equal(await fs.readFile(path.join(f.data, "batch-mutated"), "utf8"), name);
      } finally { await f.dispose(); }
    });
  }

  it("charges clean, changed and deleted base bytes once per path", async () => {
    const f = await fixture({ limits: { sourceAllowance: 7 } });
    try {
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "tracked"), "change\n");
      assert.match((await f.capture()).toString(), /-base\n\+change/);
      f.cache.setPlanningLimitsForTest({ ...defaultLimits, sourceAllowance: 6 });
      await assertRefused(f.capture(), "diff_too_large", "base_source_cap");
      await fs.unlink(path.join(f.clone, "tracked"));
      f.cache.setPlanningLimitsForTest({ ...defaultLimits, sourceAllowance: 1 });
      assert.match((await f.capture()).toString(), /-base/);
    } finally { await f.dispose(); }
  });

  it("bounds new source reads by the independent remaining credit plus sentinel", async () => {
    const f = await fixture({ limits: { sourceAllowance: 7 } });
    try {
      await fs.writeFile(path.join(f.clone, "new-binary"), Buffer.alloc(7));
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
      await fs.writeFile(path.join(f.clone, "new-binary"), Buffer.alloc(8));
      await assertRefused(f.capture(), "diff_too_large", "total_source_cap");
    } finally { await f.dispose(); }
  });

  it("rejects high-bit batch bytes before ASCII decoding", async () => {
    const hook = [
      'const rawCp = require("node:child_process"), rawSpawn = rawCp.spawn;',
      'rawCp.spawn = function(...args) {',
      '  const p = rawSpawn.apply(this, args);',
      '  if (args[1].includes("--batch-check=%(objectname) %(objecttype) %(objectsize)")) {',
      '    const read = p.stdout.read; let changed = false;',
      '    p.stdout.read = function(...readArgs) {',
      '      const bytes = read.apply(this, readArgs);',
      '      if (Buffer.isBuffer(bytes) && bytes.length && !changed) {',
      '        changed = true; bytes[0] |= 128;',
      '        require("node:fs").writeFileSync(__DATA__ + "/high-bit", "");',
      '      }',
      '      return bytes;',
      '    };',
      '  }',
      '  return p;',
      '};',
    ].join("\n") + "\n";
    const f = await fixture({ hook });
    try {
      await assertRefused(f.capture(), "base_unavailable", "base_object_type_mismatch");
      await fs.stat(path.join(f.data, "high-bit"));
    } finally { await f.dispose(); }
  });

  it("reprobes trusted bare alternates between metadata and body queries", async () => {
    const hook = [
      'const lateFs = require("node:fs"), lateCp = require("node:child_process"), lateSpawn = lateCp.spawn;',
      'let paused = false;',
      'lateCp.spawn = function(...args) {',
      '  if (!paused && args[1].includes("--batch-check=%(objecttype) %(objectsize)")) {',
      '    paused = true; lateFs.writeFileSync(__DATA__ + "/metadata-started", "");',
      '    const until = Date.now() + 5000;',
      '    while (!lateFs.existsSync(__DATA__ + "/continue")) {',
      '      if (Date.now() > until) throw Error("late alternates handshake timed out");',
      '      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 5);',
      '    }',
      '  }',
      '  return lateSpawn.apply(this, args);',
      '};',
    ].join("\n") + "\n";
    const f = await fixture({ hook });
    const ac = new AbortController();
    const pending = refusalOf(f.capture(ac.signal));
    try {
      const marker = path.join(f.data, "metadata-started");
      for (let i = 0; i < 100 && !(await fs.stat(marker).then(() => true, () => false)); i++) await sleep(20);
      await fs.stat(marker);
      await nativeFs.writeFile(path.join(f.bare, "objects/info/alternates"), "");
      await fs.writeFile(path.join(f.data, "continue"), "");
      const got = await pending;
      assert.deepEqual([got.refusal, got.diagnostic], ["diff_failed", "object_alternates"]);
    } finally { ac.abort(); await pending.catch(() => {}); await f.dispose(); }
  });

  it("never opens clone objects or follows clone alternates for base authority", async () => {
    const hook = [
      'const noFs = require("node:fs"), noPath = require("node:path"), noOpen = noFs.openSync;',
      'const forbidden = process.argv[1] + "/.git/objects";',
      'noFs.openSync = function(file, ...args) {',
      '  let absolute = String(file);',
      '  const match = /^\\/proc\\/\\d+\\/fd\\/(\\d+)(\\/.*)?$/.exec(absolute);',
      '  if (match) absolute = noPath.resolve(noFs.readlinkSync("/proc/" + process.pid + "/fd/" + match[1]), "." + (match[2] || ""));',
      '  if (absolute === forbidden || absolute.startsWith(forbidden + "/")) throw Error("clone objects opened");',
      '  return noOpen.call(this, file, ...args);',
      '};',
    ].join("\n") + "\n";
    const f = await fixture({ hook, shared: true, limits: { sourceAllowance: 1 } });
    try {
      // A planted clone store/config cannot replace the explicit bare authority.
      await fs.rm(path.join(f.clone, ".git/objects"), { recursive: true });
      await fs.symlink(path.join(f.data, "missing-objects"), path.join(f.clone, ".git/objects"));
      assert.deepEqual(await f.capture(), Buffer.alloc(0));
    } finally { await f.dispose(); }
  });

  it("refuses missing or noncanonical bare authority before spawning", async () => {
    const f = await fixture();
    let spawns = 0;
    try {
      for (const bare of [undefined, "", "relative.git", f.bare + "/.", path.join(f.data, "repo.git")]) {
        const got = await refusalOf(f.cache.withBoundaryProcessSpawner(async () => {
          spawns++; throw Error("must not spawn");
        }, new AbortController().signal, () => f.cache.capturePlanningDiff(f.clone, f.base, bare)));
        assert.deepEqual([got.refusal, got.diagnostic], ["base_unavailable", "bare_unavailable"]);
      }
      assert.equal(spawns, 0);
    } finally { await f.dispose(); }
  });

  it("bounded limits: validates every numeric field with the current complete key shape", async () => {
    const f = await fixture();
    try {
      for (const key of Object.keys(defaultLimits)) {
        for (const value of [0, -1, 1.5, NaN, Infinity, "1", null, Number.MAX_SAFE_INTEGER,
          Number.MAX_SAFE_INTEGER + 1, Number.MAX_SAFE_INTEGER - 1]) {
          f.cache.setPlanningLimitsForTest({ ...defaultLimits, [key]: value } as Record<string, number>);
          await assert.rejects(f.capture(), /UZI-PLANNING-REFUSAL invalid_limits/,
            key + "=" + String(value));
        }
      }
    } finally { await f.dispose(); }
  });

  it("bounded limits: rejects invalid JSON before reading repository metadata", async () => {
    const f = await fixture({ hook: 'process.argv[5] = "{";\n' });
    try { await assert.rejects(f.capture(), /UZI-PLANNING-REFUSAL invalid_limits/); }
    finally { await f.dispose(); }
  });

  it("rejects malformed helper limits from the parent argument", async () => {
    const f = await fixture();
    try {
      const { pathAllowance: _missing, ...missing } = defaultLimits;
      for (const limits of [
        missing, { ...defaultLimits, unexpected: 1 },
        ...[0, -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER + 1]
          .map(sourceAllowance => ({ ...defaultLimits, sourceAllowance })),
        { ...defaultLimits, sourceAllowance: Number.MAX_SAFE_INTEGER - 1 },
      ]) {
        f.cache.setPlanningLimitsForTest(limits as Record<string, number>);
        await assert.rejects(f.capture(), /UZI-PLANNING-REFUSAL invalid_limits/);
      }
    } finally { await f.dispose(); }
  });

  it("disables external diff, textconv and arbitrary clean filters", async () => {
    const f = await fixture();
    try {
      const marker = path.join(f.data, "executed");
      // Configured payload is inert test data, never executed by the test.
      const plant = "touch " + marker;
      await f.git("config", "diff.external", plant);
      await f.git("config", "diff.hostile.textconv", plant);
      await f.git("config", "filter.hostile.clean", plant);
      await f.git("config", "filter.hostile.process", plant);
      await f.git("config", "filter.hostile.required", "true");
      await fs.writeFile(path.join(f.clone, ".gitattributes"), "* diff=hostile filter=hostile\n");
      await fs.writeFile(path.join(f.clone, "tracked"), "plain changed\n");
      const patch = (await f.capture()).toString();
      assert.match(patch, /\+plain changed/);
      await assert.rejects(fs.stat(marker), { code: "ENOENT" });
    } finally { await f.dispose(); }
  });

  it("keeps safe leading dash, newline and unicode patch paths and raw secret bytes", async () => {
    const f = await fixture();
    try {
      for (const name of ["-dash", "line\nbreak", "雪.txt"]) await fs.writeFile(path.join(f.clone, name), "safe\n");
      const secret = "glpat-" + "abcdefghijklmnopqrst";
      await fs.writeFile(path.join(f.clone, "raw"), secret + "\n");
      const patch = await f.capture();
      assert.ok(patch.includes(Buffer.from(secret)));
      assert.match(patch.toString(), /b\/-dash/);
      assert.match(patch.toString(), /line\\nbreak/);
      // Verify Git itself accepts the emitted path quoting.
      const patchPath = path.join(f.data, "capture.patch");
      await fs.writeFile(patchPath, patch);
      await f.git("apply", "--reverse", "--check", patchPath);
    } finally { await f.dispose(); }
  });

  it("rejects lexical roots and aborts a real whole-root owner", async () => {
    const f = await fixture();
    try {
      await assert.rejects(f.cache.capturePlanningDiff(f.data, f.base, f.bare), /runnerRoot/);
      await assert.rejects(f.cache.capturePlanningDiff(f.clone, "HEAD", f.bare), /40-hex/);
      const ac = new AbortController(), owner = new TickSpawner({ signal: ac.signal, killGraceMs: 100 });
      const pending = f.cache.withBoundaryProcessSpawner(async req => {
        const handle = await owner.spawn(captureSpawnRequest(req));
        ac.abort();
        return handle;
      }, ac.signal, () => f.cache.capturePlanningDiff(f.clone, f.base, f.bare));
      await assert.rejects(pending, /aborted/);
      await owner.settled();
      assert.deepEqual(owner.survivors(), []);
      assert.equal(owner.cancelledAny(), true);
    } finally { await f.dispose(); }
  });
});

describe("Unit2 bounded runner stdout transport", () => {
  it("requires a trusted boundary owner and absolute argv/cwd", async () => {
    await assert.rejects(git.readBoundedPlanningOutput(process.cwd(), [process.execPath]), /trusted boundary/);
    let spawned = false;
    await assert.rejects(git.withBoundaryProcessSpawner(async () => {
      spawned = true;
      return fake(pipe(Buffer.alloc(0)));
    }, new AbortController().signal, () => git.readBoundedPlanningOutput(process.cwd(), ["node"])), /absolute/);
    assert.equal(spawned, false);
  });

  it("reads at most cap+1 bytes with read(n), and awaits cancellation", async () => {
    const stdout = pipe(Buffer.alloc(cap * 3, 97));
    const original = stdout.read.bind(stdout);
    let total = 0;
    stdout.read = (size?: number): Buffer | null => {
      assert.ok(size !== undefined && size <= 16 * 1024);
      const chunk = original(size) as Buffer | null;
      if (chunk) total += chunk.length;
      return chunk;
    };
    const cancelled = deferred<void>();
    let calls = 0;
    let settled = false;
    const pending = run(fake(stdout, { cancel: async () => { calls++; await cancelled.promise; } }));
    const verdict = assert.rejects(pending, /exceeded 512 KiB/).then(() => { settled = true; });
    await tick();
    assert.equal(total, cap + 1);
    assert.equal(calls, 1);
    assert.equal(settled, false);
    assert.equal(stdout.listenerCount("data"), 0);
    cancelled.resolve();
    await verdict;
  });

  it("accepts exactly the cap but waits for whole-root completion", async () => {
    const completed = deferred<{ code: number }>();
    let settled = false;
    const pending = run(fake(pipe(Buffer.alloc(cap, 98)), { completed: completed.promise }))
      .then((output) => { settled = true; return output; });
    await tick();
    assert.equal(settled, false);
    completed.resolve({ code: 0 });
    assert.deepEqual(await pending, Buffer.alloc(cap, 98));
  });

  it("accepts a clean empty early EOF and refuses previously read output", async () => {
    const empty = pipe(Buffer.alloc(0));
    empty.resume();
    await tick();
    assert.equal(empty.readableEnded, true);
    assert.equal(empty.readableDidRead, false);
    assert.equal((await run(fake(empty))).length, 0);
    const consumed = pipe(Buffer.from("lost"));
    consumed.resume();
    await tick();
    let cancelled = false;
    await assert.rejects(run(fake(consumed, { cancel: async () => { cancelled = true; } })), /consumed/);
    assert.equal(cancelled, true);
  });

  it("cancels on collector error and fails closed on refused cleanup", async () => {
    const stdout = pipe();
    let cancelled = false;
    const pending = run(fake(stdout, { cancel: async () => {
      cancelled = true;
      throw new Error("owner refused reap");
    } }));
    const verdict = assert.rejects(pending, /cleanup failed/);
    await tick();
    stdout.destroy(new Error("pipe failed"));
    await verdict;
    assert.equal(cancelled, true);
  });

  it("keeps abort live after EOF until cleanup, and awaits cancel", async () => {
    const ac = new AbortController();
    const completed = deferred<{ code: number }>();
    const cancelled = deferred<void>();
    let called = false;
    let settled = false;
    const pending = run(fake(pipe(Buffer.alloc(0)), {
      completed: completed.promise,
      cancel: async () => { called = true; await cancelled.promise; completed.resolve({ code: 128 }); },
    }), ac.signal);
    const verdict = assert.rejects(pending, /aborted/).then(() => { settled = true; });
    await tick();
    ac.abort();
    await tick();
    assert.equal(called, true);
    assert.equal(settled, false);
    cancelled.resolve();
    await verdict;
    assert.equal(getEventListeners(ac.signal, "abort").length, 0);
  });

  it("bounds stderr too, and cancels rather than deadlocking on a full pipe", async () => {
    let cancelled = false;
    await assert.rejects(run(fake(pipe(Buffer.from("small stdout")), {
      stderr: pipe(Buffer.alloc(cap + 1)),
      cancel: async () => { cancelled = true; },
    })), /exceeded/);
    assert.equal(cancelled, true);
  });

  it("refuses premature close and nonzero exit without returning partial bytes", async () => {
    const stdout = pipe();
    let cancelled = false;
    const pending = run(fake(stdout, { cancel: async () => { cancelled = true; } }));
    const verdict = assert.rejects(pending, /closed before end/);
    await tick();
    stdout.destroy();
    await verdict;
    assert.equal(cancelled, true);
    await assert.rejects(run(fake(pipe(Buffer.from("partial")), {
      completed: Promise.resolve({ code: 1 }),
    })), /process failed/);
  });

  it("refuses an already aborted boundary before spawn", async () => {
    const ac = new AbortController();
    ac.abort();
    let spawned = false;
    await assert.rejects(git.withBoundaryProcessSpawner(async () => {
      spawned = true;
      return fake(pipe(Buffer.alloc(0)));
    }, ac.signal, () => git.readBoundedPlanningOutput(process.cwd(), [process.execPath])), /aborted/);
    assert.equal(spawned, false);
  });

  it("refuses success when whole-root completion rejects", async () => {
    let cancelled = false;
    await assert.rejects(run(fake(pipe(Buffer.from("unapproved")), {
      completed: Promise.reject(new Error("root not reaped")),
      cancel: async () => { cancelled = true; },
    })), /cleanup failed/);
    assert.equal(cancelled, true);
  });

  it("runs a real fast child under TickSpawner with command identity and pinned env", async () => {
    const ac = new AbortController();
    const owner = new TickSpawner({ signal: ac.signal, killGraceMs: 50 });
    let request!: BoundaryProcessRequest;
    const output = await git.withBoundaryProcessSpawner((req) => {
      request = req;
      return owner.spawn(req);
    }, ac.signal, () => git.readBoundedPlanningOutput(process.cwd(),
      [process.execPath, "-e", "process.stdout.write('fast stdout'); process.stderr.write('diagnostic')"], 2000));
    assert.equal(output.toString(), "fast stdout");
    assert.equal(request.identity, "command");
    assert.equal(request.cwd, process.cwd());
    assert.equal(request.env.PATH, runnerPath());
    assert.equal(request.env.TMPDIR, runnerTmpdir() ?? gitEnv().TMPDIR);
    for (const [key, value] of Object.entries(gitEnv())) {
      if (key !== "PATH" && key !== "TMPDIR") assert.equal(request.env[key], value, key);
    }
    await owner.settled();
    assert.deepEqual(owner.survivors(), []);
    assert.throws(() => process.kill(owner.pids()[0]!, 0), { code: "ESRCH" });
  });

  it("cancels/reaps a real overflow child instead of buffering its output", async () => {
    const ac = new AbortController();
    const owner = new TickSpawner({ signal: ac.signal, killGraceMs: 50 });
    await assert.rejects(git.withBoundaryProcessSpawner(owner.spawn, ac.signal,
      () => git.readBoundedPlanningOutput(process.cwd(), [process.execPath, "-e",
        "process.stdout.write(Buffer.alloc(2 * 1024 * 1024)); setInterval(() => {}, 1000)"], 2000)), /exceeded/);
    await owner.settled();
    assert.equal(owner.cancelledAny(), true);
    assert.deepEqual(owner.survivors(), []);
    assert.throws(() => process.kill(owner.pids()[0]!, 0), { code: "ESRCH" });
  });

  it("bounds a real silent child with a live deadline", async () => {
    const ac = new AbortController();
    const owner = new TickSpawner({ signal: ac.signal, killGraceMs: 50 });
    await assert.rejects(git.withBoundaryProcessSpawner(owner.spawn, ac.signal,
      () => git.readBoundedPlanningOutput(process.cwd(),
        [process.execPath, "-e", "setInterval(() => {}, 1000)"], 100)), /timed out|cleanup failed/);
    await owner.settled();
    assert.deepEqual(owner.survivors(), []);
    assert.throws(() => process.kill(owner.pids()[0]!, 0), { code: "ESRCH" });
  });
});
