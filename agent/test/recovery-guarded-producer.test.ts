import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { GitCache, RECOVERY_BUNDLE_REF, RECOVERY_MAX_BUNDLE_BYTES, RecoveryBundleTooLargeError,
  readRecoveryBundleHeader, type PositiveOwedCandidateContext } from "../src/git.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// Fixtures and every subprocess ignore inherited Git routing, identities and config.
const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_")));
Object.assign(env, {
  GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
  GIT_AUTHOR_NAME: "fixture", GIT_AUTHOR_EMAIL: "fixture@example.com",
  GIT_COMMITTER_NAME: "fixture", GIT_COMMITTER_EMAIL: "fixture@example.com",
  GIT_AUTHOR_DATE: "2000-01-01T00:00:00Z", GIT_COMMITTER_DATE: "2000-01-01T00:00:00Z",
});
function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["--no-replace-objects", "-C", cwd, ...args],
    { env, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 60000 }).trim();
}
function randomBytes(size: number): Buffer {
  const bytes = Buffer.alloc(size);
  let state = 0x12345678;
  for (let i = 0; i < size; i++) {
    state ^= state << 13; state ^= state >>> 17; state ^= state << 5;
    bytes[i] = state & 255;
  }
  return bytes;
}
function fixture(size = 256 * 1024, publicSubject = "public deletion") {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "guarded-acceptance-"));
  const data = path.join(root, "data");
  const bare = path.join(data, "repos", "worker.git");
  const work = path.join(root, "work"), forge = path.join(root, "forge.git");
  fs.mkdirSync(path.dirname(bare), { recursive: true, mode: 0o700 });
  fs.mkdirSync(work);
  git(root, "init", "--bare", "-b", "main", forge);
  git(root, "init", "--bare", "-b", "main", bare);
  fs.chmodSync(bare, 0o700);
  git(work, "init", "-b", "main");
  for (const repo of [work, bare, forge]) {
    git(repo, "config", "gc.auto", "0");
    git(repo, "config", "maintenance.auto", "false");
    git(repo, "config", "commit.gpgsign", "false");
  }
  fs.writeFileSync(path.join(work, "large"), randomBytes(size));
  git(work, "add", "large"); git(work, "commit", "-m", "public large history");
  const base = git(work, "rev-parse", "HEAD");
  fs.unlinkSync(path.join(work, "large"));
  fs.writeFileSync(path.join(work, "current"), "public small tree\n");
  git(work, "add", "-A"); git(work, "commit", "-m", publicSubject);
  const tip = git(work, "rev-parse", "HEAD");
  git(work, "push", forge, "main");
  // Populate the admissible cache through a real fetch, rather than planting a ref.
  git(bare, "remote", "add", "origin", forge);
  git(bare, "fetch", "origin", "refs/heads/main:refs/remotes/origin/main");
  fs.writeFileSync(path.join(work, "private"), "current private contents\n");
  git(work, "add", "private"); git(work, "commit", "-m", "current private head");
  const head = git(work, "rev-parse", "HEAD");
  git(work, "push", bare, "HEAD:refs/heads/current");
  const tree = git(bare, "rev-parse", head + "^{tree}");
  const cache = new GitCache(data, nullLogger(), undefined, testGitCacheOptions());
  const calls: string[][] = [];
  const internals = cache as unknown as { runGit(cwd: string, args: string[]): Promise<string> };
  const original = internals.runGit.bind(cache);
  internals.runGit = async (cwd, args) => { calls.push([...args]); return original(cwd, args); };
  return { root, bare, forge, work, cache, base, tip, head, tree, calls };
}
type Fixture = ReturnType<typeof fixture>;
function commit(f: Fixture, tree: string, parents: string[], message: string): string {
  return git(f.bare, "commit-tree", tree, ...parents.flatMap(p => ["-p", p]), "-m", message);
}
function context(f: Fixture): PositiveOwedCandidateContext {
  return { runId: "run-acceptance", generation: 1, kind: "issue", branch: "current",
    barePath: f.bare, defaultIdentity: { ref: "refs/remotes/origin/main", sha: f.tip } };
}
async function produce(f: Fixture, sourceSha: string, name: string, maxBytes?: number, branch = "main") {
  // A network attempt cannot succeed during capture; restoration is always attempted.
  // One rename each way bounds this guard, and a restore failure fails this test.
  fs.renameSync(f.forge, f.forge + ".offline");
  f.calls.length = 0;
  try {
    const result = await f.cache.produceRecoveryBundle(f.bare, {
      sourceSha, outPath: path.join(f.root, name + ".bundle"), guardedDefaultBranch: branch,
      ...(maxBytes === undefined ? {} : { maxBytes }),
    });
    assert.ok(f.calls.every(args => !args.includes("fetch") &&
      !args.some(arg => /credential|extraheader|https?:/i.test(arg))), "capture uses no fetch or credentialed args");
    return result;
  } finally { fs.renameSync(f.forge + ".offline", f.forge); }
}
function fresh(f: Fixture, name: string): string {
  const repo = path.join(f.root, name);
  git(f.root, "clone", "--no-local", f.forge, repo);
  assert.equal(git(repo, "rev-parse", "--is-shallow-repository"), "false");
  assert.equal(fs.existsSync(path.join(repo, ".git", "objects", "info", "alternates")), false);
  assert.equal(git(repo, "for-each-ref", "--format=%(refname)", "refs/uzi"), "");
  return repo;
}
function importAndCheck(f: Fixture, repo: string, source: string, roots: string[], bundle: string, deps: string[]) {
  // Enumerate the actual unpublished closure, including blobs/trees, before import.
  const unpublished = git(f.bare, "rev-list", "--objects", source, ...deps.map(d => "^" + d))
    .split("\n").map(line => line.split(" ")[0]!);
  for (const oid of unpublished) assert.throws(() => git(repo, "cat-file", "-e", oid), "private object absent: " + oid);
  for (const oid of deps) assert.equal(git(repo, "cat-file", "-t", oid), "commit");
  git(repo, "bundle", "verify", bundle);
  git(repo, "fetch", bundle, RECOVERY_BUNDLE_REF + ":refs/heads/recovered-source");
  assert.equal(git(repo, "rev-parse", "recovered-source"), source);
  for (const root of roots) {
    git(repo, "merge-base", "--is-ancestor", root, "recovered-source");
    assert.equal(git(repo, "cat-file", "-t", root), "commit");
    assert.equal(git(repo, "show", root + ":private"), "current private contents");
  }
  for (const oid of unpublished) git(repo, "cat-file", "-e", oid);
  assert.equal(git(repo, "rev-parse", "recovered-source^{tree}"), f.tree);
  assert.equal(git(repo, "show", "recovered-source:private"), "current private contents");
}
function cleanup(f: Fixture) { fs.rmSync(f.root, { recursive: true, force: true }); }

