import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { evictCacheUnits, listGoBuildUnits, listNpmCache, measureRunCaches } from "../src/rmtree.js";
import { trimRunCaches, type TrimTarget } from "../src/run-caches.js";
import { nullLogger } from "./helpers.js";

// PRD #1809 D4: the between-turns TRIM of a running run's caches — least recently used entries
// first, never a wipe — through the pinned, agent-uid helpers. Fixture trees only (this host runs
// single-uid, so every helper pass runs as the test's own uid); sizes are allocated bytes.

const HAS_PROC_FD = fs.existsSync("/proc/self/fd");
const skipReason = "no /proc/self/fd on this host: the pinned helpers refuse here by design";

let root: string;
let home: string;
let outside: string;

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-trim-"));
  home = path.join(root, "home");
  outside = path.join(root, "outside");
  fs.mkdirSync(home);
  fs.mkdirSync(outside);
});

afterEach(() => {
  const widen = (p: string): void => {
    let st;
    try {
      st = fs.lstatSync(p);
    } catch {
      return;
    }
    if (!st.isDirectory()) return;
    fs.chmodSync(p, 0o700);
    for (const e of fs.readdirSync(p)) widen(path.join(p, e));
  };
  widen(root);
  fs.rmSync(root, { recursive: true, force: true });
});

const T0 = Date.parse("2026-09-01T00:00:00Z") / 1000;
const GB = path.join(".cache", "go-build");
const KIB = 1024;

/** Write `bytes` of data to `rel` under HOME and set its mtime to T0 + `age` hours. */
function put(rel: string, bytes: number, hour: number): string {
  const p = path.join(home, rel);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, Buffer.alloc(bytes, 1));
  fs.utimesSync(p, T0 + hour * 3600, T0 + hour * 3600);
  return p;
}

const allocated = (p: string): number => fs.lstatSync(p).blocks * 512;

/** A go-build cache of `n` 16 KiB units, unit i last used at hour i (so unit 0 is the oldest),
 *  plus Go's own bookkeeping files and a fixture symlink to a file OUTSIDE the cache. */
function seedGoBuild(n: number): string[] {
  const rels: string[] = [];
  for (let i = 0; i < n; i++) {
    const dir = (i % 4).toString(16).padStart(2, "0");
    const rel = path.join(GB, dir, `${i.toString(16).padStart(8, "0")}-${i % 2 === 0 ? "a" : "d"}`);
    put(rel, 16 * KIB, i);
    rels.push(rel);
  }
  put(path.join(GB, "README"), 64, -100);
  put(path.join(GB, "trim.txt"), 16, -100);
  put(path.join(GB, "testexpire.txt"), 16, -100);
  fs.writeFileSync(path.join(outside, "victim"), "must survive");
  fs.symlinkSync(path.join(outside, "victim"), path.join(home, GB, "00", "ffffffff-a"));
  fs.symlinkSync(outside, path.join(home, GB, "ee"));
  return rels;
}

async function cacheBytes(): Promise<number> {
  return (await measureRunCaches(home)).cacheBytes;
}

/** An npm `_cacache` entry: a bucket file whose one line references a content file, both last
 *  touched at `hour`. Returns the bucket and content rels (under HOME). */
function seedNpmEntry(key: string, bytes: number, hour: number): { bucket: string; content: string } {
  const body = Buffer.from(`${key}-body`);
  const digest = createHash("sha512").update(body).digest();
  const hex = digest.toString("hex");
  const integrity = `sha512-${digest.toString("base64")}`;
  const content = path.join(".npm", "_cacache", "content-v2", "sha512", hex.slice(0, 2), hex.slice(2, 4), hex.slice(4));
  put(content, bytes, hour);
  const keyHash = createHash("sha256").update(key).digest("hex");
  const bucket = path.join(".npm", "_cacache", "index-v5", keyHash.slice(0, 2), keyHash.slice(2, 4), keyHash.slice(4));
  const entry = JSON.stringify({ key, integrity, time: hour, size: bytes, metadata: {} });
  const line = `${createHash("sha1").update(entry).digest("hex")}\t${entry}`;
  const p = path.join(home, bucket);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, `\n${line}`);
  fs.utimesSync(p, T0 + hour * 3600, T0 + hour * 3600);
  return { bucket, content };
}

