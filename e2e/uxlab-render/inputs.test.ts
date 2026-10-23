import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { test } from "node:test";
import { readRegularFile, sourceId, MANIFEST_MAX_BYTES, HELPER_MAX_BYTES } from "./inputs.js";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
async function fixture(t: { after: (fn: () => Promise<void>) => void }) {
  const dir = await fs.mkdtemp(path.join(scratch, "uxlab-inputs-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  return dir;
}

test("regular bytes, empty file and binary helper hashes stay identical", async (t) => {
  const dir = await fixture(t);
  const file = path.join(dir, "helper");
  const bytes = Buffer.from([0, 255, 10, 128, 42]);
  await fs.writeFile(file, bytes);
  const actual = createHash("sha256").update(await readRegularFile(file, HELPER_MAX_BYTES)).digest("hex");
  assert.equal(actual, createHash("sha256").update(bytes).digest("hex"));
  await fs.writeFile(file, "");
  assert.equal((await readRegularFile(file, 0)).length, 0);
});

test("reject actual 2 MiB manifest before reading and malformed input paths", async (t) => {
  const dir = await fixture(t);
  const file = path.join(dir, "manifest");
  await fs.writeFile(file, Buffer.alloc(2 * 1024 * 1024, 32));
  t.mock.method(fs, "open", async () => { throw new Error("must reject before open/allocation"); });
  await assert.rejects(readRegularFile(file, MANIFEST_MAX_BYTES), /byte limit/);
  await assert.rejects(readRegularFile(path.join(dir, "missing"), MANIFEST_MAX_BYTES), /ENOENT/);
  await assert.rejects(readRegularFile(file, -1), /limit/);
});

test("directories, symlinks, FIFO and devices are never read", async (t) => {
  const dir = await fixture(t);
  const file = path.join(dir, "file");
  await fs.writeFile(file, "ok");
  await fs.symlink(file, path.join(dir, "link"));
  execFileSync("mkfifo", [path.join(dir, "fifo")], { timeout: 5000 });
  for (const target of [dir, path.join(dir, "link"), path.join(dir, "fifo"), "/dev/null"]) {
    await assert.rejects(readRegularFile(target, 16), /regular file/);
  }
});

test("growth after stat is capped and the handle closes on error", async (t) => {
  const dir = await fixture(t);
  const file = path.join(dir, "growing");
  await fs.writeFile(file, "a");
  const open = fs.open.bind(fs);
  let closed = false;
  let bytesRead = 0;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const handle = await open(...args);
    const stat = handle.stat.bind(handle);
    const read = handle.read.bind(handle);
    const close = handle.close.bind(handle);
    t.mock.method(handle, "stat", async () => {
      const before = await stat();
      await fs.appendFile(file, "bcdefghijklmnop");
      return before;
    });
    t.mock.method(handle, "read", async (...readArgs: Parameters<typeof handle.read>) => {
      const result = await read(...readArgs);
      bytesRead += result.bytesRead;
      return result;
    });
    t.mock.method(handle, "close", async () => { closed = true; await close(); });
    return handle;
  });
  await assert.rejects(readRegularFile(file, 4), /byte limit/);
  assert.equal(closed, true);
  assert(bytesRead <= 5, "read at most cap plus one overflow byte");
});

test("source hash preserves sorted paths, binary content and delimiters", async (t) => {
  const dir = await fixture(t);
  await fs.mkdir(path.join(dir, "nested"));
  await fs.writeFile(path.join(dir, "z"), "last");
  await fs.writeFile(path.join(dir, "nested", "a"), Buffer.from([255, 0]));
  await fs.writeFile(path.join(dir, "a"), "first");
  const expected = createHash("sha256")
    .update("a\0first\0nested/a\0").update(Buffer.from([255, 0]))
    .update("\0z\0last\0").digest("hex");
  assert.equal(await sourceId(dir), expected);
});

test("source per-file and cumulative byte limits", async (t) => {
  const dir = await fixture(t);
  await fs.writeFile(path.join(dir, "a"), "123");
  await fs.writeFile(path.join(dir, "b"), "456");
  await assert.rejects(sourceId(dir, { fileBytes: 2 }), /byte limit/);
  await assert.rejects(sourceId(dir, { totalBytes: 5 }), /byte limit/);
  assert.equal(typeof await sourceId(dir, { fileBytes: 3, totalBytes: 6 }), "string");
});

test("entry limit counts files and directories globally before collecting names", async (t) => {
  const dir = await fixture(t);
  await fs.mkdir(path.join(dir, "a"));
  await fs.writeFile(path.join(dir, "a", "b"), "");
  await fs.writeFile(path.join(dir, "c"), "");
  t.mock.method(fs, "readdir", async () => { throw new Error("must use bounded opendir iteration"); });
  const opendir = fs.opendir.bind(fs);
  let reads = 0;
  let closes = 0;
  t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    const directory = await opendir(...args);
    const read = directory.read.bind(directory);
    const close = directory.close.bind(directory);
    t.mock.method(directory, "read", async () => { reads++; return read(); });
    t.mock.method(directory, "close", async () => { closes++; await close(); });
    return directory;
  });
  await assert.rejects(sourceId(dir, { entries: 2 }), /entry limit/);
  assert(reads <= 5, "stop enumeration at first entry past the global cap");
  assert.equal(closes, 2, "root and nested directory handles close on overflow");
  assert.equal(typeof await sourceId(dir, { entries: 3 }), "string");
  // A second attempt exercises directory cleanup after the limit error.
  await assert.rejects(sourceId(dir, { entries: 0 }), /entry limit/);
});

test("depth limit includes empty directories and rejects symlink roots and entries", async (t) => {
  const dir = await fixture(t);
  await fs.mkdir(path.join(dir, "a", "b"), { recursive: true });
  await assert.rejects(sourceId(dir, { depth: 1 }), /depth limit/);
  assert.equal(typeof await sourceId(dir, { depth: 2 }), "string");
  const link = path.join(dir, "link");
  await fs.symlink(path.join(dir, "a"), link);
  await assert.rejects(sourceId(link), /directory/);
  await assert.rejects(sourceId(dir), /regular/);
  await fs.unlink(link);
  await fs.writeFile(path.join(dir, "regular"), "ok");
  await fs.symlink(path.join(dir, "regular"), link);
  await assert.rejects(sourceId(dir), /regular/);
  await fs.unlink(link);
  execFileSync("mkfifo", [path.join(dir, "fifo")], { timeout: 5000 });
  await assert.rejects(sourceId(dir), /regular/);
});
