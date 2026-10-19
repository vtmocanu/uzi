// Real-subprocess acceptance for the guarded Claude command lane (issue #2471, M4).
//
// Runs in the ROOT-START worker-uid container (worker uid 10001, UZI_UID_SPLIT=1, runner uid
// 10002, the image supervisor built from this tree). Every case drives the REAL ClaudeCommandLane
// with the REAL launcher and image supervisor and a REAL WorkerMemoryCommands context; each call
// goes through lane.hook (the attribution record) and then lane.handle, exactly as the SDK does.
// The model-facing batcher feed event is runner-level and is covered by agent unit tests; here
// the WorkerMemoryPressureResult fields are asserted against the entry identity instead.
//
// Not feasible in this image: a NON-DUMPABLE runner-uid descendant (the Alpine image ships no
// python3, C compiler or prctl helper, and node has no prctl binding), so the "non-dumpable
// descendant" scenario is proven with its nearest real equivalent, a SIGSTOPped supervisor
// (see the last-but-one case).
import assert from "node:assert/strict";
import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it } from "node:test";
import { enrollClaudeCommand, isMintedClaudeCommand, mintClaudeCommandMetadata } from "../../agent/src/claude-command/enroll.js";
import { guardedCommandEnv } from "../../agent/src/claude-command/env.js";
import { GUARDED_COMMAND_TOOL } from "../../agent/src/claude-command/grants.js";
import { ClaudeCommandLane, GuardedCommandUnconfirmedError, type ClaudeCommandLaneOptions } from "../../agent/src/claude-command/lane.js";
import type { CapturedMemoryContext } from "../../agent/src/codex/broker.js";
import { SUPERVISOR_BIN, killUnreapedSupervisor, launchRunnerCommandRoot } from "../../agent/src/codex/launcher.js";
import type { WorkerMemoryPressureResult } from "../../agent/src/protocol.js";
import { LiveAttemptRegistry, newRunAttempt, quiesceRunAttempt, setQuiescenceViewForTests } from "../../agent/src/run-quiescence.js";
import { runnerCommand } from "../../agent/src/runner-uid.js";
import { workerSpawnEnv } from "../../agent/src/worker-spawn-mark.js";
import { WorkerMemoryCommands } from "../../agent/src/worker-memory-commands.js";
// The worker-proof run preloads the hermetic (empty) quiescence view into every test process;
// the sibling-scan case clears it to scan the real procfs and puts it back afterwards.
import { restoreHermeticView } from "../../agent/test/setup/hermetic-proc.js";

// Required inventory must reject a skipped run: only the production worker with the split
// runner identity and the image supervisor can supply this evidence.
const INIT_SKIP = process.platform !== "linux" || process.getuid?.() !== 10001 ||
  process.env.UZI_UID_SPLIT !== "1"
  ? "requires Linux worker UID 10001 with the production runner 10002 split and the image supervisor"
  : false;

type Entry = NonNullable<ReturnType<WorkerMemoryCommands["eligibleEntries"]>>[number];
type Observation = Awaited<ReturnType<Entry["observe"]>>;
type HookArg = Parameters<ClaudeCommandLane["hook"]>[0];

const META = "claudecode/toolUseId";
const QUIET_LOG = { debug() {}, info() {}, warn() {} };
const CLI_ENV = { PATH: "/usr/local/bin:/usr/bin:/bin", LANG: "C" } as const;
const HELPER_ENV = workerSpawnEnv({ PATH: "/usr/bin:/bin", LANG: "C" });
const sleep = (ms: number) => new Promise<void>((done) => setTimeout(done, ms));

/** Run a short helper at the runner uid (the only uid that may signal runner processes). */
function runnerRun(command: string, args: readonly string[]) {
  const wrapped = runnerCommand(command, args);
  return spawnSync(wrapped.command, wrapped.args, { env: HELPER_ENV, encoding: "utf8", timeout: 10_000 });
}
const alive = (pid: number) => runnerRun("kill", ["-0", String(pid)]).status === 0;

/** Poll until `predicate` holds; at most `ms` (50 ms steps). A failure stops this case only. */
async function until(predicate: () => boolean, what: string, ms = 15_000) {
  for (let waited = 0; waited <= ms; waited += 50) {
    if (predicate()) return;
    await sleep(50);
  }
  assert.fail(`fixture ${what}: ${String(ms)} ms readiness deadline`);
}
async function untilGone(pids: readonly number[], what: string, ms = 8000) {
  await until(() => pids.every((pid) => !alive(pid)), `${what} (processes still alive: ${pids.join(",")})`, ms);
}

