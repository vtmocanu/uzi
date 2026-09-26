import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import {
  GitCache,
  RecoveryBundleTooLargeError,
  RECOVERY_BUNDLE_REF,
  RECOVERY_CHUNK_BYTES,
  RECOVERY_MAX_BUNDLE_BYTES,
} from "../src/git.js";

// PRD #1296 M3 (D1/D5/D6) — the WORKER bundle producer (`GitCache.produceRecoveryBundle`).
//
// These tests exercise the producer against LOCAL bare git fixtures (no DB, no network):
// a public "forge" bare, a trusted "worker" bare carrying the original committed head H,
// and a CLEAN clone supplied ONLY with the verified public prerequisite closure. The
// authoritative D5 proof is that the produced bundle imports into that clean clone —
// which never borrows the worker's private object store — and reproduces H with identical
// commit IDs, parents, tree and binary contents.

// Isolate every git invocation from host/global config so init.defaultBranch, a global
// gpg-sign, or a missing identity on the runner cannot perturb the fixture.
const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

function git(cwd: string, args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
}

function gitBuf(cwd: string, args: string[]): Buffer {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, maxBuffer: 64 * 1024 * 1024 });
}

/** Init a working repo on `main` with a stable identity and no detached maintenance. */
function initWork(dir: string): void {
  fs.mkdirSync(dir, { recursive: true });
  execFileSync("git", ["init", "-b", "main", dir], { env: GIT_ENV, stdio: "pipe" });
  git(dir, ["config", "user.email", "fixture@uzi.local"]);
  git(dir, ["config", "user.name", "fixture"]);
  git(dir, ["config", "commit.gpgsign", "false"]);
  git(dir, ["config", "maintenance.auto", "false"]);
  git(dir, ["config", "gc.auto", "0"]);
  git(dir, ["config", "core.fsmonitor", "false"]);
}

function initBare(dir: string): void {
  execFileSync("git", ["init", "--bare", "-b", "main", dir], { env: GIT_ENV, stdio: "pipe" });
  // An identity so `git commit-tree` (used by the trap fixture to synthesize checkpoint /
  // aligned commits directly in the bare) has an author/committer.
  git(dir, ["config", "user.email", "fixture@uzi.local"]);
  git(dir, ["config", "user.name", "fixture"]);
  git(dir, ["config", "commit.gpgsign", "false"]);
  git(dir, ["config", "maintenance.auto", "false"]);
  git(dir, ["config", "gc.auto", "0"]);
}

// A deterministic non-UTF-8 binary blob (all 256 byte values, twice) so the clean-clone
// import proves BINARY content survives byte-for-byte, not just text.
const BINARY = Buffer.from(Array.from({ length: 512 }, (_, i) => i % 256));

interface Topology {
  base: string;
  workDir: string;
  forgeBare: string;
  workerBare: string;
  /** The base commit B, published on the forge default branch (the merge-base). */
  baseSha: string;
  /** The original committed head H on the run branch (private to the worker bare). */
  headSha: string;
  branch: string;
}

/**
 * Build the standard topology: forge `main` at base commit B; the worker bare carries a
 * run branch whose tip H = B + a text commit + a binary commit. H is NOT on the forge.
 */
function makeTopology(): Topology {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-recovery-producer-"));
  const workDir = path.join(base, "work");
  const forgeBare = path.join(base, "forge.git");
  const workerBare = path.join(base, "worker.git");
  const branch = "agent/run-1";

  initWork(workDir);
  initBare(forgeBare);
  initBare(workerBare);

  fs.writeFileSync(path.join(workDir, "README.md"), "# fixture\n");
  git(workDir, ["add", "."]);
  git(workDir, ["commit", "-m", "base commit"]);
  const baseSha = git(workDir, ["rev-parse", "HEAD"]);

  git(workDir, ["remote", "add", "forge", forgeBare]);
  git(workDir, ["remote", "add", "worker", workerBare]);
  // Publish B on the forge default branch and mirror it into the worker bare.
  git(workDir, ["push", "forge", "main"]);
  git(workDir, ["push", "worker", "main"]);

  // Committed run work on a branch: a text commit then a binary commit → H.
  git(workDir, ["checkout", "-b", branch]);
  fs.mkdirSync(path.join(workDir, "src"), { recursive: true });
  fs.writeFileSync(path.join(workDir, "src", "app.txt"), "the committed implementation\n");
  git(workDir, ["add", "."]);
  git(workDir, ["commit", "-m", "feat: implementation"]);
  fs.mkdirSync(path.join(workDir, "assets"), { recursive: true });
  fs.writeFileSync(path.join(workDir, "assets", "logo.bin"), BINARY);
  git(workDir, ["add", "."]);
  git(workDir, ["commit", "-m", "feat: binary asset"]);
  const headSha = git(workDir, ["rev-parse", "HEAD"]);

  // The run branch (H) goes ONLY into the trusted worker bare, never to the forge.
  git(workDir, ["push", "worker", `${branch}:${branch}`]);

  return { base, workDir, forgeBare, workerBare, baseSha, headSha, branch };
}

