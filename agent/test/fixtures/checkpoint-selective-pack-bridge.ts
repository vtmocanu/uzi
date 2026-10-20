// TestAgentCheckpointSelectivePackBridge owns this process and its artifact directory.
// Packs stay on disk; only bounded object inventories and provenance cross stdout.
import assert from "node:assert/strict";
import { execFileSync, spawn } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { pipeline } from "node:stream/promises";
import { createHash } from "node:crypto";
import "../setup/hermetic-proc.js";
import { GitCache, type BoundaryProcessSpawner } from "../../src/git.js";
import { prepareFrozenHeld } from "../../src/frozen-held.js";
import { nullLogger, testGitCacheOptions } from "../helpers.js";
import { defaultGitleaksShim } from "../gitleaks-shim.js";

const raw = process.env.UZI_SELECTIVE_BRIDGE_FIXTURE ?? "";
assert.ok(Buffer.byteLength(raw) <= 16 * 1024, "bounded fixture configuration");
const cfg = JSON.parse(raw) as { origin: string; scratch: string; mode: string; defaultBranch: string };
assert.ok(path.isAbsolute(cfg.origin) && path.isAbsolute(cfg.scratch));
process.umask(0o077);
const local = fs.mkdtempSync(path.join(cfg.scratch, "selective-"));
const bare = path.join(local, "worker.git");
const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_AUTHOR_NAME: "test", GIT_AUTHOR_EMAIL: "test@example.com",
  GIT_COMMITTER_NAME: "test", GIT_COMMITTER_EMAIL: "test@example.com", GIT_TERMINAL_PROMPT: "0" };
const git = (args: string[], input?: string | Buffer) =>
  execFileSync("git", ["--git-dir", bare, ...args], { env, input, encoding: "utf8",
    maxBuffer: 1024 * 1024, timeout: 20_000, stdio: ["pipe", "pipe", "pipe"] }).trim();
execFileSync("git", ["init", "--bare", bare], { env, stdio: "pipe", timeout: 10_000 });
git(["config", "maintenance.auto", "false"]);
git(["remote", "add", "origin", cfg.origin]);
const blob = (data: string | Buffer) => git(["hash-object", "-w", "--stdin"], data);
const tree = (entries: [string, string, string?][]) => git(["mktree"],
  entries.map(([name, oid, type]) => `${type === "tree" ? "040000 tree" : "100644 blob"} ${oid}\t${name}\n`).join(""));
const commit = (root: string, parents: string[], message: string) =>
  git(["commit-tree", root, ...parents.flatMap(p => ["-p", p])], message + "\n");
const empty = tree([]);
const floor = commit(empty, [], "B floor");
const branch = "agent/selective";
const defaultName = cfg.defaultBranch;
assert.equal(defaultName, "integration/custom", "default name comes from the Go server context");
let history = floor;
const publicBlobs: string[] = [];
const big = cfg.mode === "PrimaryLargeHistory" || cfg.mode === "LargeRunOnly";
for (let i = 0; i < (big ? 6 : 3); i++) {
  // Distinct compressible objects stay below the broker's per-object limit.
  const oid = blob(Buffer.alloc(big ? 24 * 1024 * 1024 : 128, 65 + i));
  publicBlobs.push(oid);
  history = commit(tree([[`public-${i}`, oid]]), [history], `X public ${i}`);
}
const publicTree = tree([["public", publicBlobs.at(-1)!]]);
const defaultAtCapture = commit(publicTree, [history], "default at capture");
const privateBlob = blob("private work\n");
const protectedBase = tree([["config.yml", blob("base config\n")]]);
const protectedChanged = tree([["config.yml", blob("changed config\n")]]);
const guarded = cfg.mode === "ProtectedDifference" || cfg.mode === "UnprotectedDifference";
const defaultTree = guarded ? tree([["ci", protectedBase, "tree"], ["public", publicBlobs.at(-1)!]]) : publicTree;
let defaultTip = commit(defaultTree, [defaultAtCapture], "default advances after merge");
let candidateTree = tree([["private", privateBlob], ["public", publicBlobs.at(-1)!]]);
if (cfg.mode === "EqualDefault") candidateTree = defaultTree;
if (cfg.mode === "RevertDefault") candidateTree = defaultTree;
if (guarded) candidateTree = tree([["ci", cfg.mode === "ProtectedDifference" ? protectedChanged : protectedBase, "tree"],
  ["private", privateBlob], ["public", publicBlobs.at(-1)!]]);
