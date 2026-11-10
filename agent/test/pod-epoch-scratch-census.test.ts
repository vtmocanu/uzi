// Issue #4043 M1: the read-only scratch census helper (rmtree.ts SCRATCH_CENSUS_SCRIPT) run for real
// against small temp trees, and the protected-path collector of GitCache against a real bare.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import * as fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { test } from "node:test";
import { GitCache } from "../src/git.js";
import { classifyScratchCensus, parseScratchCensus, postCensusOf } from "../src/pod-epoch-scratch.js";
import { SCRATCH_CENSUS_SCRIPT, censusScratchRoot, runAgentHelper } from "../src/rmtree.js";
import { runnerCommand } from "../src/runner-uid.js";

function tmp(t: { after(fn: () => void): void }): string {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "scratch-census-")));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}
const idOf = (p: string) => { const st = fs.statSync(p, { bigint: true }); return `${st.dev}:${st.ino}`; };
const write = (file: string, bytes: number) => { fs.mkdirSync(path.dirname(file), { recursive: true }); fs.writeFileSync(file, Buffer.alloc(bytes, 1)); };
async function census(root: string, expect = idOf(root), leaf?: string) {
  const result = await censusScratchRoot(root, expect, leaf, 60_000);
  return { result, parsed: parseScratchCensus(result, expect) };
}
function entries(parsed: ReturnType<typeof parseScratchCensus>) {
  assert.ok(!("refused" in parsed), JSON.stringify(parsed));
  return new Map(parsed.entries.map(e => [e.name, e]));
}
const MIB = 1024 * 1024;

test("census reports per-leaf allocated bytes, completeness and newest ctime", async t => {
  const root = tmp(t);
  write(path.join(root, "big", "a", "file"), MIB);
  write(path.join(root, "small", "file"), 10);
  fs.mkdirSync(path.join(root, "empty"));
  const { result, parsed } = await census(root);
  assert.equal(result.code, 0);
  const e = entries(parsed);
  assert.deepEqual([...e.keys()].sort(), ["big", "empty", "small"]);
  const big = e.get("big")!;
  assert.equal(big.type, "dir");
  assert.ok(big.bytes >= MIB && big.bytes < MIB + 64 * 1024, String(big.bytes));
  assert.equal(big.sharedBytes, 0);
  assert.equal(big.incomplete, false);
  assert.equal(big.fsDiffers, false);
  assert.equal(big.foreignUid, false);
  assert.equal(big.uid, process.getuid!());
  assert.ok(Math.abs(big.maxCtimeMs - fs.statSync(path.join(root, "big", "a", "file")).ctimeMs) < 2000);
  assert.equal(big.devIno, idOf(path.join(root, "big")));
  assert.ok(e.get("empty")!.bytes < 64 * 1024);
});

test("a hard-linked file counts as shared bytes, not bytes", async t => {
  const root = tmp(t);
  write(path.join(root, "twin", "one"), MIB);
  fs.linkSync(path.join(root, "twin", "one"), path.join(root, "twin", "two"));
  write(path.join(root, "solo", "one"), MIB);
  const e = entries((await census(root)).parsed);
  const twin = e.get("twin")!;
  assert.ok(twin.sharedBytes >= 2 * MIB, String(twin.sharedBytes));
  assert.ok(twin.bytes < 64 * 1024, String(twin.bytes));
  assert.equal(e.get("solo")!.sharedBytes, 0);
});

test("symlinks and files at the top level are other, never followed", async t => {
  const root = tmp(t);
  write(path.join(root, "target", "payload"), MIB);
  fs.symlinkSync(path.join(root, "target"), path.join(root, "link"));
  fs.writeFileSync(path.join(root, "plain"), "x");
  fs.mkdirSync(path.join(root, ".dot"));
  fs.symlinkSync(root, path.join(root, "target", "loop"));
  const e = entries((await census(root)).parsed);
  assert.equal(e.get("link")!.type, "other");
  assert.ok(e.get("link")!.bytes < 64 * 1024, "the link's own size, not its target's");
  assert.equal(e.get("plain")!.type, "other");
  assert.equal(e.get(".dot")!.type, "dir");
  assert.equal(e.get("target")!.incomplete, false, "a symlink inside a leaf is counted, not followed");
});

test("a single-leaf census reports one entry or absent", async t => {
  const root = tmp(t);
  write(path.join(root, "here", "f"), 10);
  write(path.join(root, "other", "f"), 10);
  const one = entries((await census(root, undefined, "here")).parsed);
  assert.deepEqual([...one.keys()], ["here"]);
  const gone = entries((await census(root, undefined, "gone")).parsed);
  assert.equal(gone.get("gone")!.type, "absent");
});

test("a root identity mismatch prints the identity first and exits 3", async t => {
  const root = tmp(t);
  write(path.join(root, "leaf", "f"), 10);
  const { result, parsed } = await census(root, "1:1");
  assert.equal(result.code, 3);
  const first = JSON.parse(result.stdout.split("\n")[0]!) as { root: string };
  assert.equal(first.root, idOf(root));
  assert.equal(result.stdout.split("\n").length, 2, "nothing is listed after the mismatch");
  assert.deepEqual(parsed, { refused: "census_identity" });
});

