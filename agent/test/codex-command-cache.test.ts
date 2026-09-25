import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";

import { COMMAND_UID, setprivArgsForUid } from "../src/runner-uid.js";
import {
  CODEX_COMMAND_CACHE_ROOT,
  CommandCacheStartError,
  reapCodexCommandOrphans,
  removeCommandCache,
  startCommandCacheHolder,
  type SpawnStandaloneMode,
  type StandaloneModeDeps,
  type StandaloneModeProcess,
} from "../src/codex/launcher.js";

// Issue #1598 M5 — the worker side of the three standalone supervisor modes
// (--reap-orphans, --hold-cache, --remove-cache). NO real supervisor: a fake process
// prints the scripted stdout lines and exits like a ChildProcess.

const SUPERVISOR_BIN = "/usr/local/bin/uzi-codex-supervisor";
// Built at runtime: a literal UUID in source trips secret scanners (generic-api-key).
const TOKEN = randomUUID();
const CACHE_PATH = `${CODEX_COMMAND_CACHE_ROOT}/${TOKEN}`;

class FakeModeProcess extends EventEmitter {
  readonly pid = 5151;
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  readonly stdinWrites: string[] = [];
  stdinEnded = false;
  killed = 0;

  constructor() {
    super();
    this.stdin.on("data", (chunk: Buffer) => this.stdinWrites.push(chunk.toString("utf8")));
    this.stdin.on("finish", () => { this.stdinEnded = true; });
  }

  /** Any kill attempt is recorded: the contract is that the worker never kills a holder. */
  kill(): boolean {
    this.killed += 1;
    return true;
  }

  line(text: string): void {
    this.stdout.write(`${text}\n`);
  }

  /** Exit, then close stdio, like a ChildProcess. */
  finish(code: number): void {
    this.emit("exit", code, null);
    this.stdout.end();
    setImmediate(() => this.emit("close", code, null));
  }
}

interface SpawnRecord {
  command: string;
  args: readonly string[];
  env: NodeJS.ProcessEnv;
  stdio: readonly string[];
  cwd: string;
}

function fakeSpawn(script?: (p: FakeModeProcess) => void): { spawn: SpawnStandaloneMode; calls: SpawnRecord[]; procs: FakeModeProcess[] } {
  const calls: SpawnRecord[] = [];
  const procs: FakeModeProcess[] = [];
  const spawn: SpawnStandaloneMode = (command, args, options) => {
    calls.push({ command, args, env: options.env, stdio: options.stdio, cwd: options.cwd });
    const p = new FakeModeProcess();
    procs.push(p);
    if (script) setImmediate(() => script(p));
    return p as unknown as StandaloneModeProcess;
  };
  return { spawn, calls, procs };
}

const tick = (): Promise<void> => new Promise<void>((r) => setImmediate(r));
const closed = (p: FakeModeProcess): Promise<void> => new Promise<void>((r) => p.once("close", () => r()));

/** A one-line mode that prints `lines` then exits `code`. */
function oneShot(lines: readonly string[], code: number): (p: FakeModeProcess) => void {
  return (p) => {
    for (const l of lines) p.line(l);
    p.finish(code);
  };
}

const REAP_OK = JSON.stringify({
  event: "reap", scanned: 5, live: 1, removed: 2, retained: 1, foreign: 1, proof: "held", truncated: false, dirents_examined: 7,
});

/** A well-formed empty reap line with `overrides` applied (issue #1621: nine keys). */
const REAP_BASE: Readonly<Record<string, unknown>> = {
  event: "reap", scanned: 0, live: 0, removed: 0, retained: 0, foreign: 0, proof: "held", truncated: false, dirents_examined: 0,
};
const reapLine = (overrides: Record<string, unknown> = {}): string => JSON.stringify({ ...REAP_BASE, ...overrides });
const reapLineWithout = (key: string): string => {
  const o: Record<string, unknown> = { ...REAP_BASE };
  delete o[key];
  return JSON.stringify(o);
};