/** A fresh clone of the forge (the verified public prerequisite closure ONLY). It has
 *  NO connection to the worker bare, so importing the bundle must supply H's objects. */
function cleanForgeClone(topo: Topology, name: string): string {
  const dst = path.join(topo.base, name);
  execFileSync("git", ["clone", topo.forgeBare, dst], { env: GIT_ENV, stdio: "pipe" });
  git(dst, ["config", "maintenance.auto", "false"]);
  git(dst, ["config", "gc.auto", "0"]);
  return dst;
}

function objectPresent(repo: string, sha: string): boolean {
  try {
    execFileSync("git", ["-C", repo, "cat-file", "-e", `${sha}^{commit}`], {
      env: GIT_ENV,
      stdio: "pipe",
    });
    return true;
  } catch {
    return false;
  }
}

/** Import a bundle's single named ref into `repo` under refs/heads/recovered-source. */
function importBundle(repo: string, bundlePath: string): void {
  git(repo, ["fetch", bundlePath, `${RECOVERY_BUNDLE_REF}:${RECOVERY_BUNDLE_REF}`]);
}

let topo: Topology;
let cache: GitCache;

beforeEach(() => {
  topo = makeTopology();
  cache = new GitCache(path.join(topo.base, "data"), nullLogger(), undefined, testGitCacheOptions());
});

afterEach(() => {
  fs.rmSync(topo.base, { recursive: true, force: true, maxRetries: 10, retryDelay: 50 });
});

describe("produceRecoveryBundle — clean-clone import (PRD #1296 D5)", () => {
  it("imports into a clean forge clone and reproduces H with identical IDs, parents, tree and binary", async () => {
    const outPath = path.join(topo.base, "out.bundle");
    const result = await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha,
      outPath,
      forgeTip: topo.baseSha,
    });

    // The manifest facts the uploader will bind + journal.
    assert.equal(result.sourceSha, topo.headSha);
    assert.deepEqual(result.prerequisiteShas, [topo.baseSha], "prereq = merge-base(H, forge tip) = B");
    assert.equal(result.selfContained, false);
    assert.equal(result.alreadyPublished, false);
    assert.equal(result.bundlePath, outPath);
    assert.ok(result.byteSize > 0);
    assert.match(result.checksum, /^[0-9a-f]{64}$/);
    assert.equal(result.chunkCount, Math.max(1, Math.ceil(result.byteSize / RECOVERY_CHUNK_BYTES)));
    // The recorded checksum/size describe THESE exact bytes.
    const bytes = fs.readFileSync(outPath);
    assert.equal(result.byteSize, bytes.length);

    // The clean clone has ONLY the public prerequisite; H's objects are absent until import.
    const clean = cleanForgeClone(topo, "clean");
    assert.equal(objectPresent(clean, topo.baseSha), true, "clean clone has the public base B");
    assert.equal(objectPresent(clean, topo.headSha), false, "clean clone does NOT have H before import");

    importBundle(clean, outPath);

    // Identical commit IDs: the imported ref IS H.
    assert.equal(git(clean, ["rev-parse", RECOVERY_BUNDLE_REF]), topo.headSha);
    // Identical commit IDs + parents + tree across the whole recovered history: comparing
    // %H (commit) %T (tree) %P (parents) for every commit is an exhaustive equality proof.
    const shape = (repo: string, ref: string) =>
      git(repo, ["log", "--format=%H %T %P", `${topo.baseSha}..${ref}`]);
    assert.equal(shape(clean, RECOVERY_BUNDLE_REF), shape(topo.workDir, topo.branch));
    // Identical top tree.
    assert.equal(
      git(clean, ["rev-parse", `${RECOVERY_BUNDLE_REF}^{tree}`]),
      git(topo.workDir, ["rev-parse", `${topo.branch}^{tree}`]),
    );
    // Identical BINARY contents, byte-for-byte, out of the imported object graph.
    const recovered = gitBuf(clean, ["cat-file", "blob", `${RECOVERY_BUNDLE_REF}:assets/logo.bin`]);
    assert.ok(recovered.equals(BINARY), "recovered binary blob is byte-identical");
  });

  it("produces a self-contained bundle (no forge prerequisite) that imports into an EMPTY repo", async () => {
    const outPath = path.join(topo.base, "self.bundle");
    // No forgeTip supplied → self-contained (D5 fallback within the size limit).
    const result = await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha,
      outPath,
    });
    assert.deepEqual(result.prerequisiteShas, []);
    assert.equal(result.selfContained, true);
    assert.equal(result.alreadyPublished, false);

    // A brand-new empty repo — no forge, no worker objects at all.
    const empty = path.join(topo.base, "empty");
    initWork(empty);
    assert.equal(objectPresent(empty, topo.baseSha), false);
    importBundle(empty, outPath);
    assert.equal(git(empty, ["rev-parse", RECOVERY_BUNDLE_REF]), topo.headSha);
    // Full history is present: even the base commit B is reachable in a self-contained bundle.
    assert.equal(objectPresent(empty, topo.baseSha), true);
    const recovered = gitBuf(empty, ["cat-file", "blob", `${RECOVERY_BUNDLE_REF}:assets/logo.bin`]);
    assert.ok(recovered.equals(BINARY));
  });
});

