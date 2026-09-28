import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import { spawn, type ChildProcess } from "node:child_process";
import {
  LiveAttemptRegistry,
  newRunAttempt,
  procfsTable,
  quiesceRunAttempt,
  reapProcesses,
  reapRunProcesses,
  runnerHelperTmp,
  sanitizeForLog,
  scanOnce,
  teardownDocker,
  type HelperSpawn,
  type HelperTmp,
  type ProcTable,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { defaultCheckRunner } from "../src/self-improve.js";
import { makeFakeProcRoot, scopedRealView, withQuiescenceView } from "./fake-proc.js";
import { realProcfsSkip } from "./real-procfs.js";
import { RUN_ATTEMPT_ENV, registerWorkerRunnerRoot, workerSpawnNonce } from "../src/worker-spawn-mark.js";

// issue #1783 review round — the hardening of the run-quiescence reaper: attribution of a
// concurrent Codex attempt's non-dumpable supervisor, the worker-mark/attempt-marker rule, the
// helper's stdin request / private TMPDIR / single bounded verdict, the Docker body cap and host
// forms, log sanitization, and that an unmarked self-improve check's leak is reaped.

const HAS_PROCFS = process.platform === "linux" && fs.existsSync(path.join("/", "proc", "self", "status"));
const ME = process.getuid?.() ?? 0;
const RUNNER = 10002;
const CLONE = "/data/runner/github.com+o+r/issue-17";
const KEY = "github.com+o+r/issue-17";
const SELF = 900;

let tmp: string;
before(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-quiesce-hard-"));
});
const strays: number[] = [];
const spawned: ChildProcess[] = [];
after(() => {
  for (const pid of strays) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      /* gone */
    }
  }
  for (const c of spawned) {
    try {
      c.kill("SIGKILL");
    } catch {
      /* gone */
    }
  }
  fs.rmSync(tmp, { recursive: true, force: true });
});

// ─── injected process table ────────────────────────────────────────────────────────────────

interface FakeProc {
  uid: number;
  ppid?: number;
  pgid?: number;
  sid?: number;
  comm?: string;
  cwd?: string | "EACCES";
  env?: Record<string, string> | "EACCES";
  /** `stat` field 22 (start time); default {@link DEFAULT_START}. */
  start?: number;
}

const DEFAULT_START = 1000;

function errno(code: string): Error {
  return Object.assign(new Error(code), { code });
}

function fakeTable(procs: Record<number, FakeProc>): ProcTable {
  const get = (pid: number): FakeProc => {
    const p = procs[pid];
    if (!p) throw errno("ENOENT");
    return p;
  };
  return {
    listPids: () => Object.keys(procs).map(Number),
    readStatus: (pid) => {
      const p = get(pid);
      return `Name:\t${p.comm ?? `proc${pid}`}\nState:\tS (sleeping)\nUid:\t${p.uid}\t${p.uid}\t${p.uid}\t${p.uid}\n`;
    },
    readEnviron: (pid) => {
      const e = get(pid).env ?? {};
      if (typeof e === "string") throw errno(e);
      return Object.entries(e).map(([k, v]) => `${k}=${v}\u0000`).join("");
    },
    readCwd: (pid) => {
      const c = get(pid).cwd ?? "/";
      if (c === "EACCES") throw errno(c);
      return c;
    },
    readStat: (pid) => {
      const p = get(pid);
      // Fields 3..22 (state … starttime): 15 zero fields sit between the session and field 22.
      return `${pid} (proc ${pid}) S ${p.ppid ?? 1} ${p.pgid ?? pid} ${p.sid ?? pid} ${"0 ".repeat(15)}${p.start ?? DEFAULT_START} 0 0`;
    },
  };
}

/** Fake-clock reap deps over `procs` (a SIGKILL removes the pid). */
function fakeDeps(procs: Record<number, FakeProc>) {
  let t = 0;
  return {
    viaHelper: false,
    table: fakeTable(procs),
    selfPid: SELF,
    now: () => t,
    sleep: async (ms: number) => {
      t += ms;
    },
    kill: (pid: number) => {
      delete procs[pid];
    },
  };
}

