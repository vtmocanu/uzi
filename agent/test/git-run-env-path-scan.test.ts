import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache, type GitCacheOptions } from "../src/git.js";

// PRD 4416 M3: a secret containing "/" can be published as a git PATH. A tree object stores one path
// component, so no single object holds the value; GitCache.scanObjectsForRunEnv therefore also searches the
// full path of every entry each outgoing commit adds. Real git, a real bare cache; the values are synthetic
// and assembled at runtime. Every commit message is harmless on purpose.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const NESTED = ["rv-Ab3k", "Mq7w-Zx2n", "Fe9t"].join("/");
const FLAT = ["rv-Ab3k-Mq7w", "Zx2n-Fe9t"].join("/");
const SINGLE = ["rv-Ab3k", "Mq7w", "Zx2n", "Fe9t"].join("-");
const PATH_WITHHELD = "[path withheld]";

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).replace(/\n$/, "");
}
function commitPaths(repo: string, files: Record<string, string>, message = "update"): string {
  for (const [name, body] of Object.entries(files)) {
    fs.mkdirSync(path.dirname(path.join(repo, name)), { recursive: true });
    fs.writeFileSync(path.join(repo, name), body);
  }
  git(repo, "add", "-A");
  git(repo, "-c", "commit.gpgsign=false", "commit", "-m", message);
  return git(repo, "rev-parse", "HEAD");
}

let fx: Fixture;
let cache: GitCache;
let bare: string;
let work: string;
let base: string;

