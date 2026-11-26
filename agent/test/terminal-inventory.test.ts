import { NO_SCAN_NEEDLES } from "./run-env-test-support.js";
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type PositiveOwedCandidateContext } from "../src/git.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { RUN_KINDS } from "../src/protocol.js";
import {
  checkManifestCoverage,
  classifyTerminalOutput,
  readTerminalSourceUniverse,
  type TerminalUniverse,
} from "../src/terminal-inventory.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #4007 slice B M3: the read-side inventory, classification and omission check, against REAL git
// fixtures (bares, attempt ledgers, journals, owed pins, recovery records and clones).

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const RUN = "run-terminal";
const BRANCH = "task";

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
    "-c", "commit.gpgsign=false", ...args], { env: ENV, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 20000 }).trim();
}

let counter = 0;
function attemptId(generation = 1): string {
  counter++;
  return formatAttemptId(new Date(Date.UTC(2026, 0, 1, 0, 0, counter)), generation, counter.toString(16).padStart(16, "0"));
}

const roots: string[] = [];
afterEach(async () => {
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});

interface Attempt { attemptId: string; clonePath: string; bare: string; head: string; identity: string }

class Rig {
  readonly root = makeScratchRoot("terminal-inventory-");
  readonly data = path.join(this.root, "data");
  readonly cache: GitCache;
  readonly coordinator: RecoveryCoordinator;
  readonly signal = new AbortController().signal;
  constructor() {
    roots.push(this.root);
    this.cache = new GitCache(this.data, nullLogger(), undefined, testGitCacheOptions());
    this.coordinator = new RecoveryCoordinator({ ...NO_SCAN_NEEDLES,
      recoveryRoot: path.join(this.data, "recovery"), workerToken: "fixture-worker",
      git: this.cache, client: { hasFeature: () => true, getRunOwnership: async () => ({ status: "running", claim_generation: 1, inventory_guarded: true }),
        listRecoveryHolds: async () => ({ run_id: RUN, holds: [{ hold_id: "hold", generation: 1, inventory_guarded: true, has_available_capture: false }] }) } as never, log: nullLogger(),
    });
  }

  async bare(name = "repo.git"): Promise<string> {
    const bare = path.join(this.data, "repos", name);
    await fs.mkdir(path.dirname(bare), { recursive: true, mode: 0o700 });
    execFileSync("git", ["init", "--bare", bare], { env: ENV, stdio: "pipe", timeout: 20000 });
    return bare;
  }

  /** One attempt clone with two commits, its ledger value and (optionally) its journal binding. */
  async attempt(bare: string, opts: { runId?: string; branch?: string; id?: string; state?: string; journal?: boolean; present?: boolean; key?: string } = {}): Promise<Attempt> {
    const id = opts.id ?? attemptId();
    const clonePath = `${this.cache.runnerClonePath(bare, opts.key ?? "issue-1")}.attempt-${id}`;
    let head = "";
    if (opts.present !== false) {
      await fs.mkdir(clonePath, { recursive: true });
      git(clonePath, "init", "-b", opts.branch ?? BRANCH);
      await fs.writeFile(path.join(clonePath, "work.txt"), "one");
      git(clonePath, "add", "work.txt");
      git(clonePath, "commit", "-m", "one");
      await fs.writeFile(path.join(clonePath, "work.txt"), "two");
      git(clonePath, "commit", "-am", "two");
      head = git(clonePath, "rev-parse", "HEAD");
    }
    git(bare, "config", "--local", "--add", `uzi-attempts.${opts.branch ?? BRANCH}.entry`,
      JSON.stringify({ attemptId: id, runId: opts.runId ?? RUN, clonePath, state: opts.state ?? "live" }));
    if (opts.journal !== false && opts.present !== false) await this.cache.markRecoveryCapture(bare, clonePath, opts.branch ?? BRANCH, opts.runId ?? RUN, id);
    const st = opts.present === false ? undefined : await fs.lstat(clonePath);
    return { attemptId: id, clonePath, bare, head, identity: st ? `${st.dev}:${st.ino}` : "" };
  }

  universe(current: Attempt, over: Record<string, unknown> = {}, cache: unknown = this.cache): Promise<TerminalUniverse> {
    return readTerminalSourceUniverse({
      git: cache as GitCache, recovery: this.coordinator, runId: RUN,
      current: { attemptId: current.attemptId, clonePath: current.clonePath, identity: current.identity, bare: current.bare },
      signal: this.signal, deadline: Date.now() + 60000, ...over,
    });
  }

  budget() { return { signal: this.signal, deadline: Date.now() + 60000 }; }

