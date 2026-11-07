import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { runDiskReclaimPass, type DiskReclaimDeps } from "../src/disk-reclaim.js";
import { reclaimStrandedRunHomes } from "../src/home-reclaim.js";
import { RunDiskSampler } from "../src/run-disk.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import { writeRetainedHomeMarker } from "../src/retained-homes.js";
import { runDirKey } from "../src/util.js";
import { nullLogger } from "./helpers.js";

// issue #3644 plan 3.12: the reclaimers enumerate `<runId>` and `<runId>.g<N>` with parseRunDirName, so a restore successor's
// directories follow their run's status and liveness, and a run with a retained-home marker (present, malformed, unreadable, or
// behind a directory error other than ENOENT) never has its LEGACY HOME or provision dir reclaimed: names, bytes, modes and
// mtimes of the predecessor's directories stay as they were.

const LINUX_ONLY = process.platform !== "linux" ? "descriptor-pinned removal requires Linux" : false;
let root: string;
let home: string;
let provision: string;
let run: string;
beforeEach(() => {
  root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-per-attempt-"));
  home = path.join(root, "agent-home");
  provision = path.join(root, "provision");
  fs.mkdirSync(home);
  fs.mkdirSync(provision);
  run = randomUUID();
});
afterEach(() => fs.rmSync(root, { recursive: true, force: true }));

