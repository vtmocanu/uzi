// Issue #3543 opt-in measurement of plan cross-check base discovery (not a *.test.ts, so
// `npm test` and every gate never run it). Drive it through
// e2e/planning-base-discovery-measure/run.sh, which runs each job in a memory-limited container:
//
//   UZI_PLANNING_DISCOVERY_MEASURE=1 node --import tsx test/planning-base-discovery.measure.ts job <job.json>
//
// Job kinds: `generate` builds one deterministic fixture, `measure` runs one capture through the
// production GitCache.capturePlanningDiff path, `report` renders the results table. The
// measured helper is the unchanged PLANNING_CAPTURE_HELPER; instrumented modes only PREPEND a
// metering preamble to its `-e` program (the technique git-planning-diff.test.ts's meter uses).
// Nothing here changes a production bound. See the README beside run.sh for metric definitions.

import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { GitCache, classifyPlanningCaptureError, gitEnv } from "../src/git.js";
import type { BoundaryProcessHandle, BoundaryProcessRequest } from "../src/harness.js";
import type { Logger } from "../src/log.js";
import { TickSpawner } from "../src/tick-spawner.js";

if (process.env.UZI_PLANNING_DISCOVERY_MEASURE !== "1") {
  console.error("refusing to run: set UZI_PLANNING_DISCOVERY_MEASURE=1 (opt-in measurement harness, issue #3543)");
  process.exit(2);
}

// ---------------------------------------------------------------------------------------------
// Fixture generation (deterministic: pinned identities and dates, plumbing-built objects).

const PINNED_ENV: NodeJS.ProcessEnv = {
  ...gitEnv(),
  GIT_AUTHOR_NAME: "Fixture", GIT_AUTHOR_EMAIL: "fixture@example.invalid", GIT_AUTHOR_DATE: "2026-01-01T00:00:00Z",
  GIT_COMMITTER_NAME: "Fixture", GIT_COMMITTER_EMAIL: "fixture@example.invalid", GIT_COMMITTER_DATE: "2026-01-01T00:00:00Z",
};

function git(cwd: string, args: string[], input?: string): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: PINNED_ENV, input, encoding: "utf8", maxBuffer: 256 * 1024 * 1024, stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}

interface Manifest {
  spec: string;
  kind: string;
  description: string;
  commit: string;
  tree: string;
  // Decimal strings: 2^65 - 1 overflows a JS number.
  expectedTreeOccurrences: string;
  expectedLeafOccurrences: string;
  checkout: boolean;
  generationMs: number;
}

function layout(dir: string): { root: string; clone: string; bare: string } {
  const root = path.join(dir, "root");
  return { root, clone: path.join(root, "runner", "repo", "issue-1"), bare: path.join(root, "repos", "repo.git") };
}

// Build a tree DAG directly in `bare` from the bottom up and return its root tree id.
function mktree(bare: string, lines: string[]): string {
  return git(bare, ["mktree"], lines.join("\n") + "\n");
}

function sharedDag(bare: string, spec: string): { tree: string; trees: bigint; leaves: bigint; description: string } {
  const blob = git(bare, ["hash-object", "-w", "--stdin"], "leaf\n");
  let m = /^fanout2-d(\d+)$/.exec(spec);
  if (m) {
    const depth = Number(m[1]);
    let t = mktree(bare, [`100644 blob ${blob}\tf`]);
    for (let k = depth - 1; k >= 0; k--) t = mktree(bare, [`040000 tree ${t}\ta`, `040000 tree ${t}\tb`]);
    return { tree: t, trees: (1n << BigInt(depth + 1)) - 1n, leaves: 1n << BigInt(depth),
      description: `binary shared DAG, root depth 0, deepest tree at depth ${depth}, ${depth + 1} distinct trees` };
  }
  m = /^chain-d(\d+)$/.exec(spec);
  if (m) {
    const depth = Number(m[1]);
    let t = mktree(bare, [`100644 blob ${blob}\tf`]);
    for (let k = depth - 1; k >= 0; k--) t = mktree(bare, [`040000 tree ${t}\ta`]);
    return { tree: t, trees: BigInt(depth + 1), leaves: 1n,
      description: `linear chain, deepest tree at depth ${depth}` };
  }
  m = /^wide-(\d+)x(\d+)$/.exec(spec);
  if (m) {
    const fan = Number(m[1]), width = Number(m[2]);
    const names = Array.from({ length: width }, (_, i) => `f${String(i).padStart(6, "0")}`);
    const leaf = mktree(bare, names.map((n) => `100644 blob ${blob}\t${n}`));
    const dirs = Array.from({ length: fan }, (_, j) => `d${String(j).padStart(6, "0")}`);
    const t = mktree(bare, dirs.map((d) => `040000 tree ${leaf}\t${d}`));
    return { tree: t, trees: BigInt(fan) + 1n, leaves: BigInt(fan) * BigInt(width),
      description: `EXTRA (beyond spec): one ${width}-entry leaf tree referenced ${fan} times` };
  }
  throw new Error(`unknown fixture spec ${spec}`);
}