  /** The cache with some methods replaced (for evidence a fixture cannot build cheaply). */
  fake(overrides: Record<string, unknown>): GitCache {
    return Object.assign(Object.create(this.cache) as GitCache, overrides);
  }
}

function unproved(u: TerminalUniverse): { reason: string; subreason?: string } {
  assert.equal(u.kind, "unproved", JSON.stringify(u));
  return u as { reason: string; subreason?: string };
}

describe("classifyTerminalOutput", () => {
  it("maps every registry run kind to a label other than unknown", async () => {
    const registry = JSON.parse(await fs.readFile(path.resolve(import.meta.dirname, "..", "..", "fixtures", "run-kinds", "registry.json"), "utf8")) as { kinds: string[] };
    assert.deepEqual([...registry.kinds].sort(), [...RUN_KINDS].sort());
    for (const kind of registry.kinds) {
      assert.notEqual(classifyTerminalOutput({ kind, adoptedSource: true }), "unknown", kind);
      assert.notEqual(classifyTerminalOutput({ kind }), "unknown", kind);
    }
  });

  it("labels each class and refuses an unknown kind", () => {
    const issue = { kind: "issue", adoptedSource: true };
    assert.equal(classifyTerminalOutput(issue), "branch_source");
    assert.equal(classifyTerminalOutput({ kind: "mr_rework", adoptedSource: true }), "branch_source");
    assert.equal(classifyTerminalOutput({ kind: "self_improve", adoptedSource: true }), "branch_source");
    assert.equal(classifyTerminalOutput({ ...issue, egressBound: true }), "other_code_kind");
    assert.equal(classifyTerminalOutput({ kind: "issue" }), "no_source");
    assert.equal(classifyTerminalOutput({ ...issue, repoLess: true }), "repo_less");
    assert.equal(classifyTerminalOutput({ kind: "ci_fix", adoptedSource: true }), "other_code_kind");
    assert.equal(classifyTerminalOutput({ kind: "judge" }), "judge_trace");
    assert.equal(classifyTerminalOutput({ kind: "chat" }), "chat_job_workspace");
    assert.equal(classifyTerminalOutput({ kind: "job" }), "chat_job_workspace");
    assert.equal(classifyTerminalOutput({ kind: "cross_check" }), "code_checker_snapshot");
    assert.equal(classifyTerminalOutput({ kind: "task", reportOnly: true }), "report_task");
    assert.equal(classifyTerminalOutput({ kind: "task", adoptedSource: true }), "other_code_kind");
    assert.equal(classifyTerminalOutput({ kind: "not-a-kind", adoptedSource: true }), "unknown");
  });
});

