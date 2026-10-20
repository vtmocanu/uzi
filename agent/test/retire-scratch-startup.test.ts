// Issue #3153: the startup inspector for `<runnerRoot>/.retire-*` scratch parents and the counted
// best-effort removal diagnostics. `<dataDir>/runner` persists across pod replacements on the
// Docker lane, so a parent left by a crashed process is inspected once, never recursively wiped.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { GitCache, retainedUndeletableCount } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { recordingLogger, testGitCacheOptions } from "./helpers.js";

const WARNING = "runner retire scratch retained";
let fx: Fixture;
let root: string;
let lines: unknown[];
let git: GitCache;
const OLD = Date.now() + 60_000; // "process start" later than every fixture mtime

beforeEach(() => {
  fx = makeFixture();
  root = path.join(fx.dataDir, "runner");
  fs.mkdirSync(root, { recursive: true });
  const rec = recordingLogger();
  lines = rec.lines;
  git = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions());
});
afterEach(() => fx.cleanup());

const warnings = (): Array<{ count: number; paths: string[] }> =>
  lines.filter((l) => (l as { msg?: string }).msg === WARNING) as Array<{ count: number; paths: string[] }>;

describe("inspectRetireScratchAtStartup (#3153)", () => {
  it("removes an empty parent non-recursively and reports nothing", async () => {
    const p = path.join(root, ".retire-aaaa");
    fs.mkdirSync(p);
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { removed: 1, retained: 0 });
    assert.equal(fs.existsSync(p), false);
    assert.equal(warnings().length, 0);
  });

  it("retains a non-empty parent with its contents and emits one counted warning", async () => {
    const full = path.join(root, ".retire-full");
    fs.mkdirSync(path.join(full, "clone"), { recursive: true });
    fs.writeFileSync(path.join(full, "clone", "PRECIOUS"), "x");
    const full2 = path.join(root, ".retire-full2");
    fs.mkdirSync(full2);
    fs.writeFileSync(path.join(full2, "f"), "y");
    const empty = path.join(root, ".retire-empty");
    fs.mkdirSync(empty);
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { removed: 1, retained: 2 });
    assert.equal(fs.readFileSync(path.join(full, "clone", "PRECIOUS"), "utf8"), "x");
    assert.equal(fs.readFileSync(path.join(full2, "f"), "utf8"), "y");
    assert.equal(fs.existsSync(empty), false);
    const w = warnings();
    assert.equal(w.length, 1, "exactly one warning");
    assert.equal(w[0]!.count, 2);
    assert.deepEqual([...w[0]!.paths].sort(), [full, full2].sort());
  });

  it("skips a parent whose mtime is at or after process start, empty or not", async () => {
    const fresh = path.join(root, ".retire-fresh");
    fs.mkdirSync(fresh);
    const freshFull = path.join(root, ".retire-freshfull");
    fs.mkdirSync(path.join(freshFull, "clone"), { recursive: true });
    assert.deepEqual(await git.inspectRetireScratchAtStartup(0), { removed: 0, retained: 0 });
    assert.equal(fs.existsSync(fresh), true);
    assert.equal(fs.existsSync(freshFull), true);
    assert.equal(warnings().length, 0);
  });

  it("does not follow a symlink parent or a symlink entry", async () => {
    const outside = path.join(fx.dataDir, "outside");
    fs.mkdirSync(outside);
    fs.writeFileSync(path.join(outside, "keep"), "k");
    const linkParent = path.join(root, ".retire-link");
    fs.symlinkSync(outside, linkParent);
    const withLink = path.join(root, ".retire-entry");
    fs.mkdirSync(withLink);
    fs.symlinkSync(outside, path.join(withLink, "clone"));
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { removed: 0, retained: 2 });
    assert.ok(fs.lstatSync(linkParent).isSymbolicLink());
    assert.ok(fs.lstatSync(path.join(withLink, "clone")).isSymbolicLink());
    assert.equal(fs.readFileSync(path.join(outside, "keep"), "utf8"), "k");
    assert.equal(warnings()[0]!.count, 2);
  });

  it("ignores entries that are not .retire-* and tolerates a missing runner root", async () => {
    const other = path.join(root, "repo-dir");
    fs.mkdirSync(other);
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { removed: 0, retained: 0 });
    assert.equal(fs.existsSync(other), true);
    fs.rmSync(root, { recursive: true });
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { removed: 0, retained: 0 });
  });

  it("bounds the reported paths", async () => {
    for (let i = 0; i < 15; i++) {
      const p = path.join(root, `.retire-${String(i).padStart(2, "0")}`);
      fs.mkdirSync(p);
      fs.writeFileSync(path.join(p, "f"), "x");
    }
    await git.inspectRetireScratchAtStartup(OLD);
    const w = warnings();
    assert.equal(w[0]!.count, 15);
    assert.equal(w[0]!.paths.length, 10);
  });
});

describe("counted best-effort removal (#3153)", () => {
  it("counts and logs a failed removal instead of swallowing it, and still returns", async () => {
    const seam = git as unknown as { removeBestEffort: (t: string, site: string, kind?: "tree" | "file" | "emptyDir") => Promise<boolean> };
    const before = retainedUndeletableCount();
    // rmdir of a non-empty directory fails with ENOTEMPTY (works under any uid, including root)
    const dir = path.join(root, "not-empty");
    fs.mkdirSync(dir);
    fs.writeFileSync(path.join(dir, "f"), "x");
    assert.equal(await seam.removeBestEffort(dir, "test-site", "emptyDir"), false);
    assert.equal(retainedUndeletableCount(), before + 1);
    const line = lines.find((l) => (l as { msg?: string }).msg === "runner scratch removal failed; keeping it") as
      { path?: string; code?: string; site?: string; retainedUndeletable?: number } | undefined;
    assert.ok(line);
    assert.equal(line.path, dir);
    assert.equal(line.code, "ENOTEMPTY");
    assert.equal(line.site, "test-site");
    assert.equal(line.retainedUndeletable, before + 1);
    assert.equal(fs.existsSync(path.join(dir, "f")), true);
    // an absent target is success and not counted
    assert.equal(await seam.removeBestEffort(path.join(root, "gone"), "test-site"), true);
    assert.equal(retainedUndeletableCount(), before + 1);
  });
});
