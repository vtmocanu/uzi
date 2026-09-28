import { after, before, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
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
  scanOnce,
  setQuiescenceViewForTests,
  teardownDocker,
  type HelperSpawn,
  type ProcTable,
  type RunAttempt,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { SinkGate } from "../src/sink-gate.js";
import { scopedRealView } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_ENV, RUN_CLONE_KEY_ENV, WORKER_SPAWN_ENV, recordRoot, workerSpawnEnv, workerSpawnNonce, type RecordedRoot } from "../src/worker-spawn-mark.js";

// issue #1783 (R0/R3/R5 + Docker) — the run-quiescence reaper. Real processes where stated (single
// uid: the "runner uid" is this test's uid), a fake Docker daemon on a unix socket, and an injected
// process table for the classification cases no single-uid host can produce (foreign uids,
// non-dumpable processes). The runner-level fail-closed wiring is in run-quiescence-runner.test.ts.

const HAS_PROCFS = process.platform === "linux" && fs.existsSync(path.join("/", "proc", "self", "status"));
const ME = process.getuid?.() ?? 0;
const RUNNER = 10002;

// ─── fixtures ──────────────────────────────────────────────────────────────────────────────

let tmp: string;
before(() => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-quiesce-"));
  // The real-process cases reap this file's own children only, never the host's other processes
  // (the injected-table cases pass their own table and are unaffected).
  setQuiescenceViewForTests(scopedRealView());
});
const spawned: ChildProcess[] = [];
/** Grandchildren we know by pid only (the stand-in CLI's detached tool). */
const strays: number[] = [];
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
      if (c.pid) process.kill(-c.pid, "SIGKILL");
    } catch {
      /* gone */
    }
    try {
      c.kill("SIGKILL");
    } catch {
      /* gone */
    }
  }
  fs.rmSync(tmp, { recursive: true, force: true });
  restoreHermeticView();
});

/** A runner root with a repo dir holding `keys` as clone dirs. */
function makeClones(name: string, keys: string[]): Record<string, string> {
  // A distinct repoDir per case, so no two cases ever share a clone KEY (the scope is by key too).
  const repoDir = path.join(tmp, name, "runner", `github.com+o+${name}`);
  const out: Record<string, string> = {};
  for (const k of keys) {
    out[k] = path.join(repoDir, k);
    fs.mkdirSync(out[k]!, { recursive: true });
  }
  return out;
}

function attemptFor(runId: string, clone: string, roots: () => RecordedRoot[] = () => []): RunAttempt {
  return newRunAttempt(runId, 1, clone, roots);
}

function markerEnv(a: RunAttempt): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { PATH: process.env.PATH, [RUN_ATTEMPT_ENV]: a.marker, [RUN_CLONE_ENV]: a.clonePath, [RUN_CLONE_KEY_ENV]: a.cloneKey };
  return env;
}

/** A process is alive iff procfs lists it and it is not a zombie. */
function alive(pid: number | undefined): boolean {
  if (!pid) return false;
  try {
    return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

async function until(pred: () => boolean, ms = 3000): Promise<boolean> {
  const end = Date.now() + ms;
  while (Date.now() < end) {
    if (pred()) return true;
    await new Promise((r) => setTimeout(r, 25));
  }
  return pred();
}

const SLEEPER = "setInterval(() => {}, 1000);";

/** A long-lived node process with `env`, cwd `cwd`, in its own session. */
function sleeper(cwd: string, env: NodeJS.ProcessEnv): ChildProcess {
  const c = spawn(process.execPath, ["-e", SLEEPER], { cwd, env, detached: true, stdio: "ignore" });
  spawned.push(c);
  return c;
}

// ─── fake Docker daemon ────────────────────────────────────────────────────────────────────

interface FakeDaemon {
  socket: string;
  containers: Map<string, { Id: string; Mounts: Array<{ Type: string; Source: string; Destination: string }> }>;
  /** When set, the NEXT create waits for this promise before it is committed. */
  hold: Promise<void> | undefined;
  close: () => Promise<void>;
}

async function startFakeDaemon(dir: string): Promise<FakeDaemon> {
  fs.mkdirSync(dir, { recursive: true });
  const socket = path.join(dir, "docker.sock");
  let seq = 0;
  const daemon: FakeDaemon = { socket, containers: new Map(), hold: undefined, close: async () => {} };
  const server = http.createServer((req, res) => {
    const url = new URL(req.url ?? "/", "http://docker");
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      void (async () => {
        if (req.method === "GET" && url.pathname === "/containers/json") {
          res.writeHead(200, { "content-type": "application/json" });
          res.end(JSON.stringify([...daemon.containers.values()]));
          return;
        }
        if (req.method === "POST" && url.pathname === "/containers/create") {
          const held = daemon.hold;
          daemon.hold = undefined;
          const body = JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}") as { HostConfig?: { Binds?: string[] } };
          const mounts = (body.HostConfig?.Binds ?? []).map((b) => {
            const [src, dst] = b.split(":");
            // Like `docker run -v`: a missing bind source directory is created.
            fs.mkdirSync(src!, { recursive: true });
            return { Type: "bind", Source: src!, Destination: dst ?? "/" };
          });
          if (held) await held;
          const Id = `c${++seq}`;
          daemon.containers.set(Id, { Id, Mounts: mounts });
          res.writeHead(201, { "content-type": "application/json" });
          res.end(JSON.stringify({ Id }));
          return;
        }
        const del = /^\/containers\/([^/]+)$/.exec(url.pathname);
        if (req.method === "DELETE" && del) {
          const existed = daemon.containers.delete(decodeURIComponent(del[1]!));
          res.writeHead(existed ? 204 : 404);
          res.end();
          return;
        }
        res.writeHead(404);
        res.end();
      })();
    });
  });
  await new Promise<void>((r) => server.listen(socket, r));
  daemon.close = () =>
    new Promise<void>((r) => {
      server.closeAllConnections();
      server.close(() => r());
    });
  return daemon;
}