function fakeReq(extra: Partial<ScanRequest> = {}): ScanRequest {
  return {
    mode: "own",
    targetUid: RUNNER,
    targetKey: KEY,
    targetPaths: [CLONE],
    ownMarker: "run-17:own",
    liveMarkers: [],
    liveRoots: [],
    workerNonce: "nonce",
    ...extra,
  };
}

// ─── item 2: a concurrent Codex attempt ────────────────────────────────────────────────────

describe("A-codex-sibling: a concurrent Codex attempt's non-dumpable supervisor", () => {
  const SUP = 300;
  /** The Codex provider supervisor (runner uid, non-dumpable) and its app-server child. The
   *  supervisor is NOT detached: it shares the worker's process group and session (5 here). */
  const codexProcs = (): Record<number, FakeProc> => ({
    [SELF]: { uid: RUNNER, ppid: 1 },
    [SUP]: { uid: RUNNER, ppid: 1, pgid: 5, sid: 5, cwd: "EACCES", env: "EACCES" },
    301: { uid: RUNNER, ppid: SUP, pgid: 5, sid: 5, cwd: "EACCES", env: "EACCES" },
  });
  const claude = newRunAttempt("run-17", 1, CLONE, () => []);

  function quiesce(registry: LiveAttemptRegistry, procs: Record<number, FakeProc>) {
    return quiesceRunAttempt(
      { mode: "own", attempt: claude, cloneKey: KEY, targetPaths: [CLONE], processes: true, dockerHost: undefined, registry },
      { targetUid: RUNNER, reap: fakeDeps(procs) },
    );
  }

  it("does not make a Claude run's reap unverified when the Codex attempt records its supervisor", async () => {
    const registry = new LiveAttemptRegistry();
    registry.add(claude);
    registry.add(newRunAttempt("run-99", 1, "/data/runner/github.com+o+r/issue-99", () => [{ pid: SUP, startTime: DEFAULT_START }]));
    const out = await quiesce(registry, codexProcs());
    assert.equal(out.process?.state, "quiescent", out.process?.detail);
  });

  it("control: the same supervisor with no recorded root is unverified (the pre-fix behaviour)", async () => {
    const registry = new LiveAttemptRegistry();
    registry.add(claude);
    registry.add(newRunAttempt("run-99", 1, "/data/runner/github.com+o+r/issue-99", () => []));
    const out = await quiesce(registry, codexProcs());
    assert.equal(out.process?.state, "unverified");
    assert.deepEqual(out.process?.processes.map((p) => p.pid).sort(), [SUP, 301]);
  });

  it("a worker-launched runner-uid root recorded in the registry is attributable with no attempt at all", async () => {
    const registry = new LiveAttemptRegistry();
    registry.add(claude);
    const unregister = registerWorkerRunnerRoot(SUP, () => DEFAULT_START);
    try {
      assert.equal((await quiesce(registry, codexProcs())).process?.state, "quiescent");
    } finally {
      unregister();
    }
    assert.equal((await quiesce(registry, codexProcs())).process?.state, "unverified", "unregistered ⇒ unattributable again");
  });

  it("L1: a stale recorded root whose pid was recycled (same pid, a different start time) exempts nothing", async () => {
    // The Codex supervisor recorded as SUP exited and the kernel handed SUP to an unrelated
    // non-dumpable runner-uid process (live start time DEFAULT_START, recorded DEFAULT_START - 1).
    const stale = { pid: SUP, startTime: DEFAULT_START - 1 };
    const viaAttempt = new LiveAttemptRegistry();
    viaAttempt.add(claude);
    viaAttempt.add(newRunAttempt("run-99", 1, "/data/runner/github.com+o+r/issue-99", () => [stale]));
    const out = await quiesce(viaAttempt, codexProcs());
    assert.equal(out.process?.state, "unverified", "an attempt's stale root does not exempt the recycled pid");
    assert.deepEqual(out.process?.processes.map((p) => p.pid).sort(), [SUP, 301]);

    const viaWorker = new LiveAttemptRegistry();
    viaWorker.add(claude);
    const unregister = registerWorkerRunnerRoot(SUP, () => DEFAULT_START - 1);
    try {
      const w = await quiesce(viaWorker, codexProcs());
      assert.equal(w.process?.state, "unverified", "a worker-launched stale root does not exempt the recycled pid");
    } finally {
      unregister();
    }
  });

  it("an unattributed non-dumpable process still makes it unverified", async () => {
    const registry = new LiveAttemptRegistry();
    registry.add(claude);
    registry.add(newRunAttempt("run-99", 1, "/data/runner/github.com+o+r/issue-99", () => [{ pid: SUP, startTime: DEFAULT_START }]));
    const procs = { ...codexProcs(), 400: { uid: RUNNER, ppid: 1, cwd: "EACCES", env: "EACCES" } as FakeProc };
    const out = await quiesce(registry, procs);
    assert.equal(out.process?.state, "unverified");
    assert.deepEqual(out.process?.processes.map((p) => p.pid), [400]);
  });
});