function syntheticSize(i: number): number {
  return Number((BigInt(i) * 2654435761n) % 4096n);
}

function generate(spec: string, dir: string, inputs: string): Manifest {
  const started = Date.now();
  fs.rmSync(dir, { recursive: true, force: true });
  const { clone, bare } = layout(dir);
  fs.mkdirSync(path.dirname(clone), { recursive: true });
  fs.mkdirSync(path.dirname(bare), { recursive: true });
  let tree: string, trees: bigint, leaves: bigint, description: string, checkout: boolean;
  if (spec === "repo" || /^synthetic-\d+$/.test(spec)) {
    const seed = path.join(dir, "seed");
    fs.mkdirSync(seed);
    git(seed, ["init", "--quiet", "--template="]);
    let expectedTree: string | undefined;
    if (spec === "repo") {
      execFileSync("tar", ["-xf", path.join(inputs, "repo.tar"), "-C", seed]);
      expectedTree = fs.readFileSync(path.join(inputs, "repo.tree"), "utf8").trim();
      description = `this repository at ${fs.readFileSync(path.join(inputs, "repo.sha"), "utf8").trim()}`;
    } else {
      const n = Number(spec.slice("synthetic-".length));
      for (let i = 0; i < n; i++) {
        const d = path.join(seed, `d${String(Math.floor(i / 100)).padStart(5, "0")}`);
        if (i % 100 === 0) fs.mkdirSync(d);
        const size = syntheticSize(i);
        const unit = `${i}\n`;
        fs.writeFileSync(path.join(d, `f${String(i % 100).padStart(2, "0")}`), unit.repeat(Math.ceil(size / unit.length) || 0).slice(0, size));
      }
      description = `${n} unique files, 100 per directory, sizes (i*2654435761) mod 4096 bytes`;
    }
    git(seed, ["-c", "core.autocrlf=false", "add", "-A", "-f"]);
    git(seed, ["-c", "commit.gpgsign=false", "commit", "--quiet", "--no-verify", "-m", "base"]);
    tree = git(seed, ["rev-parse", "HEAD^{tree}"]);
    if (expectedTree !== undefined && tree !== expectedTree) {
      throw new Error(`repo fixture tree ${tree} differs from the source tree ${expectedTree}`);
    }
    // D = trees, N = blobs or links (no gitlinks are accepted by the helper).
    const listing = git(seed, ["ls-tree", "-r", "-t", "HEAD"]).split("\n").filter(Boolean);
    trees = BigInt(listing.filter((l) => l.split(" ")[1] === "tree").length) + 1n;
    leaves = BigInt(listing.length) - (trees - 1n);
    execFileSync("git", ["clone", "--quiet", "--bare", "--no-local", seed, bare], { env: PINNED_ENV, stdio: "pipe" });
    fs.rmSync(seed, { recursive: true, force: true });
    checkout = true;
  } else {
    git(path.dirname(bare), ["init", "--quiet", "--bare", "--template=", bare]);
    ({ tree, trees, leaves, description } = sharedDag(bare, spec));
    const commit = git(bare, ["commit-tree", "-m", "base", tree]);
    git(bare, ["update-ref", "refs/heads/main", commit]);
    git(bare, ["symbolic-ref", "HEAD", "refs/heads/main"]);
    checkout = false;
  }
  git(bare, ["repack", "-a", "-d", "-q"]);
  // Adversarial bases are cloned without a checkout: the exploded working tree cannot exist on
  // disk, and discovery reads only .git and the bare's objects (see the README).
  execFileSync("git", ["clone", "--quiet", "--no-local", ...(checkout ? [] : ["--no-checkout"]), bare, clone],
    { env: PINNED_ENV, stdio: "pipe" });
  const commit = git(clone, ["rev-parse", "HEAD"]);
  const manifest: Manifest = {
    spec, kind: spec.replace(/-.*$/, ""), description, commit, tree,
    expectedTreeOccurrences: trees.toString(), expectedLeafOccurrences: leaves.toString(),
    checkout, generationMs: Date.now() - started,
  };
  fs.writeFileSync(path.join(dir, "manifest.json"), JSON.stringify(manifest, null, 2));
  return manifest;
}