describe("readTerminalCloneLocalRefs", () => {
  async function clone(rig: Rig): Promise<{ clone: string; bare: string; a: string; head: string }> {
    const bare = await rig.bare();
    const at = await rig.attempt(bare, { journal: false });
    const a = git(at.clonePath, "rev-parse", "HEAD~1");
    return { clone: at.clonePath, bare, a, head: at.head };
  }

  it("accepts a clone whose refs are all ancestors of HEAD, origin tracking included", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "branch", "old", c.a);
    git(c.clone, "update-ref", "refs/remotes/origin/main", c.a);
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: true });
  });

  it("refuses a divergent branch whose SHA the bare holds only under a transient ref", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "checkout", "-b", "side", c.a);
    await fs.writeFile(path.join(c.clone, "side.txt"), "side");
    git(c.clone, "add", "side.txt");
    git(c.clone, "commit", "-m", "side");
    const side = git(c.clone, "rev-parse", "HEAD");
    git(c.clone, "checkout", "-q", "task");
    git(c.clone, "push", c.bare, `${side}:refs/uzi-checkpoint/transient`);
    assert.equal(git(c.bare, "rev-parse", "refs/uzi-checkpoint/transient"), side);
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "not_ancestor" });
  });

  it("refuses an off-HEAD tag", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "checkout", "-q", "--detach", c.a);
    await fs.writeFile(path.join(c.clone, "t.txt"), "t");
    git(c.clone, "add", "t.txt");
    git(c.clone, "commit", "-m", "tagged");
    git(c.clone, "tag", "off-head");
    git(c.clone, "checkout", "-q", "task");
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "not_ancestor" });
  });

  it("refuses a stash", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    await fs.writeFile(path.join(c.clone, "work.txt"), "dirty");
    git(c.clone, "stash");
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "stash" });
  });

  it("refuses a linked worktree", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "worktree", "add", "-b", "linked", path.join(rig.root, "linked-wt"), c.a);
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "linked_worktree" });
  });

  it("refuses origin/* that is not an ancestor of HEAD even when the bare tracking ref agrees", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "checkout", "-q", "--detach", c.a);
    await fs.writeFile(path.join(c.clone, "o.txt"), "origin");
    git(c.clone, "add", "o.txt");
    git(c.clone, "commit", "-m", "origin only");
    const other = git(c.clone, "rev-parse", "HEAD");
    git(c.clone, "checkout", "-q", "task");
    git(c.clone, "update-ref", "refs/remotes/origin/main", other);
    git(c.clone, "push", c.bare, `${other}:refs/remotes/origin/main`);
    assert.equal(git(c.bare, "rev-parse", "refs/remotes/origin/main"), other);
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "not_ancestor" });
  });

  it("is not fooled by an info/grafts file that forges ancestry", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "checkout", "-b", "side", c.a);
    await fs.writeFile(path.join(c.clone, "side.txt"), "side");
    git(c.clone, "add", "side.txt");
    git(c.clone, "commit", "-m", "side");
    const side = git(c.clone, "rev-parse", "HEAD");
    git(c.clone, "checkout", "-q", "task");
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "not_ancestor" });
    await fs.mkdir(path.join(c.clone, ".git", "info"), { recursive: true });
    await fs.writeFile(path.join(c.clone, ".git", "info", "grafts"), `${c.head} ${side}\n`);
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "not_ancestor" });
  });

  it("a commit-graph cannot be written over forged grafts, so the walk still refuses (core.commitGraph=false is defence in depth, not covered here)", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    git(c.clone, "checkout", "-b", "side", c.a);
    await fs.writeFile(path.join(c.clone, "side.txt"), "side");
    git(c.clone, "add", "side.txt");
    git(c.clone, "commit", "-m", "side");
    const side = git(c.clone, "rev-parse", "HEAD");
    git(c.clone, "checkout", "-q", "task");
    await fs.mkdir(path.join(c.clone, ".git", "info"), { recursive: true });
    await fs.writeFile(path.join(c.clone, ".git", "info", "grafts"), `${c.head} ${side}\n`);
    git(c.clone, "commit-graph", "write", "--reachable");
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, c.head), { ok: false, reason: "not_ancestor" });
  });

  it("refuses an unreadable clone and a malformed head", async () => {
    const rig = new Rig();
    const c = await clone(rig);
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(path.join(rig.root, "missing"), c.head), { ok: false, reason: "unreadable" });
    assert.deepEqual(await rig.cache.readTerminalCloneLocalRefs(c.clone, "not-a-sha"), { ok: false, reason: "unreadable" });
  });
});

