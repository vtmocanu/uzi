import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync, spawn } from "node:child_process";
import { once } from "node:events";
import { procfsTable, quiesceRunAttempt, type ProcTable } from "../src/run-quiescence.js";
import { GitCache } from "../src/git.js";
import { Outbox } from "../src/outbox.js";
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
async function fixture(shared = false, attempted = false, receipted = true, foreignSibling = false) {
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
  const attemptId = attempted ? `20261009T000000Z-g${generation}-1234567890abcdef` : undefined;
  const clone = (await git.runnerCloneForBranch(bare, branch, "issue-7", noProofReseed, runId, false, undefined,
    attemptId ? { attemptId, isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true } : undefined)).path;
  cmd(clone, ["config", "user.name", "fixture"]);
  cmd(clone, ["config", "user.email", "fixture@example.com"]);
  cmd(clone, ["config", "commit.gpgsign", "false"]);
  await fs.writeFile(path.join(clone, "work.txt"), "post-checkpoint\n");
  cmd(clone, ["add", "work.txt"]);
  cmd(clone, ["commit", "-m", "after checkpoint"]);
  const head = cmd(clone, ["rev-parse", "HEAD"]);
  cmd(clone, ["push", bare, head + ":refs/heads/fixture-source"]);
  await git.markRecoveryCapture(bare, clone, branch, runId, attemptId);
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
  assert.equal(await initial.recovery.bindCompletionSource({ context, clonePath: clone, expectedHead: head, roots,
    ...(attemptId ? { attemptId } : {}) }), true);
  if (receipted) assert.equal(await initial.recovery.persistCompletionReceipt({ ...literal, final_head: head, observed_branch_head: head }), true);
  if (foreignSibling) {
    assert.ok(await initial.recovery.freezeInventory({
      context: { ...context, branch: "agent/foreign" }, currentSha: head,
      originalSourceSha: head, defaultBranch: "main", locallyQuiescent: true,
    }));
  }
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

for (const conflict of ["foreign-clone", "different-attempt", "sibling-branch", "missing-MAC", "frozen-receipt"] as const) {
  it(conflict + " refuses completion source rebinding and retains physical state", async () => {
    const f = await fixture(false, true, conflict === "frozen-receipt", conflict === "sibling-branch");
    try {
      const source = f.snapshot[0]?.completionSource;
      assert.ok(source);
      let proposed = { ...source, expectedHead: f.cmd(f.bare, ["rev-parse", "refs/remotes/origin/main"]) };
      if (conflict === "foreign-clone") proposed = { ...proposed, clonePath: path.join(f.root, "foreign-clone") };
      if (conflict === "different-attempt") proposed = { ...proposed,
        attemptId: `20261009T000001Z-g${f.generation}-fedcba0987654321` };
      if (conflict === "missing-MAC") {
        await fs.writeFile(path.join(path.dirname(f.journal), "unknown.json"),
          JSON.stringify({ ...f.snapshot[0], captureId: "unknown" }));
      }
      const journals = (await fs.readdir(path.dirname(f.journal))).filter(name => name.endsWith(".json"));
      const before = await Promise.all(journals.map(name => fs.readFile(path.join(path.dirname(f.journal), name), "utf8")));
      assert.equal(await f.restarted.recovery.bindCompletionSource(proposed), false);
      const after = await Promise.all(journals.map(name => fs.readFile(path.join(path.dirname(f.journal), name), "utf8")));
      assert.deepEqual(after, before, "refusal must preserve every record's exact authority");
      await f.retained();
    } finally { await f.close(); }
  });
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

it("actual active child retains receipt source until exit permits real physical cleanup", async () => {
  const f = await fixture();
  const child = spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], {
    cwd: f.clone, env: {}, stdio: "ignore",
  });
  const exited = once(child, "exit");
  try {
    await once(child, "spawn");
    assert.ok(child.pid);
    const ownedPid = child.pid;
    const ownedRead = <T,>(reader: (pid: number) => T) => (pid: number): T => {
      if (pid !== ownedPid) throw Object.assign(new Error("outside owned child fixture"), { code: "ENOENT" });
      return reader(pid);
    };
    // The explicit table overrides the suite's empty hermetic view. No host enumeration occurs;
    // even parent-stat probes cannot read outside the child created by this test.
    const table: ProcTable = {
      listPids: () => child.exitCode === null && child.signalCode === null ? [ownedPid] : [],
      readStatus: ownedRead(procfsTable.readStatus),
      readEnviron: ownedRead(procfsTable.readEnviron),
      readCwd: ownedRead(procfsTable.readCwd),
      readStat: ownedRead(procfsTable.readStat),
    };
    const outcomes: string[] = [];
    const checked = f.make({ quiesceRun: async request => {
      assert.equal(request.mode, "capture");
      const result = await quiesceRunAttempt(request, {
        targetUid: process.getuid!(),
        // Model the separate helper scanner identity: the test parent
        // must not exempt its own deliberately unmarked fixture child.
        reap: { viaHelper: false, table, selfPid: 0, deadlineMs: 100, intervalMs: 10 },
      });
      outcomes.push(result.process!.state);
      if (child.exitCode === null && child.signalCode === null) {
        assert.equal(result.process!.state, "survivors");
        assert.deepEqual(result.process!.processes.map(p => [p.pid, p.reason]),
          [[ownedPid, "unattributed_in_scope"]]);
        assert.deepEqual(result.process!.killed, []);
      }
      return result;
    } });
    await checked.recovery.resumePending(undefined, f.snapshot);
    await f.retained();
    assert.deepEqual(outcomes, ["survivors"]);
    assert.equal(child.exitCode, null, "capture scanner retains the live unmarked child");
    assert.equal(child.signalCode, null);
    assert.equal(child.kill("SIGTERM"), true);
    await exited;
    await checked.recovery.resumePending(undefined, f.snapshot);
    assert.deepEqual(outcomes.slice(1), ["quiescent", "quiescent"]);
    for (const target of [f.clone, f.journal, f.bundle]) {
      await assert.rejects(fs.stat(target), { code: "ENOENT" }, "exit permits actual attributed source retirement");
    }
    assert.equal(f.rpc(), 0);
  } finally {
    if (child.exitCode === null && child.signalCode === null) child.kill("SIGTERM");
    await exited;
    await f.close();
  }
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

it("completed generation finalize must retire after receipt cleanup", async () => {
  const f = await fixture();
  try {
    const outbox = new Outbox({ root: path.join(f.root, "outbox"), log: nullLogger(),
      runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86400000 });
    await outbox.init();
    assert.equal((await outbox.journalFinalize(f.runId, f.generation)).written, true);
    const current = f.make({ outbox });
    await current.runner.observeSettlementTerminalAck(f.runId, f.generation,
      { ...wire.request, completion_final_head: f.head },
      { applied: true, completedPublicationReceipt: { ...literal, final_head: f.head, observed_branch_head: f.head } });
    await current.recovery.resumePending(undefined, f.snapshot);
    await assert.rejects(fs.stat(f.clone), { code: "ENOENT" });
    await assert.rejects(fs.stat(f.journal), { code: "ENOENT" });
    assert.equal(await current.runner.recoveryInventoryPending(f.runId, f.generation), false);
    assert.equal(await current.runner.recoveryInventoryPending(f.runId, f.generation + 1), true,
      "receipt success is scoped to the exact generation");
    await (current.runner as unknown as { retireFinalizeRecord(flight: unknown, site: string): Promise<void> })
      .retireFinalizeRecord({ runId: f.runId, claimGeneration: f.generation }, "completion-cleanup-regression");
    assert.deepEqual(await outbox.listPendingFinalizes(), [], "completed generation finalize must retire after receipt cleanup");
    assert.equal(f.rpc(), 0, "completion cleanup and finalize retirement require no custody RPC");
  } finally { await f.close(); }
});

for (const code of ["EIO", "EBUSY"]) {
  it(code + " completion disposal restores exact attempt attribution for a fresh coordinator retry", async t => {
    const f = await fixture(true, true);
    const remove = fs.rm.bind(fs);
    let holding = "";
    let injected = false;
    t.mock.method(fs, "rm", async (...args: Parameters<typeof fs.rm>) => {
      if (!injected && path.dirname(String(args[0])) === (f.git as unknown as { runnerHoldingRoot: string }).runnerHoldingRoot) {
        injected = true;
        holding = String(args[0]);
        throw Object.assign(new Error("transient completion disposal failure"), { code });
      }
      return remove(...args);
    });
    try {
      const sibling = await fs.readFile(f.siblingJournal!, "utf8");
      await f.sweep();
      assert.equal(injected, true, "actual attempt clone reached physical disposal");
      await f.retained();
      await assert.rejects(fs.stat(holding), { code: "ENOENT" }, "surviving source rolls back to canonical");
      t.mock.restoreAll();
      const fresh = f.make();
      fresh.executionTails.set(f.runId, Promise.resolve());
      await fresh.recovery.resumePending();
      await f.retained();
      fresh.executionTails.delete(f.runId);
      await fresh.recovery.resumePending();
      for (const target of [f.clone, holding, f.journal, f.bundle])
        await assert.rejects(fs.stat(target), { code: "ENOENT" });
      assert.equal(await fs.readFile(f.siblingJournal!, "utf8"), sibling);
      assert.equal(await fs.readFile(f.siblingBundle!, "utf8"), "successor bundle");
      assert.ok((await f.git.enumerateOwedCandidates(f.bare, f.runId))
        .some(c => c.sha === f.head && c.contexts.some(x => x.generation === f.generation + 1)));
      assert.equal(f.rpc(), 0);
    } finally { t.mock.restoreAll(); await f.close(); }
  });
}

for (const failure of ["partial", "rollback", "successor"] as const) {
  it("completion disposal " + failure + " failure retains receipt and residue without overwriting a canonical occupant", async t => {
    const f = await fixture(false, true);
    const remove = fs.rm.bind(fs);
    const rename = fs.rename.bind(fs);
    let holding = "";
    t.mock.method(fs, "rm", async (...args: Parameters<typeof fs.rm>) => {
      if (!holding && path.dirname(String(args[0])) === (f.git as unknown as { runnerHoldingRoot: string }).runnerHoldingRoot) {
        holding = String(args[0]);
        if (failure === "partial") await remove(path.join(holding, ".git"), { recursive: true, force: true });
        if (failure === "successor") {
          await fs.mkdir(f.clone);
          await fs.writeFile(path.join(f.clone, "successor.txt"), "retain occupant");
        }
        throw Object.assign(new Error("disposal interrupted"), { code: "EIO" });
      }
      return remove(...args);
    });
    if (failure === "rollback") t.mock.method(fs, "rename", async (...args: Parameters<typeof fs.rename>) => {
      if (String(args[0]) === holding) throw Object.assign(new Error("rollback interrupted"), { code: "EBUSY" });
      return rename(...args);
    });
    try {
      await f.sweep();
      assert.ok(holding, "actual attempt disposal reached");
      assert.ok(await fs.stat(holding));
      assert.ok(await fs.stat(f.journal));
      assert.ok(await fs.stat(f.bundle));
      t.mock.restoreAll();
      await f.make().recovery.resumePending();
      assert.ok(await fs.stat(holding), "unverified residue is never treated as successful disposal");
      assert.ok(await fs.stat(f.journal));
      if (failure === "successor")
        assert.equal(await fs.readFile(path.join(f.clone, "successor.txt"), "utf8"), "retain occupant");
      else await assert.rejects(fs.stat(f.clone), { code: "ENOENT" });
    } finally { t.mock.restoreAll(); await f.close(); }
  });
}

it("worker quarantine retains receipt authority and dirty clone", async () => {
  const f = await fixture();
  try {
    latchResidueQuarantine({ cause: "fixture quarantine", runId: f.runId, site: "completion_cleanup" }, nullLogger());
    await f.sweep();
    await f.retained();
  } finally { await f.close(); }
});
