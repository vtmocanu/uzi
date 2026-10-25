import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import type { CompletionSource, RecoveryCoordinator, RecoveryRecord } from "../src/recovery.js";
import type { Executor } from "../src/executor.js";
import type { CodexExecutionSafety } from "../src/harness.js";
import { RunDiskLocks } from "../src/run-disk-locks.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { codexRig, FakeCodexExecutor, commitInTree } from "./runner-codex-sinks-fixture.js";
import { fx, git, client, gitlabClaim, fakeGitlab, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// Private boundary controls. The separate lost-ack-codex full-execute case owns
// positive M2 disposal and the real prepare/bind/replay/forget lifecycle.
interface Flight {
  executor: Executor;
  terminalDisposal?: unknown;
  [key: string]: unknown;
}
interface PrivateRunner {
  recovery: RecoveryCoordinator;
  completionSourceBindings: Map<string, readonly unknown[]>;
  completionSourceBinds: Set<unknown>;
  conflictedCompletionSources: Set<unknown>;
  buildFlight(...args: unknown[]): Flight;
  terminalSource(flight: Flight, executor: Executor, safety: CodexExecutionSafety): unknown;
  rememberCompletionSource(source: CompletionSource, flight: Flight, executor: Executor, terminal: unknown): unknown;
  classifyCompletionSource(source: CompletionSource): { kind: string };
  forgetCompletionSourceBinding(source: CompletionSource): void;
  bindCompletionSource(flight: Flight): Promise<void>;
  withCompletionSourceBoundary(source: CompletionSource, action: (open: () => boolean) => Promise<void>): Promise<"passed" | "retained">;
}

async function setup(codex = true) {
  const rig = codexRig();
  const executor: Executor = codex
    ? new FakeCodexExecutor(rig.safety, async ctx => ({ branch: ctx.branch }), undefined, rig.capture)
    : { run: async ctx => ({ branch: ctx.branch }) };
  const claim = gitlabClaim(3703, { claim_generation: 29, inventory_guarded: true });
  const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, "m3-completion-source-proof");
  const p = runner as unknown as PrivateRunner;
  client.hasFeature = () => true;
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [{
    hold_id: "m3-open-hold", generation: 29, inventory_guarded: true, has_available_capture: false,
  }] });
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  fs.chmodSync(path.join(bare, "config"), 0o600);
  const branch = "agent/issue-3703";
  const attemptId = mintAttemptId(29);
  const clone = await git.runnerCloneForBranch(bare, branch, "m3-source", {
    isLive: () => false, beforeFree: async () => { throw new Error("unexpected incumbent"); },
  }, claim.run_id, false, undefined, {
    attemptId, isLive: () => false, beforeSeed: async paths => assert.deepEqual(paths, []),
  });
  await git.markRecoveryCapture(bare, clone.path, branch, claim.run_id, attemptId);
  const head = await git.worktreeHead(clone.path);
  assert.ok(head);
  const source: CompletionSource = {
    context: { barePath: bare, runId: claim.run_id, generation: 29, branch, kind: "issue",
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: head } },
    clonePath: clone.path, attemptId, expectedHead: head, roots: [],
  };
  const flight = p.buildFlight(claim, claim.run_id, executor, undefined, "", []);
  Object.assign(flight, { barePath: bare, worktreePath: clone.path, branch, runnerClone: clone });
  const remember = (s = source, e = executor) => p.rememberCompletionSource(s, flight, e,
    e.safety ? p.terminalSource(flight, e, e.safety) : undefined);
  return { p, source, flight, executor, rig, remember, claim };
}

