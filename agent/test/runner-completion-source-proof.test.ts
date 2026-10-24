import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import type { CompletionSource, RecoveryCoordinator } from "../src/recovery.js";
import type { Executor } from "../src/executor.js";
import type { CodexExecutionSafety } from "../src/harness.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import fs from "node:fs";
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
for (const outcome of ["missing", "incomplete"] as const) {
  it(`M3 binder behavioral: ${outcome} current-process Codex disposal refuses before inventory`, async t => {
    const f = await setup();
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
