// Issue #3153: the startup inspector for `<runnerRoot>/.retire-*` scratch parents and the counted
// best-effort removal diagnostics. `<dataDir>/runner` persists across pod replacements on the
// Docker lane, so a parent left by a crashed process is inspected once, never recursively wiped.
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { spawn } from "node:child_process";
import path from "node:path";
import { GitCache, retainedUndeletableCount } from "../src/git.js";
import { sanitizeForLog } from "../src/run-quiescence.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, recordingLogger, testGitCacheOptions } from "./helpers.js";

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
afterEach(() => { mock.restoreAll(); fx.cleanup(); });

type Diag = { count: number; empty: number; root: string; paths: string[]; skippedRecent: number; incomplete: boolean };
const warnings = (): Diag[] => lines.filter((l) => (l as { msg?: string }).msg === WARNING) as Diag[];

describe("inspectRetireScratchAtStartup (#3153)", () => {
  it("retains an empty parent, removes nothing, and reports it in the counted warning", async () => {
    const p = path.join(root, ".retire-aaaa");
    fs.mkdirSync(p);
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 1 });
    assert.equal(fs.existsSync(p), true);
    const w = warnings();
    assert.equal(w.length, 1);
    assert.equal(w[0]!.count, 1);
    assert.equal(w[0]!.empty, 1);
    assert.deepEqual(w[0]!.paths, [".retire-aaaa"]);
  });

  it("retains an empty parent whose cwd a separate live process holds", async () => {
    const p = path.join(root, ".retire-held");
    fs.mkdirSync(p);
    // A separate process idling with its cwd inside the empty parent; it announces itself once running.
    const child = spawn(process.execPath, ["-e", "console.log('ready'); setInterval(() => {}, 1000)"], {
      cwd: p, stdio: ["ignore", "pipe", "ignore"], env: { PATH: process.env.PATH ?? "/usr/bin:/bin" },
    });
    try {
      await new Promise<void>((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", () => reject(new Error("holder exited early")));
        child.stdout!.once("data", () => resolve());
      });
      assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 1 });
      assert.equal(fs.existsSync(p), true, "the parent a live process holds is not removed");
      assert.equal(child.exitCode, null, "the holder is still running");
      const w = warnings();
      assert.equal(w.length, 1);
      assert.equal(w[0]!.count, 1);
      assert.equal(w[0]!.empty, 1);
    } finally {
      child.kill("SIGKILL");
    }
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
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 3 });
    assert.equal(fs.readFileSync(path.join(full, "clone", "PRECIOUS"), "utf8"), "x");
    assert.equal(fs.readFileSync(path.join(full2, "f"), "utf8"), "y");
    assert.equal(fs.existsSync(empty), true);
    const w = warnings();
    assert.equal(w.length, 1, "exactly one warning");
    assert.equal(w[0]!.count, 3);
    assert.equal(w[0]!.empty, 1);
    assert.deepEqual([...w[0]!.paths].sort(), [".retire-empty", ".retire-full", ".retire-full2"]);
    // The warning bounds the logged root (sanitizeForLog, 157), so a long fixture path (a long
    // TMPDIR) is truncated there; expect the same bounded rendering, exact otherwise.
    assert.equal(w[0]!.root, sanitizeForLog(path.resolve(root), 157));
  });

  it("skips a parent whose mtime is at or after process start, empty or not", async () => {
    const fresh = path.join(root, ".retire-fresh");
    fs.mkdirSync(fresh);
    const freshFull = path.join(root, ".retire-freshfull");
    fs.mkdirSync(path.join(freshFull, "clone"), { recursive: true });
    assert.deepEqual(await git.inspectRetireScratchAtStartup(0), { retained: 0 });
    assert.equal(fs.existsSync(fresh), true);
    assert.equal(fs.existsSync(freshFull), true);
    const w = warnings();
    assert.equal(w.length, 1, "the skip is reported, not silent");
    assert.equal(w[0]!.skippedRecent, 2);
    assert.equal(w[0]!.count, 0);
    assert.equal(w[0]!.incomplete, false);
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
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 2 });
    assert.ok(fs.lstatSync(linkParent).isSymbolicLink());
    assert.ok(fs.lstatSync(path.join(withLink, "clone")).isSymbolicLink());
    assert.equal(fs.readFileSync(path.join(outside, "keep"), "utf8"), "k");
    assert.equal(warnings()[0]!.count, 2);
  });

  it("ignores entries that are not .retire-* and tolerates a missing runner root", async () => {
    const other = path.join(root, "repo-dir");
    fs.mkdirSync(other);
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 0 });
    assert.equal(fs.existsSync(other), true);
    fs.rmSync(root, { recursive: true });
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 0 });
  });

  it("charges the budget on every entry: a flood of non-matching names stops the scan and reports it incomplete", async () => {
    const flood = 4200; // above RETIRE_SCRATCH_ENUMERATION_BUDGET (4096)
    for (let i = 0; i < flood; i++) fs.writeFileSync(path.join(root, `junk-${i}`), "x");
    const empty = path.join(root, ".retire-empty");
    fs.mkdirSync(empty);
    const res = await git.inspectRetireScratchAtStartup(OLD);
    assert.ok(res.retained <= 1, JSON.stringify(res));
    assert.equal(fs.readdirSync(root).filter((n) => n.startsWith("junk-")).length, flood, "nothing else deleted");
    assert.equal(fs.existsSync(empty), true, "nothing is removed, whether or not the scan reached it");
    const w = warnings();
    assert.equal(w.length, 1);
    assert.equal(w[0]!.incomplete, true);
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

describe("retireRunnerClone holding disposal (#3153)", () => {
  it("counts a failed holding-dir removal on the discard path", async () => {
    const owner = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
    const bare = await git.ensureClone(fx.originPath);
    const branch = "agent/issue-3153";
    const clone = await git.createOrAttachRunnerClone(bare, 3153, noProofReseed, owner);
    fs.writeFileSync(path.join(clone.path, "OWNER.txt"), "owner bytes\n");
    await git.markRecoveryCapture(bare, clone.path, branch, owner);
    const holdingRoot = path.join(fx.dataDir, "runner-quarantine");
    const realRm = fsp.rm.bind(fsp);
    mock.method(fsp, "rm", async (target: string, opts?: unknown) => {
      if (String(target).startsWith(holdingRoot)) throw Object.assign(new Error("rm: permission denied"), { code: "EACCES" });
      return realRm(target, opts as never);
    });
    const before = retainedUndeletableCount();
    await git.retireRunnerClone(bare, clone.path, branch, owner, { discard: true });
    assert.equal(retainedUndeletableCount(), before + 1);
    assert.ok(lines.some((l) => (l as { msg?: string; site?: string }).site === "retire-holding-dispose"));
  });
});