/** names, bytes, modes and mtimes of a tree: a directory's byte identity. */
function digest(dir: string): string {
  const h = createHash("sha256");
  const walk = (d: string): void => {
    for (const name of fs.readdirSync(d).sort()) {
      const p = path.join(d, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(dir, p)}\0${st.mode}\0${st.mtimeMs}\0${st.size}\0`);
      if (st.isDirectory()) walk(p); else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  h.update(`${fs.lstatSync(dir).mode}\0${fs.lstatSync(dir).mtimeMs}`);
  walk(dir);
  return h.digest("hex");
}
function seed(dir: string, text = "bytes"): void {
  fs.mkdirSync(path.join(dir, ".claude", "projects"), { recursive: true });
  fs.writeFileSync(path.join(dir, ".claude", "projects", "s.jsonl"), text);
  fs.writeFileSync(path.join(dir, "note.txt"), text, { mode: 0o640 });
}
/** The predecessor's legacy HOME and provision dir, and the successor's `.g2` pair. */
function seedBoth(): { pHome: string; pProv: string; sHome: string; sProv: string } {
  const pHome = path.join(home, run), pProv = path.join(provision, run);
  const sHome = path.join(home, runDirKey(run, 2)), sProv = path.join(provision, runDirKey(run, 2));
  for (const d of [pHome, pProv]) seed(d, "predecessor");
  for (const d of [sHome, sProv]) seed(d, "successor");
  return { pHome, pProv, sHome, sProv };
}
const marker = () => writeRetainedHomeMarker(root, { version: 1, runId: run, predecessorGeneration: 1, predecessorAttemptId: "a1", barePath: "/data/git/x.git" });
const plantMarker = (content: string): void => {
  fs.mkdirSync(path.join(root, "retained-homes"), { mode: 0o700 });
  fs.writeFileSync(path.join(root, "retained-homes", `${run}.json`), content, { mode: 0o600 });
};
const passDeps = (over: Partial<DiskReclaimDeps> = {}): DiskReclaimDeps => ({
  homeRoot: home, provisionRoot: provision, statusOf: async () => "failed", isRunLive: () => false, locks: new RunDiskLocks(),
  log: nullLogger(), processesClear: async () => true, ...over,
});

describe("runDiskReclaimPass: markers and .g<N> directories", () => {
  it("a terminal run with a marker keeps its legacy HOME and provision dir byte for byte; the successor's .g2 dirs are removed", { skip: LINUX_ONLY }, async () => {
    const t = seedBoth();
    await marker();
    const before = [digest(t.pHome), digest(t.pProv)];
    const s = await runDiskReclaimPass(passDeps());
    assert.deepEqual([digest(t.pHome), digest(t.pProv)], before);
    assert.equal(fs.existsSync(t.sHome), false, "S's HOME follows the run's terminal status");
    assert.equal(fs.existsSync(t.sProv), false, "S's provision dir follows it too");
    assert.equal(s.skippedRetainedHome, 1);
    assert.equal(s.terminalHomesRemoved, 1);
    assert.equal(s.provisionDirsRemoved, 1);
    assert.equal(s.failed, 0);
  });

  for (const [name, plant] of [
    ["a malformed marker", () => plantMarker("{nope")],
    ["a marker of another bare shape (extra field)", () => plantMarker(JSON.stringify({ x: 1 }))],
    ["a 0666 marker", () => { plantMarker("{}"); fs.chmodSync(path.join(root, "retained-homes", `${run}.json`), 0o666); }],
    ["a retained-homes path that is a regular file (directory error other than ENOENT)", () => fs.writeFileSync(path.join(root, "retained-homes"), "x")],
  ] as const) {
    it(`${name} protects the legacy directories`, { skip: LINUX_ONLY }, async () => {
      const t = seedBoth();
      plant();
      const before = [digest(t.pHome), digest(t.pProv)];
      await runDiskReclaimPass(passDeps());
      assert.deepEqual([digest(t.pHome), digest(t.pProv)], before);
    });
  }

  it("a marker hook that throws counts as present", { skip: LINUX_ONLY }, async () => {
    const t = seedBoth();
    const before = [digest(t.pHome), digest(t.pProv)];
    await runDiskReclaimPass(passDeps({ hasRetainedHomeMarker: async () => { throw new Error("EACCES"); } }));
    assert.deepEqual([digest(t.pHome), digest(t.pProv)], before);
  });

  it("a marker protects the legacy dirs of a parked run too: no cache drop, nothing touched", { skip: LINUX_ONLY }, async () => {
    const t = seedBoth();
    fs.mkdirSync(path.join(t.pHome, ".cache", "go-build"), { recursive: true });
    fs.writeFileSync(path.join(t.pHome, ".cache", "go-build", "obj"), "x");
    await marker();
    const before = digest(t.pHome);
    await runDiskReclaimPass(passDeps({ statusOf: async () => "limit_wait" }));
    assert.equal(digest(t.pHome), before);
  });

  it("without a marker an ordinary run is reclaimed exactly as before: both legacy dirs go", { skip: LINUX_ONLY }, async () => {
    const t = seedBoth();
    const s = await runDiskReclaimPass(passDeps());
    for (const p of Object.values(t)) assert.equal(fs.existsSync(p), false, p);
    assert.equal(s.skippedRetainedHome, 0);
    assert.equal(s.terminalHomesRemoved, 2);
    assert.equal(s.provisionDirsRemoved, 2);
  });

  it("a .g2 dir follows its run's liveness and status: live, or not terminal, keeps it", { skip: LINUX_ONLY }, async () => {
    const t = seedBoth();
    await marker();
    const lookups: string[] = [];
    await runDiskReclaimPass(passDeps({ isRunLive: (id) => id === run, statusOf: async (id) => { lookups.push(id); return "failed"; } }));
    assert.equal(fs.existsSync(t.sHome), true, "live run");
    assert.equal(fs.existsSync(t.sProv), true);
    await runDiskReclaimPass(passDeps({ statusOf: async (id) => { lookups.push(id); return "running"; } }));
    assert.equal(fs.existsSync(t.sHome), true, "running run");
    assert.ok(lookups.every((id) => id === run), "the status lookup asks for the run id, never for the directory name");
    await runDiskReclaimPass(passDeps());
    assert.equal(fs.existsSync(t.sHome), false, "terminal run");
  });

  it("a .g<N>-looking name that is not a run directory is never listed", { skip: LINUX_ONLY }, async () => {
    for (const bad of [`${run}.g0`, `${run}.g01`, `${run}.gx`]) seed(path.join(home, bad));
    const lookups: string[] = [];
    await runDiskReclaimPass(passDeps({ statusOf: async (id) => { lookups.push(id); return "failed"; } }));
    assert.deepEqual(lookups, []);
    for (const bad of [`${run}.g0`, `${run}.g01`, `${run}.gx`]) assert.equal(fs.existsSync(path.join(home, bad)), true);
  });
});

describe("reclaimStrandedRunHomes: markers and .g<N> HOMEs", () => {
  const old = (dir: string): void => { const t = new Date(Date.now() - 24 * 3_600_000); fs.utimesSync(dir, t, t); };
  it("keeps the legacy HOME of a run with a marker and reclaims its terminal .g2 HOME", async () => {
    const t = seedBoth();
    old(t.pHome); old(t.sHome);
    await marker();
    const before = digest(t.pHome);
    const asked: string[] = [];
    const s = await reclaimStrandedRunHomes(home, async (id) => { asked.push(id); return "failed"; }, nullLogger());
    assert.equal(digest(t.pHome), before);
    assert.equal(fs.existsSync(t.sHome), false);
    assert.equal(s.skippedRetainedHome, 1);
    assert.equal(s.removed, 1);
    assert.ok(asked.every((id) => id === run));
  });
  it("a malformed marker, an unreadable marker and a throwing hook each keep the legacy HOME", async () => {
    const t = seedBoth();
    old(t.pHome);
    plantMarker("{");
    const before = digest(t.pHome);
    await reclaimStrandedRunHomes(home, async () => "failed", nullLogger());
    assert.equal(digest(t.pHome), before);
    await reclaimStrandedRunHomes(home, async () => "failed", nullLogger(), { hasRetainedHomeMarker: async () => { throw new Error("EIO"); } });
    assert.equal(digest(t.pHome), before);
  });
  it("without a marker the legacy HOME is reclaimed as before; a non-terminal .g2 HOME is kept", async () => {
    const t = seedBoth();
    old(t.pHome); old(t.sHome);
    const s = await reclaimStrandedRunHomes(home, async () => "running", nullLogger());
    assert.equal(s.removed, 0);
    await reclaimStrandedRunHomes(home, async () => "failed", nullLogger());
    assert.equal(fs.existsSync(t.pHome), false);
    assert.equal(fs.existsSync(t.sHome), false);
  });
});

describe("RunDiskSampler: .g<N> HOMEs follow their run and report one entry per run", () => {
  const sampler = (over: Partial<ConstructorParameters<typeof RunDiskSampler>[0]> = {}) => new RunDiskSampler({
    homeRoot: home, intervalMs: 60_000, isRunLive: () => false, statusOf: async () => "limit_wait", log: nullLogger(),
    measure: async (dir) => ({ homeBytes: dir.endsWith(".g2") ? 40 : 100, cacheBytes: dir.endsWith(".g2") ? 4 : 10, entries: 1, truncated: false }), ...over,
  });
  it("sums the legacy and the .g2 HOME into one entry under the run id", async () => {
    seedBoth();
    const entries = await sampler().sample();
    assert.deepEqual(entries.map(({ sampled_at: _a, ...e }) => e), [{ run_id: run, home_bytes: 140, cache_bytes: 14 }]);
  });
  it("a terminal run's .g2 HOME is not measured", async () => {
    seedBoth();
    assert.deepEqual(await sampler({ statusOf: async () => "failed" }).sample(), []);
  });
});
