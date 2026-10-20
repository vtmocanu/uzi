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
  let minBudget = 0;
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
    dispose: async (budgetMs) => {
      disposes++;
      // A disposal given less than `minBudget` ms cannot finish: it reports unconfirmed at once.
      if (budgetMs !== undefined && budgetMs < minBudget) return { clean: false, reason: "dispose budget exhausted" };
      await new Promise((done) => setTimeout(done, 20));
      return clean ? { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }
        : { clean: false, reason: "fixture unconfirmed" };
    },
  };
  return { handle, disposes: () => disposes, observations: () => observations,
    change: (next: ObservationOutcome) => { evidence = next; }, unconfirmed: () => { clean = false; }, needsBudget: (ms: number) => { minBudget = ms; } };
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
  assert.equal(c.commands.unregister(entry), false, "the reap path already unregistered the confirmed entry");
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

const settle = () => new Promise((done) => setTimeout(done, 60));

test("an unconfirmed guard cancel does not poison the root: the guard's drain retry then confirms", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  root.needsBudget(100);
  const enrolled = (await enrollClaudeCommand(c, mint(c), root.handle, 500))!;
  const entry = enrolled.entry;
  // A tiny guard budget: the disposal cannot finish, and the failure is recorded, not cached.
  await entry.cancel({ remainingMs: 30, signal: new AbortController().signal, authorized: () => true });
  await settle();
  assert.equal(root.disposes(), 1);
  assert.equal(c.commands.hasConfirmedStop(entry), false);
  assert.ok(c.commands.drainFailure(entry));
  assert.equal(enrolled.guardStopped(), true, "the guard's own cancel ran");
  // The retry gets a fresh disposal with the larger budget and confirms.
  assert.deepEqual(await entry.drain(authority()), { confirmed: true, detail: undefined });
  assert.equal(root.disposes(), 2, "the failed attempt was retried, not replayed");
  assert.equal(c.commands.hasConfirmedStop(entry), true);
  assert.equal(c.commands.unregister(entry), false, "a confirmed drain already unregistered the entry");
});

test("an unconfirmed guard cancel leaves the lane's own disposal free to succeed with a larger budget", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  root.needsBudget(100);
  const enrolled = (await enrollClaudeCommand(c, mint(c), root.handle, 500))!;
  await enrolled.entry.cancel({ remainingMs: 30, signal: new AbortController().signal, authorized: () => true });
  await settle();
  assert.equal(await disposeRootOnce(root.handle, 500), true);
  assert.equal(root.disposes(), 2);
  // A failure is not cached even for direct callers; success is, and later callers join it.
  assert.equal(await disposeRootOnce(root.handle, 1), true);
  assert.equal(root.disposes(), 2);
});

test("guardStopped is false until the guard's own cancel or drain reaches disposal", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  const enrolled = (await enrollClaudeCommand(c, mint(c), root.handle, 500))!;
  assert.equal(enrolled.guardStopped(), false);
  assert.deepEqual(await enrolled.reap(500), { ok: true }, "the lane's own reap is not a guard stop");
  assert.equal(enrolled.guardStopped(), false);
  const stale = (await enrollClaudeCommand(c, mint(c), fakeRoot().handle, 500))!;
  await assert.rejects(stale.entry.cancel({ remainingMs: 1000, signal: AbortSignal.abort(), authorized: () => true }), /stale/);
  assert.equal(stale.guardStopped(), false, "a refused cancel stopped nothing");
});

test("a stale initial observation from the wrapper's startup is re-sampled and enrolls", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  const stable = await root.handle.observe(500);
  let calls = 0;
  const handle: CodexRootHandle = { ...root.handle, observe: async () => {
    calls++;
    return calls <= 2 ? { event: "observe", id: calls, state: "unavailable", reason: "stale" } : stable;
  } };
  const enrolled = await enrollClaudeCommand(c, mint(c), handle, 1000);
  assert.ok(enrolled, "a transient stale sample does not leave the command unenrolled");
  assert.equal(calls, 3);
  assert.equal(c.commands.eligibleEntries()?.length, 1);
});

test("a persistently stale initial observation stays unenrolled within the observe budget", { timeout: 10000 }, async () => {
  const c = captured();
  const root = fakeRoot();
  root.change({ event: "observe", id: 1, state: "unavailable", reason: "stale" });
  const started = performance.now();
  const enrolled = await enrollClaudeCommand(c, mint(c), root.handle, 200);
  assert.equal(enrolled, undefined);
  assert.ok(performance.now() - started < 1000, "the retry never outlives the observe budget");
  assert.ok(root.observations() > 1, "the stale sample was retried");
  assert.equal(root.disposes(), 0, "uncertainty never disposes");
});

/** A handle whose observe behaves like the launcher's: one request in flight, its own budget timer
 *  (a late reply settles as `timeout`), and a stale reply after a few ms. */
function launcherLikeStale(replyMs: () => number) {
  let calls = 0;
  let inFlight = false;
  const root = fakeRoot();
  const handle: CodexRootHandle = { ...root.handle, observe: (budgetMs?: number) => {
    calls++;
    if (inFlight) return Promise.resolve<ObservationOutcome>({ event: "observe", id: calls, state: "unavailable", reason: "busy" } as unknown as ObservationOutcome);
    inFlight = true;
    return new Promise<ObservationOutcome>((resolve) => {
      let done = false;
      const settle = (outcome: ObservationOutcome) => { if (done) return; done = true; inFlight = false; clearTimeout(budget); clearTimeout(reply); resolve(outcome); };
      const budget = setTimeout(() => settle({ event: "observe", id: calls, state: "unavailable", reason: "timeout" }), budgetMs ?? 1000);
      const reply = setTimeout(() => settle({ event: "observe", id: calls, state: "unavailable", reason: "stale" }), replyMs());
    });
  } };
  return { handle, calls: () => calls, root };
}

test("a tree that never settles runs unenrolled, never as an enrollment failure, before the outer enrollment deadline", { timeout: 60000 }, async () => {
  const deadlineMs = 100;
  for (let run = 0; run < 200; run++) {
    const c = captured();
    const fake = launcherLikeStale(() => 1 + (run % 7));
    const started = performance.now();
    const enrolled = await enrollClaudeCommand(c, mint(c), fake.handle, deadlineMs);
    assert.equal(enrolled, undefined, `run ${run} ended unenrolled`);
    assert.ok(performance.now() - started < deadlineMs, `run ${run} returned before the outer deadline`);
    assert.equal(fake.root.disposes(), 0);
  }
});

test("the stale re-sampling stops when the command is aborted", { timeout: 10000 }, async () => {
  const c = captured();
  const fake = launcherLikeStale(() => 1);
  const abort = new AbortController();
  const pending = enrollClaudeCommand(c, mint(c), fake.handle, 10_000, abort.signal);
  await new Promise((done) => setTimeout(done, 60));
  assert.ok(fake.calls() > 1, "the loop was sampling");
  abort.abort();
  await assert.rejects(pending, /admission closed/);
  await new Promise((done) => setTimeout(done, 30));
  const after = fake.calls();
  await new Promise((done) => setTimeout(done, 100));
  assert.equal(fake.calls(), after, "no observe is issued after the abort settled");
});