/** Every integrity token referenced by the remaining buckets, as content rels under HOME. */
function referencedContent(): Set<string> {
  const refs = new Set<string>();
  const idx = path.join(home, ".npm", "_cacache", "index-v5");
  if (!fs.existsSync(idx)) return refs;
  const walk = (d: string): void => {
    for (const e of fs.readdirSync(d, { withFileTypes: true })) {
      const p = path.join(d, e.name);
      if (e.isDirectory()) walk(p);
      else
        for (const m of fs.readFileSync(p, "utf8").match(/sha512-[A-Za-z0-9+/=]+/g) ?? []) {
          const hex = Buffer.from(m.slice(7), "base64").toString("hex");
          refs.add(path.join(".npm", "_cacache", "content-v2", "sha512", hex.slice(0, 2), hex.slice(2, 4), hex.slice(4)));
        }
    }
  };
  walk(idx);
  return refs;
}

describe("trimRunCaches — go build cache (PRD #1809 D4 stage a)", () => {
  it("keeps the most recently used entries: the oldest go, the newest survive, the total ends at or under the low-water mark", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const units = seedGoBuild(24);
    const unitBytes = allocated(path.join(home, units[0]!));
    const total = await cacheBytes();
    // A cap a little under the cache, and a low-water mark half the cache: roughly half must go.
    const target: TrimTarget = { totalBytes: total, capBytes: total - unitBytes, lowWaterBytes: Math.floor(total / 2) };
    const result = await trimRunCaches(home, target, nullLogger());
    const after = await cacheBytes();
    assert.ok(after <= target.lowWaterBytes, `total ${after} must end at or under the low-water mark ${target.lowWaterBytes}`);
    const present = units.map((u) => fs.existsSync(path.join(home, u)));
    const firstKept = present.indexOf(true);
    assert.ok(firstKept > 0, "the oldest entries were evicted");
    assert.ok(present.slice(firstKept).every(Boolean), "every entry newer than the oldest survivor survived: eviction is oldest first");
    assert.ok(present[units.length - 1], "the most recently used entry survives");
    assert.strictEqual(result.goBuildUnits, firstKept);
    for (const f of ["README", "trim.txt", "testexpire.txt"]) {
      assert.ok(fs.existsSync(path.join(home, GB, f)), `${f} is never evicted`);
    }
    assert.strictEqual(result.npmBuckets + result.npmContents, 0, "go-build eviction was enough: npm untouched");
    assert.strictEqual(result.moduleCacheRemoved, false);
  });

  it("never follows a symlink inside the cache: the links are skipped and their targets survive", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedGoBuild(8);
    fs.mkdirSync(path.join(outside, "nested"));
    fs.writeFileSync(path.join(outside, "nested", "deadbeef-a"), "outside unit");
    // Evict everything evictable.
    const result = await trimRunCaches(home, { totalBytes: 10 * 1024 * KIB, capBytes: 1, lowWaterBytes: 0 }, nullLogger());
    assert.strictEqual(result.goBuildUnits, 8, "every real unit went");
    assert.strictEqual(fs.readFileSync(path.join(outside, "victim"), "utf8"), "must survive");
    assert.ok(fs.existsSync(path.join(outside, "nested", "deadbeef-a")), "a symlinked top-level dir is never descended");
    assert.ok(fs.lstatSync(path.join(home, GB, "00", "ffffffff-a")).isSymbolicLink(), "the symlink itself is skipped, not a unit");
  });

  it("evicts a Go executable-cache directory entry as one unit, with its whole tree", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    put(path.join(GB, "0b", "exe0000", "bin", "tool"), 32 * KIB, 0);
    fs.utimesSync(path.join(home, GB, "0b", "exe0000"), T0, T0);
    put(path.join(GB, "0b", "11111111-a"), 16 * KIB, 5);
    const { units } = await listGoBuildUnits(home, Number.MAX_SAFE_INTEGER);
    const dir = units.find((u) => u.rel === "0b/exe0000");
    assert.ok(dir && dir.type === "d" && dir.bytes >= 32 * KIB, "the directory is one unit carrying its tree's bytes");
    const ev = await evictCacheUnits(home, ".cache/go-build", [dir!]);
    assert.ok(ev.removed.has("0b/exe0000"));
    assert.strictEqual(fs.existsSync(path.join(home, GB, "0b", "exe0000")), false);
    assert.ok(fs.existsSync(path.join(home, GB, "0b", "11111111-a")));
  });

  it("keeps an entry whose mtime changed between the listing and the eviction (it was used meanwhile)", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const rels = seedGoBuild(6);
    const { units } = await listGoBuildUnits(home, Number.MAX_SAFE_INTEGER);
    assert.strictEqual(units.length, 6);
    // Go marks an entry used by refreshing its mtime.
    const used = path.join(home, rels[1]!);
    fs.utimesSync(used, T0 + 99 * 3600, T0 + 99 * 3600);
    const ev = await evictCacheUnits(home, ".cache/go-build", units);
    assert.ok(fs.existsSync(used), "the entry used since the listing is kept");
    assert.strictEqual(ev.kept, 1);
    assert.strictEqual(ev.removed.size, 5);
    for (const [i, r] of rels.entries()) if (i !== 1) assert.strictEqual(fs.existsSync(path.join(home, r)), false);
  });

  it("an entry swapped for a symlink after the listing is kept, and the link is not followed", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const rels = seedGoBuild(2);
    const { units } = await listGoBuildUnits(home, Number.MAX_SAFE_INTEGER);
    const p = path.join(home, rels[0]!);
    fs.rmSync(p);
    fs.symlinkSync(path.join(outside, "victim"), p);
    const ev = await evictCacheUnits(home, ".cache/go-build", units);
    assert.ok(!ev.removed.has(units.find((u) => p.endsWith(u.rel))!.rel));
    assert.strictEqual(fs.readFileSync(path.join(outside, "victim"), "utf8"), "must survive");
  });

  it("refuses a symlinked cache root: nothing is listed", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    fs.mkdirSync(path.join(outside, "go-build", "00"), { recursive: true });
    fs.writeFileSync(path.join(outside, "go-build", "00", "aaaaaaaa-a"), "x");
    fs.mkdirSync(path.join(home, ".cache"));
    fs.symlinkSync(path.join(outside, "go-build"), path.join(home, GB));
    const { units } = await listGoBuildUnits(home, Number.MAX_SAFE_INTEGER);
    assert.deepStrictEqual(units, []);
    const ev = await evictCacheUnits(home, ".cache/go-build", [
      { rel: "00/aaaaaaaa-a", mtimeNs: "0", bytes: 1, type: "f", seenBy: [0] },
    ]);
    assert.strictEqual(ev.removed.size, 0);
    assert.ok(fs.existsSync(path.join(outside, "go-build", "00", "aaaaaaaa-a")));
  });
});