function createContainer(socket: string, bindSource: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const req = http.request({ socketPath: socket, method: "POST", path: "/containers/create" }, (res) => {
      const chunks: Buffer[] = [];
      res.on("data", (c: Buffer) => chunks.push(c));
      res.on("end", () => resolve((JSON.parse(Buffer.concat(chunks).toString("utf8")) as { Id: string }).Id));
    });
    req.on("error", reject);
    req.end(JSON.stringify({ HostConfig: { Binds: [`${bindSource}:/data`] } }));
  });
}

function boundUnder(d: FakeDaemon, root: string): string[] {
  return [...d.containers.values()]
    .filter((c) => c.Mounts.some((m) => m.Source === root || m.Source.startsWith(`${root}${path.sep}`)))
    .map((c) => c.Id);
}

// A tool loop: create a container bound under `src` every 50 ms, forever.
const TOOL = `
const http = require("node:http");
const [sock, src] = process.argv.slice(1);
setInterval(() => {
  const r = http.request({ socketPath: sock, method: "POST", path: "/containers/create" }, (res) => res.resume());
  r.on("error", () => {});
  r.end(JSON.stringify({ HostConfig: { Binds: [src + ":/data"] } }));
}, 50);`;

// A stand-in agent CLI: starts the tool DETACHED (its own session, as the SDK's tool shell and a
// `setsid` both do), prints the tool's pid, and idles.
const CLI = `
const { spawn } = require("node:child_process");
const [tool, sock, src] = process.argv.slice(1);
const c = spawn(process.execPath, ["-e", tool, sock, src], { detached: true, stdio: "ignore" });
process.stdout.write(String(c.pid) + "\\n");
setInterval(() => {}, 1000);`;

/** Start a stand-in CLI group whose tool keeps creating containers bound under `bindSrc`. */
async function startAgent(a: RunAttempt, sock: string, bindSrc: string): Promise<{ cli: ChildProcess; toolPid: number }> {
  const cli = spawn(process.execPath, ["-e", CLI, TOOL, sock, bindSrc], {
    cwd: a.clonePath,
    env: markerEnv(a),
    detached: true,
    stdio: ["ignore", "pipe", "ignore"],
  });
  spawned.push(cli);
  const toolPid = await new Promise<number>((resolve, reject) => {
    let buf = "";
    cli.stdout!.on("data", (c: Buffer) => {
      buf += c.toString();
      const line = buf.split("\n")[0];
      if (buf.includes("\n") && line) resolve(Number(line));
    });
    cli.on("error", reject);
  });
  strays.push(toolPid);
  return { cli, toolPid };
}

/** killAgentTree, as SdkExecutor does it single-uid: SIGKILL the recorded CLI group. */
function killAgentTree(cli: ChildProcess): void {
  try {
    process.kill(-cli.pid!, "SIGKILL");
  } catch {
    /* gone */
  }
}

// ─── real processes ────────────────────────────────────────────────────────────────────────

