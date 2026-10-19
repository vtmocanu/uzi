import assert from "node:assert/strict";
import { test } from "node:test";
import { PassThrough } from "node:stream";
import path from "node:path";
import type { HookInput } from "@anthropic-ai/claude-agent-sdk";
import type { CapturedMemoryContext } from "../src/codex/broker.js";
import type { CodexRootHandle, ObservationOutcome, RunnerCommandChild } from "../src/codex/launcher.js";
import { GUARDED_COMMAND_TOOL } from "../src/claude-command/grants.js";
import { ClaudeCommandLane, GuardedCommandUnconfirmedError } from "../src/claude-command/lane.js";
import type { MemoryReservation, MemoryReservationRequest, WorkerMemoryPressureResult } from "../src/protocol.js";
import { WorkerMemoryCommands } from "../src/worker-memory-commands.js";
import { WorkerMemoryMonitor } from "../src/worker-memory-guard.js";
import type { WorkerMemoryReader } from "../src/worker-memory-reader.js";

// Issue #2471: the shared guard paths (reservation fence, quiet rearm, retry blocking, unconfirmed
// drain, allowance exhaustion, tool feedback, whole-run quiescence) driven end to end through the
// REAL worker memory monitor, the REAL command registry and the REAL Claude command lane. Only the
// sampler, the API ports and the supervisor roots are fixtures.

const WT = path.resolve(".");
const META = "claudecode/toolUseId";
const TICKS = "18446744073709551615";
const RUN_ID = "11111111-1111-4111-8111-111111111111";
const WORKER_ID = "22222222-2222-4222-8222-222222222222";
const log = { debug() {}, info() {}, warn() {} };
// Limit 1000: high water 800 (reserve 200), quiet 700 (hysteresis 100).
const LIMIT = 1000;
const HIGH = 900;
const QUIET = 100;
const settings = {
  enabled: true as const, reserveBytes: 200, hysteresisBytes: 100,
  sampleMs: 100, responseBudgetMs: 400, rearmMs: 150, maxInterventions: 5,
};

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
const sleep = (ms: number) => new Promise<void>((done) => setTimeout(done, ms));
async function until(predicate: () => boolean, what = "readiness") {
  // At most 200 attempts of 5 ms: a failure stops this test only.
  for (let i = 0; i < 200; i++) {
    if (predicate()) return;
    await sleep(5);
  }
  assert.fail(`fixture ${what} deadline`);
}

/** A supervisor root whose exit, output and disposal the test drives; its tree RSS is `rss`. */
function fakeRoot(supervisorPid: number, rss: number) {
  const exit = deferred<{ event: "child_exit"; code: number }>();
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  let disposes = 0;
  let clean = true;
  let exited = false;
  const evidence: ObservationOutcome = {
    event: "observe", id: 1, state: "complete", supervisor: { pid: supervisorPid, startTime: TICKS },
    root: { pid: supervisorPid + 1, startTime: TICKS },
    processes: [{ pid: supervisorPid + 1, startTime: TICKS, ppid: supervisorPid, rss }],
  };
  const exitWith = (code: number) => {
    if (!exited) { exited = true; exit.resolve({ event: "child_exit", code }); }
    stdout.end(); stderr.end();
  };
  const handle: CodexRootHandle = {
    started: { event: "started", supervisorPid, childPid: supervisorPid + 1, uid: 10002, subreaper: true,
      nondumpable: true, liveCapsZero: false, capBoundingSet: "0xc0", noNewPrivs: true },
    supervisorPid, transport: { stdin: new PassThrough(), stdout, stderr },
    failed: undefined, whenFailed: new Promise(() => {}),
    snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
    observe: async () => evidence,
    waitChild: async () => exit.promise,
    dispose: async () => {
      disposes++;
      exitWith(137);
      return clean ? { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }
        : { clean: false, reason: "fixture unconfirmed" };
    },
  };
  const child = { pid: supervisorPid, exitCode: null, signalCode: null, kill: () => true } as unknown as RunnerCommandChild;
  return { handle, child, exitWith, disposes: () => disposes, unconfirmed: () => { clean = false; } };
}
type Root = ReturnType<typeof fakeRoot>;

type Sample = Extract<ReturnType<WorkerMemoryReader["sample"]>, { available: true }>;

/**
 * One flight: the real registry/monitor/lane. `reserve` calls park on a deferred the test settles
 * (`grant`/`deny`), so the test decides when the API answers. Time is the process-wide monotonic
 * clock (the lane's observations are stamped with it); waits are short real sleeps.
 */
