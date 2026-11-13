// Issue #3153: the startup inspector for `<runnerRoot>/.retire-*` scratch parents and the counted
// best-effort removal diagnostics. `<dataDir>/runner` persists across pod replacements on the
// Docker lane, so a parent left by a crashed process is inspected once, never recursively wiped.
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import { execFileSync, spawn } from "node:child_process";
import { createHash } from "node:crypto";
import path from "node:path";
import { GitCache, readRetireScratchOutcomesActivation, restoreJournalJson, retainedUndeletableCount } from "../src/git.js";
import { restoreDigest, type RestoreBasis } from "../src/restore-journal.js";
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

// ---- issue #4143: observation records (never a removal) ----------------------------------------------

const OUTCOME = "runner retire scratch outcome";
const TOTAL = "runner retire scratch total";
type Row = {
  name: string; kind: string; outcome: string; reason: string; retained_source: string; empty: boolean; recent: boolean;
  bytes: number; bytes_state: string; partial_reasons: string[];
};
type Total = {
  parents_enumerated: number; outcomes_omitted: number; kept: number; reclaimed: number; protected_by_retained_source: number;
  no_disposition_evidence: number; bytes: number; bytes_state: string; partial_reasons: string[]; enumeration_incomplete: boolean;
  retained_source_read: string;
};
type Opts = NonNullable<Parameters<GitCache["inspectRetireScratchAtStartup"]>[1]>;
const rows = (): Row[] => lines.filter((l) => (l as { msg?: string }).msg === OUTCOME) as Row[];
const totals = (): Total[] => lines.filter((l) => (l as { msg?: string }).msg === TOTAL) as Total[];
const run = (opts: Opts = {}, start = OLD) => git.inspectRetireScratchAtStartup(start, { outcomes: true, ...opts });
const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));
const HAS_PROC_FD = fs.existsSync("/proc/self/fd");
const PROC_FD_REQUIRED = { skip: HAS_PROC_FD ? false : "no /proc/self/fd on this host: the pinned walk refuses here by design" };
const fdCount = () => fs.readdirSync("/proc/self/fd").length;
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
/** Allocated bytes of the listed fixture paths: lstat blocks * 512, an inode (hard link) once. */
function alloc(...paths: string[]): number {
  const seen = new Set<string>();
  let total = 0;
  for (const p of paths) {
    const st = fs.lstatSync(p);
    const key = `${st.dev}:${st.ino}`;
    if (seen.has(key)) continue;
    seen.add(key);
    total += st.blocks * 512;
  }
  return total;
}

function mkParent(name: string, files: Record<string, string | Buffer> = {}): string {
  const p = path.join(root, name);
  fs.mkdirSync(p, { recursive: true });
  for (const [rel, body] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(p, rel)), { recursive: true });
    fs.writeFileSync(path.join(p, rel), body);
  }
  return p;
}
function mkBare(name = "r1"): string {
  const bare = path.join(fx.dataDir, "repos", `${name}.git`);
  fs.mkdirSync(path.dirname(bare), { recursive: true });
  execFileSync("git", ["init", "--bare", "--quiet", bare], { env: GIT_ENV });
  return bare;
}
function setConfig(bare: string, key: string, value: string, add = false): void {
  execFileSync("git", ["-C", bare, "config", "--local", ...(add ? ["--add"] : []), key, value], { env: GIT_ENV });
}
const journal = (clonePath: string, extra: Record<string, unknown> = {}) => JSON.stringify({ runId: "run1", clonePath, ...extra });
const RECOVERY = "uzi-recovery.agent/issue-1.clone";
const RESTORE = "uzi-restore.agent/issue-1.journal";
const P_ID = "20261110T010000Z-g1-0123456789abcdef";
const S_ID = "20261110T020000Z-g2-fedcba9876543210";
function composite(pPath: string, sPath: string): Record<string, string> {
  const pRaw = journal(pPath, { attemptId: P_ID });
  const basis: RestoreBasis = {
    version: 1, predecessor: { clonePath: pPath, attemptId: P_ID, generation: 1, root: { dev: "2049", ino: "131072" } },
    journalDigest: restoreDigest(pRaw), boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
    slotOwner: { worker: "worker-incarnation-1", epoch: "epoch-1" },
    pin: { captureId: "capture-1", generation: 1, sourceSha: "b".repeat(40), finalization: true }, restoreTip: "b".repeat(40),
  };
  return {
    [RECOVERY]: pRaw,
    [RESTORE]: restoreJournalJson(basis, "seeding", { runId: "run1", attemptId: S_ID, clonePath: sPath }),
    "uzi-attempts.agent/issue-1.entry": JSON.stringify({ attemptId: S_ID, runId: "run1", clonePath: sPath, state: "live" }),
  };
}

/** Every entry of a tree with mode, size, mtime, links and a content digest (unreadable directories end the walk). */
function snapshot(dir: string): string[] {
  const out: string[] = [];
  const walk = (p: string, rel: string): void => {
    const st = fs.lstatSync(p);
    const body = st.isSymbolicLink() ? fs.readlinkSync(p) : st.isFile() ? createHash("sha256").update(fs.readFileSync(p)).digest("hex") : "";
    out.push(`${rel}|${st.mode.toString(8)}|${st.size}|${st.mtimeMs}|${st.nlink}|${body}`);
    if (!st.isDirectory()) return;
    let names: string[];
    try { names = fs.readdirSync(p).sort(); } catch { return; }
    for (const n of names) walk(path.join(p, n), `${rel}/${n}`);
  };
  walk(dir, ".");
  return out;
}

/** Run once on `parentName`, assert nothing changed, and return its single row. */
async function observeOne(parentName: string, opts: Opts = {}, start = OLD): Promise<Row> {
  const before = snapshot(root);
  const res = await run(opts, start);
  assert.deepEqual(snapshot(root), before, "the tree is byte-identical, mtimes included");
  assert.equal(res.retained, rows().length === 1 && rows()[0]!.recent ? 0 : 1);
  const r = rows();
  assert.equal(r.length, 1);
  assert.equal(r[0]!.name, parentName);
  assert.equal(r[0]!.outcome, "kept");
  assert.equal(totals().length, 1);
  assert.equal(totals()[0]!.reclaimed, 0);
  return r[0]!;
}

