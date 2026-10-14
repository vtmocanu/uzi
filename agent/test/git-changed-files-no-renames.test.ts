import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache } from "../src/git.js";

const WF = ".github/workflows/ci.yml";
let fx: Fixture;
let git: GitCache;
let tmp: string;

beforeEach(() => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "cfnr-"));
  tick = 0;
});
afterEach(() => {
  fx.cleanup();
  fs.rmSync(tmp, { recursive: true, force: true });
});

// Every git call gets a distinct, strictly increasing author/committer date (one second apart
// from a fixed epoch), so any commit (including merges) the call creates has a deterministic
// timestamp. This keeps `merge-base --all` ordering and the criss-cross premise independent of
// wall-clock time and of how fast the fixture commits run.
const FIXTURE_EPOCH = 1_700_000_000;
let tick = 0;

function g(dir: string, args: string[]): string {
  const date = `${FIXTURE_EPOCH + ++tick} +0000`;
  return execFileSync("git", ["-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false", ...args], {
    encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_AUTHOR_DATE: date, GIT_COMMITTER_DATE: date },
  }).trim();
}

function put(dir: string, rel: string, content: string): void {
  fs.mkdirSync(path.dirname(path.join(dir, rel)), { recursive: true });
  fs.writeFileSync(path.join(dir, rel), content);
}

function commit(dir: string, rel: string, content: string, msg: string): string {
  put(dir, rel, content);
  g(dir, ["add", rel]);
  g(dir, ["commit", "-m", msg]);
  return g(dir, ["rev-parse", "HEAD"]);
}

function newRepo(): string {
  const dir = path.join(tmp, "work");
  fs.mkdirSync(dir);
  g(dir, ["init", "-q", "-b", "main"]);
  return dir;
}

function toBare(work: string): string {
  const bare = path.join(tmp, "bare.git");
  execFileSync("git", ["clone", "-q", "--bare", work, bare]);
  return bare;
}

describe("GitCache.changedFilesNoRenames", () => {
  it("criss-cross history (two merge bases): lists a workflow change the first base hides", async () => {
    const w = newRepo();
    commit(w, "code", "0\n", "A");
    commit(w, WF, "v1\n", "OLDWF");
    commit(w, "code", "1\n", "B");
    g(w, ["checkout", "-q", "-b", "side", "main~2"]);
    commit(w, WF, "evil\n", "Y");
    commit(w, WF, "v1\n", "Y2");
    g(w, ["checkout", "-q", "main"]);
    g(w, ["merge", "--no-ff", "-q", "-m", "D", "side"]);
    g(w, ["checkout", "-q", "-b", "agent", "main~1"]);
    commit(w, "code", "2\n", "P1");
    const y = g(w, ["rev-parse", "side~1"]);
    try { g(w, ["merge", "-q", "--no-commit", "--no-ff", y]); } catch { /* add/add conflict on the workflow; resolved below */ }
    put(w, WF, "evil\n");
    g(w, ["add", WF]);
    g(w, ["commit", "-q", "-m", "M2"]);
    const m2 = g(w, ["rev-parse", "HEAD"]);
    g(w, ["checkout", "-q", "main"]);
    const bare = toBare(w);

    const bases = g(bare, ["merge-base", "--all", "main", m2]).split("\n");
    assert.equal(bases.length, 2, "fixture must be criss-cross");
    const threeDot = g(bare, ["diff", "--name-only", "-z", "--no-renames", `main...${m2}`]).split("\0").filter(Boolean);
    assert.ok(!threeDot.includes(WF), "premise: three-dot diff hides the workflow change");

    const changed = await git.changedFilesNoRenames(bare, m2);
    assert.ok(changed !== null);
    assert.ok(changed.includes(WF));
    assert.ok(changed.includes("code"));
    assert.equal(new Set(changed).size, changed.length, "de-duplicated");
  });

  it("linear history: the plain merge-base diff", async () => {
    const w = newRepo();
    commit(w, "code", "0\n", "A");
    g(w, ["checkout", "-q", "-b", "agent"]);
    commit(w, "src/a.ts", "a\n", "one");
    const tip = commit(w, WF, "x\n", "two");
    g(w, ["checkout", "-q", "main"]);
    const bare = toBare(w);
    assert.deepStrictEqual(await git.changedFilesNoRenames(bare, tip), ["src/a.ts", WF].sort());
  });

  it("a workflow copied from the moved default (single-parent overlay) drops out; run changes stay", async () => {
    const w = newRepo();
    commit(w, "code", "0\n", "A");
    g(w, ["checkout", "-q", "-b", "agent"]);
    commit(w, "src/a.ts", "a\n", "run");
    g(w, ["checkout", "-q", "main"]);
    commit(w, WF, "v1\n", "default adds a workflow");
    g(w, ["checkout", "-q", "agent"]);
    g(w, ["checkout", "main", "--", ".github/workflows"]);
    g(w, ["commit", "-q", "-m", "overlay"]);
    const overlay = g(w, ["rev-parse", "HEAD"]);
    const edited = commit(w, WF, "evil\n", "run edits the workflow");
    g(w, ["checkout", "-q", "main"]);
    const bare = toBare(w);
    const threeDot = g(bare, ["diff", "--name-only", "-z", "--no-renames", `main...${overlay}`]).split("\0").filter(Boolean);
    assert.ok(threeDot.includes(WF), "premise: the merge-base diff lists the overlaid workflow");

    assert.deepStrictEqual(await git.changedFilesNoRenames(bare, overlay), ["src/a.ts"]);
    assert.deepStrictEqual((await git.changedFilesNoRenames(bare, edited))?.sort(), [WF, "src/a.ts"].sort());
  });

  it("a deletion and both sides of a rename-out still differ from the default tip", async () => {
    const w = newRepo();
    commit(w, WF, "v1\n", "A");
    commit(w, ".github/workflows/b.yml", "b\n", "B");
    g(w, ["checkout", "-q", "-b", "agent"]);
    g(w, ["rm", "-q", WF]);
    g(w, ["mv", ".github/workflows/b.yml", "moved.yml"]);
    g(w, ["commit", "-q", "-m", "delete and rename out"]);
    const tip = g(w, ["rev-parse", "HEAD"]);
    g(w, ["checkout", "-q", "main"]);
    const bare = toBare(w);
    assert.deepStrictEqual((await git.changedFilesNoRenames(bare, tip))?.sort(), [".github/workflows/b.yml", WF, "moved.yml"].sort());
  });

  it("unrelated histories: null (fail closed)", async () => {
    const w = newRepo();
    commit(w, "code", "0\n", "A");
    g(w, ["checkout", "-q", "--orphan", "other"]);
    g(w, ["rm", "-rfq", "."]);
    const tip = commit(w, WF, "x\n", "root2");
    g(w, ["checkout", "-q", "main"]);
    const bare = toBare(w);
    assert.equal(await git.changedFilesNoRenames(bare, tip), null);
  });
});