function rig(over: { maxInterventions?: number } = {}) {
  const commands = new WorkerMemoryCommands(["private-fixture"]);
  const flight = {};
  const policy = { version: 1 as const, max_interventions: over.maxInterventions ?? 5 };
  const context = commands.begin({
    run_id: RUN_ID, worker_id: WORKER_ID, register_nonce: "nonce", claim_generation: 1, memory_episode: 1,
    flight, lifecycleRevision: 1, memory_policy: policy,
  });
  const captured: CapturedMemoryContext = { commands, context, flight, lifecycleRevision: 1 };
  const trips: Error[] = [];
  const kills: RunnerCommandChild[] = [];
  const roots = new Map<string, Root>();
  const rss = new Map<string, number>();
  let launched = 0;
  const lane = new ClaudeCommandLane({
    worktree: WT, cliEnv: { PATH: "/usr/bin", HOME: "/home/runner" }, secretPaths: [], dockerWired: false,
    secrets: ["private-fixture"], log, capturedMemory: () => captured,
    tripTurn: (error) => { trips.push(error); },
    kill: (child) => { kills.push(child); return true; },
    graceMs: 30, cleanupMs: 300,
    launch: async (spec) => {
      const id = spec.command.replace(/^sleep-/, "");
      const root = fakeRoot(50 + 10 * launched++, rss.get(id) ?? 100);
      roots.set(id, root);
      spec.onSpawned?.({ pid: root.child.pid as number, startTime: 4242 + launched }, root.child);
      return { handle: root.handle, child: root.child };
    },
  });
  lane.beginTurn();

  let usage = HIGH;
  let sampleId = 0;
  const reserves: Array<{ request: MemoryReservationRequest; answer: ReturnType<typeof deferred<MemoryReservation>> }> = [];
  const reports: string[] = [];
  const feedback: WorkerMemoryPressureResult[] = [];
  const preserved: unknown[] = [];
  const preparedPreservation: unknown[] = [];
  const now = () => performance.now();
  const ack = (request: MemoryReservationRequest, remaining: number, extra: Partial<MemoryReservation> = {}): MemoryReservation => ({
    ...request, admitted: true, authorizing: true,
    allowance: { limit: request.policy.max_interventions, used: request.policy.max_interventions - remaining, remaining }, ...extra,
  });
  const monitor = new WorkerMemoryMonitor(settings, {
    commands, now, incarnation: () => ({ worker_id: WORKER_ID, register_nonce: "nonce", revision: 1 }),
    sample: (): Sample => {
      const t = now();
      return {
        available: true, sampleId: ++sampleId, startedAtMs: t, completedAtMs: t, currentBytes: usage, limitBytes: LIMIT,
        stat: { anon: 10, shmem: 10, slab_unreclaimable: 10, unevictable: 10 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 }, full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } },
      };
    },
    reserve: (request) => {
      const answer = deferred<MemoryReservation>();
      reserves.push({ request, answer });
      return answer.promise;
    },
    report: async (request) => {
      reports.push(request.outcome);
      const reserved = reserves.find((r) => r.request.intervention_id === request.intervention_id)!.request;
      return ack(reserved, 1, { authorizing: false, outcome: request.outcome });
    },
    feedback: async (result) => { feedback.push(result); },
    preserve: async (ctx) => { preserved.push(ctx); },
    preparePreservation: (ctx) => { preparedPreservation.push(ctx); },
  });

  /** Start a long-running guarded command named `id` (its root never exits on its own). */
  const start = (id: string, treeRss: number) => {
    rss.set(id, treeRss);
    const call = callTool(`sleep-${id}`, `toolu_${id}`);
    const outcome: { settled: boolean } = { settled: false };
    void call.then(() => { outcome.settled = true; }, () => { outcome.settled = true; });
    return { id, call, outcome };
  };
  async function callTool(command: string, toolUseId: string) {
    const hook = await lane.hook({
      hook_event_name: "PreToolUse", tool_name: GUARDED_COMMAND_TOOL, tool_input: { command, timeout: 30_000 }, tool_use_id: toolUseId,
      session_id: "s", transcript_path: "/t", cwd: WT,
    } as HookInput, toolUseId, { signal: new AbortController().signal });
    assert.deepEqual(hook, {}, "the hook admits the call");
    return lane.handle({ command, timeout: 30_000 }, { _meta: { [META]: toolUseId } });
  }
  const text = (r: { content: { type: string; text?: string }[] }) => r.content.map((b) => b.text ?? "").join("");
  const grant = (index: number, remaining: number) => reserves[index]!.answer.resolve(ack(reserves[index]!.request, remaining));
  /** A refusal: no allowance left, nothing admitted, nothing authorized. */
  const deny = (index: number) => {
    const request = reserves[index]!.request;
    reserves[index]!.answer.resolve({ ...ack(request, 0, { admitted: false, authorizing: false }),
      allowance: { limit: request.policy.max_interventions, used: request.policy.max_interventions, remaining: 0 } });
  };
  return {
    commands, context, lane, monitor, trips, kills, roots, reserves, reports, feedback, preserved, preparedPreservation,
    start, callTool, text, grant, deny,
    setUsage: (n: number) => { usage = n; },
    enrolled: (n: number) => until(() => commands.eligibleEntries()?.length === n, `${String(n)} enrolled commands`),
    entryFor: (toolId: string) => commands.eligibleEntries()!.find((e) => e.toolId === toolId)!,
  };
}