function newContext(commands: WorkerMemoryCommands): CapturedMemoryContext {
  const flight = {};
  const context = commands.begin({
    run_id: randomUUID(), worker_id: randomUUID(), register_nonce: "claude-real",
    claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1,
  });
  return { commands, context, flight, lifecycleRevision: 1 };
}

interface Rig {
  readonly dir: string;
  readonly lanes: ClaudeCommandLane[];
  readonly pids: Set<number>;
  readonly children: ChildProcess[];
}
function newRig(): Rig {
  const dir = fs.mkdtempSync(path.join("/tmp", "uzi-claude-real-"));
  fs.chmodSync(dir, 0o777);
  return { dir, lanes: [], pids: new Set(), children: [] };
}
/** Always runs: quiesces lanes, kills what the case recorded, removes the runner-owned tree. */
async function cleanRig(rig: Rig) {
  for (const lane of rig.lanes) await lane.quiesce(15_000).catch(() => undefined);
  for (const child of rig.children) { try { child.kill("SIGKILL"); } catch { /* already gone */ } }
  if (rig.pids.size > 0) runnerRun("kill", ["-KILL", ...[...rig.pids].map(String)]);
  // The wrapper made `.uzi` at the runner uid inside this worker-owned dir: remove it as the runner.
  runnerRun("rm", ["-rf", "--", path.join(rig.dir, ".uzi")]);
  fs.rmSync(rig.dir, { recursive: true, force: true });
}

function newLane(rig: Rig, captured: CapturedMemoryContext, over: Partial<ClaudeCommandLaneOptions> = {}) {
  const trips: Error[] = [];
  const lane = new ClaudeCommandLane({
    worktree: rig.dir, cliEnv: CLI_ENV, secretPaths: [], dockerWired: false, secrets: [], log: QUIET_LOG,
    capturedMemory: () => captured, tripTurn: (error) => { trips.push(error); },
    graceMs: 1000, cleanupMs: 5000, ...over,
  });
  lane.beginTurn();
  rig.lanes.push(lane);
  return { lane, trips };
}

let seq = 0;
/** One model call: the PreToolUse hook (attribution record), then the MCP handler. */
function startCall(lane: ClaudeCommandLane, rig: Rig, args: Record<string, unknown>, signal?: AbortSignal) {
  const id = `toolu_real_${String(++seq)}_${randomUUID().slice(0, 8)}`;
  const pending = (async () => {
    const hook = await lane.hook({
      hook_event_name: "PreToolUse", tool_name: GUARDED_COMMAND_TOOL, tool_input: args, tool_use_id: id,
      session_id: "s", transcript_path: "/t", cwd: rig.dir,
    } as HookArg, id, { signal: new AbortController().signal });
    assert.deepEqual(hook, {}, "the hook admits the call");
    return lane.handle(args, { _meta: { [META]: id }, signal });
  })();
  void pending.catch(() => undefined);
  return { id, pending };
}
const textOf = (result: { content: { type: string; text?: string }[] }) => result.content.map((block) => block.text ?? "").join("");

async function enrolled(commands: WorkerMemoryCommands, count: number): Promise<readonly Entry[]> {
  await until(() => commands.eligibleEntries()?.length === count, `${String(count)} enrolled command(s)`);
  return commands.eligibleEntries()!;
}
const ready = (rig: Rig, name: string) => until(() => fs.existsSync(path.join(rig.dir, name)), `marker ${name}`);

/** A fresh complete observation of one enrolled command (a single observe may be in flight: retry). */
async function observeFresh(entry: Entry): Promise<Observation> {
  for (let attempt = 0; attempt < 40; attempt++) {
    const observation = await entry.observe({ remainingMs: 5000, signal: new AbortController().signal });
    if (observation.available && observation.complete && observation.rows.length > 0) return observation;
    await sleep(100);
  }
  return assert.fail("no complete observation of the enrolled command");
}
const treePids = (observation: Observation) => observation.rows.map((row) => row.pid);
const WINDOW_MS = 5000;

