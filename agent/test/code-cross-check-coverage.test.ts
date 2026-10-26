import assert from "node:assert/strict";
import test from "node:test";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { validateCodeVerdict } from "../src/codex/cross-check.js";
import type { GitCache } from "../src/git.js";
import { CODE_COVERAGE_LIMITS, collectCodeInventory, decodeCodeCoverage, normalizeCodeCoverage, type CodeInventory } from "../src/code-cross-check-coverage.js";

const exec = promisify(execFile);
test("coverage wire fixture is shared with the API normalizer", async () => {
  const fixture = JSON.parse(await fs.readFile(new URL("../../fixtures/code-cross-check/coverage-v1.json", import.meta.url), "utf8"));
  const decoded = decodeCodeCoverage(fixture)!;
  const got = normalizeCodeCoverage(inventory(decoded.entries.map(e => e.path)), decoded.entries,
    new Set(decoded.entries.filter(e => e.recorded_read).map(e => e.path)), decoded.activity_complete);
  assert.deepEqual(got, fixture);
});

const base = "a".repeat(40), head = "b".repeat(40);
const inventory = (paths: string[]): CodeInventory => ({ paths, inventory_incomplete: false, omitted_paths: 0, omitted_count_exact: true, reasons: [] });
const reader = (raw: Buffer, truncated = false): Pick<GitCache, "readBare"> => ({
  async readBare(_bare, _args, opts) {
    assert.equal(opts.includeBytes, true);
    assert.equal(opts.maxBytes, 1024 * 1024);
    return { bytes: raw, text: raw.toString("utf8"), truncated };
  },
});

