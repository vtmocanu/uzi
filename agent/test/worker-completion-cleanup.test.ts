import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";
import { GitCache } from "../src/git.js";
import { RunRunner, type RunnerOptions } from "../src/runner.js";
import type { RecoveryCoordinator, CompletionSource } from "../src/recovery.js";
import type { WorkerClient } from "../src/client.js";
import type { CompletedPublicationReceipt } from "../src/protocol.js";
import { latchResidueQuarantine } from "../src/residue-quarantine.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

resetResidueQuarantineAfterEach();
const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));
const wire = JSON.parse(await fs.readFile(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const literal: CompletedPublicationReceipt = wire.ack.completed_publication_receipt;

// All Git children are local, bounded to 10 seconds, and use no forge credentials.
async function fixture(shared = false) {
  const root = await fs.mkdtemp(path.join(scratch, "receipt-cleanup-"));
  const cmd = (cwd: string, args: string[]) => execFileSync("git", ["-C", cwd, ...args], {
    encoding: "utf8", stdio: ["ignore", "pipe", "pipe"], timeout: 10000,
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
  const seed = path.join(root, "seed");
  await fs.mkdir(seed);
  cmd(seed, ["init", "-b", "main"]);
  cmd(seed, ["config", "user.name", "fixture"]);
  cmd(seed, ["config", "user.email", "fixture@example.com"]);
  cmd(seed, ["config", "commit.gpgsign", "false"]);
  await fs.writeFile(path.join(seed, "work.txt"), "checkpoint\n");
  cmd(seed, ["add", "work.txt"]);
  cmd(seed, ["commit", "-m", "checkpoint"]);
  const git = new GitCache(path.join(root, "data"), nullLogger(), undefined, testGitCacheOptions());
  const bare = await git.ensureClone(seed);
  await fs.chmod(path.dirname(bare), 0o700);
  await fs.chmod(bare, 0o700);
  const base = cmd(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const runId = literal.run_id, generation = literal.generation, branch = literal.branch;
  const clone = (await git.runnerCloneForBranch(bare, branch, "issue-7", noProofReseed, runId)).path;
  cmd(clone, ["config", "user.name", "fixture"]);
  cmd(clone, ["config", "user.email", "fixture@example.com"]);
  cmd(clone, ["config", "commit.gpgsign", "false"]);
  await fs.writeFile(path.join(clone, "work.txt"), "post-checkpoint\n");
  cmd(clone, ["add", "work.txt"]);
  cmd(clone, ["commit", "-m", "after checkpoint"]);
  const head = cmd(clone, ["rev-parse", "HEAD"]);
  cmd(clone, ["push", bare, head + ":refs/heads/fixture-source"]);
  await git.markRecoveryCapture(bare, clone, branch, runId);
  const context: CompletionSource["context"] = { barePath: bare, runId, generation, kind: "issue", branch,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } };
  assert.equal((await git.updateTrackingRef(bare, branch, head, { context })).kind, "updated");
  const roots = (await git.enumerateOwedCandidates(bare, runId)).map(c => ({ sha: c.sha,
    contexts: c.contexts.filter(x => x.generation === generation) })).filter(c => c.contexts.length > 0);
  assert.equal(await git.cleanupRecoveryGeneration(bare, context, roots, [], [head], head, () => true, "completion-check"), "verified",
    "fixture must have verified generation attribution before installing cleanup authority");
  let rpc = 0;
  const client = {
    hasFeature: () => true,
    listRecoveryHolds: async () => ({ run_id: runId, holds: [{
      hold_id: literal.hold_id, generation, inventory_guarded: true, has_available_capture: false,
    }, ...(shared ? [{ hold_id: "sibling-hold", generation: generation + 1, inventory_guarded: true, has_available_capture: false }] : [])] }),
  } as unknown as WorkerClient;
  const make = (options: RunnerOptions = {}) => {
    const runner = new RunRunner(client, git, () => { throw new Error("cleanup must not execute a model"); },
      nullLogger(), 1, "receipt-cleanup-worker", options);
    const internals = runner as unknown as { recovery: RecoveryCoordinator; executionTails: Map<string, Promise<void>> };
    return { runner, ...internals };
  };
  const initial = make();
  const record = await initial.recovery.pin({ runId, generation, kind: "issue", branch, sourceSha: head, inventoryGuarded: true });
  assert.ok(record);
  assert.equal(await initial.recovery.bindCompletionSource({ context, clonePath: clone, expectedHead: head, roots }), true);
  assert.equal(await initial.recovery.persistCompletionReceipt({ ...literal, final_head: head, observed_branch_head: head }), true);
  let siblingJournal: string | undefined;
  let siblingBundle: string | undefined;
  if (shared) {
    assert.equal((await git.updateTrackingRef(bare, branch, head, {
      context: { ...context, generation: generation + 1 },
    })).kind, "updated");
    const sibling = await initial.recovery.pin({ runId, generation: generation + 1, kind: "issue", branch,
      sourceSha: head, inventoryGuarded: true });
    assert.ok(sibling);
    siblingJournal = path.join(git.recoveryRoot, runId, sibling.captureId + ".json");
    siblingBundle = path.join(git.recoveryRoot, runId, sibling.captureId + ".bundle");
    await fs.writeFile(siblingBundle, "successor bundle");
  }
  // Deliberately dirty tracked and untracked leftovers are allowed only by receipt cleanup.
  await fs.writeFile(path.join(clone, "work.txt"), "dirty remainder\n");
  await fs.writeFile(path.join(clone, "untracked.txt"), "untracked remainder\n");
  assert.ok(cmd(clone, ["status", "--porcelain"]).includes("?? untracked.txt"));
  client.listRecoveryHolds = async () => { rpc++; throw new Error("receipt cleanup must be local"); };
  const restarted = make();
  const snapshot = (await restarted.recovery.inspect(runId)).filter(r => r.generation === generation);
  const sweep = () => restarted.recovery.resumePending(undefined, snapshot);
  const journal = path.join(git.recoveryRoot, runId, record.captureId + ".json");
  const bundle = path.join(git.recoveryRoot, runId, record.captureId + ".bundle");
  await fs.writeFile(bundle, "attributed generation bundle");
  const retained = async () => {
    assert.equal(cmd(clone, ["rev-parse", "HEAD"]), head, "retained completing clone keeps its exact committed head");
    assert.equal(await fs.readFile(path.join(clone, "untracked.txt"), "utf8"), "untracked remainder\n");
    assert.ok(await fs.stat(journal), "retention keeps authenticated receipt authority");
    assert.ok(await fs.stat(bundle), "retention keeps completing-generation bundle");
    assert.equal(rpc, 0, "persisted receipt cleanup never calls custody RPCs");
  };
  return { root, git, bare, clone, context, head, cmd, runId, generation, branch, journal, bundle,
    restarted, make, snapshot, sweep, retained, siblingJournal, siblingBundle, rpc: () => rpc,
    close: () => fs.rm(root, { recursive: true, force: true }) };
}

it("persisted receipt removes actual dirty own clone, generation bundle and journal after quiescence", async () => {
  const f = await fixture();
  try {
    assert.equal(f.snapshot[0]?.finalAcknowledged, undefined);
    assert.equal(f.snapshot[0]?.coverageDigest, undefined);
    await f.sweep();
    for (const target of [f.clone, f.journal, f.bundle]) {
      await assert.rejects(fs.stat(target), { code: "ENOENT" }, "receipt retires its attributed physical source");
    }
    assert.deepEqual(await f.restarted.recovery.inspect(f.runId), []);
    assert.equal(f.rpc(), 0);
  } finally { await f.close(); }
});

it("active execution retains receipt source then permits cleanup when the execution tail quiesces", async () => {
  const f = await fixture();
  try {
    f.restarted.executionTails.set(f.runId, Promise.resolve());
    await f.sweep();
    await f.retained();
    f.restarted.executionTails.delete(f.runId);
    await f.sweep();
    await assert.rejects(fs.stat(f.clone), { code: "ENOENT" });
    await assert.rejects(fs.stat(f.journal), { code: "ENOENT" });
  } finally { await f.close(); }
});

it("nonquiescent process proof retains receipt source until both process checks pass", async () => {
  const f = await fixture();
  try {
    let quiescent = false;
    const sites: string[] = [];
    const blocked = f.make({ quiesceRun: async request => {
      sites.push(request.site!);
      return { process: { state: quiescent ? "quiescent" : "unverified",
        processes: [], killed: [], detail: "fixture process remains active" },
        docker: { state: "not_wired", removed: [], detail: "" } };
    } });
    await blocked.recovery.resumePending(undefined, f.snapshot);
    await f.retained();
    assert.deepEqual(sites, ["completion_source"]);
    quiescent = true;
    await blocked.recovery.resumePending(undefined, f.snapshot);
    assert.deepEqual(sites.slice(1), ["completion_source", "completion_source:after_git"]);
    await assert.rejects(fs.stat(f.clone), { code: "ENOENT" });
    await assert.rejects(fs.stat(f.journal), { code: "ENOENT" });
  } finally { await f.close(); }
});

for (const owner of ["foreign", "successor"] as const) {
  it(owner + " clone ownership excludes completing receipt cleanup", async () => {
    const f = await fixture();
    try {
      await f.git.markRecoveryCapture(f.bare, f.clone, f.branch,
        owner === "foreign" ? "22222222-2222-4222-8222-222222222222" : f.runId,
        owner === "successor" ? "20261009T000000Z-g2-1234567890abcdef" : undefined);
      await f.sweep();
      await f.retained();
    } finally { await f.close(); }
  });
}

it("malformed persisted completion source retains physical state instead of stripping attribution", async () => {
  const f = await fixture();
  try {
    const raw = JSON.parse(await fs.readFile(f.journal, "utf8"));
    raw.completionSource.expectedHead = "invalid";
    await fs.writeFile(f.journal, JSON.stringify(raw));
    await f.sweep();
    assert.equal(await f.restarted.recovery.hasPersistedCompletionReceipt(f.runId, f.generation), false,
      "malformed source cannot leave a trusted receipt-only record");
    assert.equal(f.cmd(f.clone, ["rev-parse", "HEAD"]), f.head);
    assert.equal(await fs.readFile(path.join(f.clone, "untracked.txt"), "utf8"), "untracked remainder\n");
    assert.ok(await fs.stat(f.bundle));
    assert.equal(JSON.parse(await fs.readFile(f.journal, "utf8")).completionSource.expectedHead, "invalid");
    assert.equal(f.rpc(), 0);
  } finally { await f.close(); }
});

it("malformed physical sibling record blocks receipt destruction", async () => {
  const f = await fixture();
  try {
    const malformed = path.join(path.dirname(f.journal), "unknown.json");
    await fs.writeFile(malformed, '{"completionSource":{"clonePath":"unknown"}}');
    await f.sweep();
    await f.retained();
    assert.equal(await fs.readFile(malformed, "utf8"), '{"completionSource":{"clonePath":"unknown"}}');
  } finally { await f.close(); }
});

it("receipt cleanup preserves shared owed pin and sibling generation bytes", async () => {
  const f = await fixture(true);
  try {
    const journalBytes = await fs.readFile(f.siblingJournal!, "utf8");
    const before = await f.git.enumerateOwedCandidates(f.bare, f.runId);
    assert.ok(before.some(c => c.sha === f.head && c.contexts.some(x => x.generation === f.generation)));
    assert.ok(before.some(c => c.sha === f.head && c.contexts.some(x => x.generation === f.generation + 1)));
    await f.sweep();
    await assert.rejects(fs.stat(f.clone), { code: "ENOENT" });
    await assert.rejects(fs.stat(f.journal), { code: "ENOENT" });
    assert.equal(await fs.readFile(f.siblingJournal!, "utf8"), journalBytes, "sibling authority is byte-for-byte preserved");
    assert.equal(await fs.readFile(f.siblingBundle!, "utf8"), "successor bundle");
    const after = await f.git.enumerateOwedCandidates(f.bare, f.runId);
    assert.ok(after.some(c => c.sha === f.head && c.contexts.some(x => x.generation === f.generation + 1)),
      "shared physical owed pin remains discoverable for successor");
    assert.equal(after.some(c => c.contexts.some(x => x.generation === f.generation)), false,
      "only completing generation candidate attribution retires");
    assert.equal(f.rpc(), 0);
  } finally { await f.close(); }
});

it("paused physical disposal excludes actual successor clone admission until the bare lock releases", async t => {
  const f = await fixture();
  let resume!: () => void;
  let entered!: () => void;
  const paused = new Promise<void>(resolve => { entered = resolve; });
  const barrier = new Promise<void>(resolve => { resume = resolve; });
  const remove = fs.rm.bind(fs);
  t.mock.method(fs, "rm", async (...args: Parameters<typeof fs.rm>) => {
    if (path.dirname(String(args[0])) === (f.git as unknown as { runnerHoldingRoot: string }).runnerHoldingRoot) {
      entered();
      await barrier;
    }
    return remove(...args);
  });
  let successor: Promise<string> | undefined;
  let successorStarting = false, successorLockEntered = false;
  const lockGit = f.git as unknown as {
    withLock<T>(key: string, action: () => Promise<T>): Promise<T>;
  };
  const withLock = lockGit.withLock.bind(lockGit);
  t.mock.method(lockGit, "withLock", <T>(key: string, action: () => Promise<T>) => withLock(key, async () => {
    if (successorStarting) successorLockEntered = true;
    return action();
  }));
  const cleanup = f.sweep();
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    await Promise.race([paused, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("cleanup never reached physical disposal")), 5000);
    })]);
    clearTimeout(timer);
    assert.equal(f.restarted.runner.isExecuting(f.runId), true, "execution reservation spans paused disposal");
    let admitted = false;
    successorStarting = true;
    successor = (async () => {
      const clone = await f.git.runnerCloneForBranch(f.bare, f.branch, "issue-7", noProofReseed,
        "22222222-2222-4222-8222-222222222222");
      admitted = true;
      await fs.writeFile(path.join(clone.path, "successor.txt"), "successor owns this source");
      await f.git.markRecoveryCapture(f.bare, clone.path, f.branch, "22222222-2222-4222-8222-222222222222");
      return clone.path;
    })();
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(successorLockEntered, false, "successor cannot enter the bare critical section during disposal");
    assert.equal(admitted, false, "successor cannot seed while completion disposal holds adoption exclusion");
    successorStarting = false;
    resume();
    await cleanup;
    const next = await successor;
    assert.equal(next, f.clone);
    assert.equal(await fs.readFile(path.join(next, "successor.txt"), "utf8"), "successor owns this source",
      "completion cleanup preserves admitted successor source");
    await assert.rejects(fs.stat(f.journal), { code: "ENOENT" });
    assert.equal(f.rpc(), 0);
  } finally {
    clearTimeout(timer);
    resume();
    await Promise.allSettled([cleanup, ...(successor ? [successor] : [])]);
    t.mock.restoreAll();
    await f.close();
  }
});

it("worker quarantine retains receipt authority and dirty clone", async () => {
  const f = await fixture();
  try {
    latchResidueQuarantine({ cause: "fixture quarantine", runId: f.runId, site: "completion_cleanup" }, nullLogger());
    await f.sweep();
    await f.retained();
  } finally { await f.close(); }
});
