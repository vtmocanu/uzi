import assert from "node:assert/strict";
import { spawn, spawnSync } from "node:child_process";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { createInterface } from "node:readline";
import { PassThrough } from "node:stream";
import { after, afterEach, describe, it } from "node:test";

import { guardedCommandEnv } from "../src/claude-command/env.js";
import { memoryLauncherPreflight } from "../src/claude-command/preflight.js";
import { WRAPPER, wrapperArgv } from "../src/claude-command/wrapper.js";
import {
  RunnerCommandNotStartedError,
  SUPERVISOR_BIN,
  killUnreapedSupervisor,
  launchRunnerCommandRoot,
  launchRunnerSupervisedProbe,
  type LauncherDeps,
  type RunnerCommandChild,
  type RunnerCommandLaunchSpec,
  type RunnerCommandSpawned,
  type SupervisorProcess,
} from "../src/codex/launcher.js";
import { RUNNER_UID, runnerCommand, setprivRunnerArgs } from "../src/runner-uid.js";
import { PROVIDER_CREDENTIAL_VARS } from "../src/sdk-spawn.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_ENV, RUN_CLONE_KEY_ENV, WORKER_SPAWN_ENV, workerRunnerRootPids, workerSpawnEnv } from "../src/worker-spawn-mark.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

resetResidueQuarantineAfterEach();

// issue #2471 (M1): the supervised runner-uid command launcher, its wrapper script and the
// launcher preflight. The first blocks drive the launcher against a fake supervisor, the next
// runs the real WRAPPER under the host bash (no supervisor needed), the last needs the worker
// image and runs only inside the worker-uid root-start container.

// ─── Fake supervisor ────────────────────────────────────────────────────────────

class FakeSupervisor extends EventEmitter {
  readonly pid = 4242;
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  readonly stderr = new PassThrough();
  readonly control = new PassThrough();
  readonly evidence = new PassThrough();
  readonly stdio = [this.stdin, this.stdout, this.stderr, this.control, this.evidence];

  constructor(private readonly uid: number, autoStarted = true) {
    super();
    createInterface({ input: this.control }).on("line", (line) => this.onControl(line));
    if (autoStarted) this.emitStarted();
  }

  emitStarted(): void {
    this.write({ event: "started", supervisorPid: this.pid, childPid: this.pid + 1, subreaper: true, nondumpable: true,
      uid: this.uid, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true });
  }

  write(value: unknown): void { this.evidence.write(`${JSON.stringify(value)}\n`); }

  private onControl(line: string): void {
    const cmd = JSON.parse(line) as { op: string; id: number };
    if (cmd.op !== "dispose") return;
    this.write({ event: "dispose", id: cmd.id, state: "drained", authority: "ECHILD+__WALL", killed: [], reaped: [this.pid + 1] });
    setImmediate(() => {
      this.emit("exit", 0, null);
      this.stdout.end(); this.stderr.end(); this.evidence.end();
      setImmediate(() => this.emit("close", 0, null));
    });
  }

  destroyAll(): void { for (const s of this.stdio) s.destroy(); }
}

const fakes: FakeSupervisor[] = [];
interface SpawnCall { command: string; args: readonly string[]; options: { cwd: string; env: NodeJS.ProcessEnv; stdio: readonly string[] } }
const spawnCalls: SpawnCall[] = [];
afterEach(() => {
  for (const f of fakes) f.destroyAll();
  fakes.length = 0;
  spawnCalls.length = 0;
});

const SPLIT_ENV = { UZI_UID_SPLIT: "1" };
const SINGLE_UID = process.getuid?.() ?? 0;

function depsFor(fake: FakeSupervisor | (() => never), overrides: Partial<LauncherDeps> = {}): LauncherDeps {
  return {
    env: SPLIT_ENV,
    spawnSupervisor: (command, args, options) => {
      spawnCalls.push({ command, args, options });
      if (typeof fake === "function") fake();
      return fake as unknown as SupervisorProcess;
    },
    deadlines: { started: 1000, snapshot: 1000, dispose: 1000, exit: 1000 },
    rootStartTime: () => 987654,
    ...overrides,
  };
}

function newFake(uid = RUNNER_UID, autoStarted = true): FakeSupervisor {
  const fake = new FakeSupervisor(uid, autoStarted);
  fakes.push(fake);
  return fake;
}

function spec(overrides: Partial<RunnerCommandLaunchSpec> = {}): RunnerCommandLaunchSpec {
  return {
    command: "echo 'hi there'",
    cwd: "/work/repo",
    env: { PATH: "/usr/bin:/bin", UZI_RUN_ATTEMPT: "run:attempt" },
    spillPath: "/work/repo/.uzi/scratch/uzi-cmd/call-1.log",
    spillCapBytes: 67108864,
    supervisorBin: SUPERVISOR_BIN,
    ...overrides,
  };
}

