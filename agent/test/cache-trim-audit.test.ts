import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type CommandWrapper, evictCacheUnits, listGoBuildUnits, listNpmCache } from "../src/rmtree.js";
import { trimRunCaches } from "../src/run-caches.js";
import { nullLogger } from "./helpers.js";

// PRD #1809 D4 audit (H1, M1, M3): the trim's LIST helper against planted names and swapped
// buckets, and the worker's parsing of what a helper prints. Fixture trees only: this host runs
// single-uid, so every helper pass runs as the test's own uid.

const HAS_PROC_FD = fs.existsSync("/proc/self/fd");
const skipReason = "no /proc/self/fd on this host: the pinned helpers refuse here by design";

let root: string;
let home: string;

beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-audit-"));
  home = path.join(root, "home");
  fs.mkdirSync(home);
});

afterEach(() => {
  fs.rmSync(root, { recursive: true, force: true });
});

const T0 = Date.parse("2026-09-01T00:00:00Z") / 1000;
const GB = path.join(".cache", "go-build");
const NPM = path.join(".npm", "_cacache");

function put(rel: string, bytes: number, hour: number): string {
  const p = path.join(home, rel);
  fs.mkdirSync(path.dirname(p), { recursive: true });
  fs.writeFileSync(p, Buffer.alloc(bytes, 1));
  fs.utimesSync(p, T0 + hour * 3600, T0 + hour * 3600);
  return p;
}

const mtimeNs = (p: string): string => String(fs.lstatSync(p, { bigint: true }).mtimeNs);

/** A wrapper that runs `script` instead of the helper the caller asked for. */
const fakeHelper =
  (script: string): CommandWrapper =>
  (command) => ({ command, args: ["-e", script] });