describe("retire scratch outcome records (#4143)", () => {
  it("an empty parent is kept with no disposition evidence", PROC_FD_REQUIRED, async () => {
    mkParent(".retire-empty");
    const r = await observeOne(".retire-empty");
    assert.equal(r.kind, "dir");
    assert.equal(r.empty, true);
    assert.equal(r.reason, "no_disposition_evidence");
    assert.equal(r.retained_source, "not_named");
    assert.equal(r.bytes_state, "measured");
    assert.equal(r.bytes, alloc(path.join(root, ".retire-empty")));
    assert.deepEqual(r.partial_reasons, []);
    assert.equal(r.recent, false);
  });

  it("an interrupted rename (journal names the canonical path, which is absent) is kept, not named", PROC_FD_REQUIRED, async () => {
    const parent = mkParent(".retire-rename", { "clone/src/a.txt": "work", "clone/.git": "gitdir: x" });
    const canonical = path.join(root, "slug", "issue-1");
    assert.equal(fs.existsSync(canonical), false);
    setConfig(mkBare(), RECOVERY, journal(canonical));
    const r = await observeOne(".retire-rename");
    assert.equal(r.reason, "no_disposition_evidence");
    assert.equal(r.retained_source, "not_named");
    assert.equal(r.empty, false);
    const j = (...s: string[]) => path.join(parent, ...s);
    assert.equal(r.bytes, alloc(parent, j("clone"), j("clone", "src"), j("clone", "src", "a.txt"), j("clone", ".git")));
    assert.equal(totals()[0]!.retained_source_read, "read");
  });

  it("completed metadata with a failed removal (retired ledger, empty journal) grants nothing", async () => {
    mkParent(".retire-done", { "clone/leftover": "x" });
    const bare = mkBare();
    const clone = path.join(root, ".retire-done", "clone");
    setConfig(bare, "uzi-attempts.agent/issue-1.entry", JSON.stringify({ attemptId: S_ID, runId: "run1", clonePath: clone, state: "retired" }));
    setConfig(bare, RECOVERY, "");
    const r = await observeOne(".retire-done");
    assert.equal(r.reason, "no_disposition_evidence");
    assert.equal(r.retained_source, "not_named");
  });

  it("a partial worker-side removal and an unknown-owner remnant are kept and measured", PROC_FD_REQUIRED, async () => {
    const a = mkParent(".retire-partial", { "clone/node_modules/x/y.js": "y".repeat(5000), "clone/keep": "k" });
    fs.mkdirSync(path.join(a, "clone", "removed-dir"));
    const r = await observeOne(".retire-partial");
    assert.equal(r.bytes_state, "measured");
    const ja = (...s: string[]) => path.join(a, ...s);
    assert.equal(r.bytes, alloc(a, ja("clone"), ja("clone", "node_modules"), ja("clone", "node_modules", "x"),
      ja("clone", "node_modules", "x", "y.js"), ja("clone", "keep"), ja("clone", "removed-dir")));
    lines.length = 0;
    const b = mkParent(".retire-foreign", { "uid-unknown/.hidden": "h", "uid-unknown/sub/f": "f" });
    fs.rmSync(a, { recursive: true });
    const rb = await observeOne(".retire-foreign");
    const jb = (...s: string[]) => path.join(b, ...s);
    assert.equal(rb.bytes, alloc(b, jb("uid-unknown"), jb("uid-unknown", ".hidden"), jb("uid-unknown", "sub"), jb("uid-unknown", "sub", "f")));
  });

  it("a mode-000 directory inside is partial with the unreadable reason, never opened by name", PROC_FD_REQUIRED, async (t) => {
    const parent = mkParent(".retire-locked", { "open/f": "x" });
    const locked = path.join(parent, "locked");
    fs.mkdirSync(locked);
    fs.writeFileSync(path.join(locked, "secret"), "s");
    fs.chmodSync(locked, 0o000);
    try {
      let readable = true;
      try { fs.readdirSync(locked); } catch { readable = false; }
      if (readable) { t.skip(`readdir of a mode-000 directory succeeded (uid ${process.getuid?.()}); nothing to observe`); return; }
      const r = await observeOne(".retire-locked");
      assert.equal(r.bytes_state, "partial");
      assert.ok(r.partial_reasons.includes("unreadable"));
      assert.equal(r.empty, false);
      assert.equal(totals()[0]!.bytes_state, "partial");
    } finally {
      fs.chmodSync(locked, 0o755);
    }
  });

  it("a recent parent is still recorded, flagged recent", async () => {
    mkParent(".retire-recent", { f: "x" });
    const r = await observeOne(".retire-recent", {}, 0);
    assert.equal(r.recent, true);
    assert.equal(r.reason, "no_disposition_evidence");
    assert.equal(warnings()[0]!.skippedRecent, 1);
    assert.equal(warnings()[0]!.count, 0);
  });

  it("a symlink parent and a regular-file parent count only their own lstat and are never followed", PROC_FD_REQUIRED, async () => {
    const outside = path.join(fx.dataDir, "outside");
    fs.mkdirSync(outside);
    fs.writeFileSync(path.join(outside, "big"), Buffer.alloc(1024 * 1024, 1));
    fs.symlinkSync(outside, path.join(root, ".retire-link"));
    fs.writeFileSync(path.join(root, ".retire-file"), Buffer.alloc(8192, 2));
    const outsideBefore = snapshot(outside);
    const before = snapshot(root);
    assert.deepEqual(await run(), { retained: 2 });
    assert.deepEqual(snapshot(root), before);
    assert.deepEqual(snapshot(outside), outsideBefore);
    const byName = new Map(rows().map((r) => [r.name, r]));
    assert.equal(byName.get(".retire-link")!.kind, "symlink");
    assert.ok(byName.get(".retire-link")!.bytes < 1024 * 1024, "the symlink target is not counted");
    assert.equal(byName.get(".retire-link")!.bytes, alloc(path.join(root, ".retire-link")));
    assert.equal(byName.get(".retire-file")!.kind, "other");
    assert.equal(byName.get(".retire-file")!.bytes, alloc(path.join(root, ".retire-file")));
    assert.equal(byName.get(".retire-file")!.bytes_state, "measured");
  });

  it("an unsupported proc-fd host reports unknown bytes and leaves every parent unchanged", async () => {
    mkParent(".retire-unsupported", { "clone/f": "retained work" });
    const before = snapshot(root);
    const lstat = fsp.lstat.bind(fsp);
    mock.method(fsp, "lstat", (...args: Parameters<typeof fsp.lstat>) => {
      if (String(args[0]).startsWith("/proc/self/fd/")) {
        return Promise.reject(Object.assign(new Error("proc-fd unavailable"), { code: "ENOENT" }));
      }
      return lstat(...args);
    });
    await run({ readRetainedSources: async () => ({ state: "read", paths: [] }) });
    assert.deepEqual(snapshot(root), before);
    assert.equal(rows()[0]!.outcome, "kept");
    assert.equal(rows()[0]!.bytes_state, "unknown");
    assert.equal(totals()[0]!.bytes_state, "unknown");
    assert.equal(totals()[0]!.reclaimed, 0);
  });
});

