import assert from "node:assert/strict";
import { test } from "node:test";
import { PassThrough } from "node:stream";
import path from "node:path";
import type { HookInput } from "@anthropic-ai/claude-agent-sdk";
import type { CapturedMemoryContext } from "../src/codex/broker.js";
import {
  RunnerCommandNotStartedError, SUPERVISOR_BIN,
  type CodexRootHandle, type ObservationOutcome, type RunnerCommandChild, type RunnerCommandLaunchSpec,
} from "../src/codex/launcher.js";
import { GUARDED_COMMAND_TOOL } from "../src/claude-command/grants.js";
import {
  ClaudeCommandLane, GuardedCommandUnconfirmedError, type ClaudeCommandLaneOptions,
} from "../src/claude-command/lane.js";
import { GUARDED_COMMAND_DESCRIPTION, GUARDED_SERVER_TIMEOUT_MS, buildGuardedCommandServer } from "../src/claude-command/server.js";
import type { WorkerMemoryPressureResult } from "../src/protocol.js";
import { WorkerMemoryCommands } from "../src/worker-memory-commands.js";

const WT = path.resolve(".");
const ticks = "18446744073709551615";
const META = "claudecode/toolUseId";
const log = { debug() {}, info() {}, warn() {} };
// A command the shell screen denies (it reads the process environment).
const DENIED = "printenv";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}
async function until(predicate: () => boolean, what = "readiness") {
  // At most 300 attempts / three seconds; a failure stops this test only.
  for (let i = 0; i < 300; i++) {
    if (predicate()) return;
    await new Promise((done) => setTimeout(done, 10));
  }
  assert.fail(`fixture ${what} deadline`);
}
function captured(): CapturedMemoryContext {
  const commands = new WorkerMemoryCommands(["private-fixture"]);
  const flight = {};
  const context = commands.begin({
    run_id: "11111111-1111-4111-8111-111111111111", worker_id: "22222222-2222-4222-8222-222222222222",
    register_nonce: "nonce", claim_generation: 1, memory_episode: 0, flight, lifecycleRevision: 1,
  });
  return { commands, context, flight, lifecycleRevision: 1 };
}

/** A controllable supervisor root: exit, output and disposal are driven by the test. */
function fakeRoot(opts: { supervisorPid?: number } = {}) {
  const sup = opts.supervisorPid ?? 50;
  const exit = deferred<{ event: "child_exit"; code: number }>();
  const stdout = new PassThrough();
  const stderr = new PassThrough();
  const events: string[] = [];
  let disposes = 0;
  let clean = true;
  let holdOutput = false;
  let exited = false;
  let lastWaitMs: number | undefined;
  let evidence: ObservationOutcome = {
    event: "observe", id: 1, state: "complete", supervisor: { pid: sup, startTime: ticks },
    root: { pid: sup + 1, startTime: ticks }, processes: [{ pid: sup + 1, startTime: ticks, ppid: sup, rss: 100 }],
  };
  const exitWith = (code: number) => {
    if (!exited) { exited = true; exit.resolve({ event: "child_exit", code }); }
    if (!holdOutput) { stdout.end(); stderr.end(); }
  };
  const handle: CodexRootHandle = {
    started: { event: "started", supervisorPid: sup, childPid: sup + 1, uid: 10002, subreaper: true,
      nondumpable: true, liveCapsZero: false, capBoundingSet: "0xc0", noNewPrivs: true },
    supervisorPid: sup, transport: { stdin: new PassThrough(), stdout, stderr },
    failed: undefined, whenFailed: new Promise(() => {}),
    snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
    observe: async () => evidence,
    waitChild: async (ms) => { lastWaitMs = ms; return exit.promise; },
    dispose: async () => {
      disposes++;
      events.push("dispose");
      stdout.end(); stderr.end();
      exitWith(137);
      return clean ? { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }
        : { clean: false, reason: "fixture unconfirmed" };
    },
  };
  const child = { pid: sup, exitCode: null, signalCode: null, kill: () => true } as unknown as RunnerCommandChild;
  return {
    handle, child, events, stdout, stderr, exitWith,
    disposes: () => disposes, waitMs: () => lastWaitMs,
    unconfirmed: () => { clean = false; }, holdOutput: () => { holdOutput = true; },
    unavailable: () => { evidence = { event: "observe", id: 1, state: "unavailable", reason: "unreadable" }; },
  };
}
type Root = ReturnType<typeof fakeRoot>;