// ---------------------------------------------------------------------------------------------
// Helper instrumentation preamble. Rules: prefixed identifiers only (it shares top-level scope
// with the helper's own consts), never write stdout/stderr (the classifier trusts the first
// stderr line), swallow its own write failures, and unref its timer (the helper's success path
// exits only when its event loop drains).

function preamble(metricsPath: string, injectDeadlineAfterSizing: boolean): string {
  return `
const pbdFs = require("node:fs");
const pbdCp = require("node:child_process");
const pbdNow = Date.now;
const pbdT0 = pbdNow();
const pbdPath = ${JSON.stringify(metricsPath)};
const pbdM = { t0: pbdT0, treeOccurrences: 0, treeEntries: 0, commitBytes: 0, treeBytes: 0,
  objectCheckBytes: 0, sizingBytes: 0, gitSpawns: 0, readSyncBytes: 0, discoveryGitLastCloseMs: null,
  sizingCloseMs: null, boundaryMs: null, boundaryMaxRssKiB: null, boundarySnapshot: null,
  firstPostBoundarySpawnMs: null, maxRssKiB: 0, snapshotMs: 0, injected: false };
let pbdWriting = false;
function pbdSnap() {
  if (pbdWriting) return;
  pbdWriting = true;
  try {
    pbdM.maxRssKiB = process.resourceUsage().maxRSS;
    pbdM.snapshotMs = pbdNow() - pbdT0;
    pbdFs.writeFileSync(pbdPath + ".tmp", JSON.stringify(pbdM));
    pbdFs.renameSync(pbdPath + ".tmp", pbdPath);
  } catch {} finally { pbdWriting = false; }
}
setInterval(pbdSnap, 100).unref();
process.on("exit", pbdSnap);
function pbdEntries(buf) {
  let at = 0, n = 0;
  while (at < buf.length) { const end = buf.indexOf(0, at); if (end < 0) break; at = end + 21; n++; }
  return n;
}
const pbdOpen = pbdFs.openSync;
pbdFs.openSync = function(p, ...rest) {
  if (!pbdWriting && pbdM.boundaryMs === null && typeof p === "string" && p.endsWith("/index")) {
    pbdM.boundaryMs = pbdNow() - pbdT0;
    pbdM.boundaryMaxRssKiB = process.resourceUsage().maxRSS;
    pbdM.boundarySnapshot = { treeOccurrences: pbdM.treeOccurrences, treeEntries: pbdM.treeEntries,
      commitBytes: pbdM.commitBytes, treeBytes: pbdM.treeBytes, objectCheckBytes: pbdM.objectCheckBytes,
      sizingBytes: pbdM.sizingBytes, gitSpawns: pbdM.gitSpawns };
    pbdSnap();
  }
  return pbdOpen.call(this, p, ...rest);
};
const pbdReadSync = pbdFs.readSync;
pbdFs.readSync = function(...args) {
  const n = pbdReadSync.apply(this, args);
  if (!pbdWriting && typeof n === "number") pbdM.readSyncBytes += n;
  return n;
};
const pbdSpawn = pbdCp.spawn;
pbdCp.spawn = function(...args) {
  const p = pbdSpawn.apply(this, args);
  const argv = Array.isArray(args[1]) ? args[1] : [];
  pbdM.gitSpawns++;
  const discovering = pbdM.boundaryMs === null;
  if (!discovering && pbdM.firstPostBoundarySpawnMs === null) { pbdM.firstPostBoundarySpawnMs = pbdNow() - pbdT0; pbdSnap(); }
  const at = argv.indexOf("cat-file");
  if (discovering && at >= 0) {
    const sub = argv[at + 1];
    const sizing = sub === "--batch-check=%(objectname) %(objecttype) %(objectsize)";
    const kind = sub === "tree" ? "tree" : sub === "commit" ? "commit" : sizing ? "sizing" : "check";
    if (kind === "tree") pbdM.treeOccurrences++;
    const chunks = [];
    const read = p.stdout.read;
    p.stdout.read = function(...readArgs) {
      const b = read.apply(this, readArgs);
      if (Buffer.isBuffer(b)) {
        if (kind === "tree") { pbdM.treeBytes += b.length; chunks.push(b); }
        else if (kind === "commit") pbdM.commitBytes += b.length;
        else if (kind === "sizing") pbdM.sizingBytes += b.length;
        else pbdM.objectCheckBytes += b.length;
      }
      return b;
    };
    // Registered before the helper's own close listener, so it runs first.
    p.on("close", () => {
      if (kind === "tree") { pbdM.treeEntries += pbdEntries(Buffer.concat(chunks)); chunks.length = 0; }
      pbdM.discoveryGitLastCloseMs = pbdNow() - pbdT0;
      if (kind === "sizing") {
        pbdM.sizingCloseMs = pbdM.discoveryGitLastCloseMs;
        ${injectDeadlineAfterSizing ? `// Smoke-only fault: expire the helper deadline after the sizing batch closes and
        // before the index open, so maybeYield()'s check() refuses inside that window.
        pbdM.injected = true;
        Date.now = () => pbdNow() + 3600000;` : ""}
        pbdSnap();
      }
    });
  }
  return p;
};
`;
}