describe("launchRunnerCommandRoot with a fake supervisor", () => {
  it("under the uid split runs the wrapper at the runner uid through setpriv, with no drop-caps or cleanup token", async () => {
    const fake = newFake();
    const { handle } = await launchRunnerCommandRoot(spec(), depsFor(fake));
    assert.equal(spawnCalls.length, 1);
    const call = spawnCalls[0]!;
    const setpriv = setprivRunnerArgs();
    assert.deepEqual(call.args.slice(0, setpriv.length), setpriv);
    assert.deepEqual(call.args.slice(setpriv.length), [
      SUPERVISOR_BIN, "--expect-uid", "10002", "--", "/bin/bash", "-c", WRAPPER, "uzi-cmd",
      "/work/repo/.uzi/scratch/uzi-cmd/call-1.log", "67108864", "echo 'hi there'",
    ]);
    assert.ok(!call.args.includes("--drop-controller-caps"));
    assert.ok(!call.args.includes("--cleanup-token"));
    assert.equal(call.options.cwd, "/work/repo");
    assert.deepEqual([...call.options.stdio], ["pipe", "pipe", "pipe", "pipe", "pipe"]);
    assert.equal(handle.started.uid, RUNNER_UID);
  });

  it("single-uid runs the supervisor directly at the process uid", { skip: SINGLE_UID > 0 ? false : "needs a non-root process uid" }, async () => {
    const fake = newFake(SINGLE_UID);
    await launchRunnerCommandRoot(spec(), depsFor(fake, { env: {} }));
    const call = spawnCalls[0]!;
    assert.equal(call.command, SUPERVISOR_BIN);
    assert.deepEqual(call.args.slice(0, 3), ["--expect-uid", String(SINGLE_UID), "--"]);
    assert.deepEqual(call.args.slice(3, 6), ["/bin/bash", "-c", WRAPPER]);
    assert.ok(!call.args.includes("--drop-controller-caps"));
  });

  it("passes the model command only as the last positional argument, never inside the script", async () => {
    const hostile = "echo \"$(touch /tmp/pwn)\"; exit 3 # '";
    const fake = newFake();
    await launchRunnerCommandRoot(spec({ command: hostile }), depsFor(fake));
    const args = spawnCalls[0]!.args;
    assert.equal(args.at(-1), hostile);
    assert.equal(args.filter((a) => a.includes("touch /tmp/pwn")).length, 1);
    assert.ok(!WRAPPER.includes("touch"));
    assert.deepEqual(wrapperArgv({ spillPath: "/s", spillCapBytes: 5, command: hostile }).slice(3), ["uzi-cmd", "/s", "5", hostile]);
  });

  it("refuses a supervisor binary other than the immutable image path, before spawning", async () => {
    await assert.rejects(launchRunnerCommandRoot(spec({ supervisorBin: "/tmp/evil-supervisor" }), depsFor(newFake())),
      (e: unknown) => e instanceof TrustedExecutionRefusal && /supervisorBin must equal/.test(e.message));
    assert.equal(spawnCalls.length, 0);
  });

  it("refuses a relative cwd, a relative spill path and an invalid spill cap, before spawning", async () => {
    const bad: Array<Partial<RunnerCommandLaunchSpec>> = [
      { cwd: "work/repo" }, { spillPath: "relative.log" }, { spillCapBytes: 0 }, { spillCapBytes: -1 },
      { spillCapBytes: 1.5 }, { spillCapBytes: Number.MAX_SAFE_INTEGER + 2 }, { spillCapBytes: Number.NaN },
      { command: undefined as unknown as string },
    ];
    for (const overrides of bad) {
      await assert.rejects(launchRunnerCommandRoot(spec(overrides), depsFor(newFake())), TrustedExecutionRefusal, JSON.stringify(overrides));
    }
    assert.equal(spawnCalls.length, 0);
  });

  it("requires an explicit env object and never falls back to the worker's own env", async () => {
    for (const env of [undefined, null, [] as unknown, "PATH=/bin" as unknown]) {
      await assert.rejects(
        launchRunnerCommandRoot({ ...spec(), env } as unknown as RunnerCommandLaunchSpec, depsFor(newFake())),
        (e: unknown) => e instanceof TrustedExecutionRefusal && /explicit env/.test(e.message),
        String(env),
      );
    }
    assert.equal(spawnCalls.length, 0, "a call without env must not reach the spawn");
  });

  it("spawns with exactly the supplied env, none of the worker's", async () => {
    process.env.CLAUDE_M1_CANARY = "leak";
    try {
      await launchRunnerCommandRoot(spec({ env: { PATH: "/bin", ONLY: "1" } }), depsFor(newFake()));
    } finally { delete process.env.CLAUDE_M1_CANARY; }
    assert.deepEqual(spawnCalls[0]!.options.env, { PATH: "/bin", ONLY: "1" });
  });

  it("records the supervisor root through onSpawned synchronously, before started resolves", async () => {
    const fake = newFake(RUNNER_UID, false);
    const events: string[] = [];
    let spawned: RunnerCommandSpawned | undefined;
    let seenChild: unknown;
    const launching = launchRunnerCommandRoot(spec({
      onSpawned: (root, child) => { events.push("spawned"); spawned = root; seenChild = child; },
    }), depsFor(fake)).then((r) => { events.push("started"); return r; });
    assert.deepEqual(events, ["spawned"], "recorded in the same synchronous section as the spawn");
    fake.emitStarted();
    await launching;
    assert.deepEqual(events, ["spawned", "started"]);
    assert.deepEqual(spawned, { pid: 4242, startTime: 987654 });
    assert.equal(seenChild, fake);
  });

  it("passes an unreadable start time through as undefined", async () => {
    let spawned: RunnerCommandSpawned | undefined;
    await launchRunnerCommandRoot(spec({ onSpawned: (root) => { spawned = root; } }),
      depsFor(newFake(), { rootStartTime: () => undefined }));
    assert.deepEqual(spawned, { pid: 4242, startTime: undefined });
  });

  it("never adds the supervisor to the worker-wide runner-root registry", async () => {
    const before = [...workerRunnerRootPids()];
    const fake = newFake();
    const { handle } = await launchRunnerCommandRoot(spec(), depsFor(fake));
    assert.deepEqual([...workerRunnerRootPids()], before);
    assert.ok(![...workerRunnerRootPids()].some((root) => root.pid === 4242));
    assert.equal((await handle.dispose(1000)).clean, true);
    assert.deepEqual([...workerRunnerRootPids()], before);
  });

  it("ends the supervisor stdin immediately", async () => {
    const fake = newFake();
    await launchRunnerCommandRoot(spec(), depsFor(fake));
    assert.equal(fake.stdin.writableEnded, true);
  });

  it("treats a child exit as a command result, not a failure, and disposes drained", async () => {
    const fake = newFake();
    const { handle } = await launchRunnerCommandRoot(spec(), depsFor(fake));
    fake.write({ event: "child_exit", code: 3 });
    assert.deepEqual(await handle.waitChild(1000), { event: "child_exit", code: 3 });
    assert.equal(handle.failed, undefined);
    const outcome = await handle.dispose(1000);
    assert.equal(outcome.clean, true);
    assert.equal(outcome.clean ? outcome.event.state : undefined, "drained");
  });

  it("kills the unreaped supervisor and rethrows when onSpawned throws", { skip: SINGLE_UID > 0 ? false : "needs a non-root process uid" }, async () => {
    const fake = newFake(SINGLE_UID);
    const killed: unknown[] = [];
    Object.assign(fake, { exitCode: null, signalCode: null, kill: (signal?: unknown) => { killed.push(signal); return true; } });
    const boom = new Error("record failed");
    const reported: unknown[] = [];
    await assert.rejects(
      launchRunnerCommandRoot(spec({ onSpawned: () => { throw boom; } }), depsFor(fake, { env: {}, onKillFailed: (e: unknown) => { reported.push(e); } })),
      (e: unknown) => e === boom,
    );
    assert.deepEqual(killed, ["SIGKILL"]);
    assert.deepEqual(reported, [], "a successful kill reports nothing");
  });

  it("reports an unkillable supervisor through onKillFailed and still rethrows the original onSpawned error", { skip: SINGLE_UID > 0 ? false : "needs a non-root process uid" }, async () => {
    // The fake lacks the exit fields, so killUnreapedSupervisor cannot prove it unreaped and returns false.
    const fake = newFake(SINGLE_UID);
    const boom = new Error("record failed");
    const reported: unknown[] = [];
    await assert.rejects(
      launchRunnerCommandRoot(spec({ onSpawned: () => { throw boom; } }),
        depsFor(fake, { env: {}, onKillFailed: (e: unknown) => { reported.push(e); } })),
      (e: unknown) => e === boom,
    );
    assert.deepEqual(reported, [boom]);
  });

  it("keeps the original onSpawned error when the onKillFailed hook itself throws", { skip: SINGLE_UID > 0 ? false : "needs a non-root process uid" }, async () => {
    const fake = newFake(SINGLE_UID);
    const boom = new Error("record failed");
    let hookCalls = 0;
    await assert.rejects(
      launchRunnerCommandRoot(spec({ onSpawned: () => { throw boom; } }),
        depsFor(fake, { env: {}, onKillFailed: () => { hookCalls++; throw new Error("hook failed"); } })),
      (e: unknown) => e === boom,
    );
    assert.equal(hookCalls, 1, "the throwing hook was actually reached");
  });

  it("classifies a spawn that throws as not started, without calling onSpawned", async () => {
    let called = false;
    const e2big = Object.assign(new Error("spawn E2BIG"), { code: "E2BIG" });
    await assert.rejects(
      launchRunnerCommandRoot(spec({ onSpawned: () => { called = true; } }),
        depsFor((() => { throw e2big; }) as () => never)),
      (e: unknown) => e instanceof RunnerCommandNotStartedError && /E2BIG/.test(e.message) && e.cause === e2big,
    );
    assert.equal(called, false);
  });

  it("classifies a spawn with no pid (an asynchronous spawn failure) as not started", async () => {
    const fake = newFake();
    Object.defineProperty(fake, "pid", { value: undefined });
    await assert.rejects(launchRunnerCommandRoot(spec(), depsFor(fake)), RunnerCommandNotStartedError);
  });
});