interface Harness {
  lane: ClaudeCommandLane;
  c: CapturedMemoryContext;
  trips: Error[];
  specs: RunnerCommandLaunchSpec[];
  roots: Root[];
  kills: RunnerCommandChild[];
  script: { current: (root: Root, spec: RunnerCommandLaunchSpec) => void };
  call: (args: Record<string, unknown>, id?: string, signal?: AbortSignal) => ReturnType<ClaudeCommandLane["handle"]>;
}
let seq = 0;
function harness(over: Partial<ClaudeCommandLaneOptions> = {}, startTurn = true): Harness {
  const c = captured();
  const trips: Error[] = [];
  const specs: RunnerCommandLaunchSpec[] = [];
  const roots: Root[] = [];
  const kills: RunnerCommandChild[] = [];
  // Default: the command finishes by itself right after launch.
  const script: Harness["script"] = { current: (root: Root) => { setImmediate(() => root.exitWith(0)); } };
  const lane = new ClaudeCommandLane({
    worktree: WT,
    cliEnv: { PATH: "/usr/bin", HOME: "/home/runner", UZI_WORKER_TOKEN: "w", UZI_WORKER_TOKEN_FILE: "/f", UZI_API_URL: "http://api",
      ANTHROPIC_API_KEY: "k", CLAUDE_CODE_OAUTH_TOKEN: "o", UZI_RUN_ATTEMPT: "attempt-1" },
    secretPaths: [], dockerWired: false, secrets: ["private-fixture"], log,
    capturedMemory: () => c, tripTurn: (error) => { trips.push(error); },
    kill: (child) => { kills.push(child); return true; },
    graceMs: 30, cleanupMs: 300,
    launch: async (spec) => {
      specs.push(spec);
      const root = fakeRoot({ supervisorPid: 50 + roots.length * 10 });
      roots.push(root);
      spec.onSpawned?.({ pid: root.child.pid as number, startTime: 4242 + roots.length }, root.child);
      script.current(root, spec);
      return { handle: root.handle, child: root.child };
    },
    ...over,
  });
  if (startTurn) lane.beginTurn();
  const call: Harness["call"] = async (args, id = `toolu_${String(++seq)}`, signal) => {
    const hook = await lane.hook({
      hook_event_name: "PreToolUse", tool_name: GUARDED_COMMAND_TOOL, tool_input: args, tool_use_id: id,
      session_id: "s", transcript_path: "/t", cwd: WT,
    } as HookInput, id, { signal: new AbortController().signal });
    assert.deepEqual(hook, {}, "the hook admits the call");
    return lane.handle(args, { _meta: { [META]: id }, signal });
  };
  return { lane, c, trips, specs, roots, kills, script, call };
}
const text = (r: { content: { type: string; text?: string }[] }) => r.content.map((b) => b.text ?? "").join("");
function hookInput(tool_name: string, command: string, id: string): HookInput {
  return { hook_event_name: "PreToolUse", tool_name, tool_input: { command }, tool_use_id: id } as HookInput;
}
const hookOpts = () => ({ signal: new AbortController().signal });
function resultFor(entry: ReturnType<WorkerMemoryCommands["register"]>): WorkerMemoryPressureResult {
  return {
    run_id: entry.context.run_id, worker_id: entry.context.worker_id, claim_generation: entry.context.claim_generation,
    memory_episode: entry.context.memory_episode, intervention_id: "33333333-3333-4333-8333-333333333333",
    tool_id: entry.toolId, attempt_id: entry.attemptId, command_id: entry.commandId, command: entry.command,
    code: "worker_memory_pressure", observed_total_bytes: 900, limit_bytes: 1000, tree_rss_bytes_approx: 100,
    outcome: "confirmed_drained", cancellation_confirmed: true, retry_blocked: true,
    guidance: "Use smaller or serial commands after recovery.",
  };
}