/** The guard's own sequence: fresh observe, select, fresh observe, checkBeforeCancel, then cancel. */
async function guardCancel(commands: WorkerMemoryCommands, entry: Entry) {
  const observation = await observeFresh(entry);
  const selection = commands.select([{ entry, observation }], performance.now(), WINDOW_MS);
  assert.ok(selection, "the guard selects the enrolled command");
  assert.equal(selection.entry, entry);
  const fresh = await observeFresh(entry);
  assert.equal(commands.checkBeforeCancel(selection, fresh, performance.now(), WINDOW_MS), true);
  await entry.cancel({ remainingMs: 8000, signal: new AbortController().signal, authorized: () => true });
}

function pressureFor(entry: Entry): WorkerMemoryPressureResult {
  return {
    run_id: entry.context.run_id, worker_id: entry.context.worker_id, claim_generation: entry.context.claim_generation,
    memory_episode: entry.context.memory_episode, intervention_id: randomUUID(),
    tool_id: entry.toolId, attempt_id: entry.attemptId, command_id: entry.commandId, command: entry.command,
    code: "worker_memory_pressure", observed_total_bytes: 900, limit_bytes: 1000, tree_rss_bytes_approx: 100,
    outcome: "confirmed_drained", cancellation_confirmed: true, retry_blocked: true,
    guidance: "Use smaller or serial commands after recovery.",
  };
}

// A node holding ~100 MiB resident at the runner uid, announcing itself with a marker file.
const hog = (marker: string) => `node -e 'const b = Buffer.alloc(100 * 1024 * 1024, 1); require("fs").writeFileSync("${marker}", String(b.length)); setTimeout(() => {}, 120000)'`;

