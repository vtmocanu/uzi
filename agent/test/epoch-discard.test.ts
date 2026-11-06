import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type EpochDiscardProof, type EpochDiscardProofOptions, type RetainedDiscardOptions } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

const RUN = "run-epoch-discard";
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
function setJournal(value: string): void { gitIn(["config", "--local", `uzi-recovery.${BRANCH}.clone`, value]); }

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
  assert.equal(inspection.sources[0]?.epochSettlement?.disposition.kind, "local_discard");
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

async function success(proof: EpochDiscardProof, over: Partial<RetainedDiscardOptions> = {}): Promise<void> {
  assert.equal((await git.discardStoppedSource({ ...options, epochProof: proof, ...over })).kind, "done");
  assert.equal(fs.existsSync(clonePath), false);
  assert.equal(journal(), "");
  git.assertEpochDiscardSettled(proof);
}

async function refusal(proof: EpochDiscardProof, reason: string, over: Partial<RetainedDiscardOptions> = {}): Promise<void> {
  assert.deepEqual(await git.discardStoppedSource({ ...options, epochProof: proof, ...over }),
    { kind: "refused", reason, consumers: [] });
  assert.ok(fs.existsSync(clonePath));
}


function cloneGit(...args: string[]): string {
  return execFileSync("git", ["-C", clonePath, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();
}
const IDENTITY = ["-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false"];
function bareOnlyCommit(): string {
  return gitIn([...IDENTITY, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-m", "unpublished"]);
}
async function contentRefusal(reason = "epoch_content_unproven"): Promise<void> {
  const ledger = gitIn(["config", "--get-all", `uzi-attempts.${BRANCH}.entry`]);
  const proof = git.createEpochDiscardProof(input);
  if (reason === "eligible") { await success(proof); return; }
  await refusal(proof, reason);
  git.assertEpochDiscardSettled(proof);
  assert.ok(nativeLaunches > 0, "validated production branch used the native read scope");
  assert.equal(journal(), raw);
  assert.equal(gitIn(["config", "--get-all", `uzi-attempts.${BRANCH}.entry`]), ledger);
}
function seedGitlink(): string {
  const head = cloneGit("rev-parse", "HEAD");
  cloneGit("update-index", "--add", "--cacheinfo", `160000,${head},sub`);
  cloneGit(...IDENTITY, "commit", "-m", "uninitialized submodule");
  gitIn(["fetch", clonePath, "HEAD:refs/heads/main"]);
  const published = cloneGit("rev-parse", "HEAD");
  if (gitIn(["for-each-ref", "--format=%(refname)", "refs/remotes/origin/main"]))
    gitIn(["update-ref", "refs/remotes/origin/main", published]);
  return path.join(clonePath, "sub");
}

async function legacyCoverage(): Promise<"covered" | "not_covered"> {
  return git.verifyStoppedSourceCovered(bare, BRANCH, "issue-3431",
    { runId: RUN, clonePath, attemptId: options.selector.attempt },
    cloneGit("rev-parse", "HEAD"), { runId: RUN, generation: 1 }, async () => true);
}

describe("legacy coverage keeps its directory and reachability semantics", () => {
  for (const target of ["worktrees", "gitlink"] as const) {
    for (const state of ["missing", "empty", "populated", "unreadable"] as const) {
      it(`preserves default ${target} ${state} behavior without automatic probes`, async t => {
        const dir = target === "gitlink" ? seedGitlink() : path.join(clonePath, ".git", "worktrees");
        if (state !== "missing") fs.mkdirSync(dir);
        if (state === "populated") fs.writeFileSync(path.join(dir, "retained"), "");
        const opendir = fsp.opendir.bind(fsp);
        t.mock.method(fsp, "opendir", async (name: string, ...args: Parameters<typeof fsp.opendir> extends [unknown, ...infer R] ? R : never) => {
          assert.notEqual(name, dir, "automatic directory probe must not run for coverage or inspection");
          return opendir(name, ...args);
        });
        const readdir = fsp.readdir.bind(fsp);
        t.mock.method(fsp, "readdir", async (...args: Parameters<typeof fsp.readdir>) => {
          if (state === "unreadable" && args[0] === dir) throw Object.assign(new Error("unreadable"), { code: "EACCES" });
          return readdir(...args);
        });
        const expected = state === "empty" || target === "worktrees" && state === "missing";
        assert.equal(await legacyCoverage(), expected ? "covered" : "not_covered");
      });
    }
  }
  it("does not select automatic content proof for read-only epoch inspection", async () => {
    fs.writeFileSync(path.join(clonePath, "private.txt"), "uncommitted work");
    const inspection = await git.inspectRetainedSources({
      selector: options.selector, barePath: bare, holds: options.holds, lifecycle: options.lifecycle,
      epoch: true, isLivePath: () => false,
    });
    assert.equal(inspection.partial, false);
    assert.equal(inspection.sources[0]?.epochSettlement?.disposition.kind, "local_discard");
    await contentRefusal();
  });
  it("continues accepting a bare-owned side ref for diagnostic coverage only", async () => {
    const sha = bareOnlyCommit();
    gitIn(["update-ref", "refs/unpublished/held", sha]);
    cloneGit("update-ref", "refs/private/side", sha);
    assert.equal(await legacyCoverage(), "covered");
    await contentRefusal();
  });
});

describe("automatic content preservation through registered authority", () => {
  it("accepts a clean published tree and published side refs through durable retirement", async () => {
    cloneGit("branch", "published-side", "HEAD");
    await contentRefusal("eligible");
  });
  it("keeps generated nonempty skills eligible for the existing footprint checks", async () => {
    const skills = path.join(path.dirname(clonePath), ".uzi-skills-" + path.basename(clonePath));
    fs.mkdirSync(skills);
    fs.writeFileSync(path.join(skills, "generated.md"), "generated skill");
    await contentRefusal("eligible");
    assert.equal(fs.existsSync(skills), false);
  });
  it("leaves an attributed skills sibling for the discard core when the clone is missing", async () => {
    const skills = path.join(path.dirname(clonePath), ".uzi-skills-" + path.basename(clonePath));
    fs.mkdirSync(skills);
    fs.writeFileSync(path.join(skills, "generated.md"), "generated skill");
    fs.rmSync(clonePath, { recursive: true });
    const inspection = await git.inspectRetainedSources({
      selector: options.selector, barePath: bare, holds: options.holds, lifecycle: options.lifecycle,
      epoch: true, isLivePath: () => false,
    });
    const confirm = inspection.sources[0]!.descriptor;
    const proof = git.createEpochDiscardProof({ ...input, confirm });
    await success(proof, { confirm });
    assert.equal(fs.existsSync(skills), false);
  });
  for (const shape of ["file", "symlink"] as const) {
    it(`refuses an unsafe skills sibling: ${shape}`, async () => {
      const skills = path.join(path.dirname(clonePath), ".uzi-skills-" + path.basename(clonePath));
      if (shape === "file") fs.writeFileSync(skills, "unsafe");
      else fs.symlinkSync(clonePath, skills);
      await contentRefusal();
    });
  }
  for (const dirty of ["tracked", "untracked", "assume-unchanged", "skip-worktree", "stash", "filter", "attributes", "modules"] as const) {
    it(`retains ${dirty} content without changing journal or ledger`, async () => {
      const tracked = cloneGit("ls-files").split("\n")[0]!;
      if (dirty === "tracked" || dirty === "stash" || dirty === "assume-unchanged" || dirty === "skip-worktree") {
        fs.appendFileSync(path.join(clonePath, tracked), "\nprivate work\n");
        if (dirty === "stash") cloneGit(...IDENTITY, "stash", "push");
        if (dirty === "assume-unchanged" || dirty === "skip-worktree") cloneGit("update-index", "--" + dirty, tracked);
      } else if (dirty === "untracked") fs.writeFileSync(path.join(clonePath, "private.txt"), "private work");
      else if (dirty === "filter") cloneGit("config", "filter.hostile.clean", "false");
      else if (dirty === "attributes") fs.writeFileSync(path.join(clonePath, ".git", "info", "attributes"), "");
      else fs.mkdirSync(path.join(clonePath, ".git", "modules"));
      await contentRefusal();
    });
  }
  // A clone config include can name a file holding a promisor remote that nothing classifies, so it is UNKNOWN (it
  // stops the epoch scope), never a keep.
  for (const section of ["[include]", '[includeIf "gitdir:/"]']) {
    it(`treats a clone config ${section} naming a promisor remote as UNKNOWN`, async () => {
      const target = path.join(path.dirname(clonePath), "included-promisor-config");
      fs.writeFileSync(target, '[remote "sib"]\n\turl = /tmp/sibling\n\tpromisor = true\n');
      fs.appendFileSync(path.join(clonePath, ".git", "config"), `${section}\n\tpath = ${target}\n`);
      const proof = git.createEpochDiscardProof(input);
      const result = await git.discardStoppedSource({ ...options, epochProof: proof });
      assert.equal(result.kind, "unknown");
      assert.ok(fs.existsSync(clonePath));
      assert.equal(journal(), raw);
    });
  }
  for (const extra of ["head", "side", "arbitrary-remote", "blob"] as const) {
    it(`rejects bare-owned but unpublished ${extra} objects`, async () => {
      const sha = extra === "blob"
        ? execFileSync("git", ["-C", bare, "hash-object", "-w", "--stdin"], { env: ENV, input: "unpublished blob", encoding: "utf8" }).trim()
        : bareOnlyCommit();
      gitIn(["update-ref", "refs/unpublished/held", sha]);
      if (extra === "head") cloneGit("checkout", "--detach", sha);
      else cloneGit("update-ref", extra === "arbitrary-remote" ? "refs/remotes/other/main" : "refs/private/side", sha);
      await contentRefusal();
    });
  }

  for (const state of ["absent", "empty", "populated", "symlink", "file"] as const) {
    it(`checks automatic linked worktrees: ${state}`, async () => {
      const dir = path.join(clonePath, ".git", "worktrees");
      if (state === "empty" || state === "populated") fs.mkdirSync(dir);
      if (state === "populated") for (let i = 0; i < 2000; i++) fs.writeFileSync(path.join(dir, String(i)), "");
      if (state === "symlink") fs.symlinkSync(path.join(clonePath, ".git", "objects"), dir);
      if (state === "file") fs.writeFileSync(dir, "");
      // The clone shape stage admits no `.git/worktrees` entry by name, so even an empty directory is a keep.
      await contentRefusal(state === "absent" ? "eligible" : "epoch_content_unproven");
    });
  }
  for (const state of ["missing", "empty", "populated", "symlink", "file"] as const) {
    it(`checks automatic gitlinks: ${state}`, async () => {
      const dir = seedGitlink();
      if (state === "empty" || state === "populated") fs.mkdirSync(dir);
      if (state === "populated") fs.writeFileSync(path.join(dir, "private"), "submodule content");
      if (state === "symlink") fs.symlinkSync(path.join(clonePath, ".git"), dir);
      if (state === "file") fs.writeFileSync(dir, "private");
      // An absent tracked gitlink is a deletion in Git status; the directory probe itself skips it.
      await contentRefusal(state === "empty" ? "eligible" : "epoch_content_unproven");
    });
  }

  for (const target of ["worktrees", "gitlink"] as const) {
    for (const fault of ["many", "open", "read", "close", "vanish"] as const) {
      it(`bounds ${target} reads and joins ${fault} refusal`, async t => {
        const dir = target === "gitlink" ? seedGitlink() : path.join(clonePath, ".git", "worktrees");
        fs.mkdirSync(dir);
        if (fault === "many") for (let i = 0; i < 2000; i++) fs.writeFileSync(path.join(dir, String(i)), "");
        const original = fsp.opendir.bind(fsp);
        let reads = 0, closes = 0;
        t.mock.method(fsp, "opendir", async (name: string, opts: { bufferSize: number }) => {
          if (name !== dir) return original(name, opts);
          assert.equal(opts.bufferSize, 1);
          if (fault === "open" || fault === "vanish") throw Object.assign(new Error("open failed"), { code: fault === "vanish" ? "ENOENT" : "EACCES" });
          const handle = await original(name, opts);
          return {
            async read() { reads++; if (fault === "read") throw new Error("read failed"); return handle.read(); },
            async close() { closes++; await handle.close(); if (fault === "close") throw new Error("close failed"); },
          };
        });
        await contentRefusal();
        // `.git/worktrees` is refused by name in the Node-only shape stage, so it is never opened.
        const unread = target === "worktrees" || fault === "open" || fault === "vanish";
        assert.equal(reads, unread ? 0 : 1);
        assert.equal(closes, unread ? 0 : 1);
      });
    }
  }

  it("revokes authority during a joined directory read", async t => {
    const dir = seedGitlink();
    fs.mkdirSync(dir);
    const original = fsp.opendir.bind(fsp);
    let closed = false;
    t.mock.method(fsp, "opendir", async (name: string, opts: { bufferSize: number }) => {
      const handle = await original(name, opts);
      if (name !== dir) return handle;
      return {
        async read() { live = false; return handle.read(); },
        async close() { await handle.close(); closed = true; },
      };
    });
    await refusal(git.createEpochDiscardProof(input), "stale_epoch_proof");
    assert.equal(closed, true);
  });
});

describe("worker epoch discard registration", () => {
  it("keeps raw bytes outside the frozen opaque proof and settles effects after fresh real-Git checks", async () => {
    const proof = git.createEpochDiscardProof(input);
    assert.equal(Object.isFrozen(proof), true);
    assert.equal(JSON.stringify(proof), "{}");
    assert.deepEqual(Reflect.ownKeys(proof), []);
    git.assertEpochDiscardSettled(proof);
    await success(proof);
  });

  it("rejects JSON, copies and capabilities from a different cache", async () => {
    const proof = git.createEpochDiscardProof(input);
    for (const forged of [JSON.parse(JSON.stringify(proof)), { ...proof }]) {
      await refusal(forged, "invalid_epoch_proof");
      assert.throws(() => git.assertEpochDiscardSettled(forged), /unregistered/);
    }
    const other = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const foreign = other.createEpochDiscardProof(input);
    await refusal(foreign, "invalid_epoch_proof");
  });

  it("copies selector and paths instead of retaining caller-owned containers", async () => {
    const proof = git.createEpochDiscardProof(input);
    const original = { ...options.selector };
    (input.selector as { runId: string }).runId = "changed";
    (input.paths as string[])[0] = clonePath + "-changed";
    await refusal(proof, "invalid_epoch_proof");
    await success(proof, { selector: original });
  });

  it("rejects mismatched selector, digest, bare and protected disposal", async () => {
    const proof = git.createEpochDiscardProof(input);
    await refusal(proof, "invalid_epoch_proof", { confirm: "0".repeat(64) });
    await refusal(proof, "invalid_epoch_proof", { barePath: bare + "-other" });
    await refusal(proof, "invalid_epoch_proof", { selector: { ...options.selector, generation: 2 } });
    assert.deepEqual(await git.discardProtectedSource({ ...options, epochProof: proof }),
      { kind: "refused", reason: "invalid_epoch_proof", consumers: [] });
  });

  it("refuses a canonical selector even with a valid two-key plain journal", () => {
    const canonical = { runId: RUN, generation: "none" as const, attempt: "canonical" };
    const journal = JSON.stringify({ clonePath, runId: RUN });
    assert.throws(() => git.createEpochDiscardProof({ ...input, selector: canonical, expectedRawJournal: journal }), /invalid epoch discard proof/);
    assert.throws(() => git.createEpochDiscardProof({ ...input, selector: canonical, expectedRawJournal: [""] }), /invalid epoch discard proof/);
  });

  it("refuses an absent journal ([]) for an attempt selector and admits only the explicit [\"\"] repair", () => {
    assert.throws(() => git.createEpochDiscardProof({ ...input, expectedRawJournal: [] }), /invalid epoch discard proof/);
    assert.throws(() => git.createEpochDiscardProof({ ...input, expectedRawJournal: ["", ""] }), /invalid epoch discard proof/);
    assert.throws(() => git.createEpochDiscardProof({ ...input, expectedRawJournal: ["x"] }), /invalid epoch discard proof/);
    assert.doesNotThrow(() => git.createEpochDiscardProof({ ...input, expectedRawJournal: [""] }));
    assert.doesNotThrow(() => git.createEpochDiscardProof(input));
  });

  it("rejects malformed decisions, scope contracts and noncanonical or extra-field journals", () => {
    const bad = [
      { decision: { eligible: false, attested: true } },
      { decision: { eligible: true, attested: false } },
      { scope: {} },
      { expectedRawJournal: raw + " " },
      { expectedRawJournal: JSON.stringify({ ...JSON.parse(raw), restoreTip: "a".repeat(40) }) },
      { expectedRawJournal: JSON.stringify({ ...JSON.parse(raw), runId: "other" }) },
      { expectedRawJournal: raw.replace('"runId":', '"runId":"duplicate","runId":') },
      { paths: [clonePath, clonePath] },
      { deadline: NaN },
    ];
    for (const over of bad) {
      assert.throws(() => git.createEpochDiscardProof({ ...input, ...over } as EpochDiscardProofOptions), /invalid/);
    }
  });

  it("compares the exact raw journal under B even when the attempt descriptor did not change", async () => {
    const proof = git.createEpochDiscardProof(input);
    setJournal(raw + " ");
    await refusal(proof, "epoch_journal_changed");
    assert.equal(journal(), raw); // The raw NUL read below must detect what trimming would hide.
    const stored = execFileSync("git", ["-C", bare, "config", "--null", "--get", `uzi-recovery.${BRANCH}.clone`],
      { env: ENV, encoding: "utf8" });
    assert.equal(stored, raw + " \0");
  });

  it("requires current terminal lifecycle and known zero holds for every proof use", async () => {
    const proof = git.createEpochDiscardProof(input);
    await refusal(proof, "epoch_authority_refused", { lifecycle: { state: "unknown" } });
    await refusal(proof, "epoch_authority_refused", { holds: { state: "unknown" } });
    await refusal(proof, "epoch_authority_refused", { holds: { state: "read", holds: [{ id: "h", generation: 1 }] } });
  });

  it("keeps live, stopped, pending-ticket, abort and deadline checks in settlement", async () => {
    const proof = git.createEpochDiscardProof(input);
    const ticket = scope.beginMutation();
    assert.throws(() => git.assertEpochDiscardSettled(proof), /unsettled/);
    ticket.settle();
    git.assertEpochDiscardSettled(proof);
    live = false;
    await refusal(proof, "stale_epoch_proof");
    assert.throws(() => git.assertEpochDiscardSettled(proof), /reservation/);
    live = true;
    scope.stop();
    await refusal(proof, "stale_epoch_proof");
    assert.throws(() => git.assertEpochDiscardSettled(proof), /stopped/);
    const controller = new AbortController();
    controller.abort();
    assert.throws(() => git.createEpochDiscardProof({ ...input, signal: controller.signal }));
    assert.throws(() => git.createEpochDiscardProof({ ...input, deadline: Date.now() - 1 }), /invalid/);
  });

  it("revokes an already registered proof after abort", async () => {
    const controller = new AbortController();
    const proof = git.createEpochDiscardProof({ ...input, signal: controller.signal });
    git.assertEpochDiscardSettled(proof);
    controller.abort(new Error("registered proof cancelled"));
    await refusal(proof, "stale_epoch_proof");
    assert.throws(() => git.assertEpochDiscardSettled(proof), /registered proof cancelled/);
    assert.equal(journal(), raw);
  });

  it("revokes an already registered proof when its deadline expires", async t => {
    const proof = git.createEpochDiscardProof(input);
    git.assertEpochDiscardSettled(proof);
    const clock = t.mock.method(Date, "now", () => input.deadline);
    try {
      await refusal(proof, "stale_epoch_proof");
      assert.throws(() => git.assertEpochDiscardSettled(proof), /deadline exhausted/);
    } finally { clock.mock.restore(); }
    assert.equal(journal(), raw);
  });

  it("rechecks fresh consumers rather than trusting the registration decision", async () => {
    const proof = git.createEpochDiscardProof(input);
    const tip = gitIn(["rev-parse", "HEAD"]);
    gitIn(["update-ref", `refs/uzi-owed/${RUN}/${tip}`, tip]);
    const result = await git.discardStoppedSource({ ...options, epochProof: proof });
    assert.equal(result.kind, "refused");
    if (result.kind === "refused") assert.equal(result.reason, "epoch_source_refused");
    git.assertEpochDiscardSettled(proof);
    assert.equal(journal(), raw);
    assert.ok(fs.existsSync(clonePath));
  });

  it("detects a present source disappearing after registration through the inspect digest", async () => {
    const proof = git.createEpochDiscardProof(input);
    fs.rmSync(clonePath, { recursive: true });
    const result = await git.discardStoppedSource({ ...options, epochProof: proof });
    assert.deepEqual(result, { kind: "refused", reason: "descriptor_changed", consumers: [] });
    assert.equal(journal(), raw);
  });

  it("can bind strict-live metadata with an already absent tree and performs durable retirement", async () => {
    fs.rmSync(clonePath, { recursive: true });
    const inspection = await git.inspectRetainedSources({
      selector: options.selector, barePath: bare, holds: options.holds, lifecycle: options.lifecycle,
      epoch: true, isLivePath: () => false,
    });
    const source = inspection.sources[0]!;
    assert.equal(source.presence, "missing");
    assert.equal(source.epochSettlement?.disposition.kind, "local_discard");
    const proof = git.createEpochDiscardProof({ ...input, confirm: source.descriptor });
    await success(proof, { confirm: source.descriptor });
    const entries = gitIn(["config", "--get-all", `uzi-attempts.${BRANCH}.entry`]).split("\n");
    assert.equal(JSON.parse(entries.at(-1)!).state, "retired");
  });

  it("preserves ordinary operator refusal of a live attempt named by its own journal", async () => {
    const result = await git.discardStoppedSource(options);
    assert.equal(result.kind, "refused");
    if (result.kind === "refused") assert.ok(result.consumers.includes("ledger_live"));
    assert.equal(journal(), raw);
    assert.ok(fs.existsSync(clonePath));
  });
});

for (const cancellation of ["abort", "deadline"] as const) it(`epoch ${cancellation} interrupts a queued bare lock before its holder releases`, async t => {
  const locks = git as unknown as { withLock<T>(key: string, action: () => Promise<T>): Promise<T> };
  let enter!: () => void;
  let unblock!: () => void;
  const entered = new Promise<void>(resolve => { enter = resolve; });
  const release = new Promise<void>(resolve => { unblock = resolve; });
  let released = false;
  const holder = locks.withLock(bare, async () => { enter(); await release; released = true; });
  await entered;
  const controller = new AbortController();
  const proof = git.createEpochDiscardProof({ ...input, signal: controller.signal,
    deadline: cancellation === "deadline" ? Date.now() + 100 : input.deadline });
  let queued = false;
  const original = locks.withLock.bind(git);
  t.mock.method(locks, "withLock", <T>(key: string, action: () => Promise<T>) => {
    const result = original(key, action);
    queued = true;
    if (cancellation === "abort") controller.abort(new Error("queued epoch cancelled"));
    return result;
  });
  const discard = git.discardStoppedSource({ ...options, epochProof: proof });
  const watchdog = AbortSignal.timeout(2000);
  try {
    const result = await Promise.race([discard, new Promise<never>((_, reject) => {
      watchdog.addEventListener("abort", () => reject(new Error("queued epoch did not return promptly")), { once: true });
    })]);
    assert.equal(queued, true); assert.equal(released, false);
    assert.equal(result.kind, "unknown"); assert.equal(stopped, true);
    assert.equal(journal(), raw); assert.ok(fs.existsSync(clonePath));
  } finally { unblock(); await holder; await discard; }
});