// -- attribution and refusals ------------------------------------------------------------------

test("a call without a matching, well-formed, unconsumed hook record is refused and nothing launches", { timeout: 10000 }, async () => {
  const h = harness();
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo hi", "toolu_ok"), "toolu_ok", hookOpts());
  const refusals = [
    await h.lane.handle({ command: "echo hi" }, {}),
    await h.lane.handle({ command: "echo hi" }, { _meta: {} }),
    await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: 42 } }),
    await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_unknown" } }),
    await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_ok\u0000" } }),
    await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "t".repeat(257) } }),
    await h.lane.handle({ command: "echo hi" }, undefined),
  ];
  for (const r of refusals) {
    assert.equal(r.isError, true);
    assert.match(text(r), /could not be attributed/);
  }
  assert.equal(h.specs.length, 0);
  // Malformed ids are never recorded by the hook either.
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo hi", "bad\u202eid"), "bad\u202eid", hookOpts());
  assert.match(text(await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "bad\u202eid" } })), /could not be attributed/);
  // The record is consumed by its single matching call.
  const first = await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_ok" } });
  assert.match(text(first), /^Exit code: 0/);
  assert.match(text(await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_ok" } })), /could not be attributed/);
  assert.equal(h.specs.length, 1);
});

test("both matchers firing for one call record once; a consumed id never re-arms", { timeout: 10000 }, async () => {
  const h = harness();
  const input = { ...hookInput(GUARDED_COMMAND_TOOL, "echo hi", "toolu_dup"), agent_id: "sub-1" } as HookInput;
  assert.deepEqual(await h.lane.hook(input, "toolu_dup", hookOpts()), {});
  assert.deepEqual(await h.lane.hook(input, "toolu_dup", hookOpts()), {});
  assert.match(text(await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_dup" } })), /^Exit code: 0/);
  await h.lane.hook(input, "toolu_dup", hookOpts());
  assert.match(text(await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_dup" } })), /could not be attributed/);
  assert.equal(h.specs.length, 1);
});

test("run_in_background is refused visibly before screening or launch", { timeout: 10000 }, async () => {
  const h = harness();
  const r = await h.call({ command: "sleep 1", run_in_background: true });
  assert.equal(r.isError, true);
  assert.match(text(r), /Background commands are not supported on this run/);
  assert.match(text(r), /3600000/);
  assert.equal(h.specs.length, 0);
  // Background is refused even for a command the screen would also deny.
  const id = "toolu_bg";
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo ok", id), id, hookOpts());
  assert.match(text(await h.lane.handle({ command: DENIED, run_in_background: true }, { _meta: { [META]: id } })), /Background commands are not supported/);
});

test("screening runs in the hook for the aliased name and again in the handler without the hook", { timeout: 10000 }, async () => {
  const h = harness();
  for (const tool_name of [GUARDED_COMMAND_TOOL, "Bash"]) {
    const out = await h.lane.hook(hookInput(tool_name, DENIED, "toolu_d"), "toolu_d", hookOpts());
    assert.equal((out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision, "deny", tool_name);
  }
  // Other tools are untouched.
  assert.deepEqual(await h.lane.hook(hookInput("Read", DENIED, "x"), "x", hookOpts()), {});
  // A denied command never produces a record.
  assert.match(text(await h.lane.handle({ command: DENIED }, { _meta: { [META]: "toolu_d" } })), /could not be attributed/);
  // Hook screening bypassed: a harmless command was recorded, the handler gets the hostile one.
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo ok", "toolu_h"), "toolu_h", hookOpts());
  const r = await h.lane.handle({ command: DENIED }, { _meta: { [META]: "toolu_h" } });
  assert.equal(r.isError, true);
  assert.doesNotMatch(text(r), /Exit code/);
  assert.equal(h.specs.length, 0);
});

test("a capture failure at turn start trips with the original error, rethrows it and fabricates no result", { timeout: 10000 }, async () => {
  const stop = new Error("retained recovery stop");
  const h = harness({ capturedMemory: () => { throw stop; } }, false);
  assert.throws(() => h.lane.beginTurn(), (error: unknown) => error === stop);
  assert.equal(h.trips.length, 1);
  assert.ok(h.trips[0] === stop, "the same instance, not a copy");
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo hi", "toolu_1"), "toolu_1", hookOpts());
  assert.match(text(await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_1" } })), /could not be attributed/);
  assert.equal(h.specs.length, 0);
});

test("a call before any turn began is refused and launches nothing", { timeout: 10000 }, async () => {
  const h = harness({}, false);
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo hi", "toolu_early"), "toolu_early", hookOpts());
  const r = await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_early" } });
  assert.equal(r.isError, true);
  assert.equal(h.specs.length, 0);
});

test("admission: a retired context or a held latch refuses with worker_memory_pressure and launches nothing", { timeout: 10000 }, async () => {
  const held = harness();
  const latch = held.c.commands.acquireLatch(held.c.context)!;
  const r1 = await held.call({ command: "echo hi" });
  assert.equal(r1.isError, true);
  assert.match(text(r1), /worker_memory_pressure/);
  assert.match(text(r1), /Shell admission is held or its captured flight has retired\./);
  assert.equal(held.specs.length, 0);
  held.c.commands.releaseLatch(latch);
  assert.match(text(await held.call({ command: "echo hi" })), /^Exit code: 0/, "released admission launches again");

  const retired = harness();
  retired.c.commands.retire(retired.c.context);
  const r2 = await retired.call({ command: "echo hi" });
  assert.match(text(r2), /worker_memory_pressure/);
  assert.equal(retired.specs.length, 0);
  assert.equal(held.specs.length, 1);
});

test("the ninth concurrent command is refused visibly; a finished slot is reusable", { timeout: 20000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const running = Array.from({ length: 8 }, () => h.call({ command: "sleep 100" }));
  await until(() => h.roots.length === 8, "eight launches");
  assert.equal(h.lane.liveCount, 8);
  const ninth = await h.call({ command: "echo ninth" });
  assert.equal(ninth.isError, true);
  assert.match(text(ninth), /At most 8 commands may run at once; wait for one to finish\./);
  assert.equal(h.specs.length, 8);
  h.roots[0]!.exitWith(0);
  assert.match(text((await running[0])!), /^Exit code: 0/);
  h.script.current = (root) => { setImmediate(() => root.exitWith(0)); };
  assert.match(text(await h.call({ command: "echo again" })), /^Exit code: 0/);
  for (const root of h.roots.slice(1, 8)) root.exitWith(0);
  await Promise.all(running);
  assert.equal(h.lane.liveCount, 0);
});

// -- launch contract ---------------------------------------------------------------------------

test("every call starts at the worktree root with the guarded env, a worker-minted spill path and the supervisor bin", { timeout: 10000 }, async () => {
  const h = harness();
  await h.call({ command: "cd /elsewhere && pwd" });
  await h.call({ command: "echo second", timeout: 5000 });
  assert.equal(h.specs.length, 2);
  const ids = new Set<string>();
  for (const spec of h.specs) {
    assert.equal(spec.cwd, WT);
    assert.equal(spec.supervisorBin, SUPERVISOR_BIN);
    assert.equal(spec.env["PATH"], "/usr/bin");
    assert.equal(spec.env["UZI_RUN_ATTEMPT"], "attempt-1", "run-attempt markers stay");
    for (const forbidden of ["UZI_WORKER_TOKEN", "UZI_WORKER_TOKEN_FILE", "UZI_API_URL", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"]) {
      assert.ok(!(forbidden in spec.env), `${forbidden} must not reach the command`);
    }
    const match = /^(.*)\/\.uzi\/scratch\/uzi-cmd\/([0-9a-f-]{36})\.log$/.exec(spec.spillPath);
    assert.ok(match, spec.spillPath);
    assert.equal(match[1], WT);
    ids.add(match[2]!);
    assert.equal(spec.spillCapBytes, 64 * 1024 * 1024);
  }
  assert.equal(ids.size, 2, "each call gets its own spill file");
  assert.equal(h.specs[0]!.command, "cd /elsewhere && pwd");
});

test("the deadline defaults to 120000 ms and an explicit timeout replaces it", { timeout: 10000 }, async () => {
  const h = harness();
  await h.call({ command: "true" });
  await h.call({ command: "true", timeout: 7000 });
  // waitChild is bounded by deadline + cleanup (300) + grace (30), so the deadline is recoverable exactly.
  assert.equal(h.roots[0]!.waitMs(), 120_000 + 300 + 30);
  assert.equal(h.roots[1]!.waitMs(), 7000 + 300 + 30);
});

test("invalid arguments are refused even when the schema layer is bypassed", { timeout: 10000 }, async () => {
  const h = harness();
  const bad: Record<string, unknown>[] = [
    { command: "" }, { command: "x", timeout: 0 }, { command: "x", timeout: 3_600_001 },
    { command: "x", timeout: 1.5 }, { command: "x".repeat(131_001) }, { timeout: 5 },
  ];
  for (const args of bad) {
    const r = await h.call(args);
    assert.equal(r.isError, true);
  }
  assert.equal(h.specs.length, 0);
});

test("a launch that throws RunnerCommandNotStartedError is a visible tool error without poisoning", { timeout: 10000 }, async () => {
  const h = harness({ launch: async () => { throw new RunnerCommandNotStartedError(Object.assign(new Error("spawn E2BIG"), { code: "E2BIG" })); } });
  const r = await h.call({ command: "echo hi" });
  assert.equal(r.isError, true);
  assert.match(text(r), /could not be started/);
  assert.equal(h.lane.poisoned, false);
  assert.equal(h.trips.length, 0);
  assert.equal(h.lane.liveCount, 0);
});

test("a launch that fails after the supervisor spawned kills it, poisons the lane and trips the turn", { timeout: 10000 }, async () => {
  const root = fakeRoot();
  const h = harness({
    launch: async (spec) => {
      spec.onSpawned?.({ pid: 50, startTime: 1 }, root.child);
      throw new Error("started deadline");
    },
  });
  const r = await h.call({ command: "echo hi" });
  assert.equal(r.isError, true);
  assert.deepEqual(h.kills, [root.child]);
  assert.equal(h.lane.poisoned, true);
  assert.equal(h.trips.length, 1);
  assert.ok(h.trips[0] instanceof GuardedCommandUnconfirmedError);
  assert.deepEqual(h.lane.recordedRoots(), []);
});

test("the supervisor root is recorded synchronously at spawn and removed on a confirmed drain", { timeout: 10000 }, async () => {
  const gate = deferred<void>();
  const h = harness({
    launch: async (spec) => {
      const root = fakeRoot();
      spec.onSpawned?.({ pid: 77, startTime: 9001 }, root.child);
      await gate.promise;
      setImmediate(() => root.exitWith(0));
      return { handle: root.handle, child: root.child };
    },
  });
  const pending = h.call({ command: "echo hi" });
  await until(() => h.lane.recordedRoots().length === 1, "recorded root");
  assert.deepEqual(h.lane.recordedRoots(), [{ pid: 77, startTime: 9001 }], "recorded before the launch resolved");
  gate.resolve();
  assert.match(text(await pending), /^Exit code: 0/);
  assert.deepEqual(h.lane.recordedRoots(), [], "dropped after the drain");

  // A root whose start time could not be read cannot be recorded by identity.
  const noTime = harness({ launch: async (spec) => {
    const root = fakeRoot();
    spec.onSpawned?.({ pid: 78, startTime: undefined }, root.child);
    setImmediate(() => root.exitWith(0));
    return { handle: root.handle, child: root.child };
  } });
  await noTime.call({ command: "echo hi" });
  assert.deepEqual(noTime.lane.recordedRoots(), []);
});

// -- deadline, abort, output -------------------------------------------------------------------

test("the deadline stops and drains the tree, keeps the output so far and reports the timeout", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = (root) => { root.stdout.write("partial-output\n"); };
  const started = Date.now();
  const r = await h.call({ command: "sleep 100", timeout: 60 });
  assert.ok(Date.now() - started < 3000);
  assert.equal(r.isError, true);
  assert.match(text(r), /Command timed out after 60 ms; it and all its descendants were stopped\./);
  assert.match(text(r), /partial-output/);
  assert.equal(h.roots[0]!.disposes(), 1);
  assert.equal(h.lane.liveCount, 0);
  assert.equal(h.lane.poisoned, false);
  assert.deepEqual(h.lane.recordedRoots(), []);
  assert.equal(h.c.commands.eligibleEntries()?.length, 0, "the drained entry is unregistered");
});

test("a turn abort through the call signal or the lane's turn signal cancels and disposes", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const call = new AbortController();
  const pending = h.call({ command: "sleep 100" }, undefined, call.signal);
  await until(() => h.roots.length === 1, "launch");
  call.abort();
  const r = await pending;
  assert.match(text(r), /Command was cancelled; it and all its descendants were stopped\./);
  assert.equal(r.isError, true);
  assert.equal(h.roots[0]!.disposes(), 1);

  const turn = new AbortController();
  h.lane.beginTurn(turn.signal);
  const second = h.call({ command: "sleep 100" });
  await until(() => h.roots.length === 2, "second launch");
  turn.abort();
  assert.match(text(await second), /Command was cancelled/);
  assert.equal(h.roots[1]!.disposes(), 1);
});

test("authority lost mid-turn trips with the given stop and disposes every live root", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const a = h.call({ command: "sleep 100" });
  const b = h.call({ command: "sleep 100" });
  await until(() => h.roots.length === 2, "launches");
  const stop = new Error("memory authority lost");
  h.lane.onAuthorityLost(stop);
  assert.ok(h.trips[0] === stop);
  assert.match(text(await a), /cancelled/);
  assert.match(text(await b), /cancelled/);
  assert.deepEqual(h.roots.map((root) => root.disposes()), [1, 1]);
  // New commands are refused: the guard no longer admits them.
  assert.match(text(await h.call({ command: "echo late" })), /worker_memory_pressure/);
  assert.equal(h.specs.length, 2);
});

test("output over 30000 characters is a preview that names the repo-relative spill file", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = (root) => {
    root.stdout.write(`HEAD${"m".repeat(60_000)}TAIL`);
    setImmediate(() => root.exitWith(3));
  };
  const r = await h.call({ command: "gen" });
  const body = text(r);
  const id = /\/([0-9a-f-]{36})\.log$/.exec(h.specs[0]!.spillPath)![1]!;
  assert.match(body, /^Exit code: 3/);
  assert.equal(r.isError, true);
  assert.ok(body.includes(`.uzi/scratch/uzi-cmd/${id}.log`));
  assert.ok(!body.includes(`${WT}/.uzi`), "the path is repo-relative");
  assert.match(body, /preview/);
  assert.ok(body.includes("HEAD") && body.includes("TAIL"));
  const inline = body.slice(body.indexOf("\n") + 1, body.indexOf("\n[Output exceeded"));
  assert.ok(inline.length <= 30_000, `inline ${String(inline.length)}`);
  assert.doesNotMatch(body, /truncated at/);
});

test("more than 64 MiB of output states that the spill file is truncated and bounded", { timeout: 60000 }, async () => {
  const h = harness();
  h.script.current = (root) => {
    const chunk = Buffer.alloc(1024 * 1024, "z");
    for (let i = 0; i < 65; i++) root.stdout.write(chunk);
    root.stdout.write("END");
    setImmediate(() => root.exitWith(0));
  };
  const body = text(await h.call({ command: "gen" }));
  assert.match(body, /The spill file is truncated at 64 MiB; it is bounded and not the full output\./);
  assert.match(body, /preview/);
  assert.ok(body.length < 31_000);
});

test("secrets are redacted from the inline output and the command text is never echoed", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = (root) => { root.stdout.write("token=private-fixture end\n"); setImmediate(() => root.exitWith(0)); };
  const body = text(await h.call({ command: "echo private-fixture", description: "uses private-fixture" }));
  assert.ok(!body.includes("private-fixture"));
  assert.match(body, /token=\*\*\*REDACTED\*\*\* end/);
});

test("output still held open by a descendant after the grace is reported as possibly incomplete", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = (root) => { root.holdOutput(); root.stdout.write("visible\n"); setImmediate(() => root.exitWith(5)); };
  const body = text(await h.call({ command: "sleep 100 &" }));
  assert.match(body, /^Exit code: 5/);
  assert.match(body, /visible/);
  assert.match(body, /Output may be incomplete: a background process kept the output open/);
  assert.equal(h.roots[0]!.disposes(), 1, "the straggler tree is disposed after the grace");
});

// -- memory guard integration ------------------------------------------------------------------

test("D7: dispose, then the prepared pressure outcome, then the result; unregistered afterwards", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const events: string[] = [];
  const pending = h.call({ command: "yes" }).then((r) => { events.push("result"); return r; });
  await until(() => h.c.commands.eligibleEntries()?.length === 1, "enrollment");
  const entry = h.c.commands.eligibleEntries()![0]!;
  assert.equal(entry.source, "claude-model-command");
  assert.equal(entry.primary?.pid, 51);
  h.c.commands.prepareOutcome(entry);
  // The guard cancels the enrolled command: the root is disposed through the shared flight.
  await entry.cancel({ remainingMs: 1000, signal: new AbortController().signal, authorized: () => true });
  await until(() => h.roots[0]!.disposes() === 1, "disposal");
  await new Promise((done) => setTimeout(done, 80));
  assert.equal(events.length, 0, "the lane waits for the prepared outcome after the disposal");
  events.push("outcome");
  assert.equal(h.c.commands.resolveToolOutcome(entry, resultFor(entry)), true);
  const r = await pending;
  assert.deepEqual(events, ["outcome", "result"]);
  assert.equal(h.roots[0]!.disposes(), 1, "one disposal however many parties asked");
  assert.equal(r.isError, true);
  assert.deepEqual(JSON.parse(text(r)), resultFor(entry));
  assert.equal(h.c.commands.eligibleEntries()?.length, 0);
  assert.equal(h.c.commands.hasConfirmedStop(entry), true);
});

test("a pressure result that does not belong to this call becomes memory_result_mismatch", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const pending = h.call({ command: "yes" });
  await until(() => h.c.commands.eligibleEntries()?.length === 1, "enrollment");
  const entry = h.c.commands.eligibleEntries()![0]!;
  h.c.commands.prepareOutcome(entry);
  const foreign = { ...resultFor(entry), tool_id: "toolu_other" };
  h.c.commands.outcome = () => Promise.resolve(foreign);
  h.roots[0]!.exitWith(0);
  const r = await pending;
  assert.equal(r.isError, true);
  assert.match(text(r), /memory_result_mismatch/);
  assert.ok(!text(r).includes("toolu_other"));
});