let savedSplit: string | undefined;
beforeEach(() => {
  savedSplit = process.env.UZI_UID_SPLIT;
  delete process.env.UZI_UID_SPLIT;
});
afterEach(() => {
  if (savedSplit === undefined) delete process.env.UZI_UID_SPLIT;
  else process.env.UZI_UID_SPLIT = savedSplit;
});

describe("reapCodexCommandOrphans", () => {
  it("runs the fixed reaper argv with a minimal env and parses the reap line", async () => {
    const f = fakeSpawn(oneShot([REAP_OK], 0));
    const result = await reapCodexCommandOrphans({ spawn: f.spawn });
    assert.deepEqual(result, {
      ok: true, scanned: 5, live: 1, removed: 2, retained: 1, foreign: 1, proof: "held", truncated: false, direntsExamined: 7,
    });
    assert.equal(f.calls.length, 1);
    const call = f.calls[0]!;
    assert.equal(call.command, SUPERVISOR_BIN);
    assert.deepEqual(call.args, ["--reap-orphans", "--expect-uid", String(COMMAND_UID), "--cache-root", CODEX_COMMAND_CACHE_ROOT]);
    assert.deepEqual(call.env, { PATH: "/usr/bin:/bin", LANG: "C" }, "nothing from the worker env crosses");
    assert.deepEqual(call.stdio, ["ignore", "pipe", "ignore"]);
  });

  it("runs as the command uid through setpriv under the uid split", async () => {
    process.env.UZI_UID_SPLIT = "1";
    const f = fakeSpawn(oneShot([REAP_OK], 0));
    await reapCodexCommandOrphans({ spawn: f.spawn });
    const call = f.calls[0]!;
    assert.equal(call.command, "/bin/setpriv");
    assert.deepEqual(call.args, [
      ...setprivArgsForUid(COMMAND_UID),
      SUPERVISOR_BIN, "--reap-orphans", "--expect-uid", String(COMMAND_UID), "--cache-root", CODEX_COMMAND_CACHE_ROOT,
    ]);
  });

  it("parses a truncated pass (issue #1621)", async () => {
    const line = reapLine({ scanned: 64, removed: 60, retained: 4, truncated: true, dirents_examined: 4096 });
    const f = fakeSpawn(oneShot([line], 0));
    assert.deepEqual(await reapCodexCommandOrphans({ spawn: f.spawn }), {
      ok: true, scanned: 64, live: 0, removed: 60, retained: 4, foreign: 0, proof: "held", truncated: true, direntsExamined: 4096,
    });
  });

  it("accepts scanned equal to dirents_examined", async () => {
    const f = fakeSpawn(oneShot([reapLine({ scanned: 3, removed: 3, dirents_examined: 3 })], 0));
    const result = await reapCodexCommandOrphans({ spawn: f.spawn });
    assert.equal(result.ok, true);
    if (result.ok) assert.equal(result.direntsExamined, 3);
  });

  it("reports a reap_error reason (exit 2)", async () => {
    const f = fakeSpawn(oneShot([JSON.stringify({ event: "reap_error", reason: "cache_root" })], 2));
    assert.deepEqual(await reapCodexCommandOrphans({ spawn: f.spawn }), { ok: false, reason: "cache_root" });
  });

  const garbage: Array<[string, readonly string[], number]> = [
    ["not JSON", ["reap ok"], 0],
    ["an unknown event", [JSON.stringify({ event: "reaped", scanned: 0 })], 0],
    ["an extra key", [reapLine({ x: 1 })], 0],
    ["a missing key", [reapLineWithout("foreign")], 0],
    ["counts that do not sum", [reapLine({ scanned: 9, dirents_examined: 9 })], 0],
    ["a negative count", [reapLine({ live: -1, removed: 1 })], 0],
    ["a fractional count", [reapLine({ scanned: 1.5, live: 1.5, dirents_examined: 2 })], 0],
    ["an unknown proof", [reapLine({ proof: "maybe" })], 0],
    ["the pre-#1621 seven-key line", [JSON.stringify({ event: "reap", scanned: 0, live: 0, removed: 0, retained: 0, foreign: 0, proof: "held" })], 0],
    ["a missing truncated", [reapLineWithout("truncated")], 0],
    ["a missing dirents_examined", [reapLineWithout("dirents_examined")], 0],
    ["truncated as the string \"false\"", [reapLine({ truncated: "false" })], 0],
    ["truncated as 0", [reapLine({ truncated: 0 })], 0],
    ["truncated null", [reapLine({ truncated: null })], 0],
    ["a negative dirents_examined", [reapLine({ dirents_examined: -1 })], 0],
    ["a fractional dirents_examined", [reapLine({ dirents_examined: 1.5 })], 0],
    ["dirents_examined as a string", [reapLine({ dirents_examined: "7" })], 0],
    ["scanned above dirents_examined", [reapLine({ scanned: 2, removed: 2, dirents_examined: 1 })], 0],
    ["a reap line with exit 2", [REAP_OK], 2],
    ["a reap_error with exit 0", [JSON.stringify({ event: "reap_error", reason: "list" })], 0],
    ["an unknown reap_error reason", [JSON.stringify({ event: "reap_error", reason: "whatever" })], 2],
    ["two lines", [REAP_OK, REAP_OK], 0],
    ["no line", [], 0],
    ["an oversized line", [`{"event":"reap","pad":"${"x".repeat(5000)}"}`], 0],
    ["a JSON array", ["[1,2]"], 0],
  ];
  for (const [label, lines, code] of garbage) {
    it(`fails closed on ${label}`, async () => {
      const f = fakeSpawn(oneShot(lines, code));
      const result = await reapCodexCommandOrphans({ spawn: f.spawn });
      assert.equal(result.ok, false);
      if (!result.ok) assert.equal(result.reason, "protocol");
    });
  }

  it("fails closed on a trailing partial line", async () => {
    const f = fakeSpawn((p) => {
      p.stdout.write(REAP_OK); // no newline
      p.finish(0);
    });
    const result = await reapCodexCommandOrphans({ spawn: f.spawn });
    assert.deepEqual(result.ok, false);
  });

  it("times out, kills the reaper as the command uid, waits for its close, and never throws", async () => {
    const f = fakeSpawn(); // never answers
    const kills: number[] = [];
    const result = await reapCodexCommandOrphans({
      spawn: f.spawn,
      timeoutMs: 20,
      killWaitMs: 2000,
      killAsCommandUid: (pid) => {
        kills.push(pid);
        setImmediate(() => f.procs[0]!.finish(137)); // SIGKILLed: exits, then closes
        return true;
      },
    });
    assert.deepEqual(result, { ok: false, reason: "timeout" });
    assert.deepEqual(kills, [5151]);
    assert.equal(f.procs[0]!.killed, 0, "never a direct kill (the worker lacks CAP_KILL over the command uid)");
  });

  it("a killed reaper that never closes is timeout_unkilled", async () => {
    const f = fakeSpawn();
    const result = await reapCodexCommandOrphans({ spawn: f.spawn, timeoutMs: 20, killWaitMs: 30, killAsCommandUid: () => true });
    assert.equal(result.ok, false);
    if (!result.ok) {
      assert.equal(result.reason, "timeout_unkilled");
      assert.match(String(result.detail), /kill delivered/);
    }
  });

  it("a kill that fails (or throws) is visible: unkilled when the reaper stays, noted when it closes anyway", async () => {
    for (const kill of [(): boolean => false, (): boolean => { throw new Error("EPERM"); }]) {
      const stays = fakeSpawn();
      const r1 = await reapCodexCommandOrphans({ spawn: stays.spawn, timeoutMs: 20, killWaitMs: 30, killAsCommandUid: kill });
      assert.equal(r1.ok, false);
      if (!r1.ok) {
        assert.equal(r1.reason, "timeout_unkilled");
        assert.match(String(r1.detail), /kill not delivered/);
      }
    }
    const closes = fakeSpawn();
    const r2 = await reapCodexCommandOrphans({
      spawn: closes.spawn,
      timeoutMs: 20,
      killWaitMs: 2000,
      killAsCommandUid: () => { setImmediate(() => closes.procs[0]!.finish(0)); return false; },
    });
    assert.deepEqual(r2, { ok: false, reason: "timeout", detail: "kill not delivered" });
  });

  it("the default kill runs `kill -KILL <pid>` as the command uid and checks its status", async () => {
    for (const split of [false, true]) {
      if (split) process.env.UZI_UID_SPLIT = "1";
      else delete process.env.UZI_UID_SPLIT;
      const f = fakeSpawn();
      const runs: Array<{ command: string; args: readonly string[] }> = [];
      const result = await reapCodexCommandOrphans({
        spawn: f.spawn,
        timeoutMs: 20,
        killWaitMs: 2000,
        runKill: (command, args) => {
          runs.push({ command, args });
          setImmediate(() => f.procs[0]!.finish(137));
          return { status: 0 };
        },
      });
      assert.deepEqual(result, { ok: false, reason: "timeout" });
      assert.deepEqual(runs, [split
        ? { command: "/bin/setpriv", args: [...setprivArgsForUid(COMMAND_UID), "kill", "-KILL", "5151"] }
        : { command: "kill", args: ["-KILL", "5151"] }]);
    }
    delete process.env.UZI_UID_SPLIT;
    for (const failed of [{ status: 1 }, { status: null, error: new Error("ETIMEDOUT") }]) {
      const f = fakeSpawn();
      const result = await reapCodexCommandOrphans({ spawn: f.spawn, timeoutMs: 20, killWaitMs: 30, runKill: () => failed });
      assert.equal(result.ok, false);
      if (!result.ok) {
        assert.equal(result.reason, "timeout_unkilled");
        assert.match(String(result.detail), /kill not delivered/);
      }
    }
  });

  it("an abort kills the running reaper and returns promptly", async () => {
    const f = fakeSpawn();
    const controller = new AbortController();
    const kills: number[] = [];
    const started = Date.now();
    const pass = reapCodexCommandOrphans({
      spawn: f.spawn,
      signal: controller.signal,
      killWaitMs: 2000,
      killAsCommandUid: (pid) => {
        kills.push(pid);
        setImmediate(() => f.procs[0]!.finish(137));
        return true;
      },
    });
    setTimeout(() => controller.abort(), 10);
    assert.deepEqual(await pass, { ok: false, reason: "aborted" });
    assert.deepEqual(kills, [5151]);
    assert.ok(Date.now() - started < 2000, "not the 6-minute reap deadline");

    const stays = fakeSpawn();
    const c2 = new AbortController();
    const unkilled = reapCodexCommandOrphans({ spawn: stays.spawn, signal: c2.signal, killWaitMs: 30, killAsCommandUid: () => true });
    setTimeout(() => c2.abort(), 10);
    const r2 = await unkilled;
    assert.equal(r2.ok, false);
    if (!r2.ok) assert.equal(r2.reason, "timeout_unkilled");
  });

  it("an already-aborted signal spawns nothing", async () => {
    const f = fakeSpawn();
    const controller = new AbortController();
    controller.abort();
    assert.deepEqual(await reapCodexCommandOrphans({ spawn: f.spawn, signal: controller.signal }), { ok: false, reason: "aborted" });
    assert.equal(f.calls.length, 0);
  });

  it("a spawn failure (binary absent) is a result, not a throw", async () => {
    const throwing: SpawnStandaloneMode = () => { throw new Error("ENOENT"); };
    const r1 = await reapCodexCommandOrphans({ spawn: throwing });
    assert.equal(r1.ok, false);
    if (!r1.ok) assert.equal(r1.reason, "spawn");
    const f = fakeSpawn((p) => { p.emit("error", new Error("spawn ENOENT")); });
    const r2 = await reapCodexCommandOrphans({ spawn: f.spawn, timeoutMs: 2000 });
    assert.equal(r2.ok, false);
    if (!r2.ok) assert.equal(r2.reason, "spawn");
  });
});

