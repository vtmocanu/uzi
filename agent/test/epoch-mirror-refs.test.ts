import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-r9";
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
  pending = 0; stopped = false; live = true;
  // Contract double counts real Git participation; descriptor ownership is tested separately.
  scope = {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { return [...stdio]; },
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


// Mirror refs (owner answer seq 1162, 2026-11-06 (R9)): a clone ref the trusted roots do not hold is satisfied by an
// exact bare ref whose whole closure lies in the bare's OWN object store.

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
async function observation(): Promise<boolean> {
  const proof = git.createEpochDiscardProof(input);
  const state = git["epochDiscardProofs"].get(proof)!;
  return git["epochRetained"].run(git["epochLimits"]({}), () =>
    git["withEpochNativeReads"](proof, () => git["automaticContentPreserved"](state)));
}
const IDENT = ["-c", "user.name=fixture", "-c", "user.email=fixture@example.test"];
/** A commit on top of the bare's main, written into the BARE's own object store. */
function bareCommit(message: string): string {
  return command(bare, ...IDENT, "commit-tree", "refs/heads/main^{tree}", "-p", "refs/heads/main", "-m", message);
}
/** A clone ref without a reflog: reflog ids are required on their own, so they would hide the mirror rule. */
function cloneRef(ref: string, id: string): void {
  command(clonePath, "update-ref", ref, id);
  fs.rmSync(path.join(clonePath, ".git", "logs", ref), { force: true });
}
function bareRef(ref: string, id: string): void { command(bare, "update-ref", ref, id); }
function bareTag(name: string, id: string, message = "annotated"): string {
  command(bare, ...IDENT, "tag", "-a", "-m", message, name, id);
  return command(bare, "rev-parse", `refs/tags/${name}`);
}
/** A managed alternate store (a bare repo beside the worker bare) that the BARE borrows from. */
function bareAlternate(name: string): string {
  const alt = path.join(path.dirname(bare), name);
  command(bare, "init", "--bare", alt); fs.chmodSync(alt, 0o700);
  return alt;
}
function borrow(alt: string): void {
  fs.appendFileSync(path.join(bare, "objects/info/alternates"), path.join(alt, "objects") + "\n");
}
async function unknown(message: RegExp): Promise<void> {
  await assert.rejects(observation(), error => (error as Error).constructor.name === "AutomaticCompletenessUnknown" &&
    message.test((error as Error).message));
}

describe("mirror refs", () => {
  it("control: an unpublished clone ref with no bare counterpart keeps the source", async () => {
    cloneRef("refs/remotes/origin/feature", bareCommit("feature"));
    assert.equal(await observation(), false);
  });
  it("accepts a clone origin ref whose bare branch is equal and whose closure is in the bare's own store", async () => {
    const id = bareCommit("feature");
    cloneRef("refs/remotes/origin/feature", id);
    bareRef("refs/heads/feature", id);
    assert.equal(await observation(), true);
    assert.equal(journal(), raw);
  });
  it("a missing bare ref stays required", async () => {
    const id = bareCommit("feature");
    cloneRef("refs/remotes/origin/feature", id);
    bareRef("refs/heads/feature", id);
    assert.equal(await observation(), true);
    command(bare, "update-ref", "-d", "refs/heads/feature");
    assert.equal(await observation(), false);
  });
  it("a moved bare ref is not satisfied", async () => {
    const id = bareCommit("feature");
    cloneRef("refs/remotes/origin/feature", id);
    bareRef("refs/heads/feature", bareCommit("elsewhere"));
    assert.equal(await observation(), false);
  });
  it("maps only refs/remotes/origin/<n> to refs/heads/<n>: other namespaces and crossings are not satisfied", async () => {
    const id = bareCommit("feature");
    bareRef("refs/heads/feature", id);
    for (const ref of ["refs/remotes/upstream/feature", "refs/heads/feature", "refs/notes/feature", "refs/private/feature"]) {
      cloneRef(ref, id);
      assert.equal(await observation(), false, ref);
      command(clonePath, "update-ref", "-d", ref);
    }
    cloneRef("refs/tags/feature", id);
    assert.equal(await observation(), false, "a clone tag is compared with a bare TAG, never a bare branch");
    command(clonePath, "update-ref", "-d", "refs/tags/feature");
    cloneRef("refs/remotes/origin/feature", id);
    command(bare, "update-ref", "-d", "refs/heads/feature");
    bareRef("refs/tags/feature", id);
    assert.equal(await observation(), false, "a clone origin ref is compared with a bare BRANCH, never a bare tag");
  });
  it("a clone stash is never satisfied by a mirror", async () => {
    const id = bareCommit("stash");
    bareRef("refs/heads/stash", id);
    cloneRef("refs/stash", id);
    assert.equal(await observation(), false);
  });
  describe("annotated tags", () => {
    it("equal tag objects are satisfied", async () => {
      const tag = bareTag("v1", bareCommit("tagged"));
      cloneRef("refs/tags/v1", tag);
      assert.equal(await observation(), true);
    });
    it("a lightweight tag at an equal commit is satisfied", async () => {
      const id = bareCommit("light");
      bareRef("refs/tags/light", id);
      cloneRef("refs/tags/light", id);
      assert.equal(await observation(), true);
    });
    it("the same peeled commit under a different tag object is not satisfied", async () => {
      const id = bareCommit("tagged");
      const first = bareTag("v1", id, "first");
      const second = bareTag("v1-other", id, "second");
      assert.notEqual(first, second);
      assert.equal(command(bare, "rev-parse", `${first}^{commit}`), command(bare, "rev-parse", `${second}^{commit}`));
      bareRef("refs/tags/v1", second);
      cloneRef("refs/tags/v1", first);
      assert.equal(await observation(), false);
    });
  });
  describe("the target's objects must stay in the bare's own store", () => {
    it("a bare ref whose commit lives only in an alternate is not satisfied", async () => {
      const alt = bareAlternate("mirror-alt.git");
      const tree = command(alt, "mktree", "--missing") || execFileSync("git", ["-C", alt, "mktree"], { env: ENV, encoding: "utf8", input: "" }).trim();
      const id = command(alt, ...IDENT, "commit-tree", tree, "-m", "alternate only");
      borrow(alt);
      bareRef("refs/heads/feature", id);
      cloneRef("refs/remotes/origin/feature", id);
      assert.equal(command(bare, "cat-file", "-t", id), "commit", "control: the bare can read it through the alternate");
      assert.equal(await observation(), false);
    });
    it("a bare commit whose tree lives only in an alternate is not satisfied", async () => {
      const alt = bareAlternate("mirror-alt.git");
      const blob = execFileSync("git", ["-C", alt, "hash-object", "-w", "--stdin"], { env: ENV, encoding: "utf8", input: "alternate blob\n" }).trim();
      const tree = execFileSync("git", ["-C", alt, "mktree"], { env: ENV, encoding: "utf8", input: `100644 blob ${blob}\tf\n` }).trim();
      const id = command(alt, ...IDENT, "commit-tree", tree, "-m", "split closure");
      const body = execFileSync("git", ["-C", alt, "cat-file", "commit", id], { env: ENV });
      // Only the commit object is written into the bare's own store; its tree and blob stay in the alternate.
      const written = execFileSync("git", ["-C", bare, "hash-object", "-w", "--literally", "-t", "commit", "--stdin"], { env: ENV, encoding: "utf8", input: body }).trim();
      assert.equal(written, id);
      borrow(alt);
      bareRef("refs/heads/feature", id);
      cloneRef("refs/remotes/origin/feature", id);
      assert.equal(command(bare, "rev-list", "--objects", "--no-object-names", id).split("\n").length, 3);
      assert.equal(await observation(), false);
    });
    it("an object only in the clone's own store is not covered by a bare ref", async () => {
      const id = command(clonePath, ...IDENT, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "clone only");
      cloneRef("refs/remotes/origin/feature", id);
      bareRef("refs/heads/feature", bareCommit("different"));
      assert.equal(await observation(), false);
    });
  });
  describe("refs/remotes/origin/HEAD", () => {
    it("is satisfied when its symref target is satisfied", async () => {
      const id = bareCommit("feature");
      cloneRef("refs/remotes/origin/feature", id);
      command(clonePath, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/feature");
      fs.rmSync(path.join(clonePath, ".git", "logs", "refs", "remotes", "origin", "HEAD"), { force: true });
      bareRef("refs/heads/feature", id);
      assert.equal(await observation(), true);
    });
    it("is not satisfied when its symref target is not", async () => {
      cloneRef("refs/remotes/origin/feature", bareCommit("feature"));
      command(clonePath, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/feature");
      fs.rmSync(path.join(clonePath, ".git", "logs", "refs", "remotes", "origin", "HEAD"), { force: true });
      assert.equal(await observation(), false);
    });
    it("is not satisfied by a differently shaped symref target", async () => {
      const id = bareCommit("local");
      bareRef("refs/heads/local", id);
      cloneRef("refs/heads/local", id);
      command(clonePath, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/local");
      fs.rmSync(path.join(clonePath, ".git", "logs", "refs", "remotes", "origin", "HEAD"), { force: true });
      assert.equal(await observation(), false);
    });
  });
  describe("a concurrent ref change is UNKNOWN", () => {
    function setup(): string {
      const id = bareCommit("feature");
      cloneRef("refs/remotes/origin/feature", id);
      bareRef("refs/heads/feature", id);
      return id;
    }
    it("a bare ref moving between the first read and the final re-check", async t => {
      setup();
      // Written before the proof starts, so the move changes a ref value and no object store.
      const elsewhere = bareCommit("elsewhere");
      const original = git["runGitWithEnv"].bind(git);
      let reads = 0;
      t.mock.method(git as unknown as { runGitWithEnv: typeof original }, "runGitWithEnv", async (...args: Parameters<typeof original>) => {
        const out = await original(...args);
        if (args[0] === bare && args[1].includes("refs/heads/") && args[1].includes("refs/tags/") && ++reads === 1)
          bareRef("refs/heads/feature", elsewhere);
        return out;
      });
      await unknown(/object observations changed/);
      assert.equal(reads, 2, "the bare refs were read once and re-read once");
    });
    it("a clone ref moving between the first read and the final re-check", async t => {
      setup();
      const elsewhere = bareCommit("elsewhere");
      const original = git["runGitAsRunner"].bind(git);
      let reads = 0;
      t.mock.method(git as unknown as { runGitAsRunner: typeof original }, "runGitAsRunner", async (...args: Parameters<typeof original>) => {
        const out = await original(...args);
        if (args[1].includes("for-each-ref") && ++reads === 1) cloneRef("refs/remotes/origin/late", elsewhere);
        return out;
      });
      await unknown(/object observations changed/);
    });
    it("a bare ref created between the reads is also UNKNOWN", async t => {
      setup();
      const original = git["runGitWithEnv"].bind(git);
      let reads = 0;
      t.mock.method(git as unknown as { runGitWithEnv: typeof original }, "runGitWithEnv", async (...args: Parameters<typeof original>) => {
        const out = await original(...args);
        if (args[0] === bare && args[1].includes("refs/heads/") && args[1].includes("refs/tags/") && ++reads === 1)
          bareRef("refs/heads/unrelated", command(bare, "rev-parse", "refs/heads/main"));
        return out;
      });
      await unknown(/object observations changed/);
    });
  });
});