// ---------------------------------------------------------------------------------------------
// Parent-side sampling: helper process tree RSS from /proc and the container cgroup.

function readText(p: string): string | undefined {
  try { return fs.readFileSync(p, "utf8"); } catch { return undefined; }
}
function cgroupNumber(name: string): number | null {
  const v = readText(`/sys/fs/cgroup/${name}`);
  return v === undefined || !/^\d+\s*$/.test(v) ? null : Number(v.trim());
}
function oomKills(): number | null {
  const v = readText("/sys/fs/cgroup/memory.events");
  const m = v === undefined ? null : /^oom_kill (\d+)$/m.exec(v);
  return m ? Number(m[1]) : null;
}
function treeRssBytes(rootPid: number): number {
  const parent = new Map<number, number>(), rss = new Map<number, number>();
  let entries: string[] = [];
  try { entries = fs.readdirSync("/proc"); } catch { return 0; }
  for (const e of entries) {
    if (!/^\d+$/.test(e)) continue;
    const stat = readText(`/proc/${e}/stat`);
    const status = readText(`/proc/${e}/status`);
    if (!stat || !status) continue;
    const after = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
    parent.set(Number(e), Number(after[1]));
    const m = /^VmRSS:\s+(\d+) kB$/m.exec(status);
    rss.set(Number(e), m ? Number(m[1]) * 1024 : 0);
  }
  if (!rss.has(rootPid)) return 0;
  let total = 0;
  for (const pid of rss.keys()) {
    let p: number | undefined = pid;
    for (let hops = 0; p !== undefined && p > 0 && hops < 64; hops++) {
      if (p === rootPid) { total += rss.get(pid) ?? 0; break; }
      p = parent.get(p);
    }
  }
  return total;
}

const nullLog: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child() { return nullLog; },
};

interface MeasureJob {
  kind: "measure";
  label: string;
  fixture: string;
  mode: "full" | "control" | "cancel";
  out: string;
  tmp: string;
  injectDeadlineAfterSizing?: boolean;
  cancelAfterTreeOccurrences?: number;
}

type HelperMetrics = Record<string, unknown> & {
  treeOccurrences?: number; boundaryMs?: number | null; firstPostBoundarySpawnMs?: number | null;
};

function readJson<T>(p: string): T | undefined {
  try { return JSON.parse(fs.readFileSync(p, "utf8")) as T; } catch { return undefined; }
}
function writeJson(p: string, value: unknown): void {
  fs.writeFileSync(p + ".tmp", JSON.stringify(value, null, 2));
  fs.renameSync(p + ".tmp", p);
}