test("an unconfirmed disposal fails closed: failed drain, kill, poison, typed trip, later calls refused", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const pending = h.call({ command: "yes", timeout: 40 });
  await until(() => h.c.commands.eligibleEntries()?.length === 1, "enrollment");
  const entry = h.c.commands.eligibleEntries()![0]!;
  h.roots[0]!.unconfirmed();
  const r = await pending;
  assert.equal(r.isError, true);
  assert.match(text(r), /cleanup could not be confirmed/);
  assert.equal(h.lane.poisoned, true);
  assert.deepEqual(h.kills, [h.roots[0]!.child]);
  assert.equal(h.trips.length, 1);
  assert.ok(h.trips[0] instanceof GuardedCommandUnconfirmedError);
  assert.ok(h.c.commands.drainFailure(entry), "recordDrain({confirmed:false}) reached the registry");
  assert.equal(h.c.commands.hasConfirmedStop(entry), false);
  assert.deepEqual(h.lane.recordedRoots(), []);
  const later = await h.call({ command: "echo hi" });
  assert.equal(later.isError, true);
  assert.match(text(later), /earlier command's cleanup could not be confirmed/);
  assert.equal(h.specs.length, 1);
  await assert.rejects(h.lane.quiesce(100), GuardedCommandUnconfirmedError);
});