/** The registry's identity of an enrolled command, captured before it is unregistered. */
const identityOf = (r: ReturnType<typeof rig>, toolId: string) => {
  const entry = r.entryFor(toolId);
  return { toolId, attemptId: entry.attemptId, commandId: entry.commandId, command: entry.command };
};
const expectedResult = (identity: ReturnType<typeof identityOf>, fields: Pick<WorkerMemoryPressureResult, "intervention_id" | "tree_rss_bytes_approx" | "outcome" | "cancellation_confirmed">): WorkerMemoryPressureResult => {
  const { toolId, attemptId, commandId, command } = identity;
  return {
    run_id: RUN_ID, worker_id: WORKER_ID, claim_generation: 1, memory_episode: 1, intervention_id: fields.intervention_id,
    attempt_id: attemptId, code: "worker_memory_pressure", command_id: commandId, tool_id: toolId,
    command, observed_total_bytes: HIGH, limit_bytes: LIMIT, tree_rss_bytes_approx: fields.tree_rss_bytes_approx,
    cancellation_confirmed: fields.cancellation_confirmed, outcome: fields.outcome, retry_blocked: true,
    guidance: "Use smaller or serial commands after recovery.",
  };
};

test("reservation fence: an intervention disposes the victim only after the API reservation is granted", { timeout: 15000 }, async () => {
  const r = rig();
  const victim = r.start("a", 500);
  await r.enrolled(1);
  const identity = identityOf(r, "toolu_a");
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "reservation request");
  await sleep(50);
  assert.equal(r.roots.get("a")!.disposes(), 0, "nothing is disposed while the reservation is unanswered");
  assert.equal(victim.outcome.settled, false, "the model's call is still running");
  assert.equal(r.lane.liveCount, 1);
  r.grant(0, 4);
  const result = await victim.call;
  assert.equal(r.roots.get("a")!.disposes() >= 1, true);
  assert.equal(result.isError, true);
  assert.deepEqual(JSON.parse(r.text(result)), expectedResult(identity, {
    intervention_id: r.reserves[0]!.request.intervention_id, tree_rss_bytes_approx: 500,
    outcome: "confirmed_drained", cancellation_confirmed: true,
  }));
  await r.monitor.stop();
});

test("a refused reservation cancels nothing: the command keeps running and the model gets a no_signal outcome", { timeout: 15000 }, async () => {
  const r = rig();
  const victim = r.start("a", 500);
  await r.enrolled(1);
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "reservation request");
  r.deny(0);
  await r.monitor.settle();
  await sleep(30);
  assert.equal(r.roots.get("a")!.disposes(), 0, "a refusal never disposes the command");
  assert.equal(victim.outcome.settled, false, "the command is still live after the refusal");
  assert.equal(r.lane.liveCount, 1);
  assert.equal(r.feedback[0]?.outcome, "no_signal");
  assert.equal(r.feedback[0]?.cancellation_confirmed, false);
  assert.equal(r.trips.length, 0, "a refusal does not trip the turn");
  // The run's own end disposes the survivor; the outcome it carries is the truthful no_signal.
  await r.lane.quiesce(1000);
  const result = await victim.call;
  assert.equal(JSON.parse(r.text(result)).outcome, "no_signal");
  assert.equal(JSON.parse(r.text(result)).cancellation_confirmed, false);
  await r.monitor.stop();
});