// ─── item 6: the worker mark never exempts an attempt-marked process ───────────────────────

describe("A-nonce-forgery: the worker mark exempts only a process with NO attempt marker", () => {
  it("an attempt-marked process that also carries the nonce is classified (and reaped) like any other", () => {
    const procs: Record<number, FakeProc> = {
      10: { uid: RUNNER, cwd: CLONE, env: { UZI_WORKER_SPAWN: "nonce", [RUN_ATTEMPT_ENV]: "run-17:own" } },
      11: { uid: RUNNER, cwd: CLONE, env: { UZI_WORKER_SPAWN: "nonce" } },
    };
    const res = scanOnce(fakeReq(), fakeTable(procs), SELF);
    assert.deepEqual(res.kill.map((p) => [p.pid, p.reason]), [[10, "own_attempt"]], "the genuine worker op (11) is still skipped");
  });
});

// ─── item 8: log sanitization ──────────────────────────────────────────────────────────────

describe("A-sanitize: comm, cwd and helper strings never reach a log raw", () => {
  it("sanitizeForLog replaces C0/C1/DEL/bidi code points and caps the length", () => {
    assert.equal(sanitizeForLog("a\u001b[31mb\u0085c\u007fd‮e⁦f‎g"), "a?[31mb?c?d?e?f?g");
    assert.equal(sanitizeForLog("x".repeat(10), 4), "xxxx...");
    assert.equal(sanitizeForLog("plain/path issue-17"), "plain/path issue-17");
  });

  it("nit: no '...' at exactly max length", () => {
    assert.equal(sanitizeForLog("xxxx", 4), "xxxx", "a string of exactly max length is complete, not truncated");
    assert.equal(sanitizeForLog("xxxxx", 4), "xxxx...");
  });

  it("nit: a surrogate pair is never split by the cap", () => {
    // "ab" + U+1F600 (a surrogate pair, 2 UTF-16 units) + "c": a cap of 3 must not keep half the pair.
    const cut = sanitizeForLog("ab\u{1F600}c", 3);
    assert.ok(![...cut].some((ch) => { const c = ch.codePointAt(0)!; return c >= 0xd800 && c <= 0xdfff; }), "no lone surrogate");
    assert.equal(cut, "ab...");
    assert.equal(sanitizeForLog("ab\u{1F600}c", 4), "ab\u{1F600}...");
  });

  it("nit: U+2028/U+2029 are neutralized", () => {
    assert.equal(sanitizeForLog("a\u2028b\u2029c"), "a?b?c", "line/paragraph separators are replaced");
  });

  it("a scanned comm and cwd with control/bidi characters are sanitized in the result", () => {
    const evilCwd = `${CLONE}/‮gnp.exe\u001b[2J`;
    const procs: Record<number, FakeProc> = {
      12: { uid: RUNNER, comm: "sle\u001b]0;pwn\u0007ep", cwd: evilCwd, env: { [RUN_ATTEMPT_ENV]: "run-17:own" } },
    };
    const [hit] = scanOnce(fakeReq(), fakeTable(procs), SELF).kill;
    assert.ok(hit);
    assert.equal(hit.comm, "sle?]0;pwn?ep");
    assert.equal(hit.cwd, `${CLONE}/?gnp.exe?[2J`);
  });
});

// ─── items 6/7: the helper request, TMPDIR and verdict ─────────────────────────────────────