describe("retire scratch journal attribution (#4143)", () => {
  const PARENT = ".retire-a";
  const named = (name = "clone") => path.join(root, PARENT, name);
  const expectNamed = async () => {
    const r = await observeOne(PARENT);
    assert.equal(r.reason, "protected_by_retained_source");
    assert.equal(r.retained_source, "named");
    assert.equal(totals()[0]!.protected_by_retained_source, 1);
    assert.equal(totals()[0]!.retained_source_read, "read");
  };
  const expectUnknown = async () => {
    const r = await observeOne(PARENT);
    assert.equal(r.reason, "no_disposition_evidence");
    assert.equal(r.retained_source, "unknown");
    assert.equal(totals()[0]!.retained_source_read, "unknown");
  };
  beforeEach(() => { mkParent(PARENT, { "clone/f": "x" }); });

  it("retainedSources naming <parent>/clone protects the parent", async () => {
    setConfig(mkBare(), RECOVERY, journal(path.join(root, "slug", "issue-1"), { retainedSources: [{ runId: "run1", clonePath: named() }] }));
    await expectNamed();
  });

  it("a top-level clonePath of <parent>/clone protects the parent, and the parent path itself counts", async () => {
    setConfig(mkBare(), RECOVERY, journal(named()));
    await expectNamed();
  });

  it("the restore-entry variant (composite predecessor and successor) protects the parent", async () => {
    const bare = mkBare();
    for (const [k, v] of Object.entries(composite(path.join(root, "slug", "pred.attempt-" + P_ID), named(`succ.attempt-${S_ID}`)))) setConfig(bare, k, v);
    await expectNamed();
  });

  it("cleared recovery and restore keys next to a valid journal across two bares read as protected", async () => {
    const one = mkBare("r1"), two = mkBare("r2");
    setConfig(one, RECOVERY, "");
    setConfig(one, RESTORE, "");
    setConfig(two, "uzi-recovery.agent/issue-2.clone", journal(named()));
    await expectNamed();
  });

  it("duplicate restore keys (non-empty then empty, or two non-empty) make the read unknown", async () => {
    const bare = mkBare();
    setConfig(bare, RESTORE, "{\"v\":1}", true);
    setConfig(bare, RESTORE, "", true);
    await expectUnknown();
    lines.length = 0;
    const second = mkBare("r2");
    setConfig(second, "uzi-restore.agent/issue-9.journal", "{\"v\":1}", true);
    setConfig(second, "uzi-restore.agent/issue-9.journal", "{\"v\":2}", true);
    fs.rmSync(bare, { recursive: true });
    await expectUnknown();
  });

  it("a duplicate recovery key reads last-value-wins", async () => {
    const bare = mkBare();
    setConfig(bare, RECOVERY, "not json at all", true);
    setConfig(bare, RECOVERY, journal(named()), true);
    await expectNamed();
  });

  it("a malformed value is unknown", async () => {
    setConfig(mkBare(), RECOVERY, "{not json");
    await expectUnknown();
  });

  it("a valueless recovery key is unknown", async () => {
    const bare = mkBare();
    fs.appendFileSync(path.join(bare, "config"), '\n[uzi-recovery "agent/issue-1"]\n\tclone\n');
    await expectUnknown();
  });

  it("an empty restore predecessor is unknown", async () => {
    const bare = mkBare();
    const cfg = composite(path.join(root, "slug", "pred.attempt-" + P_ID), named(`succ.attempt-${S_ID}`));
    setConfig(bare, RESTORE, cfg[RESTORE]!);
    setConfig(bare, "uzi-attempts.agent/issue-1.entry", cfg["uzi-attempts.agent/issue-1.entry"]!);
    setConfig(bare, RECOVERY, "");
    await expectUnknown();
  });

  it("an unrelated uzi-* value that names the path is not attribution", async () => {
    const bare = mkBare();
    setConfig(bare, "uzi-other.key.value", named());
    setConfig(bare, "uzi-attempts.agent/issue-1.entry", JSON.stringify({ attemptId: S_ID, runId: "run1", clonePath: named(), state: "live" }));
    const r = await observeOne(PARENT);
    assert.equal(r.reason, "no_disposition_evidence");
    assert.equal(r.retained_source, "not_named");
  });

  it("an injected reader failure is unknown", async () => {
    const r = await observeOne(PARENT, { readRetainedSources: async () => { throw new Error("boom"); } });
    assert.equal(r.retained_source, "unknown");
    assert.equal(r.reason, "no_disposition_evidence");
  });

  it("a sibling prefix is not named (P + '/' boundary)", async () => {
    setConfig(mkBare(), RECOVERY, journal(path.join(root, `${PARENT}x`, "clone")));
    const r = await observeOne(PARENT);
    assert.equal(r.retained_source, "not_named");
  });
});

