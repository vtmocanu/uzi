// issue #1866 M1: the run-start environment probe contract.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ChildProcess } from "node:child_process";
import {
  ENV_PROBE_ARGV,
  ENV_PROBE_SCRIPT,
  EnvProbeCleanupError,
  ProbeCleanupError,
  defaultEnvProbeSpawner,
  probeEnv,
  probeFacts,
  runEnvProbe,
  setDefaultEnvProbeSpawnerForTests,
  spawnRunnerProbe,
  type EnvProbeSpawner,
} from "../src/env-probe.js";
import { restoreHermeticEnvProbe } from "./setup/hermetic-proc.js";

type FakeFs = Parameters<typeof probeFacts>[0];

function errno(code: string): NodeJS.ErrnoException {
  const e = new Error(code) as NodeJS.ErrnoException;
  e.code = code;
  return e;
}

function fakeFs(over: Partial<FakeFs> = {}): FakeFs {
  return {
    readdirSync: () => ["1", "self"],
    readFileSync: () => PROC_PLAIN,
    mkdtempSync: (prefix: string) => `${prefix}abc`,
    rmdirSync: () => undefined,
    ...over,
  };
}

const PROC_PLAIN = "22 1 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw\n";

const ENV = { HOME: "/home/u", TMPDIR: "/tmp/u" };

describe("probeFacts", () => {
  it("reports ok when every check succeeds", () => {
    assert.deepEqual(probeFacts(fakeFs(), ENV), { proc: "ok", home: "ok", tmp: "ok" });
  });
  it("maps a readdir EACCES to limited", () => {
    const f = fakeFs({ readdirSync: () => { throw errno("EACCES"); } });
    assert.equal(probeFacts(f, ENV).proc, "limited");
  });
  it("maps a readdir ENOENT to unverified", () => {
    const f = fakeFs({ readdirSync: () => { throw errno("ENOENT"); } });
    assert.equal(probeFacts(f, ENV).proc, "unverified");
  });
  it("maps an empty listing to unverified", () => {
    assert.equal(probeFacts(fakeFs({ readdirSync: () => [] }), ENV).proc, "unverified");
  });
  // A hidepid/subset procfs still lists entries (e.g. only `self`) while hiding other rows.
  for (const [name, line] of [
    ["hidepid=2 (per-mount)", "22 1 0:21 / /proc rw,relatime,hidepid=2 - proc proc rw\n"],
    ["hidepid=invisible (super-block)", "22 1 0:21 / /proc rw,relatime - proc proc rw,hidepid=invisible\n"],
    ["subset=pid", "22 1 0:21 / /proc rw,relatime - proc proc rw,subset=pid\n"],
  ] as const) {
    it(`maps a listable /proc mounted ${name} to limited`, () => {
      const f = fakeFs({ readdirSync: () => ["self"], readFileSync: () => line });
      assert.equal(probeFacts(f, ENV).proc, "limited");
    });
  }
  it("keeps hidepid=0 / hidepid=off as ok", () => {
    for (const v of ["0", "off"]) {
      const f = fakeFs({ readFileSync: () => `22 1 0:21 / /proc rw - proc proc rw,hidepid=${v}\n` });
      assert.equal(probeFacts(f, ENV).proc, "ok");
    }
  });
  it("maps an unreadable or /proc-less mountinfo to unverified", () => {
    const unreadable = fakeFs({ readFileSync: () => { throw errno("EACCES"); } });
    assert.equal(probeFacts(unreadable, ENV).proc, "unverified");
    const noProc = fakeFs({ readFileSync: () => "30 1 0:5 / /sys rw - sysfs sysfs rw\n" });
    assert.equal(probeFacts(noProc, ENV).proc, "unverified");
  });
  for (const code of ["EACCES", "EPERM", "EROFS"]) {
    it(`maps a mkdtemp ${code} to limited`, () => {
      const f = fakeFs({ mkdtempSync: () => { throw errno(code); } });
      const r = probeFacts(f, ENV);
      assert.equal(r.home, "limited");
      assert.equal(r.tmp, "limited");
    });
  }
  it("maps any other mkdtemp error to unverified", () => {
    const f = fakeFs({ mkdtempSync: () => { throw errno("ENOSPC"); } });
    const r = probeFacts(f, ENV);
    assert.equal(r.home, "unverified");
    assert.equal(r.tmp, "unverified");
  });
  it("reports unverified for an unset or empty HOME / TMPDIR without touching fs", () => {
    let calls = 0;
    const f = fakeFs({ mkdtempSync: (p: string) => { calls++; return p; } });
    assert.deepEqual(probeFacts(f, { TMPDIR: "" }), { proc: "ok", home: "unverified", tmp: "unverified" });
    assert.equal(calls, 0);
  });
  it("maps an rmdir failure to unverified", () => {
    const f = fakeFs({ rmdirSync: () => { throw errno("EBUSY"); } });
    const r = probeFacts(f, ENV);
    assert.equal(r.home, "unverified");
    assert.equal(r.tmp, "unverified");
  });
  it("probes each dir under a .uzi-envprobe- prefix and removes what it made", () => {
    const made: string[] = [];
    const removed: string[] = [];
    const f = fakeFs({
      mkdtempSync: (p: string) => { made.push(p); return `${p}X`; },
      rmdirSync: (p: string) => { removed.push(p); },
    });
    probeFacts(f, ENV);
    assert.deepEqual(made, ["/home/u/.uzi-envprobe-", "/tmp/u/.uzi-envprobe-"]);
    assert.deepEqual(removed, ["/home/u/.uzi-envprobe-X", "/tmp/u/.uzi-envprobe-X"]);
  });
});