test("quiet rearm: after a confirmed drain a fresh continuous quiet dwell is required before the next intervention", { timeout: 15000 }, async () => {
  const r = rig();
  const victim = r.start("a", 500);
  const sibling = r.start("b", 100);
  await r.enrolled(2);
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "first reservation");
  r.grant(0, 4);
  await victim.call;
  await r.monitor.settle();
  assert.equal(r.reports[0], "confirmed_drained");
  assert.equal(r.monitor.state.unconfirmedDrain, false);
  // Usage stays high: the drain being confirmed is not enough, the guard is not re-armed.
  r.monitor.tick();
  await r.monitor.settle();
  assert.equal(r.reserves.length, 1, "no second intervention while still above the quiet line");
  // Quiet starts a dwell; it is not yet long enough and a spike resets it.
  r.setUsage(QUIET);
  r.monitor.tick();
  assert.equal(r.commands.admitsCommands(r.context), false);
  r.setUsage(HIGH);
  r.monitor.tick();
  await r.monitor.settle();
  assert.equal(r.reserves.length, 1, "a spike does not intervene while the guard is disarmed");
  r.setUsage(QUIET);
  r.monitor.tick();
  r.monitor.tick();
  assert.equal(r.commands.admitsCommands(r.context), false, "a dwell shorter than rearmMs does not rearm");
  await sleep(settings.rearmMs + 30);
  r.monitor.tick();
  assert.equal(r.commands.admitsCommands(r.context), true, "a full quiet dwell releases the latch");
  // Only now can pressure intervene again, on the sibling.
  r.setUsage(HIGH);
  r.monitor.tick();
  await until(() => r.reserves.length === 2, "second reservation");
  r.grant(1, 3);
  const second = await sibling.call;
  assert.equal(JSON.parse(r.text(second)).tool_id, "toolu_b");
  assert.equal(JSON.parse(r.text(second)).outcome, "confirmed_drained");
  await r.monitor.stop();
});

test("retry blocking: during a pressure latch or a phase hold new lane calls are refused; after rearm they are admitted", { timeout: 15000 }, async () => {
  const r = rig();
  const victim = r.start("a", 500);
  await r.enrolled(1);
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "reservation request");
  r.grant(0, 4);
  await victim.call;
  await r.monitor.settle();
  const launchesBefore = r.roots.size;
  const refusedByLatch = await r.callTool("sleep-retry", "toolu_retry");
  assert.equal(refusedByLatch.isError, true);
  assert.match(r.text(refusedByLatch), /worker_memory_pressure/);
  assert.equal(r.roots.size, launchesBefore, "a refused call launches nothing");
  // Rearm through a quiet dwell.
  r.setUsage(QUIET);
  r.monitor.tick();
  await sleep(settings.rearmMs + 30);
  r.monitor.tick();
  assert.equal(r.commands.admitsCommands(r.context), true);
  // A phase hold refuses too, and releasing it admits again.
  const hold = r.commands.holdPhase(r.context)!;
  const refusedByPhase = await r.callTool("sleep-phase", "toolu_phase");
  assert.match(r.text(refusedByPhase), /worker_memory_pressure/);
  assert.equal(r.roots.size, launchesBefore);
  assert.equal(r.commands.releasePhase(hold), true);
  const admitted = r.start("again", 100);
  await until(() => r.roots.has("again"), "the admitted launch");
  assert.equal(r.roots.size, launchesBefore + 1);
  await r.lane.quiesce(1000);
  await admitted.call;
  await r.monitor.stop();
});

test("unconfirmed drain: failed disposal prepares preservation, poisons the lane and trips the turn with GuardedCommandUnconfirmedError", { timeout: 15000 }, async () => {
  const r = rig();
  const victim = r.start("a", 500);
  const sibling = r.start("b", 100);
  await r.enrolled(2);
  r.roots.get("a")!.unconfirmed();
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "reservation request");
  r.grant(0, 4);
  const result = await victim.call;
  await r.monitor.settle();
  assert.equal(result.isError, true);
  assert.match(r.text(result), /cleanup could not be confirmed/);
  assert.equal(r.lane.poisoned, true);
  assert.equal(r.trips.length >= 1 && r.trips[0] instanceof GuardedCommandUnconfirmedError, true);
  assert.equal(r.kills.length >= 1, true, "the unreaped supervisor is killed");
  assert.equal(r.feedback[0]?.outcome, "unknown");
  assert.equal(r.feedback[0]?.cancellation_confirmed, false);
  assert.deepEqual(r.preparedPreservation, [r.context], "the owner hold is prepared for the captured context");
  assert.equal(r.monitor.state.unconfirmedDrain, true, "the global fence stays closed");
  assert.equal(r.commands.hasConfirmedStop(r.entryFor("toolu_a")), false);
  // No further victim: pressure persists but no second reservation is made, and the lane refuses.
  r.monitor.tick();
  await r.monitor.settle();
  assert.equal(r.reserves.length, 1);
  assert.match(r.text(await r.callTool("sleep-next", "toolu_next")), /Commands are unavailable/);
  assert.equal(r.roots.has("next"), false);
  // The sibling is released by the poisoning abort; its disposal is the lane's, not the guard's.
  await sibling.call;
  await r.monitor.stop();
});