describe("retire scratch budget and settlement (#4143)", () => {
  type Spy = { withLock: unknown[][]; recovery: unknown[][]; configs: string[] };
  function spy(): Spy {
    const seam = git as unknown as Record<string, (...a: unknown[]) => unknown>;
    const calls: Spy = { withLock: [], recovery: [], configs: [] };
    const lock = seam.withLock!.bind(git), rec = seam.withRecoveryOperation!.bind(git), cfg = seam.configEntries!.bind(git);
    mock.method(seam, "withLock", (...a: unknown[]) => { calls.withLock.push(a); return lock(...a); });
    mock.method(seam, "withRecoveryOperation", (...a: unknown[]) => { calls.recovery.push(a); return rec(...a); });
    mock.method(seam, "configEntries", (...a: unknown[]) => { calls.configs.push(String(a[0])); return cfg(...a); });
    return calls;
  }
  beforeEach(() => { mkParent(".retire-a", { f: "x" }); });

  it("several bares are read under one deadline and one signal, never reset", async () => {
    for (const n of ["r1", "r2", "r3"]) setConfig(mkBare(n), RECOVERY, journal(path.join(root, "slug", n)));
    const calls = spy();
    const t0 = Date.now();
    const stop = new AbortController();
    await run({ now: () => t0, signal: stop.signal });
    assert.equal(calls.recovery.length, 1);
    assert.equal(calls.recovery[0]![0], stop.signal);
    assert.equal(calls.recovery[0]![1], t0 + 30_000);
    assert.equal(calls.withLock.length, 3);
    const signals = new Set(calls.withLock.map((c) => c[2]));
    assert.equal(signals.size, 1, "one combined signal for every bare");
    assert.ok(signals.values().next().value instanceof AbortSignal);
    assert.equal(totals()[0]!.retained_source_read, "read");
  });

  it("a contended lock past the deadline is unknown, later bares are not read, and the holder is untouched", async () => {
    const [one, two, three] = ["r1", "r2", "r3"].map((n) => mkBare(n));
    for (const [i, b] of [one, two, three].entries()) setConfig(b!, RECOVERY, journal(path.join(root, "slug", String(i))));
    const calls = spy();
    const seam = git as unknown as { withLock: (key: string, fn: () => Promise<unknown>) => Promise<unknown> };
    // The spy above wraps withLock; take the lock through the same seam before the inspector starts.
    let release!: () => void;
    let holding = false;
    const held = seam.withLock(two!, () => new Promise<void>((resolve) => { holding = true; release = resolve; }));
    await sleep(20);
    assert.equal(holding, true, "the test holds the second bare's lock");
    calls.withLock.length = 0;
    const startedAt = Date.now();
    await run({ now: () => Date.now() - 29_700 });
    const waited = Date.now() - startedAt;
    assert.ok(waited >= 150, `the inspector returned only after the queued wait ended (${waited} ms)`);
    assert.equal(holding, true);
    assert.equal(totals()[0]!.retained_source_read, "unknown");
    assert.deepEqual(calls.configs, [one], "only the first bare was read; the contended one never started and the third was not tried");
    let holderDone = false;
    void held.then(() => { holderDone = true; });
    await sleep(20);
    assert.equal(holderDone, false, "the held lock was not released by the inspector");
    release();
    await held;
  });

  it("an abort during the read is unknown", async () => {
    for (const n of ["r1", "r2"]) setConfig(mkBare(n), RECOVERY, journal(path.join(root, "slug", n)));
    const stop = new AbortController();
    const seam = git as unknown as Record<string, (...a: unknown[]) => unknown>;
    const lock = seam.withLock!.bind(git);
    mock.method(seam, "withLock", (...a: unknown[]) => { stop.abort(); return lock(...a); });
    await run({ signal: stop.signal });
    assert.equal(totals()[0]!.retained_source_read, "unknown");
  });

  it("an abort inside bare enumeration is unknown and no bare is locked or read afterwards", async () => {
    for (const n of ["r1", "r2"]) setConfig(mkBare(n), RECOVERY, journal(path.join(root, "slug", n)));
    const calls = spy();
    const stop = new AbortController();
    const seam = git as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
    const enumerate = seam.retainedBares!.bind(git);
    mock.method(seam, "retainedBares", async (...a: unknown[]) => {
      stop.abort();
      const bares = await enumerate(...a);
      calls.withLock.length = 0;
      calls.configs.length = 0;
      return bares;
    });
    await run({ signal: stop.signal });
    assert.equal(totals()[0]!.retained_source_read, "unknown");
    assert.equal(calls.withLock.length, 0, "no bare lock after the abort");
    assert.equal(calls.configs.length, 0, "no config read after the abort");
  });

  it("the branch loop stops at the deadline even when every await settles on a microtask", async () => {
    const bare = mkBare();
    for (let i = 1; i <= 6; i++) setConfig(bare, `uzi-recovery.agent/issue-${i}.clone`, "");
    const seam = git as unknown as Record<string, (...a: unknown[]) => unknown>;
    const resolver = mock.method(seam, "resolveBranchJournalUnderLock", seam.resolveBranchJournalUnderLock!.bind(git));
    const t0 = Date.now();
    await run({ now: () => (resolver.mock.callCount() >= 2 ? t0 + 31_000 : t0) });
    assert.equal(totals()[0]!.retained_source_read, "unknown");
    assert.ok(resolver.mock.callCount() < 6, `the resolver ran ${resolver.mock.callCount()} times for 6 branches`);
  });

  it("every git call settles before the inspector resolves, even past the deadline", async () => {
    setConfig(mkBare(), RECOVERY, journal(path.join(root, "slug", "r1")));
    const seam = git as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
    const realRunGit = seam.runGit!.bind(git);
    let seq = 0;
    let late = false;
    let first = true;
    const settled: number[] = [];
    mock.method(seam, "runGit", (...a: unknown[]) => {
      const slow = first;
      first = false;
      const p = (async () => {
        if (slow) { await sleep(150); late = true; }
        return realRunGit(...a);
      })();
      const idx = settled.push(-1) - 1;
      const mark = (): void => { settled[idx] = ++seq; };
      p.then(mark, mark);
      return p;
    });
    const t0 = Date.now();
    await run({ now: () => (late ? t0 + 31_000 : t0) });
    const resolvedAt = ++seq;
    assert.ok(settled.length > 0, "git was called");
    assert.ok(settled.every((s) => s > 0 && s < resolvedAt), `every git call settled first: ${settled.join(",")} < ${resolvedAt}`);
    assert.equal(totals()[0]!.retained_source_read, "unknown");
  });

  const limitCases: Array<[string, NonNullable<Opts["limits"]>, number]> = [
    ["maxBares", { maxBares: 1 }, 0],
    ["maxMetadataBytes", { maxMetadataBytes: 16 }, 1],
    ["maxMetadataRecords", { maxMetadataRecords: 1 }, 1],
    ["maxSeenPaths", { maxSeenPaths: 1 }, 1],
  ];
  for (const [name, limits, configs] of limitCases) {
    it(`${name} forced small makes the read unknown and stops at the first refusal`, async () => {
      for (const n of ["r1", "r2"]) {
        setConfig(mkBare(n), RECOVERY, journal(path.join(root, "slug", n), { retainedSources: [{ runId: "run1", clonePath: path.join(root, "slug", `${n}-b`) }] }));
      }
      const calls = spy();
      await run({ limits });
      assert.equal(totals()[0]!.retained_source_read, "unknown");
      assert.equal(calls.configs.length, configs, "no bare after the refusal is read");
    });
  }

  it("decoded journal bytes are charged before the resolver parses them", async () => {
    const rec = recordingLogger();
    const small = new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions({ recoveryDecodedLimitBytes: 10 }));
    setConfig(mkBare(), RECOVERY, journal(path.join(root, "slug", "r1"))); // well over 10 bytes
    const seam = small as unknown as Record<string, (...a: unknown[]) => unknown>;
    const resolver = mock.method(seam, "resolveBranchJournalUnderLock", seam.resolveBranchJournalUnderLock!.bind(small));
    await small.inspectRetireScratchAtStartup(OLD, { outcomes: true });
    assert.equal(resolver.mock.callCount(), 0, "the resolver (and its parse) never ran once the budget was exceeded");
    const total = rec.lines.find((l) => (l as { msg?: string }).msg === TOTAL) as Total | undefined;
    assert.equal(total?.retained_source_read, "unknown");
  });

  it("a clock past the deadline mid-walk caps the walk with time_cap", PROC_FD_REQUIRED, async () => {
    mkParent(".retire-big", Object.fromEntries(Array.from({ length: 10 }, (_, i) => [`f${i}`, "x".repeat(4096)])));
    const t0 = Date.now();
    let late = false;
    await run({
      now: () => (late ? t0 + 31_000 : t0),
      beforeEntry: async (dir) => { if (dir === path.join(root, ".retire-big")) late = true; },
    });
    const big = rows().find((r) => r.name === ".retire-big")!;
    assert.equal(big.bytes_state, "partial");
    assert.ok(big.partial_reasons.includes("time_cap"));
    assert.equal(totals()[0]!.bytes_state, "partial");
    assert.ok(totals()[0]!.partial_reasons.includes("time_cap"));
  });

  it("an already spent clock leaves every parent unknown with time_cap", async () => {
    const t0 = Date.now();
    let calls = 0;
    await run({ now: () => (calls++ === 0 ? t0 : t0 + 31_000) });
    assert.ok(rows().every((r) => r.bytes_state === "unknown" && r.partial_reasons.includes("time_cap")));
    assert.equal(totals()[0]!.bytes_state, "unknown");
  });

  it("settles before returning: the lock re-acquires at once, no git child and no handle remains", PROC_FD_REQUIRED, async () => {
    for (const n of ["r1", "r2"]) setConfig(mkBare(n), RECOVERY, journal(path.join(root, "slug", n)));
    mkParent(".retire-b", { "d/e/f": "x" });
    const gitChildren = (): number => {
      let n = 0;
      for (const pid of fs.readdirSync("/proc")) {
        if (!/^\d+$/.test(pid)) continue;
        try {
          const m = /^\d+ \((.*)\) \S (\d+)/.exec(fs.readFileSync(`/proc/${pid}/stat`, "utf8"));
          if (m && m[1] === "git" && Number(m[2]) === process.pid) n++;
        } catch { /* gone */ }
      }
      return n;
    };
    const fdsBefore = fdCount();
    await run();
    assert.equal(gitChildren(), 0);
    const seam = git as unknown as { withLock: (key: string, fn: () => Promise<string>) => Promise<string> };
    const t = Date.now();
    assert.equal(await seam.withLock(path.join(fx.dataDir, "repos", "r1.git"), async () => "ok"), "ok");
    assert.ok(Date.now() - t < 1000, "the bare lock is free");
    assert.equal(fdCount(), fdsBefore, "no descriptor leaked");
  });

  it("leaks no descriptor after a cap stop or an unknown read", PROC_FD_REQUIRED, async () => {
    mkParent(".retire-c", Object.fromEntries(Array.from({ length: 20 }, (_, i) => [`d${i}/f`, "x"])));
    const before = fdCount();
    await run({ walkLimits: { maxEntries: 3 } });
    assert.equal(fdCount(), before, "entry cap stop");
    await run({ readRetainedSources: async () => { throw new Error("x"); }, now: () => Date.now() });
    assert.equal(fdCount(), before, "unknown read");
  });
});