if (cfg.mode === "LargeRunOnly") {
  candidateTree = tree(Array.from({ length: 6 }, (_, i) =>
    [`run-${i}`, blob(Buffer.alloc(24 * 1024 * 1024, 97 + i))] as [string, string]));
}
const exact = cfg.mode === "ExactLinearHistory";
const x = commit(tree([["private", privateBlob], ["public", publicBlobs[0]!]]), [floor], "X work");
const d = commit(exact ? defaultTree : candidateTree, exact || cfg.mode === "NoDefaultMerge" || cfg.mode === "LargeRunOnly"
  ? [x] : [x, defaultAtCapture], exact ? "D default" : "D merged");
const tip = commit(candidateTree, [d], "C candidate");
if (exact) defaultTip = d;
if (cfg.mode === "ReachableDefault") defaultTip = commit(defaultTree, [tip], "default includes candidate");
if (cfg.mode === "EqualDefault" || cfg.mode === "RevertDefault") {
  assert.equal(git(["rev-parse", `${tip}^{tree}`]), git(["rev-parse", `${defaultTip}^{tree}`]));
}
if (cfg.mode === "RevertDefault") {
  assert.notEqual(git(["rev-parse", `${x}^{tree}`]), defaultTree, "private edit precedes revert");
  assert.equal(git(["merge-base", x, tip]), x);
}
if (exact) {
  assert.equal(defaultTip, d);
  for (const [child, expected] of [[x, floor], [d, x], [tip, d]]) {
    assert.equal(git(["show", "-s", "--format=%P", child!]), expected);
  }
}
git(["update-ref", `refs/uzi-runner/${branch}`, tip]);
if (cfg.mode !== "AbsentOriginBranch") git(["update-ref", `refs/remotes/origin/${branch}`, floor]);
git(["update-ref", `refs/remotes/origin/${defaultName}`, defaultTip]);
git(["update-ref", `refs/heads/${defaultName}`, defaultTip]);
git(["push", "-q", "origin", `${defaultTip}:refs/heads/${defaultName}`, `${floor}:refs/heads/${branch}`]);
if (cfg.mode === "AbsentOriginBranch") git(["push", "-q", "origin", `:refs/heads/${branch}`]);
// Private worker has no origin/HEAD unless a negative-provenance control explicitly adds it.
assert.equal(git(["for-each-ref", "--format=%(refname)", "refs/remotes/origin/HEAD"]), "");
// These controls exercise the pre-existing merge-base/CI floor resolver as well.
if (cfg.mode === "AbsentOriginBranch" || cfg.mode === "FrozenHeld") {
  git(["symbolic-ref", "refs/remotes/origin/HEAD", `refs/remotes/origin/${defaultName}`]);
}
// Show that an omitted historical blob exists at origin but is absent from a depth-one default fetch.
const historicalBlob = publicBlobs[exact ? 0 : 1]!;
const shallow = path.join(local, "depth-one.git");
execFileSync("git", ["init", "--bare", shallow], { env, stdio: "pipe", timeout: 10_000 });
execFileSync("git", ["--git-dir", shallow, "fetch", "--depth=1", cfg.origin, `refs/heads/${defaultName}`],
  { env, stdio: "pipe", timeout: 20_000 });