describe("A-core: a setsid'd agent tool survives killAgentTree; quiesce + retire removes it", { skip: !HAS_PROCFS }, () => {
  it("reaps the attributed tool and its clone-bound containers, and never touches the issue-1769 sibling", async () => {
    const clones = makeClones("core", ["issue-17", "issue-1769"]);
    const daemon = await startFakeDaemon(path.join(tmp, "core-docker"));
    try {
      const registry = new LiveAttemptRegistry();
      let ownRoots: RecordedRoot[] = [];
      const own = attemptFor("run-17", clones["issue-17"]!, () => ownRoots);
      const sib = attemptFor("run-1769", clones["issue-1769"]!);
      registry.add(own);
      registry.add(sib);
      const ownAgent = await startAgent(own, daemon.socket, path.join(own.clonePath, "data"));
      ownRoots = [recordRoot(ownAgent.cli.pid)!];
      const sibAgent = await startAgent(sib, daemon.socket, path.join(sib.clonePath, "data"));
      assert.ok(await until(() => boundUnder(daemon, own.clonePath).length > 0 && boundUnder(daemon, sib.clonePath).length > 0));

      // BASE behaviour (the bug): killAgentTree alone kills the CLI group only. The detached tool
      // survives in its own session and keeps binding containers into the clone.
      killAgentTree(ownAgent.cli);
      await until(() => !alive(ownAgent.cli.pid), 2000);
      await new Promise((r) => setTimeout(r, 300));
      assert.equal(alive(ownAgent.toolPid), true, "base: the setsid'd tool survives killAgentTree");
      const before = boundUnder(daemon, own.clonePath).length;
      await new Promise((r) => setTimeout(r, 200));
      assert.ok(boundUnder(daemon, own.clonePath).length > before, "base: the survivor keeps creating clone-bound containers");

      // The fix: quiesce (process reap, then Docker teardown), then retire the clone. The tool
      // was reparented away from this process when its CLI died, so the scoped view names it.
      setQuiescenceViewForTests(scopedRealView({ pids: strays }));
      const out = await quiesceRunAttempt(
        {
          mode: "own",
          attempt: own,
          cloneKey: own.cloneKey,
          targetPaths: [own.clonePath],
          processes: true,
          dockerHost: `unix://${daemon.socket}`,
          registry,
        },
        { reap: { viaHelper: false }, docker: { intervalMs: 100 } },
      );
      assert.equal(out.process?.state, "quiescent", out.process?.detail);
      assert.ok(out.process?.killed.includes(ownAgent.toolPid), "the tool was reaped by pid");
      assert.equal(out.docker.state, "docker_unconfirmed");
      fs.rmSync(own.clonePath, { recursive: true, force: true });

      assert.equal(await until(() => !alive(ownAgent.toolPid)), true, "no attributed process survives");
      assert.deepEqual(boundUnder(daemon, own.clonePath), [], "no container bound within the retired clone");
      // The sibling next door keeps its process, its containers and its clone.
      assert.equal(alive(sibAgent.toolPid), true, "the issue-1769 tool is untouched");
      assert.equal(alive(sibAgent.cli.pid), true, "the issue-1769 CLI is untouched");
      assert.ok(boundUnder(daemon, sib.clonePath).length > 0, "the issue-1769 containers are untouched");
      assert.equal(fs.existsSync(sib.clonePath), true);
      killAgentTree(sibAgent.cli);
      process.kill(sibAgent.toolPid, "SIGKILL");
    } finally {
      await daemon.close();
    }
  });
});

describe("A-concurrent: two unrelated healthy runs reap at the same time", { skip: !HAS_PROCFS }, () => {
  it("both are quiescent and each kills only its own residue", async () => {
    const clones = makeClones("concurrent", ["issue-17", "issue-1769"]);
    const registry = new LiveAttemptRegistry();
    const a = attemptFor("run-a", clones["issue-17"]!);
    const b = attemptFor("run-b", clones["issue-1769"]!);
    registry.add(a);
    registry.add(b);
    const pa = sleeper(a.clonePath, markerEnv(a));
    const pb = sleeper(b.clonePath, markerEnv(b));
    await until(() => alive(pa.pid) && alive(pb.pid));
    const req = (x: RunAttempt) =>
      quiesceRunAttempt(
        { mode: "own", attempt: x, cloneKey: x.cloneKey, targetPaths: [x.clonePath], processes: true, dockerHost: undefined, registry },
        { reap: { viaHelper: false } },
      );
    const [ra, rb] = await Promise.all([req(a), req(b)]);
    assert.equal(ra.process?.state, "quiescent", ra.process?.detail);
    assert.equal(rb.process?.state, "quiescent", rb.process?.detail);
    assert.deepEqual(ra.process?.killed, [pa.pid]);
    assert.deepEqual(rb.process?.killed, [pb.pid]);
    assert.equal(ra.docker.state, "not_wired");
  });
});

describe("A-component: issue-17 vs issue-1769 on a real process table", { skip: !HAS_PROCFS }, () => {
  it("scopes by whole path components for cwd and by exact key for UZI_RUN_CLONE_KEY", async () => {
    const clones = makeClones("component", ["issue-17", "issue-1769"]);
    const own = attemptFor("run-17", clones["issue-17"]!);
    // Every process below carries a TERMINAL attempt's marker (so it would be killed if in scope,
    // and is not protected as the scanner's unmarked descendant): only scope can spare it.
    const dead = (key?: string): NodeJS.ProcessEnv => ({
      PATH: process.env.PATH,
      [RUN_ATTEMPT_ENV]: "run-gone:20260101T000000Z-g1-0000000000000000",
      ...(key ? { [RUN_CLONE_KEY_ENV]: key } : {}),
    });
    // In the sibling clone (its cwd prefix-matches issue-17 as a string).
    const byCwd = sleeper(clones["issue-1769"]!, dead());
    // Elsewhere, with the sibling's key.
    const byKey = sleeper(tmp, dead("github.com+o+component/issue-1769"));
    // Elsewhere, with OUR key: in scope by key.
    const ours = sleeper(tmp, dead(own.cloneKey));
    await until(() => alive(byCwd.pid) && alive(byKey.pid) && alive(ours.pid));
    const r = await reapProcesses(scanReq(own, [own.clonePath]));
    assert.equal(r.state, "quiescent", r.detail);
    assert.deepEqual(r.killed, [ours.pid]);
    assert.equal(alive(byCwd.pid), true);
    assert.equal(alive(byKey.pid), true);
    process.kill(byCwd.pid!, "SIGKILL");
    process.kill(byKey.pid!, "SIGKILL");
  });
});