describe("ENV_PROBE_ARGV / ENV_PROBE_SCRIPT", () => {
  it("is exactly [execPath, -e, script]", () => {
    assert.deepEqual([...ENV_PROBE_ARGV], [process.execPath, "-e", ENV_PROBE_SCRIPT]);
    assert.ok(Object.isFrozen(ENV_PROBE_ARGV));
  });
  it("carries no transpiler helper", () => {
    assert.ok(!ENV_PROBE_SCRIPT.includes("__name"));
  });
  it("runs under a plain node -e and prints one valid line", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-envprobe-test-"));
    try {
      const out = execFileSync(ENV_PROBE_ARGV[0]!, ENV_PROBE_ARGV.slice(1), {
        env: { PATH: process.env.PATH ?? "", HOME: dir, TMPDIR: dir },
        encoding: "utf8",
        timeout: 20_000,
      });
      assert.ok(out.endsWith("\n"));
      assert.equal(out.split("\n").length, 2);
      const parsed = JSON.parse(out) as Record<string, unknown>;
      assert.equal(parsed.uzi_envprobe, 1);
      assert.equal(parsed.home, "ok");
      assert.equal(parsed.tmp, "ok");
      assert.ok(["ok", "limited", "unverified"].includes(parsed.proc as string));
      // The probe removed its own directories.
      assert.deepEqual(fs.readdirSync(dir), []);
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});

function line(proc: string, home: string, tmp: string): string {
  return `${JSON.stringify({ uzi_envprobe: 1, proc, home, tmp })}\n`;
}

const OPTS = { harness: "claude" as const, dockerWired: true };
const UNVERIFIED = { ...OPTS, proc: "unverified", home: "unverified", tmp: "unverified" };

function fixed(result: { code: number; stdout: string; cleanedUp: boolean }): EnvProbeSpawner {
  return async () => result;
}

describe("runEnvProbe", () => {
  it("returns the parsed facts on a clean run", async () => {
    const r = await runEnvProbe(fixed({ code: 0, stdout: line("ok", "ok", "ok"), cleanedUp: true }), {}, OPTS);
    assert.deepEqual(r, { ...OPTS, proc: "ok", home: "ok", tmp: "ok" });
  });
  it("passes a limited fact through", async () => {
    const r = await runEnvProbe(fixed({ code: 0, stdout: line("limited", "ok", "ok"), cleanedUp: true }), {}, {
      harness: "codex",
      dockerWired: false,
    });
    assert.deepEqual(r, { harness: "codex", dockerWired: false, proc: "limited", home: "ok", tmp: "ok" });
  });
  it("passes the fixed argv and a scrubbed env", async () => {
    let seenArgv: readonly string[] = [];
    let seenEnv: NodeJS.ProcessEnv = {};
    const spawner: EnvProbeSpawner = async (argv, env) => {
      seenArgv = argv;
      seenEnv = env;
      return { code: 0, stdout: line("ok", "ok", "ok"), cleanedUp: true };
    };
    const env = { HOME: "/h", TMPDIR: "/t", NODE_OPTIONS: "--require x", NODE_PATH: "/np", PATH: "/bin" };
    await runEnvProbe(spawner, env, OPTS);
    assert.deepEqual([...seenArgv], [process.execPath, "-e", ENV_PROBE_SCRIPT]);
    assert.equal(seenEnv.NODE_OPTIONS, undefined);
    assert.equal(seenEnv.NODE_PATH, undefined);
    assert.equal(seenEnv.HOME, "/h");
    assert.equal(seenEnv.TMPDIR, "/t");
    assert.equal(env.NODE_OPTIONS, "--require x", "the caller's env is not mutated");
    assert.deepEqual(probeEnv(env), { HOME: "/h", TMPDIR: "/t", PATH: "/bin" });
  });
  it("returns all unverified on timeout", async () => {
    const spawner: EnvProbeSpawner = (_a, _e, signal) =>
      new Promise((resolve) => {
        signal.addEventListener("abort", () => resolve({ code: -1, stdout: "", cleanedUp: true }));
      });
    assert.deepEqual(await runEnvProbe(spawner, {}, { ...OPTS, timeoutMs: 20 }), UNVERIFIED);
  });
  it("returns all unverified when the output arrives after an abort", async () => {
    const spawner: EnvProbeSpawner = (_a, _e, signal) =>
      new Promise((resolve) => {
        signal.addEventListener("abort", () => resolve({ code: 0, stdout: line("ok", "ok", "ok"), cleanedUp: true }));
      });
    assert.deepEqual(await runEnvProbe(spawner, {}, { ...OPTS, timeoutMs: 20 }), UNVERIFIED);
  });
  it("returns all unverified when the spawner throws", async () => {
    const spawner: EnvProbeSpawner = async () => {
      throw new Error("spawn failed");
    };
    assert.deepEqual(await runEnvProbe(spawner, {}, OPTS), UNVERIFIED);
  });
  it("returns all unverified on a non-zero exit", async () => {
    const r = await runEnvProbe(fixed({ code: 1, stdout: line("ok", "ok", "ok"), cleanedUp: true }), {}, OPTS);
    assert.deepEqual(r, UNVERIFIED);
  });
  const bad: Array<[string, string]> = [
    ["garbled", "not json\n"],
    ["no trailing newline", line("ok", "ok", "ok").trimEnd()],
    ["extra line", line("ok", "ok", "ok") + line("ok", "ok", "ok")],
    ["leading noise", `hello\n${line("ok", "ok", "ok")}`],
    ["oversize", `${JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok", tmp: "ok" }).padEnd(5000, " ")}\n`],
    ["wrong marker", `${JSON.stringify({ uzi_envprobe: 2, proc: "ok", home: "ok", tmp: "ok" })}\n`],
    ["bad status", line("yes", "ok", "ok")],
    ["extra key", `${JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok", tmp: "ok", x: 1 })}\n`],
    ["missing key", `${JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok" })}\n`],
    ["array", "[1]\n"],
    ["empty", ""],
  ];
  for (const [name, stdout] of bad) {
    it(`returns all unverified on ${name} output`, async () => {
      assert.deepEqual(await runEnvProbe(fixed({ code: 0, stdout, cleanedUp: true }), {}, OPTS), UNVERIFIED);
    });
  }
  it("fails closed when cleanup is unconfirmed", async () => {
    await assert.rejects(
      runEnvProbe(fixed({ code: 0, stdout: line("ok", "ok", "ok"), cleanedUp: false }), {}, OPTS),
      (err: unknown) => err instanceof EnvProbeCleanupError && /cleanup/.test(err.message) && /probe/.test(err.message),
    );
  });
  it("fails closed when the spawner throws ProbeCleanupError", async () => {
    const spawner: EnvProbeSpawner = async () => {
      throw new ProbeCleanupError("group still present");
    };
    await assert.rejects(runEnvProbe(spawner, {}, OPTS), EnvProbeCleanupError);
  });
});

// A minimal ChildProcess stand-in: emits `close` when told.
class FakeChild extends EventEmitter {
  pid: number | undefined;
  stdout = new PassThrough();
  stderr = new PassThrough();
  constructor(pid: number | undefined) {
    super();
    this.pid = pid;
  }
}

function harness(opts: {
  present: boolean | undefined;
  exitOnKill?: boolean;
  /** The code the child closes with after a kill (default null, i.e. killed by a signal). */
  closeCodeOnKill?: number | null;
  output?: string;
  pid?: number | undefined;
  spawnError?: boolean;
}) {
  const kills: number[] = [];
  let spawned: FakeChild | undefined;
  let spawnOpts: unknown;
  const spawner = spawnRunnerProbe({
    cwd: "/work",
    deps: {
      spawn: (_c, _a, o) => {
        spawnOpts = o;
        const child = new FakeChild("pid" in opts ? opts.pid : 4242);
        spawned = child;
        setImmediate(() => {
          if (opts.spawnError) {
            child.emit("error", errno("ENOENT"));
            child.emit("close", -2);
            return;
          }
          if (opts.output !== undefined) {
            child.stdout.write(opts.output);
            child.stdout.end();
            setImmediate(() => child.emit("close", 0));
          }
        });
        return child as unknown as ChildProcess;
      },
      killGroup: (pid) => {
        if (pid !== undefined) kills.push(pid);
        if (opts.exitOnKill && spawned) {
          const child = spawned;
          spawned = undefined; // close once: a follow-up kill finds nothing left to close
          setImmediate(() => child.emit("close", opts.closeCodeOnKill ?? null));
        }
        return true;
      },
      groupPresent: () => opts.present,
      closeWaitMs: 50,
      settleMs: 30,
    },
  });
  return { spawner, kills, spawnOpts: () => spawnOpts };
}

describe("spawnRunnerProbe", () => {
  it("resolves the output with cleanedUp true when the group is confirmed absent", async () => {
    const h = harness({ present: false, output: line("ok", "ok", "ok") });
    const r = await h.spawner(ENV_PROBE_ARGV, {}, new AbortController().signal);
    assert.deepEqual(r, { code: 0, stdout: line("ok", "ok", "ok"), cleanedUp: true });
    assert.deepEqual(h.kills, []);
    const o = h.spawnOpts() as { cwd: string; detached: boolean; stdio: unknown };
    assert.equal(o.cwd, "/work");
    assert.equal(o.detached, true);
    assert.deepEqual(o.stdio, ["ignore", "pipe", "pipe"]);
  });
  it("reports cleanedUp false when the group is still present after the kill", async () => {
    const h = harness({ present: true, exitOnKill: true });
    const ac = new AbortController();
    const p = h.spawner(ENV_PROBE_ARGV, {}, ac.signal);
    ac.abort();
    const r = await p;
    assert.equal(r.cleanedUp, false);
    assert.notEqual(r.code, 0);
    assert.deepEqual(h.kills, [4242, 4242], "the abort kill, then the one follow-up kill of the still-present group");
  });
  it("sends one follow-up kill and reports cleanedUp false when the group outlives a clean exit", async () => {
    const h = harness({ present: true, output: line("ok", "ok", "ok") });
    const r = await h.spawner(ENV_PROBE_ARGV, {}, new AbortController().signal);
    assert.equal(r.cleanedUp, false);
    assert.equal(r.code, 0, "no abort and no kill before close: the exit code is kept");
    assert.deepEqual(h.kills, [4242], "only the follow-up kill");
  });
  it("never reports a zero exit for a probe it killed", async () => {
    const h = harness({ present: false, exitOnKill: true, closeCodeOnKill: 0 });
    const ac = new AbortController();
    const p = h.spawner(ENV_PROBE_ARGV, {}, ac.signal);
    ac.abort();
    const r = await p;
    assert.equal(r.code, -1, "a killed probe that closed with 0 reads as a failure");
    assert.equal(r.cleanedUp, true);
  });
  it("reports cleanedUp false when presence is unknown", async () => {
    const h = harness({ present: undefined, exitOnKill: true });
    const ac = new AbortController();
    const p = h.spawner(ENV_PROBE_ARGV, {}, ac.signal);
    ac.abort();
    assert.equal((await p).cleanedUp, false);
  });
  it("reports cleanedUp true on an abort whose group is confirmed absent", async () => {
    const h = harness({ present: false, exitOnKill: true });
    const ac = new AbortController();
    const p = h.spawner(ENV_PROBE_ARGV, {}, ac.signal);
    ac.abort();
    const r = await p;
    assert.equal(r.cleanedUp, true);
    assert.notEqual(r.code, 0);
    assert.deepEqual(h.kills, [4242]);
  });
  it("settles within the bounded close wait when the child never closes", async () => {
    const h = harness({ present: false });
    const ac = new AbortController();
    const p = h.spawner(ENV_PROBE_ARGV, {}, ac.signal);
    ac.abort();
    const r = await p;
    assert.equal(r.code, -1);
    assert.equal(r.cleanedUp, true);
  });
  it("kills on stdout overflow and keeps the output past the cap for rejection", async () => {
    const h = harness({ present: false, exitOnKill: true, output: "x".repeat(10_000) });
    const r = await h.spawner(ENV_PROBE_ARGV, {}, new AbortController().signal);
    assert.ok(h.kills.includes(4242));
    assert.ok(r.stdout.length > 4096 && r.stdout.length <= 4097);
    assert.notEqual(r.code, 0);
  });
  it("treats a spawn that never produced a pid as cleaned up", async () => {
    const h = harness({ present: true, pid: undefined, spawnError: true });
    const r = await h.spawner(ENV_PROBE_ARGV, {}, new AbortController().signal);
    assert.equal(r.cleanedUp, true);
    assert.notEqual(r.code, 0);
  });
  it("feeds runEnvProbe: an unconfirmed cleanup rejects", async () => {
    const h = harness({ present: true, exitOnKill: true });
    await assert.rejects(runEnvProbe(h.spawner, {}, { ...OPTS, timeoutMs: 10 }), EnvProbeCleanupError);
  });
});

// Issue #1866 M2 (review follow-up): the real runner spawn, single-uid in tests, end to end.
describe("spawnRunnerProbe with the real runner spawn", () => {
  const OK_OPTS = { harness: "claude" as const, dockerWired: true, timeoutMs: 20_000 };
  function withDir(fn: (dir: string) => Promise<void>): Promise<void> {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-envprobe-real-"));
    return fn(dir).finally(() => fs.rmSync(dir, { recursive: true, force: true }));
  }
  it("confirms cleanup and measures a writable HOME and TMPDIR", async () =>
    withDir(async (dir) => {
      const facts = await runEnvProbe(
        spawnRunnerProbe({ cwd: dir }),
        { PATH: process.env.PATH ?? "", HOME: dir, TMPDIR: dir },
        OK_OPTS,
      );
      // Resolving at all proves the cleanup path: an unconfirmed cleanup throws.
      assert.equal(facts.home, "ok");
      assert.equal(facts.tmp, "ok");
      assert.deepEqual(fs.readdirSync(dir), [], "the probe removed its own directories");
    }));
  it("the production default (no test factory installed) is the real runner probe", async () =>
    withDir(async (dir) => {
      setDefaultEnvProbeSpawnerForTests(undefined);
      try {
        const facts = await runEnvProbe(
          defaultEnvProbeSpawner(dir),
          { PATH: process.env.PATH ?? "", HOME: dir, TMPDIR: dir },
          OK_OPTS,
        );
        assert.equal(facts.home, "ok");
        assert.equal(facts.tmp, "ok");
      } finally {
        restoreHermeticEnvProbe();
      }
    }));
  it("an installed test factory replaces the default", async () => {
    let cwdSeen = "";
    const fake: EnvProbeSpawner = async () => ({ code: 0, stdout: line("limited", "ok", "ok"), cleanedUp: true });
    setDefaultEnvProbeSpawnerForTests((cwd) => {
      cwdSeen = cwd;
      return fake;
    });
    try {
      const facts = await runEnvProbe(defaultEnvProbeSpawner("/work/x"), {}, OK_OPTS);
      assert.equal(facts.proc, "limited");
      assert.equal(cwdSeen, "/work/x");
    } finally {
      restoreHermeticEnvProbe();
    }
  });
});