let historicalBlobAbsentDepthOne = false;
try { execFileSync("git", ["--git-dir", shallow, "cat-file", "-e", historicalBlob], { env, stdio: "pipe", timeout: 10_000 }); }
catch (error) {
  assert.equal((error as { status: number }).status, 1);
  historicalBlobAbsentDepthOne = true;
}
assert.equal(historicalBlobAbsentDepthOne, true);
execFileSync("git", ["--git-dir", cfg.origin, "cat-file", "-e", historicalBlob], { env, stdio: "pipe", timeout: 10_000 });
let authority: string | undefined = defaultName;
if (cfg.mode === "AbsentServerName") authority = undefined;
if (cfg.mode === "UnresolvedDefault") {
  git(["update-ref", "-d", `refs/remotes/origin/${defaultName}`]);
  git(["update-ref", "-d", `refs/heads/${defaultName}`]);
}
if (cfg.mode === "MirrorHeadsOnly") git(["update-ref", "-d", `refs/remotes/origin/${defaultName}`]);
if (cfg.mode === "ConflictingOriginHEAD") git(["symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/other"]);
const cache = new GitCache(local, nullLogger(), undefined, testGitCacheOptions({
  gitleaksBin: defaultGitleaksShim(),
  checkpointPackBounds: cfg.mode === "LiveCapFallback" || cfg.mode === "CapExpiredParent" ? { bytes: 1 } : undefined,
}));
type Packed = NonNullable<Awaited<ReturnType<GitCache["checkpointPack"]>>>;
const artifacts: { name: string; file: string; tip: string; inventory: string[]; reconstructed: number }[] = [];
async function save(name: string, packed: Pick<Packed, "pack" | "exited" | "cancel">, oid: string) {
  const file = path.join(local, name + ".pack");
  let size = 0;
  packed.pack.on("data", chunk => {
    size += Buffer.byteLength(chunk);
    if (size > 64 * 1024 * 1024) { packed.cancel(); packed.pack.destroy(new Error("wire cap")); }
  });
  try { await pipeline(packed.pack, fs.createWriteStream(file, { flags: "wx" })); }
  finally { assert.equal(await packed.exited, 0, "pack child joined successfully"); }
  git(["index-pack", file]);
  const rows = git(["verify-pack", "-v", file.replace(/\.pack$/, ".idx")]).split("\n")
    .filter(row => /^[0-9a-f]{40} /.test(row)).map(row => row.split(/ +/));
  assert.ok(rows.some(row => row[0] === oid && row[1] === "commit"), "candidate tip retained in pack, even when reachable from default");
  artifacts.push({ name, file, tip: oid, inventory: rows.map(row => row.slice(0, 2).join(" ")).sort(),
    reconstructed: rows.reduce((sum, row) => sum + Number(row[2]), 0) });
}
// One absolute budget for this checkpoint operation, including fallback and replay.
const operationDeadline = Date.now() + 90_000;
const options = { defaultBranch: authority, deadline: cfg.mode === "ExpiredParent" ? Date.now() - 1 : operationDeadline };
// Pin explicit provenance facts separately from inventory so evidence is reviewable.
assert.equal(git(["merge-base", floor, tip]), floor);
let defaultIsAncestor = false;
try { git(["merge-base", "--is-ancestor", defaultTip, tip]); defaultIsAncestor = true; }
catch (error) { assert.equal((error as { status: number }).status, 1); }
assert.equal(defaultIsAncestor, exact, "only the exact linear control includes the current default");
if (cfg.mode === "ReachableDefault") assert.equal(git(["merge-base", tip, defaultTip]), tip);
const legacy = await cache.checkpointPack(bare, branch);
assert.ok(legacy);
await save("legacy", legacy, legacy.tipOid);
let stopped = false;
let capturedDefault: string | undefined, capturedFloor: string | undefined;
const calls: string[] = [];
const preparationChildren = new Set<ReturnType<typeof spawn>>();
let fallbackSettled = true;
const parent = new AbortController();
const realNow = Date.now;
let clockOffset = 0;
Date.now = () => realNow() + clockOffset;
const spawner: BoundaryProcessSpawner = async request => {
  const args = request.argv.slice(1);
  const preparation = !args.includes("pack-objects");
  const injectPreparation = args.includes("rev-list") && args.some(arg => /^\^[0-9a-f]{40}$/.test(arg));
  if (args.includes("pack-objects")) {
    fallbackSettled &&= preparationChildren.size === 0 && !parent.signal.aborted && Date.now() < options.deadline;
    assert.equal(fallbackSettled, true, "pack launch requires joined preparation and live parent budget");
  }
  calls.push(args.join(" "));
  const child = spawn(request.argv[0]!, [...args], { cwd: request.cwd, env: request.env });
  if (preparation) {
    preparationChildren.add(child);
    if (injectPreparation && cfg.mode === "PreparationAbort") parent.abort(new Error("injected preparation abort"));
    if (injectPreparation && cfg.mode === "PreparationDeadline") clockOffset = 120_000;
  }
  const completed = new Promise<{ code: number }>((resolve, reject) => {
    child.once("error", reject);
    child.once("close", code => {
      if (preparation) preparationChildren.delete(child);
      if (injectPreparation && cfg.mode === "CapExpiredParent") clockOffset = 120_000;
      resolve({ code: code ?? -1 });
    });
  });
  return { stdin: child.stdin, stdout: child.stdout, stderr: child.stderr, completed,
    cancel: async () => { child.kill("SIGTERM"); await completed; } };
};
if (["ExpiredParent", "CancelledParent", "PreparationAbort", "PreparationDeadline", "CapExpiredParent"].includes(cfg.mode)) {
  if (cfg.mode === "CancelledParent") parent.abort(new Error("injected parent abort"));
  await assert.rejects(cache.withBoundaryProcessSpawner(spawner, parent.signal,
    () => cache.checkpointPack(bare, branch, undefined, undefined, undefined, undefined, undefined, undefined,
      options)));
  stopped = true;
  assert.equal(calls.some(call => call.includes("pack-objects")), false);
} else if (cfg.mode === "FrozenHeld") {
  const result = await prepareFrozenHeld({ git: cache, barePath: bare, branch, sourceSha: tip,
    deadlineMs: 90_000, defaultBranch: defaultName,
    policy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: [], extraFloors: [] } });
  assert.ok("pack" in result, "reason" in result ? result.reason : "held bytes prepared");
  // Retain the exact production-prepared bytes, without JSON/base64 transport.
  const { Readable } = await import("node:stream");
  await save("held", { pack: Readable.from(result.pack), exited: Promise.resolve(0), cancel: () => {} }, tip);
  const denied = await prepareFrozenHeld({ git: cache, barePath: bare, branch, sourceSha: tip,
    deadlineMs: 90_000, defaultBranch: defaultName,
    policy: { version: 1, blocked: true, everKnown: false, flaggedCommits: [], ciGlobs: [], extraFloors: [] } });
  assert.deepEqual(denied, { reason: "secret_remediation_pending" });
  const guardedResult = await prepareFrozenHeld({ git: cache, barePath: bare, branch, sourceSha: tip,
    deadlineMs: 90_000, defaultBranch: defaultName,
    policy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: ["private"], extraFloors: [] } });
  assert.deepEqual(guardedResult, { reason: "ci_config_guard" });
} else {
  const range = await cache.resolveCheckpointRange(bare, branch, { defaultBranch: process.env.UZI_SELECTIVE_BRIDGE_LEGACY === "1" ? undefined : authority });
  assert.ok(range);
  capturedDefault = range.defaultBlobSha;
  capturedFloor = range.excludeSha;
  const packed = await cache.withBoundaryProcessSpawner(spawner, parent.signal,
    () => cache.checkpointPack(bare, branch, undefined, range, undefined, undefined, undefined, undefined,
      { ...options, defaultBranch: process.env.UZI_SELECTIVE_BRIDGE_LEGACY === "1" ? undefined : authority }));
  // Mutation selector enters production without server authority; never supplies a test manifest.
  assert.ok(packed);
  await save("selective", packed, packed.tipOid);
  if (cfg.mode.startsWith("Replay")) {
    const next = commit(empty, cfg.mode === "ReplayRewrite" ? [floor] : [defaultTip], "changed after capture");
    if (cfg.mode === "ReplayDelete") {
      git(["update-ref", "-d", `refs/remotes/origin/${defaultName}`]);
      git(["push", "-q", "origin", `:refs/heads/${defaultName}`]);
    } else {
      git(["update-ref", `refs/remotes/origin/${defaultName}`, next]);
      git(["push", "-q", "--force", "origin", `${next}:refs/heads/${defaultName}`]);
    }
    git(["update-ref", `refs/uzi-runner/${branch}`, floor]);
    const replay = await packed.replay();
    await save("replay", replay, packed.tipOid);
    const replayInventory = artifacts.at(-1)!.inventory;
    const capturedInventory = new Set(artifacts.at(-2)!.inventory);
    assert.equal(replayInventory.length, capturedInventory.size);
    assert.equal(replayInventory.find(row => !capturedInventory.has(row)), undefined, "first replay inventory mismatch");
    const digest = async (file: string) => {
      const hash = createHash("sha256");
      for await (const chunk of fs.createReadStream(file)) hash.update(chunk);
      return hash.digest("hex");
    };
    assert.equal(await digest(artifacts.at(-1)!.file), await digest(artifacts.at(-2)!.file));
  }
}
Date.now = realNow;
assert.equal(preparationChildren.size, 0, "all opened preparation children closed");
const settled = preparationChildren.size === 0;
assert.equal(fallbackSettled, true);
console.log("SELECTIVE_SUMMARY " + JSON.stringify({ local, branch, defaultName, tip, floor, x, d,
  defaultTip, publicBlobs, privateBlob, candidateTree, protectedChanged, protectedBase,
  stopped, calls, settled, capturedDefault, capturedFloor, historicalBlob, historicalBlobAbsentDepthOne, artifacts }));