// Existing runner/coordinator seams only: these cases can run unchanged on the unfixed base.
for (const outcome of ["missing", "incomplete", "missing-facade", "swapped-to-Claude"] as const) {
  it(`M3 binder behavioral: ${outcome} current-process Codex disposal refuses before inventory`, async t => {
    const f = await setup();
    if (outcome === "missing-facade") f.executor.safety = undefined;
    if (outcome === "swapped-to-Claude") f.flight.executor = { run: async ctx => ({ branch: ctx.branch }) };
    assert.ok(await f.p.recovery.pin({ runId: f.claim.run_id, generation: 29, kind: "issue",
      branch: f.source.context.branch, sourceSha: f.source.expectedHead, inventoryGuarded: true }));
    await f.p.bindCompletionSource(f.flight);
    const source = (await f.p.recovery.inspect(f.claim.run_id))[0]?.completionSource;
    assert.ok(source, "the real runner binder persisted authenticated source provenance");
    if (outcome === "incomplete") f.flight.terminalDisposal = { result: { kind: "incomplete", errors: [] } };
    const inventory = t.mock.method(git, "readInventoryCloneHeads", async () => { throw new Error("premature inventory"); });
    const journals = await f.p.recovery.inspect(f.claim.run_id);
    let actions = 0;
    assert.equal(await f.p.withCompletionSourceBoundary(JSON.parse(JSON.stringify(source)), async () => { actions++; }), "retained");
    assert.equal(actions, 0);
    assert.equal(inventory.mock.callCount(), 0, "missing exact disposal refuses BEFORE inventory");
    assert.deepEqual(await f.p.recovery.inspect(f.claim.run_id), journals, "refusal changes no authenticated record");
  });
}

it("M3 private boundary: missing, non-disposed and forged disposal retain before inventory including absent source", async t => {
  const f = await setup();
  f.remember();
  const inventory = t.mock.method(git, "readInventoryCloneHeads", async () => {
    throw new Error("unproved Codex must not inventory");
  });
  const retired = t.mock.method(git, "completionSourceAlreadyRetired", async () => true);
  const head = t.mock.method(git, "worktreeHead");
  const retirement = t.mock.method(git, "retireRunnerClone");
  let actions = 0;
  for (const disposal of [undefined, { result: { kind: "incomplete", errors: [] } }, {
    source: f.p.terminalSource(f.flight, f.executor, f.rig.safety), result: { kind: "disposed" },
  }]) {
    f.flight.terminalDisposal = disposal;
    for (const source of [f.source, { ...f.source, clonePath: f.source.clonePath + "-absent" }]) {
      assert.equal(await f.p.withCompletionSourceBoundary(source, async () => { actions++; }), "retained");
      assert.equal(f.p.classifyCompletionSource(source).kind, "knownCodex", "denial must not erase provenance");
    }
  }
  assert.equal(actions, 0);
  for (const spy of [inventory, retired, head, retirement]) assert.equal(spy.mock.callCount(), 0);
});

it("M3 private binding: serialized identity, explicit HEAD/roots advancement and sticky swapped provenance", async () => {
  const f = await setup();
  f.remember();
  const serialized: CompletionSource = JSON.parse(JSON.stringify(f.source));
  assert.equal(f.p.classifyCompletionSource(serialized).kind, "knownCodex");
  assert.equal(f.remember(serialized), f.remember(), "same executor/facade binding is idempotent");
  const advanced = { ...serialized, expectedHead: "b".repeat(40), roots: [{ sha: f.source.expectedHead, contexts: [] }] };
  f.remember(advanced);
  assert.equal(f.p.classifyCompletionSource(advanced).kind, "knownCodex");
  for (const changed of [
    { ...advanced, expectedHead: "c".repeat(40) },
    { ...serialized, context: { ...serialized.context, branch: "agent/changed" } },
    { ...serialized, attemptId: mintAttemptId(29) },
  ]) assert.equal(await f.p.withCompletionSourceBoundary(changed, async () => assert.fail("changed source deleted")), "retained");
  const replacement = new FakeCodexExecutor(f.rig.safety, async ctx => ({ branch: ctx.branch }));
  f.flight.executor = replacement;
  f.remember(f.source, replacement);
  f.flight.executor = f.executor;
  const originalSafety = f.executor.safety!;
  f.executor.safety = codexRig().safety;
  f.remember();
  f.executor.safety = originalSafety;
  assert.equal(await f.p.withCompletionSourceBoundary(serialized, async () => assert.fail("swapped provenance deleted")), "retained");
});