describe("launchRunnerSupervisedProbe", () => {
  it("runs the inert /bin/true child through the same supervisor argv, not the wrapper", async () => {
    const fake = newFake();
    await launchRunnerSupervisedProbe(depsFor(fake));
    const args = spawnCalls[0]!.args;
    const setpriv = setprivRunnerArgs();
    assert.deepEqual(args.slice(setpriv.length), [SUPERVISOR_BIN, "--expect-uid", "10002", "--", "/bin/true"]);
    assert.ok(!args.includes(WRAPPER));
    assert.equal(spawnCalls[0]!.options.cwd, "/");
  });
});

describe("killUnreapedSupervisor", () => {
  function child(overrides: Partial<RunnerCommandChild> = {}): RunnerCommandChild & { killed: unknown[] } {
    const killed: unknown[] = [];
    return Object.assign({ pid: 4242, exitCode: null, signalCode: null, killed,
      kill: (signal?: NodeJS.Signals | number) => { killed.push(signal); return true; } }, overrides) as never;
  }

  it("never signals an exited or signalled child", () => {
    const runs: unknown[] = [];
    const runKill = (...args: unknown[]) => { runs.push(args); return { status: 0 }; };
    for (const split of [true, false]) {
      const exited = child({ exitCode: 0 });
      const signalled = child({ signalCode: "SIGKILL" });
      const unknown = child({ exitCode: undefined, signalCode: undefined });
      for (const c of [exited, signalled, unknown]) assert.equal(killUnreapedSupervisor(c, { split, runKill }), false);
      assert.deepEqual([...exited.killed, ...signalled.killed, ...unknown.killed], []);
    }
    assert.deepEqual(runs, []);
  });

  it("single-uid signals the unreaped child directly with SIGKILL", () => {
    const c = child();
    assert.equal(killUnreapedSupervisor(c, { split: false }), true);
    assert.deepEqual(c.killed, ["SIGKILL"]);
  });

  it("under the split forks kill -KILL as the runner uid instead of signalling directly", () => {
    const c = child();
    const runs: Array<[string, readonly string[]]> = [];
    assert.equal(killUnreapedSupervisor(c, { split: true, runKill: (command, args) => { runs.push([command, args]); return { status: 0 }; } }), true);
    assert.deepEqual(c.killed, []);
    assert.equal(runs.length, 1);
    assert.deepEqual(runs[0]![1], [...setprivRunnerArgs(), "kill", "-KILL", "4242"]);
  });

  it("under the split reports delivery only when the forked kill ran and exited 0", () => {
    const results: Array<[{ status: number | null; error?: Error }, boolean]> = [
      [{ status: null, error: new Error("spawn setpriv ENOENT") }, false],
      [{ status: 1 }, false],
      [{ status: null }, false],
      [{ status: 0 }, true],
    ];
    for (const [result, expected] of results) {
      assert.equal(killUnreapedSupervisor(child(), { split: true, runKill: () => result }), expected, JSON.stringify(result));
    }
  });

  it("under the split runs the forked kill with the worker spawn mark and a minimal env, none of the worker's", () => {
    const secretKey = "UZI_TEST_KILL_ENV_SECRET";
    const prior = process.env[secretKey];
    process.env[secretKey] = ["sk", "ant", "fragment", "value"].join("-");
    let seen: NodeJS.ProcessEnv | undefined;
    try {
      killUnreapedSupervisor(child(), { split: true, runKill: (_command, _args, options) => { seen = options.env; return { status: 0 }; } });
    } finally {
      if (prior === undefined) delete process.env[secretKey]; else process.env[secretKey] = prior;
    }
    assert.ok(seen, "an env was passed");
    assert.ok(seen[WORKER_SPAWN_ENV], "carries the worker spawn mark");
    assert.equal(seen[RUN_ATTEMPT_ENV], undefined);
    assert.equal(seen[secretKey], undefined, "the worker's env does not reach the forked kill");
    assert.equal(seen.PATH, "/usr/bin:/bin");
  });

  it("does not throw when the kill machinery fails", () => {
    assert.equal(killUnreapedSupervisor(child(), { split: true, runKill: () => { throw new Error("no setpriv"); } }), false);
  });
});

