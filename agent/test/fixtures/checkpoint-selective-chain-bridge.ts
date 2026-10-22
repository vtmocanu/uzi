// Launched by TestAgentCheckpointChainBridge (api/internal/pushbroker). Issue #2733 binding bridge:
// drives a REAL RunRunner + GitCache through a GitHub-forge turn that publishes a workflow overlay
// checkpoint O, then a raw tick or iteration checkpoint N, and sends every publish through the REAL
// WorkerClient.publishCheckpoint to the Go test's loopback server, which runs the real
// pushbroker.Publish against the Go-owned bare origin. Every other worker RPC goes to the in-process
// FakeApi. It also checks the current selective-pack server-name and pinned-default contract.
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { Transform } from "node:stream";
import { pipeline } from "node:stream/promises";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import "../setup/hermetic-proc.js";
import { WorkerClient } from "../../src/client.js";
import { GitCache } from "../../src/git.js";
import { RunRunner, type RunnerOptions } from "../../src/runner.js";
import type { RunContext } from "../../src/executor.js";
import { FakeApi } from "../fake-api.js";
import { defaultGitleaksShim } from "../gitleaks-shim.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "../helpers.js";
import { fakeGitHub } from "../runner-harness.js";

const raw = process.env.UZI_CHAIN_BRIDGE_FIXTURE ?? "";
assert.ok(Buffer.byteLength(raw) <= 16 * 1024, "bounded fixture configuration");
const cfg = JSON.parse(raw) as {
  url: string; token: string; origin: string; iid: number; boundary: "tick" | "iteration"; scratch: string; defaultBranch: string; historicalBlob: string;
};
assert.ok(path.isAbsolute(cfg.scratch));
assert.ok(path.isAbsolute(cfg.origin));
process.umask(0o077);
const local = fs.mkdtempSync(path.join(cfg.scratch, "worker-"));
const shallow = path.join(local, "depth-one.git");
execFileSync("git", ["init", "--bare", shallow], { timeout: 10_000, maxBuffer: 1024 * 1024 });
execFileSync("git", ["-C", shallow, "fetch", "--depth=1", cfg.origin, `refs/heads/${cfg.defaultBranch}`],
  { timeout: 20_000, maxBuffer: 1024 * 1024 });
assert.throws(() => execFileSync("git", ["-C", shallow, "cat-file", "-e", cfg.historicalBlob],
  { timeout: 10_000, maxBuffer: 1024 * 1024, stdio: "pipe" }), (error: unknown) => (error as { status: number }).status === 1);
assert.equal(execFileSync("git", ["-C", cfg.origin, "cat-file", "-t", cfg.historicalBlob],
  { encoding: "utf8", timeout: 10_000, maxBuffer: 1024 * 1024 }).trim(), "blob");
const INTERVAL_MS = 1_000;
const TICK_MS = 424_242;
const workflow = ".github/workflows/ci.yml";

const gitEnv = {
  ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const ident = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: gitEnv, encoding: "utf8", timeout: 20_000, maxBuffer: 1024 * 1024 }).trim();
}
function commitIn(tree: string, file: string, content: string): string {
  fs.mkdirSync(path.dirname(path.join(tree, file)), { recursive: true });
  fs.writeFileSync(path.join(tree, file), content);
  gitIn(tree, ["add", "-A"]);
  gitIn(tree, [...ident, "commit", "-m", `work ${file}`]);
  return gitIn(tree, ["rev-parse", "HEAD"]);
}
/** Advance the server-named default workflow in the Go-owned bare origin, so the agent branch
 *  is behind on .github/workflows and the overlay path runs. */
function advanceOriginWorkflow(): void {
  const work = path.join(local, "origin-work");
  execFileSync("git", ["clone", "-q", "--branch", cfg.defaultBranch, cfg.origin, work], { env: gitEnv, timeout: 20_000 });
  // Match the run's only non-workflow change, so the real overlay has default's full tree.
  commitIn(work, "r.txt", "R\n");
  commitIn(work, workflow, `name: ci\non: push\njobs: {}\n# advanced ${cfg.iid}\n`);
  gitIn(work, ["push", "-q", "origin", `HEAD:refs/heads/${cfg.defaultBranch}`]);
}

