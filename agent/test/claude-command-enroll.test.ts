import assert from "node:assert/strict";
import { test } from "node:test";
import { PassThrough } from "node:stream";
import type { CapturedMemoryContext } from "../src/codex/broker.js";
import type { CodexRootHandle, ObservationOutcome } from "../src/codex/launcher.js";
import {
  disposeRootOnce, enrollClaudeCommand, isMintedClaudeCommand, mintClaudeCommandMetadata,
  type ClaudeCommandMetadata,
} from "../src/claude-command/enroll.js";
import { WorkerMemoryCommands } from "../src/worker-memory-commands.js";

const ticks = "18446744073709551615";
const COMMAND_ID = "55555555-5555-4555-8555-555555555555";

function captured(): CapturedMemoryContext {
  const commands = new WorkerMemoryCommands(["private-fixture"]);
  const flight = {};
  const context = commands.begin({
    run_id: "11111111-1111-4111-8111-111111111111", worker_id: "22222222-2222-4222-8222-222222222222",
    register_nonce: "nonce", claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1,
  });
  return { commands, context, flight, lifecycleRevision: 1 };
}

function fakeRoot() {
  let disposes = 0;
  let observations = 0;
  let clean = true;
  let evidence: ObservationOutcome = {
    event: "observe", id: 1, state: "complete", supervisor: { pid: 50, startTime: ticks },
    root: { pid: 51, startTime: ticks }, processes: [{ pid: 51, startTime: ticks, ppid: 50, rss: 100 }],
  };
  const handle: CodexRootHandle = {
    started: { event: "started", supervisorPid: 50, childPid: 51, uid: 10002, subreaper: true,
      nondumpable: true, liveCapsZero: false, capBoundingSet: "0xc0", noNewPrivs: true },
    supervisorPid: 50, transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
    failed: undefined, whenFailed: new Promise(() => {}),
    snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
    observe: async () => { observations++; return evidence; },
    waitChild: async () => new Promise(() => {}),
    dispose: async () => {
      disposes++;
      await new Promise((done) => setTimeout(done, 20));
      return clean ? { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }
        : { clean: false, reason: "fixture unconfirmed" };
    },
  };
  return { handle, disposes: () => disposes, observations: () => observations,
    change: (next: ObservationOutcome) => { evidence = next; }, unconfirmed: () => { clean = false; } };
}

function mint(c: CapturedMemoryContext): ClaudeCommandMetadata {
  return mintClaudeCommandMetadata(c, { toolId: "toolu_01", attemptId: "33333333-3333-4333-8333-333333333333", commandId: COMMAND_ID, command: "echo private-fixture" });
}
const authority = () => ({ remainingMs: 1000, signal: new AbortController().signal, authorized: () => true });

test("lane-minted metadata enrolls the supervisor as root and the wrapper as primary", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  const enrolled = await enrollClaudeCommand(c, mint(c), root.handle, 500);
  assert.ok(enrolled);
  const entry = enrolled.entry;
  assert.equal(entry.source, "claude-model-command");
  assert.equal(entry.root.pid, 50);
  assert.equal(entry.root.startTime, ticks, "maximum uint64 ticks are preserved");
  assert.equal(entry.primary?.pid, 51);
  assert.equal(entry.commandId, COMMAND_ID);
  assert.ok(!entry.command.includes("private-fixture"), "the registry redacts the command");
  assert.equal(c.commands.eligibleEntries()?.length, 1);
});

test("forged, cloned, unminted, foreign-context and absent-context metadata never enroll", { timeout: 10000 }, async () => {
  const c = captured();
  const other = captured();
  const root = fakeRoot();
  const minted = mint(c);
  const forged: ClaudeCommandMetadata = { toolId: "toolu_01", attemptId: "a", commandId: COMMAND_ID, command: "x" };
  assert.equal(isMintedClaudeCommand(minted, c), true);
  assert.equal(isMintedClaudeCommand({ ...minted }, c), false, "a clone has no authority");
  assert.equal(isMintedClaudeCommand(forged, c), false);
  assert.equal(isMintedClaudeCommand(minted, other), false, "bound to its own context only");
  assert.equal(isMintedClaudeCommand(undefined), false);
  for (const [ctx, meta] of [[c, forged], [c, { ...minted }], [other, minted], [undefined, minted], [c, undefined]] as const) {
    assert.equal(await enrollClaudeCommand(ctx, meta, root.handle, 500), undefined);
  }
  assert.equal(root.observations(), 0, "no observation was even attempted");
  assert.equal(c.commands.eligibleEntries()?.length, 0);
  assert.equal(other.commands.eligibleEntries()?.length, 0);
});