describe("readTerminalSourceUniverse", () => {
  it("proves the current attempt alone and is deterministic", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const first = await rig.universe(current);
    assert.equal(first.kind, "proved", JSON.stringify(first));
    if (first.kind !== "proved") return;
    assert.equal(first.sources.length, 1);
    assert.equal(first.sources[0]!.head, current.head);
    assert.deepEqual(first.roots, [{ commit: current.head, origin: "current_head" }]);
    const second = await rig.universe(current);
    assert.equal(second.kind === "proved" && second.universeDigest, first.universeDigest);
  });

  it("refuses when the own clone is missing, or its identity differs", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    assert.equal(unproved(await rig.universe({ ...current, clonePath: `${current.clonePath}-gone` })).reason, "current_missing");
    assert.equal(unproved(await rig.universe({ ...current, identity: "1:1" })).reason, "identity_mismatch");
  });

  it("refuses an older present attempt clone that is not the current flight's", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    await rig.attempt(bare, { state: "retired", journal: false });
    const current = await rig.attempt(bare);
    assert.equal(unproved(await rig.universe(current)).reason, "non_current_clone_present");
  });

  it("refuses a foreign owner in the bare", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    await rig.attempt(bare, { runId: "run-other", branch: "other", journal: false, key: "issue-2" });
    assert.equal(unproved(await rig.universe(current)).reason, "foreign_owner");
  });

  it("refuses a retained predecessor", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const real = rig.cache.readInventoryCloneHeads.bind(rig.cache);
    const fake = rig.fake({
      readInventoryCloneHeads: async (...args: Parameters<GitCache["readInventoryCloneHeads"]>) => {
        const result = await real(...args);
        return result.kind === "verified"
          ? { ...result, retainedPredecessors: [{ clonePath: "/p", branch: BRANCH, runId: RUN, attemptId: "p", generation: 1 }] } : result;
      },
    });
    assert.equal(unproved(await rig.universe(current, {}, fake)).reason, "predecessor");
  });

  it("refuses the same attempt in two bares as a duplicate", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const second = await rig.bare("second.git");
    const current = await rig.attempt(bare);
    await fs.mkdir(path.dirname(rig.cache.runnerClonePath(second, "issue-1")), { recursive: true });
    git(second, "config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, JSON.stringify({
      attemptId: current.attemptId, runId: RUN,
      clonePath: `${rig.cache.runnerClonePath(second, "issue-1")}.attempt-${current.attemptId}`, state: "abandoned" }));
    assert.deepEqual(await rig.universe(current), { kind: "unproved", reason: "duplicate", detail: `attempt ${current.attemptId} in two ledgers` });
  });

  it("refuses a source in a bare that is not the current bare", async () => {
    const rig = new Rig();
    const home = await rig.bare("home.git");
    const other = await rig.bare("other.git");
    const current = await rig.attempt(other);
    assert.equal(unproved(await rig.universe({ ...current, bare: home })).reason, "foreign_bare");
  });

  it("refuses a clone with no ledger attempt (a canonical journaled clone)", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const canonical = rig.cache.runnerClonePath(bare, "issue-9");
    await fs.mkdir(canonical, { recursive: true });
    git(canonical, "init", "-b", "other");
    git(canonical, "commit", "--allow-empty", "-m", "canonical");
    await rig.cache.markRecoveryCapture(bare, canonical, "other", RUN);
    assert.equal(unproved(await rig.universe(current)).reason, "join_contradiction");
  });

  it("refuses a ledger attempt that is present on disk but unlisted by the inventory", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const real = rig.cache.readInventoryCloneHeads.bind(rig.cache);
    const fake = rig.fake({
      readInventoryCloneHeads: async (...args: Parameters<GitCache["readInventoryCloneHeads"]>) => {
        const result = await real(...args);
        return result.kind === "verified" ? { ...result, clones: [] } : result;
      },
    });
    assert.equal(unproved(await rig.universe(current, {}, fake)).reason, "join_contradiction");
  });

  it("refuses a corrupt ledger value", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    git(bare, "config", "--local", "--add", "uzi-attempts.broken.entry", "{");
    assert.equal(unproved(await rig.universe(current)).reason, "unknown_read");
    assert.deepEqual(await rig.cache.readTerminalAttribution(bare, RUN, rig.budget()), { kind: "unknown", cause: "malformed_ledger" });
  });

  it("refuses a malformed stopped marker", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    await rig.attempt(bare);
    git(bare, "config", "--local", "--add", `uzi-stopped.${RUN}.entry`, "{");
    const read = await rig.cache.readTerminalAttribution(bare, RUN, rig.budget());
    assert.equal(read.kind, "unknown");
  });

  it("refuses a record bound to no attempt of the run", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    assert.ok(await rig.coordinator.pin({ runId: RUN, generation: 1, sourceSha: current.head, kind: "issue", branch: "unrelated-branch" }));
    assert.equal(unproved(await rig.universe(current)).reason, "record_outside_universe");
  });

  it("refuses a corrupt record and a bundle that no record owns", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const dir = path.join(rig.data, "recovery", RUN);
    await fs.mkdir(dir, { recursive: true });
    await fs.writeFile(path.join(dir, "stray.bundle"), "x");
    assert.equal(unproved(await rig.universe(current)).reason, "unknown_read");
    await fs.rm(path.join(dir, "stray.bundle"));
    await fs.writeFile(path.join(dir, "garbage.json"), "{");
    assert.equal(unproved(await rig.universe(current)).reason, "unknown_read");
  });

  it("refuses when a read throws, and when the deadline has passed", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const throwing = rig.fake({ listRunBares: async () => { throw new Error("boom"); } });
    assert.equal(unproved(await rig.universe(current, {}, throwing)).reason, "thrown");
    assert.equal(unproved(await rig.universe(current, { deadline: Date.now() - 1 })).reason, "deadline");
  });

  it("refuses more bares than the cap", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const many = rig.fake({ listRunBares: async () => Array.from({ length: 65 }, (_, i) => `/b${i}.git`) });
    assert.equal(unproved(await rig.universe(current, {}, many)).reason, "cap");
  });

  it("refuses a current clone that carries a stash", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    await fs.writeFile(path.join(current.clonePath, "work.txt"), "dirty");
    git(current.clonePath, "stash");
    assert.deepEqual(await rig.universe(current), { kind: "unproved", reason: "local_ref", subreason: "stash", detail: current.clonePath });
  });

  const fakeRecovery = (records: Array<Record<string, unknown>>) =>
    ({ readTerminalRecords: async () => ({ kind: "records", records }) });

  it("binds a record only to the proved source: same branch and the current bare", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const second = await rig.bare("second.git");
    const current = await rig.attempt(bare);
    const rec = (over: Record<string, unknown>) => ({ captureId: "c1", branch: BRANCH, sourceSha: current.head, generation: 1, ...over });
    const run = (r: Record<string, unknown>) => rig.universe(current, { recovery: fakeRecovery([r]) });
    assert.equal((await run(rec({ bareDir: path.basename(bare) }))).kind, "proved");
    assert.equal(unproved(await run(rec({ bareDir: path.basename(second) }))).reason, "record_outside_universe");
    // Two bares exist: a record without a bareDir cannot be placed.
    assert.equal(unproved(await run(rec({}))).reason, "record_outside_universe");
    assert.equal(unproved(await run(rec({ bareDir: path.basename(bare), branch: "other" }))).reason, "record_outside_universe");
  });

  it("binds a bareDir-less record when exactly one bare exists and the branch matches", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const rec = { captureId: "c1", branch: BRANCH, sourceSha: current.head, generation: 1 };
    assert.equal((await rig.universe(current, { recovery: fakeRecovery([rec]) })).kind, "proved");
  });

  describe("a bound record's sourceSha must be covered", () => {
    const bound = (sourceSha: string, over: Record<string, unknown> = {}) => ({ captureId: "c1", branch: BRANCH, sourceSha, generation: 1, ...over });

    it("refuses an unrelated sourceSha, which the manifest would never declare", async () => {
      const rig = new Rig();
      const bare = await rig.bare();
      const current = await rig.attempt(bare);
      const u = await rig.universe(current, { recovery: fakeRecovery([bound("f".repeat(40))]) });
      assert.equal(unproved(u).reason, "record_root_uncovered");
    });

    it("accepts an ancestor of the head, and refuses a commit the bare knows that is not one", async () => {
      const rig = new Rig();
      const bare = await rig.bare();
      const current = await rig.attempt(bare);
      git(bare, "fetch", current.clonePath, `refs/heads/${BRANCH}:refs/heads/${BRANCH}`);
      git(current.clonePath, "checkout", "-b", "side", "HEAD~1");
      await fs.writeFile(path.join(current.clonePath, "side.txt"), "x");
      git(current.clonePath, "add", "side.txt");
      git(current.clonePath, "commit", "-m", "side");
      const side = git(current.clonePath, "rev-parse", "HEAD");
      git(current.clonePath, "checkout", BRANCH);
      git(bare, "fetch", current.clonePath, "refs/heads/side:refs/heads/side");
      git(current.clonePath, "branch", "-D", "side");
      const parent = git(current.clonePath, "rev-parse", "HEAD~1");
      assert.equal((await rig.universe(current, { recovery: fakeRecovery([bound(parent)]) })).kind, "proved");
      assert.equal(unproved(await rig.universe(current, { recovery: fakeRecovery([bound(side)]) })).reason, "record_root_uncovered");
    });

    it("keeps the bound record identities in the universe digest", async () => {
      const rig = new Rig();
      const bare = await rig.bare();
      const current = await rig.attempt(bare);
      const digest = async (records: Array<Record<string, unknown>>) => {
        const u = await rig.universe(current, { recovery: fakeRecovery(records) });
        assert.equal(u.kind, "proved", JSON.stringify(u));
        return u.kind === "proved" ? u.universeDigest : "";
      };
      const base = await digest([]);
      const one = await digest([bound(current.head)]);
      assert.notEqual(one, base);
      assert.equal(await digest([bound(current.head)]), one);
      assert.notEqual(await digest([bound(current.head, { captureId: "c2" })]), one);
      assert.notEqual(await digest([bound(current.head, { generation: 2 })]), one);
      assert.notEqual(await digest([bound(current.head, { bundlePath: "/x/c1.bundle" })]), one);
    });
  });

  it("accepts a record on a retired attempt's branch only when its sourceSha is an explicit root", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const second = await rig.bare("second.git");
    const retired = await rig.attempt(bare, { state: "retired", journal: false, present: false, branch: "gone" });
    const current = await rig.attempt(bare);
    assert.ok(retired.attemptId);
    const rec = (sourceSha: string, dir = path.basename(bare)) => ({ captureId: "c1", branch: "gone", bareDir: dir, sourceSha, generation: 1 });
    assert.equal(unproved(await rig.universe(current, { recovery: fakeRecovery([rec("f".repeat(40))]) })).reason, "record_outside_universe");
    assert.equal((await rig.universe(current, { recovery: fakeRecovery([rec(current.head)]) })).kind, "proved");
    // The retired attempt lives in the first bare: a record naming the second bare does not bind to it.
    assert.equal(unproved(await rig.universe(current, { recovery: fakeRecovery([rec(current.head, path.basename(second))]) })).reason,
      "record_outside_universe");
  });

  it("refuses a live attempt whose clone vanished, but allows a retired one with an owed root", async () => {
    const live = new Rig();
    const liveBare = await live.bare();
    const liveCurrent = await live.attempt(liveBare);
    await live.attempt(liveBare, { present: false, state: "live", journal: false });
    assert.equal(unproved(await live.universe(liveCurrent)).reason, "ledger_attempt_clone_missing");

    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    await rig.attempt(bare, { present: false, state: "retired", journal: false });
    await rig.attempt(bare, { present: false, state: "abandoned", journal: false });
    const parent = git(current.clonePath, "rev-parse", "HEAD~1");
    git(current.clonePath, "push", bare, `${current.head}:refs/heads/producer`, `${parent}:refs/heads/parent`);
    const context: PositiveOwedCandidateContext = {
      barePath: bare, runId: RUN, generation: 1, kind: "issue", branch: BRANCH,
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: parent },
    };
    assert.equal((await rig.cache.updateTrackingRef(bare, BRANCH, parent, { context })).kind, "updated");
    const universe = await rig.universe(current);
    assert.equal(universe.kind, "proved", JSON.stringify(universe));
    if (universe.kind !== "proved") return;
    assert.ok(universe.roots.some(r => r.commit === parent && r.origin === "owed"));
  });

  it("refuses an lstat failure other than ENOENT as an unknown read", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const ledgerPath = `${current.clonePath}/work.txt/under-a-file`;
    git(bare, "config", "--local", "--add", "uzi-attempts.other.entry",
      JSON.stringify({ attemptId: attemptId(), runId: RUN, clonePath: ledgerPath, state: "retired" }));
    assert.equal(unproved(await rig.universe(current)).reason, "unknown_read");
  });

  it("refuses the same attempt id under two branch keys of one bare", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    git(bare, "config", "--local", "--add", "uzi-attempts.other.entry",
      JSON.stringify({ attemptId: current.attemptId, runId: RUN, clonePath: `${current.clonePath}-x`, state: "retired" }));
    assert.deepEqual(await rig.cache.readTerminalAttribution(bare, RUN, rig.budget()), { kind: "unknown", cause: "duplicate_attempt" });
  });

  describe("an attempt re-appended under the same key as its state advances", () => {
    const entry = (a: Attempt, over: Record<string, string>) => JSON.stringify({
      attemptId: a.attemptId, runId: RUN, clonePath: a.clonePath, state: "live", ...over });

    it("the last state wins for the requested run (live then reclaimed)", async () => {
      const rig = new Rig();
      const bare = await rig.bare();
      const a = await rig.attempt(bare);
      git(bare, "config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`, entry(a, { state: "reclaimed" }));
      const read = await rig.cache.readTerminalAttribution(bare, RUN, rig.budget());
      assert.equal(read.kind, "verified", JSON.stringify(read));
      assert.deepEqual((read as { attempts: Array<{ attemptId: string; state: string }> }).attempts
        .map(x => [x.attemptId, x.state]), [[a.attemptId, "reclaimed"]]);
    });

    it("an unrelated run's re-appended attempt on the same bare does not block the requested run", async () => {
      const rig = new Rig();
      const bare = await rig.bare();
      const mine = await rig.attempt(bare);
      const other = await rig.attempt(bare, { runId: "run-other", journal: false });
      git(bare, "config", "--local", "--add", `uzi-attempts.${BRANCH}.entry`,
        JSON.stringify({ attemptId: other.attemptId, runId: "run-other", clonePath: other.clonePath, state: "reclaimed" }));
      const read = await rig.cache.readTerminalAttribution(bare, RUN, rig.budget());
      assert.equal(read.kind, "verified", JSON.stringify(read));
      assert.deepEqual((read as { attempts: Array<{ attemptId: string }> }).attempts.map(x => x.attemptId), [mine.attemptId]);
    });

    it("refuses the same attempt id with a conflicting run, clone path or branch", async () => {
      for (const variant of ["runId", "clonePath", "branch"] as const) {
        const rig = new Rig();
        const bare = await rig.bare();
        const a = await rig.attempt(bare);
        const key = variant === "branch" ? "other" : BRANCH;
        const over: Record<string, string> = variant === "runId" ? { runId: "run-other" } : variant === "clonePath" ? { clonePath: `${a.clonePath}-x` } : {};
        git(bare, "config", "--local", "--add", `uzi-attempts.${key}.entry`, entry(a, { ...over, state: "reclaimed" }));
        assert.deepEqual(await rig.cache.readTerminalAttribution(bare, RUN, rig.budget()),
          { kind: "unknown", cause: "duplicate_attempt" }, variant);
      }
    });

    it("refuses a ledger value whose field order is not the canonical one", async () => {
      const rig = new Rig();
      const bare = await rig.bare();
      const a = await rig.attempt(bare);
      git(bare, "config", "--local", "--add", `uzi-attempts.other.entry`,
        JSON.stringify({ attemptId: attemptId(), runId: RUN, state: "retired", clonePath: `${a.clonePath}-y` }));
      assert.deepEqual(await rig.cache.readTerminalAttribution(bare, RUN, rig.budget()), { kind: "unknown", cause: "malformed_ledger" });
    });
  });

  it("refuses a journal whose attempt id differs from the ledger attempt at the same path", async () => {
    const rig = new Rig();
    const bare = await rig.bare();
    const current = await rig.attempt(bare);
    const real = rig.cache.readTerminalAttribution.bind(rig.cache);
    const fake = rig.fake({
      readTerminalAttribution: async (...args: Parameters<GitCache["readTerminalAttribution"]>) => {
        const result = await real(...args);
        return result.kind === "verified"
          ? { ...result, journal: result.journal.map(j => ({ ...j, attemptId: attemptId() })) } : result;
      },
    });
    assert.equal(unproved(await rig.universe(current, {}, fake)).reason, "join_contradiction");
  });

  it("F-gate: an inventory-guarded single-attempt issue flight is proved end to end (clean tree; an untracked file does not change the universe)", async () => {
    for (const dirty of [false, true]) {
      const rig = new Rig();
      const bare = await rig.bare();
      const current = await rig.attempt(bare);
      const parent = git(current.clonePath, "rev-parse", "HEAD~1");
      if (dirty) await fs.writeFile(path.join(current.clonePath, "untracked.txt"), "wip");
      // An owed root: an earlier commit of the flight that the bare owes the server.
      git(current.clonePath, "push", bare, `${current.head}:refs/heads/producer`, `${parent}:refs/heads/parent`);
      const context: PositiveOwedCandidateContext = {
        barePath: bare, runId: RUN, generation: 1, kind: "issue", branch: BRANCH,
        defaultIdentity: { ref: "refs/remotes/origin/main", sha: parent },
      };
      assert.equal((await rig.cache.updateTrackingRef(bare, BRANCH, parent, { context })).kind, "updated");
      const record = await rig.coordinator.pin({ runId: RUN, generation: 1, sourceSha: current.head, kind: "issue", branch: BRANCH, inventoryGuarded: true });
      assert.ok(record);

      assert.deepEqual(await rig.cache.listRunBares(), [bare]);
      const attribution = await rig.cache.readTerminalAttribution(bare, RUN, rig.budget());
      assert.equal(attribution.kind, "verified", JSON.stringify(attribution));
      if (attribution.kind !== "verified") return;
      assert.deepEqual(attribution.attempts, [{ attemptId: current.attemptId, clonePath: current.clonePath, branch: BRANCH, state: "live" }]);
      assert.deepEqual(attribution.journal, [{ branch: BRANCH, clonePath: current.clonePath, attemptId: current.attemptId }]);
      assert.deepEqual(attribution.stoppedMarkers, []);
      assert.deepEqual(attribution.owedRoots, [parent]);

      const universe = await rig.universe(current);
      assert.equal(universe.kind, "proved", JSON.stringify(universe));
      if (universe.kind !== "proved") return;
      const [source] = universe.sources;
      assert.equal(universe.sources.length, 1);
      assert.equal(source!.attemptId, current.attemptId);
      assert.equal(source!.clonePath, current.clonePath);
      assert.equal(source!.head, current.head);
      assert.equal(source!.identity, current.identity);
      assert.equal(source!.bare, bare);
      assert.equal(source!.generation, 1);
      assert.deepEqual(source!.recordGenerations, [1]);
      assert.deepEqual(universe.roots, [{ commit: current.head, origin: "current_head" }, { commit: parent, origin: "owed" }]
        .sort((a, b) => (a.commit < b.commit ? -1 : 1)));
      assert.equal(classifyTerminalOutput({ kind: "issue", adoptedSource: true }), "branch_source");

      // The omission check accepts exactly this universe and refuses an omission of the owed root.
      const tree = git(current.clonePath, "rev-parse", "HEAD^{tree}");
      const parentTree = git(current.clonePath, "rev-parse", `${parent}^{tree}`);
      const rootsOf = [
        { source_id: source!.sourceId, attempt_id: source!.attemptId, commit: current.head, tree },
        { source_id: source!.sourceId, attempt_id: source!.attemptId, commit: parent, tree: parentTree },
      ];
      const trees = new Map([[current.head, tree], [parent, parentTree]]);
      assert.deepEqual(checkManifestCoverage(universe, { roots: rootsOf, abandoned: [] }, { currentFlight: true, expectedTrees: trees }), { ok: true });
      assert.equal((checkManifestCoverage(universe, { roots: rootsOf.slice(0, 1), abandoned: [] }, { currentFlight: true }) as { reason: string }).reason, "missing_root");
    }
  });
});