/** A HelperTmp that makes a real private dir and records its removal. */
function recordingTmp(): HelperTmp & { made: string[]; removed: string[] } {
  const made: string[] = [];
  const removed: string[] = [];
  return {
    made,
    removed,
    make: () => {
      const d = fs.mkdtempSync(path.join(tmp, "helper-tmp-"));
      made.push(d);
      return d;
    },
    remove: (d) => {
      removed.push(d);
      fs.rmSync(d, { recursive: true, force: true });
    },
  };
}

/** A stand-in helper: `script` runs under node with the helper's env and cwd; stdin is piped. */
function scriptHelper(script: string, seen?: { args?: string[]; env?: NodeJS.ProcessEnv }): HelperSpawn {
  return (_command, args, opts) => {
    if (seen) {
      seen.args = args;
      seen.env = opts.env;
    }
    const c = spawn(process.execPath, ["-e", script], { cwd: opts.cwd, env: opts.env, stdio: ["pipe", "pipe", "pipe"] });
    spawned.push(c);
    return c;
  };
}

const VERDICT = (detail: string) => JSON.stringify({ state: "quiescent", processes: [], killed: [], detail });

describe("A-helper-tmp: the uid-split TMPDIR setup runs mktemp/rm asynchronously and time-bounded", () => {
  /** A runner-command wrapper for a single-uid host: runs the command as this uid, unchanged. */
  const passthrough = (command: string, args: string[]) => ({ command, args });

  it("mktemp (through the injected runner command) makes a private dir inside base; rm removes it", async () => {
    const base = fs.mkdtempSync(path.join(tmp, "split-base-"));
    const seen: string[][] = [];
    const t = runnerHelperTmp(base, (command, args) => {
      seen.push([command, ...args]);
      return passthrough(command, args);
    });
    const dir = await t.make();
    assert.equal(path.dirname(dir), base);
    assert.match(path.basename(dir), /^uzi-quiesce-/);
    assert.equal(fs.statSync(dir).mode & 0o777, 0o700, "mktemp -d makes it 0700");
    await t.remove(dir);
    assert.equal(fs.existsSync(dir), false);
    assert.deepEqual(seen.map((a) => a[0]), ["mktemp", "rm"], "both steps went through the runner command");
  });

  it("a hung mktemp does not block the event loop, is killed at its deadline, and the reap is unverified", async () => {
    const base = fs.mkdtempSync(path.join(tmp, "split-hang-"));
    const pidFile = path.join(base, "hung.pid");
    const hang = () => ({ command: "sh", args: ["-c", `echo $$ > ${pidFile}; exec sleep 30`] });
    let ticks = 0;
    const ticker = setInterval(() => (ticks += 1), 10);
    const started = Date.now();
    let err: unknown;
    try {
      await runnerHelperTmp(base, hang, { makeMs: 300 }).make();
    } catch (e) {
      err = e;
    } finally {
      clearInterval(ticker);
    }
    assert.match(String((err as Error | undefined)?.message), /timed out/);
    assert.ok(Date.now() - started < 5_000, "bounded by its deadline");
    assert.ok(ticks >= 5, `the event loop kept running while mktemp hung (ticks=${ticks})`);
    const hungPid = Number(fs.readFileSync(pidFile, "utf8").trim());
    // hungPid is this process's own spawned child, so node reaps it once it dies and
    // process.kill(pid, 0) then reports ESRCH. A failed status read never counts as gone by itself:
    // ENOENT (also what a host with no proc filesystem returns for every pid) and a read denied by
    // the sandbox (EACCES) both fall back to the signal probe instead of passing vacuously.
    const hungGone = (): boolean => {
      try {
        return /^State:\s*[ZX]/m.test(procfsTable.readStatus(hungPid));
      } catch (e) {
        if ((e as NodeJS.ErrnoException).code === "ESRCH") return true;
      }
      try {
        process.kill(hungPid, 0);
        return false;
      } catch (e) {
        return (e as NodeJS.ErrnoException).code === "ESRCH";
      }
    };
    const gone = await (async () => {
      for (let i = 0; i < 100; i++) {
        if (hungGone()) return true;
        await new Promise((r) => setTimeout(r, 20));
      }
      return false;
    })();
    assert.ok(gone, "the hung mktemp's group was killed at the deadline");

    const r = await reapRunProcesses(fakeReq(), {
      viaHelper: true,
      spawnHelper: scriptHelper(`process.stdout.write(${JSON.stringify(`${VERDICT("x")}\n`)});`),
      helperTmp: runnerHelperTmp(base, () => ({ command: "sleep", args: ["30"] }), { makeMs: 200 }),
    });
    assert.equal(r.state, "unverified");
    assert.match(r.detail, /TMPDIR unavailable/);
  });

  it("an mktemp answer outside base (or a failed exit) is refused", async () => {
    const base = fs.mkdtempSync(path.join(tmp, "split-outside-"));
    await assert.rejects(runnerHelperTmp(base, () => ({ command: "sh", args: ["-c", "echo /etc"] })).make(), /mktemp as the runner exited/);
    await assert.rejects(runnerHelperTmp(base, () => ({ command: "sh", args: ["-c", "exit 3"] })).make(), /exited 3/);
    await assert.rejects(runnerHelperTmp(base, () => ({ command: "sh", args: ["-c", `echo ${base}`] })).make(), /mktemp as the runner/);
  });
});