describe("A-worker-op: a worker-marked op in the clone survives a concurrent reap", { skip: !HAS_PROCFS }, () => {
  it("the tick's git-like op, started under the sink gate, completes while the reap runs", async () => {
    const clones = makeClones("worker-op", ["issue-17"]);
    const own = attemptFor("run-17", clones["issue-17"]!);
    const gate = new SinkGate();
    const release = gate.tryAcquire(() => undefined);
    assert.ok(release, "the tick holds the sink gate");
    const done = path.join(own.clonePath, "op-done");
    // The worker's op: worker-marked, cwd in the clone, a second of work, then a file write.
    const op = spawn(
      process.execPath,
      ["-e", `setTimeout(() => { require("node:fs").writeFileSync(process.argv[1], "ok"); }, 1000);`, done],
      { cwd: own.clonePath, env: workerSpawnEnv({ PATH: process.env.PATH }), stdio: "ignore" },
    );
    spawned.push(op);
    const exited = new Promise<number | null>((r) => op.on("exit", (code) => r(code)));
    await until(() => alive(op.pid));
    // The reap runs while the op is live (the mark alone must protect it) ...
    const r = await reapProcesses(scanReq(own, [own.clonePath]));
    assert.equal(r.state, "quiescent", r.detail);
    assert.deepEqual(r.killed, []);
    // ... and the gated quiesce waits for the tick to release, then finds nothing to kill.
    const gated = gate.run(() => reapProcesses(scanReq(own, [own.clonePath])));
    assert.equal(await exited, 0, "the worker op completed");
    release!();
    assert.equal((await gated).state, "quiescent");
    assert.equal(fs.readFileSync(done, "utf8"), "ok");
  });
});

describe("A-self: the scanner never kills itself or its ancestors", { skip: !HAS_PROCFS }, () => {
  it("a helper started (via a wrapper) with this attempt's marker and cwd inside the clone stays alive", async () => {
    const clones = makeClones("self", ["issue-17"]);
    const own = attemptFor("run-17", clones["issue-17"]!);
    const victim = sleeper(own.clonePath, markerEnv(own));
    await until(() => alive(victim.pid));
    const WRAP = `
const { spawnSync } = require("node:child_process");
const a = JSON.parse(process.argv[1]);
const r = spawnSync(a[0], a.slice(1), { stdio: ["inherit", "pipe", "inherit"], encoding: "utf8" });
process.stdout.write(r.stdout || "");
process.exit(r.status === null ? 1 : r.status);`;
    const spawnHelper: HelperSpawn = (command, args, opts) => {
      // The wrapper and the helper both carry THIS attempt's marker and sit in the clone, and
      // neither has the worker mark: only the R5 self/ancestor exclusion protects them.
      const env: NodeJS.ProcessEnv = { ...opts.env, ...markerEnv(own) };
      delete env[WORKER_SPAWN_ENV];
      // stdin is piped through the wrapper: the helper reads its request there (never argv).
      const c = spawn(process.execPath, ["-e", WRAP, JSON.stringify([command, ...args])], {
        cwd: own.clonePath,
        env,
        stdio: ["pipe", "pipe", "pipe"],
      });
      spawned.push(c);
      return c;
    };
    const r = await reapRunProcesses(scanReq(own, [own.clonePath]), { viaHelper: true, spawnHelper });
    assert.equal(r.state, "quiescent", r.detail);
    assert.deepEqual(r.killed, [victim.pid], "only the marked victim was killed, not the scanner or its wrapper");
  });
});

describe("A-healthy: a normal run through runnerCommand (passthrough single-uid)", { skip: !HAS_PROCFS }, () => {
  it("is quiescent with no residue, and reaps an own-marked straggler", async () => {
    const clones = makeClones("healthy", ["issue-17"]);
    const own = attemptFor("run-17", clones["issue-17"]!);
    const clean = await reapRunProcesses(scanReq(own, [own.clonePath]), { viaHelper: true });
    assert.equal(clean.state, "quiescent", clean.detail);
    assert.deepEqual(clean.killed, []);
    const straggler = sleeper(own.clonePath, markerEnv(own));
    await until(() => alive(straggler.pid));
    const reaped = await reapRunProcesses(scanReq(own, [own.clonePath]), { viaHelper: true });
    assert.equal(reaped.state, "quiescent", reaped.detail);
    assert.deepEqual(reaped.killed, [straggler.pid]);
    assert.equal(await until(() => !alive(straggler.pid)), true);
  });
});

