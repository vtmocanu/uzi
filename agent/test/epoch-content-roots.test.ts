import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { deflateSync } from "node:zlib";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-content-roots";
const BRANCH = "agent/issue-3431";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
let fx: Fixture;
let git: GitCache;
let bare: string;
let clonePath: string;
let raw: string;
let options: RetainedDiscardOptions;
let input: EpochDiscardProofOptions;
let scope: PodEpochReclaimScope;
let pending: number;
let stopped: boolean;
let live: boolean;
let nativeLaunches: number;

function gitIn(args: string[]): string {
  return execFileSync("git", ["-C", bare, ...args], { env: ENV, encoding: "utf8" }).trim();
}
function journal(): string { return gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`]); }

beforeEach(async () => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-3431", noProofReseed, RUN, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  clonePath = clone.path;
  await git.markRecoveryCapture(bare, clonePath, BRANCH, RUN, attempt);
  raw = journal();
  const selector = { runId: RUN, generation: 1, attempt };
  const holds = { state: "read" as const, holds: [] };
  const lifecycle = { state: "terminal" as const, status: "completed" as const };
  const inspection = await git.inspectRetainedSources({ selector, barePath: bare, holds, lifecycle, epoch: true, isLivePath: () => false });
  assert.equal(inspection.partial, false);
  pending = 0; stopped = false; live = true; nativeLaunches = 0;
  // Contract double counts real Git participation; descriptor ownership is tested separately.
  scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { nativeLaunches++; return [...stdio]; },
    stop() { stopped = true; },
    stopped() { return stopped; },
    assertSettled() { if (stopped || pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
  options = { selector, confirm: inspection.sources[0]!.descriptor, holds, lifecycle, barePath: bare,
    isLivePath: () => false, quiesce: async () => true };
  input = { decision: { eligible: true, attested: true }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: options.confirm, expectedRawJournal: raw,
    beforeEffect: async () => {}, checkLive: () => { if (!live) throw new Error("reservation expired"); },
    signal: new AbortController().signal, deadline: Date.now() + 30_000 };
});
afterEach(() => fx?.cleanup());


// Invoke the registered automatic proof under its production native/limit scopes.
// The discard core intentionally refuses owed consumers before reaching this proof.
async function observation(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number } = {}): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"](limits), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}
async function covered(limits: { maxMetadataRecords?: number; maxMetadataBytes?: number } = {}): Promise<boolean> {
  try { return await observation(limits); }
  catch (error) {
    if ((error as Error).constructor.name !== "AutomaticCompletenessUnknown") throw error;
    return false;
  }
}
async function unknown(): Promise<void> {
  await assert.rejects(observation(), error => (error as Error).constructor.name === "AutomaticCompletenessUnknown");
}
function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
function side(): string {
  const sha = command(bare, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "owed side");
  command(clonePath, "update-ref", "refs/private/side", sha);
  return sha;
}
async function pin(sha: string, runId = RUN): Promise<void> {
  await git["pinOwedUnderLock"]({
    runId, barePath: bare, branch: BRANCH, generation: 1, kind: "issue",
    defaultIdentity: { ref: "refs/heads/main", sha: command(bare, "rev-parse", "HEAD") },
  }, sha);
}
function snapshot(): string {
  return fs.readdirSync(path.join(bare, "uzi-owed")).sort().map(name =>
    name + ":" + fs.readFileSync(path.join(bare, "uzi-owed", name), "utf8")).join("\n") +
    command(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-owed/");
}

describe("automatic approved content roots", () => {
  it("accepts the clean published control and refs naming covered blobs and trees", async () => {
    assert.equal(await covered(), true);
    command(clonePath, "update-ref", "refs/private/blob", command(clonePath, "rev-parse", "HEAD:README.md"));
    command(clonePath, "update-ref", "refs/private/tree", command(clonePath, "rev-parse", "HEAD^{tree}"));
    assert.equal(await covered(), true);
  });
  for (const location of ["reflog", "loose"] as const) {
    it("retains an unpublished commit held only by " + location, async () => {
      assert.equal(await covered(), true);
      const head = command(clonePath, "rev-parse", "HEAD");
      const blob = execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin"],
        { env: ENV, encoding: "utf8", input: "unpublished README evidence\n" }).trim();
      const tree = location === "reflog" ? execFileSync("git", ["-C", clonePath, "mktree"],
        { env: ENV, encoding: "utf8", input: "100644 blob " + blob + "\tREADME.md\n" }).trim() : "HEAD^{tree}";
      const sha = command(clonePath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
        "commit-tree", tree, "-p", "HEAD", "-m", "unpublished evidence");
      if (location === "reflog") {
        command(clonePath, "update-ref", "--no-deref", "HEAD", head);
        command(clonePath, "update-ref", "HEAD", sha);
        command(clonePath, "update-ref", "HEAD", head);
        assert.match(command(clonePath, "reflog", "--format=%H"), new RegExp(sha));
        for (const ref of command(clonePath, "for-each-ref", "--format=%(refname)").split("\n"))
          assert.ok(!command(clonePath, "reflog", "--format=%H", ref).includes(sha));
      } else assert.ok(!command(clonePath, "reflog", "--all", "--format=%H").includes(sha));
      for (const cwd of [clonePath, bare])
        assert.ok(command(cwd, "for-each-ref", "--format=%(objectname)").split("\n").every(id => id === head));
      assert.equal(command(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-owed/"), "");
      assert.throws(() => command(bare, "cat-file", "-t", sha));
      assert.equal(command(clonePath, "status", "--porcelain"), "");
      assert.equal(journal(), raw);
      assert.equal(await covered(), false);
    });
  }
  for (const kind of ["commit", "blob", "tree", "tag"] as const) {
    it("retains an unpublished " + kind + " even when the bare has the same object", async () => {
      const blob = kind === "blob" ? execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin"],
        { env: ENV, encoding: "utf8", input: "unpublished blob\n" }).trim() : command(clonePath, "rev-parse", "HEAD:README.md");
      const tree = kind === "tree" ? execFileSync("git", ["-C", clonePath, "mktree"],
        { env: ENV, encoding: "utf8", input: "100644 blob " + blob + "\tevidence\n" }).trim() : "";
      let sha = blob;
      if (kind === "tree") sha = tree;
      if (kind === "commit") sha = command(clonePath, "-c", "user.name=fixture",
        "-c", "user.email=fixture@example.test", "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unpublished");
      if (kind === "tag") {
        command(clonePath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
          "tag", "-a", "private-evidence", "-m", "unpublished", "HEAD");
        sha = command(clonePath, "rev-parse", "refs/tags/private-evidence");
        command(clonePath, "update-ref", "-d", "refs/tags/private-evidence");
      }
      // Copy loose storage without granting any publication ref or pin.
      const objects = path.join(clonePath, ".git", "objects");
      for (const dir of fs.readdirSync(objects).filter(name => /^[0-9a-f]{2}$/.test(name)))
        fs.cpSync(path.join(objects, dir), path.join(bare, "objects", dir), { recursive: true });
      assert.equal(command(bare, "cat-file", "-t", sha), kind);
      assert.equal(await covered(), false);
    });
  }
  for (const change of ["addition", "removal"] as const) {
    it("retains when the source inventory has an " + change + " between observations", async t => {
      assert.equal(await covered(), true);
      const blob = command(clonePath, "rev-parse", "HEAD:README.md");
      const object = path.join(clonePath, ".git", "objects", blob.slice(0, 2), blob.slice(2));
      const original = git["runGitAsRunner"].bind(git);
      let inventories = 0;
      const internal = git as unknown as { runGitAsRunner: typeof original };
      t.mock.method(internal, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
        if (args[1].includes("--batch-all-objects") && ++inventories === 2) {
          if (change === "addition") execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin"],
            { env: ENV, input: "late evidence" });
          else fs.unlinkSync(object);
        }
        return original(...args);
      });
      assert.equal(await covered(), false);
      assert.equal(inventories, 2);
      assert.equal(journal(), raw);
    });
  }
  for (const output of ["not-a-sha\n", "a".repeat(40), "\n", "A".repeat(40) + "\n"]) {
    it("retains malformed or unterminated object inventory " + JSON.stringify(output), async t => {
      const original = git["runGitAsRunner"].bind(git);
      const internal = git as unknown as { runGitAsRunner: typeof original };
      t.mock.method(internal, "runGitAsRunner", async (...args: Parameters<typeof original>) =>
        args[1].includes("--batch-all-objects") ? output : original(...args));
      assert.equal(await covered(), false);
    });
  }
  it("refuses a topology change even when repacking leaves the same object IDs", async t => {
    const original = git["runGitAsRunner"].bind(git);
    const internal = git as unknown as { runGitAsRunner: typeof original };
    let inventories = 0;
    t.mock.method(internal, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
      if (args[1].includes("--batch-all-objects") && ++inventories === 2)
        command(clonePath, "repack", "-ad");
      return original(...args);
    });
    await unknown();
    assert.equal(inventories, 2);
  });
  for (const failure of ["error", "cancel"] as const) {
    it("retains after inventory reread " + failure, async t => {
      const original = git["runGitAsRunner"].bind(git);
      const internal = git as unknown as { runGitAsRunner: typeof original };
      let inventories = 0;
      t.mock.method(internal, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
        if (args[1].includes("--batch-all-objects") && ++inventories === 2) {
          if (failure === "error") throw new Error("inventory unavailable");
          const value = await original(...args);
          live = false;
          return value;
        }
        return original(...args);
      });
      assert.equal(await covered(), false);
      assert.equal(inventories, 2);
    });
  }
  for (const output of ["not-a-sha\n", "a".repeat(40), "\n"]) {
    it("retains malformed or unterminated trusted closure " + JSON.stringify(output), async t => {
      const original = git["runGitWithEnv"].bind(git);
      const internal = git as unknown as { runGitWithEnv: typeof original };
      t.mock.method(internal, "runGitWithEnv", async (...args: Parameters<typeof original>) =>
        args[1].includes("--no-object-names") ? output : original(...args));
      assert.equal(await covered(), false);
    });
  }
  it("retains on tiny automatic object collection limits", async () => {
    assert.equal(await covered({ maxMetadataRecords: 1 }), false);
    assert.equal(await covered({ maxMetadataBytes: 40 }), false);
  });
  it("accepts a validated own-run owed side commit without changing custody or waiving consumers", async () => {
    await pin(side());
    const before = snapshot();
    assert.equal(await covered(), true);
    assert.ok(nativeLaunches > 0);
    assert.equal(snapshot(), before);
    const inspected = await git.inspectRetainedSources({
      selector: options.selector, barePath: bare, holds: options.holds, lifecycle: options.lifecycle,
      epoch: true, isLivePath: () => false,
    });
    assert.equal(inspected.partial, false);
    assert.ok(inspected.sources[0]!.consumers.includes("owed_pins:1"));
    assert.equal(inspected.sources[0]!.epochSettlement, undefined, "owed consumers prevent strict-plain authority");
    const confirm = inspected.sources[0]!.descriptor;
    const result = await git.discardStoppedSource({ ...options, confirm,
      epochProof: git.createEpochDiscardProof({ ...input, confirm }) });
    assert.equal(result.kind, "refused");
    assert.ok(fs.existsSync(clonePath));
    assert.equal(snapshot(), before);
  });
  it("does not approve another run's owed root or an arbitrary bare ref", async () => {
    const sha = side();
    command(bare, "update-ref", "refs/private/arbitrary", sha);
    assert.equal(await covered(), false);
    await pin(sha, "foreign-run");
    assert.equal(await covered(), false);
  });
  for (const damage of ["missing", "invalid", "unreadable"] as const) {
    it("refuses " + damage + " owed discovery context", async t => {
      await pin(side());
      const dir = path.join(bare, "uzi-owed");
      const name = path.join(dir, fs.readdirSync(dir).find(n => n.startsWith("context-"))!);
      if (damage === "missing") fs.unlinkSync(name);
      if (damage === "invalid") fs.writeFileSync(name, "{}");
      if (damage === "unreadable") {
        const open = fsp.open.bind(fsp);
        t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
          if (args[0] === name) throw Object.assign(new Error("unreadable"), { code: "EACCES" });
          return open(...args);
        });
      }
      assert.equal(await covered(), false);
    });
  }
  it("caps automatic directory reads while preserving legacy enumeration defaults", async t => {
    await pin(side());
    const dir = path.join(bare, "uzi-owed");
    for (let i = 0; i < 40; i++) fs.writeFileSync(path.join(dir,
      ".tmp-" + i.toString(16).padStart(8, "0") + "-0000-4000-8000-000000000000"), "");
    let reads = 0, closes = 0;
    const opendir = fsp.opendir.bind(fsp);
    t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
      const handle = await opendir(...args);
      if (args[0] === dir) {
        assert.equal((args[1] as { bufferSize: number }).bufferSize, 1);
        const read = handle.read.bind(handle), close = handle.close.bind(handle);
        handle.read = (async () => { reads++; return read(); }) as typeof handle.read;
        handle.close = (async () => { closes++; await close(); }) as typeof handle.close;
      }
      return handle;
    });
    // Exercise this collection's own cap directly: a whole content observation now
    // also has an earlier physical-object cap, which must not mask this assertion.
    const enumerate = (limits: { maxMetadataRecords?: number; maxMetadataBytes?: number }) => {
      const proof = git.createEpochDiscardProof(input);
      return git["epochRetained"].run(git["epochLimits"](limits), () =>
        git["withEpochNativeReads"](proof, () => git["enumerateOwedUnderLock"](bare, RUN, undefined,
          { roots: [], checkLive: input.checkLive })));
    };
    await assert.rejects(enumerate({ maxMetadataRecords: 3 }), /owed entry budget/);
    assert.equal(reads, 4);
    assert.equal(closes, 1);
    t.mock.restoreAll();
    assert.equal((await git.enumerateOwedCandidates(bare, RUN)).length, 1);
    await assert.rejects(enumerate({ maxMetadataBytes: 128 }), /automatic owed byte limit/);
  });
  for (const phase of ["open", "read", "opendir"] as const) {
    it("joins the opened handle after cancellation during actual owed " + phase, async t => {
      await pin(side());
      const dir = path.join(bare, "uzi-owed");
      let closes = 0, reads = 0;
      const open = fsp.open.bind(fsp), opendir = fsp.opendir.bind(fsp);
      if (phase === "opendir") {
        t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
          const handle = await opendir(...args);
          if (args[0] === dir) {
            const close = handle.close.bind(handle);
            handle.close = (async () => { closes++; await close(); }) as typeof handle.close;
            live = false;
          }
          return handle;
        });
      } else {
        t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
          const handle = await open(...args);
          if (String(args[0]).startsWith(dir + path.sep)) {
            const close = handle.close.bind(handle), read = handle.read.bind(handle);
            handle.close = async () => { closes++; await close(); };
            handle.read = (async (...readArgs: Parameters<typeof handle.read>) => {
              reads++;
              const value = await read(...readArgs);
              live = false;
              return value;
            }) as typeof handle.read;
            if (phase === "open") live = false;
          }
          return handle;
        });
      }
      assert.equal(await covered(), false);
      assert.equal(closes, 1);
      assert.equal(reads, phase === "read" ? 1 : 0);
    });
  }
});

it("is UNKNOWN for unpublished local refs and objects hidden by a common-directory redirect", async () => {
  const sha = command(clonePath, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unpublished source");
  command(clonePath, "update-ref", "refs/private/side", sha);
  assert.equal(await covered(), false);
  const gitdir = path.join(clonePath, ".git");
  const common = path.join(fx.dataDir, "alternate-common");
  fs.cpSync(gitdir, common, { recursive: true });
  command(common, "--git-dir=.", "update-ref", "-d", "refs/private/side");
  const objectPath = path.join("objects", sha.slice(0, 2), sha.slice(2));
  fs.unlinkSync(path.join(common, objectPath));
  fs.writeFileSync(path.join(gitdir, "commondir"), common + "\n");
  assert.equal(fs.readFileSync(path.join(gitdir, "refs/private/side"), "utf8").trim(), sha);
  assert.equal(fs.existsSync(path.join(gitdir, objectPath)), true);
  const before = fs.readFileSync(path.join(bare, "config"));
  assert.equal(await covered(), false, "automatic coverage must refuse hidden local metadata");
  // A common-directory redirect shares another store: an indirection, so UNKNOWN, never a settled keep.
  assert.equal((await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) })).kind, "unknown");
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
  assert.equal(fs.existsSync(path.join(gitdir, objectPath)), true);
});

for (const shape of ["gitfile", "symlink"] as const) {
  it(`a redirected .git (${shape}) is UNKNOWN, never a settled keep`, async () => {
    const real = path.join(clonePath, ".git"), moved = path.join(clonePath, ".git-real");
    fs.renameSync(real, moved);
    if (shape === "gitfile") fs.writeFileSync(real, `gitdir: ${moved}\n`);
    else fs.symlinkSync(moved, real);
    assert.equal((await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) })).kind, "unknown");
    assert.equal(fs.existsSync(moved), true);
  });
}

function packObjects(cwd: string, ids: string[]): string {
  const dir = path.join(cwd, cwd === bare ? "objects" : ".git/objects", "pack");
  const hash = execFileSync("git", ["-C", cwd, "pack-objects", path.join(dir, "pack")],
    { env: ENV, encoding: "utf8", input: ids.join("\n") + "\n" }).trim();
  return path.join(dir, "pack-" + hash);
}
for (const location of ["source", "bare"] as const) {
  it("refuses an orphan pack in the " + location + " (C1)", async () => {
    const cwd = location === "source" ? clonePath : bare;
    const id = execFileSync("git", ["-C", cwd, "hash-object", "-w", "--stdin"],
      { env: ENV, encoding: "utf8", input: "hidden unpublished pack object" }).trim();
    const packed = packObjects(cwd, [id]);
    fs.unlinkSync(packed + ".idx");
    const root = path.join(cwd, cwd === bare ? "objects" : ".git/objects");
    fs.unlinkSync(path.join(root, id.slice(0, 2), id.slice(2)));
    await unknown();
    assert.equal(journal(), raw);
    assert.ok(fs.existsSync(packed + ".pack"));
  });
}
it("accepts indexed covered packs and keeps indexed unpublished source objects", async () => {
  const ids = command(clonePath, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").split("\n");
  packObjects(clonePath, ids); packObjects(bare, ids);
  assert.equal(await observation(), true);
  const id = execFileSync("git", ["-C", clonePath, "hash-object", "-w", "--stdin"],
    { env: ENV, encoding: "utf8", input: "indexed unpublished" }).trim();
  packObjects(clonePath, [id]);
  assert.equal(await observation(), false);
});
for (const damage of ["corrupt", "mismatch", "temporary", "alternate-gap", "external-alternate"] as const) {
  it("returns completeness UNKNOWN for " + damage, async () => {
    const id = command(clonePath, "rev-parse", "HEAD");
    const packed = packObjects(clonePath, [id]);
    fs.chmodSync(packed + ".idx", 0o600);
    if (damage === "corrupt") {
      const idx = fs.readFileSync(packed + ".idx"); idx[idx.length - 1]! ^= 1;
      fs.writeFileSync(packed + ".idx", idx);
    }
    if (damage === "mismatch") {
      const other = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD^{tree}")]);
      fs.copyFileSync(other + ".idx", packed + ".idx");
    }
    if (damage === "temporary") fs.writeFileSync(path.join(path.dirname(packed), "tmp_pack_unfinished"), "");
    if (damage === "alternate-gap") {
      const alt = path.join(fx.dataDir, "repos", "alternate.git");
      command(bare, "init", "--bare", alt); fs.chmodSync(alt, 0o700);
      const altPack = path.join(alt, "objects", "pack", path.basename(packed));
      fs.copyFileSync(packed + ".pack", altPack + ".pack");
      fs.writeFileSync(path.join(clonePath, ".git/objects/info/alternates"), alt + "/objects\n");
    }
    if (damage === "external-alternate") fs.writeFileSync(path.join(clonePath, ".git/objects/info/alternates"), fx.originPath + "/objects\n");
    await unknown();
  });
}
it("accepts a bounded managed alternate closure and fingerprints bypassed metadata", async () => {
  fs.writeFileSync(path.join(clonePath, ".git/objects/info/alternates"), path.relative(path.join(clonePath, ".git/objects"), path.join(bare, "objects")) + "\n");
  const packed = packObjects(clonePath, [command(clonePath, "rev-parse", "HEAD")]);
  for (const ext of ["keep", "promisor", "rev", "bitmap", "mtimes"]) {
    if (!fs.existsSync(packed + "." + ext)) fs.writeFileSync(packed + "." + ext, "");
  }
  fs.writeFileSync(path.join(path.dirname(packed), "multi-pack-index"), "bypassed invalid MIDX");
  fs.writeFileSync(path.join(clonePath, ".git/objects/info/commit-graph"), "bypassed invalid graph");
  const incremental = path.join(path.dirname(packed), "multi-pack-index.d");
  fs.mkdirSync(incremental);
  fs.writeFileSync(path.join(incremental, "multi-pack-index-chain"), "bypassed chain");
  for (const ext of ["midx", "rev", "bitmap"])
    fs.writeFileSync(path.join(incremental, "multi-pack-index-" + "a".repeat(40) + "." + ext), "bypassed");
  const graphs = path.join(clonePath, ".git/objects/info/commit-graphs");
  fs.mkdirSync(graphs);
  fs.writeFileSync(path.join(graphs, "commit-graph-chain"), "bypassed chain");
  fs.writeFileSync(path.join(graphs, "graph-" + "a".repeat(40) + ".graph"), "bypassed graph");
  // The clone's `.git/config` admits only the keys uzi writes (strict allowlist in the automatic
  // worktree proof), so these object-store tuning keys can only be planted in the bare here; the
  // clone side is covered by epoch-worktree-proof.test.ts, where any such key is a settled keep.
  for (const cwd of [bare]) {
    for (const key of ["core.multiPackIndex", "core.commitGraph", "pack.readReverseIndex", "pack.writeReverseIndex"])
      command(cwd, "config", key, "true");
    command(cwd, "config", "pack.threads", "32");
  }
  const files = [packed + ".rev", packed + ".bitmap", packed + ".mtimes"];
  const before = files.map(file => ({ bytes: fs.readFileSync(file).toString("hex"), mode: fs.statSync(file).mode }));
  assert.equal(await observation(), true);
  assert.deepEqual(files.map(file => ({ bytes: fs.readFileSync(file).toString("hex"), mode: fs.statSync(file).mode })), before);
});
it("joins object-directory handles when opening completes after cancellation", async t => {
  const root = path.join(clonePath, ".git/objects");
  const open = fsp.opendir.bind(fsp); let closes = 0;
  t.mock.method(fsp, "opendir", async (...args: Parameters<typeof fsp.opendir>) => {
    const handle = await open(...args);
    if (args[0] === root) {
      const close = handle.close.bind(handle);
      handle.close = async () => { closes++; await close(); }; live = false;
    }
    return handle;
  });
  await unknown(); assert.equal(closes, 1);
});

for (const location of ["source", "bare"] as const) {
  it("accepts complete indexed managed alternates from " + location, async () => {
    const alt = path.join(path.dirname(bare), "complete-alt.git");
    command(bare, "init", "--bare", alt); fs.chmodSync(alt, 0o700);
    const root = path.join(location === "source" ? clonePath + "/.git" : bare, "objects");
    const ids = command(clonePath, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").split("\n");
    const packed = packObjects(clonePath, ids);
    for (const ext of [".pack", ".idx"])
      fs.copyFileSync(packed + ext, path.join(alt, "objects/pack", path.basename(packed) + ext));
    fs.writeFileSync(path.join(root, "info/alternates"), alt + "/objects\n");
    assert.equal(await observation(), true);
    if (location === "bare") await assert.rejects(git.enumerateOwedCandidates(bare, RUN), /layout_unsupported/);
  });
}
for (const damage of ["unreadable", "zlib", "header", "size", "hash", "trailing"] as const) {
  it("returns UNKNOWN for a physical loose " + damage + " despite a healthy bare copy", async t => {
    const id = command(clonePath, "rev-parse", "HEAD:README.md");
    const loose = path.join(clonePath, ".git/objects", id.slice(0, 2), id.slice(2));
    const healthy = fs.readFileSync(path.join(bare, "objects", id.slice(0, 2), id.slice(2)));
    fs.mkdirSync(path.dirname(loose), { recursive: true });
    // Give this test its own physical copy; do not corrupt a fixture hardlink or alternate.
    if (fs.existsSync(loose)) fs.unlinkSync(loose);
    fs.writeFileSync(loose, healthy, { flag: "wx" });
    assert.equal(await observation(), true, "the healthy local occurrence must be observed first");
    if (damage === "unreadable") {
      const open = fsp.open.bind(fsp);
      t.mock.method(fsp, "open", async (...args: Parameters<typeof fsp.open>) => {
        if (args[0] === loose) throw Object.assign(new Error("owned unreadable seam"), { code: "EACCES" });
        return open(...args);
      });
    } else if (damage === "trailing") {
      fs.appendFileSync(loose, "trailing");
    } else {
      const body = damage === "header" ? "blob 01\0x" : damage === "size" ? "blob 2\0x" : "blob 1\0x";
      fs.writeFileSync(loose, damage === "zlib" ? Buffer.from("broken") : deflateSync(body));
    }
    await unknown();
    assert.equal(journal(), raw);
    assert.ok(fs.existsSync(loose));
  });
}
for (const mutation of ["config", "entries", "alternates"] as const) {
  it("returns UNKNOWN on late S1 " + mutation + " changes", async t => {
    const original = git["runGitAsRunner"].bind(git);
    let inventories = 0;
    t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
      const value = await original(...args);
      if (args[1].includes("--batch-all-objects") && ++inventories === 2) {
        if (mutation === "config") fs.appendFileSync(path.join(bare, "config"), "\n[audit]\n value = late\n");
        if (mutation === "entries") fs.writeFileSync(path.join(bare, "objects/pack/tmp_pack_late"), "");
        if (mutation === "alternates") fs.writeFileSync(path.join(bare, "objects/info/alternates"), "/missing/objects\n");
      }
      return value;
    });
    await unknown();
  });
}
it("rejects a silent exit-zero partial native inventory against physical membership", async t => {
  const original = git["runGitAsRunner"].bind(git);
  t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
    const out = await original(...args);
    return args[1].includes("--batch-all-objects") ? out.split("\n").slice(1).join("\n") : out;
  });
  await unknown();
});
for (const location of ["source", "bare"] as const) {
  it("core stops on " + location + " orphan completeness UNKNOWN without deleting or retiring", async () => {
    const cwd = location === "source" ? clonePath : bare;
    const packed = packObjects(cwd, [command(clonePath, "rev-parse", "HEAD")]);
    fs.unlinkSync(packed + ".idx");
    const config = fs.readFileSync(path.join(bare, "config"));
    const ledger = await git["readAllAttemptLedgerRaw"](bare);
    const result = await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) });
    assert.equal(result.kind, "unknown");
    assert.equal(scope.stopped(), true);
    assert.ok(fs.existsSync(clonePath));
    assert.ok(fs.existsSync(packed + ".pack"));
    assert.deepEqual(fs.readFileSync(path.join(bare, "config")), config);
    assert.deepEqual(await git["readAllAttemptLedgerRaw"](bare), ledger);
  });
}
it("reports comparable loose and indexed verification time excluding fixture initialization", async () => {
  const start = performance.now();
  assert.equal(await observation(), true);
  const looseMs = performance.now() - start;
  const ids = command(clonePath, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").split("\n");
  const packs = [packObjects(clonePath, ids), packObjects(bare, ids)];
  const packedStart = performance.now();
  assert.equal(await observation(), true);
  console.log(JSON.stringify({ verificationOnly: true, fixtureInitializationExcluded: true,
    looseMs: Math.round(looseMs), indexedMs: Math.round(performance.now() - packedStart),
    pairCount: packs.length, physicalPackBytes: packs.reduce((n, p) => n + fs.statSync(p + ".pack").size, 0) }));
});

it("the shared core deletes clean covered indexed content and settles its metadata", async () => {
  const ids = command(clonePath, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)").split("\n");
  packObjects(clonePath, ids); packObjects(bare, ids);
  const result = await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) });
  assert.equal(result.kind, "done");
  assert.equal("metadata" in result && result.metadata, "retired");
  assert.equal(fs.existsSync(clonePath), false);
  assert.equal(scope.stopped(), false);
  scope.assertSettled();
});

it("treats a source include as UNKNOWN", async () => {
  command(clonePath, "config", "include.path", path.join(fx.dataDir, "not-followed-source-config"));
  const before = fs.readFileSync(path.join(bare, "config"));
  const result = await git.discardStoppedSource({ ...options, epochProof: git.createEpochDiscardProof(input) });
  assert.equal(result.kind, "unknown");
  assert.ok(fs.existsSync(clonePath));
  assert.deepEqual(fs.readFileSync(path.join(bare, "config")), before);
});

it("treats a bare include as UNKNOWN", async () => {
  command(bare, "config", "include.path", path.join(fx.dataDir, "unknown-bare-config"));
  await unknown();
});