describe("produceRecoveryBundle — the private-base / checkpoint trap (PRD #1296 D1/D5)", () => {
  it("uses ORIGINAL H and the FORGE merge-base, never a private origin/<default> or an aligned H'", async () => {
    // Plant the traps the runner's private bare could contain: a private origin/main
    // pointing at an unpublished checkpoint, and a post-align H' — both distinct from H,
    // both NOT on the forge. A producer that wrongly used either as the prerequisite (or
    // exported it instead of H) would break the clean-forge-clone import below.
    // 1) A checkpoint commit built privately on B, tracked as origin/main. Synthesized
    //    directly IN the worker bare so its objects live there (as a resumed checkpoint would).
    const checkpoint = git(topo.workerBare, ["commit-tree", `${topo.baseSha}^{tree}`, "-p", topo.baseSha, "-m", "private checkpoint"]);
    git(topo.workerBare, ["update-ref", "refs/remotes/origin/main", checkpoint]);
    // 2) An aligned H' (rebased onto the checkpoint), tracked as the post-align branch ref.
    const alignedTree = git(topo.workerBare, ["rev-parse", `${topo.headSha}^{tree}`]);
    const alignedHprime = git(topo.workerBare, ["commit-tree", alignedTree, "-p", checkpoint, "-m", "aligned H-prime"]);
    git(topo.workerBare, ["update-ref", `refs/heads/${topo.branch}`, alignedHprime]);
    assert.notEqual(alignedHprime, topo.headSha, "H' differs from H");
    assert.notEqual(checkpoint, topo.baseSha);

    const outPath = path.join(topo.base, "trap.bundle");
    const result = await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha, // the runner pins the pre-align ORIGINAL H
      outPath,
      forgeTip: topo.baseSha, // the FRESH forge tip, resolved via fetchDefaultTip
    });
    // Prereq is the forge merge-base B, NOT the private checkpoint.
    assert.deepEqual(result.prerequisiteShas, [topo.baseSha]);
    assert.equal(result.sourceSha, topo.headSha);

    // A clean forge clone (which has neither the checkpoint nor H') imports H fine.
    const clean = cleanForgeClone(topo, "trap-clean");
    assert.equal(objectPresent(clean, checkpoint), false, "clean clone lacks the private checkpoint");
    assert.equal(objectPresent(clean, alignedHprime), false, "clean clone lacks the aligned H'");
    importBundle(clean, outPath);
    // The imported ref is the ORIGINAL H, not H'.
    assert.equal(git(clean, ["rev-parse", RECOVERY_BUNDLE_REF]), topo.headSha);
    assert.notEqual(git(clean, ["rev-parse", RECOVERY_BUNDLE_REF]), alignedHprime);
  });

  it("reports alreadyPublished (nothing to archive) when H is fully reachable from the forge tip", async () => {
    // Publish the run branch to the forge, so H IS the forge tip → merge-base(H, tip) == H.
    git(topo.workDir, ["push", "forge", `${topo.branch}:${topo.branch}`]);
    const outPath = path.join(topo.base, "published.bundle");
    const result = await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha,
      outPath,
      forgeTip: topo.headSha, // the forge already carries H
    });
    assert.equal(result.alreadyPublished, true);
    assert.equal(result.bundlePath, "");
    assert.equal(fs.existsSync(outPath), false, "no bundle file is written when nothing needs archiving");
  });
});