const syncFeature = (ref = "feature"): void => {
  git(bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*");
  git(bare, "update-ref", "refs/uzi-test/feature", git(work, "rev-parse", ref));
};
const needles = (...values: string[]) => [{ name: "DB_PASSWORD", forms: values }];
type Caps = { maxObjects?: number; maxTotalBytes?: number; maxPaths?: number; maxPathBytes?: number };
const scan = (tip: string, values: string[], o: { caps?: Caps; deadlineMs?: number } = {}) =>
  cache.scanObjectsForRunEnv(bare, { tip, excludes: [base] }, needles(...values),
    { ...(o.caps ? { caps: o.caps } : {}), ...(o.deadlineMs === undefined ? {} : { deadlineMs: o.deadlineMs }) });

function assertWithheld(res: Awaited<ReturnType<typeof scan>>, secret: string, commit: string): void {
  assert.ok(res.trusted, "the scan is trusted");
  if (!res.trusted) return;
  assert.equal(res.findings.length, 1, "one finding");
  const f = res.findings[0]!;
  assert.equal(f.ruleId, "uzi-run-env");
  assert.equal(f.variable, "DB_PASSWORD");
  assert.equal(f.file, PATH_WITHHELD);
  assert.equal(f.startLine, 0);
  assert.equal(f.commit, commit, "the commit whose diff listed the path");
  assert.ok(!JSON.stringify(res).includes(secret.replace(/[\n\t"\\]/g, "")), "no value bytes in the result");
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
  // Hostile repository configuration the scan must not be fooled by.
  git(bare, "config", "core.quotePath", "true");
  git(bare, "config", "diff.ignoreSubmodules", "all");
  git(bare, "config", "diff.renames", "copies");
});
afterEach(() => fx.cleanup());

describe("scanObjectsForRunEnv: a secret published as a path", () => {
  it("finds a nested path whose components join to the value, and names the commit", async () => {
    commitPaths(work, { "clean.txt": "ordinary\n" });
    const introducing = commitPaths(work, { [NESTED]: "harmless\n" });
    const tip = commitPaths(work, { "later.txt": "later\n" });
    syncFeature();
    assertWithheld(await scan(tip, [NESTED]), NESTED, introducing);
  });

  it("finds a value that is one path component", async () => {
    const introducing = commitPaths(work, { [SINGLE]: "harmless\n" });
    syncFeature();
    // The object pass sees the one-component name inside the tree object too, so there may be two findings.
    const res = await scan(introducing, [SINGLE]);
    assert.ok(res.trusted && res.findings.length >= 1 && res.findings.every((f) => f.variable === "DB_PASSWORD"));
    assert.ok(res.trusted && res.findings.some((f) => f.file === PATH_WITHHELD && f.commit === introducing), "the path finding");
  });

  it("finds a value that is the directory part of a deeper path", async () => {
    const introducing = commitPaths(work, { [`${FLAT}/inner/leaf.txt`]: "harmless\n" });
    syncFeature();
    assertWithheld(await scan(introducing, [FLAT]), FLAT, introducing);
  });

  it("finds a rename of an already-public file to such a path", async () => {
    git(work, "mv", "README.md", FLAT.replace("/", "-"));
    fs.mkdirSync(path.dirname(path.join(work, FLAT)), { recursive: true });
    git(work, "mv", FLAT.replace("/", "-"), FLAT);
    git(work, "-c", "commit.gpgsign=false", "commit", "-m", "move");
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    assertWithheld(await scan(tip, [FLAT]), FLAT, tip);
  });

  it("finds a path an evil merge introduces (absent from every parent)", async () => {
    git(work, "checkout", "-q", "-b", "side", base);
    commitPaths(work, { "side.txt": "side\n" });
    git(work, "checkout", "-q", "feature");
    commitPaths(work, { "main-side.txt": "main\n" });
    git(work, "merge", "--no-commit", "--no-ff", "side");
    fs.mkdirSync(path.dirname(path.join(work, NESTED)), { recursive: true });
    fs.writeFileSync(path.join(work, NESTED), "harmless\n");
    git(work, "add", "-A");
    git(work, "-c", "commit.gpgsign=false", "commit", "-m", "merge");
    const merge = git(work, "rev-parse", "HEAD");
    syncFeature();
    assertWithheld(await scan(merge, [NESTED]), NESTED, merge);
  });

  it("finds an empty-tree entry at such a path", async () => {
    const empty = git(work, "hash-object", "-t", "tree", "/dev/null");
    const [a, mid, leaf] = NESTED.split("/") as [string, string, string];
    const inner = execFileSync("git", ["-C", work, "mktree"], { env: GIT_ENV, encoding: "utf8", input: `040000 tree ${empty}\t${leaf}\n` }).trim();
    const middle = execFileSync("git", ["-C", work, "mktree"], { env: GIT_ENV, encoding: "utf8", input: `040000 tree ${inner}\t${mid}\n` }).trim();
    const existing = git(work, "ls-tree", "HEAD");
    const root = execFileSync("git", ["-C", work, "mktree"], { env: GIT_ENV, encoding: "utf8", input: `${existing}\n040000 tree ${middle}\t${a}\n` }).trim();
    const commit = execFileSync("git", ["-C", work, "-c", "commit.gpgsign=false", "commit-tree", root, "-p", "HEAD", "-m", "update"],
      { env: GIT_ENV, encoding: "utf8" }).trim();
    git(work, "update-ref", "refs/heads/feature", commit);
    syncFeature();
    assertWithheld(await scan(commit, [NESTED]), NESTED, commit);
  });

  it("finds a gitlink at such a path even with diff.ignoreSubmodules=all", async () => {
    git(work, "update-index", "--add", "--cacheinfo", `160000,${base},${FLAT}`);
    git(work, "-c", "commit.gpgsign=false", "commit", "-m", "update");
    const tip = git(work, "rev-parse", "HEAD");
    syncFeature();
    assertWithheld(await scan(tip, [FLAT]), FLAT, tip);
  });

  it("a form with a leading or trailing slash meets a root directory", async () => {
    const introducing = commitPaths(work, { "secretdir/file.txt": "harmless\n" });
    syncFeature();
    assertWithheld(await scan(introducing, ["/secretdir/"]), "/secretdir/", introducing);
  });

  it("an object finding for a blob under a directory named by a leading-slash form withholds its path", async () => {
    const dir = ["rv-Ab3k", "Mq7w", "Zx2n"].join("-");
    const form = `/${dir}`;
    const introducing = commitPaths(work, { [`${dir}/README`]: `holds ${form} in its content\n` });
    syncFeature();
    const res = await scan(introducing, [form]);
    assert.ok(res.trusted, "the scan is trusted");
    if (!res.trusted) return;
    assert.ok(res.findings.length >= 1, "at least one finding");
    for (const f of res.findings) assert.equal(f.file, PATH_WITHHELD, "every finding withholds the path");
    assert.ok(!JSON.stringify(res).includes(dir), "no path bytes in the result");
  });

  const hostile: Array<[string, string]> = [
    ["a non-ASCII letter", "caf\u00e9-Kq7/Zp2m"],
    ["an emoji", "\u{1F511}-Kq7/Zp2m"],
    ["a TAB", "tab\tKq7/Zp2m"],
    ["a newline", "nl\nKq7/Zp2m"],
    ["a space", "sp ace-Kq7/Zp2m"],
    ["a double quote", 'q"uote-Kq7/Zp2m'],
    ["a backslash", "back\\slash-Kq7/Zp2m"],
  ];
  for (const [label, secret] of hostile) {
    it(`detects a path secret with ${label} (core.quotePath=true, diff.ignoreSubmodules=all)`, async () => {
      const introducing = commitPaths(work, { [secret]: "harmless\n" });
      syncFeature();
      assertWithheld(await scan(introducing, [secret]), secret, introducing);
    });
  }

  it("a clean range with other paths stays clean and trusted", async () => {
    const tip = commitPaths(work, { "src/app/main.txt": "ordinary\n", "docs/readme.txt": "text\n" });
    syncFeature();
    const res = await scan(tip, [NESTED, SINGLE]);
    assert.deepEqual(res, { trusted: true, findings: [] });
  });

});

describe("scanObjectsForRunEnv: the path pass fails closed", () => {
  const prepare = (): string => {
    const tip = commitPaths(work, { "a/b/c.txt": "1\n", "d/e.txt": "2\n", "f.txt": "3\n" });
    syncFeature();
    return tip;
  };
  const incomplete = { trusted: false, reason: "path_scan_incomplete" };

  it("reaching maxPaths is path_scan_incomplete", async () => {
    assert.deepEqual(await scan(prepare(), [NESTED], { caps: { maxPaths: 2 } }), incomplete);
  });
  it("reaching maxPathBytes is path_scan_incomplete", async () => {
    assert.deepEqual(await scan(prepare(), [NESTED], { caps: { maxPathBytes: 8 } }), incomplete);
  });
  type Stage = Parameters<NonNullable<NonNullable<GitCacheOptions["objectScanClockForTest"]>["atStage"]>>[0];
  // A scan whose clock jumps a billion milliseconds when it reaches `jumpAt` (never, when undefined).
  const scanWithClock = async (tip: string, jumpAt: Stage | undefined) => {
    let t = 0;
    const seen: Stage[] = [];
    const clocked = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
      objectScanClockForTest: { now: () => t, atStage: (s) => { seen.push(s); if (s === jumpAt) t += 10 ** 9; } },
    }));
    const res = await clocked.scanObjectsForRunEnv(bare, { tip, excludes: [base] }, needles(NESTED));
    return { res, seen: [...new Set(seen)] }; // path_stream fires once per chunk read: the order of first arrival

  };

  it("a deadline spent inside the path pass is path_scan_incomplete", async () => {
    const { res, seen } = await scanWithClock(prepare(), "path_pass");
    assert.deepEqual(res, incomplete);
    assert.deepEqual(seen, ["before_path_pass", "path_pass"]);
  });
  it("a deadline spent while the path pass reads git's output is path_scan_incomplete", async () => {
    const { res, seen } = await scanWithClock(prepare(), "path_stream");
    assert.deepEqual(res, incomplete);
    assert.deepEqual(seen, ["before_path_pass", "path_pass", "path_stream"]);
  });
  it("a deadline spent before the path pass is deadline", async () => {
    const { res, seen } = await scanWithClock(prepare(), "before_path_pass");
    assert.deepEqual(res, { trusted: false, reason: "deadline" });
    assert.deepEqual(seen, ["before_path_pass"]);
  });
  it("a deadline spent before the object pass is deadline", async () => {
    const { res, seen } = await scanWithClock(prepare(), "object_pass");
    assert.deepEqual(res, { trusted: false, reason: "deadline" });
    assert.deepEqual(seen, ["before_path_pass", "path_pass", "path_stream", "object_pass"]);
  });
  it("a deadline spent inside the path pass of an empty range is path_scan_incomplete", async () => {
    const { res } = await scanWithClock(base, "path_pass");
    assert.deepEqual(res, incomplete);
  });
  it("without a clock jump every stage is reached and the scan is clean", async () => {
    const { res, seen } = await scanWithClock(prepare(), undefined);
    assert.deepEqual(res, { trusted: true, findings: [] });
    assert.deepEqual(seen, ["before_path_pass", "path_pass", "path_stream", "object_pass"]);
  });
  it("the same range under the default caps is clean", async () => {
    assert.deepEqual(await scan(prepare(), [NESTED]), { trusted: true, findings: [] });
  });
});