// ─── injected process table ────────────────────────────────────────────────────────────────

interface FakeProc {
  uid: number;
  ppid?: number;
  pgid?: number;
  sid?: number;
  cwd?: string | "EACCES" | "ENOENT";
  env?: Record<string, string> | "EACCES" | "ENOENT";
  zombie?: boolean;
  /** `stat` field 22 (start time); default {@link DEFAULT_START}. */
  start?: number;
}

const DEFAULT_START = 1000;

function errno(code: string): Error {
  return Object.assign(new Error(code), { code });
}

function fakeTable(procs: Record<number, FakeProc>, opts: { listThrows?: boolean } = {}): ProcTable {
  const get = (pid: number): FakeProc => {
    const p = procs[pid];
    if (!p) throw errno("ENOENT");
    return p;
  };
  return {
    listPids: () => {
      if (opts.listThrows) throw errno("EACCES");
      return Object.keys(procs).map(Number);
    },
    readStatus: (pid) => {
      const p = get(pid);
      return `Name:\tproc${pid}\nState:\t${p.zombie ? "Z (zombie)" : "S (sleeping)"}\nUid:\t${p.uid}\t${p.uid}\t${p.uid}\t${p.uid}\n`;
    },
    readEnviron: (pid) => {
      const e = get(pid).env ?? {};
      if (typeof e === "string") throw errno(e);
      return Object.entries(e).map(([k, v]) => `${k}=${v}\u0000`).join("");
    },
    readCwd: (pid) => {
      const c = get(pid).cwd ?? "/";
      if (c === "EACCES" || c === "ENOENT") throw errno(c);
      return c;
    },
    readStat: (pid) => {
      const p = get(pid);
      // Fields 3..22 (state … starttime): 15 zero fields sit between the session and field 22.
      return `${pid} (proc ${pid}) S ${p.ppid ?? 1} ${p.pgid ?? pid} ${p.sid ?? pid} ${"0 ".repeat(15)}${p.start ?? DEFAULT_START} 0 0`;
    },
  };
}

const CLONE = "/data/runner/github.com+o+r/issue-17";
const KEY = "github.com+o+r/issue-17";
const SELF = 900;