async function measure(job: MeasureJob): Promise<void> {
  const manifest = readJson<Manifest>(path.join(job.fixture, "manifest.json"));
  if (!manifest) throw new Error(`fixture ${job.fixture} has no manifest`);
  const { root, clone, bare } = layout(job.fixture);
  fs.mkdirSync(job.out, { recursive: true });
  const helperPath = path.join(job.out, `${job.label}.helper.json`);
  const livePath = path.join(job.out, `${job.label}.live.json`);
  const resultPath = path.join(job.out, `${job.label}.json`);
  for (const p of [helperPath, livePath, resultPath]) fs.rmSync(p, { force: true });
  // The helper places its temp tree under TMPDIR (readBoundedPlanningOutput passes runnerTmpdir()).
  fs.rmSync(job.tmp, { recursive: true, force: true });
  fs.mkdirSync(job.tmp, { recursive: true, mode: 0o700 });
  process.env.TMPDIR = job.tmp;
  delete process.env.UZI_RUNNER_TMPDIR;
  const instrumented = job.mode !== "control";

  const live = {
    label: job.label, startedAt: Date.now(), cgroupCurrentAtStart: cgroupNumber("memory.current"),
    oomKillsAtStart: oomKills(), helperPid: null as number | null, treeRssPeak: 0, treeRssDiscoveryPeak: 0,
    cgroupPeakAtBoundary: null as number | null, lastHelper: undefined as HelperMetrics | undefined,
    spawnTmpdir: null as string | null, signals: [] as string[], elapsedMs: 0,
  };
  let boundarySeen = false;
  const sample = (): void => {
    if (live.helperPid !== null) {
      const rss = treeRssBytes(live.helperPid);
      live.treeRssPeak = Math.max(live.treeRssPeak, rss);
      if (!boundarySeen) live.treeRssDiscoveryPeak = Math.max(live.treeRssDiscoveryPeak, rss);
    }
  };
  const sampler = setInterval(sample, 10);
  let lastPersist = 0;
  const ac = new AbortController();
  // Held in an object: TypeScript does not see closure assignments to a local `let`.
  const st: { cancel?: { trigger: string; at: number }; completion?: { code: number } | { rejected: string }; outcomeAt?: number } = {};
  const watcher = setInterval(() => {
    live.elapsedMs = Date.now() - live.startedAt;
    if (instrumented) {
      const h = readJson<HelperMetrics>(helperPath);
      if (h) live.lastHelper = h;
      if (h && h.boundaryMs !== null && h.boundaryMs !== undefined && !boundarySeen) {
        boundarySeen = true;
        live.cgroupPeakAtBoundary = cgroupNumber("memory.peak");
      }
      if (job.mode === "cancel" && !st.cancel && st.outcomeAt === undefined && h) {
        const n = job.cancelAfterTreeOccurrences ?? 50;
        if (!boundarySeen && (h.treeOccurrences ?? 0) >= n) st.cancel = { trigger: `mid_discovery:treeOccurrences>=${n}`, at: Date.now() };
        else if (boundarySeen && h.firstPostBoundarySpawnMs !== null && h.firstPostBoundarySpawnMs !== undefined) {
          st.cancel = { trigger: "post_boundary:first_capture_spawn", at: Date.now() };
        }
        if (st.cancel) ac.abort();
      }
    }
    if (Date.now() - lastPersist >= 100) {
      lastPersist = Date.now();
      try { writeJson(livePath, live); } catch { /* best effort */ }
    }
  }, 10);

  // Production envelope (runner.ts captureCheckedPlanningDiff): 60 s scope, combined signal,
  // default-grace TickSpawner, scope.abort() in finally, then settled()/survivors().
  const scope = new AbortController();
  const ownerSignal = AbortSignal.any([ac.signal, scope.signal]);
  const scopeTimer = setTimeout(() => scope.abort(), 60_000);
  const spawner = new TickSpawner({
    signal: ownerSignal, log: nullLog,
    // Observation only: record the pid and delivered signals; returning undefined keeps real delivery.
    hooks: {
      onSpawn: (pid) => { live.helperPid ??= pid; },
      signalGroup: (_pgid, sig) => { live.signals.push(`${sig}@${Date.now() - live.startedAt}ms`); return undefined; },
    },
  });
  const wrapped = async (request: BoundaryProcessRequest): Promise<BoundaryProcessHandle> => {
    live.spawnTmpdir = request.env.TMPDIR ?? null;
    let req = request;
    if (instrumented) {
      const argv = [...request.argv];
      if (argv[1] !== "-e" || typeof argv[2] !== "string") throw new Error("unexpected planning helper argv");
      argv[2] = preamble(helperPath, job.injectDeadlineAfterSizing === true) + argv[2];
      req = { ...request, argv };
    }
    const handle = await spawner.spawn(req);
    handle.completed.then((c) => { st.completion = { code: c.code }; },
      (e: unknown) => { st.completion = { rejected: String((e as Error)?.message ?? e).slice(0, 200) }; });
    return handle;
  };
  const cache = new GitCache(root, nullLog);
  let outcome: Record<string, unknown>;
  let abortToSettledMs: number | null = null;
  const started = Date.now();
  try {
    const bytes = await cache.withBoundaryProcessSpawner(wrapped, ownerSignal,
      () => cache.capturePlanningDiff(clone, manifest.commit, bare));
    st.outcomeAt = Date.now();
    outcome = { kind: "completed", patchBytes: bytes.length };
  } catch (error) {
    const message = String((error as Error)?.message ?? error);
    st.outcomeAt = Date.now();
    const c = classifyPlanningCaptureError(message);
    // `cancelled` only when the abort landed before the capture's outcome.
    const cancelled = st.cancel !== undefined && st.cancel.at <= st.outcomeAt;
    outcome = { kind: cancelled ? "cancelled" : "refused", refusal: c.refusal, token: c.diagnostic, message: message.slice(0, 300) };
  } finally {
    clearTimeout(scopeTimer);
    scope.abort();
  }
  const captureMs = Date.now() - started;
  await spawner.settled();
  const cancel = st.cancel;
  if (cancel) abortToSettledMs = Date.now() - cancel.at;
  clearInterval(watcher);
  sample();
  clearInterval(sampler);
  // A completion handler may still be pending one tick after settlement.
  await new Promise((r) => setImmediate(r));
  const helper = instrumented ? readJson<HelperMetrics>(helperPath) : undefined;
  // Scan the TMPDIR the helper was actually spawned with; flag any mismatch with the job's dir.
  const scanned = live.spawnTmpdir ?? job.tmp;
  const tmpdirMatches = live.spawnTmpdir === job.tmp;
  const leftover = fs.readdirSync(scanned).filter((n) => n.startsWith("uzi-planning-"));
  const survivors = spawner.survivors();
  const oomDelta = (() => { const now = oomKills(); return now === null || live.oomKillsAtStart === null ? null : now - live.oomKillsAtStart; })();
  if (oomDelta !== null && oomDelta > 0) outcome = { ...outcome, kind: "oom", oomKillDelta: oomDelta };
  // A helper signal death that no envelope signal explains (code 128 with nothing sent, no OOM
  // evidence, no cancel) is not a refusal.
  else if (outcome.kind === "refused" && st.completion && "code" in st.completion && st.completion.code === 128
      && live.signals.length === 0) outcome = { ...outcome, kind: "helper_killed_unknown" };
  const result = {
    label: job.label, spec: manifest.spec, fixture: manifest, mode: job.mode,
    memLimit: process.env.MEASURE_MEM_LIMIT ?? null, cpus: process.env.MEASURE_CPUS ?? null,
    node: process.version, gitVersion: (() => { try { return execFileSync("git", ["--version"], { encoding: "utf8" }).trim(); } catch { return null; } })(),
    captureMs, outcome,
    discovery: instrumented ? {
      completed: helper?.boundaryMs !== null && helper?.boundaryMs !== undefined,
      ms: helper?.boundaryMs ?? null,
      gitLastCloseMs: helper?.discoveryGitLastCloseMs ?? null,
      sizingCloseMs: helper?.sizingCloseMs ?? null,
      injected: helper?.injected ?? false,
      atBoundary: helper?.boundarySnapshot ?? null,
    } : null,
    helper: helper ?? null,
    helperMaxRssKiB: instrumented ? { discovery: helper?.boundaryMaxRssKiB ?? null, final: helper?.maxRssKiB ?? null } : null,
    treeRss: { discoveryPeakBytes: instrumented ? live.treeRssDiscoveryPeak : null, peakBytes: live.treeRssPeak, sampledEveryMs: 10 },
    cgroup: { currentAtStart: live.cgroupCurrentAtStart, peakAtBoundary: live.cgroupPeakAtBoundary,
      peakAtEnd: cgroupNumber("memory.peak"), oomKillDelta: oomDelta },
    helperCompletion: st.completion ?? null,
    signals: live.signals,
    survivors,
    spawnTmpdir: live.spawnTmpdir,
    tmpdirMatches,
    leftoverTemp: leftover,
    cancel: cancel ? {
      trigger: cancel.trigger, abortToSettledMs, graceMs: 2000,
      landedBeforeOutcome: st.outcomeAt === undefined || cancel.at <= st.outcomeAt,
      cleanupOk: (st.outcomeAt === undefined || cancel.at <= st.outcomeAt) && abortToSettledMs !== null
        && abortToSettledMs <= 2000 && survivors.length === 0 && leftover.length === 0 && tmpdirMatches,
    } : null,
  };
  writeJson(resultPath, result);
  fs.rmSync(job.tmp, { recursive: true, force: true });
}