test("repeated interventions: allowance exhaustion prepares preservation with the captured context and selects no further victim", { timeout: 15000 }, async () => {
  const r = rig({ maxInterventions: 2 });
  const first = r.start("a", 500);
  const second = r.start("b", 300);
  const survivor = r.start("c", 100);
  await r.enrolled(3);
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "first reservation");
  r.grant(0, 1);
  assert.equal(JSON.parse(r.text(await first.call)).tool_id, "toolu_a");
  await r.monitor.settle();
  assert.deepEqual(r.preparedPreservation, [], "allowance remains: nothing to preserve yet");
  r.setUsage(QUIET);
  r.monitor.tick();
  await sleep(settings.rearmMs + 30);
  r.monitor.tick();
  r.setUsage(HIGH);
  r.monitor.tick();
  await until(() => r.reserves.length === 2, "second reservation");
  r.grant(1, 0);
  const secondResult = JSON.parse(r.text(await second.call));
  await r.monitor.settle();
  assert.equal(secondResult.tool_id, "toolu_b");
  assert.equal(secondResult.outcome, "confirmed_drained");
  assert.deepEqual(r.preparedPreservation, [r.context], "the last allowance preserves the captured flight");
  assert.deepEqual(r.preserved, [r.context]);
  // The run is parked: pressure continues but no third victim is ever selected.
  r.monitor.tick();
  await r.monitor.settle();
  assert.equal(r.reserves.length, 2);
  assert.equal(r.roots.get("c")!.disposes(), 0, "the surviving command was never a victim");
  assert.equal(survivor.outcome.settled, false);
  await r.lane.quiesce(1000);
  await survivor.call;
  await r.monitor.stop();
});

test("the victim's tool result is the exact worker_memory_pressure JSON for its own toolUseId while a sibling call keeps running", { timeout: 15000 }, async () => {
  const r = rig();
  const sibling = r.start("sib", 100);
  const victim = r.start("vic", 500);
  await r.enrolled(2);
  const identity = identityOf(r, "toolu_vic");
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "reservation request");
  r.grant(0, 4);
  const result = await victim.call;
  assert.equal(result.isError, true);
  assert.deepEqual(JSON.parse(r.text(result)), expectedResult(identity, {
    intervention_id: r.reserves[0]!.request.intervention_id, tree_rss_bytes_approx: 500,
    outcome: "confirmed_drained", cancellation_confirmed: true,
  }));
  assert.equal(r.roots.get("sib")!.disposes(), 0);
  assert.equal(sibling.outcome.settled, false, "the sibling call has not been answered");
  assert.equal(r.lane.liveCount, 1);
  // The sibling finishes on its own and gets its own ordinary result, never the victim's.
  r.roots.get("sib")!.exitWith(0);
  const siblingResult = await sibling.call;
  assert.match(r.text(siblingResult), /^Exit code: 0/);
  assert.equal(siblingResult.isError, false);
  await r.monitor.stop();
});

test("one victim's drain is not whole-run quiescence: the sibling stays live and recorded until lane.quiesce drains it", { timeout: 15000 }, async () => {
  const r = rig();
  const victim = r.start("vic", 500);
  const sibling = r.start("sib", 100);
  await r.enrolled(2);
  r.monitor.tick();
  await until(() => r.reserves.length === 1, "reservation request");
  r.grant(0, 4);
  await victim.call;
  await r.monitor.settle();
  assert.equal(r.lane.liveCount, 1, "the other command is still live after the victim's confirmed drain");
  const recorded = r.lane.recordedRoots();
  assert.equal(recorded.length, 1, "exactly the sibling's supervisor remains recorded for the sinks");
  assert.equal(r.roots.get("sib")!.disposes(), 0);
  // Sinks may only run after the lane's own quiesce has drained what is left.
  await r.lane.quiesce(1000);
  assert.equal(r.roots.get("sib")!.disposes(), 1, "quiesce disposes the sibling");
  assert.equal(r.lane.liveCount, 0);
  assert.deepEqual(r.lane.recordedRoots(), []);
  const siblingResult = await sibling.call;
  assert.match(r.text(siblingResult), /cancelled/);
  assert.equal(r.lane.poisoned, false);
  await r.monitor.stop();
});