describe("retire scratch output bound (#4143)", () => {
  it("an expired budget does not normalize retained paths for many parents", async () => {
    for (let i = 0; i < 64; i++) mkParent(`.retire-${i}`);
    const paths = Array.from({ length: 128 }, (_, i) => `/retire-classification-fixture/${i}/clone`);
    const before = snapshot(root);
    const start = Date.now();
    let expired = false;
    let normalized = 0;
    const resolve = path.resolve.bind(path);
    mock.method(path, "resolve", (...args: string[]) => {
      if (args[0]?.startsWith("/retire-classification-fixture/")) normalized++;
      return resolve(...args);
    });
    await run({ now: () => expired ? start + 31_000 : start,
      readRetainedSources: async () => { expired = true; return { state: "read", paths }; } });
    assert.equal(normalized, 0, "expired classification must not scan or normalize source paths");
    assert.equal(totals()[0]!.retained_source_read, "unknown");
    assert.ok(rows().every(r => r.outcome === "kept" && r.retained_source === "unknown"));
    assert.deepEqual(snapshot(root), before);
  });

  it("normalizes each retained path once across many parents and the total", async () => {
    const parents = Array.from({ length: 64 }, (_, i) => mkParent(`.retire-${i}`));
    const paths = Array.from({ length: 128 }, (_, i) => `${parents[i % parents.length]}/clone/${i}`);
    const start = Date.now();
    const normalized = new Map<string, number>();
    const resolve = path.resolve.bind(path);
    mock.method(path, "resolve", (...args: string[]) => {
      if (paths.includes(args[0]!)) {
        const count = (normalized.get(args[0]!) ?? 0) + 1;
        normalized.set(args[0]!, count);
        assert.equal(count, 1, "a source path is normalized once, not once per parent or output row");
      }
      return resolve(...args);
    });
    await run({ now: () => start, readRetainedSources: async () => ({ state: "read", paths }) });
    assert.equal(normalized.size, paths.length);
    assert.equal(totals()[0]!.protected_by_retained_source, parents.length);
    assert.equal(totals()[0]!.outcomes_omitted, parents.length - 10);
    assert.ok(rows().every(r => r.outcome === "kept" && r.retained_source === "named"));
  });

  for (const stop of ["deadline", "abort"] as const) {
    it(`stops retained-path classification on ${stop} and reports all attribution unknown`, async () => {
      const parent = mkParent(".retire-stop");
      const paths = Array.from({ length: 16 }, (_, i) => `${parent}/clone/${i}`);
      const start = Date.now();
      const controller = new AbortController();
      let stopped = false;
      let normalized = 0;
      const resolve = path.resolve.bind(path);
      mock.method(path, "resolve", (...args: string[]) => {
        if (paths.includes(args[0]!)) {
          normalized++;
          if (normalized === 3) { stopped = true; if (stop === "abort") controller.abort(); }
        }
        return resolve(...args);
      });
      await run({ now: () => stopped && stop === "deadline" ? start + 31_000 : start,
        signal: controller.signal, readRetainedSources: async () => ({ state: "read", paths }) });
      assert.equal(normalized, 3, "no further path is processed after cancellation or expiry");
      assert.equal(totals()[0]!.retained_source_read, "unknown");
      assert.equal(totals()[0]!.protected_by_retained_source, 0);
      assert.equal(rows()[0]!.retained_source, "unknown");
    });
  }

  it("25 parents give 10 rows and a total over all of them", async () => {
    const parents: string[] = [];
    for (let i = 0; i < 25; i++) parents.push(mkParent(`.retire-${String(i).padStart(2, "0")}`, { "clone/f": "x".repeat(1000 * (i + 1)) }));
    setConfig(mkBare(), RECOVERY, journal(path.join(parents[3]!, "clone"), { retainedSources: [
      { runId: "run1", clonePath: path.join(parents[20]!, "clone") }, { runId: "run1", clonePath: path.join(parents[24]!, "clone") }] }));
    const expectedBytes = parents.reduce((n, p) => n + alloc(p, path.join(p, "clone"), path.join(p, "clone", "f")), 0);
    await run();
    assert.equal(rows().length, 10);
    assert.deepEqual(rows().map((r) => r.name), parents.slice(0, 10).map((p) => path.basename(p)));
    const t = totals();
    assert.equal(t.length, 1);
    assert.equal(t[0]!.parents_enumerated, 25);
    assert.equal(t[0]!.outcomes_omitted, 15);
    assert.equal(t[0]!.kept, 25);
    assert.equal(t[0]!.reclaimed, 0);
    if (HAS_PROC_FD) {
      assert.equal(t[0]!.bytes, expectedBytes, "bytes cover all 25, not just the reported rows");
      assert.equal(t[0]!.bytes_state, "measured");
    } else {
      assert.equal(t[0]!.bytes_state, "unknown");
    }
    assert.equal(t[0]!.protected_by_retained_source, 3);
    assert.equal(t[0]!.no_disposition_evidence, 22);
    assert.equal(t[0]!.protected_by_retained_source + t[0]!.no_disposition_evidence, 25);
    assert.equal(t[0]!.enumeration_incomplete, false);
    assert.equal(rows().filter((r) => r.reason === "protected_by_retained_source").length, 1, "only parent 03 is in the reported rows");
  });

  it("enumeration overflow is independent of omitted outcomes", async () => {
    for (let i = 0; i < 12; i++) mkParent(`.retire-${String(i).padStart(2, "0")}`, { f: "x" });
    await run();
    assert.equal(totals()[0]!.outcomes_omitted, 2);
    assert.equal(totals()[0]!.enumeration_incomplete, false);
    lines.length = 0;
    for (let i = 0; i < 4200; i++) fs.writeFileSync(path.join(root, `junk-${i}`), "x");
    // Parents created on both sides of the flood, so any directory order still reaches some of them.
    for (let i = 0; i < 6; i++) mkParent(`.retire-zz-${i}`, { f: "x" });
    await run();
    const t = totals()[0]!;
    assert.equal(t.enumeration_incomplete, true);
    assert.equal(t.outcomes_omitted, t.parents_enumerated - rows().length);
    assert.notEqual(t.bytes_state, "measured", "an incomplete enumeration is never a measured total");
  });
});