// ---------------------------------------------------------------------------------------------
// Report: one Markdown table over every job, merging partial (killed) records as lower bounds.

const MiB = (b: unknown): string => typeof b === "number" ? (b / 1048576).toFixed(1) : "n/a";
const KiBtoMiB = (k: unknown): string => typeof k === "number" ? (k / 1024).toFixed(1) : "n/a";

function report(out: string): void {
  const labels = new Set<string>();
  for (const f of fs.readdirSync(out)) {
    const m = /^(.+?)\.(json|live\.json|helper\.json|term\.json)$/.exec(f);
    if (m?.[1] && !f.endsWith(".tmp")) labels.add(m[1]);
  }
  const rows: string[] = [
    "| fixture | mode | mem/cpus | outcome | discovery done | discovery ms | capture ms | tree occ (obs / expected) | tree entries | discovery object bytes | readSync bytes | helper maxRSS MiB (disc / final) | tree RSS peak MiB (disc / all) | cgroup peak MiB (disc / end) | cancel: settle ms, cleanup |",
    "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|",
  ];
  for (const label of [...labels].sort()) {
    const r = readJson<Record<string, any>>(path.join(out, `${label}.json`));
    const term = readJson<Record<string, any>>(path.join(out, `${label}.term.json`));
    const liveRec = readJson<Record<string, any>>(path.join(out, `${label}.live.json`));
    const helperRec = readJson<Record<string, any>>(path.join(out, `${label}.helper.json`));
    const [spec = label, base = "?", mem = "?", extra] = label.split("__");
    const mode = extra ? `${base} (${extra})` : base;
    if (!r) {
      // Killed before the result was written: last persisted values are lower bounds.
      const kind = term?.oomKilled === true ? "oom" : term?.killedByHarness === true ? "killed_by_harness"
        : term?.exitCode === "137" ? "killed_unknown" : term ? "harness_error" : "missing";
      const h = helperRec ?? liveRec?.lastHelper;
      const L = (v: unknown): string => (v === undefined || v === null ? "n/a" : `≥${String(v)}`);
      const objBytes = h ? (h.commitBytes ?? 0) + (h.treeBytes ?? 0) + (h.objectCheckBytes ?? 0) + (h.sizingBytes ?? 0) : undefined;
      rows.push(`| ${spec} | ${mode} | ${mem} | ${kind} (exit ${term?.exitCode ?? "?"}) | ${h?.boundaryMs != null ? "yes" : "no/unknown"} | ${h?.boundaryMs ?? "n/a"} | ${L(liveRec?.elapsedMs)} | ${L(h?.treeOccurrences)} / ${readJson<Manifest>(path.join(out, "..", "fixtures", spec, "manifest.json"))?.expectedTreeOccurrences ?? "?"} | ${L(h?.treeEntries)} | ${L(objBytes)} | ${L(h?.readSyncBytes)} | ${KiBtoMiB(h?.boundaryMaxRssKiB)} / ${h?.maxRssKiB != null ? "≥" + KiBtoMiB(h.maxRssKiB) : "n/a"} | ${liveRec ? "≥" + MiB(liveRec.treeRssDiscoveryPeak) : "n/a"} / ${liveRec ? "≥" + MiB(liveRec.treeRssPeak) : "n/a"} | ${MiB(liveRec?.cgroupPeakAtBoundary)} / n/a | n/a |`);
      continue;
    }
    let kind: string = r.outcome?.kind ?? "?";
    if (term?.oomKilled === true) kind = "oom";
    const token = r.outcome?.token ? ` \`${r.outcome.token}\`` : "";
    const d = r.discovery;
    const h = r.helper ?? {};
    const exp = r.fixture?.expectedTreeOccurrences ?? "?";
    const occ = d ? `${h.treeOccurrences ?? "n/a"} / ${exp}` : `n/a / ${exp}`;
    const objBytes = d ? String((h.commitBytes ?? 0) + (h.treeBytes ?? 0) + (h.objectCheckBytes ?? 0) + (h.sizingBytes ?? 0)) : "n/a";
    const c = r.cancel ? `${r.cancel.abortToSettledMs}, ${r.cancel.cleanupOk ? "ok" : "NOT ok"} (${r.cancel.trigger}${r.cancel.landedBeforeOutcome === false ? "; after outcome" : ""})` : "";
    // A helper that died by a signal (OOM or otherwise) left only its last snapshot: lower bounds.
    const killed = kind === "oom" || kind === "helper_killed_unknown" || (r.helperCompletion && "rejected" in r.helperCompletion) || r.helperCompletion?.code === 128;
    const B = (v: unknown): string => {
      const text = v === undefined || v === null ? "n/a" : String(v);
      return killed && !text.startsWith("n/a") ? `≥${text}` : text;
    };
    const cgEnd = kind === "oom" ? `OOM at limit ${r.memLimit ?? mem}` : MiB(r.cgroup?.peakAtEnd);
    const helperRss = r.helperMaxRssKiB ? `${KiBtoMiB(r.helperMaxRssKiB.discovery)} / ${B(KiBtoMiB(r.helperMaxRssKiB.final))}` : "n/a";
    rows.push(`| ${spec} | ${mode} | ${r.memLimit ?? mem}/${r.cpus ?? "?"} | ${kind}${token} | ${d ? (d.completed ? "yes" : "no") : "n/a"} | ${d?.ms ?? "n/a"} | ${r.captureMs} | ${d ? `${B(h.treeOccurrences)} / ${exp}` : occ} | ${d ? B(h.treeEntries) : "n/a"} | ${d ? B(objBytes) : "n/a"} | ${d ? B(h.readSyncBytes) : "n/a"} | ${helperRss} | ${B(MiB(r.treeRss?.discoveryPeakBytes))} / ${B(MiB(r.treeRss?.peakBytes))} | ${MiB(r.cgroup?.peakAtBoundary)} / ${cgEnd} | ${c} |`);
  }
  const table = rows.join("\n") + "\n";
  fs.writeFileSync(path.join(out, "results.md"), table);
  process.stdout.write(table);
}

// ---------------------------------------------------------------------------------------------

type Job =
  | { kind: "generate"; spec: string; dir: string; inputs: string }
  | MeasureJob
  | { kind: "report"; out: string }
  // Absorbs a container's first-start transients so no measurement is the first start.
  | { kind: "noop" };

async function main(): Promise<void> {
  const [command, arg] = process.argv.slice(2);
  if (command !== "job" || !arg) {
    console.error("usage: planning-base-discovery.measure.ts job <job.json>");
    process.exit(2);
  }
  const job = JSON.parse(fs.readFileSync(arg, "utf8")) as Job;
  if (job.kind === "generate") {
    const m = generate(job.spec, job.dir, job.inputs);
    process.stdout.write(`generated ${m.spec}: tree ${m.tree} trees=${m.expectedTreeOccurrences} leaves=${m.expectedLeafOccurrences} ` +
      `in ${m.generationMs} ms (sha256 manifest ${createHash("sha256").update(JSON.stringify({ ...m, generationMs: 0 })).digest("hex").slice(0, 12)})\n`);
  } else if (job.kind === "measure") {
    await measure(job);
    process.stdout.write(`measured ${job.label}\n`);
  } else if (job.kind === "report") {
    report(job.out);
  }
}

await main();