it("production 64MiB cap captures real coverage of divergent retained roots into a fresh forge clone", async () => {
  const start = performance.now(), f = fixture(65 * 1024 * 1024);
  try {
    const divergent = commit(f, f.tree, [f.base], "retained older-base output");
    const unrelated = commit(f, f.tree, [], "retained unrelated output");
    const roots = [divergent, unrelated];
    // Owned contexts are admitted through the public tracking interface.
    for (const [i, sha] of roots.entries()) {
      const c = { ...context(f), generation: i + 2, branch: "retained-" + i };
      assert.equal((await f.cache.updateTrackingRef(f.bare, c.branch, sha, { context: c })).kind, "updated");
      assert.equal((await f.cache.committedTrackingOwnership(f.bare, c.branch, c.runId, sha, c.generation)).kind, "owned");
    }
    const coverage = await f.cache.buildRecoveryCoverage(f.bare, context(f), roots, f.head);
    const full = await f.cache.produceRecoveryBundle(f.bare, {
      sourceSha: coverage.sha, outPath: path.join(f.root, "full.bundle"), maxBytes: 80 * 1024 * 1024,
    });
    assert.equal(full.selfContained, true);
    assert.ok(full.byteSize > RECOVERY_MAX_BUNDLE_BYTES);
    const thin = await produce(f, coverage.sha, "thin");
    assert.ok(thin.byteSize < RECOVERY_MAX_BUNDLE_BYTES);
    assert.equal(thin.alreadyPublished, false);
    assert.equal(thin.selfContained, false);
    // Git advertises the older public boundary as well as the selected merge base.
    assert.deepEqual(thin.prerequisiteShas, [f.base, f.tip].sort());
    assert.deepEqual(await readRecoveryBundleHeader(thin.bundlePath, coverage.sha),
      { prerequisiteShas: thin.prerequisiteShas, selfContained: false });
    importAndCheck(f, fresh(f, "fresh"), coverage.sha, [...roots, f.head], thin.bundlePath, thin.prerequisiteShas);
    console.log(JSON.stringify({ case: "production", capBytes: RECOVERY_MAX_BUNDLE_BYTES,
      fullBytes: full.byteSize, thinBytes: thin.byteSize, runtimeMs: Math.round(performance.now() - start) }));
  } finally { cleanup(f); }
});