it("M3 private durable bind: actual second source-write failure preserves authenticated partial Codex provenance", async t => {
  const f = await setup();
  const first = await f.p.recovery.pin({ runId: f.claim.run_id, generation: 29, kind: "issue",
    branch: f.source.context.branch, sourceSha: f.source.expectedHead, inventoryGuarded: true });
  assert.ok(first);
  commitInTree(f.source.clonePath, "second.txt", "distinct second root\n");
  const secondHead = await git.worktreeHead(f.source.clonePath);
  assert.ok(secondHead);
  execFileSync("git", ["-C", f.source.clonePath, "push", f.source.context.barePath,
    secondHead + ":refs/heads/m3-source-fixture"], { stdio: "pipe" });
  assert.ok(await f.p.recovery.freezeInventory({ context: f.source.context,
    currentSha: secondHead, originalSourceSha: f.source.expectedHead, defaultBranch: "main", locallyQuiescent: true }));
  assert.equal((await f.p.recovery.inspect(f.claim.run_id)).length, 2);
  const writer = f.p.recovery as unknown as { writeRecordUnlocked(record: { completionSource?: CompletionSource }): Promise<void> };
  const write = writer.writeRecordUnlocked.bind(writer);
  let sourceWrites = 0;
  t.mock.method(writer, "writeRecordUnlocked", async (record: { completionSource?: CompletionSource }) => {
    if (record.completionSource) {
      sourceWrites++;
      assert.equal(f.p.classifyCompletionSource(record.completionSource).kind, "knownCodex",
        "runner installs provenance before the first durable source write");
      f.p.forgetCompletionSourceBinding(record.completionSource);
      assert.equal(f.p.classifyCompletionSource(record.completionSource).kind, "knownCodex",
        "pending durable bind cannot be forgotten");
      if (sourceWrites === 2) throw new Error("M3 deliberate second source-write failure");
    }
    await write(record);
  });
  await f.p.bindCompletionSource(f.flight);
  assert.equal(sourceWrites, 2);
  const records = await f.p.recovery.inspect(f.claim.run_id);
  assert.equal(records.filter(r => r.completionSource).length, 1, "first source write actually authenticated");
  const partial = records.find(r => r.completionSource)!.completionSource!;
  assert.equal(f.p.classifyCompletionSource(partial).kind, "knownCodex");
  const inventory = t.mock.method(git, "readInventoryCloneHeads", async () => assert.fail("partial bind fell back to legacy"));
  assert.equal(await f.p.withCompletionSourceBoundary(partial, async () => assert.fail("partial bind deleted")), "retained");
  assert.equal(inventory.mock.callCount(), 0);
  assert.equal(f.p.classifyCompletionSource(partial).kind, "knownCodex");
});

for (const origin of ["Codex", "Claude"] as const) {
  it(`M3 private legacy positive: unknown serialized ${origin} source retires real clone`, { skip: process.platform !== "linux" }, async () => {
    const f = await setup(origin === "Codex");
    // Serialization carries no provenance; a fresh runner must preserve legacy behavior.
    assert.ok(await f.p.recovery.pin({ runId: f.claim.run_id, generation: 29, kind: "issue",
      branch: f.source.context.branch, sourceSha: f.source.expectedHead, inventoryGuarded: true }));
    assert.equal(await f.p.recovery.bindCompletionSource(f.source), true);
    const restarted = runnerWith(() => ({ executor: f.executor }), fakeGitlab().gitlab, "m3-completion-source-proof") as unknown as PrivateRunner;
    const source = (await restarted.recovery.inspect(f.claim.run_id))[0]?.completionSource;
    assert.ok(source, "legacy fixture is read back through its authenticated durable format");
    let actions = 0;
    assert.equal(await restarted.withCompletionSourceBoundary(source, async open => {
      assert.equal(open(), true); actions++;
    }), "passed");
    assert.equal(actions, 1);
    assert.throws(() => execFileSync("git", ["-C", source.clonePath, "rev-parse", "HEAD"], { stdio: "pipe" }));
  });
}