describe("produceRecoveryBundle — leakage & size bounds (PRD #1296 D5/D6)", () => {
  it("pins the D4 byte/chunk limits (64 MiB max bundle, ~1 MiB chunk)", () => {
    // The worker's produce ceiling must not exceed the API's 64 MiB max (D4), and the
    // chunk size must agree with the server's split so chunk_count is verifiable.
    assert.equal(RECOVERY_MAX_BUNDLE_BYTES, 64 * 1024 * 1024);
    assert.equal(RECOVERY_CHUNK_BYTES, 1024 * 1024);
  });


  it("exports EXACTLY the single named source ref — no --all, remotes, tags or reflogs", async () => {
    // Add refs that MUST NOT leak: a tag, an unrelated branch, and a remote-tracking ref.
    git(topo.workerBare, ["tag", "v-secret", topo.baseSha]);
    git(topo.workerBare, ["update-ref", "refs/heads/other-branch", topo.baseSha]);
    git(topo.workerBare, ["update-ref", "refs/remotes/origin/main", topo.headSha]);

    const outPath = path.join(topo.base, "single.bundle");
    await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha,
      outPath,
      forgeTip: topo.baseSha,
    });
    const heads = execFileSync("git", ["bundle", "list-heads", outPath], { env: GIT_ENV, encoding: "utf8" })
      .trim()
      .split("\n")
      .map((l) => l.trim())
      .filter(Boolean);
    assert.equal(heads.length, 1, "the bundle carries exactly one ref");
    assert.match(heads[0]!, new RegExp(`\\s${RECOVERY_BUNDLE_REF}$`));
    // None of the planted refs leaked.
    for (const line of heads) {
      assert.doesNotMatch(line, /refs\/tags\//);
      assert.doesNotMatch(line, /refs\/remotes\//);
      assert.doesNotMatch(line, /other-branch/);
    }
  });

  it("does not capture UNCOMMITTED working-tree files (a bundle is only committed objects)", async () => {
    // An uncommitted file in the work tree is never in the bare, so it cannot enter a
    // bundle. Prove the recovered tree contains only committed paths.
    fs.writeFileSync(path.join(topo.workDir, "SECRET_UNCOMMITTED.txt"), "do not export me\n");
    const outPath = path.join(topo.base, "clean.bundle");
    await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha,
      outPath,
      forgeTip: topo.baseSha,
    });
    const clean = cleanForgeClone(topo, "noleak-clean");
    importBundle(clean, outPath);
    const files = git(clean, ["ls-tree", "-r", "--name-only", RECOVERY_BUNDLE_REF]).split("\n");
    assert.deepEqual(files.sort(), ["README.md", "assets/logo.bin", "src/app.txt"]);
    assert.ok(!files.includes("SECRET_UNCOMMITTED.txt"));
  });

  it("removes the transient named ref from the bare after producing (no namespace residue)", async () => {
    const outPath = path.join(topo.base, "residue.bundle");
    await cache.produceRecoveryBundle(topo.workerBare, {
      sourceSha: topo.headSha,
      outPath,
      forgeTip: topo.baseSha,
    });
    const refs = git(topo.workerBare, ["for-each-ref", "--format=%(refname)"]);
    assert.ok(!refs.includes(RECOVERY_BUNDLE_REF), "the transient recovered-source ref is deleted");
  });

  it("throws RecoveryBundleTooLargeError (retain custody, not truncate) when over the byte limit", async () => {
    const outPath = path.join(topo.base, "toobig.bundle");
    await assert.rejects(
      () =>
        cache.produceRecoveryBundle(topo.workerBare, {
          sourceSha: topo.headSha,
          outPath,
          forgeTip: topo.baseSha,
          maxBytes: 1, // any real bundle exceeds 1 byte
        }),
      (err: unknown) => {
        assert.ok(err instanceof RecoveryBundleTooLargeError);
        assert.equal(err.maxBytes, 1);
        assert.ok(err.byteSize > 1);
        return true;
      },
    );
    // The oversized file is cleaned up rather than left as a partial artifact.
    assert.equal(fs.existsSync(outPath), false);
  });

  it("refuses a source SHA that is not present in the trusted bare", async () => {
    const outPath = path.join(topo.base, "missing.bundle");
    await assert.rejects(
      () =>
        cache.produceRecoveryBundle(topo.workerBare, {
          sourceSha: "0000000000000000000000000000000000000000",
          outPath,
          forgeTip: topo.baseSha,
        }),
      /source commit is not present/,
    );
  });
});
