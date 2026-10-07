import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, type FetchAgentBranchOptions } from "../src/git.js";

// issue #1924: discovery enumerated the whole bare once PER CONTEXT while holding the repository
// lock (each pass rereading every context and candidate file and re-checking every pin), which
// made a long-lived worker's pass quadratic. (Pruning released contexts is deferred to a follow-up.)

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-1924";
const RUN = "19241924-1111-2222-3333-444444444444";
let fx: Fixture;
let cache: GitCache;
let bare: string;
let clone: string;
let base: string;

type Internals = {
  readOwedFile(bare: string, name: string): Promise<unknown | undefined>;
  execScoped(command: string, args: string[], options: unknown): Promise<{ stdout: string; stderr: string }>;
  writeOwedFile(bare: string, name: string, value: unknown): Promise<void>;
};

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

function contextFor(generation: number): FetchAgentBranchOptions {
  return { context: { runId: RUN, generation, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
}

/** Chain of N claim generations of one run, each committing on top of the previous head, so every
 *  generation's displaced head is pinned under its own context and the last head covers them all. */
async function buildGenerations(n: number): Promise<string[]> {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const heads: string[] = [];
  let parent = base;
  for (let g = 1; g <= n; g++) {
    const sha = gitIn(clone, [...IDENT, "commit-tree", tree, "-p", parent, "-m", `gen ${g}`]);
    gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
    const result = await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, contextFor(g));
    assert.equal(result.kind, "updated", JSON.stringify(result));
    heads.push(sha);
    parent = sha;
  }
  return heads;
}

/** Count metadata file reads and git subprocess calls made while `action` runs. */
async function measure<T>(action: () => Promise<T>): Promise<{ result: T; reads: number; catFile: number; forEachRef: number }> {
  const internals = cache as unknown as Internals;
  const readOriginal = internals.readOwedFile.bind(cache);
  const execOriginal = internals.execScoped.bind(cache);
  const counts = { reads: 0, catFile: 0, forEachRef: 0 };
  internals.readOwedFile = async (b, name) => { counts.reads++; return readOriginal(b, name); };
  internals.execScoped = async (command, args, options) => {
    if (command === "git" && args.includes("cat-file")) counts.catFile++;
    if (command === "git" && args.includes("for-each-ref")) counts.forEachRef++;
    return execOriginal(command, args, options);
  };
  try {
    const result = await action();
    return { result, ...counts };
  } finally {
    internals.readOwedFile = readOriginal;
    internals.execScoped = execOriginal;
  }
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-1924", noProofReseed, RUN)).path;
});
afterEach(() => fx.cleanup());

describe("issue1924 owed discovery cost", () => {
  it("reads each metadata file and checks each pin once per pass, not once per context", async () => {
    const N = 8;
    await buildGenerations(N);
    const m = await measure(() => cache.discoverOwedCandidates());
    assert.equal(m.result.length, N, "one discovery entry per context");
    // N context files + N candidate files, each read once: a per-context enumeration reads
    // every one of them again for every context.
    assert.ok(m.reads <= 2 * N + 2, `metadata reads ${m.reads} must be linear in ${N} contexts`);
    assert.ok(m.catFile <= N, `pin commit checks ${m.catFile} must be one per pin`);
    assert.ok(m.forEachRef <= 3, `ref listings ${m.forEachRef} must not repeat per context`);
  });
});