describe("retire scratch single total (#4143)", () => {
  it("an enumeration truncated before any .retire-* name still emits one unknown total and reads nothing", async () => {
    for (let i = 0; i < 4097; i++) fs.writeFileSync(path.join(root, `junk-${i}`), "x");
    let reads = 0;
    await run({ readRetainedSources: async () => { reads++; return { state: "read", paths: [] }; } });
    assert.equal(reads, 0);
    assert.equal(rows().length, 0);
    assert.equal(totals().length, 1);
    const t = totals()[0]!;
    assert.equal(t.parents_enumerated, 0);
    assert.equal(t.bytes_state, "unknown");
    assert.equal(t.enumeration_incomplete, true);
    assert.equal(t.retained_source_read, "not_attempted");
  });

  it("a complete listing with no .retire-* parents emits exactly one measured zero total and never reads", async () => {
    fs.mkdirSync(path.join(root, "repo-dir"));
    let reads = 0;
    await run({ readRetainedSources: async () => { reads++; return { state: "read", paths: [] }; } });
    assert.equal(reads, 0);
    assert.equal(rows().length, 0);
    assert.equal(totals().length, 1);
    const t = totals()[0]!;
    assert.equal(t.parents_enumerated, 0);
    assert.equal(t.outcomes_omitted, 0);
    assert.equal(t.kept, 0);
    assert.equal(t.bytes, 0);
    assert.equal(t.bytes_state, "measured");
    assert.equal(t.enumeration_incomplete, false);
    assert.equal(t.retained_source_read, "not_attempted");
  });

  it("a missing runner root emits no lines", async () => {
    fs.rmSync(root, { recursive: true });
    let reads = 0;
    await run({ readRetainedSources: async () => { reads++; return { state: "read", paths: [] }; } });
    assert.equal(reads, 0);
    assert.equal(rows().length + totals().length, 0);
  });
});