function scanReq(own: RunAttempt | undefined, targetPaths: string[], extra: Partial<ScanRequest> = {}): ScanRequest {
  return {
    mode: "own",
    targetUid: ME,
    targetKey: own?.cloneKey ?? KEY,
    targetPaths,
    ownMarker: own?.marker,
    liveMarkers: [],
    liveRoots: [],
    workerNonce: workerSpawnNonce(),
    ...extra,
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

/** Run the reap loop against a fake table (and a fake clock); a SIGKILL removes the pid. */
async function fakeReap(procs: Record<number, FakeProc>, req: ScanRequest, opts: { listThrows?: boolean } = {}) {
  const signalled: number[] = [];
  let t = 0;
  const r = await reapProcesses(req, {
    table: fakeTable(procs, opts),
    selfPid: SELF,
    now: () => t,
    sleep: async (ms) => {
      t += ms;
    },
    kill: (pid) => {
      signalled.push(pid);
      delete procs[pid];
    },
  });
  return { r, signalled };
}

describe("A-attempt: same-key attempts", () => {
  it("never signals a LIVE other attempt of the key (conflict → survivors) but kills a terminal one's", async () => {
    const procs: Record<number, FakeProc> = {
      [SELF]: { uid: RUNNER, ppid: 1 },
      10: { uid: RUNNER, cwd: CLONE, env: { [RUN_ATTEMPT_ENV]: "run-17:live-other" } },
      11: { uid: RUNNER, cwd: `${CLONE}/sub`, env: { [RUN_ATTEMPT_ENV]: "run-17:terminal" } },
      12: { uid: RUNNER, cwd: CLONE, env: { [RUN_ATTEMPT_ENV]: "run-17:own" } },
    };
    const { r, signalled } = await fakeReap(procs, fakeReq({ liveMarkers: ["run-17:live-other"] }));
    assert.equal(r.state, "survivors");
    assert.deepEqual(signalled.sort(), [11, 12]);
    assert.deepEqual(r.processes.map((p) => [p.pid, p.reason]), [[10, "live_attempt_conflict"]]);
  });

  it("an unmarked in-scope process is killed in mode own, but is a survivor in seed and capture", async () => {
    for (const mode of ["seed", "capture"] as const) {
      const { r, signalled } = await fakeReap({ 20: { uid: RUNNER, cwd: CLONE, env: {} } }, fakeReq({ mode }));
      assert.equal(r.state, "survivors", mode);
      assert.deepEqual(signalled, []);
      assert.equal(r.processes[0]?.reason, "unattributed_in_scope");
    }
    const own = await fakeReap({ 20: { uid: RUNNER, cwd: CLONE, env: {} } }, fakeReq({ mode: "own" }));
    assert.equal(own.r.state, "quiescent");
    assert.deepEqual(own.signalled, [20]);
  });
});

describe("A-component: whole-component scope on an injected table", () => {
  it("issue-1769 is out of scope by cwd, key and mount-like prefix; issue-17 is in", async () => {
    const procs: Record<number, FakeProc> = {
      30: { uid: RUNNER, cwd: "/data/runner/github.com+o+r/issue-1769", env: {} },
      31: { uid: RUNNER, cwd: "/elsewhere", env: { [RUN_CLONE_KEY_ENV]: "github.com+o+r/issue-1769" } },
      32: { uid: RUNNER, cwd: "/data/runner/github.com+o+r/issue-17 (deleted)", env: {} },
      33: { uid: RUNNER, cwd: "/elsewhere", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: `run-gone:${TERMINAL_ID}` } },
    };
    const { r, signalled } = await fakeReap(procs, fakeReq());
    assert.equal(r.state, "quiescent");
    assert.deepEqual(signalled.sort(), [32, 33]);
  });
});

const TERMINAL_ID = "20260101T000000Z-g1-0000000000000000";

describe("issue #1783 final (R3): the key scopes a process only together with a well-formed attempt marker", () => {
  it("a key-only (or malformed-marker) process outside every target path is out of scope in every mode", async () => {
    for (const mode of ["seed", "capture", "own"] as const) {
      const procs: Record<number, FakeProc> = {
        80: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY } },
        81: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: "run-b:not-an-attempt-id" } },
        82: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: `:${TERMINAL_ID}` } },
        83: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: "" } },
      };
      const { r, signalled } = await fakeReap(procs, fakeReq({ mode }));
      assert.equal(r.state, "quiescent", `${mode}: ${r.detail}`);
      assert.deepEqual(signalled, [], mode);
      assert.deepEqual(r.processes, [], mode);
    }
  });

  it("with a well-formed marker the key still scopes it: terminal → killed, live → conflict, own → killed", async () => {
    const live = `run-live:20260101T000001Z-g2-00000000000000aa`;
    const own = `run-own:20260101T000002Z-g3-00000000000000bb`;
    const procs: Record<number, FakeProc> = {
      84: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: `run-gone:${TERMINAL_ID}` } },
      85: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: live } },
      86: { uid: RUNNER, cwd: "/tmp", env: { [RUN_CLONE_KEY_ENV]: KEY, [RUN_ATTEMPT_ENV]: own } },
    };
    const { r, signalled } = await fakeReap(procs, fakeReq({ mode: "seed", ownMarker: own, liveMarkers: [live] }));
    assert.equal(r.state, "survivors");
    assert.deepEqual(signalled.sort(), [84, 86]);
    assert.deepEqual(r.processes.map((p) => [p.pid, p.reason]), [[85, "live_attempt_conflict"]]);
  });

  it("a key-only process whose cwd IS in a target path stays in scope (cwd scope is unchanged)", async () => {
    const { r, signalled } = await fakeReap({ 87: { uid: RUNNER, cwd: CLONE, env: { [RUN_CLONE_KEY_ENV]: KEY } } }, fakeReq({ mode: "seed" }));
    assert.equal(r.state, "survivors");
    assert.deepEqual(signalled, []);
    assert.equal(r.processes[0]?.reason, "unattributed_in_scope");
  });
});

describe("A-foreign-uids: worker, root and runner-cmd processes never make a reap unverified", () => {
  it("ignores every non-runner uid, even in scope and even when unreadable", async () => {
    const procs: Record<number, FakeProc> = {
      40: { uid: 10001, cwd: CLONE, env: "EACCES" }, // worker
      41: { uid: 0, cwd: "EACCES", env: "EACCES" }, // root
      42: { uid: 10003, cwd: CLONE, env: { [RUN_ATTEMPT_ENV]: "run-17:own" } }, // runner-cmd
    };
    const { r, signalled } = await fakeReap(procs, fakeReq());
    assert.equal(r.state, "quiescent", r.detail);
    assert.deepEqual(signalled, []);
  });
});

describe("A-nondumpable: a runner-uid process whose environ is unreadable", () => {
  it("with no ancestry to a live attempt → unverified", async () => {
    const { r, signalled } = await fakeReap({ 50: { uid: RUNNER, cwd: "EACCES", env: "EACCES", ppid: 1 } }, fakeReq());
    assert.equal(r.state, "unverified");
    assert.deepEqual(signalled, []);
    assert.deepEqual(r.processes.map((p) => [p.pid, p.cwd, p.reason]), [[50, "unreadable", "unreadable_unattributed"]]);
  });

  it("with its ppid chain, group or session leading to another live attempt's recorded root → ignored", async () => {
    const root = 60;
    for (const shape of [{ ppid: 61 }, { pgid: root }, { sid: root }] as const) {
      const procs: Record<number, FakeProc> = {
        [root]: { uid: RUNNER, cwd: "/data/runner/github.com+o+r/issue-99", env: {} },
        61: { uid: RUNNER, ppid: root, cwd: "/data/runner/github.com+o+r/issue-99", env: {} },
        62: { uid: RUNNER, cwd: "EACCES", env: "EACCES", ...shape },
      };
      const { r } = await fakeReap(procs, fakeReq({ liveRoots: [{ pid: root, startTime: DEFAULT_START }], liveMarkers: ["run-99:x"] }));
      assert.equal(r.state, "quiescent", JSON.stringify(shape));
    }
  });
});