test("the registry accepts exactly the two model-command source literals", { timeout: 10000 }, () => {
  const c = captured();
  const base = {
    toolId: "t", attemptId: "a", command: "x", root: { pid: 50, startTime: "1" },
    observe: async () => ({ available: false, complete: false, startedAtMs: 0, completedAtMs: 0, rows: [] }),
    cancel: async () => {}, drain: async () => ({ confirmed: false }),
  };
  for (const source of ["codex-model-command", "claude-model-command"]) {
    assert.doesNotThrow(() => c.commands.register(c.context, { ...base, source } as never));
  }
  for (const source of ["claude-model-commands", "Claude-model-command", "model-command", "", "codex", undefined, null, 1]) {
    assert.throws(() => c.commands.register(c.context, { ...base, source } as never), /untrusted memory command/, String(source));
  }
});

test("an uncertain initial observation leaves the command unenrolled", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  root.change({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" });
  assert.equal(await enrollClaudeCommand(c, mint(c), root.handle, 500), undefined);
  assert.equal(c.commands.eligibleEntries()?.length, 0);
  assert.equal(root.disposes(), 0, "uncertainty never disposes");
});

test("enrollment is refused when the captured admission is closed before or while observing", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  const metadata = mint(c);
  const latch = c.commands.acquireLatch(c.context)!;
  await assert.rejects(enrollClaudeCommand(c, metadata, root.handle, 500), /admission closed/);
  c.commands.releaseLatch(latch);
  const slow = fakeRoot();
  const original = slow.handle.observe;
  const handle: CodexRootHandle = { ...slow.handle, observe: async (ms) => { c.commands.retire(c.context); return original(ms); } };
  await assert.rejects(enrollClaudeCommand(c, metadata, handle, 500), /admission closed/);
  assert.equal(c.commands.eligibleEntries()?.length, 0);
});

test("single-flight disposal: reap, drain, cancel and the lane's own dispose share one operation", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  const enrolled = (await enrollClaudeCommand(c, mint(c), root.handle, 500))!;
  const entry = enrolled.entry;
  const results = await Promise.all([
    enrolled.reap(500), entry.drain(authority()), entry.cancel(authority()).then(() => "cancelled"),
    disposeRootOnce(root.handle, 500),
  ]);
  assert.equal(root.disposes(), 1, "the supervisor is disposed once however many parties ask");
  assert.deepEqual(results[0], { ok: true });
  assert.deepEqual(results[1], { confirmed: true, detail: undefined });
  assert.equal(results[3], true);
  assert.equal(c.commands.hasConfirmedStop(entry), true);
  assert.equal(c.commands.eligibleEntries()?.length, 0, "a confirmed drain unregisters the entry");
  assert.equal(await disposeRootOnce(root.handle, 500), true);
  assert.equal(root.disposes(), 1, "later callers still join the memoized result");
});

test("a stale cancellation never disposes: changed supervisor ticks, retired context, aborted authority", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  const enrolled = (await enrollClaudeCommand(c, mint(c), root.handle, 500))!;
  const entry = enrolled.entry;
  root.change({ event: "observe", id: 2, state: "complete", supervisor: { pid: 50, startTime: "18446744073709551614" },
    root: { pid: 51, startTime: ticks }, processes: [{ pid: 51, startTime: ticks, ppid: 50, rss: 100 }] });
  await assert.rejects(entry.cancel(authority()), /stale/);
  const aborted = new AbortController();
  aborted.abort();
  await assert.rejects(entry.cancel({ remainingMs: 1000, signal: aborted.signal, authorized: () => true }), /stale/);
  c.commands.retire(c.context);
  await assert.rejects(entry.cancel(authority()), /stale/);
  assert.equal(root.disposes(), 0);
});

test("an unconfirmed disposal records a failed drain and keeps the entry registered", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  root.unconfirmed();
  const enrolled = (await enrollClaudeCommand(c, mint(c), root.handle, 500))!;
  const entry = enrolled.entry;
  assert.deepEqual(await enrolled.reap(500), { ok: false });
  enrolled.recordDisposal(false);
  assert.equal(c.commands.hasConfirmedStop(entry), false);
  assert.ok(c.commands.drainFailure(entry), "the failed drain is recorded");
  assert.equal(c.commands.unregister(entry), false, "an unconfirmed entry cannot be unregistered");
});
