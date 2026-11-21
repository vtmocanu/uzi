import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { providerKeyForms } from "../src/redact.js";

// Owner decision, issue #4505: the exact-key push scan. A prefixless provider key has no gitleaks
// rule, so GitCache.scanObjectsForLiterals reads every object a publication would carry and searches
// it for every encoded form of the key. Real git throughout. The key is a synthetic, prefixless value
// assembled at runtime; it exists only in these throwaway fixture repos.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const KEY = ["Zq9x", "Lm/4", "tR+7", "&kQ2", "<w8>", " vN"].join("");
const FORMS = providerKeyForms(KEY);
const RAW = KEY;

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
}
function commitFile(repo: string, name: string, content: string | Buffer): string {
  fs.mkdirSync(path.dirname(path.join(repo, name)), { recursive: true });
  fs.writeFileSync(path.join(repo, name), content);
  git(repo, "add", "-A");
  git(repo, "-c", "commit.gpgsign=false", "commit", "-m", `edit ${path.basename(name).slice(0, 8)}`);
  return git(repo, "rev-parse", "HEAD");
}

let fx: Fixture;
let cache: GitCache;
let bare: string;
let work: string;
let base: string;

/** Bring origin's branches into the bare (remote-tracking refs) and the feature tip under a plain ref. */
function syncFeature(): void {
  git(bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*");
  git(bare, "update-ref", "refs/uzi-test/feature", git(work, "rev-parse", "feature"));
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  work = fx.originPath;
  git(work, "config", "user.email", "t@t");
  git(work, "config", "user.name", "t");
  base = git(work, "rev-parse", "main");
  git(work, "checkout", "-q", "-b", "feature");
});
afterEach(() => fx.cleanup());

interface ScanOpts {
  literals?: readonly string[];
  excludeRemoteTracking?: boolean;
  deadlineMs?: number;
  caps?: { maxObjects?: number; maxTotalBytes?: number };
}
const scan = (tip: string, excludes: string[], o: ScanOpts = {}) =>
  cache.scanObjectsForLiterals(
    bare, { tip, excludes, ...(o.excludeRemoteTracking ? { excludeRemoteTracking: true } : {}) }, o.literals ?? FORMS,
    { ...(o.deadlineMs === undefined ? {} : { deadlineMs: o.deadlineMs }), ...(o.caps ? { caps: o.caps } : {}) },
  );

function assertNoKeyBytes(value: unknown): void {
  const text = JSON.stringify(value);
  for (const form of FORMS) assert.ok(!text.includes(form), "no encoded form of the key appears in the result");
}