for (const guarded of [true, false]) for (const [label, subject] of [
  ["Unicode", "公開された履歴"], ["TAB", "public\tdeletion"],
] as const) {
  it(`${guarded ? "guarded" : "legacy"} thin ${label} prerequisite imports into a fresh forge clone`, async () => {
    const f = fixture(256 * 1024, subject);
    try {
      const bundle = guarded
        ? await produce(f, f.head, "unicode-guarded", 32 * 1024)
        : await f.cache.produceRecoveryBundle(f.bare, {
          sourceSha: f.head, outPath: path.join(f.root, "unicode-legacy.bundle"),
          forgeTip: f.tip, maxBytes: 32 * 1024,
        });
      const repo = fresh(f, "unicode-import");
      git(repo, "bundle", "verify", bundle.bundlePath);
      assert.ok(fs.readFileSync(bundle.bundlePath).includes(Buffer.from(subject)));
      assert.deepEqual(await readRecoveryBundleHeader(bundle.bundlePath, f.head),
        { prerequisiteShas: [f.tip], selfContained: false });
      assert.equal(bundle.selfContained, false);
      assert.deepEqual(bundle.prerequisiteShas, [f.tip]);
      importAndCheck(f, repo, f.head, [f.head], bundle.bundlePath, bundle.prerequisiteShas);
    } finally { cleanup(f); }
  });
}

it("criss-cross public graph declares all actual sorted dependencies and imports both merge bases", async () => {
  const f = fixture();
  try {
    const left = commit(f, f.tree, [f.tip], "left");
    const right = commit(f, f.tree, [f.tip], "right");
    const publicMerge = commit(f, f.tree, [right, left], "public merge");
    const privateMerge = commit(f, f.tree, [left, right], "private merge");
    git(f.bare, "push", f.forge, publicMerge + ":refs/heads/main");
    git(f.bare, "fetch", "origin", "refs/heads/main:refs/remotes/origin/main");
    assert.deepEqual(git(f.bare, "merge-base", "--all", publicMerge, privateMerge).split("\n").sort(), [left, right].sort());
    const thin = await produce(f, privateMerge, "cross", 32 * 1024);
    assert.deepEqual(thin.prerequisiteShas, [left, right].sort());
    importAndCheck(f, fresh(f, "fresh"), privateMerge, [privateMerge], thin.bundlePath, thin.prerequisiteShas);
  } finally { cleanup(f); }
});