describe("removeCommandCache", () => {
  const cases: Array<[string, string, number, { state: string; reason: string }]> = [
    ["removed", JSON.stringify({ event: "cache_cleanup", state: "removed", reason: "" }), 0, { state: "removed", reason: "" }],
    ["absent", JSON.stringify({ event: "cache_cleanup", state: "absent", reason: "" }), 0, { state: "absent", reason: "" }],
    ["retained live", JSON.stringify({ event: "cache_cleanup", state: "retained", reason: "live" }), 3, { state: "retained", reason: "live" }],
    ["retained mismatch", JSON.stringify({ event: "cache_cleanup", state: "retained", reason: "mismatch" }), 3, { state: "retained", reason: "mismatch" }],
    ["cache_error root", JSON.stringify({ event: "cache_error", reason: "root" }), 2, { state: "error", reason: "root" }],
    // Garbage and holder-only vocabulary fail closed.
    ["the holder-only unattested word", JSON.stringify({ event: "cache_cleanup", state: "retained", reason: "unattested" }), 3, { state: "error", reason: "protocol" }],
    ["a holder-only cache_error", JSON.stringify({ event: "cache_error", reason: "lock" }), 2, { state: "error", reason: "protocol" }],
    ["removed with a reason", JSON.stringify({ event: "cache_cleanup", state: "removed", reason: "io" }), 0, { state: "error", reason: "protocol" }],
    ["removed with exit 3", JSON.stringify({ event: "cache_cleanup", state: "removed", reason: "" }), 3, { state: "error", reason: "protocol" }],
    ["an unknown event", JSON.stringify({ event: "cache_ready", path: CACHE_PATH }), 0, { state: "error", reason: "protocol" }],
    ["not JSON", "removed", 0, { state: "error", reason: "protocol" }],
  ];
  for (const [label, line, code, want] of cases) {
    it(`parses ${label}`, async () => {
      const f = fakeSpawn(oneShot([line], code));
      assert.deepEqual(await removeCommandCache(TOKEN, { spawn: f.spawn }), want);
    });
  }

  it("runs the fixed argv with the token", async () => {
    const f = fakeSpawn(oneShot([JSON.stringify({ event: "cache_cleanup", state: "removed", reason: "" })], 0));
    await removeCommandCache(TOKEN, { spawn: f.spawn });
    assert.deepEqual(f.calls[0]!.args, [
      "--remove-cache", "--expect-uid", String(COMMAND_UID), "--cache-root", CODEX_COMMAND_CACHE_ROOT, "--cache-token", TOKEN,
    ]);
  });

  it("rejects a non-lowercase-UUID token without spawning", async () => {
    for (const bad of [TOKEN.toUpperCase(), "../etc", "", `${TOKEN}/x`, TOKEN.replaceAll("-", "")]) {
      const f = fakeSpawn();
      assert.deepEqual(await removeCommandCache(bad, { spawn: f.spawn }), { state: "error", reason: "token" });
      assert.equal(f.calls.length, 0);
    }
  });

  it("is bounded: a silent remover is pending (never killed)", async () => {
    const f = fakeSpawn();
    assert.deepEqual(await removeCommandCache(TOKEN, { spawn: f.spawn, timeoutMs: 20 }), { state: "pending", reason: "" });
    assert.equal(f.procs[0]!.killed, 0);
  });
});

