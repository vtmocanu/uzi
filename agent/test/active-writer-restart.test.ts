// Issue #3153 (section 3b): `<dataDir>/runner` persists across a worker restart on the Docker lane,
// and a rootful daemon or a lingering old pod can keep writing into it. A SEPARATE process keeps
// appending to a file in (1) a preserved run directory, (2) the per-uid scratch directory and
// (3) an ancestor of the run directory while a fresh GitCache over the same data directory (the
// simulated restart) runs the startup inspection and the retained-recovery reads. Nothing may be
// deleted, the diagnostics must be emitted, and the active source must never read as missing,
// clean or quiescent. The shell-side scratch adoption under a live writer is pinned beside the
// other entrypoint tests (entrypoint-migration.test.ts).
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { spawn, type ChildProcess } from "node:child_process";
import { GitCache, retainedUndeletableCount } from "../src/git.js";
import { procfsTable, scanOnce, type ScanRequest } from "../src/run-quiescence.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, recordingLogger, testGitCacheOptions } from "./helpers.js";

const branch = "agent/issue-3153";
const key = "issue-3153";
const runId = "run-3153";
// A "process start" later than every fixture mtime, as in retire-scratch-startup.test.ts.
const LATER = Date.now() + 60_000;
// Appends a line every 10 ms to ./live.log in its own cwd until it is killed.
const WRITER = `
const fs = require("node:fs");
setInterval(() => fs.appendFileSync("live.log", "tick\\n"), 10);
`;

let fx: Fixture;
let lines: unknown[];
let cache: GitCache;
let bare: string;
let cloneDir: string;
const writers: ChildProcess[] = [];

function recreate(): GitCache {
  const rec = recordingLogger();
  lines = rec.lines;
  return new GitCache(fx.dataDir, rec.logger, undefined, testGitCacheOptions());
}

/** Start a separate node process whose cwd is `dir`; resolves once its log file has grown. */
async function startWriter(dir: string): Promise<{ pid: number; log: string }> {
  fs.mkdirSync(dir, { recursive: true });
  // A bare environment: a writer inheriting this process's UZI_* attempt markers would be
  // classified by the marker instead of by where it writes.
  const child = spawn(process.execPath, ["-e", WRITER], { cwd: dir, stdio: "ignore", env: { PATH: process.env.PATH ?? "/usr/bin:/bin" } });
  writers.push(child);
  const log = path.join(dir, "live.log");
  await grew(log, 0);
  return { pid: child.pid!, log };
}

/** Wait (at most ~5 s) until `file` is larger than `above` bytes; returns the new size. */
async function grew(file: string, above: number): Promise<number> {
  for (let i = 0; i < 250; i++) {
    const size = fs.existsSync(file) ? fs.statSync(file).size : 0;
    if (size > above) return size;
    await new Promise((r) => setTimeout(r, 20));
  }
  assert.fail(`${file} did not grow past ${above} bytes: the writer is not alive`);
}

beforeEach(async () => {
  fx = makeFixture();
  cache = recreate();
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await cache.createOrAttachRunnerClone(bare, 3153, noProofReseed, runId);
  cloneDir = clone.path;
  await cache.markRecoveryCapture(bare, cloneDir, branch, runId);
});

afterEach(() => {
  for (const child of writers.splice(0)) child.kill("SIGKILL");
  fx.cleanup();
});

describe("a separate process keeps writing across a simulated worker restart (#3153)", () => {
  it("preserves a run directory, its ancestor and the scratch directory, and never reads the active source as quiescent", async () => {
    const runnerRoot = path.join(fx.dataDir, "runner");
    const repoDir = path.dirname(cloneDir);
    const scratch = path.join(runnerRoot, ".tmp", "uzi-worker");
    const retireParent = path.join(runnerRoot, ".retire-live");
    const inRun = await startWriter(path.join(cloneDir, "out"));
    const inAncestor = await startWriter(path.join(repoDir, "ancestor-dir"));
    const inScratch = await startWriter(scratch);
    const inRetire = await startWriter(path.join(retireParent, "clone"));
    const active = [inRun, inAncestor, inScratch, inRetire];
    const sizes = active.map((w) => fs.statSync(w.log).size);
    const journalBefore = fs.readFileSync(path.join(bare, "config"), "utf8");
    const before = retainedUndeletableCount();

    const restarted = recreate(); // the fresh worker process: same dataDir, no memory of the predecessor
    const swept = await restarted.inspectRetireScratchAtStartup(LATER);
    assert.deepEqual(swept, { removed: 0, retained: 1 }, "the parent holding a live writer is retained, not removed");
    const diagnostic = lines.find((l) => (l as { msg?: string }).msg === "runner retire scratch retained") as
      { count: number; paths: string[]; incomplete: boolean } | undefined;
    assert.ok(diagnostic, "the retention diagnostic is emitted");
    assert.equal(diagnostic.count, 1);
    assert.deepEqual(diagnostic.paths, [".retire-live"]);

    // The retained-recovery reads find the journaled source: it is neither missing nor clean.
    const found = await restarted.discoverRetainedRecovery(fx.originPath, branch, key, runId);
    assert.equal(found?.journal.clonePath, cloneDir);
    const inventory = await restarted.readInventoryCloneHeads(bare, runId);
    assert.equal(inventory.kind, "verified");
    if (inventory.kind === "verified") assert.ok(inventory.clones.some((c) => c.clonePath === cloneDir));
    assert.equal(retainedUndeletableCount(), before, "no removal was even attempted");
    assert.equal(fs.readFileSync(path.join(bare, "config"), "utf8"), journalBefore, "the journal and ledger are not rewritten");
    assert.equal(lines.some((l) => /remov|delet|discard/i.test(String((l as { msg?: string }).msg))
      && (l as { level?: string }).level !== "debug"), false, JSON.stringify(lines));

    // Real procfs: the writer in the run directory is a survivor of a capture-mode scan, so a
    // quiescence proof over that path is not given. (selfPid is a pid that cannot exist, so the
    // writer is not skipped as the scanner's own descendant.)
    const request: ScanRequest = {
      mode: "capture", targetUid: process.getuid!(), targetKey: path.join(repoDir, key),
      targetPaths: [cloneDir], liveMarkers: [], liveRoots: [], workerNonce: "n3153",
    };
    const scan = scanOnce(request, procfsTable, 4_194_300);
    assert.equal(scan.tableError, undefined);
    assert.ok(scan.survivors.some((s) => s.pid === inRun.pid && s.reason === "unattributed_in_scope"),
      `the live writer in the run directory is a survivor: ${JSON.stringify(scan)}`);
    assert.deepEqual(scan.kill, [], "capture mode signals nothing");

    // Everything is still there and every writer is still alive and appending.
    for (const [i, w] of active.entries()) {
      assert.ok(await grew(w.log, sizes[i]!) > sizes[i]!, `${w.log} still grows after the restart handling`);
    }
    assert.ok(fs.existsSync(path.join(retireParent, "clone", "live.log")));
    assert.ok(fs.existsSync(path.join(cloneDir, ".git")));
    assert.ok(fs.existsSync(scratch));
  });
});