it("cached public H is still captured using H parents; cached root H keeps the oversized result", async () => {
  const f = fixture();
  try {
    const thin = await produce(f, f.tip, "public", 32 * 1024);
    assert.equal(thin.alreadyPublished, false);
    assert.equal(thin.sourceSha, f.tip);
    assert.deepEqual(thin.prerequisiteShas, [f.base]);
    git(f.bare, "update-ref", "refs/remotes/origin/main", f.base);
    await assert.rejects(produce(f, f.base, "root", 32 * 1024), RecoveryBundleTooLargeError);
    assert.equal(fs.existsSync(path.join(f.root, "root.bundle")), false);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(refname)", RECOVERY_BUNDLE_REF), "");
  } finally { cleanup(f); }
});

it("missing, unrelated, symbolic and noncommit cache cannot replace full-first capture or original oversize", async () => {
  const f = fixture();
  try {
    // Runner/checkpoint/local refs are tempting private boundaries but are inadmissible.
    for (const ref of ["refs/heads/main", "refs/uzi-runner/main", "refs/uzi-checkpoints/main"])
      git(f.bare, "update-ref", ref, f.head);
    const unrelated = commit(f, f.tree, [], "unrelated cache");
    const blob = git(f.bare, "rev-parse", f.head + ":private");
    const variants = [
      () => git(f.bare, "update-ref", "-d", "refs/remotes/origin/main"),
      () => git(f.bare, "update-ref", "refs/remotes/origin/main", unrelated),
      () => git(f.bare, "symbolic-ref", "refs/remotes/origin/main", "refs/heads/main"),
      () => { git(f.bare, "symbolic-ref", "--delete", "refs/remotes/origin/main");
        git(f.bare, "update-ref", "refs/remotes/origin/main", blob); },
    ];
    for (const [i, change] of variants.entries()) {
      change();
      await assert.rejects(produce(f, f.head, "oversize-" + i, 32 * 1024), RecoveryBundleTooLargeError);
      assert.equal(fs.existsSync(path.join(f.root, "oversize-" + i + ".bundle")), false);
      const full = await produce(f, f.head, "fits-" + i, 1024 * 1024);
      assert.equal(full.selfContained, true);
      assert.deepEqual(full.prerequisiteShas, []);
      assert.equal(f.calls.some(args => args.includes("merge-base")), false, "fitting full capture never consults cache");
    }
    await assert.rejects(produce(f, f.head, "invalid-branch", 32 * 1024, "../main"), RecoveryBundleTooLargeError);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(refname)", RECOVERY_BUNDLE_REF), "");
  } finally { cleanup(f); }
});

it("force-push before and after capture leaves stale dependencies declared and fresh-clone verification fails", async () => {
  const f = fixture();
  try {
    const before = await produce(f, f.head, "before", 32 * 1024);
    const publicTree = git(f.bare, "rev-parse", f.tip + "^{tree}");
    const forced = commit(f, publicTree, [], "new unrelated public history");
    git(f.bare, "push", "--force", f.forge, forced + ":refs/heads/main");
    // Deliberately do not fetch: the direct worker cache remains stale.
    const after = await produce(f, f.head, "after", 32 * 1024);
    assert.deepEqual(before.prerequisiteShas, [f.tip]);
    assert.deepEqual(after.prerequisiteShas, [f.tip]);
    const clean = fresh(f, "forced");
    assert.throws(() => git(clean, "cat-file", "-e", f.tip));
    for (const bundle of [before, after]) {
      assert.equal(bundle.alreadyPublished, false);
      assert.equal(bundle.selfContained, false);
      assert.throws(() => git(clean, "bundle", "verify", bundle.bundlePath));
    }
  } finally { cleanup(f); }
});

it("unpublished incompressible blobs still exceed injected cap after subtraction and clean partial file/ref", async () => {
  const f = fixture();
  try {
    fs.writeFileSync(path.join(f.work, "unpublished-large"), randomBytes(128 * 1024));
    git(f.work, "add", "unpublished-large"); git(f.work, "commit", "-m", "private large output");
    const head = git(f.work, "rev-parse", "HEAD");
    git(f.work, "push", f.bare, "HEAD:refs/heads/large");
    await assert.rejects(produce(f, head, "too-large", 32 * 1024), RecoveryBundleTooLargeError);
    assert.equal(fs.existsSync(path.join(f.root, "too-large.bundle")), false);
    assert.equal(git(f.bare, "for-each-ref", "--format=%(refname)", RECOVERY_BUNDLE_REF), "");
  } finally { cleanup(f); }
});