describe("trimRunCaches — npm cache and module cache (PRD #1809 D4 stages b, c)", () => {
  it("touches npm only when go-build eviction is not enough, oldest index entries first, and leaves no remaining index line pointing at deleted content", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedGoBuild(2);
    const entries = [0, 1, 2, 3, 4, 5].map((h) => seedNpmEntry(`pkg-${h}`, 64 * KIB, h));
    // An orphan: content no bucket references.
    const orphan = seedNpmEntry("orphan", 16 * KIB, 9);
    fs.rmSync(path.join(home, orphan.bucket));
    // A content file two buckets share: it must survive while the newer one remains.
    const shared = entries[5]!;
    const sharedLine = fs.readFileSync(path.join(home, shared.bucket), "utf8");
    const olderSharer = path.join(".npm", "_cacache", "index-v5", "aa", "bb", "older-sharer");
    fs.mkdirSync(path.dirname(path.join(home, olderSharer)), { recursive: true });
    fs.writeFileSync(path.join(home, olderSharer), sharedLine);
    fs.utimesSync(path.join(home, olderSharer), T0 - 3600, T0 - 3600);

    const total = await cacheBytes();
    const target: TrimTarget = { totalBytes: total, capBytes: total, lowWaterBytes: Math.floor(total / 2) };
    const result = await trimRunCaches(home, target, nullLogger());
    assert.strictEqual(result.goBuildUnits, 2, "go-build went first, all of it");
    assert.ok(result.npmBuckets > 0, "npm was touched because go-build alone was not enough");
    assert.ok((await cacheBytes()) <= target.lowWaterBytes);
    assert.strictEqual(fs.existsSync(path.join(home, orphan.content)), false, "unreferenced content goes");
    // Oldest first: the older sharer and pkg-0 went before pkg-5.
    assert.strictEqual(fs.existsSync(path.join(home, olderSharer)), false);
    assert.strictEqual(fs.existsSync(path.join(home, entries[0]!.bucket)), false);
    assert.ok(fs.existsSync(path.join(home, shared.bucket)), "the newest entry survives");
    // The invariant: every content a remaining index line references is still there.
    for (const ref of referencedContent()) {
      assert.ok(fs.existsSync(path.join(home, ref)), `remaining index references deleted content ${ref}`);
    }
    // And the index entries that went took the content only they referenced.
    assert.strictEqual(fs.existsSync(path.join(home, entries[0]!.content)), false);
    assert.strictEqual(result.moduleCacheRemoved, false, "under the cap after (a)+(b): the module cache stays");
  });

  it("does not touch npm when the go build cache alone reaches the low-water mark", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedGoBuild(40);
    const npm = seedNpmEntry("pkg", 8 * KIB, 0);
    const total = await cacheBytes();
    const result = await trimRunCaches(home, { totalBytes: total, capBytes: total, lowWaterBytes: Math.floor(total * 0.7) }, nullLogger());
    assert.ok(result.goBuildUnits > 0);
    assert.strictEqual(result.npmBuckets + result.npmContents, 0);
    assert.ok(fs.existsSync(path.join(home, npm.bucket)) && fs.existsSync(path.join(home, npm.content)));
  });

  it("skips npm content eviction when the index could not be read completely", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const e = seedNpmEntry("pkg", 64 * KIB, 0);
    // A symlinked index dir: not a layout the trim trusts, so no content can be judged unreferenced.
    fs.mkdirSync(path.join(outside, "idx"));
    fs.symlinkSync(path.join(outside, "idx"), path.join(home, ".npm", "_cacache", "index-v5", "zz"));
    const listing = await listNpmCache(home);
    assert.strictEqual(listing.complete, false);
    const result = await trimRunCaches(home, { totalBytes: 10 * 1024 * KIB, capBytes: 10 * 1024 * KIB, lowWaterBytes: 0 }, nullLogger());
    assert.strictEqual(result.npmBuckets + result.npmContents, 0);
    assert.ok(result.npmSkipped);
    assert.ok(fs.existsSync(path.join(home, e.content)) && fs.existsSync(path.join(home, e.bucket)));
  });

  it("removes go/pkg/mod whole only when still over the CAP after both, and keeps go/bin", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedGoBuild(2);
    const mod = path.join("go", "pkg", "mod", "example.com", "m@v1.0.0");
    put(path.join(mod, "m.go"), 512 * KIB, 0);
    fs.chmodSync(path.join(home, mod), 0o555);
    put(path.join("go", "bin", "gopls"), 4 * KIB, 0);
    const total = await cacheBytes();
    // Over the cap even with every go-build unit gone: the module cache is what holds it.
    const over = await trimRunCaches(home, { totalBytes: total, capBytes: 128 * KIB, lowWaterBytes: 64 * KIB }, nullLogger());
    assert.strictEqual(over.moduleCacheRemoved, true);
    assert.strictEqual(fs.existsSync(path.join(home, "go", "pkg", "mod")), false);
    assert.ok(fs.existsSync(path.join(home, "go", "bin", "gopls")), "go/bin survives");
  });

  it("keeps go/pkg/mod when (a) and (b) bring the run under the cap", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    seedGoBuild(40);
    put(path.join("go", "pkg", "mod", "example.com", "m@v1.0.0", "m.go"), 16 * KIB, 0);
    const total = await cacheBytes();
    const result = await trimRunCaches(home, { totalBytes: total, capBytes: Math.floor(total * 0.8), lowWaterBytes: Math.floor(total * 0.5) }, nullLogger());
    assert.ok(result.goBuildUnits > 0);
    assert.strictEqual(result.moduleCacheRemoved, false);
    assert.ok(fs.existsSync(path.join(home, "go", "pkg", "mod", "example.com", "m@v1.0.0", "m.go")));
  });
});