test("a missing or symlinked root fails closed", async t => {
  const dir = tmp(t);
  fs.mkdirSync(path.join(dir, "real"));
  fs.symlinkSync(path.join(dir, "real"), path.join(dir, "link"));
  const expect = idOf(path.join(dir, "real"));
  assert.equal((await censusScratchRoot(path.join(dir, "absent"), expect, undefined, 60_000)).code, 2);
  assert.equal((await censusScratchRoot(path.join(dir, "link"), expect, undefined, 60_000)).code, 3);
  assert.equal((await censusScratchRoot("relative", expect, undefined, 60_000)).code, 6);
  assert.equal((await censusScratchRoot(path.join(dir, "real"), expect, "a/b", 60_000)).code, 6);
  assert.equal((await censusScratchRoot(path.join(dir, "real"), expect, "..", 60_000)).code, 6);
});

async function raw(root: string, over: { leaf?: string; maxRoot?: number; maxLeaf?: number; maxTotal?: number; maxDepth?: number; budgetMs?: number }) {
  return runAgentHelper(runnerCommand, SCRATCH_CENSUS_SCRIPT, [root, idOf(root), over.leaf ?? "-", String(over.maxRoot ?? 4096),
    String(over.maxLeaf ?? 200_000), String(over.maxTotal ?? 2_000_000), String(over.maxDepth ?? 256), String(over.budgetMs ?? 60_000)], 60_000);
}
const rows = (stdout: string) => stdout.trim().split("\n").map(l => JSON.parse(l) as Record<string, unknown>);

test("depth, per-leaf, total and root-entry caps mark the leaf incomplete or the listing overflowed", async t => {
  const root = tmp(t);
  write(path.join(root, "deep", "a", "b", "c", "d", "f"), 10);
  write(path.join(root, "wide", "1"), 1); write(path.join(root, "wide", "2"), 1); write(path.join(root, "wide", "3"), 1);
  fs.mkdirSync(path.join(root, "third"));
  const byName = (stdout: string) => new Map(rows(stdout).filter(r => typeof r.name === "string").map(r => [r.name as string, r]));
  assert.equal(byName((await raw(root, { maxDepth: 3 })).stdout).get("deep")!.inc, true, "depth 4 is past the cap of 3");
  assert.equal(byName((await raw(root, { maxDepth: 5 })).stdout).get("deep")!.inc, false);
  assert.equal(byName((await raw(root, { maxLeaf: 2 })).stdout).get("wide")!.inc, true);
  const total = byName((await raw(root, { maxTotal: 3 })).stdout);
  assert.ok([...total.values()].some(r => r.inc === true), "the total cap marks leaves incomplete");
  const limited = rows((await raw(root, { maxRoot: 2 })).stdout);
  assert.deepEqual(limited.at(-1), { end: true, entries: 2, overflow: true });
  assert.deepEqual(rows((await raw(root, {})).stdout).at(-1), { end: true, entries: 3, overflow: false });
  assert.equal(byName((await raw(root, { budgetMs: 0 })).stdout).get("deep")!.inc, true, "an exhausted time budget is a lower bound");
});

const bytesPath = (...parts: Array<string | Buffer>) => Buffer.concat(parts.map(p => (typeof p === "string" ? Buffer.from(p) : p)));
const sleepMs = (ms: number) => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);

test("names that collide under UTF-8 replacement are all examined (U+FFFD file vs 0xFF directory)", async t => {
  const root = tmp(t);
  const leaf = path.join(root, "leaf");
  fs.mkdirSync(leaf);
  fs.writeFileSync(bytesPath(leaf, "/", Buffer.from([0xef, 0xbf, 0xbd])), "x");
  fs.mkdirSync(bytesPath(leaf, "/", Buffer.from([0xff])));
  sleepMs(40);
  const nested = bytesPath(leaf, "/", Buffer.from([0xff]), "/late");
  fs.writeFileSync(nested, Buffer.alloc(MIB, 1));
  const e = entries((await census(root)).parsed).get("leaf")!;
  assert.equal(e.incomplete, false);
  assert.ok(e.maxCtimeMs >= fs.statSync(nested).ctimeMs - 1, "the write inside the 0xFF directory raised the leaf ctime");
  assert.ok(e.bytes >= MIB, "and its bytes were counted: " + e.bytes);
});

test("top-level names that are not valid UTF-8 are reported distinctly and refused by name", async t => {
  const root = tmp(t);
  fs.mkdirSync(bytesPath(root, "/", Buffer.from([0xfe])));
  fs.mkdirSync(bytesPath(root, "/", Buffer.from([0xfd])));
  fs.mkdirSync(path.join(root, "good"));
  const { result, parsed } = await census(root);
  assert.equal(result.code, 0);
  assert.ok(!("refused" in parsed), JSON.stringify(parsed));
  assert.equal(parsed.entries.length, 3);
  const classified = classifyScratchCensus({ census: parsed, t0Ms: Infinity, mountedLeaves: new Set(), boundLeaves: new Set(),
    protectedLeaves: new Set(), heldLeaves: new Set() });
  assert.equal(classified.kept.name_refused, 2);
  assert.deepEqual(classified.admitted.map(l => l.name), ["good"]);
});

