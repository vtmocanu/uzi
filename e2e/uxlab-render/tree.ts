// Bounded repository copy and artifact receipts; validate source links before copying.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { readRegularFile } from "./inputs.js";

const MAX_ENTRIES = 32768;
const MAX_TOTAL = 256 * 1024 * 1024;
const MAX_FILE = 16 * 1024 * 1024;
const OMIT = new Set([".git", ".uzi", "node_modules"]);
export async function inventory(root: string, copyTo?: string, limits = { entries: MAX_ENTRIES, totalBytes: MAX_TOTAL, fileBytes: MAX_FILE, depth: 64 }, allowLinks = true) {
  for (const [value, max] of [[limits.entries, MAX_ENTRIES], [limits.totalBytes, MAX_TOTAL], [limits.fileBytes, MAX_FILE], [limits.depth, 64]])
    assert(Number.isSafeInteger(value) && value >= 0 && value <= max, "invalid tree limit");
  const result: Record<string, { sha256: string; bytes: number; mtimeNs: string }> = {};
  // Source is a stable readonly export throughout validation and copy.
  const canonicalRoot = await fs.realpath(root);
  let entries = 0, total = 0;
  async function walk(dir: string, depth: number): Promise<void> {
    assert(depth <= limits.depth, "tree depth limit");
    assert((await fs.lstat(dir)).isDirectory(), "tree root/directory must be real");
    const names: string[] = [];
    const handle = await fs.opendir(dir);
    try {
      for (;;) {
        const entry = await handle.read();
        if (!entry) break;
        assert(++entries <= limits.entries, "tree entry limit");
        names.push(entry.name);
      }
    } finally { await handle.close(); }
    for (const name of names.sort()) {
      if (allowLinks && OMIT.has(name)) continue;
      const file = path.join(dir, name), relative = path.relative(root, file);
      const stat = await fs.lstat(file, { bigint: true });
      const target = copyTo && path.join(copyTo, relative);
      if (stat.isDirectory()) {
        if (target) await fs.mkdir(target, { mode: 0o2770 });
        await walk(file, depth + 1);
      } else if (stat.isSymbolicLink()) {
        assert(allowLinks, "artifact symlink");
        const link = await fs.readlink(file);
        assert(!path.isAbsolute(link), "absolute source link");
        const resolved = path.relative(root, path.resolve(path.dirname(file), link));
        assert(resolved !== ".." && !resolved.startsWith("../"), "escaping source link");
        assert(!resolved.split(path.sep).some((part) => OMIT.has(part)), "link into omitted source");
        let canonical: string;
        try { canonical = path.relative(canonicalRoot, await fs.realpath(file)); }
        catch { throw new Error("unresolvable source link"); }
        assert(canonical !== ".." && !canonical.startsWith("../"), "escaping source link");
        assert(!canonical.split(path.sep).some((part) => OMIT.has(part)), "link into omitted source");
        total += Buffer.byteLength(link);
        assert(total <= limits.totalBytes, "tree total byte limit");
        if (target) await fs.symlink(link, target);
      } else {
        const bytes = await readRegularFile(file, Math.min(limits.fileBytes, limits.totalBytes - total));
        total += bytes.length;
        result[relative] = { sha256: createHash("sha256").update(bytes).digest("hex"),
          bytes: bytes.length, mtimeNs: String(stat.mtimeNs) };
        if (target) await fs.writeFile(target, bytes, { mode: (Number(stat.mode) & 0o111) ? 0o770 : 0o660, flag: "wx" });
      }
    }
  }
  await walk(root, 0);
  return result;
}

export async function verifyPNGs(lab: string) {
  const frames = await inventory(path.join(lab, "frames"), undefined, undefined, false);
  const pngs = await inventory(path.join(lab, "png"), undefined, undefined, false);
  const names = Object.keys(frames);
  assert(names.length > 0 && names.length <= 2048, "bounded nonempty generated set");
  assert.equal(Object.keys(pngs).length, names.length, "exact PNG inventory");
  const receipts = [];
  for (const name of names) {
    assert(/^[a-zA-Z0-9_.-]+-(light|dark)\.ansi$/.test(name), "unexpected generated frame");
    const peer = name.replace(/-(light|dark)\.ansi$/, (_all, theme) => `-${theme === "light" ? "dark" : "light"}.ansi`);
    assert(frames[peer], `missing theme peer: ${name}`);
    const png = name.replace(/\.ansi$/, ".png");
    assert(pngs[png], `missing PNG: ${png}`);
    assert(BigInt(pngs[png].mtimeNs) > BigInt(frames[name].mtimeNs), `stale PNG: ${png}`);
    const bytes = await readRegularFile(path.join(lab, "png", png), MAX_FILE);
    assert(bytes.length >= 33 && bytes.subarray(0, 8).equals(Buffer.from("89504e470d0a1a0a", "hex")), `PNG signature: ${png}`);
    assert.equal(bytes.readUInt32BE(8), 13, `IHDR length: ${png}`);
    assert.equal(bytes.toString("ascii", 12, 16), "IHDR", `IHDR: ${png}`);
    const width = bytes.readUInt32BE(16), height = bytes.readUInt32BE(20);
    assert(width > 0 && height > 0 && width <= 16384 && height <= 16384, `PNG dimensions: ${png}`);
    receipts.push({ name: png, width, height, ...pngs[png], frame: { name, ...frames[name] } });
  }
  return receipts;
}
