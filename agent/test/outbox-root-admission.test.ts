import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const errno = (code: string) => Object.assign(new Error("root fixture"), { code });
const store = (root: string) => new Outbox({ root, log: nullLogger(), runMaxBytes: 1e6,
  maxBytes: 4e6, retentionMs: 86400000 });

for (const location of ["root", "ancestor"] as const)
it(`root admission: a FIFO at the ${location} agrees with the held source proof`, async t => {
  const dir = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "outbox-fifo-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const fifo = path.join(dir, "fifo");
  const made = spawnSync("mkfifo", [fifo], { encoding: "utf8" });
  assert.ifError(made.error);
  assert.equal(made.status, 0, made.stderr);
  const outbox = store(location === "root" ? fifo : path.join(fifo, "outbox"));
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true, "production source proof retains special files");
  assert.equal(outbox.unusableRootReason(), "unreadable", "do not admit claims that cannot prove a safe first writer");
});

it("root admission: a dangling root symlink is diagnosed after mkdir fails", async t => {
  const dir = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "outbox-dangling-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  await fs.symlink(path.join(dir, "missing"), root, "dir");
  const outbox = store(root);
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  assert.equal(outbox.unusableRootReason(), "symlink");
});

it("root admission: an ENOTDIR root lookup walks to the symlinked ancestor", async t => {
  const dir = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "outbox-alias-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  await fs.writeFile(path.join(dir, "file"), "regular file");
  await fs.symlink(path.join(dir, "file"), path.join(dir, "alias"));
  const outbox = store(path.join(dir, "alias", "outbox"));
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  assert.equal(outbox.unusableRootReason(), "symlink");
});

for (const code of ["EACCES", "ENOTDIR"])
it(`root admission: an ENOENT root lookup retains an uncertain ${code} ancestor`, async t => {
  const dir = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "outbox-ancestor-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const mkdir = fs.mkdir.bind(fs), lstat = fs.lstat.bind(fs);
  let roots = 0, ancestors = 0;
  t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
    if (String(args[0]) === root) throw errno("EACCES");
    return mkdir(...args);
  });
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (String(args[0]) === root) { roots++; throw errno("ENOENT"); }
    if (String(args[0]) === dir) { ancestors++; throw errno(code); }
    return lstat(...args);
  });
  const outbox = store(root);
  await outbox.init();
  assert.equal(roots, 1, "the initial root lookup failed before the component walk");
  assert.equal(ancestors, 1, "the component walk reached the uncertain ancestor");
  assert.equal(outbox.isDisabled(), true);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.equal(outbox.unusableRootReason(), "unreadable");
});

it("root admission: a directory reappearing during the component walk stays disabled", async t => {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "outbox-reappeared-"));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const mkdir = fs.mkdir.bind(fs), lstat = fs.lstat.bind(fs);
  let roots = 0;
  t.mock.method(fs, "mkdir", async (...args: Parameters<typeof fs.mkdir>) => {
    if (String(args[0]) === root) throw errno("EACCES");
    return mkdir(...args);
  });
  t.mock.method(fs, "lstat", async (...args: Parameters<typeof fs.lstat>) => {
    if (String(args[0]) === root && ++roots === 1) throw errno("ENOENT");
    return lstat(...args);
  });
  const outbox = store(root);
  await outbox.init();
  assert.equal(roots, 2, "the failed root lookup and successful walk are both exercised");
  assert.equal(outbox.isDisabled(), true);
  assert.equal(outbox.unusableRootReason(), "unreadable");
});