it("replacement refs cannot rewrite the aggregate graph or its cached dependencies", async () => {
  const f = fixture();
  try {
    const replacement = commit(f, f.tree, [], "unrelated replacement");
    git(f.bare, "replace", f.head, replacement);
    const thin = await produce(f, f.head, "replaced", 32 * 1024);
    assert.equal(thin.sourceSha, f.head);
    assert.deepEqual(thin.prerequisiteShas, [f.tip]);
    importAndCheck(f, fresh(f, "replacement-import"), f.head, [f.head], thin.bundlePath, thin.prerequisiteShas);
    git(f.bare, "replace", "-d", f.head);
    git(f.bare, "replace", f.tip, replacement);
    const cached = await produce(f, f.head, "cached-replaced", 32 * 1024);
    assert.deepEqual(cached.prerequisiteShas, [f.tip]);
  } finally { cleanup(f); }
});

it("producer options are exclusive and header parser bounds actual dependencies and source/ref", async () => {
  const f = fixture();
  try {
    await assert.rejects(f.cache.produceRecoveryBundle(f.bare, {
      sourceSha: f.head, outPath: path.join(f.root, "exclusive.bundle"),
      guardedDefaultBranch: "main", forgeTip: f.tip,
    }), /mutually exclusive/);
    assert.equal(fs.existsSync(path.join(f.root, "exclusive.bundle")), false);
    const out = path.join(f.root, "header.bundle");
    const prereqs = Array.from({ length: 65 }, (_, i) => (i + 1).toString(16).padStart(40, "0"));
    const header = (deps: string[], source = f.head, ref = RECOVERY_BUNDLE_REF) =>
      "# v2 git bundle\n" + deps.map(d => "-" + d + " boundary\n").join("") + source + " " + ref + "\n\nPACK";
    const read = (text: string | Buffer) => { fs.writeFileSync(out, text); return readRecoveryBundleHeader(out, f.head); };
    assert.deepEqual(await read(header([])), { prerequisiteShas: [], selfContained: true });
    assert.deepEqual(await read(header(prereqs.slice(0, 64).reverse())),
      { prerequisiteShas: prereqs.slice(0, 64), selfContained: false });
    await assert.rejects(read(header(prereqs)), /prerequisites/);
    await assert.rejects(read(header([f.tip, f.tip])), /prerequisites/);
    await assert.rejects(read(header([], f.tip)), /source\/ref mismatch/);
    await assert.rejects(read(header([], f.head, "refs/heads/wrong")), /source\/ref mismatch/);
    await assert.rejects(read(header(["invalid"])), /prerequisites/);
    await assert.rejects(read(header([]).replace("PACK", "NOPE")), /header/);
    await assert.rejects(read(Buffer.concat([
      Buffer.from(header([]).slice(0, -4)), Buffer.from([0xd0, 0xc1, 0xc3, 0xcb]),
    ])), /header/);
    // PACK must fit in the same bounded 16KiB read as the complete header.
    const prefix = "# v2 git bundle\n-" + f.tip + " ";
    const suffix = "\n" + f.head + " " + RECOVERY_BUNDLE_REF + "\n\nPACK";
    const exact = prefix + "x".repeat(16 * 1024 - prefix.length - suffix.length) + suffix;
    assert.equal(Buffer.byteLength(exact), 16 * 1024);
    assert.deepEqual(await read(exact), { prerequisiteShas: [f.tip], selfContained: false });
    await assert.rejects(read(prefix + "x" + exact.slice(prefix.length)), /excessive/);
  } finally { cleanup(f); }
});
