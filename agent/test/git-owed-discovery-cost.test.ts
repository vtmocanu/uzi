import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, type FetchAgentBranchOptions } from "../src/git.js";

// issue #1924: discovery enumerated the whole bare once PER CONTEXT while holding the repository
// lock (each pass rereading every context and candidate file and re-checking every pin), and a
// context file outlived the release of its pins. Both made a long-lived worker's pass quadratic.

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

  it("a context whose pins were released is no longer enumerated, and its file is gone", async () => {
    const N = 6;
    const heads = await buildGenerations(N);
    const files = () => fs.readdirSync(path.join(bare, "uzi-owed"));
    assert.equal(files().filter((f) => f.startsWith("context-")).length, N);
    const released = await cache.reconcileOwedCandidates(bare, RUN, heads[N - 1]!, contextFor(N).context);
    assert.equal(released.removedShas.length, N, "every generation's head was pinned");
    assert.equal(files().filter((f) => f.startsWith("candidate-")).length, 0);
    // Only the live generation's context remains: its tracking receipt and the caller still name it.
    assert.equal(files().filter((f) => f.startsWith("context-")).length, 1);
    const discovered = await cache.discoverOwedCandidates();
    assert.deepEqual(discovered.map((d) => d.context.generation), [N]);
    assert.deepEqual(discovered.map((d) => d.candidates.length), [0]);
    assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN, heads[N - 1], N)).kind, "owned");
  });

  it("keeps a released generation discoverable while its recovery journal is unsettled", async () => {
    const N = 4;
    const heads = await buildGenerations(N);
    // Generation 2 still owes a final disposition to the server: its context must survive the
    // release of its pins, or discovery drops it and its open hold can never be settled.
    await cache.reconcileOwedCandidates(bare, RUN, heads[N - 1]!, contextFor(N).context, new Set([2]));
    const discovered = await cache.discoverOwedCandidates();
    assert.deepEqual(discovered.map((d) => d.context.generation), [2, N]);
    assert.deepEqual(discovered.map((d) => d.candidates.length), [0, 0]);
    // FINAL is now acknowledged, so generation 2 no longer needs discovery.
    await cache.reconcileOwedCandidates(bare, RUN, heads[N - 1]!, contextFor(N).context, new Set());
    assert.deepEqual((await cache.discoverOwedCandidates()).map((d) => d.context.generation), [N],
      "a protected zero-pin context must be pruned once its generation is acknowledged");
  });

  it("keeps a still-pinned context and prunes nothing while a receipt is unreadable (fail closed)", async () => {
    const N = 4;
    const heads = await buildGenerations(N);
    // A retained head the confirmed tip does not cover keeps its candidate metadata and context.
    const stranger = gitIn(clone, [...IDENT, "commit-tree", gitIn(clone, ["rev-parse", `${base}^{tree}`]), "-m", "unrelated"]);
    gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, stranger]);
    assert.equal((await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, contextFor(N))).kind, "updated");
    const internals = cache as unknown as Internals;
    await internals.writeOwedFile(bare, `receipt-${"a".repeat(64)}.json`, { not: "a receipt" });
    const before = fs.readdirSync(path.join(bare, "uzi-owed")).sort();
    await cache.reconcileOwedCandidates(bare, RUN, heads[N - 1]!, contextFor(N).context);
    assert.deepEqual(fs.readdirSync(path.join(bare, "uzi-owed")).filter((f) => f.startsWith("context-")).sort(),
      before.filter((f) => f.startsWith("context-")), "an unreadable receipt blocks every prune");
  });
});