describe("checkManifestCoverage", () => {
  const head = "a".repeat(40), owed = "b".repeat(40);
  const universe = {
    kind: "proved" as const, universeDigest: "d",
    sources: [{ sourceId: "s1", attemptId: "att", clonePath: "/c", head, identity: "1:2", bare: "/b", branch: BRANCH, generation: 1, recordGenerations: [] }],
    roots: [{ commit: head, origin: "current_head" as const }, { commit: owed, origin: "owed" as const }],
  };
  const root = (commit: string, source_id = "s1", attempt_id = "att", tree = "t") => ({ source_id, attempt_id, commit, tree });
  const full = [root(head), root(owed)];
  const reasonOf = (r: ReturnType<typeof checkManifestCoverage>) => (r.ok ? "ok" : r.reason);

  it("accepts the exact universe", () => {
    assert.deepEqual(checkManifestCoverage(universe, { roots: full, abandoned: [{ class: "ignored", source_id: "s1", extents: { files: 1, bytes: 1 } }] }, { currentFlight: true }), { ok: true });
  });
  it("refuses a missing source, a missing root, an extra root and an extra source", () => {
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [], abandoned: [] }, { currentFlight: true })), "missing_source");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [root(head)], abandoned: [] }, { currentFlight: true })), "missing_root");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [...full, root("c".repeat(40))], abandoned: [] }, { currentFlight: true })), "extra_root");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [...full, root(head, "s2")], abandoned: [] }, { currentFlight: true })), "extra_source");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [root(head, "s1", "other"), root(owed)], abandoned: [] }, { currentFlight: true })), "extra_source");
  });
  it("refuses a duplicate root and a source whose head is not a manifest root", () => {
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [...full, root(owed)], abandoned: [] }, { currentFlight: true })), "duplicate_root");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: [root(owed)], abandoned: [] }, { currentFlight: true })), "commit_mismatch");
  });
  it("refuses a duplicated abandoned entry", () => {
    const item = { class: "ignored" as const, source_id: "s1", extents: { files: 1, bytes: 1 } };
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: full, abandoned: [item, item] }, { currentFlight: true })), "duplicate_abandoned");
  });
  it("refuses a tree mismatch against the expected trees", () => {
    const expected = new Map([[head, "t"], [owed, "other-tree"]]);
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: full, abandoned: [] }, { currentFlight: true, expectedTrees: expected })), "tree_mismatch");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: full, abandoned: [] }, { currentFlight: true, expectedTrees: new Map([[head, "t"]]) })), "tree_mismatch");
  });
  it("admits exactly the one declared snapshot root, and refuses it missing, substituted or unexpected", () => {
    const snap = { commit: "d".repeat(40), tree: "snaptree" };
    const snapRoot = (over: { commit?: string; tree?: string; source_id?: string } = {}) =>
      root(over.commit ?? snap.commit, over.source_id ?? "s1:snapshot", "att", over.tree ?? snap.tree);
    const run = (roots: ReturnType<typeof root>[], snapshot?: typeof snap) =>
      reasonOf(checkManifestCoverage(universe, { roots, abandoned: [] }, { currentFlight: true, ...(snapshot ? { snapshot } : {}) }));
    assert.equal(run([...full, snapRoot()], snap), "ok");
    assert.equal(run(full, snap), "missing_root");
    assert.equal(run([...full, snapRoot({ tree: "other" })], snap), "tree_mismatch");
    assert.equal(run([...full, snapRoot({ commit: "e".repeat(40) })], snap), "commit_mismatch");
    assert.equal(run([...full, snapRoot()]), "extra_root");
    assert.equal(run([...full, snapRoot(), snapRoot({ commit: "e".repeat(40) })], snap), "extra_root");
    assert.equal(run([...full, snapRoot({ source_id: "s9:snapshot" })], snap), "extra_root");
  });
  it("refuses an unknown abandoned class and cold_dirty on a current flight", () => {
    const abandoned = (cls: string) => [{ class: cls as "ignored", source_id: "s1", extents: { files: 1, bytes: 1 } }];
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: full, abandoned: abandoned("mystery") }, { currentFlight: true })), "unknown_abandoned_class");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: full, abandoned: abandoned("cold_dirty") }, { currentFlight: true })), "cold_dirty_current_flight");
    assert.equal(reasonOf(checkManifestCoverage(universe, { roots: full, abandoned: abandoned("cold_dirty") }, { currentFlight: false })), "ok");
  });
});