it("M3 private legacy positive: known Claude source retains legacy cleanup", { skip: process.platform !== "linux" }, async () => {
  const f = await setup(false);
  assert.ok(await f.p.recovery.pin({ runId: f.claim.run_id, generation: 29, kind: "issue",
    branch: f.source.context.branch, sourceSha: f.source.expectedHead, inventoryGuarded: true }));
  await f.p.bindCompletionSource(f.flight);
  const source = (await f.p.recovery.inspect(f.claim.run_id))[0]?.completionSource;
  assert.ok(source);
  let actions = 0;
  assert.equal(await f.p.withCompletionSourceBoundary(source, async open => {
    assert.equal(open(), true); actions++;
  }), "passed");
  assert.equal(actions, 1);
});

// Lifecycle controls: the authenticated coordinator and runner callback are real;
// the physical Git cleanup is stubbed, so these cases do not attest process proof.
for (const outcome of ["success", "partial-retry", "pending-bind", "concurrent-binds", "late-bind",
  "normal-success", "normal-failure", "normal-uncertain", "producer-success", "producer-failure", "producer-uncertain"] as const) {
  it(`fallback binding lifecycle: ${outcome}`, async t => {
    const f = await setup();
    const group = f.claim.run_id + ":29";
    assert.ok(await f.p.recovery.pin({ runId: f.claim.run_id, generation: 29, kind: "issue",
      branch: f.source.context.branch, sourceSha: f.source.expectedHead, inventoryGuarded: true }));
    await f.p.bindCompletionSource(f.flight);
    assert.ok(f.p.completionSourceBindings.get(group)?.length, "real runner binder installed provenance");
    const frozen = await f.p.recovery.freezeInventory({ context: f.source.context,
      currentSha: f.source.expectedHead, originalSourceSha: f.source.expectedHead,
      defaultBranch: "main", locallyQuiescent: true });
    assert.ok(frozen?.coverageDigest);
    const writer = f.p.recovery as unknown as { writeRecordUnlocked(record: RecoveryRecord): Promise<void> };
    // Represent an authenticated archive FINAL accepted without a publication receipt.
    await writer.writeRecordUnlocked({ ...frozen, serverCaptureId: "fallback-archive",
      finalAcknowledged: true, finalRequest: { disposition: { kind: "archive",
        capture_id: "fallback-archive", source_sha: frozen.sourceSha, coverage_digest: frozen.coverageDigest } } });
    const records = await f.p.recovery.inspect(f.claim.run_id);
    assert.equal(records.length, 2);
    assert.ok(records.every(r => !r.completionReceipt), "this is the archive/FINAL path");
    const ackFile = path.join(git.recoveryRoot, f.claim.run_id, frozen.captureId + ".json");
    assert.ok(fs.existsSync(ackFile));

    const original = f.remember();
    const advanced = f.remember({ ...f.source, expectedHead: "b".repeat(40) });
    const replacement = new FakeCodexExecutor(f.rig.safety, async ctx => ({ branch: ctx.branch }));
    const conflicting = f.remember(f.source, replacement);
    const otherSource = { ...f.source, attemptId: mintAttemptId(29), clonePath: f.source.clonePath + "-other" };
    const other = f.remember(otherSource);
    for (const binding of [original, advanced, conflicting])
      assert.equal(f.p.conflictedCompletionSources.has(binding), true, "fixture carries real identity conflicts");
    const siblingGeneration = { ...f.source, context: { ...f.source.context, generation: 30 } };
    const siblingRun = { ...f.source, context: { ...f.source.context, runId: "33333333-3333-4333-8333-333333333333" } };
    f.remember(siblingGeneration);
    f.remember(siblingRun);
    const siblingGroups = [...f.p.completionSourceBindings].filter(([key]) => key !== group);

    const holdsBind = !["success", "partial-retry"].includes(outcome);
    const cleanupEntered = barrier(), cleanupRelease = barrier();
    const cleanup = t.mock.method(git, "cleanupRecoveryGeneration", async () => {
      cleanupEntered.open();
      if (holdsBind) await cleanupRelease.wait;
      return "removed" as const;
    });
    const unlink = fsp.unlink.bind(fsp);
    const refusal = outcome === "partial-retry" ? t.mock.method(fsp, "unlink", async (target: Parameters<typeof fsp.unlink>[0]) => {
      if (String(target) === ackFile) throw new Error("deliberate fallback ACK unlink failure");
      return unlink(target);
    }) : undefined;
    const binders: Array<{ installed: ReturnType<typeof barrier>; acquired: ReturnType<typeof barrier>;
      release: ReturnType<typeof barrier>; pending?: Promise<void> }> = [];
    const acquire = RunDiskLocks.prototype.acquire;
    const bindResults: boolean[] = [];
    const bind = f.p.recovery.bindCompletionSource.bind(f.p.recovery);
    t.mock.method(f.p.recovery, "bindCompletionSource", async (source: CompletionSource) => {
      const result = await bind(source);
      bindResults.push(result);
      return result;
    });
    let nextBinder = 0;
    t.mock.method(RunDiskLocks.prototype, "acquire", async function (this: RunDiskLocks,
      key: string, signal?: AbortSignal) {
      const gate = key === path.resolve(git.recoveryRoot, f.claim.run_id) ? binders[nextBinder] : undefined;
      const tails = (this as unknown as { tails: Map<string, Promise<void>> }).tails;
      const before = tails.get(key);
      const result = acquire.call(this, key, signal);
      if (!gate) return result;
      nextBinder++;
      assert.notEqual(tails.get(key), before, "real acquire installed the journal-lock waiter synchronously");
      gate.installed.open();
      const release = await result;
      gate.acquired.open();
      await gate.release.wait; // Own the real lock, but delay the real binder body.
      return release;
    });
    const startBinder = async () => {
      const gate = { installed: barrier(), acquired: barrier(), release: barrier(), pending: undefined as Promise<void> | undefined };
      binders.push(gate);
      gate.pending = f.p.bindCompletionSource(f.flight);
      await gate.installed.wait;
      return gate;
    };
    const controlWitness = holdsBind ? undefined : f.p.recovery.registerCompletionCleanupWitness(f.claim.run_id, 29);
    let forgetting: Promise<void> | undefined;
    if (holdsBind) {
      forgetting = f.p.recovery.forgetGeneration(f.claim.run_id, 29);
      await cleanupEntered.wait;
      await startBinder();
      if (outcome === "concurrent-binds") await startBinder();
      assert.ok(f.p.completionSourceBinds.size, "actual runner binders wait on fallback's journal lock");
    }
    try {
      if (refusal) {
        await assert.rejects(f.p.recovery.forgetGeneration(f.claim.run_id, 29), /deliberate fallback ACK unlink failure/);
        assert.equal(f.p.classifyCompletionSource(f.source).kind, "knownCodex");
        assert.equal(f.p.completionSourceBindings.get(group)?.length, 4, "partial failure retains the full group");
        assert.ok(fs.existsSync(ackFile));
        assert.equal(controlWitness!.fullyForgotten(), false, "partial ACK unlink never proves cleanup");
        for (const binding of [original, advanced, conflicting])
          assert.equal(f.p.conflictedCompletionSources.has(binding), true);
        refusal.mock.restore();
      }
      cleanupRelease.open();
      if (forgetting) await forgetting;
      else await f.p.recovery.forgetGeneration(f.claim.run_id, 29);
      if (holdsBind) await binders[0]!.acquired.wait;
      assert.equal(cleanup.mock.callCount(), refusal ? 2 : 1);
      if (!holdsBind) {
        assert.deepEqual(await f.p.recovery.inspect(f.claim.run_id), []);
        assert.equal(controlWitness!.fullyForgotten(), true);
      }
      assert.equal(fs.existsSync(ackFile), false);
      if (holdsBind) {
        const retained = f.p.completionSourceBindings.get(group)!;
        assert.equal(retained.length, 3 + binders.length - 1);
        for (const binding of [original, advanced, conflicting]) {
          assert.equal(retained.includes(binding), true);
          assert.equal(f.p.conflictedCompletionSources.has(binding), true);
        }
        assert.equal(retained.includes(other), false, "another identity in the same generation is released");
        assert.equal(f.p.classifyCompletionSource(f.source).kind, "knownCodex");
        const inventory = t.mock.method(git, "readInventoryCloneHeads", async () => assert.fail("pending provenance used legacy inventory"));
        assert.equal(await f.p.withCompletionSourceBoundary(f.source, async () => assert.fail("pending bind deleted")), "retained");
        assert.equal(inventory.mock.callCount(), 0);
        if (outcome === "late-bind") await startBinder();
        const recreated = outcome.startsWith("normal-") || outcome.startsWith("producer-");
        if (recreated) {
          const witness = f.p.recovery.registerCompletionCleanupWitness(f.claim.run_id, 29);
          assert.equal(witness.fullyForgotten(), true, "late lease inherits shared proof");
          const writer = f.p.recovery as unknown as JournalWriter;
          const producer = outcome.startsWith("producer-");
          const firstIO = producer ? "lstat" : "mkdir";
          const originalIO = fsp[firstIO].bind(fsp);
          let ioSeen = false;
          const first = t.mock.method(fsp, firstIO, async (...args: unknown[]) => {
            ioSeen = true;
            assert.equal(witness.fullyForgotten(), false, "write invalidates BEFORE its first IO await");
            if (outcome.endsWith("failure")) throw new Error("deliberate first IO failure");
            return (originalIO as (...args: unknown[]) => Promise<unknown>)(...args);
          });
          const rename = fsp.rename.bind(fsp);
          const uncertain = outcome.endsWith("uncertain") ? t.mock.method(fsp, "rename", async (...args: Parameters<typeof fsp.rename>) => {
            await rename(...args);
            throw new Error("deliberate uncertain rename after persistence");
          }) : undefined;
          try {
            const writing = producer
              ? MaintenanceBudget.runProducer(() => writer.writeProducerOriginUnlocked(frozen, () => {}, async () => {}))
              : writer.writeRecordUnlocked(frozen);
            assert.equal(witness.fullyForgotten(), false, "invalidation is synchronous at writer entry");
            if (outcome.endsWith("success")) await writing;
            else await assert.rejects(writing, /deliberate/);
            assert.equal(ioSeen, true);
            assert.equal(witness.fullyForgotten(), false);
          } finally {
            first.mock.restore(); uncertain?.mock.restore(); witness.release(); witness.release();
          }
        }
        for (const [i, binder] of binders.entries()) {
          await binder.acquired.wait;
          binder.release.open();
          await binder.pending;
          if (i + 1 < binders.length) {
            const retained = f.p.completionSourceBindings.get(group)!;
            for (const binding of [original, advanced, conflicting]) {
              assert.equal(retained.includes(binding), true, "another matching binder retains every identity version");
              assert.equal(f.p.conflictedCompletionSources.has(binding), true);
            }
          }
        }
        if (recreated) {
          const retained = f.p.completionSourceBindings.get(group)!;
          assert.ok(retained?.length, "recreation invalidates cleanup proof even on failed/uncertain writes");
          for (const binding of [original, advanced, conflicting]) {
            assert.equal(retained.includes(binding), true);
            assert.equal(f.p.conflictedCompletionSources.has(binding), true);
          }
          assert.equal(f.p.classifyCompletionSource(f.source).kind, "knownCodex");
        } else {
          assert.deepEqual(bindResults, binders.map(() => false), "each real binder finds empty records and returns false");
          assert.deepEqual(await f.p.recovery.inspect(f.claim.run_id), []);
          assert.equal(f.p.completionSourceBindings.has(group), false, "settled real binders release the fully forgotten generation");
          for (const binding of [original, advanced, conflicting, other])
            assert.equal(f.p.conflictedCompletionSources.has(binding), false, "settlement removes identity conflicts");
        }
        assert.equal(inventory.mock.callCount(), 0);

      } else {
        assert.equal(f.p.completionSourceBindings.has(group), false, "full cleanup, including retry, drops the flight/executor group");
        for (const binding of [original, advanced, conflicting, other])
          assert.equal(f.p.conflictedCompletionSources.has(binding), false);
      }
      for (const [key, bindings] of siblingGroups)
        assert.equal(f.p.completionSourceBindings.get(key), bindings, "sibling run/generation remains unchanged");
    } finally {
      cleanupRelease.open();
      for (const gate of binders) gate.release.open();
      await Promise.all(binders.map(gate => gate.pending));
      await forgetting;
      controlWitness?.release(); controlWitness?.release();
    }
    // Behavioral cases deliberately use only pre-existing APIs, so they can be copied to the base.
    if (outcome !== "pending-bind" && outcome !== "concurrent-binds" && outcome !== "late-bind") {
      const internals = f.p.recovery as unknown as { completionCleanupWitnesses: Map<string, unknown> };
      assert.equal(internals.completionCleanupWitnesses.size, 0, "last release removes shared evidence");
      const fresh = f.p.recovery.registerCompletionCleanupWitness(f.claim.run_id, 29);
      assert.equal(fresh.fullyForgotten(), false, "a new group cannot inherit a tombstone");
      fresh.release();
    }
  });
}