async function until(p: string, ms = 10_000): Promise<void> {
  const end = Date.now() + ms;
  while (!fs.existsSync(p)) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${p}`);
    await new Promise((r) => setTimeout(r, 10));
  }
}

describe("trim LIST helper: names with control characters never forge protocol lines (M1)", () => {
  it("a go-build directory named to forge a README unit: README is never listed, and a trim leaves it", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const readme = put(path.join(GB, "README"), 64, -100);
    put(path.join(GB, "0a", "0000000a-a"), 4096, 5);
    // A directory entry is a unit whatever its name, so its name carries a forged second line.
    const forged = `x\nU\t${mtimeNs(readme)}\tf\t64\tREADME`;
    fs.mkdirSync(path.join(home, GB, "0a", forged));
    fs.utimesSync(path.join(home, GB, "0a", forged), T0 - 200 * 3600, T0 - 200 * 3600);

    const { units, truncated } = await listGoBuildUnits(home, 1 << 30);
    assert.ok(!units.some((u) => u.rel === "README"), `README must never be a unit: ${JSON.stringify(units.map((u) => u.rel))}`);
    assert.strictEqual(truncated, true, "a pass that skipped a control-character name reports itself incomplete");
    const control = (x: string): boolean => [...x].some((ch) => ch.charCodeAt(0) < 0x20);
    assert.ok(!units.some((u) => control(u.rel)), "no unit carries a control character");

    await trimRunCaches(home, { totalBytes: 1 << 30, capBytes: 1 << 20, lowWaterBytes: 0 }, nullLogger(), {
      listGoBuild: listGoBuildUnits,
      listNpm: listNpmCache,
      evict: evictCacheUnits,
      removeSubtree: async () => "absent",
    });
    assert.ok(fs.existsSync(readme), "Go's README survives the trim");
    assert.ok(fs.existsSync(path.join(home, GB, "0a", forged)), "the planted entry is not a unit and is left alone");
  });

  it("a go-build listing with no control-character name is not truncated (the flag does not over-reach)", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    put(path.join(GB, "README"), 64, -100);
    put(path.join(GB, "0a", "0000000a-a"), 4096, 5);
    fs.mkdirSync(path.join(home, GB, "0b", "0000000b-d"), { recursive: true });
    const { units, truncated } = await listGoBuildUnits(home, 1 << 30);
    assert.strictEqual(truncated, false);
    assert.deepStrictEqual(units.map((u) => u.rel).sort(), ["0a/0000000a-a", "0b/0000000b-d"]);
  });

  it("a forged H line inside a bucket name cannot make a truncated npm listing complete", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    // The index is walked first and is small; the content walk then runs out of the entry budget,
    // so the real (last) summary says the listing is truncated.
    put(path.join(NPM, "index-v5", "00", `b\nH\t${JSON.stringify({ truncated: false })}`), 64, 0);
    for (let i = 0; i < 20; i++) put(path.join(NPM, "content-v2", "sha512", "aa", "bb", `c${i}`), 64, i);
    const listing = await listNpmCache(home, { maxEntries: 8 });
    assert.strictEqual(listing.complete, false);
  });

  it("a tab in a bucket name marks the listing incomplete instead of dropping the bucket silently", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    put(path.join(NPM, "index-v5", "00", "good"), 64, 0);
    put(path.join(NPM, "index-v5", "00", "with\ttab"), 64, 0);
    const listing = await listNpmCache(home);
    assert.strictEqual(listing.complete, false, "an unprinted bucket leaves the index unresolved");
    assert.deepStrictEqual(
      listing.buckets.map((b) => b.rel),
      ["index-v5/00/good"],
    );
  });

  it("a clean index is still complete (the control check does not over-reach)", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    put(path.join(NPM, "index-v5", "00", "11", "good"), 64, 0);
    put(path.join(NPM, "content-v2", "sha512", "aa", "bb", "cc"), 64, 0);
    const listing = await listNpmCache(home);
    assert.strictEqual(listing.complete, true);
    assert.strictEqual(listing.buckets.length, 1);
    assert.strictEqual(listing.contents.length, 1);
  });
});

describe("trim LIST helper: a bucket swapped after its lstat (H1, M3)", () => {
  async function swapped(swap: (p: string) => void): Promise<{ complete: boolean; buckets: string[]; ms: number }> {
    const bucket = put(path.join(NPM, "index-v5", "00", "11", "bucket"), 64, 0);
    const sync = path.join(root, "sync");
    const started = Date.now();
    const listing = listNpmCache(home, { sync, deadline: Date.now() + 30_000 });
    await until(`${sync}.bucket`);
    swap(bucket);
    fs.writeFileSync(`${sync}.go`, "");
    const r = await listing;
    return { complete: r.complete, buckets: r.buckets.map((b) => b.rel), ms: Date.now() - started };
  }

  it("a FIFO swapped in as a bucket does not block the listing; the bucket is unresolved", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const r = await swapped((p) => {
      fs.unlinkSync(p);
      execFileSync("mkfifo", [p]);
    });
    assert.strictEqual(r.complete, false);
    assert.deepStrictEqual(r.buckets, []);
    assert.ok(r.ms < 15_000, `the listing finished promptly (${r.ms} ms)`);
  });

  it("a sparse 256 MiB file swapped in as a bucket (lstat said 64 bytes) is never read; the bucket is unresolved", async (t) => {
    if (!HAS_PROC_FD) return t.skip(skipReason);
    const r = await swapped((p) => {
      fs.unlinkSync(p);
      const fd = fs.openSync(p, "w");
      fs.ftruncateSync(fd, 256 * 1024 ** 2);
      fs.closeSync(fd);
    });
    assert.strictEqual(r.complete, false);
    assert.deepStrictEqual(r.buckets, []);
  });
});

describe("worker parsing of a helper's output (M1)", () => {
  const H = `H\\t${JSON.stringify({ truncated: false }).replace(/"/g, '\\"')}`;
  const print = (text: string): string => `process.stdout.write("${text}");`;

  it("a duplicated summary fails the pass: nothing is merged and the listing is not complete", async () => {
    const listing = await listNpmCache(home, {
      wrappers: [fakeHelper(print(`B\\t1\\t4096\\tindex-v5/00/a\\t\\n${H}\\n${H}\\n`))],
    });
    assert.strictEqual(listing.complete, false);
    assert.deepStrictEqual(listing.buckets, []);
  });

  it("a summary that is not the last line fails the pass", async () => {
    const listing = await listNpmCache(home, {
      wrappers: [fakeHelper(print(`${H}\\nB\\t1\\t4096\\tindex-v5/00/a\\t\\n`))],
    });
    assert.strictEqual(listing.complete, false);
    assert.deepStrictEqual(listing.buckets, []);
  });

  it("a relpath of the wrong shape fails the pass: npm and go", async () => {
    for (const bad of ["index-v5/../x", "content-v2/sha512/aa", "elsewhere/x", "index-v5/a/b/c/d"]) {
      const listing = await listNpmCache(home, {
        wrappers: [fakeHelper(print(`B\\t1\\t4096\\t${bad}\\t\\n${H}\\n`))],
      });
      assert.strictEqual(listing.complete, false, bad);
      assert.deepStrictEqual(listing.buckets, [], bad);
    }
    for (const bad of ["README", "a/b/c", "../x", "a/.."]) {
      const { units, truncated } = await listGoBuildUnits(home, 1, {
        wrappers: [fakeHelper(print(`U\\t1\\tf\\t4096\\t${bad}\\n${H}\\n`))],
      });
      assert.deepStrictEqual(units, [], bad);
      assert.strictEqual(truncated, true, bad);
    }
  });

  it("a bucket listed twice in one pass fails the pass", async () => {
    const line = "B\\t1\\t4096\\tindex-v5/00/a\\t\\n";
    const listing = await listNpmCache(home, { wrappers: [fakeHelper(print(`${line}${line}${H}\\n`))] });
    assert.strictEqual(listing.complete, false);
  });

  it("an eviction whose summary is malformed still reports what it removed, and never throws", async () => {
    const unit = { rel: "0a/0000000a-a", mtimeNs: "1", bytes: 4096, type: "f" as const, seenBy: [0] };
    const r = await evictCacheUnits(home, ".cache/go-build", [unit], {
      wrappers: [fakeHelper(print("R\\t0a/0000000a-a\\nR\\t0a/not-given-a\\nH\\tnot json\\n"))],
    });
    assert.deepStrictEqual([...r.removed], ["0a/0000000a-a"], "only a unit this pass was given counts");
  });
});