describe("guardedCommandEnv", () => {
  const cliEnv = {
    PATH: "/usr/bin", HOME: "/home/runner",
    CLAUDE_CODE_OAUTH_TOKEN: "oauth", ANTHROPIC_API_KEY: "key", ANTHROPIC_AUTH_TOKEN: "auth",
    UZI_WORKER_TOKEN: "worker", UZI_WORKER_TOKEN_FILE: "/run/token", UZI_WORKER_TOKEN_OTHER: "x",
    UZI_API_URL: "http://api", [WORKER_SPAWN_ENV]: "nonce",
    [RUN_ATTEMPT_ENV]: "run:attempt", [RUN_CLONE_ENV]: "/clone", [RUN_CLONE_KEY_ENV]: "repo/key",
    UNDEFINED_ONE: undefined,
  };

  it("drops every credential, worker token, API url and spawn mark but keeps the run-attempt markers", () => {
    const out = guardedCommandEnv(cliEnv);
    for (const key of ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "UZI_WORKER_TOKEN",
      "UZI_WORKER_TOKEN_FILE", "UZI_WORKER_TOKEN_OTHER", "UZI_API_URL", WORKER_SPAWN_ENV, "UNDEFINED_ONE"]) {
      assert.ok(!(key in out), key);
    }
    assert.deepEqual(out, { PATH: "/usr/bin", HOME: "/home/runner", [RUN_ATTEMPT_ENV]: "run:attempt",
      [RUN_CLONE_ENV]: "/clone", [RUN_CLONE_KEY_ENV]: "repo/key" });
  });

  it("covers every variable the SDK spawn treats as a provider credential", () => {
    const env = Object.fromEntries(PROVIDER_CREDENTIAL_VARS.map((k) => [k, "secret"]));
    assert.deepEqual(guardedCommandEnv({ ...env, KEEP: "1" }), { KEEP: "1" });
  });

  it("returns a copy and throws when no env object is supplied", () => {
    const input = { A: "1" };
    const out = guardedCommandEnv(input);
    out.B = "2";
    assert.deepEqual(input, { A: "1" });
    for (const bad of [undefined, null, [], "x"]) {
      assert.throws(() => guardedCommandEnv(bad as never), /explicit env/);
    }
  });
});

