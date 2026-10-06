import { describe, it } from "node:test";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import assert from "node:assert/strict";
import { PassThrough, Readable } from "node:stream";
import { getEventListeners } from "node:events";
import { createHash } from "node:crypto";
import { crc32, deflateSync } from "node:zlib";
import { GitCache, gitEnv } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import { TickSpawner } from "../src/tick-spawner.js";
import { runnerPath, runnerTmpdir } from "../src/runner-uid.js";
import { nullLogger } from "./helpers.js";
import { commandSandboxArgv } from "../src/codex/codex-executor.js";
import { probeLandlockAvailability } from "../src/codex/codex-capability.js";

const cap = 512 * 1024;
const git = new GitCache(process.cwd(), nullLogger());
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

const exec = promisify(execFile);
async function fixture(options: { seed?: (seed: string) => Promise<void>; shared?: boolean; meter?: boolean; sandbox?: boolean } = {}): Promise<{
  cache: GitCache; clone: string; base: string; data: string;
  git: (...args: string[]) => Promise<string>;
  capture: (signal?: AbortSignal) => Promise<Buffer>;
  dispose: () => Promise<void>;
}> {
  const tempRoot = await fs.realpath(os.tmpdir());
  const data = await fs.mkdtemp(path.join(tempRoot, "planning-fixture-"));
  const seed = path.join(data, "seed");
  const clone = path.join(data, "runner", "repo", "issue-1");
  await fs.mkdir(seed);
  const run = async (cwd: string, ...args: string[]): Promise<string> =>
    (await exec("git", ["-C", cwd, ...args], { env: gitEnv(), timeout: 5000 })).stdout;
  await run(seed, "init", "--quiet", "--template=");
  await fs.writeFile(path.join(seed, "tracked"), "base\n");
  await fs.writeFile(path.join(seed, "deleted"), "delete me\n");
  await fs.writeFile(path.join(seed, ".gitignore"), "ignored\nignored-dir/\n");
  await options.seed?.(seed);
  await run(seed, "add", ".");
  await run(seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "base");
  await fs.mkdir(path.dirname(clone), { recursive: true });
  await run(seed, "clone", "--quiet", options.shared ? "--shared" : "--no-local", seed, clone);
  const base = (await run(clone, "rev-parse", "HEAD")).trim();
  const cache = new GitCache(data, nullLogger());
  return {
    data, clone, base, cache, git: (...args) => run(clone, ...args),
    capture: async (signal = new AbortController().signal) => {
      const owner = new TickSpawner({ signal, killGraceMs: 100 });
      const privateTmp = options.sandbox
        ? await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "planning-command-")) : undefined;
      try {
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
` + argv[2];
            return owner.spawn({
              ...request,
              argv: ["/usr/local/bin/uzi-codex-command-sandbox",
                ...commandSandboxArgv(clone, clone, argv[0]!, argv.slice(1), privateTmp, "required")],
              env: { ...request.env, HOME: privateTmp, TMPDIR: privateTmp },
            });
          }
          if (!options.meter) return owner.spawn(request);
          // Instrument only the trusted inline capture program, preserving its argv and body.
          const argv = [...request.argv];
          assert.equal(argv[1], "-e");
          const metricPath = path.join(data, "read-metrics.json");
          argv[2] = `
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
` + argv[2];
          return owner.spawn({ ...request, argv });
        }, signal, () => cache.capturePlanningDiff(clone, base));
      } finally {
        await owner.settled();
        assert.deepEqual(owner.survivors(), []);
        if (privateTmp) await fs.rm(privateTmp, { recursive: true, force: true });
      }
    },
    dispose: () => fs.rm(data, { recursive: true, force: true }),
  };
}

// Native capture walks Linux /proc/<pid>/fd paths (src/git.ts); Linux CI runs this block.
describe("Unit2 runner source capture", { skip: process.platform !== "linux" && "Linux /proc capture required" }, () => {
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
            { env: gitEnv(), timeout: 5000 });
          const patchPath = path.join(f.data, "replacement.patch");
          await fs.writeFile(patchPath, patch);
          await exec("git", ["-C", target, "apply", patchPath], { env: gitEnv(), timeout: 5000 });
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
          await assert.rejects(f.capture(), /process failed/);
        } finally { await f.dispose(); }
      });
    }
  }

  for (const kind of ["outside loose symlink", "outside loose directory symlink", "forged blob", "forged commit", "forged tree", "packed pack symlink", "packed idx symlink", "packed directory symlink"]) {
    it("refuses object integrity attack: " + kind, async () => {
      const f = await fixture();
      let forged: { type: string; oid: string } | undefined;
      try {
        if (!kind.startsWith("packed")) {
          // Force a loose-only baseline so Git cannot prefer an authentic packed duplicate.
          await fs.rm(path.join(f.clone, ".git/objects"), { recursive: true });
          await fs.cp(path.join(f.data, "seed/.git/objects"), path.join(f.clone, ".git/objects"), { recursive: true });
        }
        await fs.unlink(path.join(f.clone, "tracked"));
        assert.match((await f.capture()).toString(), /-base/);
        if (kind.startsWith("packed")) {
          await f.git("repack", "-a", "-d");
          assert.match((await f.capture()).toString(), /-base/);
          const packDir = path.join(f.clone, ".git/objects/pack");
          const suffix = kind.includes("idx") ? ".idx" : ".pack";
          const name = (await fs.readdir(packDir)).find(name => name.endsWith(suffix))!;
          const object = path.join(packDir, name), outside = path.join(f.data, "outside-pack");
          if (kind === "packed directory symlink") {
            await fs.cp(packDir, outside, { recursive: true });
            await fs.rm(packDir, { recursive: true });
            await fs.symlink(outside, packDir);
          } else {
            await fs.copyFile(object, outside);
            await fs.unlink(object);
            await fs.symlink(outside, object);
          }
        } else {
          const type = kind.includes("commit") ? "commit" : kind.includes("tree") ? "tree" : "blob";
          const oid = (await f.git("rev-parse", type === "commit" ? "HEAD" : type === "tree" ? "HEAD^{tree}" : "HEAD:tracked")).trim();
          if (kind.startsWith("forged")) forged = { type, oid };
          let body: Buffer;
          if (type === "commit") body = Buffer.from((await f.git("cat-file", "commit", oid)) + "forged message\n");
          else if (type === "tree") {
            // An otherwise valid tree under the original OID points tracked at a different authentic blob.
            const other = (await f.git("rev-parse", "HEAD:deleted")).trim();
            body = Buffer.concat([Buffer.from("100644 tracked\0"), Buffer.from(other, "hex")]);
          } else body = Buffer.from(kind.startsWith("outside") ? "base\n" : "evil\n"); // Same size; symlink controls use authentic bytes.
          assert.equal(createHash("sha1").update(type + " " + body.length + "\0").update(body).digest("hex") === oid,
            kind.startsWith("outside"));
          const bytes = deflateSync(Buffer.concat([Buffer.from(type + " " + body.length + "\0"), body]));
          const object = path.join(f.clone, ".git/objects", oid.slice(0, 2), oid.slice(2));
          await fs.mkdir(path.dirname(object), { recursive: true });
          await fs.rm(object, { force: true });
          if (kind === "outside loose directory symlink") {
            // Redirect an authentic fanout directory, not just the final object file.
            await fs.writeFile(object, bytes);
            const outside = path.join(f.data, "outside-fanout");
            await fs.cp(path.dirname(object), outside, { recursive: true });
            await fs.rm(path.dirname(object), { recursive: true });
            await fs.symlink(outside, path.dirname(object));
          } else if (kind === "outside loose symlink") {
            const outside = path.join(f.data, "outside-object");
            await fs.writeFile(outside, bytes);
            await fs.symlink(outside, object);
          } else if (type === "blob") await fs.writeFile(object, bytes);
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
            const packDir = path.join(f.clone, ".git/objects/pack");
            await fs.mkdir(packDir, { recursive: true });
            const stem = path.join(packDir, "pack-" + packHash.toString("hex"));
            await fs.writeFile(stem + ".pack", pack);
            await fs.writeFile(stem + ".idx", Buffer.concat([idx, createHash("sha1").update(idx).digest()]));
          }
        }
        if (forged) {
          // Prove Git serves the forged body; rejection must come from capture authentication.
          const { type, oid } = forged;
          const served = (await exec("git", ["-C", f.clone, "cat-file", type, oid],
            { env: gitEnv(), timeout: 5000, encoding: "buffer" })).stdout;
          assert.notEqual(createHash("sha1").update(type + " " + served.length + "\0").update(served).digest("hex"), oid);
        }
        await assert.rejects(f.capture(), kind.startsWith("forged") ? /base object integrity mismatch/ : /ELOOP|ENOTDIR/);
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
      await assert.rejects(f.capture(), /non UTF8 path metadata/);
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
      await assert.rejects(f.capture(), /process failed/);
    } finally { await f.dispose(); }
  });

  it("bounds source bytes even when huge binary content would produce no patch", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.clone, "huge"), Buffer.alloc(4 * 1024 * 1024 + 1));
      await assert.rejects(f.capture(), /process failed/);
    } finally { await f.dispose(); }
  });

  it("refuses aggregate overflow from unchanged baseline text without producing a patch", async () => {
    const f = await fixture({ meter: true, seed: async seed => {
      const text = Buffer.alloc(4 * 1024 * 1024, 120);
      for (let i = 0; i < 33; i++) await fs.writeFile(path.join(seed, "unchanged-" + i), text);
    } });
    try {
      // Every file hashes to its verified base OID; snapshot and raw object pipes also consume the budget.
      await assert.rejects(f.capture(), /total source cap/);
      const metrics = JSON.parse(await fs.readFile(path.join(f.data, "read-metrics.json"), "utf8"));
      console.log("aggregate source actual reads:", metrics.source);
      assert.equal(metrics.source, 128 * 1024 * 1024 + 1);
      assert.equal(metrics.diff, 0);
    } finally { await f.dispose(); }
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
      await assert.rejects(f.capture(), /Git pipe cap|patch cap/);
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

  it("refuses a direct shared clone and captures after repack and removal of alternates", async () => {
    const f = await fixture({ shared: true });
    try {
      const alternates = path.join(f.clone, ".git/objects/info/alternates");
      assert.ok((await fs.readFile(alternates)).length > 0);
      await assert.rejects(f.capture(), /object alternates unsupported/);
      // This is the materialization GitCache's existing selfContained preparation performs.
      await f.git("repack", "-a", "-d");
      await fs.unlink(alternates);
      await fs.rm(path.join(f.data, "seed"), { recursive: true });
      assert.equal((await f.capture()).length, 0);
      await fs.writeFile(path.join(f.clone, "tracked"), "dissociated change\n");
      assert.match((await f.capture()).toString(), /-base\n\+dissociated change/);
    } finally { await f.dispose(); }
  });

  it("ignores hostile local config includes even when config itself is a FIFO", async () => {
    const f = await fixture();
    try {
      const fifo = path.join(f.data, "hostile-config");
      await exec("mkfifo", [fifo], { env: gitEnv(), timeout: 5000 });
      await f.git("config", "include.path", fifo);
      await fs.writeFile(path.join(f.clone, "tracked"), "include-safe change\n");
      assert.match((await f.capture()).toString(), /\+include-safe change/);
      await fs.unlink(path.join(f.clone, ".git/config"));
      await exec("mkfifo", [path.join(f.clone, ".git/config")], { env: gitEnv(), timeout: 5000 });
      assert.match((await f.capture()).toString(), /\+include-safe change/);
    } finally { await f.dispose(); }
  });

  it("refuses leaf and parent symlink escapes, nonregular ignores and an unavailable base", async () => {
    const f = await fixture();
    try {
      await fs.writeFile(path.join(f.data, "outside"), "outside content\n");
      await fs.symlink(path.join(f.data, "outside"), path.join(f.clone, "escape"));
      await assert.rejects(f.capture(), /process failed/);
      await fs.unlink(path.join(f.clone, "escape"));
      await fs.mkdir(path.join(f.clone, "parent"));
      await fs.writeFile(path.join(f.clone, "parent", "file"), "inside\n");
      await f.git("add", "parent/file");
      await fs.rm(path.join(f.clone, "parent"), { recursive: true });
      await fs.symlink(f.data, path.join(f.clone, "parent"));
      await assert.rejects(f.capture(), /process failed/);
      await fs.unlink(path.join(f.clone, "parent"));
      await fs.mkdir(path.join(f.clone, ".gitignore-bad"));
      await fs.unlink(path.join(f.clone, ".gitignore"));
      await fs.symlink(path.join(f.data, "outside"), path.join(f.clone, ".gitignore"));
      await assert.rejects(f.capture(), /process failed/);
      await fs.unlink(path.join(f.clone, ".gitignore"));
      await fs.writeFile(path.join(f.clone, ".gitignore"), "");
      const ac = new AbortController(), owner = new TickSpawner({ signal: ac.signal });
      await assert.rejects(f.cache.withBoundaryProcessSpawner(owner.spawn, ac.signal,
        () => f.cache.capturePlanningDiff(f.clone, "0".repeat(40))), /process failed/);
      await owner.settled();
      assert.deepEqual(owner.survivors(), []);
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
      await assert.rejects(f.cache.capturePlanningDiff(f.data, f.base), /runnerRoot/);
      await assert.rejects(f.cache.capturePlanningDiff(f.clone, "HEAD"), /40-hex/);
      const ac = new AbortController(), owner = new TickSpawner({ signal: ac.signal, killGraceMs: 100 });
      const pending = f.cache.withBoundaryProcessSpawner(async req => {
        const handle = await owner.spawn(req);
        ac.abort();
        return handle;
      }, ac.signal, () => f.cache.capturePlanningDiff(f.clone, f.base));
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