describe("A-self on an injected table", () => {
  it("excludes the scanner and its ancestors even when marked and in scope, and its unmarked descendants", async () => {
    const marked = { [RUN_ATTEMPT_ENV]: "run-17:own" };
    const procs: Record<number, FakeProc> = {
      70: { uid: RUNNER, ppid: 1, cwd: CLONE, env: marked }, // grandparent
      71: { uid: RUNNER, ppid: 70, cwd: CLONE, env: marked }, // parent
      [SELF]: { uid: RUNNER, ppid: 71, cwd: CLONE, env: marked },
      72: { uid: RUNNER, ppid: SELF, cwd: CLONE, env: {} }, // unmarked child: the scanner's own
      73: { uid: RUNNER, ppid: 72, cwd: CLONE, env: {} }, // unmarked grandchild
      74: { uid: RUNNER, ppid: 1, cwd: CLONE, env: marked }, // unrelated: reaped
      // A MARKED descendant is agent residue (single-uid, the scanner is the worker and the agent
      // CLI is its child), so it is reaped.
      76: { uid: RUNNER, ppid: 72, cwd: CLONE, env: marked },
    };
    const { r, signalled } = await fakeReap(procs, fakeReq());
    assert.equal(r.state, "quiescent");
    assert.deepEqual(signalled.sort(), [74, 76]);
  });

  it("skips a process carrying this worker's spawn mark", async () => {
    const { signalled } = await fakeReap({ 75: { uid: RUNNER, cwd: CLONE, env: { [WORKER_SPAWN_ENV]: "nonce" } } }, fakeReq());
    assert.deepEqual(signalled, []);
  });
});

describe("A-unverified: every way the proof can fail is unverified", () => {
  it("(a) a helper that cannot be spawned (sync throw, or the binary is missing)", async () => {
    const throws: HelperSpawn = () => {
      throw new Error("setpriv: cannot exec");
    };
    const r1 = await reapRunProcesses(fakeReq(), { viaHelper: true, spawnHelper: throws });
    assert.equal(r1.state, "unverified");
    const missing: HelperSpawn = (_c, args, opts) => spawn(path.join(tmp, "no-such-setpriv"), args, { cwd: opts.cwd, env: opts.env });
    const r2 = await reapRunProcesses(fakeReq(), { viaHelper: true, spawnHelper: missing });
    assert.equal(r2.state, "unverified");
    const garbage: HelperSpawn = (_c, _a, opts) => spawn(process.execPath, ["-e", "console.log('not json')"], { cwd: opts.cwd, env: opts.env });
    const r3 = await reapRunProcesses(fakeReq(), { viaHelper: true, spawnHelper: garbage });
    assert.equal(r3.state, "unverified");
  });

  it("(b) the process table is unreadable as a whole", async () => {
    const { r } = await fakeReap({}, fakeReq(), { listThrows: true });
    assert.equal(r.state, "unverified");
    assert.match(r.detail, /process table unreadable/);
  });

  it("(c) EACCES on one in-scope runner-uid pid", async () => {
    const procs: Record<number, FakeProc> = {
      80: { uid: RUNNER, cwd: CLONE, env: "EACCES" },
      81: { uid: RUNNER, cwd: CLONE, env: { [RUN_ATTEMPT_ENV]: "run-17:own" } },
    };
    const { r, signalled } = await fakeReap(procs, fakeReq());
    assert.equal(r.state, "unverified");
    assert.deepEqual(signalled, [81], "the attributable one is still reaped");
    assert.deepEqual(r.processes.map((p) => p.pid), [80]);
  });

  it("(d) a pid that vanishes mid-scan is skipped silently", async () => {
    const procs: Record<number, FakeProc> = {
      90: { uid: RUNNER, cwd: "ENOENT", env: {} },
      91: { uid: RUNNER, cwd: CLONE, env: "ENOENT" },
      92: { uid: RUNNER, zombie: true, cwd: CLONE, env: {} },
    };
    const { r, signalled } = await fakeReap(procs, fakeReq());
    assert.equal(r.state, "quiescent");
    assert.deepEqual(signalled, []);
  });

  it("a pid unreadable for an instant (mid-execve) is rescanned, not reported", async () => {
    let reads = 0;
    const base = fakeTable({ 97: { uid: RUNNER, cwd: CLONE, env: {} } });
    const table: ProcTable = {
      ...base,
      readEnviron: (pid) => {
        if (++reads === 1) throw errno("EACCES");
        return base.readEnviron(pid);
      },
    };
    let t = 0;
    const r = await reapProcesses(fakeReq({ mode: "seed" }), { table, selfPid: SELF, kill: () => {}, now: () => t, sleep: async (ms) => { t += ms; } });
    assert.equal(r.state, "survivors", "rescanned and then classified (an unattributed survivor in seed mode)");
    assert.equal(t, 100);
  });

  it("a kill that never takes effect ends at the deadline as survivors", async () => {
    let t = 0;
    const r = await reapProcesses(fakeReq(), {
      table: fakeTable({ 95: { uid: RUNNER, cwd: CLONE, env: { [RUN_ATTEMPT_ENV]: "run-17:own" } } }),
      selfPid: SELF,
      kill: () => {},
      sleep: async (ms) => {
        t += ms;
      },
      now: () => t,
    });
    assert.equal(r.state, "survivors");
    assert.equal(r.processes[0]?.reason, "own_attempt:kill_unconfirmed");
    assert.ok(t >= 5000 && t < 5200, "rescans every 100 ms until the 5 s deadline");
  });

  it("scanOnce signals nothing by itself", () => {
    const res = scanOnce(fakeReq(), fakeTable({ 96: { uid: RUNNER, cwd: CLONE, env: {} } }), SELF);
    assert.deepEqual(res.kill.map((p) => p.pid), [96]);
  });
});