describe("retire scratch walk (#4143)", PROC_FD_REQUIRED, () => {
  it("two hard-linked symlink parents count their one inode once", async (t) => {
    const a = path.join(root, ".retire-a");
    fs.symlinkSync("t".repeat(300), a);
    fs.linkSync(a, path.join(root, ".retire-b"));
    if (fs.lstatSync(a).nlink !== 2 || fs.lstatSync(a).blocks === 0) { t.skip("filesystem cannot hard-link or allocate blocks for a symlink"); return; }
    await run();
    assert.equal(rows().length, 2);
    assert.equal(totals()[0]!.bytes, alloc(a), "the shared inode is counted once");
    assert.equal(totals()[0]!.bytes_state, "measured");
  });

  it("measures an exact allocated total with a hard link counted once", async () => {
    const p = mkParent(".retire-w", { "a/one": Buffer.alloc(100_000, 1), "b/c/two": Buffer.alloc(9000, 2) });
    fs.linkSync(path.join(p, "a/one"), path.join(p, "b/c/one-again"));
    fs.symlinkSync("../a", path.join(p, "b/link"));
    await observeOne(".retire-w");
    const w = (...s: string[]) => path.join(p, ...s);
    assert.equal(rows()[0]!.bytes, alloc(p, w("a"), w("b"), w("b", "c"), w("a", "one"), w("b", "c", "two"), w("b", "c", "one-again"), w("b", "link")));
    assert.equal(rows()[0]!.bytes_state, "measured");
  });

  it("a directory swapped for a symlink to an outside tree is changed and the outside is neither counted nor touched", async () => {
    const p = mkParent(".retire-s", { "d/inner": "x", "other": "y" });
    const outside = path.join(fx.dataDir, "outside");
    fs.mkdirSync(outside);
    fs.writeFileSync(path.join(outside, "big"), Buffer.alloc(4 * 1024 * 1024, 7));
    const outsideBefore = snapshot(outside);
    await run({
      beforeEntry: async (dir, name) => {
        if (dir !== p || name !== "d") return;
        await sleep(25);
        fs.rmSync(path.join(p, "d"), { recursive: true });
        fs.symlinkSync(outside, path.join(p, "d"));
      },
    });
    const r = rows()[0]!;
    assert.equal(r.bytes_state, "partial");
    assert.ok(r.partial_reasons.includes("changed"));
    assert.ok(r.bytes < 1024 * 1024, `outside bytes are not counted (${r.bytes})`);
    assert.deepEqual(snapshot(outside), outsideBefore);
  });

  it("a parent swapped between the listing and its open is never followed to the outside tree", async () => {
    const p = mkParent(".retire-t", { f: "x" });
    const outside = path.join(fx.dataDir, "outside");
    fs.mkdirSync(outside);
    fs.writeFileSync(path.join(outside, "big"), Buffer.alloc(4 * 1024 * 1024, 7));
    const outsideBefore = snapshot(outside);
    await run({
      beforeEntry: async (dir, name) => {
        if (dir !== root || name !== ".retire-t") return;
        fs.renameSync(p, path.join(fx.dataDir, "moved"));
        fs.symlinkSync(outside, p);
      },
    });
    const r = rows()[0]!;
    assert.equal(r.kind, "symlink");
    assert.ok(r.bytes < 1024 * 1024);
    assert.deepEqual(snapshot(outside), outsideBefore);
  });

  it("a directory swapped for a symlink between its lstat and its open is opened with O_NOFOLLOW and never resolves outside", async () => {
    const p = mkParent(".retire-o", { "d/inner": "x" });
    const outside = path.join(fx.dataDir, "outside");
    fs.mkdirSync(outside);
    fs.writeFileSync(path.join(outside, "big"), Buffer.alloc(1024 * 1024, 7));
    const outsideIno = fs.statSync(outside).ino;
    const opens: Array<{ target: string; flags: number }> = [];
    const resolvedInos: number[] = [];
    const realOpen = fsp.open.bind(fsp);
    mock.method(fsp, "open", async (target: string, flags?: number, mode?: number) => {
      const handle = await realOpen(target, flags, mode);
      opens.push({ target: String(target), flags: Number(flags) });
      resolvedInos.push((await handle.stat()).ino);
      return handle;
    });
    await run({
      readRetainedSources: async () => ({ state: "read", paths: [] }),
      beforeOpen: async (dir) => {
        if (dir !== path.join(p, "d")) return;
        fs.rmSync(path.join(p, "d"), { recursive: true });
        fs.symlinkSync(outside, path.join(p, "d"));
      },
    });
    assert.ok(opens.length >= 2, "the walk opened the root and the parent");
    for (const o of opens) assert.ok((o.flags & fs.constants.O_NOFOLLOW) !== 0, `open of ${o.target} lacks O_NOFOLLOW`);
    assert.ok(!resolvedInos.includes(outsideIno), "no walk open resolved to the outside directory");
    const r = rows()[0]!;
    assert.ok(r.partial_reasons.includes("changed"));
    assert.ok(r.bytes < 1024 * 1024);
  });

  it("an ancestor swapped after its open still measures the descendant through the original inode", async () => {
    const p = mkParent(".retire-u", { "a/b": Buffer.alloc(65_536, 3) });
    const outside = path.join(fx.dataDir, "outside");
    fs.mkdirSync(outside);
    fs.writeFileSync(path.join(outside, "b"), Buffer.alloc(4 * 1024 * 1024, 7));
    const outsideBefore = snapshot(outside);
    await run({
      beforeEntry: async (dir, name) => {
        if (dir !== path.join(p, "a") || name !== "b") return;
        await sleep(25);
        fs.renameSync(path.join(p, "a"), path.join(fx.dataDir, "aside"));
        fs.symlinkSync(outside, path.join(p, "a"));
      },
    });
    const r = rows()[0]!;
    assert.ok(r.bytes >= 65_536, `the original descendant was measured (${r.bytes})`);
    assert.ok(r.bytes < 1024 * 1024, "the swapped-in outside tree was not");
    assert.ok(r.partial_reasons.includes("changed"));
    assert.deepEqual(snapshot(outside), outsideBefore);
  });

  it("a file swapped for a populated directory is never measured with its descendants omitted", async () => {
    const p = mkParent(".retire-v", { f: "x", g: "y" });
    await run({
      beforeEntry: async (dir, name) => {
        if (dir !== p || name !== "f") return;
        await sleep(25);
        fs.rmSync(path.join(p, "f"));
        fs.mkdirSync(path.join(p, "f"));
        fs.writeFileSync(path.join(p, "f", "payload"), Buffer.alloc(1024 * 1024, 9));
      },
    });
    const r = rows()[0]!;
    assert.notEqual(r.bytes_state, "measured");
    assert.ok(r.partial_reasons.includes("changed"));
    assert.ok(r.bytes >= 1024 * 1024, "the new directory's contents were descended, not skipped");
  });

  it("an entry created during enumeration is changed", async () => {
    const p = mkParent(".retire-x", { a: "x", b: "y" });
    let done = false;
    await run({
      beforeEntry: async (dir) => {
        if (dir !== p || done) return;
        done = true;
        await sleep(25);
        fs.writeFileSync(path.join(p, "created"), "new");
      },
    });
    assert.ok(rows()[0]!.partial_reasons.includes("changed"));
    assert.equal(rows()[0]!.bytes_state, "partial");
    assert.equal(rows()[0]!.empty, false);
  });

  it("the entry cap gives a partial total", async () => {
    mkParent(".retire-y", Object.fromEntries(Array.from({ length: 10 }, (_, i) => [`f${i}`, "x"])));
    await run({ walkLimits: { maxEntries: 3 } });
    assert.equal(rows()[0]!.bytes_state, "partial");
    assert.deepEqual(rows()[0]!.partial_reasons, ["entry_cap"]);
    assert.equal(totals()[0]!.bytes_state, "partial");
    assert.deepEqual(totals()[0]!.partial_reasons, ["entry_cap"]);
  });
});