test("a leading U+FEFF is kept: the name is never reported as another name", async t => {
  const root = tmp(t);
  fs.mkdirSync(path.join(root, "﻿abc"));
  fs.mkdirSync(path.join(root, "abc"));
  const { result, parsed } = await census(root);
  assert.equal(result.code, 0);
  assert.ok(!("refused" in parsed), JSON.stringify(parsed));
  const classified = classifyScratchCensus({ census: parsed, t0Ms: Infinity, mountedLeaves: new Set(), boundLeaves: new Set(),
    protectedLeaves: new Set(), heldLeaves: new Set() });
  assert.equal(classified.kept.name_refused, 1);
  assert.deepEqual(classified.admitted.map(l => l.name), ["abc"]);
});

test("a single-leaf lstat error other than ENOENT is unreadable, not another inode", async t => {
  const root = tmp(t);
  const { parsed } = await census(root, undefined, "a".repeat(300));
  const e = entries(parsed).get("a".repeat(300))!;
  assert.equal(e.type, "unreadable");
  assert.equal(e.incomplete, true);
  assert.deepEqual(postCensusOf(e, "1:1"), { state: "unreadable" });
});

test("the census never writes: the tree is byte-for-byte unchanged", async t => {
  const root = tmp(t);
  write(path.join(root, "leaf", "f"), 100);
  const snapshot = () => execFileSync("ls", ["-lAR", "--time-style=full-iso", root]).toString();
  const before = snapshot();
  await census(root);
  assert.equal(snapshot(), before);
});

// ---- GitCache.collectEpochScratchProtectedPaths -----------------------------------------------

const noop = () => undefined;
const log = { debug: noop, info: noop, warn: noop, error: noop, child: () => log } as never;
const window = () => ({ signal: new AbortController().signal, deadline: Date.now() + 60_000 });

function cacheWithBare(t: { after(fn: () => void): void }, config: Record<string, string> = {}) {
  const data = tmp(t);
  const bare = path.join(data, "repos", "r1.git");
  fs.mkdirSync(path.dirname(bare), { recursive: true });
  execFileSync("git", ["init", "--bare", "--quiet", bare], { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
  for (const [key, value] of Object.entries(config))
    execFileSync("git", ["-C", bare, "config", "--local", key, value], { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
  return { data, cache: new GitCache(data, log) };
}

test("collectEpochScratchProtectedPaths gathers bare-record and store paths", async t => {
  const { data, cache } = cacheWithBare(t, {
    "uzi-recovery.agent-issue-1.clone": "/data/runner/slug/issue-1",
    "uzi-retained.run1.journal": JSON.stringify({ runId: "run1", clonePath: "/data/runner/slug/issue-2", nested: [{ p: "/scratch/a" }] }),
    "uzi-attemptmode.key.enabled": "maybe",
    "user.name": "ignored",
  });
  fs.mkdirSync(path.join(data, "recovery", "run1"), { recursive: true });
  fs.writeFileSync(path.join(data, "recovery", "run1", "rec.json"), JSON.stringify({ bundlePath: "/data/recovery/run1/x.bundle" }));
  const result = await cache.collectEpochScratchProtectedPaths(window());
  assert.deepEqual(result, { state: "read", paths: ["/data/recovery/run1/x.bundle", "/data/runner/slug/issue-1", "/data/runner/slug/issue-2", "/scratch/a"] });
});

test("collectEpochScratchProtectedPaths is empty for a fresh volume and unknown for an uninterpretable record", async t => {
  const fresh = new GitCache(tmp(t), log);
  assert.deepEqual(await fresh.collectEpochScratchProtectedPaths(window()), { state: "read", paths: [] });
  const { cache } = cacheWithBare(t, { "uzi-attempts.agent-issue-1.entry": "{truncated" });
  assert.deepEqual(await cache.collectEpochScratchProtectedPaths(window()), { state: "unknown", reason: "bare_value_unparseable" });
});

test("collectEpochScratchProtectedPaths surfaces a malformed store, an abort and a spent deadline", async t => {
  const { data, cache } = cacheWithBare(t);
  fs.mkdirSync(path.join(data, "recovery-settlement"), { recursive: true });
  fs.writeFileSync(path.join(data, "recovery-settlement", "run1"), '{"half":');
  assert.deepEqual(await cache.collectEpochScratchProtectedPaths(window()), { state: "malformed", reason: "store_file_malformed" });
  const aborted = new AbortController();
  aborted.abort();
  assert.deepEqual(await cache.collectEpochScratchProtectedPaths({ signal: aborted.signal, deadline: Date.now() + 60_000 }),
    { state: "unknown", reason: "protected_aborted" });
  assert.deepEqual(await cache.collectEpochScratchProtectedPaths({ signal: new AbortController().signal, deadline: Date.now() - 1 }),
    { state: "unknown", reason: "protected_deadline" });
});