describe("startCommandCacheHolder", () => {
  const ready = JSON.stringify({ event: "cache_ready", path: CACHE_PATH });
  const readyDeps = (f: ReturnType<typeof fakeSpawn>): StandaloneModeDeps => ({ spawn: f.spawn, timeoutMs: 1000 });

  it("waits for cache_ready, and a release(true) writes the line, ends stdin, and returns removed", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    assert.deepEqual(f.calls[0]!.args, [
      "--hold-cache", "--expect-uid", String(COMMAND_UID), "--cache-root", CODEX_COMMAND_CACHE_ROOT, "--cache-token", TOKEN,
    ]);
    assert.deepEqual(f.calls[0]!.stdio, ["pipe", "pipe", "ignore"]);
    assert.equal(holder.path, CACHE_PATH);
    assert.equal(holder.alive(), true);
    assert.equal(p.stdinEnded, false, "stdin stays open for the whole run");

    p.stdin.once("finish", () => {
      p.line(JSON.stringify({ event: "cache_cleanup", state: "removed", reason: "" }));
      p.finish(0);
    });
    const result = await holder.release(true, 1000);
    assert.deepEqual(result, { state: "removed", reason: "" });
    assert.deepEqual(p.stdinWrites.join(""), `${JSON.stringify({ op: "release", drained: true })}\n`);
    assert.equal(p.stdinEnded, true);
    assert.equal(holder.alive(), false);
    assert.equal(await holder.release(false, 10), result, "idempotent: a second release returns the first result");
    assert.equal(p.stdinWrites.length, 1, "exactly one release line was ever written");
  });

  it("a release(false) sends drained:false and reports the holder's retained reason", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    p.stdin.once("finish", () => {
      p.line(JSON.stringify({ event: "cache_cleanup", state: "retained", reason: "unattested" }));
      p.finish(3);
    });
    assert.deepEqual(await holder.release(false, 1000), { state: "retained", reason: "unattested" });
    assert.deepEqual(p.stdinWrites.join(""), '{"op":"release","drained":false}\n');
  });

  it("a release the holder never answers returns pending and does NOT kill the holder", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    assert.deepEqual(await holder.release(true, 20), { state: "pending", reason: "" });
    assert.equal(p.killed, 0, "the holder keeps running and settles on its own");
    // A late answer is harmless.
    p.line(JSON.stringify({ event: "cache_cleanup", state: "removed", reason: "" }));
    p.finish(0);
    await closed(p);
  });

  it("a holder that exits mid-run is not alive, and release reports it without writing", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    p.emit("exit", 137, null);
    assert.equal(holder.alive(), false, "not alive from the exit on");
    await tick();
    assert.equal(p.stdinEnded, true, "every not-alive transition ends stdin");
    p.stdout.end();
    p.emit("close", 137, null);
    assert.deepEqual(await holder.release(true, 100), { state: "error", reason: "exited" });
    assert.equal(p.stdinWrites.length, 0);
  });

  const cleanupLine = (state: string, reason: string): string => JSON.stringify({ event: "cache_cleanup", state, reason });

  it("an exit that precedes the cleanup line's data still reports the line (settles on close)", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    p.stdin.once("finish", () => {
      p.emit("exit", 0, null);
      setImmediate(() => {
        p.line(cleanupLine("removed", ""));
        p.stdout.end();
        setImmediate(() => p.emit("close", 0, null));
      });
    });
    assert.deepEqual(await holder.release(true, 1000), { state: "removed", reason: "" });
  });

  it("the cleanup line before the exit is checked against the exit code", async () => {
    const cases: Array<[string, number | null, { state: string; reason: string }]> = [
      [cleanupLine("removed", ""), 0, { state: "removed", reason: "" }],
      [cleanupLine("retained", "unattested"), 3, { state: "retained", reason: "unattested" }],
      // Mismatched codes fail closed.
      [cleanupLine("removed", ""), 3, { state: "error", reason: "protocol" }],
      [cleanupLine("removed", ""), null, { state: "error", reason: "protocol" }],
      [cleanupLine("retained", "unattested"), 0, { state: "error", reason: "protocol" }],
      [cleanupLine("retained", "unattested"), 2, { state: "error", reason: "protocol" }],
    ];
    for (const [line, code, want] of cases) {
      const f = fakeSpawn((p) => p.line(ready));
      const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
      const p = f.procs[0]!;
      p.stdin.once("finish", () => {
        p.line(line);
        p.emit("exit", code, code === null ? "SIGKILL" : null);
        p.stdout.end();
        setImmediate(() => p.emit("close", code, code === null ? "SIGKILL" : null));
      });
      assert.deepEqual(await holder.release(true, 1000), want, `${line} exit ${String(code)}`);
    }
  });

  it("a third line (past the holder's 2-line cap) fails closed", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    p.stdin.once("finish", () => {
      p.line(cleanupLine("removed", ""));
      p.line(cleanupLine("removed", ""));
      p.finish(0);
    });
    assert.deepEqual(await holder.release(true, 1000), { state: "error", reason: "protocol" });
  });

  it("an extra line before release makes the holder not alive AND ends its stdin (no lock held to worker exit)", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    p.line(cleanupLine("retained", "unattested"));
    await tick();
    assert.equal(holder.alive(), false);
    assert.equal(p.stdinEnded, true, "the holder sees EOF and settles on its own");
    p.finish(3);
    assert.deepEqual(await holder.release(true, 1000), { state: "retained", reason: "unattested" });
    assert.equal(p.stdinWrites.length, 0, "no release line is written to a holder that is not alive");
  });

  it("garbage after ready marks the holder dead and ends its stdin (fail closed, unattested)", async () => {
    const f = fakeSpawn((p) => p.line(ready));
    const holder = await startCommandCacheHolder(TOKEN, readyDeps(f));
    const p = f.procs[0]!;
    p.line("x".repeat(5000));
    await new Promise<void>((r) => setImmediate(r));
    assert.equal(holder.alive(), false);
    assert.equal(p.stdinEnded, true);
    assert.deepEqual(await holder.release(true, 100), { state: "error", reason: "protocol" });
    assert.equal(p.stdinWrites.length, 0, "no drained attestation is written to a misbehaving holder");
  });

  const startFailures: Array<[string, (p: FakeModeProcess) => void, string]> = [
    ["a cache_error", (p) => { p.line(JSON.stringify({ event: "cache_error", reason: "lock" })); p.finish(2); }, "lock"],
    ["an unknown cache_error reason", (p) => { p.line(JSON.stringify({ event: "cache_error", reason: "nope" })); p.finish(2); }, "protocol"],
    ["a ready line for another path", (p) => p.line(JSON.stringify({ event: "cache_ready", path: `${CODEX_COMMAND_CACHE_ROOT}/other` })), "protocol"],
    ["a ready line with an extra key", (p) => p.line(JSON.stringify({ event: "cache_ready", path: CACHE_PATH, x: 1 })), "protocol"],
    ["not JSON", (p) => p.line("ready"), "protocol"],
    ["an early exit", (p) => p.finish(1), "exited"],
    ["a spawn error", (p) => p.emit("error", new Error("spawn ENOENT")), "spawn"],
  ];
  for (const [label, script, reason] of startFailures) {
    it(`throws CommandCacheStartError(${reason}) on ${label} and ends stdin`, async () => {
      const f = fakeSpawn(script);
      await assert.rejects(startCommandCacheHolder(TOKEN, readyDeps(f)), (e: unknown) => {
        assert.ok(e instanceof CommandCacheStartError);
        assert.equal(e.reason, reason);
        return true;
      });
      await new Promise<void>((r) => setImmediate(r));
      assert.equal(f.procs[0]!.stdinEnded, true);
      assert.equal(f.procs[0]!.stdinWrites.length, 0);
    });
  }

  it("times out without a ready line, ends stdin, and never kills", async () => {
    const f = fakeSpawn();
    await assert.rejects(startCommandCacheHolder(TOKEN, { spawn: f.spawn, timeoutMs: 20 }), (e: unknown) => {
      assert.ok(e instanceof CommandCacheStartError);
      assert.equal(e.reason, "timeout");
      return true;
    });
    await new Promise<void>((r) => setImmediate(r));
    assert.equal(f.procs[0]!.stdinEnded, true);
    assert.equal(f.procs[0]!.killed, 0);
    // A late exit after the deadline is not an unhandled rejection.
    f.procs[0]!.finish(3);
    await new Promise<void>((r) => setImmediate(r));
  });

  it("rejects an invalid token without spawning", async () => {
    const f = fakeSpawn();
    await assert.rejects(startCommandCacheHolder("NOT-A-UUID", { spawn: f.spawn }), /token/);
    assert.equal(f.calls.length, 0);
  });
});