function barrier() {
  let open!: () => void;
  const wait = new Promise<void>(resolve => { open = resolve; });
  return { wait, open };
}

interface JournalWriter {
  writeRecordUnlocked(record: RecoveryRecord): Promise<void>;
  writeProducerOriginUnlocked(record: RecoveryRecord, check: () => void, fresh: () => Promise<void>): Promise<void>;
}

for (const result of ["false", "throw"] as const) {
  it(`cleanup witness: unproven ${result} binder preserves provenance and releases its lease`, async t => {
    const f = await setup();
    t.mock.method(f.p.recovery, "bindCompletionSource", async () => {
      if (result === "throw") throw new Error("deliberate unproven bind failure");
      return false;
    });
    await f.p.bindCompletionSource(f.flight);
    assert.equal(f.p.classifyCompletionSource(f.source).kind, "knownCodex");
    assert.equal(f.p.completionSourceBinds.size, 0);
    const internals = f.p.recovery as unknown as { completionCleanupWitnesses: Map<string, unknown> };
    assert.equal(internals.completionCleanupWitnesses.size, 0);
  });
}

it("cleanup witness: empty cleanup never proves; leases are exact, idempotent and bounded", async () => {
  const f = await setup();
  const internals = f.p.recovery as unknown as { completionCleanupWitnesses: Map<string, unknown> };
  for (let generation = 29; generation < 39; generation++) {
    const a = f.p.recovery.registerCompletionCleanupWitness(f.claim.run_id, generation);
    const b = f.p.recovery.registerCompletionCleanupWitness(f.claim.run_id, generation);
    const sibling = f.p.recovery.registerCompletionCleanupWitness("33333333-3333-4333-8333-333333333333", generation);
    await f.p.recovery.forgetGeneration(f.claim.run_id, generation);
    assert.equal(a.fullyForgotten(), false);
    assert.equal(b.fullyForgotten(), false);
    assert.equal(sibling.fullyForgotten(), false);
    a.release(); a.release();
    assert.equal(a.fullyForgotten(), false);
    assert.equal(internals.completionCleanupWitnesses.size, 2);
    b.release(); sibling.release();
    assert.equal(internals.completionCleanupWitnesses.size, 0);
  }
});