test("uncertain memory observation leaves the command unenrolled but still running and bounded", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = (root) => { root.unavailable(); root.stdout.write("ran\n"); setImmediate(() => root.exitWith(0)); };
  const body = text(await h.call({ command: "echo ran" }));
  assert.match(body, /^Exit code: 0/);
  assert.match(body, /ran/);
  assert.equal(h.c.commands.eligibleEntries()?.length, 0, "unenrolled: no memory intervention target");
});

test("quiesce disposes live commands, clears attribution and closes the turn; it is idempotent", { timeout: 10000 }, async () => {
  const h = harness();
  h.script.current = () => {};
  const pending = h.call({ command: "sleep 100" });
  await until(() => h.roots.length === 1, "launch");
  await h.lane.hook(hookInput(GUARDED_COMMAND_TOOL, "echo hi", "toolu_stale"), "toolu_stale", hookOpts());
  await h.lane.quiesce(1000);
  assert.match(text(await pending), /cancelled/);
  assert.equal(h.roots[0]!.disposes(), 1);
  assert.equal(h.lane.liveCount, 0);
  assert.equal(h.lane.poisoned, false);
  await h.lane.quiesce(1000);
  // The stale record was cleared and the turn is closed until the next beginTurn.
  assert.match(text(await h.lane.handle({ command: "echo hi" }, { _meta: { [META]: "toolu_stale" } })), /could not be attributed/);
  h.lane.beginTurn();
  h.script.current = (root) => { setImmediate(() => root.exitWith(0)); };
  assert.match(text(await h.call({ command: "echo again" })), /^Exit code: 0/);
});