// The runner's worker client talks to the in-process FakeApi; only publishCheckpoint is delegated to
// a second REAL WorkerClient aimed at the Go server.
const api = new FakeApi(cfg.token);
const apiUrl = await api.listen();
const client = new WorkerClient(apiUrl, cfg.token, "0.1.0-test", nullLogger(), {
  sleep: async () => {}, terminalRetrySchedule: [1, 1],
});
const goClient = new WorkerClient(cfg.url, cfg.token, "0.1.0-test", nullLogger(), {
  sleep: async () => {}, terminalRetrySchedule: [1, 1],
});
const tips: string[] = [];
const results: unknown[] = [];
const packRanges = new Map<string, { bare: string; floor: string; defaultSha: string }>();
const wireDigests = new Map<string, string>();
const wireArtifacts: { file: string; legacyFile: string; tip: string; floor: string; defaultSha: string; bytes: number }[] = [];
let inventoryFailure: string | undefined;
function inventory(bare: string, file: string): string[] {
  gitIn(bare, ["index-pack", file]);
  return gitIn(bare, ["verify-pack", "-v", file.replace(/\.pack$/, ".idx")]).split("\n")
    .filter(row => /^[0-9a-f]{40} /.test(row)).map(row => row.split(/ +/).slice(0, 2).join(" ")).sort();
}
function assertInventory(expected: string[], actual: string[]): void {
  for (let i = 0; i < Math.max(expected.length, actual.length); i++) {
    if (actual[i] !== expected[i]) {
      inventoryFailure ??= `inventory row ${i}: got ${actual[i] ?? "<end>"} want ${expected[i] ?? "<end>"}`;
      return;
    }
  }
}

(client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
  runId: string, tipOid: string, pack: Parameters<WorkerClient["publishCheckpoint"]>[2], signal?: AbortSignal,
) => {
  signal?.throwIfAborted();
  const range = packRanges.get(tipOid);
  assert.ok(range, "actual producer range captured before upload");
  const file = path.join(local, `wire-${tips.length}.pack`);
  let bytes = 0;
  // Spool the actual production stream with a hard cap, then send precisely those bytes.
  await pipeline(pack, new Transform({
    transform(chunk: Buffer, _encoding, callback) {
      bytes += chunk.length;
      callback(bytes > 64 * 1024 * 1024 ? new Error("wire exceeds 64 MiB") : null, chunk);
    },
  }), fs.createWriteStream(file, { flags: "wx" }));
  const actual = inventory(range.bare, file);
  const legacyFile = path.join(local, `legacy-${tips.length}.pack`);
  const fd = fs.openSync(legacyFile, "wx");
  try {
    execFileSync("git", ["-C", range.bare, "pack-objects", "--revs", "--stdout"], {
      env: gitEnv, input: `${tipOid}\n^${range.floor}\n`, stdio: ["pipe", fd, "pipe"],
      timeout: 20_000, maxBuffer: 1024 * 1024,
    });
  } finally { fs.closeSync(fd); }
  assert.ok(fs.statSync(legacyFile).size <= 64 * 1024 * 1024);
  const legacy = inventory(range.bare, legacyFile);
  wireArtifacts.push({ file, legacyFile, tip: tipOid, floor: range.floor, defaultSha: range.defaultSha, bytes });
  // Independent oracle: the full default closure is tiny in this fixture only.
  const objects = gitIn(range.bare, ["rev-list", "--objects", range.defaultSha]).split("\n")
    .map(row => row.split(" ")[0]!);
  const eligible = new Set(execFileSync("git", ["-C", range.bare, "cat-file", "--batch-check=%(objectname) %(objecttype)"], {
    env: gitEnv, input: objects.join("\n") + "\n", encoding: "utf8", timeout: 20_000, maxBuffer: 1024 * 1024,
  }).trim().split("\n").filter(row => row.endsWith(" blob")));
  // Every original commit/tree and non-default blob is required. The bounded walk may
  // retain eligible blobs, so require eligibility for each omission without demanding removal.
  const present = new Set(actual);
  assertInventory(legacy.filter(row => !eligible.has(row) || present.has(row)), actual);
  assert.ok(legacy.includes(`${cfg.historicalBlob} blob`), "legacy actual wrapper range carries historical blob");
  assert.ok(!actual.includes(`${cfg.historicalBlob} blob`), "actual wrapper/replay stream omits historical blob");
  const hash = createHash("sha256");
  for await (const chunk of fs.createReadStream(file)) hash.update(chunk);
  const digest = hash.digest("hex");
  if (wireDigests.has(tipOid)) assert.equal(digest, wireDigests.get(tipOid), "lost-ack replay bytes identical");
  wireDigests.set(tipOid, digest);
  signal?.throwIfAborted();
  tips.push(tipOid);
  const res = await goClient.publishCheckpoint(runId, tipOid, fs.createReadStream(file), signal);
  results.push(res);
  return res;
};