describe("preflight against the real launcher with a fake supervisor", () => {
  it("passes when the fake supervisor starts and drains", async () => {
    const fake = newFake();
    assert.deepEqual(await memoryLauncherPreflight({ launcherDeps: depsFor(fake), deadlineMs: 3000 }), { ok: true });
  });

  it("reports a profile refusal from the supervisor's abnormal evidence", async () => {
    const fake = newFake(RUNNER_UID, false);
    fake.write({ event: "abnormal", reason: "profile:capDrop", cleanup: { state: "unconfirmed" } });
    const result = await memoryLauncherPreflight({ launcherDeps: depsFor(fake), deadlineMs: 3000, killUnreaped: () => false });
    assert.deepEqual(result, { ok: false, reason: "profile:capDrop" });
  });

  it("fails inside its deadline when the supervisor never reports started", async () => {
    const fake = newFake(RUNNER_UID, false);
    const killed: RunnerCommandChild[] = [];
    const startedAt = Date.now();
    const result = await memoryLauncherPreflight({ launcherDeps: depsFor(fake), deadlineMs: 150, killUnreaped: (c) => { killed.push(c); return true; } });
    assert.equal(result.ok, false);
    assert.ok(Date.now() - startedAt < 3000);
    assert.equal(killed.length, 1);
  });
});

// ─── The real wrapper under the host bash ────────────────────────────────────────────

const BASH = "/bin/bash";
const bashVersion = (): { major: number; minor: number } | undefined => {
  const r = spawnSync(BASH, ["-c", 'printf "%s.%s" "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}"'], { encoding: "utf8" });
  const m = /^(\d+)\.(\d+)$/.exec(r.stdout ?? "");
  return m ? { major: Number(m[1]), minor: Number(m[2]) } : undefined;
};
const version = process.platform === "linux" ? bashVersion() : undefined;
const WRAPPER_SKIP = version && (version.major > 5 || (version.major === 5 && version.minor >= 1))
  ? false : "needs Linux with /bin/bash 5.1 or later (wait for the last process substitution)";

const scratchRoot = fs.mkdtempSync(path.join(process.env.TMPDIR ?? os.tmpdir(), "claude-command-launcher-"));
after(() => { fs.rmSync(scratchRoot, { recursive: true, force: true }); });
let counter = 0;
const nextSpill = (): string => path.join(scratchRoot, `spill-${String(++counter)}`, "call.log");
/** A spill path whose directory can never be created (a path component is /dev/null). */
const UNWRITABLE_SPILL = "/dev/null/uzi-cmd/call.log";

/** SIGKILL a background holder whose pid the command wrote to `file` (safe when it never did). */
function killPidFile(file: string): void {
  try {
    const pid = Number(fs.readFileSync(file, "utf8").trim());
    if (Number.isInteger(pid) && pid > 1) process.kill(pid, "SIGKILL");
  } catch { /* never started, or already gone */ }
}
const nextPidFile = (): string => path.join(scratchRoot, `holder-${String(++counter)}.pid`);

interface WrapperRun {
  readonly code: number | null;
  readonly exitMs: number;
  readonly bytes: number;
  readonly head: string;
  readonly tail: string;
  readonly stderr: string;
  /** Whether the transport (wrapper stdout) closed shortly after the wrapper exited. */
  readonly closed: boolean;
}

const WRAPPER_ENV = { PATH: process.env.PATH ?? "/usr/bin:/bin" };

/** Run WRAPPER like the supervisor does: stdin open, stdout/stderr piped. Resolves shortly after
 *  `exit` (not `close`), because a descendant holding the pipe must not hold the verdict. */
function runWrapper(command: string, opts: { spill?: string; cap?: number; readerDelayMs?: number; timeoutMs?: number; graceTenths?: string } = {}): Promise<WrapperRun> {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const child = spawn(BASH, ["-c", WRAPPER, "uzi-cmd", opts.spill ?? nextSpill(), String(opts.cap ?? 1024), command,
      ...(opts.graceTenths === undefined ? [] : [opts.graceTenths])],
      { stdio: ["pipe", "pipe", "pipe"], env: WRAPPER_ENV });
    let bytes = 0;
    let head = "";
    let tail = "";
    let stderr = "";
    let closed = false;
    child.stdout.once("close", () => { closed = true; });
    const onData = (chunk: Buffer): void => {
      bytes += chunk.length;
      if (head.length < 4096) head += chunk.toString("latin1", 0, 4096);
      tail = (tail + chunk.toString("latin1")).slice(-4096);
    };
    if (opts.readerDelayMs) setTimeout(() => child.stdout.on("data", onData), opts.readerDelayMs);
    else child.stdout.on("data", onData);
    child.stderr.on("data", (c: Buffer) => { stderr += c.toString("latin1"); });
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      reject(new Error(`wrapper did not exit within ${String(opts.timeoutMs ?? 30000)}ms`));
    }, opts.timeoutMs ?? 30000);
    child.once("exit", (code) => {
      const exitMs = Date.now() - started;
      clearTimeout(timer);
      const finish = (): void => resolve({ code, exitMs, bytes, head, tail, stderr, closed });
      // A flushed pipe delivers its last chunks right after exit; a pipe held open by a
      // descendant never closes, so do not wait for it.
      if (closed) finish(); else child.stdout.once("close", finish);
      setTimeout(finish, 400);
    });
    child.once("error", reject);
  });
}