test("quiesce poisons, kills and throws when a root cannot be confirmed within the deadline", { timeout: 10000 }, async () => {
  const h = harness({ cleanupMs: 5000 });
  h.script.current = (root) => {
    // Disposal never completes: the supervisor does not answer.
    (root.handle as unknown as { dispose: () => Promise<never> }).dispose = () => new Promise(() => {});
  };
  void h.call({ command: "sleep 100" });
  await until(() => h.roots.length === 1, "launch");
  await assert.rejects(h.lane.quiesce(100), GuardedCommandUnconfirmedError);
  assert.equal(h.lane.poisoned, true);
  assert.deepEqual(h.kills, [h.roots[0]!.child]);
  assert.deepEqual(h.lane.recordedRoots(), []);
  await assert.rejects(h.lane.quiesce(100), GuardedCommandUnconfirmedError);
});

test("more than 256 sequential enrolled commands keep the registry selectable", { timeout: 60000 }, async () => {
  const h = harness();
  for (let i = 0; i < 300; i++) {
    const r = await h.call({ command: "true" });
    assert.match(text(r), /^Exit code: 0/);
  }
  assert.equal(h.roots.length, 300);
  const eligible = h.c.commands.eligibleEntries();
  assert.ok(eligible, "the registry still selects (it stops above 256 retained entries)");
  assert.equal(eligible.length, 0);
});

// -- the MCP server ----------------------------------------------------------------------------

test("the server description states the lane's limits and the transport timeout is headroom over the maximum", { timeout: 10000 }, () => {
  assert.equal(GUARDED_SERVER_TIMEOUT_MS, 3_660_000);
  for (const phrase of [
    "Foreground only", "run_in_background is refused", "no long-running mode", "120000 ms", "3600000 ms", "60 minutes",
    "blocker", "repository root", "cd dir && command", "30000 characters", ".uzi/scratch/uzi-cmd/<id>.log", "64 MiB", "At most 8 commands",
  ]) {
    assert.ok(GUARDED_COMMAND_DESCRIPTION.includes(phrase), phrase);
  }
  const h = harness();
  const server = buildGuardedCommandServer(h.lane);
  assert.equal(server.type, "sdk");
  assert.equal(server.name, "uzi_cmd");
});