// Fake clock + tick timer, as control() in test/runner-midturn-checkpoint.test.ts.
let fakeNow = 0;
let tickCb: (() => void) | undefined;
let outcomeWaiter: ((o: string) => void) | undefined;
const hooks: NonNullable<RunnerOptions["checkpointTestHooks"]> = {
  tickKillGraceMs: 300,
  onTickOutcome: (o) => { outcomeWaiter?.(o); },
};
async function fireTick(): Promise<string> {
  assert.ok(tickCb, "the tick timer is armed");
  const done = new Promise<string>((resolve) => { outcomeWaiter = resolve; });
  tickCb();
  return await done;
}

const gitCache = new GitCache(path.join(local, "data"), nullLogger(), undefined,
  testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
const authorities: (string | undefined)[] = [];
const defaults: (string | undefined)[] = [];
const resolveRange = gitCache.resolveCheckpointRange.bind(gitCache);
gitCache.resolveCheckpointRange = async (...args) => {
  authorities.push(args[2]?.defaultBranch);
  const range = await resolveRange(...args);
  if (range) defaults.push(range.defaultBlobSha);
  return range;
};
const packAuthorities: (string | undefined)[] = [];
const packDefaults: string[] = [];
const makePack = gitCache.checkpointPack.bind(gitCache);
gitCache.checkpointPack = async (...args) => {
  packAuthorities.push(args[8]?.defaultBranch);
  // A pinned scan may precede the overlay's default fetch. Its original SHA stays authoritative.
  const captured = args[3]?.defaultBlobSha ?? gitIn(args[0], ["rev-parse", `refs/remotes/origin/${cfg.defaultBranch}`]);
  assert.match(captured, /^[0-9a-f]{40}$/);
  const range = args[3] ?? await resolveRange(args[0], args[1], { tipSha: args[5], defaultBranch: cfg.defaultBranch });
  assert.ok(range, "public range resolver supplies the legacy comparison floor");
  const floor = range.excludeSha;
  const packed = await makePack(...args);
  if (packed) {
    if (args[3]) assert.equal(args[3].defaultBlobSha, captured, "overlay must retain scanned default authority");
    packDefaults.push(captured);
    packRanges.set(packed.tipOid, { bare: args[0], floor, defaultSha: captured });
  }
  return packed;
};
const { github } = fakeGitHub();
const branch = `agent/issue-${cfg.iid}`;
const summary: Record<string, unknown> = { branch, boundary: cfg.boundary };
let turnError: unknown;

const runner = new RunRunner(client, gitCache, () => ({
  executor: {
    run: async (ctx: RunContext) => {
      try {
        const progress = { completed: ["m1"], in_progress: [] };
        // Merge the bounded historical default before a real run-only edit and revert.
        gitIn(ctx.worktreePath, [...ident, "merge", "--no-edit", `origin/${cfg.defaultBranch}`]);
        const matched = commitIn(ctx.worktreePath, "r.txt", "R\n");
        const edit = commitIn(ctx.worktreePath, "r.txt", "run-only edit\n");
        summary.realTip = commitIn(ctx.worktreePath, "r.txt", "R\n");
        assert.equal(gitIn(ctx.worktreePath, ["show", "-s", "--format=%P", edit]), matched);
        assert.equal(gitIn(ctx.worktreePath, ["show", "-s", "--format=%P", summary.realTip as string]), edit);
        assert.equal(gitIn(ctx.worktreePath, ["show", `${edit}:r.txt`]), "run-only edit");
        assert.equal(gitIn(ctx.worktreePath, ["show", `${summary.realTip}:r.txt`]), "R");
        assert.equal(gitIn(ctx.worktreePath, ["rev-parse", `${matched}^{tree}`]),
          gitIn(ctx.worktreePath, ["rev-parse", `${summary.realTip}^{tree}`]), "real edit reverted before overlay");
        advanceOriginWorkflow();
        await ctx.checkpoint!({ reap: true, progress });
        summary.overlay = tips[0];
        summary.overlayRef = gitIn(cfg.origin, ["rev-parse", "--verify", "--quiet", `refs/uzi-checkpoints/${branch}`]);
        assert.equal(summary.overlayRef, summary.overlay, "overlay actually reached the broker ref");
        const rows = (revision: string) => gitIn(cfg.origin, ["ls-tree", "-r", revision]).split("\n").filter(Boolean);
        const expectedRows = new Set(rows(summary.realTip as string).filter(row => !row.endsWith(`\t${workflow}`)));
        const defaultWorkflow = rows(`refs/heads/${cfg.defaultBranch}`).find(row => row.endsWith(`\t${workflow}`));
        assert.ok(defaultWorkflow, "current default supplies workflow");
        expectedRows.add(defaultWorkflow);
        const overlayRows = rows(summary.overlay as string);
        assert.equal(overlayRows.length, expectedRows.size);
        assert.equal(overlayRows.find(row => !expectedRows.has(row)), undefined, "first overlay tree mismatch");
        summary.overlayTree = gitIn(cfg.origin, ["rev-parse", `${summary.overlay}^{tree}`]);
        summary.rawTip = commitIn(ctx.worktreePath, "n.txt", "N\n");
        fakeNow += INTERVAL_MS + 1;
        if (cfg.boundary === "tick") summary.tickOutcome = await fireTick();
        else await ctx.checkpoint!({ reap: false, progress });
        const final = gitIn(cfg.origin, ["rev-parse", `refs/uzi-checkpoints/${branch}`]);
        assert.equal(gitIn(cfg.origin, ["show", "-s", "--format=%P", final]), `${summary.overlay} ${summary.rawTip}`,
          "next checkpoint chains over overlay and raw work");
        const chainedRows = rows(final);
        const expectedChain = new Set([
          ...rows(summary.rawTip as string).filter(row => !row.endsWith(`\t${workflow}`)),
          defaultWorkflow,
        ]);
        assert.equal(chainedRows.length, expectedChain.size);
        assert.equal(chainedRows.find(row => !expectedChain.has(row)), undefined, "first chained overlay tree mismatch");
        summary.tips = tips;
        summary.results = results;
        summary.wireArtifacts = wireArtifacts;
      } catch (e) {
        turnError = e;
        throw e;
      }
      return { branch: ctx.branch };
    },
  },
}), nullLogger(), 20, undefined, {
  pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, prDescriptionHeadLagMs: 0,
  github, checkpointIntervalMs: INTERVAL_MS, checkpointTickIntervalMs: TICK_MS,
  setTimer: (cb, ms) => { const t = setTimeout(cb, ms); t.unref?.(); return () => clearTimeout(t); },
  setTickTimer: (cb) => {
    tickCb = cb;
    return () => { if (tickCb === cb) tickCb = undefined; };
  },
  now: () => fakeNow,
  checkpointTestHooks: hooks,
});

const claim = makeClaim({
  issue_iid: cfg.iid,
  issue_title: `Bridge ${cfg.iid}`,
  repo: { id: "r1", url: "https://github.com/org/repo", clone_url: cfg.origin, forge_type: "github", default_branch: cfg.defaultBranch },
  secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
});
let code = 0;
try {
  await runner.execute(claim);
  if (turnError !== undefined) throw turnError;
  assert.equal(inventoryFailure, undefined, "complete inventories of exact uploaded wrapper/chained/replay bytes");
  assert.ok(authorities.length >= 1, "runner must resolve the scanned checkpoint range");
  assert.ok(packAuthorities.length >= 2, "runner must build the overlay and subsequent pack");
  assert.ok(packAuthorities.every(name => name === cfg.defaultBranch), "all actual pack calls carry server authority");
  assert.ok(authorities.every(name => name === cfg.defaultBranch), "exact server default threaded");
  assert.ok(defaults.every(oid => /^[0-9a-f]{40}$/.test(oid ?? "")), "actual ranges capture default provenance");
  summary.defaultAuthorities = authorities;
  assert.ok(packDefaults.length >= 2, "overlay and subsequent pack both captured default");
  summary.packAuthorities = packAuthorities;
  summary.packDefaults = packDefaults;
  summary.capturedDefaults = defaults;
  console.log("BRIDGE_SUMMARY " + JSON.stringify(summary));
} catch (e) {
  console.error("bridge fixture failed:", e);
  console.log("BRIDGE_SUMMARY " + JSON.stringify({ ...summary, error: String(e) }));
  code = 1;
} finally {
  runner.shutdown();
  await api.close();
}
process.exit(code);