describe("retire scratch outcomes switch (#4143)", () => {
  it("off emits no outcome or total lines, an identical warning, and never calls the reader", async () => {
    mkParent(".retire-s", { f: "x" });
    let reads = 0;
    await run({ outcomes: false, readRetainedSources: async () => { reads++; return { state: "read", paths: [] }; } });
    assert.equal(reads, 0);
    assert.equal(rows().length + totals().length, 0, "no total either, even though a parent exists");
    const off = warnings();
    assert.equal(off.length, 1);
    await run({ readRetainedSources: async () => { reads++; return { state: "read", paths: [] }; } });
    assert.equal(reads, 1);
    assert.equal(rows().length, 1);
    assert.equal(totals().length, 1);
    assert.deepEqual(warnings()[1], off[0]);
    assert.deepEqual(await git.inspectRetireScratchAtStartup(OLD), { retained: 1 }, "the default call is the off path");
    assert.equal(rows().length, 1, "no new rows from the default call");
  });

  it("parses the activation switch", () => {
    for (const v of [undefined, "", "1"]) assert.equal(readRetireScratchOutcomesActivation({ UZI_RETIRE_SCRATCH_OUTCOMES: v }), true);
    assert.equal(readRetireScratchOutcomesActivation({}), true);
    assert.equal(readRetireScratchOutcomesActivation({ UZI_RETIRE_SCRATCH_OUTCOMES: "0" }), false);
    for (const v of ["2", "true", "yes", " 1", "01", "off"]) {
      assert.throws(() => readRetireScratchOutcomesActivation({ UZI_RETIRE_SCRATCH_OUTCOMES: v }),
        /UZI_RETIRE_SCRATCH_OUTCOMES must be unset, empty, 0 or 1/);
    }
  });
});