describe("real Claude guarded command lane", { skip: INIT_SKIP }, () => {
  it("stops a detached command and its descendants through a guard-style cancel and confirms the drain", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      const commands = new WorkerMemoryCommands();
      const captured = newContext(commands);
      const { lane, trips } = newLane(rig, captured);
      const { pending } = startCall(lane, rig, {
        command: 'echo "main=$$"; setsid sleep 300 & echo "setsid=$!"; nohup sleep 300 >/dev/null 2>&1 & echo "nohup=$!"; touch ready; sleep 280',
        timeout: 90_000,
      });
      await ready(rig, "ready");
      const [entry] = await enrolled(commands, 1);
      const before = treePids(await observeFresh(entry!));
      for (const pid of before) rig.pids.add(pid);
      assert.ok(before.length >= 3, `the enrolled tree includes the foreground command and both detached sleeps: ${before.join(",")}`);

      await guardCancel(commands, entry!);
      assert.equal(commands.hasConfirmedStop(entry!), true);
      const result = await pending;
      const output = textOf(result);
      assert.match(output, /^Exit code: \d+/);
      assert.equal(result.isError, true);
      const detached = [/setsid=(\d+)/, /nohup=(\d+)/].map((pattern) => Number(pattern.exec(output)?.[1]));
      for (const pid of detached) {
        assert.ok(Number.isInteger(pid) && pid > 1, `the command echoed its detached pid: ${output}`);
        assert.ok(before.includes(pid), `the detached pid ${String(pid)} was inside the enrolled supervisor tree`);
      }
      await untilGone([...before, ...detached], "every process of the cancelled tree");
      assert.equal(lane.liveCount, 0);
      assert.equal(lane.poisoned, false);
      assert.deepEqual(trips, []);
      assert.equal(commands.eligibleEntries()?.length, 0, "the confirmed drain unregistered the command");
    } finally { await cleanRig(rig); }
  });

  it("expires a short explicit deadline on a command with a detached descendant and drains the whole tree", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      const commands = new WorkerMemoryCommands();
      const { lane, trips } = newLane(rig, newContext(commands));
      const startedAt = Date.now();
      const { pending } = startCall(lane, rig, {
        command: 'echo "main=$$"; setsid sleep 300 & echo "setsid=$!"; nohup sleep 300 >/dev/null 2>&1 & echo "nohup=$!"; touch ready; sleep 280',
        timeout: 3000,
      });
      await ready(rig, "ready");
      const [entry] = await enrolled(commands, 1);
      const before = treePids(await observeFresh(entry!));
      for (const pid of before) rig.pids.add(pid);
      assert.ok(before.length >= 3, `the enrolled tree includes the foreground command and both detached sleeps: ${before.join(",")}`);

      const result = await pending;
      const elapsed = Date.now() - startedAt;
      const output = textOf(result);
      assert.equal(result.isError, true);
      assert.match(output, /Command timed out after 3000 ms; it and all its descendants were stopped\./);
      assert.ok(elapsed >= 2900, `the lane waited for its own deadline, not less: ${String(elapsed)} ms`);
      assert.ok(elapsed < 3000 + 1000 + 5000 + 10_000, `the deadline was enforced within the lane's bound: ${String(elapsed)} ms`);
      const detached = [/setsid=(\d+)/, /nohup=(\d+)/].map((pattern) => Number(pattern.exec(output)?.[1]));
      for (const pid of detached) {
        assert.ok(Number.isInteger(pid) && pid > 1, `the command echoed its detached pid: ${output}`);
        assert.ok(before.includes(pid), `the detached pid ${String(pid)} was inside the enrolled supervisor tree`);
      }
      await untilGone([...before, ...detached], "every process of the timed-out tree");
      assert.equal(lane.liveCount, 0);
      assert.equal(lane.poisoned, false);
      assert.deepEqual(trips, []);
      assert.equal(commands.eligibleEntries()?.length, 0, "the deadline drain unregistered the command");
    } finally { await cleanRig(rig); }
  });

  it("cancelling the selected run's command leaves a sibling run's command alive until it completes", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      // One worker-wide registry, two flights (two run ids), one lane per flight.
      const commands = new WorkerMemoryCommands();
      const capturedA = newContext(commands);
      const capturedB = newContext(commands);
      assert.notEqual(capturedA.context.run_id, capturedB.context.run_id);
      const laneA = newLane(rig, capturedA).lane;
      const laneB = newLane(rig, capturedB).lane;
      const callA = startCall(laneA, rig, { command: hog("ready-a"), timeout: 90_000 });
      const callB = startCall(laneB, rig, { command: "touch ready-b; sleep 8; echo sibling-done", timeout: 90_000 });
      await ready(rig, "ready-a");
      await ready(rig, "ready-b");
      const entries = await enrolled(commands, 2);
      const entryA = entries.find((entry) => entry.context === capturedA.context)!;
      const entryB = entries.find((entry) => entry.context === capturedB.context)!;
      assert.ok(entryA && entryB);
      const observationA = await observeFresh(entryA);
      const observationB = await observeFresh(entryB);
      const siblingPids = treePids(observationB);
      for (const pid of [...treePids(observationA), ...siblingPids]) rig.pids.add(pid);
      const selection = commands.select([{ entry: entryA, observation: observationA }, { entry: entryB, observation: observationB }],
        performance.now(), WINDOW_MS);
      assert.equal(selection?.entry, entryA, "the larger tree (run A) is the guard's pick");

      await guardCancel(commands, entryA);
      const resultA = await callA.pending;
      assert.match(textOf(resultA), /^Exit code: \d+/);
      assert.equal(commands.hasConfirmedStop(entryB), false);
      assert.ok(siblingPids.every((pid) => alive(pid)), "the sibling run's command survived the other run's cancel");
      assert.equal(laneB.liveCount, 1);
      const resultB = await callB.pending;
      assert.equal(resultB.isError, false);
      assert.match(textOf(resultB), /^Exit code: 0\n/);
      assert.match(textOf(resultB), /sibling-done/);
    } finally { await cleanRig(rig); }
  });

  it("lets another attempt's capture scan skip the live command's recorded root while the owner's own reap still kills it", { timeout: 90_000 }, async () => {
    const rig = newRig();
    const cloneB = path.join(fs.mkdtempSync(path.join("/tmp", "uzi-claude-real-b-")), "runner", "github.com+o+r", "issue-2");
    fs.mkdirSync(cloneB, { recursive: true });
    // The real runner-uid quiescence helper reads the real procfs: clear the hermetic empty view.
    setQuiescenceViewForTests(undefined);
    try {
      const commands = new WorkerMemoryCommands();
      const { lane } = newLane(rig, newContext(commands));
      const { pending } = startCall(lane, rig, { command: "setsid sleep 300 & touch ready; sleep 280", timeout: 90_000 });
      await ready(rig, "ready");
      const [entry] = await enrolled(commands, 1);
      const tree = treePids(await observeFresh(entry!));
      for (const pid of tree) rig.pids.add(pid);
      const supervisorPid = entry!.root.pid;
      rig.pids.add(supervisorPid);
      // The lane's recorded roots are what SdkExecutor.recordedRootPids() unions into its attempt.
      assert.deepEqual(lane.recordedRoots().map((root) => root.pid), [supervisorPid]);
      assert.ok(Number.isSafeInteger(lane.recordedRoots()[0]!.startTime));

      const runA = randomUUID();
      const attemptB = newRunAttempt(randomUUID(), 1, cloneB, () => []);
      const sibling = async (recorded: () => ReturnType<ClaudeCommandLane["recordedRoots"]>) => {
        const registry = new LiveAttemptRegistry();
        registry.add(newRunAttempt(runA, 1, rig.dir, recorded));
        registry.add(attemptB);
        return quiesceRunAttempt({
          mode: "capture", attempt: attemptB, cloneKey: attemptB.cloneKey, targetPaths: [attemptB.clonePath],
          processes: true, dockerHost: undefined, registry, otherClaimInFlight: true,
        });
      };
      // Control: with no recorded root the real scan does see the live supervisor and cannot attribute it.
      const control = await sibling(() => []);
      assert.equal(control.process?.state, "unverified", control.process?.detail);
      assert.ok(control.process?.processes.some((p) => p.pid === supervisorPid && p.reason === "unreadable_unattributed"),
        `the unrecorded supervisor is reported: ${JSON.stringify(control.process?.processes)}`);
      assert.deepEqual(control.process?.killed, []);
      // The lane's recorded root attributes it to the live sibling: the capture proof is clean and signals nothing.
      const clean = await sibling(() => lane.recordedRoots());
      assert.equal(clean.process?.state, "quiescent", clean.process?.detail);
      assert.deepEqual(clean.process?.killed, []);
      assert.ok(tree.every((pid) => alive(pid)), "the sibling scan did not touch the live command");
      assert.equal(lane.liveCount, 1);

      // The record exempts nothing from the owner's own reap: its teardown excludes its own roots and kills the tree.
      const registryA = new LiveAttemptRegistry();
      const attemptA = newRunAttempt(runA, 1, rig.dir, () => lane.recordedRoots());
      registryA.add(attemptA);
      const own = await quiesceRunAttempt({
        mode: "own", attempt: attemptA, cloneKey: attemptA.cloneKey, targetPaths: [rig.dir],
        processes: true, dockerHost: undefined, registry: registryA, otherClaimInFlight: false,
      });
      assert.equal(own.process?.state, "quiescent", own.process?.detail);
      assert.ok(own.process?.killed.includes(supervisorPid), `the own reap killed the recorded supervisor: ${JSON.stringify(own.process)}`);
      await untilGone([supervisorPid], "the supervisor killed by the own-run reap");
      await pending;
    } finally {
      restoreHermeticView();
      await cleanRig(rig);
      fs.rmSync(path.dirname(path.dirname(path.dirname(cloneB))), { recursive: true, force: true });
    }
  });

  it("rejects a candidate observation whose root start time differs from the enrolled one and cancels nothing", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      const commands = new WorkerMemoryCommands();
      const { lane } = newLane(rig, newContext(commands));
      const ac = new AbortController();
      const { pending } = startCall(lane, rig, { command: "touch ready; sleep 120", timeout: 90_000 }, ac.signal);
      await ready(rig, "ready");
      const [entry] = await enrolled(commands, 1);
      const observation = await observeFresh(entry!);
      for (const pid of treePids(observation)) rig.pids.add(pid);
      const reused = (value: { pid: number; startTime: string }) => ({ ...value, startTime: String(BigInt(value.startTime) + 1n) });
      const now = performance.now();
      const badPrimary: Observation = { ...observation, primary: reused(observation.primary!) };
      const badSupervisor: Observation = { ...observation, supervisor: reused(observation.supervisor!) };
      assert.equal(commands.select([{ entry: entry!, observation: badPrimary }], now, WINDOW_MS), undefined);
      assert.equal(commands.select([{ entry: entry!, observation: badSupervisor }], now, WINDOW_MS), undefined);
      const good = commands.select([{ entry: entry!, observation }], performance.now(), WINDOW_MS);
      assert.ok(good, "the unmodified observation is selectable");
      assert.equal(commands.checkBeforeCancel(good, badPrimary, performance.now(), WINDOW_MS), false);
      assert.equal(commands.checkBeforeCancel(good, badSupervisor, performance.now(), WINDOW_MS), false);
      // Nothing was cancelled: the command is still enrolled, running and unconfirmed.
      assert.equal(commands.hasConfirmedStop(entry!), false);
      assert.equal(lane.liveCount, 1);
      assert.ok(treePids(observation).every((pid) => alive(pid)));
      ac.abort();
      assert.match(textOf(await pending), /^Command was cancelled/);
      await untilGone(treePids(observation), "the aborted command's tree");
    } finally { await cleanRig(rig); }
  });

  it("never selects a larger unattributed process tree", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      // A ~200 MiB process started outside the lane, as the worker uid, never enrolled.
      const big = spawn(process.execPath, ["-e", "const b = Buffer.alloc(200 * 1024 * 1024, 1); setInterval(() => b[0]++, 1000);"], { stdio: "ignore" });
      rig.children.push(big);
      const bigPid = big.pid!;
      const residentBytes = () => {
        try { return Number(fs.readFileSync(`/proc/${String(bigPid)}/statm`, "utf8").split(" ")[1]) * 4096; } catch { return 0; }
      };
      await until(() => residentBytes() > 150 * 1024 * 1024, "unattributed process residency");

      const commands = new WorkerMemoryCommands();
      const { lane } = newLane(rig, newContext(commands));
      const ac = new AbortController();
      const { pending } = startCall(lane, rig, { command: "touch ready; sleep 120", timeout: 90_000 }, ac.signal);
      await ready(rig, "ready");
      const entries = await enrolled(commands, 1);
      assert.equal(entries.length, 1, "only the lane's own command is eligible");
      const observation = await observeFresh(entries[0]!);
      for (const pid of treePids(observation)) rig.pids.add(pid);
      assert.ok(!treePids(observation).includes(bigPid), "the unattributed process is not in the enrolled tree");
      const enrolledBytes = observation.rows.reduce((sum, row) => sum + row.rssBytes, 0);
      assert.ok(residentBytes() > enrolledBytes, "the unattributed tree is the larger one");
      const selection = commands.select([{ entry: entries[0]!, observation }], performance.now(), WINDOW_MS);
      assert.equal(selection?.entry, entries[0], "the guard selects only the enrolled smaller command");
      assert.equal(selection?.rssBytesApprox, enrolledBytes);
      ac.abort();
      await pending;
      assert.equal(alive(bigPid), true, "the unattributed process was never touched");
    } finally { await cleanRig(rig); }
  });

  it("returns the exact worker_memory_pressure result as the selected command's tool error", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      const commands = new WorkerMemoryCommands();
      const captured = newContext(commands);
      const { lane, trips } = newLane(rig, captured);
      const { id, pending } = startCall(lane, rig, { command: "touch ready; sleep 120", timeout: 90_000 });
      await ready(rig, "ready");
      const [entry] = await enrolled(commands, 1);
      for (const pid of treePids(await observeFresh(entry!))) rig.pids.add(pid);
      // The guard prepares the outcome before it cancels, then resolves it with its result.
      commands.prepareOutcome(entry!);
      const pressure = pressureFor(entry!);
      await guardCancel(commands, entry!);
      assert.equal(commands.hasConfirmedStop(entry!), true);
      assert.equal(commands.resolveToolOutcome(entry!, pressure), true);
      const result = await pending;
      assert.equal(result.isError, true);
      assert.equal(textOf(result), JSON.stringify(pressure));
      // The result belongs to this exact call. (The batcher feed event built from these same
      // fields is runner-level and is covered by the agent tests, not repeated here.)
      const parsed = JSON.parse(textOf(result)) as WorkerMemoryPressureResult;
      assert.equal(parsed.tool_id, id);
      assert.equal(parsed.attempt_id, entry!.attemptId);
      assert.equal(parsed.command_id, entry!.commandId);
      assert.equal(parsed.run_id, captured.context.run_id);
      assert.equal(parsed.worker_id, captured.context.worker_id);
      assert.equal(parsed.claim_generation, captured.context.claim_generation);
      assert.equal(parsed.memory_episode, captured.context.memory_episode);
      assert.equal(parsed.code, "worker_memory_pressure");
      assert.equal(parsed.cancellation_confirmed, true);
      assert.equal(lane.poisoned, false);
      assert.deepEqual(trips, []);
    } finally { await cleanRig(rig); }
  });

  it("refuses a registration whose source is not one of the two model-command literals", { timeout: 30_000 }, () => {
    const commands = new WorkerMemoryCommands();
    const { context } = newContext(commands);
    const base = {
      toolId: "tool", attemptId: "attempt", command: "echo", root: { pid: 4242, startTime: "100" },
      observe: async () => ({ available: false, complete: false, startedAtMs: 0, completedAtMs: 0, rows: [] }),
      cancel: async () => undefined, drain: async () => ({ confirmed: false }),
    };
    for (const source of ["bash", "codex", "claude", "claude-model-command ", "CLAUDE-MODEL-COMMAND", "", "provider", undefined]) {
      assert.throws(() => commands.register(context, { ...base, source: source as never }), /untrusted memory command/, String(source));
    }
    assert.equal(commands.eligibleEntries()?.length, 0);
    for (const source of ["claude-model-command", "codex-model-command"] as const) {
      const entry = commands.register(context, { ...base, source });
      assert.equal(entry.source, source);
      assert.equal(commands.recordDrain(entry, { confirmed: true }), true);
      assert.equal(commands.unregister(entry), true);
    }
  });

  it("never enrolls unminted, cloned or foreign-context metadata, yet the command still runs", { timeout: 60_000 }, async () => {
    const rig = newRig();
    let handle: Awaited<ReturnType<typeof launchRunnerCommandRoot>>["handle"] | undefined;
    try {
      const commands = new WorkerMemoryCommands();
      const captured = newContext(commands);
      const other = newContext(commands);
      const launched = await launchRunnerCommandRoot({
        command: "sleep 60", cwd: rig.dir, env: guardedCommandEnv(CLI_ENV),
        spillPath: path.join(rig.dir, ".uzi/scratch/uzi-cmd/forged.log"), spillCapBytes: 1024, supervisorBin: SUPERVISOR_BIN,
        onSpawned: (root) => { rig.pids.add(root.pid); },
      }, { deadlines: { started: 10_000 } });
      handle = launched.handle;
      const fields = { toolId: "toolu_forged", attemptId: "attempt-forged", commandId: randomUUID(), command: "sleep 60" };
      const minted = mintClaudeCommandMetadata(captured, fields);
      const forgeries = [
        { name: "plain object", metadata: { ...fields } },
        { name: "cloned minted metadata", metadata: Object.freeze({ ...minted }) },
        { name: "undefined", metadata: undefined },
      ];
      for (const { name, metadata } of forgeries) {
        assert.equal(isMintedClaudeCommand(metadata), false, name);
        assert.equal(await enrollClaudeCommand(captured, metadata, handle, 5000), undefined, name);
      }
      // Genuinely minted, but for a different captured context: never enrolled into this one.
      assert.equal(isMintedClaudeCommand(minted, other), false);
      assert.equal(await enrollClaudeCommand(other, minted, handle, 5000), undefined);
      assert.equal(commands.eligibleEntries()?.length, 0, "nothing was enrolled");
      assert.equal(launched.child.exitCode, null, "the command it describes is still running, only unprotected by the guard");
      assert.equal(alive(launched.child.pid!), true);
    } finally {
      const disposed = await handle?.dispose(10_000).catch(() => undefined);
      await cleanRig(rig);
      if (handle) assert.equal(disposed?.clean, true, "the forged-metadata root must drain");
    }
  });

  it("gives a command that declares its own worker marks no protection and no special enrollment", { timeout: 60_000 }, async () => {
    const rig = newRig();
    try {
      const commands = new WorkerMemoryCommands();
      const captured = newContext(commands);
      const { lane } = newLane(rig, captured);
      const { id, pending } = startCall(lane, rig, {
        command: 'export UZI_WORKER_SPAWN=forged-nonce UZI_RUN_ATTEMPT=forged:forged UZI_RUN_CLONE=/forged UZI_RUN_CLONE_KEY=/forged/key; echo "mark=$UZI_WORKER_SPAWN"; touch ready; sleep 120',
        timeout: 90_000,
      });
      await ready(rig, "ready");
      const entries = await enrolled(commands, 1);
      const entry = entries[0]!;
      // The registry still holds exactly its own normal entry: the forged names are not an identity.
      assert.equal(entry.toolId, id);
      assert.equal(entry.source, "claude-model-command");
      assert.ok(!entry.attemptId.includes("forged"));
      const observation = await observeFresh(entry);
      for (const pid of treePids(observation)) rig.pids.add(pid);
      // Selectable and cancellable exactly like any other command: nothing exempted it.
      await guardCancel(commands, entry);
      const result = await pending;
      assert.match(textOf(result), /mark=forged-nonce/, "the command really ran with its self-declared mark");
      assert.equal(commands.hasConfirmedStop(entry), true);
      await untilGone(treePids(observation), "the self-marked command's tree");
    } finally { await cleanRig(rig); }
  });

  it("poisons the lane and kills the exact supervisor when a stopped supervisor cannot confirm disposal", { timeout: 90_000 }, async (t) => {
    // The nearest real equivalent of a non-dumpable descendant that defeats observation:
    // the supervisor itself is SIGSTOPped, so its dispose can never be confirmed.
    const rig = newRig();
    try {
      const commands = new WorkerMemoryCommands();
      const killed: Array<{ pid: number | undefined; reaped: boolean }> = [];
      const { lane, trips } = newLane(rig, newContext(commands), {
        cleanupMs: 2000,
        kill: (child) => {
          killed.push({ pid: child.pid, reaped: child.exitCode !== null || child.signalCode !== null });
          return killUnreapedSupervisor(child);
        },
      });
      const ac = new AbortController();
      const { pending } = startCall(lane, rig, { command: "setsid sleep 300 & touch ready; sleep 280", timeout: 90_000 }, ac.signal);
      await ready(rig, "ready");
      const [entry] = await enrolled(commands, 1);
      const supervisorPid = entry!.root.pid;
      const tree = treePids(await observeFresh(entry!));
      for (const pid of [supervisorPid, ...tree]) rig.pids.add(pid);

      assert.equal(runnerRun("kill", ["-STOP", String(supervisorPid)]).status, 0);
      ac.abort();
      const result = await pending;
      assert.equal(result.isError, true);
      assert.match(textOf(result), /cleanup could not be confirmed/);
      assert.equal(lane.poisoned, true);
      assert.equal(trips.length, 1);
      assert.ok(trips[0] instanceof GuardedCommandUnconfirmedError);
      assert.equal(commands.hasConfirmedStop(entry!), false);
      assert.equal(commands.drainFailure(entry!), "supervisor disposal unconfirmed");
      // The exact-identity kill ran against the still-unreaped supervisor and it is gone.
      assert.deepEqual(killed, [{ pid: supervisorPid, reaped: false }]);
      await untilGone([supervisorPid], "the SIGSTOPped supervisor after the exact-identity SIGKILL");
      // A poisoned lane refuses every later command and rethrows at the join.
      const later = await startCall(lane, rig, { command: "echo never", timeout: 5000 }).pending;
      assert.equal(later.isError, true);
      assert.match(textOf(later), /Commands are unavailable/);
      await assert.rejects(lane.quiesce(2000), GuardedCommandUnconfirmedError);
      // The supervisor is the tree's only reaper and has no parent-death signal, so processes it
      // adopted may outlive a supervisor killed this way. Report it rather than assert it; the
      // rig kills any survivor by its recorded pid.
      await sleep(500);
      t.diagnostic(`descendants alive after the supervisor kill: ${tree.filter((pid) => pid !== supervisorPid && alive(pid)).join(",") || "none"}`);
    } finally { await cleanRig(rig); }
  });

  it("keeps native Bash when the guard is off", { timeout: 30_000 }, async () => {
    // Pure option assembly; the CLI is not started. Guard-off native Bash EXECUTION is covered by
    // the M0 CONTROL scenario (test/fixtures/claude-command-m0-long-call.md and the manual
    // long-call harness), which runs the real CLI without the alias.
    const { buildSdkOptions } = await import("../../agent/src/claude-harness.js");
    const base = {
      cwd: "/tmp", env: {}, skillsPluginPath: "/tmp/uzi-skills", skills: [], systemPrompt: "prompt", agents: {},
      mcpServers: {}, preToolUse: [],
    };
    const off = buildSdkOptions(base);
    assert.equal(off.toolAliases, undefined);
    assert.equal("toolAliases" in off, false);
    assert.ok(Array.isArray(off.disallowedTools));
    assert.equal(off.disallowedTools.includes("Bash"), false);
    const on = buildSdkOptions({ ...base, guardedCommand: true });
    assert.deepEqual(on.toolAliases, { Bash: GUARDED_COMMAND_TOOL });
    assert.ok(on.disallowedTools?.includes("Bash"));
  });
});