describe("WRAPPER under the host bash", () => {
  it("is run by a bash of at least version 5.1 (wait for the last process substitution)", { skip: process.platform === "linux" ? false : "Linux only" }, () => {
    assert.ok(version, "bash version readable");
    assert.ok(version.major > 5 || (version.major === 5 && version.minor >= 1), `bash ${String(version.major)}.${String(version.minor)}`);
  });

  it("returns the command's output and status quickly when it leaves no descendants", { skip: WRAPPER_SKIP }, async () => {
    const spill = nextSpill();
    const run = await runWrapper("echo hi; echo err >&2; exit 3", { spill });
    assert.equal(run.code, 3);
    assert.ok(run.exitMs <= 2000, `exit took ${String(run.exitMs)}ms`);
    assert.equal(run.head.split("\n").sort().join(","), ",err,hi");
    assert.equal(run.stderr, "");
    assert.equal(fs.readFileSync(spill, "utf8").split("\n").sort().join(","), ",err,hi");
  });

  it("preserves statuses including signals and prints no job notice", { skip: WRAPPER_SKIP }, async () => {
    const cases: Array<[string, number]> = [["exit 0", 0], ["exit 42", 42], ["kill -9 $$", 137], ["kill -TERM $$", 143], ["(exit 255)", 255]];
    for (const [command, code] of cases) {
      const run = await runWrapper(command);
      assert.equal(run.code, code, command);
      assert.doesNotMatch(run.head + run.stderr, /Killed|Terminated/, command);
      assert.ok(run.exitMs <= 2000, command);
    }
  });

  it("treats the command as data: quotes, substitutions and newlines are not interpreted by the wrapper", { skip: WRAPPER_SKIP }, async () => {
    const marker = path.join(scratchRoot, "must-not-exist");
    const run = await runWrapper(`printf '%s|' 'a"b$(touch ${marker})c'\nprintf done`);
    assert.equal(run.code, 0);
    assert.equal(run.head, `a"b$(touch ${marker})c|done`);
    assert.equal(fs.existsSync(marker), false);
  });

  it("gives the command a closed stdin", { skip: WRAPPER_SKIP }, async () => {
    const run = await runWrapper("cat; echo after");
    assert.equal(run.code, 0);
    assert.equal(run.head, "after\n");
  });

  it("returns within the writer grace when a descendant keeps the output fd open", { skip: WRAPPER_SKIP }, async () => {
    const pidFile = nextPidFile();
    try {
      const run = await runWrapper(`sleep 300 & echo $! > ${pidFile}; exit 5`, { timeoutMs: 20000 });
      assert.equal(run.code, 5);
      assert.ok(run.exitMs <= 11000, `exit took ${String(run.exitMs)}ms`);
      assert.ok(run.exitMs >= 4000, "the wrapper waits the bounded writer grace before giving up");
    } finally { killPidFile(pidFile); }
  });

  it("honours the test-only grace argument: a small value shortens the bounded wait, a bad value falls back to the default and is never evaluated", { skip: WRAPPER_SKIP }, async () => {
    const pidFiles: string[] = [];
    const holder = (): string => { const f = nextPidFile(); pidFiles.push(f); return `sleep 300 & echo $! > ${f}; exit 6`; };
    try {
      const short = await runWrapper(holder(), { graceTenths: "5", timeoutMs: 20000 });
      assert.equal(short.code, 6);
      assert.ok(short.exitMs < 3000, `a 0.5 s grace took ${String(short.exitMs)}ms`);
      // The value reaches an arithmetic context only after a digits-only check, so it cannot run code.
      const marker = path.join(scratchRoot, "grace-injection");
      const bad = await runWrapper(holder(), { graceTenths: `a[$(touch ${marker})]`, timeoutMs: 20000 });
      assert.equal(bad.code, 6);
      assert.ok(bad.exitMs >= 4000, `an invalid grace falls back to the 5 s default (${String(bad.exitMs)}ms)`);
      assert.equal(fs.existsSync(marker), false);
      // A 3-digit value passes the digits check but must be clamped to 50 (5 s), not honoured as 99.9 s.
      const huge = await runWrapper(holder(), { graceTenths: "999", timeoutMs: 20000 });
      assert.equal(huge.code, 6);
      // Compare with the invalid-value run (the 5 s default): a cap drift would move this by seconds.
      assert.ok(Math.abs(huge.exitMs - bad.exitMs) < 2000,
        `an over-large grace is clamped to the 5 s cap (${String(huge.exitMs)}ms vs ${String(bad.exitMs)}ms)`);
    } finally {
      for (const f of pidFiles) killPidFile(f);
    }
  });

  it("does not leak the wrapper's writer fd to the command or its descendants", { skip: WRAPPER_SKIP }, async () => {
    const pidFile = nextPidFile();
    try {
      const quiet = await runWrapper(`sleep 300 >/dev/null 2>&1 & echo $! > ${pidFile}; exit 0`, { timeoutMs: 20000 });
      assert.equal(quiet.code, 0);
      assert.ok(quiet.exitMs <= 2000, `a quiet background process held the wrapper for ${String(quiet.exitMs)}ms`);
      assert.ok(quiet.closed, "the transport closed promptly");
    } finally { killPidFile(pidFile); }
    // Piped so that $$ stays the command's shell (a bare ls would exec in place and list its own dir fd).
    const fds = await runWrapper('ls /proc/$$/fd | cat');
    assert.equal(fds.code, 0);
    const names = fds.head.trim().split(/\s+/).sort();
    assert.deepEqual(names, ["0", "1", "2"], "only stdio is open in the command");
  });

  it("refuses any pre-existing spill path (FIFO, symlink, dangling symlink) without blocking or writing through it", { skip: WRAPPER_SKIP, timeout: 90000 }, async () => {
    const dir = path.join(scratchRoot, "preplanted");
    fs.mkdirSync(dir, { recursive: true });
    const fifo = path.join(dir, "fifo");
    assert.equal(spawnSync("mkfifo", [fifo]).status, 0);
    const target = path.join(dir, "target");
    fs.writeFileSync(target, "KEEP");
    const link = path.join(dir, "link");
    fs.symlinkSync(target, link);
    const dangling = path.join(dir, "dangling");
    const missing = path.join(dir, "missing-target");
    fs.symlinkSync(missing, dangling);
    try {
      // The default grace (no override) is used: a blocked FIFO open would hold the writer, and
      // so the transport, for the whole grace, and the closed check proves it did not.
      for (const spill of [fifo, link, dangling]) {
        const run = await runWrapper("echo visible; exit 8", { spill, timeoutMs: 15000 });
        assert.equal(run.code, 8, spill);
        assert.equal(run.head, "visible\n", spill);
        assert.ok(run.exitMs <= 2000, `${spill} held the wrapper for ${String(run.exitMs)}ms`);
        assert.ok(run.closed, `${spill}: the transport closed promptly (no stuck spill open)`);
      }
    } finally {
      // Release any writer a broken refusal left blocked on the FIFO open (O_RDWR never blocks).
      try { fs.closeSync(fs.openSync(fifo, fs.constants.O_RDWR)); } catch { /* nothing to release */ }
    }
    assert.equal(fs.readFileSync(target, "utf8"), "KEEP");
    assert.equal(fs.existsSync(missing), false);
    assert.equal(fs.lstatSync(fifo).isFIFO(), true);
  });

  it("caps the spill at the byte limit while the transport still receives every byte", { skip: WRAPPER_SKIP }, async () => {
    const total = 200 * 1024 * 1024;
    const cap = 64 * 1024 * 1024;
    const spill = nextSpill();
    const run = await runWrapper(`yes x | head -c ${String(total)}; echo; echo SENTINEL`, { spill, cap, timeoutMs: 60000 });
    assert.equal(run.code, 0);
    assert.equal(run.bytes, total + 1 + "SENTINEL\n".length);
    assert.match(run.tail, /SENTINEL\n$/);
    assert.equal(fs.statSync(spill).size, cap);
    const fd = fs.openSync(spill, "r");
    try {
      const first = Buffer.alloc(8);
      fs.readSync(fd, first, 0, 8, 0);
      assert.equal(first.toString(), "x\nx\nx\nx\n");
    } finally { fs.closeSync(fd); }
  });

  it("keeps the exit status and transport output when the spill directory cannot be created", { skip: WRAPPER_SKIP }, async () => {
    const run = await runWrapper("echo visible; exit 9", { spill: UNWRITABLE_SPILL });
    assert.equal(run.code, 9);
    assert.equal(run.head, "visible\n");
    assert.equal(run.stderr, "");
  });

  it("never reuses an existing spill file and keeps the status and output", { skip: WRAPPER_SKIP }, async () => {
    const spill = nextSpill();
    fs.mkdirSync(path.dirname(spill), { recursive: true });
    fs.writeFileSync(spill, "OLD");
    const run = await runWrapper("echo fresh; exit 4", { spill });
    assert.equal(run.code, 4);
    assert.equal(run.head, "fresh\n");
    assert.equal(fs.readFileSync(spill, "utf8"), "OLD");
  });

  it("does not SIGPIPE a command when the spill cannot be written", { skip: WRAPPER_SKIP }, async () => {
    const run = await runWrapper("yes y | head -c 3000000; echo end", { spill: UNWRITABLE_SPILL });
    assert.equal(run.code, 0);
    assert.equal(run.bytes, 3000000 + 4);
  });

  it("still delivers the tail sentinel when the writer is delayed past the command's exit", { skip: WRAPPER_SKIP }, async () => {
    // The command returns at once; a background generator keeps writing, blocked until the
    // transport reader attaches. The wrapper must hold for the writer (within its grace).
    const run = await runWrapper("(yes z | head -c 3000000; echo; echo TAIL-SENTINEL) &", { readerDelayMs: 1500, timeoutMs: 20000 });
    assert.equal(run.code, 0);
    assert.ok(run.exitMs >= 1400, `the wrapper waited for the writer (${String(run.exitMs)}ms)`);
    assert.equal(run.bytes, 3000000 + 1 + "TAIL-SENTINEL\n".length);
    assert.match(run.tail, /TAIL-SENTINEL\n$/);
  });

  it("returns 124 for timeout -s INT, which needs the command to keep SIGINT", { skip: WRAPPER_SKIP }, async () => {
    const run = await runWrapper("timeout -s INT 1 sleep 30", { timeoutMs: 15000 });
    assert.equal(run.code, 124);
    assert.ok(run.exitMs < 5000);
  });

  it("waits for a command that stops itself and continues, with the wrapper idle meanwhile", { skip: WRAPPER_SKIP }, async () => {
    const child = spawn(BASH, ["-c", WRAPPER, "uzi-cmd", nextSpill(), "1024", "echo $$; kill -STOP $$; echo resumed; exit 7"],
      { stdio: ["pipe", "pipe", "pipe"], env: WRAPPER_ENV });
    let out = "";
    child.stdout.on("data", (c: Buffer) => { out += c.toString(); });
    const exited = new Promise<number | null>((resolve) => child.once("exit", (code) => resolve(code)));
    // utime + stime of the wrapper, in clock ticks, from the procfs stat line.
    const cpuTicks = (pid: number): number => {
      const stat = fs.readFileSync(`/proc/${String(pid)}/stat`, "utf8");
      const fields = stat.slice(stat.lastIndexOf(")") + 2).split(" ");
      return Number(fields[11]) + Number(fields[12]);
    };
    try {
      const deadline = Date.now() + 5000;
      while (!/^\d+\n/.test(out) && Date.now() < deadline) await new Promise((r) => setTimeout(r, 20));
      const cmdPid = Number(out.split("\n")[0]);
      assert.ok(cmdPid > 1, "the command reported its pid");
      await new Promise((r) => setTimeout(r, 400));
      const before = cpuTicks(child.pid!);
      await new Promise((r) => setTimeout(r, 1500));
      const spent = cpuTicks(child.pid!) - before;
      assert.ok(spent <= 5, `the wrapper used ${String(spent)} clock ticks while the command was stopped`);
      assert.equal(child.exitCode, null, "the wrapper has not returned while the command is merely stopped");
      process.kill(cmdPid, "SIGCONT");
      assert.equal(await exited, 7);
      assert.match(out, /resumed\n$/);
    } finally { child.kill("SIGKILL"); }
  });
});