describe("scanObjectsForLiterals", () => {
  for (const form of FORMS) {
    it(`finds the form (length ${form.length}) in a blob and attributes the introducing commit`, async () => {
      commitFile(work, "clean.txt", "ordinary\n");
      const introducing = commitFile(work, "src/config.txt", `value=${form}\n`);
      const after = commitFile(work, "later.txt", "later\n");
      syncFeature();
      const res = await scan(after, [base]);
      assert.ok(res.trusted && res.findings.length === 1, "exactly the one blob");
      if (!res.trusted) return;
      assert.equal(res.findings[0]!.ruleId, "uzi-provider-key");
      assert.equal(res.findings[0]!.commit, introducing);
      assert.equal(res.findings[0]!.file, "src/config.txt");
      assertNoKeyBytes(res);
    });
  }

  it("finds the key in an EMPTY commit's message (no hunk for gitleaks to count)", async () => {
    git(work, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", `note ${RAW} tail`);
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    const res = await scan(tip, [base]);
    assert.ok(res.trusted && res.findings.length === 1);
    if (res.trusted) assert.equal(res.findings[0]!.commit, tip);
    assertNoKeyBytes(res);
  });

  it("finds the key in a file NAME (percent-encoded) through the tree that lists it", async () => {
    const tip = commitFile(work, `dir-${encodeURIComponent(KEY)}.txt`, "innocent content\n");
    syncFeature();
    const res = await scan(tip, [base]);
    assert.ok(res.trusted && res.findings.length >= 1, "the tree that names the file is hit");
    assertNoKeyBytes(res);
  });

  it("withholds a BLOB path that itself contains a key form", async () => {
    const tip = commitFile(work, `${encodeURIComponent(KEY)}.txt`, `contents ${RAW}\n`);
    syncFeature();
    const res = await scan(tip, [base]);
    assert.ok(res.trusted && res.findings.length >= 1);
    if (res.trusted) assert.ok(res.findings.some((f) => f.file === "[path withheld]"));
    assertNoKeyBytes(res);
  });

  it("finds the key in a binary file with NUL bytes", async () => {
    const tip = commitFile(work, "blob.bin", Buffer.concat([Buffer.from([0, 0, 1, 0]), Buffer.from(RAW), Buffer.from([0, 7])]));
    syncFeature();
    const res = await scan(tip, [base]);
    assert.ok(res.trusted && res.findings.length === 1);
  });

  it("finds the key in an evil merge's own resolution", async () => {
    commitFile(work, "f.txt", "base\n");
    const fork = git(work, "rev-parse", "HEAD");
    git(work, "checkout", "-q", "-b", "side", fork);
    commitFile(work, "f.txt", "side edit\n");
    git(work, "checkout", "-q", "feature");
    commitFile(work, "f.txt", "feature edit\n");
    try { git(work, "merge", "--no-commit", "side"); } catch { /* the conflict is expected */ }
    fs.writeFileSync(path.join(work, "f.txt"), `resolved ${RAW}\n`);
    git(work, "add", "f.txt");
    git(work, "-c", "commit.gpgsign=false", "commit", "-m", "merge");
    const merge = git(work, "rev-parse", "HEAD");
    syncFeature();
    const res = await scan(merge, [base]);
    assert.ok(res.trusted && res.findings.length === 1);
    if (res.trusted) assert.equal(res.findings[0]!.commit, merge, "attributed to the merge that introduced the blob");
  });

  it("is clean when the key lives only below an excluded floor", async () => {
    const floor = commitFile(work, "old.txt", `old ${RAW}\n`);
    const tip = commitFile(work, "new.txt", "new and clean\n");
    syncFeature();
    assert.deepEqual(await scan(tip, [floor]), { trusted: true, findings: [] });
    const unfloored = await scan(tip, [base]);
    assert.ok(unfloored.trusted && unfloored.findings.length === 1, "without the floor the same key is found");
  });

  it("excludeRemoteTracking skips what origin's remote-tracking refs already hold", async () => {
    const tip = commitFile(work, "x.txt", `x ${RAW}\n`);
    syncFeature(); // refs/remotes/origin/feature now holds the tip
    assert.deepEqual(await scan(tip, [base], { excludeRemoteTracking: true }), { trusted: true, findings: [] });
    const res = await scan(tip, [base]);
    assert.ok(res.trusted && res.findings.length === 1);
  });

  it("is untrusted over the object cap and over the byte cap", async () => {
    commitFile(work, "a.txt", "a\n");
    const tip = commitFile(work, "b.txt", "b\n");
    syncFeature();
    assert.equal((await scan(tip, [base], { caps: { maxObjects: 1 } })).trusted, false);
    assert.equal((await scan(tip, [base], { caps: { maxTotalBytes: 1 } })).trusted, false);
  });

  it("is untrusted for an unresolvable revision and a malformed range", async () => {
    const tip = commitFile(work, "a.txt", "a\n");
    syncFeature();
    assert.equal((await scan(tip, ["f".repeat(40)])).trusted, false);
    assert.equal((await scan("not-a-sha", [base])).trusted, false);
    assert.equal((await scan(tip, ["HEAD"])).trusted, false);
  });

  it("with no literals it is clean and spawns no git child at all", async (t) => {
    const tip = commitFile(work, "a.txt", `${RAW}\n`);
    syncFeature();
    const exec = t.mock.method(cache as unknown as { execScoped: () => unknown }, "execScoped");
    const spawn = t.mock.method(cache as unknown as { spawnGit: () => unknown }, "spawnGit");
    assert.deepEqual(await scan(tip, [base], { literals: [] }), { trusted: true, findings: [] });
    assert.deepEqual(await scan(tip, [base], { literals: [""] }), { trusted: true, findings: [] });
    assert.equal(exec.mock.callCount() + spawn.mock.callCount(), 0);
  });

  it("a range with no object carrying the key is clean", async () => {
    const tip = commitFile(work, "a.txt", "nothing to see\n");
    syncFeature();
    assert.deepEqual(await scan(tip, [base]), { trusted: true, findings: [] });
  });

  it("an object boundary never joins two objects into a false hit", async () => {
    const half = Math.floor(RAW.length / 2);
    commitFile(work, "a.txt", RAW.slice(0, half));
    const tip = commitFile(work, "b.txt", RAW.slice(half));
    syncFeature();
    assert.deepEqual(await scan(tip, [base]), { trusted: true, findings: [] });
  });
});

describe("the scanned objects are a superset of what a pack for the same range carries", () => {
  /** The object ids a `pack-objects --revs` over `input` carries. */
  function packedObjects(input: string): Set<string> {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-pk-"));
    try {
      const pack = execFileSync("git", ["-C", bare, "pack-objects", "--revs", "--stdout"], { env: GIT_ENV, input, maxBuffer: 1 << 28 });
      execFileSync("git", ["-C", dir, "init", "-q", "--bare"], { env: GIT_ENV });
      const packFile = path.join(dir, "x.pack");
      fs.writeFileSync(packFile, pack);
      execFileSync("git", ["-C", dir, "index-pack", packFile], { env: GIT_ENV });
      const idx = execFileSync("git", ["-C", dir, "show-index"], {
        env: GIT_ENV, input: fs.readFileSync(path.join(dir, "x.idx")), encoding: "utf8",
      });
      return new Set(idx.split("\n").filter(Boolean).map((l) => l.split(" ")[1]!));
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  }

  it("rev-list --objects tip --not floors covers every object checkpointPack / pushBranch would send", () => {
    commitFile(work, "a.txt", "a\n");
    const floor = commitFile(work, "dir/b.txt", "b\n");
    git(work, "checkout", "-q", "-b", "side", floor);
    commitFile(work, "side.txt", "side\n");
    git(work, "checkout", "-q", "feature");
    commitFile(work, "dir/c.txt", "c\n");
    git(work, "-c", "commit.gpgsign=false", "merge", "--no-ff", "-m", "merge", "side");
    git(work, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "empty");
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    const objectsOf = (out: string) => new Set(out.split("\n").filter(Boolean).map((l) => l.split(" ")[0]!));
    const listed = objectsOf(git(bare, "rev-list", "--objects", tip, "--not", floor, base));
    const packed = packedObjects(`${tip}\n^${floor}\n^${base}\n`);
    assert.ok(packed.size > 0);
    for (const oid of packed) assert.ok(listed.has(oid), `packed object ${oid} is scanned`);
    // The remote-tracking form a branch push excludes: the remote holds exactly origin/main here (the
    // feature and side branches were only mirrored into the bare by syncFeature, so drop them).
    git(bare, "update-ref", "-d", "refs/remotes/origin/feature");
    git(bare, "update-ref", "-d", "refs/remotes/origin/side");
    const listedPush = objectsOf(git(bare, "rev-list", "--objects", tip, "--not", floor, "--remotes=origin", "--"));
    const packedPush = packedObjects(`${tip}\n^${floor}\n^refs/remotes/origin/main\n`);
    assert.ok(packedPush.size > 0);
    for (const oid of packedPush) assert.ok(listedPush.has(oid), `pushed object ${oid} is scanned`);
  });
});

describe("secretScanCheckpointRange with provider key literals", () => {
  it("returns a uzi-provider-key finding for an EMPTY commit that carries the key (red on base: the zero-hunk early return)", async () => {
    git(work, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", `note ${RAW}`);
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    const res = await cache.secretScanCheckpointRange(bare, { tipSha: tip, excludeSha: base }, { literals: FORMS });
    assert.equal(res.trusted, true);
    assert.equal(res.findings.length, 1);
    assert.equal(res.findings[0]!.ruleId, "uzi-provider-key");
    assert.equal(res.findings[0]!.commit, tip);
    assertNoKeyBytes(res);
  });

  it("a zero range (tip === excludeSha) scans the objects the pack carries: the tip against its pinned parents", async () => {
    const parent = commitFile(work, "p.txt", "p\n");
    const tip = commitFile(work, "a.txt", `leaked: ${RAW}\n`);
    syncFeature();
    const res = await cache.secretScanCheckpointRange(
      bare, { tipSha: tip, excludeSha: tip, packExcludeShas: [parent] }, { literals: FORMS });
    assert.equal(res.trusted, true);
    assert.equal(res.findings.length, 1, "the declared commit's own blob is scanned, not the empty tip..^tip");
    assert.equal(res.findings[0]!.ruleId, "uzi-provider-key");
    assertNoKeyBytes(res);
  });

  it("the same range without literals is clean (a vendor run: no extra work)", async (t) => {
    git(work, "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", `note ${RAW}`);
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    const probe = t.mock.method(cache, "scanObjectsForLiterals");
    assert.deepEqual(await cache.secretScanCheckpointRange(bare, { tipSha: tip, excludeSha: base }, { literals: [] }),
      { trusted: true, findings: [] });
    assert.equal(probe.mock.callCount(), 0);
  });

  it("an untrusted key scan makes the whole scan untrusted with its own reason", async (t) => {
    const tip = commitFile(work, "a.txt", "a\n");
    syncFeature();
    t.mock.method(cache, "scanObjectsForLiterals", async () => ({ trusted: false as const, reason: "read_failed" }));
    const res = await cache.secretScanCheckpointRange(bare, { tipSha: tip, excludeSha: base }, { literals: FORMS });
    assert.deepEqual(res, { trusted: false, findings: [], reason: "provider_key_scan_untrusted" });
  });
});
