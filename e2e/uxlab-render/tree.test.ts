import assert from "node:assert/strict";
import { test } from "node:test";
import { spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { inventory, verifyPNGs } from "./tree.js";

async function fixture(t: { after: (fn: () => Promise<void>) => void }) {
  const dir = await fs.mkdtemp(fileURLToPath(new URL("../../.uzi/scratch/uxlab-tree-", import.meta.url)));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  return dir;
}
test("copy helper CLI loads outside an ESM package before validating its destination", () => {
  const index = process.execArgv.indexOf("--import");
  assert(index >= 0, "run fixture tests with their supported tsx loader");
  const result = spawnSync(process.execPath, ["--import", process.execArgv[index + 1],
    fileURLToPath(new URL("./copy.ts", import.meta.url)), "unused", "invalid"],
  { encoding: "utf8", timeout: 10_000, maxBuffer: 65536 });
  assert.ifError(result.error);
  assert.equal(result.status, 1);
  assert.match(result.stderr, /invalid copy destination/);
  assert.doesNotMatch(result.stderr, /Transform failed|Top-level await/);
});
test("full export copy keeps bytes, executable scripts, safe links and both manifests", async (t) => {
  const dir = await fixture(t), source = path.join(dir, "src"), dest = path.join(dir, "dest");
  await fs.mkdir(source); await fs.mkdir(dest);
  await fs.mkdir(path.join(source, "nested"));
  await fs.mkdir(path.join(source, ".git"));
  await fs.writeFile(path.join(source, ".git", "config"), "omitted");
  await fs.writeFile(path.join(source, "devbox.json"), '{"packages":["ruby@4.0.6"]}');
  await fs.writeFile(path.join(source, "nested", "devbox.json"), '{"packages":["charm-freeze@0.2.2"]}');
  await fs.writeFile(path.join(source, "build.sh"), "#!/bin/sh\nexit 0\n", { mode: 0o755 });
  await fs.symlink("../devbox.json", path.join(source, "nested", "link"));
  const result = await inventory(source, dest);
  assert.equal(Object.keys(result).length, 3);
  for (const name of Object.keys(result))
    assert((await fs.readFile(path.join(source, name))).equals(await fs.readFile(path.join(dest, name))), name);
  assert.equal((await fs.stat(path.join(dest, "build.sh"))).mode & 0o777, 0o770);
  assert.equal(await fs.readlink(path.join(dest, "nested", "link")), "../devbox.json");
  assert.equal(await fs.stat(path.join(dest, ".git")).then(() => true, () => false), false);
});
test("export rejects escaping, absolute and omitted-directory links", async (t) => {
  const dir = await fixture(t);
  for (const link of ["../escape", "/etc/passwd", ".git/config"]) {
    await fs.symlink(link, path.join(dir, "link"));
    await assert.rejects(inventory(dir), /source link|omitted source/);
    await fs.unlink(path.join(dir, "link"));
  }
});
test("source canonical containment rejects composed escapes, dangling links, cycles and omitted targets", async (t) => {
  const dir = await fixture(t), source = path.join(dir, "src");
  await fs.mkdir(source);
  await fs.writeFile(path.join(dir, "outside"), "outside");
  await fs.symlink(".", path.join(source, "d"));
  await fs.symlink("d/../outside", path.join(source, "link"));
  await assert.rejects(inventory(source), /escaping source link/);
  await fs.unlink(path.join(source, "link"));
  for (const target of ["missing", "link"]) {
    await fs.symlink(target, path.join(source, "link"));
    await assert.rejects(inventory(source), /unresolvable source link/);
    await fs.unlink(path.join(source, "link"));
  }
  await fs.mkdir(path.join(source, ".uzi"));
  await fs.writeFile(path.join(source, ".uzi", "hidden"), "hidden");
  await fs.symlink(".uzi", path.join(source, "alias"));
  await fs.symlink("alias/hidden", path.join(source, "link"));
  await assert.rejects(inventory(source), /omitted source/);
});
test("artifact traversal validates omitted names and rejects their links", async (t) => {
  const dir = await fixture(t);
  for (const name of [".git", ".uzi", "node_modules"]) {
    await fs.mkdir(path.join(dir, name));
    await fs.writeFile(path.join(dir, name, "file"), "visible");
    assert((await inventory(dir, undefined, undefined, false))[name + "/file"]);
    await fs.symlink("file", path.join(dir, name, "link"));
    await assert.rejects(inventory(dir, undefined, undefined, false), /artifact symlink/);
    await fs.rm(path.join(dir, name), { recursive: true });
    await fs.symlink(".", path.join(dir, name));
    await assert.rejects(inventory(dir, undefined, undefined, false), /artifact symlink/);
    await fs.unlink(path.join(dir, name));
  }
});
test("export entry, depth, per-file and total budgets fail closed", async (t) => {
  const dir = await fixture(t);
  await fs.writeFile(path.join(dir, "a"), "123");
  await fs.writeFile(path.join(dir, "b"), "456");
  const limits = { entries: 2, totalBytes: 6, fileBytes: 3, depth: 1 };
  assert.equal(Object.keys(await inventory(dir, undefined, limits)).length, 2);
  await assert.rejects(inventory(dir, undefined, { ...limits, entries: 1 }), /entry limit/);
  await assert.rejects(inventory(dir, undefined, { ...limits, fileBytes: 2 }), /byte limit/);
  await assert.rejects(inventory(dir, undefined, { ...limits, totalBytes: 5 }), /byte limit/);
  await fs.mkdir(path.join(dir, "nested"));
  await assert.rejects(inventory(dir, undefined, { ...limits, entries: 4, depth: 0 }), /depth limit/);
});
async function makePNGs(dir: string) {
  await fs.mkdir(path.join(dir, "frames")); await fs.mkdir(path.join(dir, "png"));
  for (const theme of ["dark", "light"]) {
    const name = "real-scene-" + theme, frame = path.join(dir, "frames", name + ".ansi");
    await fs.writeFile(frame, "generated ANSI");
    await fs.utimes(frame, 1000, 1000);
    const bytes = Buffer.alloc(33);
    Buffer.from("89504e470d0a1a0a", "hex").copy(bytes);
    bytes.writeUInt32BE(13, 8); bytes.write("IHDR", 12);
    bytes.writeUInt32BE(1200, 16); bytes.writeUInt32BE(900, 20);
    const png = path.join(dir, "png", name + ".png");
    await fs.writeFile(png, bytes); await fs.utimes(png, 1001, 1001);
  }
}
test("artifact seam requires both themes, exact inventory, PNG header/dimensions and strict freshness", async (t) => {
  const dir = await fixture(t); await makePNGs(dir);
  const receipts = await verifyPNGs(dir);
  assert.equal(receipts.length, 2); assert.equal(receipts[0].width, 1200); assert.equal(receipts[0].height, 900);
  assert.equal(receipts[0].frame.name, receipts[0].name.replace(/\.png$/, ".ansi"));
  assert(BigInt(receipts[0].mtimeNs) > BigInt(receipts[0].frame.mtimeNs));
  const png = path.join(dir, "png", "real-scene-dark.png");
  await fs.utimes(png, 1000, 1000);
  await assert.rejects(verifyPNGs(dir), /stale PNG/);
  await fs.utimes(png, 1001, 1001);
  const valid = await fs.readFile(png);
  const corrupt = Buffer.from(valid); corrupt[0] = 0;
  await fs.writeFile(png, corrupt);
  await assert.rejects(verifyPNGs(dir), /signature/);
  valid.writeUInt32BE(0, 16); await fs.writeFile(png, valid);
  await assert.rejects(verifyPNGs(dir), /dimensions/);
  await fs.unlink(path.join(dir, "frames", "real-scene-light.ansi"));
  await fs.unlink(path.join(dir, "png", "real-scene-light.png"));
  await assert.rejects(verifyPNGs(dir), /theme peer/);
});
test("artifact inventory rejects links instead of silently exporting extra files", async (t) => {
  const dir = await fixture(t); await makePNGs(dir);
  await fs.symlink("real-scene-dark.png", path.join(dir, "png", "extra.png"));
  await assert.rejects(verifyPNGs(dir), /artifact symlink/);
});
test("seed inventory records byte identity and nanosecond mtimes", async (t) => {
  const dir = await fixture(t), seed = path.join(dir, "seed.ansi");
  await fs.writeFile(seed, "before");
  const before = await inventory(dir);
  assert.equal(before["seed.ansi"].mtimeNs, String((await fs.stat(seed, { bigint: true })).mtimeNs));
  await fs.writeFile(seed, "after");
  const after = await inventory(dir);
  assert.notEqual(before["seed.ansi"].sha256, after["seed.ansi"].sha256);
});