// ─── Real image: the worker-uid root-start container only ────────────────────────────

// Required inventory must reject a skipped run: only the production worker with the split
// runner identity and the image supervisor can supply this evidence.
const INIT_SKIP = process.platform !== "linux" || process.getuid?.() !== 10001 ||
  process.env.UZI_UID_SPLIT !== "1"
  ? "requires Linux worker UID 10001 with the production runner 10002 split and the image supervisor"
  : false;

describe("real runner-uid command launcher", { skip: INIT_SKIP }, () => {
  it("launches the image supervisor at the runner uid and runs the wrapper", async () => {
    const dir = fs.mkdtempSync(path.join("/tmp", "uzi-cmd-launch-"));
    fs.chmodSync(dir, 0o777);
    try {
      const { handle } = await launchRunnerCommandRoot({
        command: "echo hi; exit 3", cwd: "/tmp", env: { PATH: "/usr/bin:/bin" },
        spillPath: path.join(dir, "uzi-cmd", "call.log"), spillCapBytes: 1024, supervisorBin: SUPERVISOR_BIN,
      }, { deadlines: { started: 10000, snapshot: 5000, dispose: 5000, exit: 5000 } });
      assert.equal(handle.started.uid, 10002);
      let out = "";
      handle.transport.stdout!.on("data", (c: Buffer) => { out += c.toString(); });
      assert.deepEqual(await handle.waitChild(10000), { event: "child_exit", code: 3 });
      const outcome = await handle.dispose(10000);
      assert.equal(outcome.clean, true);
      assert.equal(outcome.clean ? outcome.event.state : undefined, "drained");
      assert.equal(out, "hi\n");
      // The spill was created by the wrapper at the runner uid, so only the runner uid can read it.
      const readEnv = workerSpawnEnv({ PATH: "/usr/bin:/bin", LANG: "C" });
      const cat = runnerCommand("cat", ["--", path.join(dir, "uzi-cmd", "call.log")]);
      const spill = spawnSync(cat.command, cat.args, { env: readEnv, encoding: "utf8" });
      assert.equal(spill.status, 0, spill.stderr);
      assert.equal(spill.stdout, "hi\n");
    } finally {
      // The wrapper's runner-owned uzi-cmd/ subdir sits inside this worker-owned dir: remove it as
      // the runner uid first, or the worker uid cannot remove the dir.
      const rm = runnerCommand("rm", ["-rf", "--", path.join(dir, "uzi-cmd")]);
      spawnSync(rm.command, rm.args, { env: workerSpawnEnv({ PATH: "/usr/bin:/bin", LANG: "C" }), stdio: "ignore", timeout: 10_000 });
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("passes the launcher preflight", async () => {
    assert.deepEqual(await memoryLauncherPreflight(), { ok: true });
  });

  it("finds bash 5.1 or later where the wrapper runs", () => {
    const r = spawnSync("/bin/bash", ["-c", 'printf "%s.%s" "${BASH_VERSINFO[0]}" "${BASH_VERSINFO[1]}"'], { encoding: "utf8" });
    const [major, minor] = r.stdout.split(".").map(Number);
    assert.ok(major! > 5 || (major === 5 && minor! >= 1), r.stdout);
  });
});