describe("A-helper: request on stdin, private TMPDIR, tsx cache off, one bounded verdict", () => {
  it("serializes the solitary permission only for an explicit clear claim and empty registry under uid split", async () => {
    const previous = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    try {
      const registry = new LiveAttemptRegistry();
      const echo = `let b = ""; process.stdin.on("data", (c) => (b += c)); process.stdin.on("end", () => {
        const r = JSON.parse(b);
        process.stdout.write(JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: String(r.mayKillUnreadableUnattributed) }) + "\\n");
      });`;
      const check = async (otherClaimInFlight?: boolean) => {
        const r = await quiesceRunAttempt(
          { mode: "own", attempt: undefined, cloneKey: KEY, targetPaths: [CLONE], processes: true,
            dockerHost: undefined, registry, otherClaimInFlight },
          { targetUid: RUNNER, reap: { viaHelper: true, spawnHelper: scriptHelper(echo), helperTmp: recordingTmp() } },
        );
        return r.process?.detail;
      };
      assert.equal(await check(false), "true");
      assert.equal(await check(true), "false");
      assert.equal(await check(), "false");
      registry.add(newRunAttempt("run-other", 1, "/other", () => []));
      assert.equal(await check(false), "false");
    } finally {
      if (previous === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = previous;
    }
  });

  it("the actual serialized helper accepts omitted and boolean permission, rejecting wrong types", async () => {
    const root = makeFakeProcRoot();
    try {
      for (const value of [undefined, false, true, "false", 1]) {
        const request = fakeReq();
        if (value !== undefined) (request as unknown as { mayKillUnreadableUnattributed: unknown }).mayKillUnreadableUnattributed = value;
        const r = await withQuiescenceView({ procRoot: root }, () =>
          reapRunProcesses(request, { viaHelper: true, helperTmp: recordingTmp() }),
        );
        if (typeof value === "string" || typeof value === "number") {
          assert.equal(r.state, "unverified");
          assert.equal(r.detail, "helper request invalid");
        } else {
          assert.equal(r.state, "quiescent", r.detail);
        }
      }
    } finally {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("the request rides stdin (never argv), TMPDIR is a fresh private dir removed after, TSX_DISABLE_CACHE=1", async () => {
    const seen: { args?: string[]; env?: NodeJS.ProcessEnv } = {};
    // Echo the request's targetKey back as the verdict detail: proof the request arrived on stdin.
    const echo = `let b = ""; process.stdin.on("data", (c) => (b += c)); process.stdin.on("end", () => {
      const r = JSON.parse(b);
      process.stdout.write(JSON.stringify({ state: "quiescent", processes: [], killed: [], detail: r.targetKey + "|" + r.workerNonce }) + "\\n");
    });`;
    const helperTmp = recordingTmp();
    const req = fakeReq({ workerNonce: "n0nce-canary" });
    const r = await reapRunProcesses(req, { viaHelper: true, spawnHelper: scriptHelper(echo, seen), helperTmp });
    assert.equal(r.state, "quiescent", r.detail);
    assert.equal(r.detail, `${KEY}|n0nce-canary`, "the helper read its request from stdin");
    const argv = (seen.args ?? []).join(" ");
    assert.doesNotMatch(argv, /n0nce-canary|targetKey/, "the request is not in the world-readable argv");
    assert.doesNotMatch(argv, /[A-Za-z0-9+/]{40,}={0,2}$/, "no base64 payload in argv");
    assert.equal(seen.env?.TSX_DISABLE_CACHE, "1");
    assert.equal(helperTmp.made.length, 1);
    assert.equal(seen.env?.TMPDIR, helperTmp.made[0], "TMPDIR is the fresh private dir, not the shared runner TMPDIR");
    assert.deepEqual(helperTmp.removed, helperTmp.made, "the private dir is removed after the helper answered");
    assert.equal(fs.existsSync(helperTmp.made[0]!), false);
  });

  it("two verdict lines are unverified, even when both are well-formed", async () => {
    const two = `process.stdout.write(${JSON.stringify(`${VERDICT("real")}\n${VERDICT("forged")}\n`)});`;
    const r = await reapRunProcesses(fakeReq(), { viaHelper: true, spawnHelper: scriptHelper(two), helperTmp: recordingTmp() });
    assert.equal(r.state, "unverified");
    assert.match(r.detail, /2 lines/);
  });

  it("stdout above 1 MiB is unverified (the helper is not trusted to stop)", async () => {
    const big = `process.stdout.write("x".repeat(2 * 1024 * 1024)); setTimeout(() => {}, 5000);`;
    const r = await reapRunProcesses(fakeReq(), { viaHelper: true, spawnHelper: scriptHelper(big), helperTmp: recordingTmp() });
    assert.equal(r.state, "unverified");
    assert.match(r.detail, /exceeded 1 MiB/);
  });

  it("a verdict's strings are sanitized on receipt (they crossed a uid boundary)", async () => {
    const v = JSON.stringify({
      state: "survivors",
      processes: [{ pid: 7, uid: RUNNER, comm: "a\u001bb", cwd: "/x‮y", reason: "own_attempt" }],
      killed: [],
      detail: "d\u0007",
    });
    const r = await reapRunProcesses(fakeReq(), {
      viaHelper: true,
      spawnHelper: scriptHelper(`process.stdout.write(${JSON.stringify(`${v}\n`)});`),
      helperTmp: recordingTmp(),
    });
    assert.equal(r.state, "survivors");
    assert.deepEqual([r.processes[0]?.comm, r.processes[0]?.cwd, r.detail], ["a?b", "/x?y", "d?"]);
  });

  it("a TMPDIR that cannot be made is unverified, and nothing is spawned", async () => {
    let spawnedHelper = false;
    const r = await reapRunProcesses(fakeReq(), {
      viaHelper: true,
      spawnHelper: (...a) => {
        spawnedHelper = true;
        return scriptHelper(`process.stdout.write(${JSON.stringify(`${VERDICT("x")}\n`)});`)(...a);
      },
      helperTmp: {
        make: () => {
          throw new Error("mktemp failed");
        },
        remove: () => undefined,
      },
    });
    assert.equal(r.state, "unverified");
    assert.equal(spawnedHelper, false);
  });
});

// ─── items 3 and 9: Docker body cap and DOCKER_HOST forms ──────────────────────────────────

/** A minimal Docker Engine stand-in: GET /containers/json answers `listing()`. */
async function startDaemon(listen: { socket: string } | { tcp: true }, listing: () => string): Promise<{ host: string; close: () => Promise<void> }> {
  const server = http.createServer((req, res) => {
    if (req.method === "GET" && req.url?.startsWith("/containers/json")) {
      res.writeHead(200, { "content-type": "application/json" });
      res.end(listing());
      return;
    }
    res.writeHead(404);
    res.end();
  });
  if ("socket" in listen) {
    await new Promise<void>((r) => server.listen(listen.socket, r));
  } else {
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
  }
  const host = "socket" in listen ? listen.socket : `tcp://127.0.0.1:${(server.address() as AddressInfo).port}/`;
  return {
    host,
    close: () =>
      new Promise<void>((r) => {
        server.closeAllConnections();
        server.close(() => r());
      }),
  };
}

describe("A-docker-hardening", () => {
  it("a listing body above the 8 MiB cap is docker_error (the request is destroyed)", async () => {
    // A VALID empty array padded past the cap: without the cap it parses clean and reads unconfirmed.
    const d = await startDaemon({ socket: path.join(tmp, "big.sock") }, () => `[${" ".repeat(9 * 1024 * 1024)}]`);
    try {
      const r = await teardownDocker({ dockerHost: `unix://${d.host}`, targetPaths: [CLONE], intervalMs: 10 });
      assert.equal(r.state, "docker_error");
      assert.match(r.detail, /exceeded 8388608 bytes/);
    } finally {
      await d.close();
    }
  });

  it("the cap is per response and configurable; a body under it is read normally", async () => {
    const d = await startDaemon({ socket: path.join(tmp, "small.sock") }, () => "[]");
    try {
      const ok = await teardownDocker({ dockerHost: `unix://${d.host}`, targetPaths: [CLONE], intervalMs: 10 });
      assert.equal(ok.state, "docker_unconfirmed");
      const tiny = await teardownDocker({ dockerHost: `unix://${d.host}`, targetPaths: [CLONE], intervalMs: 10, maxBodyBytes: 1 });
      assert.equal(tiny.state, "docker_error");
    } finally {
      await d.close();
    }
  });

  it("accepts a bare /path socket and tcp://host:port/ like docker-wiring does", async () => {
    const bare = await startDaemon({ socket: path.join(tmp, "bare.sock") }, () => "[]");
    const tcp = await startDaemon({ tcp: true }, () => "[]");
    try {
      assert.equal((await teardownDocker({ dockerHost: bare.host, targetPaths: [CLONE], intervalMs: 10 })).state, "docker_unconfirmed");
      assert.equal((await teardownDocker({ dockerHost: tcp.host, targetPaths: [CLONE], intervalMs: 10 })).state, "docker_unconfirmed");
    } finally {
      await bare.close();
      await tcp.close();
    }
  });
});

// ─── item 4: a self-improve check's leak is reaped ─────────────────────────────────────────

// The leak is found and reaped on the REAL process table: a sandbox that denies enumerating the
// proc root skips through the shared detector (issue #1863).
describe("A-check-leak: a process leaked by a self-improve check is reaped", {
  skip: !HAS_PROCFS ? "reads procfs (Linux only)" : realProcfsSkip("run-quiescence-hardening A-check-leak"),
}, () => {
  it("the check is NOT worker-marked, so its detached leak (cwd in the clone) is killed by the run's own reap", async () => {
    const repoDir = path.join(tmp, "check-leak", "runner", "github.com+o+check");
    const clone = path.join(repoDir, "issue-17");
    fs.mkdirSync(clone, { recursive: true });
    const pidFile = path.join(tmp, "check-leak.pid");
    // The repo-authored "test" backgrounds a detached dev server in the clone and exits 0.
    // A constant program: the clone and the pid file arrive as argv (process.argv[1], [2] under -e).
    const leaky = `const [cwd, pidFile] = process.argv.slice(1);
const c = require("node:child_process").spawn(process.execPath, ["-e", "setInterval(() => {}, 1000)"], { cwd, detached: true, stdio: "ignore" });
require("node:fs").writeFileSync(pidFile, String(c.pid)); c.unref();`;
    const run = defaultCheckRunner({ PATH: String(process.env.PATH ?? "") }, 30_000);
    const res = await run({ name: "leaky", cwd: ".", command: process.execPath, args: ["-e", leaky, clone, pidFile] }, clone);
    assert.equal(res.status, "passed");
    const leaked = Number(fs.readFileSync(pidFile, "utf8"));
    strays.push(leaked);
    const aliveNow = (): boolean => {
      try {
        return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(leaked));
      } catch {
        return false;
      }
    };
    assert.equal(aliveNow(), true, "the leak outlived its check");
    const own = newRunAttempt("run-17", 1, clone, () => []);
    // The real procfs, scoped to this process's descendants plus the (reparented) leak.
    const r = await withQuiescenceView(scopedRealView({ pidFiles: [pidFile] }), () =>
      reapProcesses({
        mode: "own",
        targetUid: ME,
        targetKey: own.cloneKey,
        targetPaths: [clone],
        ownMarker: own.marker,
        liveMarkers: [],
        liveRoots: [],
        workerNonce: workerSpawnNonce(),
      }),
    );
    assert.equal(r.state, "quiescent", r.detail);
    assert.ok(r.killed.includes(leaked), "the unmarked leak was reaped");
  });
});