test("real git inventory retains deleted, renamed, binary and type-changed paths", async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "coverage-git-"));
  const git = async (...args: string[]): Promise<string> => (await exec("git", ["-C", root, ...args],
    { env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } })).stdout.trim();
  try {
    await git("init");
    await git("config", "user.name", "coverage fixture");
    await git("config", "user.email", "fixture@example.invalid");
    for (const file of ["modified", "deleted", "old", "typed"]) await fs.writeFile(path.join(root, file), file);
    await git("add", "."); await git("commit", "-m", "base");
    const b = await git("rev-parse", "HEAD");
    await fs.writeFile(path.join(root, "modified"), "changed");
    await fs.unlink(path.join(root, "deleted"));
    await fs.rename(path.join(root, "old"), path.join(root, "renamed"));
    await fs.unlink(path.join(root, "typed")); await fs.symlink("modified", path.join(root, "typed"));
    await fs.writeFile(path.join(root, "binary"), Buffer.from([0, 1, 255]));
    await fs.writeFile(path.join(root, "added"), "new");
    await fs.writeFile(path.join(root, "é.txt"), "unicode");
    await fs.writeFile(Buffer.concat([Buffer.from(root + "/"), Buffer.from([255])]), "non-UTF8");
    await git("add", "-A"); await git("commit", "-m", "changes");
    const h = await git("rev-parse", "HEAD");
    const real: Pick<GitCache, "readBare"> = { async readBare(bare, args, opts) {
      const result = await exec("git", ["-C", bare, ...args], { encoding: "buffer",
        env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
        maxBuffer: opts.maxBytes + 1024, signal: opts.signal });
      return { bytes: result.stdout, text: result.stdout.toString("utf8"), truncated: false };
    } };
    const got = await collectCodeInventory(real, root, b, h, { timeoutMs: 10000 });
    assert.deepEqual(new Set(got.paths), new Set(["modified", "deleted", "old", "renamed", "typed", "binary", "added", "é.txt"]));
    assert.equal(got.omitted_paths, 1);
    assert.equal(got.omitted_count_exact, true);
    assert.equal(got.inventory_incomplete, true);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

test("inventory caps preserve exact counts only for fully consumed records", async () => {
  const rows = Array.from({ length: 129 }, (_, i) => "A\0file-" + i + "\0").join("");
  const got = await collectCodeInventory(reader(Buffer.from(rows)), "", base, head, { timeoutMs: 1000 });
  assert.equal(got.paths.length, 128); assert.equal(got.omitted_paths, 1); assert.equal(got.omitted_count_exact, true);
  const partial = await collectCodeInventory(reader(Buffer.from("M\0known\0D\0unfinished"), true), "", base, head, { timeoutMs: 1000 });
  assert.deepEqual(partial.paths, ["known"]); assert.equal(partial.omitted_count_exact, false);
  const long = await collectCodeInventory(reader(Buffer.from("A\0" + "é".repeat(257) + "\0")), "", base, head, { timeoutMs: 1000 });
  assert.equal(long.omitted_paths, 1); assert.equal(long.omitted_count_exact, true);
  const records = Buffer.from(Array.from({ length: 9000 }, (_, i) => "A\0f" + i + "\0").join(""));
  const limited = await collectCodeInventory(reader(records), "", base, head, { timeoutMs: 1000 });
  assert.equal(limited.omitted_count_exact, false); assert.equal(limited.inventory_incomplete, true);
  const failed = await collectCodeInventory({ async readBare() { throw new Error("deadline"); } }, "", base, head, { timeoutMs: 1 });
  assert.equal(failed.omitted_count_exact, false); assert.deepEqual(failed.reasons, ["inventory_failed"]);
});

test("coverage accounts for every inventory path and ignores invented denominator/read flags", () => {
  const inv = inventory([...Array.from({ length: 11 }, (_, i) => "file-" + i), "deleted.ts"]);
  const judgments = inv.paths.slice(0, 9).map(p => ({ path: p, status: "examined", reason: "inspected patch", recorded_read: true }));
  const got = normalizeCodeCoverage(inv, judgments, new Set(["file-0"]), true);
  assert.equal(got.entries.filter(e => e.status === "examined").length, 9);
  assert.equal(got.entries.filter(e => e.status === "unassessed").length, 3);
  assert.equal(got.entries.at(-1)?.path, "deleted.ts");
  assert.equal(got.entries[0]?.recorded_read, true);
  assert.equal(got.entries[1]?.recorded_read, false);
  assert.deepEqual(decodeCodeCoverage(got), got);
  assert.equal(decodeCodeCoverage(undefined), null); assert.equal(decodeCodeCoverage(null), null);
});

test("invalid, unknown and duplicate judgments cannot claim examination", () => {
  const got = normalizeCodeCoverage(inventory(["a", "b", "c"]), [
    { path: "a", status: "examined", reason: "first" }, { path: "a", status: "excluded", reason: "contradiction" },
    { path: "b", status: "excluded", reason: "" }, { path: "outside", status: "examined", reason: "invented" },
    { path: "c", status: "examined", reason: "valid", recorded_read: true },
  ], new Set(), false);
  assert.deepEqual(got.entries.map(e => e.status), ["unassessed", "unassessed", "examined"]);
  assert.equal(got.unknown_judgments, 1); assert.equal(got.invalid_judgments, 2);
  assert.equal(got.entries[2]?.recorded_read, false);
  assert.equal(got.activity_complete, false);
  assert.throws(() => decodeCodeCoverage({ ...got, entries: [got.entries[0], got.entries[0]] }));
  assert.throws(() => decodeCodeCoverage({ ...got, omitted_paths: -1 }));
  assert.throws(() => decodeCodeCoverage({ ...got, entries: [{ ...got.entries[0], recorded_read: "yes" }] }));
});

test("verdict parser preserves valid findings independently of optional malformed coverage", () => {
  for (const coverage of [null, {}, false, [], "x".repeat(40000)]) {
    assert.deepEqual(validateCodeVerdict(JSON.stringify({ findings: [], coverage })).findings, []);
  }
  assert.deepEqual(validateCodeVerdict('{"findings":[]}').findings, []);
  assert.throws(() => validateCodeVerdict('{"findings":{},"coverage":[]}'));
  assert.throws(() => validateCodeVerdict('{"findings":[],"unexpected":true}'));
  assert.throws(() => validateCodeVerdict(JSON.stringify({ findings: [], coverage: "x".repeat(256 * 1024) })));
});

test("partial and over-budget coverage stays unassessed and obeys encoded account cap", () => {
  for (const raw of [undefined, null, {}, "malformed", [{ path: "a", status: "examined", reason: "x".repeat(257) }]]) {
    const got = normalizeCodeCoverage(inventory(["a"]), raw, new Set(), true);
    assert.equal(got.entries[0]?.status, "unassessed");
    assert.ok(got.inventory_incomplete);
  }
  const large = normalizeCodeCoverage(inventory(["a"]), [{ path: "a", status: "examined", reason: "x".repeat(32768) }], new Set(), true);
  assert.equal(large.omitted_judgments, 1); assert.ok(large.reasons.includes("judgments_truncated"));
  const inv = inventory(Array.from({ length: 128 }, (_, i) => "p" + i + "\u0001".repeat(500)));
  const bounded = normalizeCodeCoverage(inv, [], new Set(), true);
  assert.ok(Buffer.byteLength(JSON.stringify(bounded)) <= CODE_COVERAGE_LIMITS.accountBytes);
  assert.ok(bounded.omitted_paths > 0);
  assert.deepEqual(decodeCodeCoverage(bounded), bounded);
});