// ─── Docker ────────────────────────────────────────────────────────────────────────────────

describe("A-docker: the teardown never claims quiescence", () => {
  it("removes only clone-bound containers; a held create landing after the clean listings still reads docker_unconfirmed", async () => {
    const clones = makeClones("docker", ["issue-17", "issue-1769"]);
    const daemon = await startFakeDaemon(path.join(tmp, "docker-daemon"));
    try {
      await createContainer(daemon.socket, path.join(clones["issue-17"]!, "a"));
      const sibling = await createContainer(daemon.socket, path.join(clones["issue-1769"]!, "a"));
      const unrelated = await createContainer(daemon.socket, path.join(tmp, "volumes", "v", "_data"));
      let release!: () => void;
      daemon.hold = new Promise<void>((r) => (release = r));
      const late = createContainer(daemon.socket, path.join(clones["issue-17"]!, "late"));
      const r = await teardownDocker({ dockerHost: `unix://${daemon.socket}`, targetPaths: [clones["issue-17"]!], intervalMs: 50 });
      assert.equal(r.state, "docker_unconfirmed");
      assert.notEqual(r.state as string, "quiescent");
      assert.equal(r.removed.length, 1);
      assert.ok(daemon.containers.has(sibling), "issue-1769's container is never touched");
      assert.ok(daemon.containers.has(unrelated), "a container binding nothing under the clone is never touched");
      release();
      await late;
      assert.equal(boundUnder(daemon, clones["issue-17"]!).length, 1, "the accepted create completed after the teardown");
    } finally {
      await daemon.close();
    }
  });

  it("N6: a container binding the clone's PARENT (the repo dir) or an ancestor (the runner root) is never removed", async () => {
    const clones = makeClones("docker-ancestor", ["issue-17"]);
    const clone = clones["issue-17"]!;
    const repoDir = path.dirname(clone); // /…/runner/<repo>
    const runnerRoot = path.dirname(repoDir); // /…/runner
    const daemon = await startFakeDaemon(path.join(tmp, "docker-ancestor-daemon"));
    try {
      const parent = await createContainer(daemon.socket, repoDir);
      const ancestor = await createContainer(daemon.socket, runnerRoot);
      const own = await createContainer(daemon.socket, clone);
      const r = await teardownDocker({ dockerHost: `unix://${daemon.socket}`, targetPaths: [clone], intervalMs: 20 });
      assert.equal(r.state, "docker_unconfirmed");
      assert.deepEqual(r.removed, [own], "only the container bound AT the clone was removed");
      assert.ok(daemon.containers.has(parent), "the parent-bound container (it may serve a sibling run) is untouched");
      assert.ok(daemon.containers.has(ancestor), "the ancestor-bound container is untouched");
    } finally {
      await daemon.close();
    }
  });

  it("daemon down → docker_error; nothing wired → not_wired; an unsupported host → docker_error", async () => {
    const down = await teardownDocker({ dockerHost: `unix://${path.join(tmp, "absent.sock")}`, targetPaths: ["/x"] });
    assert.equal(down.state, "docker_error");
    assert.equal((await teardownDocker({ dockerHost: undefined, targetPaths: ["/x"] })).state, "not_wired");
    assert.equal((await teardownDocker({ dockerHost: "ssh://host", targetPaths: ["/x"] })).state, "docker_error");
  });

  it("quiesceRunAttempt skips the process half for a Codex run but still tears Docker down", async () => {
    const out = await quiesceRunAttempt({
      mode: "own",
      attempt: undefined,
      cloneKey: KEY,
      targetPaths: [CLONE],
      processes: false,
      dockerHost: undefined,
      registry: new LiveAttemptRegistry(),
    });
    assert.equal(out.process, undefined);
    assert.equal(out.docker.state, "not_wired");
  });
});
